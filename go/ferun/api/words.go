// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

// The headers of a request to FeRun: ProjectHeader names the project a
// session acts in (a project token's is its own); PersonHeader names,
// on an integration's request, the person it acts for (ADR 0056).
const (
	ProjectHeader = "FeRun-Project"
	PersonHeader  = "FeRun-Person"
)

// The words answers carry, which a client compares (ADR 0018, "The
// states' words").

// The states of a deployment (DeploymentInfo.State, RolloutStatus.State,
// EnvSummary.State).
const (
	// DeploymentWaiting: applied, its rollout not begun.
	DeploymentWaiting = "waiting"
	// DeploymentDeploying: the controller brings the environment to it.
	DeploymentDeploying = "deploying"
	// DeploymentDeployed: every object matched it and was ready.
	DeploymentDeployed = "deployed"
	// DeploymentFailed: its rollout failed (its reason); the environment
	// keeps serving its deployed deployment.
	DeploymentFailed = "failed"
	// DeploymentReplaced: a later deployment took its place before or
	// after it was deployed.
	DeploymentReplaced = "replaced"
)

// The states of an environment in the list (EnvSummary.State) besides
// its latest deployment's state.
const (
	// EnvNone: the environment has no deployment yet.
	EnvNone = "none"
	// EnvDeleting: the environment is being deleted.
	EnvDeleting = "deleting"
)

// What made a deployment (DeploymentInfo.Origin).
const (
	OriginApply  = "apply"
	OriginRevert = "revert"
	OriginClone  = "clone"
	OriginObject = "object"
)

// The phases of an instance (InstanceStatus.Phase) and of an attached
// service (ObjectStatus.Phase, which has neither succeeded nor
// stopping).
const (
	PhaseStarting  = "starting"
	PhaseReady     = "ready"
	PhaseUnready   = "unready"
	PhaseExited    = "exited"
	PhaseSucceeded = "succeeded"
	PhaseFailed    = "failed"
	// PhaseStopping: it no longer serves; its traffic drains, then its
	// pod stops.
	PhaseStopping = "stopping"
)

// The phases of a run of an environment's job (JobRunStatus.Phase).
const (
	RunRunning   = "running"
	RunSucceeded = "succeeded"
	RunFailed    = "failed"
)

// The conditions of an incident (Incident.Condition).
const (
	ConditionRolloutFailed     = "rollout_failed"
	ConditionCrashLoop         = "crash_loop"
	ConditionJobFailed         = "job_failed"
	ConditionNoCapacity        = "no_capacity"
	ConditionAddonUnhealthy    = "addon_unhealthy"
	ConditionVolumeUnavailable = "volume_unavailable"
	ConditionAttachFailed      = "attach_failed"
	// ConditionNamesUnserved: a pod of the object runs on a node that
	// serves no names.
	ConditionNamesUnserved = "names_unserved"
	// The conditions of a domain bound to the environment, on the object
	// "domain/<name>".
	ConditionDomainRecordWrong = "domain_record_wrong"
	ConditionDomainDeactivated = "domain_deactivated"
	ConditionDomainSuperseded  = "domain_superseded"
	// The conditions of an HTTP binding, on the object "binding/<name>".
	ConditionCredentialRefused = "credential_refused"
	ConditionUpstreamFailing   = "upstream_failing"
	ConditionTokenFailing      = "token_failing"
)

// Who a public environment admits (EnvStatus.Access, ADR 0054), and the
// provider every project has without making it (EnvStatus.Provider).
const (
	AccessAnyone   = "anyone"
	AccessVisitors = "visitors"
	ProviderFerun  = "ferun"
)
