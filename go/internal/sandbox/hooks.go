// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// What the environments (ADR 0018) hook into the engine. An instance row
// of an environment (sandboxes.environment_id set) is a sandbox to the
// engine; its pod is built from its pod_template, whose secrets the
// SecretResolver unseals, and the PrepareHook puts on the node what the
// pod needs before the pod call (its security groups, its volume).

// SecretResolver returns the value of version version of the secret name
// of the environment envID, for a pod template's env (model.TemplateValue
// and its parts). An error fails the op before any pod call.
type SecretResolver func(ctx context.Context, envID uuid.UUID, name string, version int) (string, error)

// PrepareHook prepares node for the op of kind on the row sb, which has a
// pod template: called in execute once the op has its connection to node
// and before the pod call (CreatePod, StartPod, ResumePod, RestorePod),
// and inside callRepair before its pod calls. A node that is not
// connected never reaches the hook: its op rolls back without a cause. An
// error before the pod call means nothing was sent: the op rolls back
// with the error as its cause, and only an error that wraps ErrNodeUnfit
// (and not model.ErrNodeUnavailable) puts the node of an unpinned create
// on avoid_nodes. Inside callRepair it counts as sent and as no call made
// (the observation decides).
type PrepareHook func(ctx context.Context, node string, sb store.Sandbox, kind OpKind) error

// ErrNodeUnfit is what a PrepareHook's error wraps when the node itself
// cannot take the pod (a volume the node refuses to create, one in use by
// another pod or failed there): a fresh unpinned create then avoids the
// node, as a create the node refused does (placementFailure). Any other
// hook error may pass (a node that went away, a transient RPC or database
// error) and is a plain rollback: avoiding the node for it would leave a
// single-node system unplaceable, since only a run clears avoid_nodes.
var ErrNodeUnfit = errors.New("the node cannot take the pod")

// engineHooks are the hooks the server sets after the engine and the
// environments both exist.
type engineHooks struct {
	mu      sync.Mutex
	secrets SecretResolver
	prepare PrepareHook
	access  func(ctx context.Context, pinned string, auth model.RegistryCredentials) error
}

// SetSecretResolver sets what a pod template's secrets are unsealed with;
// nil: a template that names a secret fails its op.
func (e *Engine) SetSecretResolver(r SecretResolver) {
	e.hooks.mu.Lock()
	defer e.hooks.mu.Unlock()
	e.hooks.secrets = r
}

// SetImageAccess replaces the registries (ADR 0025): the question before
// every create is access's, nil a yes, and no pull token is asked for, so
// the guest pulls anonymously. For tests and test clusters whose images
// name no registry that answers; nil keeps the registries as they are.
func (e *Engine) SetImageAccess(access func(ctx context.Context, pinned string, auth model.RegistryCredentials) error) {
	if access == nil {
		return
	}
	e.hooks.mu.Lock()
	defer e.hooks.mu.Unlock()
	e.hooks.access = access
}

// imagePullToken gets the guest's pull token: none when SetImageAccess
// replaced the registries, else the registry's.
func (e *Engine) imagePullToken() func(ctx context.Context, pinned string, auth model.RegistryCredentials) (string, error) {
	e.hooks.mu.Lock()
	defer e.hooks.mu.Unlock()
	if e.hooks.access != nil {
		return func(context.Context, string, model.RegistryCredentials) (string, error) { return "", nil }
	}
	return e.pullToken
}

// imageAccess is the question before a create: SetImageAccess's, or the
// registry's.
func (e *Engine) imageAccess() func(ctx context.Context, pinned string, auth model.RegistryCredentials) error {
	e.hooks.mu.Lock()
	defer e.hooks.mu.Unlock()
	if e.hooks.access != nil {
		return e.hooks.access
	}
	return e.checkImage
}

// SetPrepareHook sets the hook that prepares a node for an op on a row
// with a pod template; nil: none.
func (e *Engine) SetPrepareHook(h PrepareHook) {
	e.hooks.mu.Lock()
	defer e.hooks.mu.Unlock()
	e.hooks.prepare = h
}

// secretResolver is the resolver SetSecretResolver set, or nil.
func (e *Engine) secretResolver() SecretResolver {
	e.hooks.mu.Lock()
	defer e.hooks.mu.Unlock()
	return e.hooks.secrets
}

// prepareHook is the hook SetPrepareHook set, or nil.
func (e *Engine) prepareHook() PrepareHook {
	e.hooks.mu.Lock()
	defer e.hooks.mu.Unlock()
	return e.hooks.prepare
}

// Restart restarts the pod of a running sandbox, for an environment's
// instance whose container exited: a repair op with the trigger "restart",
// which the precheck allows for any running row and which skips the
// repair's recheck (callRepair's StopPod and StartPod; a row with volumes
// never takes the DeletePod and RestorePod branch). reason is the op's
// description. It returns once the op began; the op's outcome arrives as
// the row's state.
func (e *Engine) Restart(ctx context.Context, id podid.ID, reason string) error {
	row, err := e.st.GetSandbox(ctx, id)
	if err != nil {
		return err
	}
	if row.Op != "" {
		return errorf(model.ErrBusy, "an op %s of instance %s is in flight", row.Op, row.OpOwner)
	}
	if row.ClientState != model.StateRunning {
		return errorf(model.ErrPrecondition, "sandbox %s is %s: no restart", id, row.ClientState)
	}
	h, err := e.runner.Start(ctx, id, OpIntent{Kind: OpRepair, Trigger: TriggerRestart, Target: row.Node,
		Incarnation: row.Incarnation, reason: reason})
	if err != nil {
		return err
	}
	if h.kind != OpRepair || h.trigger != TriggerRestart {
		// This instance's op of another kind, begun after the read.
		return errorf(model.ErrBusy, "an op %s is in flight", h.kind)
	}
	return nil
}

