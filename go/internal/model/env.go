// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package model

import "github.com/gardenlinux/feos/go/ferun/api"

// The values of the environments' tables (migration 0006, ADR 0018),
// which the environment service, its controller and the API share, and
// which FeCode reads in the REST answers. The words the answers carry are
// the published module's (ADR 0057).

// The states of a deployment (env_deployments.state; ADR 0018, "The states'
// words").
const (
	// DeploymentWaiting: applied, its rollout not begun.
	DeploymentWaiting = api.DeploymentWaiting
	// DeploymentDeploying: the controller brings the environment to it.
	DeploymentDeploying = api.DeploymentDeploying
	// DeploymentDeployed: every object matched it and was ready.
	DeploymentDeployed = api.DeploymentDeployed
	// DeploymentFailed: its rollout failed (env_deployments.reason); the
	// environment keeps serving its deployed deployment.
	DeploymentFailed = api.DeploymentFailed
	// DeploymentReplaced: a later deployment took its place before or after it
	// was deployed.
	DeploymentReplaced = api.DeploymentReplaced
)

// What made a deployment (env_deployments.origin).
const (
	OriginApply  = api.OriginApply
	OriginRevert = api.OriginRevert
	OriginClone  = api.OriginClone
	OriginObject = api.OriginObject
)

// The phases of an instance (env_instances.phase) and of an attached
// service (env_attached.phase, which has neither succeeded nor
// stopping).
const (
	PhaseStarting  = api.PhaseStarting
	PhaseReady     = api.PhaseReady
	PhaseUnready   = api.PhaseUnready
	PhaseExited    = api.PhaseExited
	PhaseSucceeded = api.PhaseSucceeded
	PhaseFailed    = api.PhaseFailed
	// PhaseStopping: it no longer serves; its traffic drains until its
	// retire_at, then its pod stops (the sandboxes' op stop), and its row
	// goes once the pod has ended (ADR 0018).
	PhaseStopping = api.PhaseStopping
)

// The phases of a run of an environment's job (env_job_runs.phase).
const (
	RunRunning   = api.RunRunning
	RunSucceeded = api.RunSucceeded
	RunFailed    = api.RunFailed
)

// The phases of a project's job (jobs.phase), which are its one run's.
const (
	JobQueued           = "queued"
	JobRunning          = RunRunning
	JobSucceeded        = RunSucceeded
	JobFailed           = RunFailed
	JobDeadlineExceeded = "deadline_exceeded"
	JobCancelled        = "cancelled"
)

// The reasons of a project's failed job (jobs.reason).
const (
	// JobReasonExit: its process exited with a code other than 0.
	JobReasonExit = "exit"
	// JobReasonImage: the image's registry refused a create of its pod.
	JobReasonImage = "image"
	// JobReasonNodeLost: its pod was lost with its node.
	JobReasonNodeLost = "node_lost"
	// JobReasonNoExitStatus: it ended without an exit code (its pod
	// stopped, failed or went).
	JobReasonNoExitStatus = "no_exit_status"
)

// The conditions of an incident (env_incidents.condition).
const (
	ConditionRolloutFailed     = api.ConditionRolloutFailed
	ConditionCrashLoop         = api.ConditionCrashLoop
	ConditionJobFailed         = api.ConditionJobFailed
	ConditionNoCapacity        = api.ConditionNoCapacity
	ConditionAddonUnhealthy    = api.ConditionAddonUnhealthy
	ConditionVolumeUnavailable = api.ConditionVolumeUnavailable
	ConditionAttachFailed      = api.ConditionAttachFailed
	// ConditionNamesUnserved: a pod of the object runs on a node whose
	// feosd serves no names (it predates the NameService).
	ConditionNamesUnserved = api.ConditionNamesUnserved
	// The conditions of a domain bound to the environment (ADR 0021), on
	// the object "domain/<name>": a record of it answers wrong and its
	// grace runs (DomainRecordWrong), the grace ran out and it is no
	// longer served (DomainDeactivated), another claim's token took its
	// name (DomainSuperseded).
	ConditionDomainRecordWrong = api.ConditionDomainRecordWrong
	ConditionDomainDeactivated = api.ConditionDomainDeactivated
	ConditionDomainSuperseded  = api.ConditionDomainSuperseded
	// The conditions of an HTTP binding of the environment (ADR 0028 §6),
	// on the object "binding/<name>", from its pods' requests over the
	// last five minutes: mostly refused by the upstream with 401 or 403
	// (CredentialRefused), mostly 429, 5xx or an upstream that could not
	// be reached (UpstreamFailing), its token endpoint failing
	// (TokenFailing).
	ConditionCredentialRefused = api.ConditionCredentialRefused
	ConditionUpstreamFailing   = api.ConditionUpstreamFailing
	ConditionTokenFailing      = api.ConditionTokenFailing
)

// The states of an environment in the list (GET /api/v1/environments)
// besides its latest deployment's state.
const (
	// EnvNone: the environment has no deployment yet.
	EnvNone = api.EnvNone
	// EnvDeleting: the environment is being deleted.
	EnvDeleting = api.EnvDeleting
)
