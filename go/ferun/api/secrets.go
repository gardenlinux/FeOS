// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
)

// An environment's secrets of both kinds (ADR 0055).

// SecretList is GET /api/v1/environments/{env}/secrets: the project's
// secrets of the environment, both kinds (ADR 0055), at their latest
// versions, by name (never a value, never a secret field, never a system
// secret).
type SecretList struct {
	Secrets []SecretInfo `json:"secrets"`
}

// The kinds of a secret (ADR 0055).
const (
	// SecretValue is a value, handed to the containers that reference it.
	SecretValue = "value"
	// SecretHTTP is an HTTP API whose key only the node uses.
	SecretHTTP = "http"
)

// SecretInfo is a secret's latest version: its name, kind, version and
// who made it when; an HTTP secret's also its definition, every secret
// field {"set": true}, and its uses (HTTPSecretDef, nil for a value).
type SecretInfo struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Version   int    `json:"version"`
	CreatedBy string `json:"createdBy"`
	CreatedAt string `json:"createdAt"`
	*HTTPSecretDef
}

// PutSecretRequest is PUT /api/v1/environments/{env}/secrets/{name}: a new
// version. Type "value" or none is a value secret's: Value. Type "http"
// is an HTTP secret's (ADR 0055): its definition as a credential's PUT of
// ADR 0033 has it, a secret field {"set": true} for the latest version's
// value while the origin, the CA and the token URL stay.
type PutSecretRequest struct {
	Type   string `json:"type,omitempty"`
	Value  string `json:"value,omitempty"`
	Author string `json:"author,omitempty"`

	Upstream      string          `json:"upstream,omitempty"`
	Mode          string          `json:"mode,omitempty"`
	CaPem         string          `json:"caPem,omitempty"`
	PathPrefixes  []string        `json:"pathPrefixes,omitempty"`
	ForwardedHost bool            `json:"forwardedHost,omitempty"`
	RateLimit     json.RawMessage `json:"rateLimit,omitempty"`
	Credential    json.RawMessage `json:"credential,omitempty"`
}

// SecretVersion is a secret put's answer: a value's name and version; an
// HTTP secret's the secret as GET answers it.
type SecretVersion struct {
	Name      string `json:"name"`
	Version   int    `json:"version"`
	Type      string `json:"type,omitempty"`
	CreatedBy string `json:"createdBy,omitempty"`
	CreatedAt string `json:"createdAt,omitempty"`
	*HTTPSecretDef
	// Created: an HTTP secret's PUT made a new version (201); else it
	// was the same definition again (200). A value's PUT answers 200.
	Created bool `json:"-"`
}

// SecretUses are where an HTTP secret is used in its environment: the
// sandboxes that name it, the services and jobs of the latest deployment
// that get it, the bindings of the latest or deployed deployment that
// name it (credentialRef), and the version each of the two deployments
// pins (0: none).
type SecretUses struct {
	Sandboxes []string `json:"sandboxes"`
	Objects   []string `json:"objects"`
	Bindings  []string `json:"bindings"`
	Latest    int      `json:"latest"`
	Deployed  int      `json:"deployed"`
}

// HTTPSecretDef is an HTTP secret's definition as an answer carries it,
// every secret field {"set": true}, with its uses.
type HTTPSecretDef struct {
	Upstream      string            `json:"upstream"`
	Mode          string            `json:"mode"`
	CaPem         string            `json:"caPem"`
	PathPrefixes  []string          `json:"pathPrefixes"`
	ForwardedHost bool              `json:"forwardedHost"`
	RateLimit     *HTTPRateLimitOut `json:"rateLimit"`
	Credential    HTTPCredentialOut `json:"credential"`
	UsedBy        SecretUses        `json:"usedBy"`
}

// SecretSet stands for a secret field's value in an answer.
type SecretSet struct {
	Set bool `json:"set"`
}

// HTTPCredentialOut is a binding's credential as an answer carries it:
// its kind and fields, every secret field {"set": true}.
type HTTPCredentialOut struct {
	Kind             string               `json:"kind"`
	Headers          map[string]SecretSet `json:"headers,omitempty"`
	User             string               `json:"user,omitempty"`
	Password         *SecretSet           `json:"password,omitempty"`
	TokenURL         string               `json:"tokenUrl,omitempty"`
	ClientID         string               `json:"clientId,omitempty"`
	ClientSecret     *SecretSet           `json:"clientSecret,omitempty"`
	Scope            string               `json:"scope,omitempty"`
	Audience         string               `json:"audience,omitempty"`
	ClientAuthInBody bool                 `json:"clientAuthInBody,omitempty"`
	CertPem          string               `json:"certPem,omitempty"`
	KeyPem           *SecretSet           `json:"keyPem,omitempty"`
}

// HTTPRateLimitOut is a binding's rate limit as an answer carries it.
type HTTPRateLimitOut struct {
	PerSecond int `json:"perSecond"`
	Burst     int `json:"burst"`
}
