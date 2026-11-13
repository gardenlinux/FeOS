// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	envspec "github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/pki"
)

// tlsCallTimeout bounds SetPodTlsBindings: the guest listens on the
// bindings before the node answers.
const tlsCallTimeout = 30 * time.Second

// errNoTrustDomain is a TLS binding on an instance without a trust
// domain: no pod has an identity to present.
var errNoTrustDomain = errors.New("TLS bindings need a trust domain, which this instance has none of")

// podTLSBindings are a template's TLS bindings as a pod's spec names them,
// each pinned to its addon's ID in the environment envID of owner.
func podTLSBindings(td, owner string, envID uuid.UUID, bindings []model.TemplateTLSBinding) []*podv1.TlsBinding {
	out := make([]*podv1.TlsBinding, 0, len(bindings))
	for _, b := range bindings {
		out = append(out, &podv1.TlsBinding{Listen: b.Listen, Host: b.Host, Port: uint32(b.Port), Alpn: b.ALPN,
			ServerId: pki.WorkloadID(td, owner, envID, envspec.KindAddon, b.Addon)})
	}
	return out
}

// SetTLSBindings gives the sandbox's pod the TLS bindings (ADR 0036) of the
// attached services in it, stated whole: written into its template, which
// every later create of its pod carries, and sent to the pod of lease
// (SetPodTlsBindings), whose guest listens on them when it answers. The
// sandbox is made before its attached services, so its create carried
// none of theirs. A node too old for the call is an error: the service's
// references would reach nothing.
func (e *Engine) SetTLSBindings(ctx context.Context, lease model.Lease, bindings []model.TemplateTLSBinding) error {
	if len(bindings) > 0 && e.cfg.TrustDomain == "" {
		return errNoTrustDomain
	}
	row, err := e.st.GetSandbox(ctx, lease.SandboxID)
	if err != nil {
		return err
	}
	if row.Incarnation != lease.Incarnation {
		return fmt.Errorf("sandbox %s is in another incarnation", lease.SandboxID)
	}
	var have []model.TemplateTLSBinding
	if row.PodTemplate != nil {
		have = row.PodTemplate.TLSBindings
	}
	if !slices.Equal(have, bindings) {
		ok, err := e.st.SetSandboxTLSBindings(ctx, row.ID, row.Incarnation, bindings)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("sandbox %s is gone", row.ID)
		}
		e.log.Info("sandbox TLS bindings set", "sandbox", row.ID, "bindings", len(bindings))
	}
	return e.pushTLSBindings(ctx, lease, row.Owner, row.EnvironmentID, bindings)
}

// pushTLSBindings sends the pod of lease its TLS bindings; the node does
// nothing when it has them already. The call runs on a context of its
// own: a mutating pod call never runs on its caller's.
func (e *Engine) pushTLSBindings(ctx context.Context, lease model.Lease, owner string, envID uuid.UUID,
	bindings []model.TemplateTLSBinding) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tlsCallTimeout)
	defer cancel()
	pc, err := e.podClient(cctx, lease.Node)
	if err != nil {
		return err
	}
	req := &podv1.SetPodTlsBindingsRequest{PodId: lease.SandboxID.String()}
	if len(bindings) > 0 {
		req.TlsBindings = podTLSBindings(e.cfg.TrustDomain, owner, envID, bindings)
	}
	_, err = pc.SetPodTlsBindings(cctx, req)
	switch grpcCode(err) {
	case codes.OK:
		return nil
	case codes.Unimplemented:
		return fmt.Errorf("node %s does not take SetPodTlsBindings: its feos-node is older than TLS bindings "+
			"for attached services", lease.Node)
	default:
		return fmt.Errorf("SetPodTlsBindings on node %s: %w", lease.Node, err)
	}
}
