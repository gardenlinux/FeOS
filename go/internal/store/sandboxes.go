// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// Sandbox is a row of sandboxes: every column, Go-typed. A sandbox's row
// exists exactly while the sandbox exists.
type Sandbox struct {
	ID          podid.ID  // = the pod's ID on feosd
	Incarnation uuid.UUID // the pod label ferun.incarnation
	Owner       string    // the tenant
	Image       string    // as the client gave it
	ImageRef    string    // resolved repo@sha256:..., fixed at the first materialisation
	VCPUs       int
	MemoryMiB   int

	ClientState       string // model.State*
	StatusDescription string
	Node              string // "" = NULL: no node
	Materialised      bool   // a pod of this incarnation was seen on Node
	LostFrom          string // "" = NULL
	AvoidNodes        []string

	Op          string    // "" when no op is in flight
	OpID        uuid.UUID // uuid.Nil when no op is in flight
	OpOwner     string
	OpDeadline  time.Time
	OpTarget    string
	OpFromState string

	Gen int64 // +1 on every lifecycle write

	CheckpointEpoch  int64
	CheckpointHead   string
	CheckpointMemory bool
	CheckpointAt     time.Time
	CheckpointStore  string

	PendingSince     time.Time
	UnreachableSince time.Time
	RunID            string
	KubeVersion      string
	SSHVersion       string
	// HTTPVersion is the version of the sandbox's own HTTP set (ADR 0028),
	// "" for none; HTTPPushed the version of the clear part of the pod's
	// HTTP bindings that its node took last ("" for none).
	HTTPVersion string
	HTTPPushed  string

	CreatedAt    time.Time
	UpdatedAt    time.Time
	LastActiveAt time.Time
	PausedAt     time.Time

	// PolicyFailed names the policy, PolicyIdle or PolicyHibernateAfter,
	// whose last PolicyFailures ops for the sandbox failed in a row; that
	// policy's loop begins none before PolicyNotBefore. Empty (0, zero):
	// no policy is backed off. Each policy heeds only its own backoff.
	PolicyFailed    string
	PolicyFailures  int
	PolicyNotBefore time.Time

	// EnvironmentID is the environment the row belongs to (ADR 0035):
	// an instance's, a sandbox's, a tenant's job's. Fixed at the insert,
	// as are Instance and PodTemplate (nil: NULL, a pod without one), but
	// a template's legacy groups, which DropLegacyGroups drops.
	EnvironmentID uuid.UUID
	// Instance makes the row an instance of its environment (ADR 0018),
	// its pod built from a deployment's template; fixed at the insert.
	Instance    bool
	PodTemplate *model.PodTemplate
	// Reaches is what of its environment a sandbox's or a tenant's job's
	// pod reaches (ADR 0035, revised); nothing for an instance, whose
	// references are its deployment's. Written by SetSandboxReaches.
	// Egress is its egress (nil: the internet), fixed at the insert.
	Reaches spec.Reaches
	Egress  *spec.Egress
	// PinNode is the only node a create may go to ("": any).
	PinNode string
	// IdleAfterS is the idle pause's interval in seconds: nil (NULL) the
	// instance's --idle-pause, 0 never.
	IdleAfterS *int
	// Address is the pod's IPv6 address as the engine last observed it;
	// "" when there is none (no pod, or a hibernated one).
	Address string
	// TenantJob makes the row a tenant's job's pod (ADR 0018), its ID the
	// job's; fixed at the insert.
	TenantJob bool
	// LogLabels are the labels of the pod's container logs (ADR 0026),
	// sent with its create; fixed at the insert.
	LogLabels map[string]string
}

// IsOwn reports whether the row is a sandbox, the tenant's own (POST
// /api/v1/sandboxes): neither an environment's instance nor a tenant's
// job's pod, which the sandbox doors treat as absent.
func (s *Sandbox) IsOwn() bool { return !s.Instance && !s.TenantJob }

// Workload is the workload the row's pod is part of (ADR 0036): an
// instance's object, by the kind and the object its log labels name
// (fixed at the insert), with the ID its pods' set carries on the nodes
// (model.WorkloadGroupID, ADR 0034); a tenant's job or a sandbox by the
// row's ID. kind is "" for a row outside an environment, or an instance
// whose labels name no object.
func (s *Sandbox) Workload() (kind, name, set string) {
	if s.EnvironmentID == uuid.Nil {
		return "", "", ""
	}
	switch {
	case s.Instance:
		kind, name = s.LogLabels[model.LogKind], s.LogLabels[model.LogObject]
		switch kind {
		case spec.KindService, spec.KindJob, spec.KindAddon:
		default:
			return "", "", ""
		}
		if name == "" {
			return "", "", ""
		}
		return kind, name, model.WorkloadGroupID(s.EnvironmentID, kind, name)
	case s.TenantJob:
		return "tenant-job", s.ID.String(), model.JobWorkloadGroupID(s.ID.String())
	default:
		return "sandbox", s.ID.String(), model.SandboxWorkloadGroupID(s.ID.String())
	}
}

// The policies that back off after a failed op (Sandbox.PolicyFailed).
const (
	PolicyIdle           = "idle"
	PolicyHibernateAfter = "hibernate_after"
)

// HasCheckpoint reports whether the sandbox has a checkpoint to restore.
func (s *Sandbox) HasCheckpoint() bool { return s.CheckpointHead != "" }

// sandboxColumns lists the columns of sandboxes in the order scanSandbox
// reads them, each prefixed with p and a dot when p is not empty.
func sandboxColumns(p string) string {
	cols := []string{"id", "incarnation", "owner", "image", "image_ref", "vcpus", "memory_mib",
		"client_state", "status_description", "node", "materialised", "lost_from", "avoid_nodes",
		"op", "op_id", "op_owner", "op_deadline", "op_target", "op_from_state", "gen",
		"checkpoint_epoch", "checkpoint_head", "checkpoint_memory", "checkpoint_at", "checkpoint_store",
		"pending_since", "unreachable_since", "run_id", "kube_version", "ssh_version",
		"created_at", "updated_at", "last_active_at", "paused_at", "policy_failed", "policy_failures",
		"policy_not_before", "environment_id", "instance", "pod_template", "pin_node", "idle_after_s", "address",
		"tenant_job", "log_labels", "http_version", "http_pushed", "reaches", "egress"}
	if p != "" {
		for i, c := range cols {
			cols[i] = p + "." + c
		}
	}
	return strings.Join(cols, ", ")
}

