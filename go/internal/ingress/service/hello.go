// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/gardenlinux/feos/go/internal/activity"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/mesh"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

// notMeshedText is what a Hello on a cluster without the pods' mesh is
// answered with (FAILED_PRECONDITION).
const notMeshedText = "the cluster is not meshed; the ingress needs the pods' mesh"

// Hello is the ingress's Hello (§5 of the ingress's design), the first
// call on a connection with a certificate. In one transaction, under the
// ingress's row lock: a removed ingress, or a certificate that no longer
// passes CheckIngress's conditions, is refused (PERMISSION_DENIED); the
// presenting certificate becomes current when it is newer than the
// current one, and the older ones are superseded, revoked one grace ahead
// (store.PresentIngressCert); the ingress takes its place on the pods'
// mesh with its WireGuard key and endpoint (store.JoinIngressMesh: its /96
// at the first Hello after a join, the same one after; the key is fixed
// until a join again, FAILED_PRECONDITION otherwise); and it is seen with
// its version. The key must not be a point of small order
// (INVALID_ARGUMENT); at the first Hello after a join it must not be the
// one the join again retired, nor another peer's (FAILED_PRECONDITION
// "…; make a new one"). The answer is the cluster's ULA, the ingress's /96
// and address, the apps zone and the answering instance. A Hello changes
// nothing when repeated.
func (s *Service) Hello(ctx context.Context, req *wire.HelloRequest) (*wire.HelloResponse, error) {
	p, err := ingressPeer(ctx)
	if err != nil {
		return nil, err
	}
	ula := s.cfg.PodNetwork
	if !ula.IsValid() {
		return nil, status.Error(codes.FailedPrecondition, notMeshedText)
	}
	if len(req.PublicKey) != mesh.KeyLen {
		return nil, status.Errorf(codes.InvalidArgument, "publicKey has %d bytes, not %d", len(req.PublicKey), mesh.KeyLen)
	}
	if err := mesh.CheckKey(req.PublicKey); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "publicKey: %v", err)
	}
	endpoint, err := mesh.CheckEndpoint(req.Endpoint)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, activity.Clean(err.Error()))
	}
	if len(req.Version) > maxVersionLen {
		return nil, status.Errorf(codes.InvalidArgument, "version has more than %d bytes", maxVersionLen)
	}
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	var j store.IngressMeshJoin
	var superseded int64
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		j, superseded = store.IngressMeshJoin{}, 0
		in, err := tx.LockIngress(ctx, p.Name, store.ForUpdate)
		if errors.Is(err, store.ErrNoRow) {
			return status.Errorf(codes.PermissionDenied, "certificate %s is not ingress %s's", p.Serial, p.Name)
		}
		if err != nil {
			return err
		}
		if in.Status == model.IngressRemoved {
			return status.Errorf(codes.PermissionDenied, "ingress %s was removed; join it again", p.Name)
		}
		// A certificate revoked since the connection's last check (a join
		// again) must not become current: it would supersede the
		// certificate of the join.
		if err := checkInTx(ctx, tx, p.Name, p.Serial); err != nil {
			return err
		}
		if superseded, err = tx.PresentIngressCert(ctx, p.Name, p.Serial, s.cfg.SupersedeGrace); err != nil {
			if errors.Is(err, store.ErrNoRow) {
				return status.Errorf(codes.PermissionDenied, "certificate %s is not ingress %s's", p.Serial, p.Name)
			}
			return err
		}
		if j, err = tx.JoinIngressMesh(ctx, in, ula, req.PublicKey, endpoint); err != nil {
			return err
		}
		return tx.SeenIngress(ctx, p.Name, req.Version)
	})
	switch {
	case errors.Is(err, store.ErrIngressKeyFixed):
		s.rl.warn("hello-key", "ingress Hello refused: its mesh key is fixed", "ingress", p.Name, "serial", p.Serial, "peer", p.Addr)
		return nil, status.Errorf(codes.FailedPrecondition, "the mesh key of ingress %s is fixed; join it again to change it", p.Name)
	case errors.Is(err, store.ErrIngressKeyRetired):
		s.rl.warn("hello-key-retired", "ingress Hello refused: its mesh key is the one its join again retired", "ingress", p.Name,
			"serial", p.Serial, "peer", p.Addr)
		return nil, status.Errorf(codes.FailedPrecondition, "the mesh key of ingress %s was retired by its join again; make a new one",
			p.Name)
	case errors.Is(err, store.ErrMeshKeyTaken):
		s.rl.warn("hello-key-taken", "ingress Hello refused: its mesh key is another peer's", "ingress", p.Name, "serial", p.Serial,
			"peer", p.Addr)
		return nil, status.Errorf(codes.FailedPrecondition, "the mesh key of ingress %s is another peer's; make a new one", p.Name)
	case errors.Is(err, store.ErrMeshExhausted):
		s.log.Error("an ingress cannot join the pods' mesh: every /96 of it has been handed out", "ingress", p.Name,
			"pod_network", ula.String())
		return nil, status.Errorf(codes.ResourceExhausted, "every /96 of the pod network %s has been handed out", ula)
	case errors.Is(err, store.ErrNotMeshed):
		return nil, status.Error(codes.FailedPrecondition, notMeshedText)
	case err != nil:
		if st, ok := refused(err); ok {
			s.rl.warn("hello", "ingress Hello refused", "ingress", p.Name, "serial", p.Serial, "code", st.Code().String(),
				"err", st.Message())
			return nil, st.Err()
		}
		return nil, rpcError(err, "hello")
	}
	s.markHello(p)
	if superseded > 0 {
		s.log.Info("ingress presented a newer certificate; the older ones are superseded", "ingress", p.Name,
			"serial", p.Serial, "superseded", superseded, "grace", s.cfg.SupersedeGrace)
	}
	if j.Changed || j.AddressChanged {
		s.log.Info("ingress is on the pods' mesh", "ingress", p.Name, "prefix", j.Prefix, "address", j.Address,
			"endpoint", endpoint)
	}
	return &wire.HelloResponse{
		Name: p.Name, PodNetwork: ula.String(), Prefix: j.Prefix, Address: j.Address,
		AppsZone: s.cfg.AppsZone, TenantZone: s.cfg.TenantZone, Instance: s.cfg.InstanceID,
	}, nil
}
