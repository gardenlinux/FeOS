// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package envdeps holds the interfaces through which the environment
// service (internal/env) and its controller (internal/env/ctl) reach the
// rest of the instance: the sandbox engine, the tools' runner, the port
// door and the sealing of secrets. Both packages use these same types;
// they live apart from either because env composes ctl (ctl cannot import
// env), and env re-exports them under its own name (env.Engine, …).
package envdeps

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/tools"
)

// Engine is what the environments need of the sandbox engine
// (*sandbox.Engine). An environment's instance is a sandbox row with
// environment_id set; the engine places, starts, restores, moves and
// deletes it as any sandbox, and builds its pod from its pod_template.
type Engine interface {
	// Ensure brings the row's pod up (create, start, resume, restore) and
	// returns its lease; it may take the preparation budget (7 m 5 s).
	Ensure(ctx context.Context, id podid.ID) (model.Lease, error)
	// Delete deletes the row of owner and its pod (through tombstones): the
	// row as it was, and whether there was one.
	Delete(ctx context.Context, owner string, id podid.ID) (store.Sandbox, bool, error)
	// Restart restarts a running row's pod (a repair op with the trigger
	// restart: StopPod and StartPod), for an instance whose container
	// exited; reason becomes the op's description.
	Restart(ctx context.Context, id podid.ID, reason string) error
	// ResolveImage pins an image to its digest
	// ("<registry>/<repo>@sha256:…"), asked with auth (ADR 0025).
	ResolveImage(ctx context.Context, image string, auth model.RegistryCredentials) (string, error)
	// Acquire ensures the row runs and holds it awake with a hold of kind
	// until release is called (it wakes a paused or hibernated row).
	Acquire(ctx context.Context, id podid.ID, kind model.HoldKind) (lease model.Lease, release func(), err error)
	// Touch counts as activity of the row (it postpones its idle pause).
	Touch(id podid.ID)
	// Sync reads the row's pod from its node and records what it finds.
	Sync(ctx context.Context, id podid.ID) error
	// State reads the row's client state and node; it never wakes it.
	State(ctx context.Context, id podid.ID) (state, node string, err error)
}

// Runner is what the environments run in a pod's main container
// (*tools.Runner's detached calls, internal/tools/detached.go). Every call
// takes the lease the caller read from the row: it does not acquire the
// sandbox, takes no hold, writes no activity row and does not count as
// activity, so that probes and the supervisor never keep a pod awake.
type Runner interface {
	// ExecDetached runs argv in the main container with env added, in cwd
	// ("": /), for at most timeout: its exit code and its output (each
	// capped at the tools' output cap).
	ExecDetached(ctx context.Context, lease model.Lease, argv []string, env map[string]string, cwd string,
		timeout time.Duration) (code int, stdout, stderr string, err error)
	// StartProcessDetached starts a background process without a time
	// limit and returns its id; an error wrapping tools.ErrNoCwd when its
	// working directory does not exist. The records of the processes of
	// its name that are over go but the newest few (tools.keptExited).
	StartProcessDetached(ctx context.Context, lease model.Lease, p tools.ProcessStart) (id string, err error)
	// ProcessesDetached lists the background processes of the container
	// named name (every one for ""), running or not.
	ProcessesDetached(ctx context.Context, lease model.Lease, name string) ([]tools.ProcessStatus, error)
	// ProcessLogsDetached returns the last lines lines of a process's
	// output; an error wrapping tools.ErrNoProcess for an unknown id.
	ProcessLogsDetached(ctx context.Context, lease model.Lease, id string, lines int) ([]string, error)
	// StopProcessDetached stops a process (TERM, then KILL after 2 s);
	// nil when it was not running any more.
	StopProcessDetached(ctx context.Context, lease model.Lease, id string) error
}

// Ports is what the environments need of the port door (*ports.Handler).
type Ports interface {
	// Probe GETs path on the pod's port over a port-forward, with Host
	// localhost:<port>, and returns the status code; it takes no hold,
	// touches nothing, has its own 2 s timeout and fails instead of
	// waiting for the per-sandbox stream cap.
	Probe(ctx context.Context, lease model.Lease, port int, path string) (int, error)
	// DialPod opens a port-forward to the pod's port (answered Ready),
	// under the same terms as Probe: a TCP readiness check.
	DialPod(ctx context.Context, lease model.Lease, port int) (net.Conn, error)
	// ForwardTo forwards the request to the sandbox's port as the door
	// does: Acquire with a forward hold (it wakes a paused row) and Touch,
	// Host passed through, the headers in credentials (nil:
	// Authorization) and Proxy-Authorization stripped, every other header
	// kept, upgrades and streaming kept, a Kubernetes binding's port
	// refused. upstreamPath and rawPath are the path the pod sees (rawPath
	// its escaped form, "" when it needs none); the query is kept. It
	// writes the answer, errors too (plain text).
	ForwardTo(w http.ResponseWriter, r *http.Request, sandboxID podid.ID, port int, upstreamPath, rawPath string,
		credentials []string)
}

// Sealer seals the secrets of environments at rest (AES-256-GCM under the
// key of --secret-key-file, internal/seal). aad binds a blob to its row:
// SecretAAD.
type Sealer interface {
	Seal(plaintext, aad []byte) ([]byte, error)
	Open(sealed, aad []byte) ([]byte, error)
}

// SecretAAD is the additional data of a secret's sealed blob
// (env_secrets.sealed): "env_secrets|<env id>|<name>|<version>". A blob
// copied to another environment, name or version does not open.
func SecretAAD(envID uuid.UUID, name string, version int) []byte {
	return []byte("env_secrets|" + envID.String() + "|" + name + "|" + strconv.Itoa(version))
}