var sandboxCols = sandboxColumns("")

func scanSandbox(row pgx.Row) (Sandbox, error) {
	var s Sandbox
	var node, lostFrom *string
	var opID *uuid.UUID
	var opDeadline, cpAt, pendingSince, unreachableSince, pausedAt, policyNotBefore *time.Time
	var template, reaches, egress []byte
	err := row.Scan(&s.ID, &s.Incarnation, &s.Owner, &s.Image, &s.ImageRef, &s.VCPUs, &s.MemoryMiB,
		&s.ClientState, &s.StatusDescription, &node, &s.Materialised, &lostFrom, &s.AvoidNodes,
		&s.Op, &opID, &s.OpOwner, &opDeadline, &s.OpTarget, &s.OpFromState, &s.Gen,
		&s.CheckpointEpoch, &s.CheckpointHead, &s.CheckpointMemory, &cpAt, &s.CheckpointStore,
		&pendingSince, &unreachableSince, &s.RunID, &s.KubeVersion, &s.SSHVersion,
		&s.CreatedAt, &s.UpdatedAt, &s.LastActiveAt, &pausedAt, &s.PolicyFailed, &s.PolicyFailures,
		&policyNotBefore, &s.EnvironmentID, &s.Instance, &template, &s.PinNode, &s.IdleAfterS, &s.Address,
		&s.TenantJob, &s.LogLabels, &s.HTTPVersion, &s.HTTPPushed, &reaches, &egress)
	if err != nil {
		return Sandbox{}, err
	}
	s.Node, s.LostFrom, s.OpID = stringOf(node), stringOf(lostFrom), uuidOf(opID)
	s.OpDeadline, s.CheckpointAt, s.PendingSince = timeOf(opDeadline), timeOf(cpAt), timeOf(pendingSince)
	s.UnreachableSince, s.PausedAt, s.PolicyNotBefore = timeOf(unreachableSince), timeOf(pausedAt), timeOf(policyNotBefore)
	s.AvoidNodes, s.LogLabels = nonNil(s.AvoidNodes), stringMap(s.LogLabels)
	if s.Reaches, s.Egress, err = scanReach(reaches, egress); err != nil {
		return Sandbox{}, fmt.Errorf("sandbox %s: %w", s.ID, err)
	}
	if template != nil {
		var t model.PodTemplate
		if err := json.Unmarshal(template, &t); err != nil {
			return Sandbox{}, fmt.Errorf("sandbox %s: pod_template: %w", s.ID, err)
		}
		s.PodTemplate = &t
	}
	return s, nil
}

// templateJSON is a pod template as pod_template stores it: nil for none
// (NULL).
func templateJSON(t *model.PodTemplate) (any, error) {
	if t == nil {
		return nil, nil
	}
	b, err := json.Marshal(t)
	if err != nil {
		return nil, fmt.Errorf("pod template: %w", err)
	}
	return string(b), nil
}

// sameTemplate reports whether two rows' templates are the same: both
// none, or equal as JSON.
func sameTemplate(a, b *model.PodTemplate) bool {
	if a == nil || b == nil {
		return a == b
	}
	ja, errA := templateJSON(a)
	jb, errB := templateJSON(b)
	return errA == nil && errB == nil && ja == jb
}

func sameIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func collectSandboxes(rows pgx.Rows, err error) ([]Sandbox, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Sandbox, error) { return scanSandbox(r) })
}

// RowLock is the lock a read takes on the rows it returns.
type RowLock string

const (
	NoLock         RowLock = ""
	ForUpdate      RowLock = "FOR UPDATE"
	ForNoKeyUpdate RowLock = "FOR NO KEY UPDATE"
	ForShare       RowLock = "FOR SHARE"
)

// GetSandbox reads a sandbox without a lock; model.ErrNotFound if it does
// not exist.
func (s *Store) GetSandbox(ctx context.Context, id podid.ID) (Sandbox, error) {
	return s.LockSandbox(ctx, id, NoLock)
}

// LockSandbox reads a sandbox and locks its row with lock (inside a
// transaction); model.ErrNotFound if it does not exist.
func (s *Store) LockSandbox(ctx context.Context, id podid.ID, lock RowLock) (Sandbox, error) {
	sb, err := scanSandbox(s.q.QueryRow(ctx, "SELECT "+sandboxCols+" FROM sandboxes WHERE id = $1 "+string(lock), id))
	if err != nil {
		return Sandbox{}, noRow(err, model.ErrNotFound)
	}
	return sb, nil
}

// NewSandbox is what a create request sets; every other column takes its
// default (a new incarnation, client_state pending).
type NewSandbox struct {
	ID        podid.ID
	Owner     string
	Image     string
	VCPUs     int
	MemoryMiB int
	// ImageRef is the image pinned to its digest, when the caller resolved
	// it already (an environment's instances of one template share one);
	// "": the first create resolves Image.
	ImageRef string
	// EnvironmentID is the environment the row belongs to (the caller
	// holds the tenant and the environment, and refuses one being
	// deleted); required.
	EnvironmentID uuid.UUID
	// Instance makes the row an instance of the environment.
	Instance bool
	// Reaches and Egress are a sandbox's or a tenant's job's (validated
	// by the caller); an instance has neither.
	Reaches spec.Reaches
	Egress  *spec.Egress
	// PodTemplate is what the pod is built from; nil: the sandbox's own
	// pod.
	PodTemplate *model.PodTemplate
	PinNode     string
	// IdleAfterS: nil the instance's --idle-pause, 0 never, n seconds.
	IdleAfterS *int
	// TenantJob makes the row a tenant's job's pod (the caller inserts
	// the job under its ID in the same transaction).
	TenantJob bool
	// LogLabels are the labels of the pod's logs (model.Log*); nil: none.
	LogLabels map[string]string
	// MadeBy is the principal that asked for it (ADR 0031); "": ferund.
	MadeBy string
}