// prepareError is a failure of what an op does before its pod call
// (prepareSpec, prepareNode): the prepare hook's, or the building of a
// create's pod spec. Nothing was sent: the op rolls back with it as its
// cause.
type prepareError struct {
	node string
	kind OpKind
	// hook: the prepare hook failed (the node's side); else the pod spec
	// (a secret that does not resolve).
	hook bool
	err  error
}

func (e *prepareError) Error() string {
	if e.hook {
		return fmt.Sprintf("prepare node %s for %s: %v", e.node, e.kind, e.err)
	}
	return fmt.Sprintf("build the pod spec for %s: %v", e.kind, e.err)
}

func (e *prepareError) Unwrap() error { return e.err }

// prepares reports whether the prepare hook runs before an op's pod call:
// the ops that bring a pod up (a repair calls it inside callRepair).
func prepares(k OpKind) bool {
	return k == OpCreate || k == OpStart || k == OpResume || k == OpRestore
}

// What an op does before its pod call, where a failure means that
// nothing was sent: a create builds its pod spec (prepareSpec: a secret
// that does not resolve fails it), the op gets its connection, then a row
// with a pod template has its node prepared by the prepare hook
// (prepareNode: create, start, resume, restore). The spec comes first, so
// that a create that cannot be sent leaves nothing on the node; the
// connection before the hook, so that a node that is not connected is a
// rollback without a cause and never the hook's failure.

// prepareSpec builds a create's pod spec.
func (e *Engine) prepareSpec(ctx context.Context, h *OpHandle) error {
	if h.kind != OpCreate {
		return nil
	}
	spec, err := e.podSpec(ctx, h.row)
	if err != nil {
		return &prepareError{node: h.target, kind: h.kind, err: err}
	}
	h.spec = spec
	h.layers = e.imageLayers(ctx, h.row.ImageRef)
	// The guest's pull token, for a sandbox with image credentials: asked
	// for now, so that it lives the registry's whole life for the pull.
	auth, err := e.registryAuth(ctx, h.row)
	if err != nil {
		return &prepareError{node: h.target, kind: h.kind, err: err}
	}
	if h.pullToken, err = e.imagePullToken()(ctx, h.row.ImageRef, auth); err != nil {
		return &prepareError{node: h.target, kind: h.kind, err: fmt.Errorf("the image's pull token: %w", err)}
	}
	return nil
}

// prepareNode runs the prepare hook before the pod call of an op that
// brings a pod up, for a row with a pod template.
func (e *Engine) prepareNode(ctx context.Context, h *OpHandle) error {
	if h.row.PodTemplate == nil || !prepares(h.kind) {
		return nil
	}
	return e.runPrepareHook(ctx, h)
}

// prepareRepair runs the prepare hook inside callRepair, before its pod
// calls, for a row with a pod template.
func (e *Engine) prepareRepair(ctx context.Context, h *OpHandle) error {
	if h.row.PodTemplate == nil {
		return nil
	}
	return e.runPrepareHook(ctx, h)
}

func (e *Engine) runPrepareHook(ctx context.Context, h *OpHandle) error {
	hook := e.prepareHook()
	if hook == nil {
		return nil
	}
	if err := hook(ctx, h.target, h.row, h.kind); err != nil {
		return &prepareError{node: h.target, kind: h.kind, hook: true, err: err}
	}
	return nil
}

// hasVolumes reports whether the row's pod has volumes: its repair never
// deletes and restores it, since a volume that a deleted pod released
// does not restore from the checkpoint.
func hasVolumes(sb store.Sandbox) bool {
	return sb.PodTemplate != nil && len(sb.PodTemplate.Volumes) > 0
}

// noteAddress records the address of the op's pod as its call answered it
// (CreatePod, RestorePod: the address is the pod's from its record on,
// before its microVM boots), so that the environments' security groups
// take it as a member while the pod comes up. Best effort: the op's finish
// writes the address it observes.
func (e *Engine) noteAddress(h *OpHandle, info *podv1.PodInfo) {
	if podIncarnation(info) != h.inc {
		return
	}
	addr := info.GetIpv6Address()
	if addr == "" || addr == h.row.Address {
		return
	}
	ctx, cancel := context.WithTimeout(e.bg, 10*time.Second)
	defer cancel()
	err := e.st.InTx(ctx, func(tx *store.Store) error {
		_, err := tx.SetSandboxAddress(ctx, h.id, h.inc, addr)
		return err
	})
	if err != nil {
		e.rl.warn("address", "cannot record a pod's address", "sandbox", h.id, "err", err)
	}
}

// ResolveImage pins an image to its digest as a sandbox's create does:
// "<registry>/<repo>@sha256:<linux/amd64 manifest digest>", asked with
// auth (anonymous when empty), honouring --deny-prefix, a tag cached for
// 60 s per asker. A digest reference is pinned without asking the
// registry.
func (e *Engine) ResolveImage(ctx context.Context, image string, auth model.RegistryCredentials) (string, error) {
	return e.resolveImage(ctx, image, auth)
}
