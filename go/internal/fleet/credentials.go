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

	// The node and the pod's placement are checked before anything of its
	// bindings is read (PodBindings may make the pod's interception CA),
	// and again in the transaction.
	if err := checkInTx(ctx, s.st, p.Node, p.Serial); err != nil {
		return nil, s.credentialsRefused(p.Node, podID, err)
	}
	if _, err := podRunOn(ctx, s.st, podID, p.Node, store.NoLock); err != nil {
		return nil, s.credentialsRefused(p.Node, podID, err)
	}

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
		sb, err := podRunOn(ctx, tx, podID, p.Node, store.ForShare)
		if err != nil {
			return err
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
		kind, name, set := sb.Workload()
		spiffeID, subject := pki.PodIdentity(s.iss.TrustDomain(), sb.Owner, sb.EnvironmentID, sb.ID, kind, name, set)
		c, leaf, err := s.iss.IssuePod(req.GetCsr(), spiffeID, subject, nil, s.cfg.PodCertTTL)
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
		return nil, s.credentialsRefused(p.Node, podID, err)
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

// podRunOn reads the pod's row with lock and checks that the pod is placed
// on node and has a run there (it is created, started, resumed or
// restored, or runs).
func podRunOn(ctx context.Context, st *store.Store, podID podid.ID, node string, lock store.RowLock) (store.Sandbox,
	error) {
	sb, err := st.LockSandbox(ctx, podID, lock)
	if errors.Is(err, model.ErrNotFound) {
		return store.Sandbox{}, status.Errorf(codes.NotFound, "no pod %s", podID)
	}
	if err != nil {
		return store.Sandbox{}, err
	}
	if sb.Node != node {
		return store.Sandbox{}, status.Errorf(codes.PermissionDenied, "pod %s is not placed on node %s", podID, node)
	}
	switch sb.ClientState {
	case model.StateCreating, model.StateRunning, model.StateResuming:
	default:
		return store.Sandbox{}, status.Errorf(codes.PermissionDenied, "pod %s has no run on node %s", podID, node)
	}
	return sb, nil
}

// credentialsRefused is the answer of IssuePodCredentials that failed with
// err: a refusal as it is, logged; anything else as rpcError.
func (s *Service) credentialsRefused(node string, podID podid.ID, err error) error {
	if st, ok := status.FromError(err); ok && st.Code() != codes.Unavailable {
		s.rl.warn("credentials", "pod credentials refused", "node", node, "pod", podID, "code", st.Code().String(),
			"err", st.Message())
		return st.Err()
	}
	return rpcError(err, "issue pod credentials")
}