// CreateSandbox inserts the sandbox unless its ID exists. It returns the
// new row and true, or the existing row (of whatever owner) and false: a
// caller that minted the ID draws another (podid.Mint). An insert sends
// ferun_sandbox.
func (s *Store) CreateSandbox(ctx context.Context, n NewSandbox) (Sandbox, bool, error) {
	template, err := templateJSON(n.PodTemplate)
	if err != nil {
		return Sandbox{}, false, fmt.Errorf("create sandbox %s: %w", n.ID, err)
	}
	logLabels, err := jsonText(stringMap(n.LogLabels))
	if err != nil {
		return Sandbox{}, false, fmt.Errorf("create sandbox %s: log labels: %w", n.ID, err)
	}
	if n.EnvironmentID == uuid.Nil {
		return Sandbox{}, false, fmt.Errorf("create sandbox %s: no environment", n.ID)
	}
	reaches, egress, err := reachJSON(n.Reaches, n.Egress)
	if err != nil {
		return Sandbox{}, false, fmt.Errorf("create sandbox %s: %w", n.ID, err)
	}
	for range 3 {
		sb, err := scanSandbox(s.q.QueryRow(ctx, `INSERT INTO sandboxes (id, owner, image, vcpus, memory_mib, image_ref,
				environment_id, instance, pod_template, pin_node, idle_after_s, tenant_job, log_labels, made_by,
				reaches, egress)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10, $11, $12, $13::jsonb, $14, $15::jsonb, $16::jsonb)
			ON CONFLICT (id) DO NOTHING RETURNING `+sandboxCols,
			n.ID, n.Owner, n.Image, n.VCPUs, n.MemoryMiB, n.ImageRef, n.EnvironmentID, n.Instance, template, n.PinNode,
			n.IdleAfterS, n.TenantJob, logLabels, n.MadeBy, reaches, egress))
		if err == nil {
			return sb, true, s.Notify(ctx, chanSandbox, sb.ID.String())
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return Sandbox{}, false, err
		}
		sb, err = s.GetSandbox(ctx, n.ID)
		if err == nil {
			return sb, false, nil
		}
		if !errors.Is(err, model.ErrNotFound) {
			return Sandbox{}, false, err
		}
		// Deleted between the insert and the read: insert again.
	}
	return Sandbox{}, false, fmt.Errorf("create sandbox %s: the id keeps being deleted and re-created", n.ID)
}

// mintAttempts bounds the IDs CreateMintedSandbox draws: of about 2^54,
// a second draw is rare already.
const mintAttempts = 8

// CreateMintedSandbox inserts a sandbox under a new short ID
// (podid.Mint), unique over all tenants: an ID that is taken is drawn
// again. of makes the row of an ID drawn (its ID set to it; a template
// may name it). It returns the new row; the insert sends ferun_sandbox.
func (s *Store) CreateMintedSandbox(ctx context.Context, of func(id podid.ID) NewSandbox) (Sandbox, error) {
	for range mintAttempts {
		id := podid.Mint()
		n := of(id)
		if n.ID != id {
			return Sandbox{}, fmt.Errorf("create sandbox %s: the new row names %s", id, n.ID)
		}
		sb, created, err := s.CreateSandbox(ctx, n)
		if err != nil {
			return Sandbox{}, err
		}
		if created {
			return sb, nil
		}
	}
	return Sandbox{}, fmt.Errorf("create sandbox: %d new IDs in a row were taken", mintAttempts)
}

// DeleteSandbox removes the row (its tokens, holds, watches, bindings and
// booking cascade) and sends ferun_sandbox. It reports whether a row was
// there.
func (s *Store) DeleteSandbox(ctx context.Context, id podid.ID) (bool, error) {
	tag, err := s.q.Exec(ctx, "DELETE FROM sandboxes WHERE id = $1", id)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	return true, s.Notify(ctx, chanSandbox, id.String())
}

// ListSandboxes returns the sandboxes of owner, without the instance rows
// of its environments and its jobs' pods, or every row when owner is "",
// ordered by created_at and id. envs, when not nil, keeps those of these
// environments.
func (s *Store) ListSandboxes(ctx context.Context, owner string, envs []uuid.UUID) ([]Sandbox, error) {
	if owner == "" {
		return collectSandboxes(s.q.Query(ctx, "SELECT "+sandboxCols+" FROM sandboxes ORDER BY created_at, id"))
	}
	return collectSandboxes(s.q.Query(ctx, "SELECT "+sandboxCols+` FROM sandboxes
		WHERE owner = $1 AND NOT instance AND NOT tenant_job AND ($2::uuid[] IS NULL OR environment_id = ANY($2))
		ORDER BY created_at, id`, owner, envs))
}

// LockSandboxesOfOwner locks up to limit sandboxes of owner, ordered by id:
// a batch of a tenant's deletion.
func (s *Store) LockSandboxesOfOwner(ctx context.Context, owner string, limit int) ([]Sandbox, error) {
	return collectSandboxes(s.q.Query(ctx, "SELECT "+sandboxCols+" FROM sandboxes WHERE owner = $1 ORDER BY id LIMIT $2 FOR UPDATE",
		owner, limit))
}

// CountSandboxesOfOwner counts a tenant's sandboxes.
func (s *Store) CountSandboxesOfOwner(ctx context.Context, owner string) (int64, error) {
	var n int64
	err := s.q.QueryRow(ctx, "SELECT count(*) FROM sandboxes WHERE owner = $1", owner).Scan(&n)
	return n, err
}

// SandboxesOfNode returns the sandboxes whose node or lost_from is node:
// the snapshot an inventory reconcile starts from.
func (s *Store) SandboxesOfNode(ctx context.Context, node string) ([]Sandbox, error) {
	return collectSandboxes(s.q.Query(ctx, "SELECT "+sandboxCols+" FROM sandboxes WHERE node = $1 OR lost_from = $1 ORDER BY id", node))
}

// CountSandboxesOnNode counts the sandboxes whose node is node.
func (s *Store) CountSandboxesOnNode(ctx context.Context, node string) (int64, error) {
	var n int64
	err := s.q.QueryRow(ctx, "SELECT count(*) FROM sandboxes WHERE node = $1", node).Scan(&n)
	return n, err
}

// SandboxStateCounts counts the sandboxes of every node by client_state:
// node → state → count.
func (s *Store) SandboxStateCounts(ctx context.Context) (map[string]map[string]int64, error) {
	rows, err := s.q.Query(ctx, "SELECT node, client_state, count(*) FROM sandboxes WHERE node IS NOT NULL GROUP BY node, client_state")
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]int64{}
	var node, state string
	var n int64
	_, err = pgx.ForEachRow(rows, []any{&node, &state, &n}, func() error {
		if out[node] == nil {
			out[node] = map[string]int64{}
		}
		out[node][state] = n
		return nil
	})
	return out, err
}

