// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package model holds the value types and sentinel errors that several of
// ferund's packages share. It depends on nothing of ferund but podid, the
// IDs of pods, so that every package can name them without importing
// another's implementation.
package model

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/podid"
)

// HoldKind is what keeps a sandbox awake (the sandbox_holds.kind column).
type HoldKind string

const (
	// HoldExec is held while a tool call or an exec runs in the sandbox.
	HoldExec HoldKind = "exec"
	// HoldForward is held while a forwarded request or an upgraded
	// connection to one of the sandbox's ports is open.
	HoldForward HoldKind = "forward"
	// HoldProcess is held for a background process started by
	// process_start, until it ends, is stopped or its timeout passes.
	HoldProcess HoldKind = "process"
)

// Lease names a running sandbox that a caller holds: the node its pod runs
// on and the incarnation of that pod.
type Lease struct {
	SandboxID   podid.ID
	Incarnation uuid.UUID
	Node        string
}

// Loop is a job the leader runs on its interval and when woken. Run is
// called with the leader's context; one run at a time per loop.
type Loop struct {
	Name     string
	Interval time.Duration
	Run      func(ctx context.Context) error
}

// The client states of a sandbox (sandboxes.client_state), which FeCode
// reads as clientState.
const (
	StatePending     = "pending"
	StateCreating    = "creating"
	StateRunning     = "running"
	StatePausing     = "pausing"
	StatePaused      = "paused"
	StateResuming    = "resuming"
	StateHibernating = "hibernating"
	StateHibernated  = "hibernated"
	StateStopped     = "stopped"
	StateFailed      = "failed"
	StateUnknown     = "unknown"
	StateLost        = "lost"
)

// The statuses of a node (nodes.status).
const (
	NodeJoined  = "joined"
	NodeReady   = "ready"
	NodeDown    = "down"
	NodeRemoved = "removed"
)

var (
	ErrNotFound        = errors.New("no such sandbox")
	ErrLost            = errors.New("sandbox lost")
	ErrNoCapacity      = errors.New("no capacity")      // wrapped with the gates; retryable
	ErrNodeUnavailable = errors.New("node unavailable") // wrapped with the node and cause; retryable
	ErrBusy            = errors.New("an op is in flight")
	// ErrPrecondition is a failed recheck when an op begins: the row
	// changed between the caller's read and the begin.
	ErrPrecondition = errors.New("the sandbox changed; op not begun")
	ErrStopping     = errors.New("instance is stopping") // retryable
	ErrNotRunning   = errors.New("sandbox is not running")
	ErrUnknownToken = errors.New("unknown token")
	// ErrNodeRefused is a definitive refusal of a node's certificate,
	// wrapped with the reason.
	ErrNodeRefused = errors.New("node refused")
	// ErrCertSuperseded is a re-check of a superseded certificate past its
	// grace: the connection closes as at the drain timeout.
	ErrCertSuperseded = errors.New("superseded certificate")
	// ErrImageRefused is an image whose registry refused it to the create,
	// or does not have it for it (ADR 0025), wrapped with the answer.
	ErrImageRefused = errors.New("image refused")
	// ErrRegistryUnavailable is an image whose registry did not answer the
	// question before a create (ADR 0025): no answer is no; retryable.
	ErrRegistryUnavailable = errors.New("registry unavailable")
)
