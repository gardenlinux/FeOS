// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The labels of a pod's container logs (ADR 0026), set at the insert of
// its row (sandboxes.log_labels) and sent with its create.

// instanceLogLabels are those of an environment's instance; attempt > 0
// makes them a job's run's.
func instanceLogLabels(owner string, id podid.ID, envID uuid.UUID, kind, object string, replica, deployment, attempt int) map[string]string {
	l := map[string]string{
		model.LogTenant: owner, model.LogInstance: id.String(), model.LogEnvironment: envID.String(),
		model.LogKind: kind, model.LogObject: object, model.LogReplica: strconv.Itoa(replica),
		model.LogDeployment: strconv.Itoa(deployment),
	}
	if attempt > 0 {
		l[model.LogAttempt] = strconv.Itoa(attempt)
	}
	return l
}

// tenantJobLogLabels are those of a tenant's job's pod in envID, its one
// attempt; days > 0 is the retention the job asked.
func tenantJobLogLabels(owner string, id podid.ID, envID uuid.UUID, days int) map[string]string {
	l := map[string]string{
		model.LogTenant: owner, model.LogInstance: id.String(), model.LogEnvironment: envID.String(),
		model.LogJob: id.String(), model.LogAttempt: "1",
	}
	if days > 0 {
		l[model.LogRetentionDays] = strconv.Itoa(days)
	}
	return l
}

// SandboxLogLabels are those of a sandbox of envID (ADR 0035): its
// environment's, of the kind sandbox, the object its ID.
func SandboxLogLabels(owner string, id podid.ID, envID uuid.UUID) map[string]string {
	return map[string]string{
		model.LogTenant: owner, model.LogInstance: id.String(), model.LogEnvironment: envID.String(),
		model.LogKind: model.LogKindSandbox, model.LogObject: id.String(),
	}
}

// LogRedact is the engine's log redactor (sandbox.LogRedactor): the
// values a pod's shipped log lines are redacted of on its node
// (CreatePodRequest.log_redact), computed at every create and stored
// nowhere: those a stored tail of the row is redacted of, each with its
// userinfo escaping (redactValues). An environment's row: envSecretValues
// (its addons' credentials, its tenant secrets, those its template
// names); a tenant's job's: its secrets and image credentials. An error
// fails the create, so that no pod ships what it could not be told to
// redact.
func (c *Controller) LogRedact(ctx context.Context, row store.Sandbox) ([]string, error) {
	var vals []string
	switch {
	case row.TenantJob:
		sealed, err := c.st.TenantJobSealed(ctx, row.ID)
		if err != nil {
			return nil, fmt.Errorf("job %s: %w", row.ID, err)
		}
		if sealed != nil {
			secrets, err := c.tenantJobSecrets(ctx, row.ID)
			if err != nil {
				return nil, err
			}
			vals = slices.Collect(maps.Values(secrets))
		}
	case row.Instance:
		v, err := c.load(ctx, row.EnvironmentID)
		if err != nil {
			return nil, fmt.Errorf("environment %s: %w", row.EnvironmentID, err)
		}
		if v == nil {
			return nil, fmt.Errorf("environment %s is being deleted or has no deployment", row.EnvironmentID)
		}
		vals = c.envSecretValues(ctx, v, row.PodTemplate)
	}
	return redactValues(vals), nil
}