// SteadyPod is a sandbox the registry holds as RUNNING or SUSPENDED on a
// node: running or paused, without an op.
type SteadyPod struct {
	ID    podid.ID
	State string // model.StateRunning or model.StatePaused
}

// SteadyPods returns the steady sandboxes of node ordered by ID, byte by
// byte (the column's collation is C): the lines of the registry's
// inventory hash.
func (s *Store) SteadyPods(ctx context.Context, node string) ([]SteadyPod, error) {
	rows, err := s.q.Query(ctx, `SELECT id, client_state FROM sandboxes
		WHERE node = $1 AND op = '' AND client_state IN ('running', 'paused') ORDER BY id`, node)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (SteadyPod, error) {
		var p SteadyPod
		err := r.Scan(&p.ID, &p.State)
		return p, err
	})
}

// tapHolds is the condition on a sandbox row s under which its pod may hold
// a TAP device on s.node: not hibernated, pending or lost, or an op in
// flight.
const tapHolds = "(s.client_state NOT IN ('hibernated', 'pending', 'lost') OR s.op <> '')"

// tapShares is the condition on a sandbox row s under which its pod's TAP
// device has the name of the pod with the ID $<n>: both are UUIDs, and
// they differ but share their first 8 hex digits (podid.TAPConflict). A
// short ID shares no TAP device's name, also not when its first 8
// characters are hex digits. The index sandboxes_tap serves it.
func tapShares(n int) string {
	return fmt.Sprintf("length($%d::text) = 36 AND length(s.id) = 36 AND left(s.id, 8) = left($%d::text, 8) AND s.id <> $%d::text",
		n, n, n)
}

// TAPConflict returns another sandbox on node whose pod's TAP device has
// the name of id's and may exist there (placement's TAP prefix gate:
// feosd names a UUID's TAP device after its first 8 hex digits); "" when
// there is none, always for a short ID, whose TAP device's name is its
// own (ADR 0019).
func (s *Store) TAPConflict(ctx context.Context, node string, id podid.ID) (podid.ID, error) {
	if !id.IsLegacy() {
		return "", nil
	}
	var other podid.ID
	err := s.q.QueryRow(ctx, `SELECT s.id FROM sandboxes s
		WHERE s.node = $1 AND `+tapShares(2)+` AND `+tapHolds+`
		ORDER BY s.id LIMIT 1`, node, id).Scan(&other)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return other, err
}

// TAPConflicts returns, per node, a sandbox that blocks id there as
// TAPConflict does.
func (s *Store) TAPConflicts(ctx context.Context, id podid.ID) (map[string]podid.ID, error) {
	out := map[string]podid.ID{}
	if !id.IsLegacy() {
		return out, nil
	}
	rows, err := s.q.Query(ctx, `SELECT DISTINCT ON (s.node) s.node, s.id FROM sandboxes s
		WHERE s.node IS NOT NULL AND `+tapShares(1)+` AND `+tapHolds+`
		ORDER BY s.node, s.id`, id)
	if err != nil {
		return nil, err
	}
	var node string
	var other podid.ID
	_, err = pgx.ForEachRow(rows, []any{&node, &other}, func() error {
		out[node] = other
		return nil
	})
	return out, err
}

// Attribution is the sandbox a node's record about a pod belongs to.
type Attribution struct {
	SandboxID podid.ID
	TenantID  string
}

// AttributePod decides whether a pod-scoped record from node about podID
// is the sandbox's: the row exists and the pod is or was on node (node,
// op_target or lost_from is node, a tombstone (podID, node) exists, or a
// pod certificate for it on node was issued within a day). ok is false for
// a foreign record.
func (s *Store) AttributePod(ctx context.Context, podID podid.ID, node string) (a Attribution, ok bool, err error) {
	err = s.q.QueryRow(ctx, `SELECT s.id, s.owner FROM sandboxes s WHERE s.id = $1 AND (
			s.node = $2 OR s.op_target = $2 OR s.lost_from = $2
			OR EXISTS (SELECT 1 FROM pod_tombstones t WHERE t.pod_id = $1 AND t.node = $2)
			OR EXISTS (SELECT 1 FROM issued_certs c WHERE c.pod_id = $1 AND c.node = $2
				AND c.issued_at > now() - interval '1 day'))`, podID, node).Scan(&a.SandboxID, &a.TenantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Attribution{}, false, nil
	}
	if err != nil {
		return Attribution{}, false, err
	}
	return a, true, nil
}

// sandboxColumn describes a column UpdateSandbox may write.
type sandboxColumn struct {
	name string
	// lifecycle: a write bumps gen and sends ferun_sandbox.
	lifecycle bool
	equal     func(a, b *Sandbox) bool
	value     func(s *Sandbox) any
}

func plainColumn[T comparable](name string, lifecycle bool, get func(*Sandbox) T) sandboxColumn {
	return sandboxColumn{name: name, lifecycle: lifecycle,
		equal: func(a, b *Sandbox) bool { return get(a) == get(b) },
		value: func(s *Sandbox) any { return get(s) }}
}

func timeColumn(name string, lifecycle bool, get func(*Sandbox) time.Time) sandboxColumn {
	return sandboxColumn{name: name, lifecycle: lifecycle,
		equal: func(a, b *Sandbox) bool { return get(a).Equal(get(b)) },
		value: func(s *Sandbox) any { return nullTime(get(s)) }}
}

func nullableColumn(name string, lifecycle bool, get func(*Sandbox) string) sandboxColumn {
	return sandboxColumn{name: name, lifecycle: lifecycle,
		equal: func(a, b *Sandbox) bool { return get(a) == get(b) },
		value: func(s *Sandbox) any { return nullString(get(s)) }}
}

