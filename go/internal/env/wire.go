// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"encoding/json"

	"github.com/gardenlinux/feos/go/ferun/api"
)

// The request and answer bodies of the environments' REST routes are the
// published module's (ADR 0057), github.com/gardenlinux/feos/go/ferun/api:
// these aliases are the same definitions under the service's names, not
// copies.
type (
	CreateEnvRequest    = api.CreateEnvRequest
	RenameEnvRequest    = api.RenameEnvRequest
	EnvList             = api.EnvList
	EnvSummary          = api.EnvSummary
	EnvStatus           = api.EnvStatus
	RolloutStatus       = api.RolloutStatus
	ObjectStatus        = api.ObjectStatus
	InstanceStatus      = api.InstanceStatus
	JobRunStatus        = api.JobRunStatus
	RouteStatus         = api.RouteStatus
	PlanSpecRequest     = api.PlanSpecRequest
	PlanSpecAnswer      = api.PlanSpecAnswer
	PlanRequest         = api.PlanRequest
	PlanAnswer          = api.PlanAnswer
	AddonCatalog        = api.AddonCatalog
	AddonType           = api.AddonType
	AddonPlan           = api.AddonPlan
	ApplyRequest        = api.ApplyRequest
	DeploymentList      = api.DeploymentList
	DeploymentInfo      = api.DeploymentInfo
	DeploymentDetail    = api.DeploymentDetail
	ApplyAnswer         = api.ApplyAnswer
	RevertRequest       = api.RevertRequest
	PutObjectRequest    = api.PutObjectRequest
	DeleteObjectRequest = api.DeleteObjectRequest
	ObjectAnswer        = api.ObjectAnswer
	CloneRequest        = api.CloneRequest
	CloneCredential     = api.CloneCredential
	CloneSecret         = api.CloneSecret
	TagList             = api.TagList
	Tag                 = api.Tag
	PutTagRequest       = api.PutTagRequest
	SecretList          = api.SecretList
	SecretInfo          = api.SecretInfo
	PutSecretRequest    = api.PutSecretRequest
	SecretVersion       = api.SecretVersion
	IncidentList        = api.IncidentList
	Incident            = api.Incident
	IncidentEvidence    = api.IncidentEvidence
	Exit                = api.Exit
	LogsAnswer          = api.LogsAnswer
	PublicRequest       = api.PublicRequest
	PublicAnswer        = api.PublicAnswer
	RouteToken          = api.RouteToken
)

// The doors of a route (RouteStatus.Door) and the kinds of a secret.
const (
	DoorIngress = api.DoorIngress
	DoorRoute   = api.DoorRoute
	SecretValue = api.SecretValue
	SecretHTTP  = api.SecretHTTP
)

// httpFields reports whether the request has a field of an HTTP secret's
// definition.
func httpFields(r PutSecretRequest) bool {
	return r.Upstream != "" || r.Mode != "" || r.CaPem != "" || r.PathPrefixes != nil || r.ForwardedHost ||
		len(r.RateLimit) > 0 || len(r.Credential) > 0
}

// httpDefinition is the request's HTTP secret's definition as the
// bindings read it.
func httpDefinition(r PutSecretRequest) ([]byte, error) {
	return json.Marshal(struct {
		Upstream      string          `json:"upstream,omitempty"`
		Mode          string          `json:"mode,omitempty"`
		CaPem         string          `json:"caPem,omitempty"`
		PathPrefixes  []string        `json:"pathPrefixes,omitempty"`
		ForwardedHost bool            `json:"forwardedHost,omitempty"`
		RateLimit     json.RawMessage `json:"rateLimit,omitempty"`
		Credential    json.RawMessage `json:"credential,omitempty"`
	}{r.Upstream, r.Mode, r.CaPem, r.PathPrefixes, r.ForwardedHost, r.RateLimit, r.Credential})
}
