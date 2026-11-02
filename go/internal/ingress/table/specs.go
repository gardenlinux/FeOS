// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package table

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/store"
)

// ErrSpec is wrapped by the errors of a deployment that has no usable spec:
// it does not exist, or its stored spec does not parse. Deployments never
// change, so reading it again does not help.
var ErrSpec = errors.New("no usable deployment spec")

// ErrTooLarge is wrapped by the error of an environment whose entry in
// the table would take more than MaxEnvBytes coded. Like an environment
// without a usable spec, it is left out of the table, so that no
// tenant's environments can make the table's messages too large for the
// ingress (wire.PartBytes, wire.MaxMessageBytes).
var ErrTooLarge = errors.New("the environment is too large for the ingress's table")

// MaxEnvBytes is the most one environment's entry may take coded. A
// valid spec stays far below it: 128 routes with paths of at most
// spec.MaxRoutePathBytes, and the endpoints of 64 services of 20
// replicas, take about 1 MiB at the very most.
const MaxEnvBytes = 2 << 20

// unroutable reports whether err, of BuildEnv, means that the
// environment cannot be routed (ErrSpec, ErrTooLarge): it is left out of
// the table, and reading it again does not help until it changes.
func unroutable(err error) bool {
	return errors.Is(err, ErrSpec) || errors.Is(err, ErrTooLarge)
}

// SpecFunc returns the parsed spec of deployment number of the environment
// being built, for reading only; an error wrapping ErrSpec when that
// deployment has none, any other error when it could not be read.
type SpecFunc func(number int) (*spec.Spec, error)

// maxSpecs bounds the specs a Specs keeps parsed.
const maxSpecs = 4096

// specKey names a deployment: an environment ID is free again once its
// environment is deleted, and the next environment of that ID has
// deployments of its own, so the key has the environment's creation too.
type specKey struct {
	env     uuid.UUID
	created int64 // the environment's created_at in µs
	number  int
}

// Specs caches parsed deployment specs, which never change, for reading
// only. It is safe for concurrent use. Past maxSpecs specs it forgets an
// arbitrary one for each new one.
type Specs struct {
	st *store.Store

	mu sync.Mutex
	m  map[specKey]*spec.Spec
}

// NewSpecs returns an empty cache that reads deployments with st.
func NewSpecs(st *store.Store) *Specs {
	return &Specs{st: st, m: map[specKey]*spec.Spec{}}
}

// Get returns the parsed spec of deployment number of the environment env,
// created at created (cached). A deployment that does not exist, or whose
// spec does not parse, is an error wrapping ErrSpec.
func (c *Specs) Get(ctx context.Context, env uuid.UUID, created time.Time, number int) (*spec.Spec, error) {
	if number <= 0 {
		return nil, fmt.Errorf("deployment %d of environment %s: %w", number, env, ErrSpec)
	}
	k := specKey{env: env, created: created.UnixMicro(), number: number}
	c.mu.Lock()
	sp, ok := c.m[k]
	c.mu.Unlock()
	if ok {
		return sp, nil
	}
	r, err := c.st.GetDeployment(ctx, env, number)
	if errors.Is(err, store.ErrNoRow) {
		return nil, fmt.Errorf("deployment %d of environment %s does not exist: %w", number, env, ErrSpec)
	}
	if err != nil {
		return nil, fmt.Errorf("read deployment %d of environment %s: %w", number, env, err)
	}
	var parsed spec.Spec
	if err := json.Unmarshal(r.Spec, &parsed); err != nil {
		return nil, fmt.Errorf("deployment %d of environment %s: %w: %w", number, env, ErrSpec, err)
	}
	c.mu.Lock()
	for old := range c.m {
		if len(c.m) < maxSpecs {
			break
		}
		delete(c.m, old)
	}
	c.m[k] = &parsed
	c.mu.Unlock()
	return &parsed, nil
}

// For returns the SpecFunc of environment e's deployments, which reads with
// ctx.
func (c *Specs) For(ctx context.Context, e EnvRows) SpecFunc {
	return func(number int) (*spec.Spec, error) { return c.Get(ctx, e.ID, e.CreatedAt, number) }
}

// size reports how many specs c holds.
func (c *Specs) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}