// sandboxWritable lists the columns UpdateSandbox writes. A lifecycle
// write is one of client_state, node, op*, incarnation, materialised or
// the checkpoint columns. id, owner, image, vcpus, memory_mib,
// environment_id, instance, pod_template, tenant_job, log_labels and
// created_at are never written; gen and updated_at are UpdateSandbox's;
// http_version, http_pushed, reaches and a template's legacy groups are
// written by queries of their own (SetHTTPBindings, SetHTTPPushed,
// SetSandboxReaches, DropLegacyGroups) and left as they are.
var sandboxWritable = []sandboxColumn{
	plainColumn("incarnation", true, func(s *Sandbox) uuid.UUID { return s.Incarnation }),
	plainColumn("image_ref", false, func(s *Sandbox) string { return s.ImageRef }),
	plainColumn("client_state", true, func(s *Sandbox) string { return s.ClientState }),
	plainColumn("status_description", false, func(s *Sandbox) string { return s.StatusDescription }),
	nullableColumn("node", true, func(s *Sandbox) string { return s.Node }),
	plainColumn("materialised", true, func(s *Sandbox) bool { return s.Materialised }),
	nullableColumn("lost_from", false, func(s *Sandbox) string { return s.LostFrom }),
	{name: "avoid_nodes", equal: func(a, b *Sandbox) bool { return slices.Equal(a.AvoidNodes, b.AvoidNodes) },
		value: func(s *Sandbox) any { return nonNil(s.AvoidNodes) }},
	plainColumn("op", true, func(s *Sandbox) string { return s.Op }),
	{name: "op_id", lifecycle: true, equal: func(a, b *Sandbox) bool { return a.OpID == b.OpID },
		value: func(s *Sandbox) any { return nullUUID(s.OpID) }},
	plainColumn("op_owner", true, func(s *Sandbox) string { return s.OpOwner }),
	timeColumn("op_deadline", true, func(s *Sandbox) time.Time { return s.OpDeadline }),
	plainColumn("op_target", true, func(s *Sandbox) string { return s.OpTarget }),
	plainColumn("op_from_state", true, func(s *Sandbox) string { return s.OpFromState }),
	plainColumn("checkpoint_epoch", true, func(s *Sandbox) int64 { return s.CheckpointEpoch }),
	plainColumn("checkpoint_head", true, func(s *Sandbox) string { return s.CheckpointHead }),
	plainColumn("checkpoint_memory", true, func(s *Sandbox) bool { return s.CheckpointMemory }),
	timeColumn("checkpoint_at", true, func(s *Sandbox) time.Time { return s.CheckpointAt }),
	plainColumn("checkpoint_store", true, func(s *Sandbox) string { return s.CheckpointStore }),
	timeColumn("pending_since", false, func(s *Sandbox) time.Time { return s.PendingSince }),
	timeColumn("unreachable_since", false, func(s *Sandbox) time.Time { return s.UnreachableSince }),
	plainColumn("run_id", false, func(s *Sandbox) string { return s.RunID }),
	plainColumn("kube_version", false, func(s *Sandbox) string { return s.KubeVersion }),
	plainColumn("ssh_version", false, func(s *Sandbox) string { return s.SSHVersion }),
	timeColumn("last_active_at", false, func(s *Sandbox) time.Time { return s.LastActiveAt }),
	timeColumn("paused_at", false, func(s *Sandbox) time.Time { return s.PausedAt }),
	plainColumn("policy_failed", false, func(s *Sandbox) string { return s.PolicyFailed }),
	plainColumn("policy_failures", false, func(s *Sandbox) int { return s.PolicyFailures }),
	timeColumn("policy_not_before", false, func(s *Sandbox) time.Time { return s.PolicyNotBefore }),
	plainColumn("pin_node", false, func(s *Sandbox) string { return s.PinNode }),
	{name: "idle_after_s", equal: func(a, b *Sandbox) bool { return sameIntPtr(a.IdleAfterS, b.IdleAfterS) },
		value: func(s *Sandbox) any { return s.IdleAfterS }},
	plainColumn("address", false, func(s *Sandbox) string { return s.Address }),
}

// sandboxFixed lists the columns UpdateSandbox refuses to change.
var sandboxFixed = []sandboxColumn{
	plainColumn("owner", false, func(s *Sandbox) string { return s.Owner }),
	plainColumn("image", false, func(s *Sandbox) string { return s.Image }),
	plainColumn("vcpus", false, func(s *Sandbox) int { return s.VCPUs }),
	plainColumn("memory_mib", false, func(s *Sandbox) int { return s.MemoryMiB }),
	plainColumn("gen", false, func(s *Sandbox) int64 { return s.Gen }),
	timeColumn("created_at", false, func(s *Sandbox) time.Time { return s.CreatedAt }),
	timeColumn("updated_at", false, func(s *Sandbox) time.Time { return s.UpdatedAt }),
	{name: "environment_id", equal: func(a, b *Sandbox) bool { return a.EnvironmentID == b.EnvironmentID }},
	{name: "instance", equal: func(a, b *Sandbox) bool { return a.Instance == b.Instance }},
	{name: "pod_template", equal: func(a, b *Sandbox) bool { return sameTemplate(a.PodTemplate, b.PodTemplate) }},
	{name: "tenant_job", equal: func(a, b *Sandbox) bool { return a.TenantJob == b.TenantJob }},
	{name: "log_labels", equal: func(a, b *Sandbox) bool { return maps.Equal(a.LogLabels, b.LogLabels) }},
}

