// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package model

// The labels of a pod's container logs (ADR 0026,
// CreatePodRequest.log_labels, docs/architecture/sink.md): ferund sets
// them at the insert of an environment's instance or job run and of a
// tenant's job's pod (sandboxes.log_labels); a sandbox a tenant made
// itself has none, and its logs are not shipped.
const (
	LogTenant        = "ferun.tenant"
	LogInstance      = "ferun.instance"
	LogEnvironment   = "ferun.environment"
	LogKind          = "ferun.kind"
	LogObject        = "ferun.object"
	LogReplica       = "ferun.replica"
	LogDeployment    = "ferun.deployment"
	LogJob           = "ferun.job"
	LogAttempt       = "ferun.attempt"
	LogRetentionDays = "ferun.retention_days"
)

// The bounds of a tenant's job's logRetentionDays.
const (
	MinLogRetentionDays = 1
	MaxLogRetentionDays = 400
)

// The kinds of service tokens (service_tokens.kind): ServiceSink, an
// instance of ferun-sink.
const ServiceSink = "sink"
