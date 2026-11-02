// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/store"
)

// evidence is what an incident keeps of what the controller saw (§4.9):
// the container's last exits, the end of its log, a failed rollout's diff
// against the deployed release, why an instance could not be placed, what
// a release past its deadline waited for.
type evidence struct {
	Exits     []store.EnvExit  `json:"exits,omitempty"`
	LogTail   []string         `json:"logTail,omitempty"`
	Diff      []spec.DiffEntry `json:"diff,omitempty"`
	Placement string           `json:"placement,omitempty"`
	// Pending is what a release past its deadline still waited for, and
	// the details of those objects' instances.
	Pending []string `json:"pending,omitempty"`
}

func (e evidence) json() json.RawMessage {
	b, err := json.Marshal(e)
	if err != nil {
		return json.RawMessage("{}")
	}
	return b
}

// openIncident opens an incident in tx unless one is open for its
// object, release and condition; it reports whether it opened one.
func openIncident(ctx context.Context, tx *store.Store, envID uuid.UUID, object string, release int, condition, summary string,
	ev evidence) (bool, error) {
	_, opened, err := tx.OpenIncident(ctx, store.EnvIncident{EnvID: envID, Object: object, Release: release,
		Condition: condition, Summary: summary, Evidence: ev.json()})
	return opened, err
}

// incident opens an incident in a fenced transaction of its own.
func (c *Controller) incident(ctx context.Context, envID uuid.UUID, object string, release int, condition, summary string,
	ev evidence) {
	err := c.fenced(ctx, func(tx *store.Store) error {
		opened, err := openIncident(ctx, tx, envID, object, release, condition, summary, ev)
		if opened {
			c.log.Info("incident opened", "environment", envID, "object", object, "release", release,
				"condition", condition, "summary", summary)
		}
		return err
	})
	if err != nil && ctx.Err() == nil {
		c.rl.warn("incident", "cannot open an incident", "environment", envID, "object", object, "condition", condition,
			"err", err)
	}
}

// closeIncidents closes an object's open incidents of condition, when
// the view saw one.
func (c *Controller) closeIncidents(ctx context.Context, v *view, object, condition string) {
	if !v.isOpen(object, condition) {
		return
	}
	err := c.fenced(ctx, func(tx *store.Store) error {
		_, err := tx.EnvCtlCloseObjectIncidents(ctx, v.env.ID, object, condition)
		return err
	})
	if err != nil && ctx.Err() == nil {
		c.rl.warn("incident", "cannot close incidents", "environment", v.env.ID, "object", object, "condition", condition,
			"err", err)
	}
}