// UpdateSandbox writes the columns in which after differs from before, the
// row as the caller read it (normally locked FOR UPDATE in the same
// transaction), and returns the row as written. A change of a lifecycle
// column (client_state, node, op*, incarnation, materialised, checkpoint)
// increments gen and sends ferun_sandbox; such a change or one of the
// status description also sets updated_at = now(). Times the caller sets
// should come from Now, the transaction's clock. When the row's gen or
// incarnation are no longer before's, nothing is written and ErrStale is
// returned; when nothing differs, before is returned unwritten.
func (s *Store) UpdateSandbox(ctx context.Context, before, after Sandbox) (Sandbox, error) {
	if before.ID != after.ID {
		return Sandbox{}, fmt.Errorf("update sandbox %s: the id changed to %s", before.ID, after.ID)
	}
	for _, c := range sandboxFixed {
		if !c.equal(&before, &after) {
			return Sandbox{}, fmt.Errorf("update sandbox %s: column %s is not written by UpdateSandbox", before.ID, c.name)
		}
	}
	var sets []string
	args := []any{before.ID, before.Gen, before.Incarnation}
	lifecycle, described := false, false
	for _, c := range sandboxWritable {
		if c.equal(&before, &after) {
			continue
		}
		args = append(args, c.value(&after))
		sets = append(sets, fmt.Sprintf("%s = $%d", c.name, len(args)))
		lifecycle = lifecycle || c.lifecycle
		described = described || c.name == "status_description"
	}
	if len(sets) == 0 {
		return before, nil
	}
	if lifecycle {
		sets = append(sets, "gen = gen + 1")
	}
	if lifecycle || described {
		sets = append(sets, "updated_at = now()")
	}
	row, err := scanSandbox(s.q.QueryRow(ctx, "UPDATE sandboxes SET "+strings.Join(sets, ", ")+
		" WHERE id = $1 AND gen = $2 AND incarnation = $3 RETURNING "+sandboxCols, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return Sandbox{}, ErrStale
	}
	if err != nil {
		return Sandbox{}, fmt.Errorf("update sandbox %s: %w", before.ID, err)
	}
	if lifecycle {
		if err := s.Notify(ctx, chanSandbox, before.ID.String()); err != nil {
			return Sandbox{}, err
		}
	}
	return row, nil
}

// HintEnvGroups is the ferun_leader hint of a change of an environment's
// security group members: the address of a row changed, and every row is
// a member of its environment's groups (ADR 0035). It wakes the
// environments' loop env-groups.
const HintEnvGroups = "env-groups"

// SetSandboxAddress writes the IPv6 address of the sandbox's pod as an
// op's call answered it (CreatePod, RestorePod), when the row is still of
// incarnation and has another: a write of its own that bumps no gen. It
// sends ferun_leader HintEnvGroups. Inside a transaction, as the hint goes
// with the write. It reports whether it wrote.
func (s *Store) SetSandboxAddress(ctx context.Context, id podid.ID, incarnation uuid.UUID, address string) (bool, error) {
	tag, err := s.q.Exec(ctx, `UPDATE sandboxes s SET address = $3 WHERE s.id = $1 AND s.incarnation = $2 AND s.address <> $3`,
		id, incarnation, address)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	return true, s.Notify(ctx, db.ChanLeader, HintEnvGroups)
}

// TouchSandbox sets last_active_at = now(): activity, no lifecycle write.
func (s *Store) TouchSandbox(ctx context.Context, id podid.ID) error {
	_, err := s.q.Exec(ctx, "UPDATE sandboxes SET last_active_at = now() WHERE id = $1", id)
	return err
}

// MarkUnreachable records that the guest of a running sandbox did not
// answer: unreachable_since = coalesce(unreachable_since, now()). It
// reports whether the row is running.
func (s *Store) MarkUnreachable(ctx context.Context, id podid.ID) (bool, error) {
	tag, err := s.q.Exec(ctx, `UPDATE sandboxes SET unreachable_since = coalesce(unreachable_since, now())
		WHERE id = $1 AND client_state = 'running'`, id)
	return tag.RowsAffected() > 0, err
}

// ClearUnreachable clears unreachable_since: the guest answered.
func (s *Store) ClearUnreachable(ctx context.Context, id podid.ID) error {
	_, err := s.q.Exec(ctx, "UPDATE sandboxes SET unreachable_since = NULL WHERE id = $1 AND unreachable_since IS NOT NULL", id)
	return err
}

// MarkPending records the first observation of the sandbox's pod (of
// incarnation) as PENDING: pending_since = coalesce(pending_since, now()),
// a write of its own that bumps no gen. It reports whether the row of that
// incarnation exists.
func (s *Store) MarkPending(ctx context.Context, id podid.ID, incarnation uuid.UUID) (bool, error) {
	tag, err := s.q.Exec(ctx, `UPDATE sandboxes SET pending_since = coalesce(pending_since, now())
		WHERE id = $1 AND incarnation = $2`, id, incarnation)
	return tag.RowsAffected() > 0, err
}

// SetStatusDescription writes only the status description (no gen bump,
// no notification), when the row is still of incarnation.
func (s *Store) SetStatusDescription(ctx context.Context, id podid.ID, incarnation uuid.UUID, description string) error {
	_, err := s.q.Exec(ctx, `UPDATE sandboxes SET status_description = $3, updated_at = now()
		WHERE id = $1 AND incarnation = $2 AND status_description <> $3`, id, incarnation, description)
	return err
}

// ReleaseOpsOf releases the ops still owned by a stopping instance: the op
// columns cleared, client_state unknown with the description "op <op> of
// stopping instance <id> was released", node and booking kept, gen + 1.
// It returns the ids and notifies them.
func (s *Store) ReleaseOpsOf(ctx context.Context, instanceID string) ([]podid.ID, error) {
	rows, err := s.q.Query(ctx, `UPDATE sandboxes SET op = '', op_id = NULL, op_owner = '', op_deadline = NULL,
			op_target = '', op_from_state = '', client_state = 'unknown',
			status_description = 'op ' || op || ' of stopping instance ' || $1 || ' was released',
			gen = gen + 1, updated_at = now()
		WHERE op_owner = $1 AND op <> '' RETURNING id`, instanceID)
	return s.notifyReturned(ctx, rows, err)
}

func (s *Store) notifyReturned(ctx context.Context, rows pgx.Rows, err error) ([]podid.ID, error) {
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[podid.ID])
	if err != nil {
		return nil, err
	}
	return ids, s.notifySandboxes(ctx, ids)
}

// rfc3339 renders a timestamptz expression as RFC 3339 in UTC, in SQL.
func rfc3339(expr string) string {
	return "to_char(" + expr + " AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"')"
}

// nodeDownSince is the time a node n went out of service, as RFC 3339 in
// SQL: since it was down, or, for a node removed while it was not down,
// since its removal. It is the <t> of the down sweep's and lost grace's
// "node <n> down since <t>".
var nodeDownSince = rfc3339("coalesce(n.down_since, n.removed_at, now())")

// DownSweep moves up to limit sandboxes without an op whose node is down
// or removed, and which are running, paused, creating, stopped or failed,
// to unknown ("node <n> down since <t>; it was <state>"), gen + 1, booking
// unchanged. Rows locked by others are skipped. It returns the ids and
// notifies them.
func (s *Store) DownSweep(ctx context.Context, limit int) ([]podid.ID, error) {
	rows, err := s.q.Query(ctx, `WITH c AS (
			SELECT s.id FROM sandboxes s
			WHERE s.node IN (SELECT name FROM nodes WHERE status IN ('down', 'removed')) AND s.op = ''
				AND s.client_state IN ('running', 'paused', 'creating', 'stopped', 'failed')
			ORDER BY s.id LIMIT $1 FOR UPDATE SKIP LOCKED)
		UPDATE sandboxes s SET client_state = 'unknown',
			status_description = 'node ' || n.name || ' down since ' || `+nodeDownSince+`
				|| '; it was ' || s.client_state,
			gen = s.gen + 1, updated_at = now()
		FROM c, nodes n WHERE s.id = c.id AND n.name = s.node RETURNING s.id`, limit)
	return s.notifyReturned(ctx, rows, err)
}

