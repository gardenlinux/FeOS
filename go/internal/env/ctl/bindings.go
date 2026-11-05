// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The incidents of an environment's HTTP bindings (ADR 0028 §6): every
// bindingsEvery the leader reads, per binding, the audit records of the
// environment's pods' requests through it over the last bindingWindow
// (activity, source authn, kind http_request or http: feos-authn's "http"
// records, which feosd publishes as the pods' events). Its requests are
// those the upstream answered and those feos-authn refused because the
// upstream could not be reached (unreachable, tls) or no token could be
// had (credential_unavailable); its refusals for the pod's own mistakes
// (denied_path, unknown_binding, rate_limited, misdirected, …) count for
// none. With at least bindingMinRequests:
//
//   - token_failing while more than half had no token;
//   - credential_refused while more than half were answered 401 or 403 by
//     the upstream;
//   - upstream_failing while more than half were answered 429 or 5xx by
//     it, or could not reach it.
//
// An incident opens on the binding's object, binding/<name>, of the
// environment's latest deployment, with the counts and the newest records
// as evidence, and closes once its condition no longer holds (no requests
// among it), or the binding is gone.
const (
	LoopBindings       = "env-bindings"
	bindingsEvery      = 30 * time.Second
	bindingWindow      = 5 * time.Minute
	bindingMinRequests = 5
	bindingEvidence    = 10
)

// egressEvidence is a binding incident's evidence: the window's counts
// and its newest records of the condition's kind.
type egressEvidence struct {
	WindowSeconds int               `json:"windowSeconds"`
	Requests      int64             `json:"requests"`
	Refused       int64             `json:"refused"`
	Failing       int64             `json:"failing"`
	TokenFailed   int64             `json:"tokenFailed"`
	Records       []json.RawMessage `json:"records"`
}

// bindingCondition is the condition a binding's window holds, "" for
// none, and how many requests it counts.
func bindingCondition(st store.EgressStat) (string, int64) {
	if st.Requests < bindingMinRequests {
		return "", 0
	}
	switch {
	case st.TokenFailed*2 > st.Requests:
		return model.ConditionTokenFailing, st.TokenFailed
	case st.Refused*2 > st.Requests:
		return model.ConditionCredentialRefused, st.Refused
	case st.Failing*2 > st.Requests:
		return model.ConditionUpstreamFailing, st.Failing
	}
	return "", 0
}

// bindingConditions are the conditions of a binding's incidents.
var bindingConditions = []string{model.ConditionCredentialRefused, model.ConditionUpstreamFailing,
	model.ConditionTokenFailing}

// bindingSummary says what a binding's condition is.
func bindingSummary(name, cond string, n, of int64) string {
	switch cond {
	case model.ConditionCredentialRefused:
		return fmt.Sprintf("binding %s: the upstream refused the credential, %d of %d requests in the last %s answered "+
			"401 or 403", name, n, of, bindingWindow)
	case model.ConditionTokenFailing:
		return fmt.Sprintf("binding %s: its token endpoint fails, %d of %d requests in the last %s had no token", name,
			n, of, bindingWindow)
	}
	return fmt.Sprintf("binding %s: the upstream fails, %d of %d requests in the last %s were answered 429 or 5xx or did "+
		"not reach it", name, n, of, bindingWindow)
}

// bindingsLoop opens and closes the bindings' incidents of every
// environment with bindings.
func (c *Controller) bindingsLoop(ctx context.Context) error {
	envs, err := c.st.BindingEnvs(ctx)
	if err != nil {
		return fmt.Errorf("bindings: %w", err)
	}
	for _, e := range envs {
		if err := c.stepBindings(ctx, e); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			c.rl.warn("bindings", "cannot watch an environment's bindings", "environment", e.ID, "err", err)
		}
	}
	return nil
}

// stepBindings is one environment's pass.
func (c *Controller) stepBindings(ctx context.Context, e store.BindingEnv) error {
	stats, err := c.st.EgressStats(ctx, e.ID, bindingWindow)
	if err != nil {
		return err
	}
	byName := map[string]store.EgressStat{}
	for _, st := range stats {
		byName[st.Binding] = st
	}
	open, err := c.st.ListIncidents(ctx, e.ID, true)
	if err != nil {
		return err
	}
	isOpen := map[string]bool{}
	for _, i := range open {
		if strings.HasPrefix(i.Object, spec.KindBinding+"/") {
			isOpen[i.Object+" "+i.Condition] = true
		}
	}
	for _, name := range e.Bindings {
		obj := objectName(spec.KindBinding, name)
		st := byName[name]
		cond, n := bindingCondition(st)
		for _, other := range bindingConditions {
			if other != cond && isOpen[obj+" "+other] {
				if err := c.closeBindingIncident(ctx, e, obj, other); err != nil {
					return err
				}
			}
		}
		if cond == "" || isOpen[obj+" "+cond] {
			continue
		}
		records, err := c.st.EgressRecords(ctx, e.ID, name, bindingWindow, bindingEvidence)
		if err != nil {
			return err
		}
		ev := evidence{Egress: &egressEvidence{WindowSeconds: int(bindingWindow / time.Second), Requests: st.Requests,
			Refused: st.Refused, Failing: st.Failing, TokenFailed: st.TokenFailed, Records: records}}
		c.incident(ctx, e.ID, obj, e.Deployment, cond, bindingSummary(name, cond, n, st.Requests), ev)
	}
	// A binding that is gone has no incident.
	for _, i := range open {
		name, ok := strings.CutPrefix(i.Object, spec.KindBinding+"/")
		if ok && !slices.Contains(e.Bindings, name) && slices.Contains(bindingConditions, i.Condition) {
			if err := c.closeBindingIncident(ctx, e, i.Object, i.Condition); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Controller) closeBindingIncident(ctx context.Context, e store.BindingEnv, obj, cond string) error {
	return c.fenced(ctx, func(tx *store.Store) error {
		n, err := tx.EnvCtlCloseObjectIncidents(ctx, e.ID, obj, cond)
		if n > 0 {
			c.log.Info("incident closed", "environment", e.ID, "object", obj, "condition", cond)
		}
		return err
	})
}
