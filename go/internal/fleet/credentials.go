// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"context"
	"crypto/ecdh"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	"github.com/gardenlinux/feos/go/internal/activity"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// Pod credentials' rate limit: at most credentialsPerWindow certificates
// per pod within credentialsWindow, whatever the key.
const (
	credentialsPerWindow = 5
	credentialsWindow    = time.Minute
)

// issuePodCredentials is IssuePodCredentials (§8): a pod's feos-authn,
// through its node's uplink, gets the pod's certificate for its signing
// request and the pod's bindings sealed to the run's HPKE key. Only a pod
// the control plane placed on this node and that has a run there (it is
// created, started, resumed or restored, or runs) gets them.
func (s *Service) issuePodCredentials(ctx context.Context, req *fleetv1.IssuePodCredentialsRequest) (*fleetv1.IssuePodCredentialsResponse, error) {
	p, err := nodePeer(ctx)
	if err != nil {
		return nil, err
	}
	podID, err := podid.Parse(req.GetPodId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "no pod %s", activity.Clean(req.GetPodId()))
	}
	if _, _, err := pki.CheckCSR(req.GetCsr(), false); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	hpkeKey := req.GetHpkePublicKey()
	if _, err := ecdh.X25519().NewPublicKey(hpkeKey); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "hpke_public_key is not an X25519 public key: %v", err)
	}
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	// The bindings are read before the transaction, so that the version
	// issued_certs records is the version sealed into the answer.
	var bindings *fleetv1.PodBindings
	var version string
	if s.bindings != nil {
		if bindings, version, err = s.bindings.PodBindings(ctx, podID); err != nil {
			return nil, rpcError(err, "read the pod's bindings")
		}
		if bindings != nil && len(bindings.GetKube()) == 0 && len(bindings.GetSsh()) == 0 &&
			len(bindings.GetHttp()) == 0 {
			bindings = nil
		}
		if bindings == nil {
			version = ""
		}
	}

	var cert *fleetv1.Certificate
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		cert = nil
		if err := checkInTx(ctx, tx, p.Node, p.Serial); err != nil {
			return err
		}
		sb, err := tx.LockSandbox(ctx, podID, store.ForShare)
		if errors.Is(err, model.ErrNotFound) {
			return status.Errorf(codes.NotFound, "no pod %s", podID)
		}
		if err != nil {
			return err
		}
		if sb.Node != p.Node {
			return status.Errorf(codes.PermissionDenied, "pod %s is not placed on node %s", podID, p.Node)
		}
		switch sb.ClientState {
		case model.StateCreating, model.StateRunning, model.StateResuming:
		default:
			return status.Errorf(codes.PermissionDenied, "pod %s has no run on node %s", podID, p.Node)
		}
		// Concurrent requests for the pod (feos-authn retrying over
		// several uplinks) count and insert one at a time.
		if err := tx.LockPodCredentials(ctx, podID); err != nil {
			return err
		}
		n, err := tx.CountIssuedCerts(ctx, podID, credentialsWindow)
		if err != nil {
			return err
		}
		if n >= credentialsPerWindow {
			return status.Errorf(codes.ResourceExhausted, "too many credential requests for pod %s", podID)
		}
		spiffeID := pki.SandboxID(s.iss.TrustDomain(), sb.Owner, sb.ID)
		c, leaf, err := s.iss.IssuePod(req.GetCsr(), spiffeID, nil, s.cfg.PodCertTTL)
		if err != nil {
			return status.Errorf(codes.InvalidArgument, "issue the pod's certificate: %v", err)
		}
		serial := pki.Serial(leaf)
		if _, err := tx.InsertIssuedCert(ctx, store.IssuedCert{
			Serial: serial, PodID: podID, Incarnation: sb.Incarnation, Node: p.Node, SPIFFEID: spiffeID,
			HPKEPublicKey: hpkeKey, BindingsVersion: version, NotAfter: leaf.NotAfter,
		}); err != nil {
			return err
		}
		keySum := sha256.Sum256(hpkeKey)
		if err := s.act.RecordTx(ctx, tx.Q(), activity.Event{
			SandboxID: sb.ID, TenantID: sb.Owner, Node: p.Node, Kind: activity.KindPodCredentials,
			Name: fmt.Sprintf("pod credentials issued on node %s", p.Node),
			Detail: map[string]any{
				"node": p.Node, "serial": serial, "hpke_key_sha256": hex.EncodeToString(keySum[:]),
				"bindings_version": version, "not_after": leaf.NotAfter.UTC().Format(time.RFC3339),
			},
		}); err != nil {
			return err
		}
		cert = c
		return nil
	})
	if err != nil {
		if st, ok := status.FromError(err); ok && st.Code() != codes.Unavailable {
			s.rl.warn("credentials", "pod credentials refused", "node", p.Node, "pod", podID, "code", st.Code().String(), "err", st.Message())
			return nil, st.Err()
		}
		return nil, rpcError(err, "issue pod credentials")
	}
	resp := &fleetv1.IssuePodCredentialsResponse{Certificate: cert}
	if bindings != nil {
		sealed, err := pki.Seal(hpkeKey, podID, bindings, version)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "seal the pod's bindings: %v", err)
		}
		resp.Bindings = sealed
	}
	return resp, nil
}