// LostRow is a sandbox lost grace resolved.
type LostRow struct {
	ID       podid.ID
	Owner    string
	Node     string // the node that went down
	State    string // model.StateHibernated, model.StateLost or model.StatePending
	RunID    string // the run still open, for the caller to close
	Previous string // the description before
}

// LostGrace resolves up to limit unknown sandboxes without an op on a node
// that is down for longer than grace, or removed, gen + 1, "node <n> down
// since <t>…": with a checkpoint to hibernated ("; restored from the
// checkpoint of <checkpoint_at> on the next use"), else, when a pod of
// theirs was seen there (materialised), to lost; both keep the node and
// set lost_from = node. One whose pod was never seen (a first create whose
// outcome its node took down with it) has nothing to lose and becomes
// pending without a node ("; its pod was never seen there; created on the
// next use"), as a sync makes of a pod that is not found; a pod the node
// still has when it returns belongs to no row there, and reconcile
// tombstones it. pending_since and the address are cleared (ferun_leader
// HintEnvGroups when one had one). Their bookings and process
// holds are deleted (these states end the pod's use of the node and its
// processes); no tombstone is written. Rows locked by others are skipped.
// It returns the rows and notifies them.
func (s *Store) LostGrace(ctx context.Context, grace time.Duration, limit int) ([]LostRow, error) {
	rows, err := s.q.Query(ctx, `WITH c AS (
			SELECT s.id FROM sandboxes s JOIN nodes n ON n.name = s.node
			WHERE s.client_state = 'unknown' AND s.op = ''
				AND (n.status = 'removed' OR (n.status = 'down' AND n.down_since < now() - $1::interval))
			ORDER BY s.id LIMIT $2 FOR UPDATE OF s SKIP LOCKED),
		old AS (SELECT s.id, s.node, s.status_description,
				s.checkpoint_head <> '' AS checkpoint, s.checkpoint_head <> '' OR s.materialised AS kept,
				s.address <> '' AS member
			FROM sandboxes s JOIN c ON c.id = s.id)
		UPDATE sandboxes s SET
			client_state = CASE WHEN old.checkpoint THEN 'hibernated' WHEN old.kept THEN 'lost' ELSE 'pending' END,
			node = CASE WHEN old.kept THEN s.node END,
			lost_from = CASE WHEN old.kept THEN s.node END,
			pending_since = NULL, address = '',
			status_description = 'node ' || n.name || ' down since ' || `+nodeDownSince+`
				|| CASE WHEN old.checkpoint THEN '; restored from the checkpoint of '
					|| `+rfc3339("coalesce(s.checkpoint_at, now())")+` || ' on the next use'
				WHEN old.kept THEN ''
				ELSE '; its pod was never seen there; created on the next use' END,
			gen = s.gen + 1, updated_at = now()
		FROM c, nodes n, old WHERE s.id = c.id AND old.id = s.id AND n.name = s.node
		RETURNING s.id, s.owner, old.node, s.client_state, s.run_id, old.status_description, old.member`, interval(grace), limit)
	if err != nil {
		return nil, err
	}
	members := false
	lost, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (LostRow, error) {
		var l LostRow
		var member bool
		err := r.Scan(&l.ID, &l.Owner, &l.Node, &l.State, &l.RunID, &l.Previous, &member)
		members = members || member
		return l, err
	})
	if err != nil || len(lost) == 0 {
		return lost, err
	}
	if members {
		if err := s.Notify(ctx, db.ChanLeader, HintEnvGroups); err != nil {
			return nil, err
		}
	}
	ids := make([]podid.ID, len(lost))
	for i, l := range lost {
		ids[i] = l.ID
	}
	if _, err := s.q.Exec(ctx, "DELETE FROM bookings WHERE pod_id = ANY($1)", ids); err != nil {
		return nil, err
	}
	if _, err := s.q.Exec(ctx, "DELETE FROM sandbox_holds WHERE sandbox_id = ANY($1) AND kind = 'process'", ids); err != nil {
		return nil, err
	}
	return lost, s.notifySandboxes(ctx, ids)
}

// StaleOps returns up to limit sandboxes with an op in flight whose owner
// is not live (dead or stopping), or whose op deadline passed more than
// overdue ago, oldest deadline first.
func (s *Store) StaleOps(ctx context.Context, overdue time.Duration, limit int) ([]Sandbox, error) {
	return collectSandboxes(s.q.Query(ctx, "SELECT "+sandboxColumns("s")+` FROM sandboxes s
		WHERE s.op <> '' AND (NOT EXISTS (SELECT 1 FROM instances i WHERE i.id = s.op_owner AND `+instanceLive("i")+`)
			OR s.op_deadline < now() - $1::interval)
		ORDER BY s.op_deadline LIMIT $2`, interval(overdue), limit))
}

// policyDue is the condition on a sandbox row s under which policy
// (PolicyIdle or PolicyHibernateAfter) may begin an op for it: no failed
// op of that policy backs it off. Another policy's backoff does not count.
func policyDue(policy string) string {
	return "(s.policy_failed <> '" + policy + "' OR s.policy_not_before IS NULL OR s.policy_not_before <= now())"
}

// idleAfter is the idle pause's interval of a sandbox row s as SQL, with
// $n the instance's --idle-pause (an interval): its idle_after_s, else
// $n. idleOn is the condition under which s idles at all: not 0 (never).
func idleAfter(n int) string {
	return fmt.Sprintf("coalesce(make_interval(secs => s.idle_after_s), $%d::interval)", n)
}

func idleOn(n int) string {
	return fmt.Sprintf("coalesce(s.idle_after_s > 0, $%d::interval > interval '0')", n)
}

// IdleSandboxes returns up to limit running sandboxes without an op on one
// of nodes whose last_active_at is older than their idle interval (their
// idle_after_s, or idle for a row without one; 0 never idles), that have
// no live hold of any kind, that no failed idle pause backs off, that
// the ingress's fence does not keep awake (ingressFence: a service
// instance of a public environment while an ingress may be cut off) and
// that no attached service keeps running (attachedAwake), the least
// recently active first.
func (s *Store) IdleSandboxes(ctx context.Context, idle time.Duration, nodes []string, limit int) ([]Sandbox, error) {
	return collectSandboxes(s.q.Query(ctx, "SELECT "+sandboxColumns("s")+` FROM sandboxes s
		WHERE s.client_state = 'running' AND s.op = '' AND `+idleOn(1)+` AND s.last_active_at < now() - `+idleAfter(1)+`
			AND s.node = ANY($2) AND `+policyDue(PolicyIdle)+`
			AND NOT EXISTS (SELECT 1 FROM sandbox_holds h WHERE h.sandbox_id = s.id AND h.lease_until > now())
			AND `+ingressFence+` AND NOT `+attachedAwake+`
		ORDER BY s.last_active_at LIMIT $3`, interval(idle), nonNil(nodes), limit))
}

// attachedAwake is the condition under which a sandbox row s stays running
// for an attached service whose process runs in its incarnation (ADR 0018,
// "An attached service idles as an image service does"): one that starts,
// until its awake_until; one whose idle is 0 (never); one with an idle,
// while s was active within it. A sandbox's idle interval is thus the
// longest of its own and its attached services' claims.
const attachedAwake = `EXISTS (SELECT 1 FROM env_attached a WHERE a.sandbox_id = s.id AND a.process_id <> ''
		AND a.sandbox_incarnation = s.incarnation
		AND (a.awake_until > now() OR a.idle_after_s = 0
			OR s.last_active_at > now() - make_interval(secs => a.idle_after_s)))`

// HibernateCandidates returns up to limit paused sandboxes without an op,
// paused for longer than after, on one of nodes that has an object store,
// that no failed hibernate-after backs off.
func (s *Store) HibernateCandidates(ctx context.Context, after time.Duration, nodes []string, limit int) ([]Sandbox, error) {
	return collectSandboxes(s.q.Query(ctx, "SELECT "+sandboxColumns("s")+` FROM sandboxes s JOIN nodes n ON n.name = s.node
		WHERE s.client_state = 'paused' AND s.op = '' AND s.paused_at < now() - $1::interval
			AND s.node = ANY($2) AND n.object_store_id <> '' AND `+policyDue(PolicyHibernateAfter)+`
		ORDER BY s.paused_at LIMIT $3`, interval(after), nonNil(nodes), limit))
}

// UnknownSandboxes returns up to limit unknown sandboxes without an op on
// one of nodes that is ready.
func (s *Store) UnknownSandboxes(ctx context.Context, nodes []string, limit int) ([]Sandbox, error) {
	return collectSandboxes(s.q.Query(ctx, "SELECT "+sandboxColumns("s")+` FROM sandboxes s JOIN nodes n ON n.name = s.node
		WHERE s.client_state = 'unknown' AND s.op = '' AND s.node = ANY($1) AND n.status = 'ready'
		ORDER BY s.updated_at LIMIT $2`, nonNil(nodes), limit))
}

// RepairCandidates returns up to limit sandboxes without an op on one of
// nodes that need a repair: creating with pending_since older than
// pendingAfter (a pod stuck PENDING), or running with unreachable_since
// older than unreachableAfter.
func (s *Store) RepairCandidates(ctx context.Context, pendingAfter, unreachableAfter time.Duration, nodes []string, limit int) ([]Sandbox, error) {
	return collectSandboxes(s.q.Query(ctx, "SELECT "+sandboxColumns("s")+` FROM sandboxes s
		WHERE s.op = '' AND s.node = ANY($3) AND (
			(s.client_state = 'creating' AND s.pending_since < now() - $1::interval)
			OR (s.client_state = 'running' AND s.unreachable_since < now() - $2::interval))
		ORDER BY s.id LIMIT $4`, interval(pendingAfter), interval(unreachableAfter), nonNil(nodes), limit))
}

// PendingWithoutCapacity returns up to limit pending sandboxes without an
// op whose description begins "no capacity".
func (s *Store) PendingWithoutCapacity(ctx context.Context, limit int) ([]Sandbox, error) {
	return collectSandboxes(s.q.Query(ctx, "SELECT "+sandboxCols+` FROM sandboxes
		WHERE client_state = 'pending' AND op = '' AND status_description LIKE 'no capacity%'
		ORDER BY created_at LIMIT $1`, limit))
}

// SandboxesInState returns up to limit sandboxes without an op on node in
// one of states, ordered by id.
func (s *Store) SandboxesInState(ctx context.Context, node string, states []string, limit int) ([]Sandbox, error) {
	return collectSandboxes(s.q.Query(ctx, "SELECT "+sandboxCols+` FROM sandboxes
		WHERE node = $1 AND op = '' AND client_state = ANY($2) ORDER BY id LIMIT $3`, node, nonNil(states), limit))
}

// SetSandboxTLSBindings writes the outbound TLS bindings (ADR 0036) into
// the template of sandbox id of incarnation inc, a sandbox (no instance,
// no tenant's job): those of the attached services in it, which its
// environment's controller sets before it starts one, so that every
// later create of its pod carries them. None removes them. A row without
// a template gets one that keeps its pod as it is (sleep infinity, the
// template-less command). Not a lifecycle write: its gen stays. It
// reports whether the row was there.
func (s *Store) SetSandboxTLSBindings(ctx context.Context, id podid.ID, inc uuid.UUID,
	bindings []model.TemplateTLSBinding) (bool, error) {
	var bj any
	if len(bindings) > 0 {
		b, err := json.Marshal(bindings)
		if err != nil {
			return false, fmt.Errorf("TLS bindings: %w", err)
		}
		bj = string(b)
	}
	tag, err := s.q.Exec(ctx, `UPDATE sandboxes SET pod_template = CASE
			WHEN $3::jsonb IS NULL THEN pod_template - 'tlsBindings'
			ELSE jsonb_set(coalesce(pod_template, '{"command": ["sleep", "infinity"]}'::jsonb), '{tlsBindings}', $3::jsonb)
		END, updated_at = now()
		WHERE id = $1 AND incarnation = $2 AND NOT instance AND NOT tenant_job`, id, inc, bj)
	if err != nil {
		return false, fmt.Errorf("TLS bindings of sandbox %s: %w", id, err)
	}
	return tag.RowsAffected() > 0, nil
}
