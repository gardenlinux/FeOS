// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package apierr is the REST API's error: a status, a message and, for the
// surfaces that have them, a machine-readable code. Clients match on all
// three, so their texts are part of the API.
package apierr

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// MsgSandboxID is the message of a 400 for a sandbox ID that is neither
// a short ID nor a UUID (ADR 0019), in a path, a body or a query.
const MsgSandboxID = "sandboxId must be a sandbox ID: 11 characters of lowercase Crockford base32, " +
	"the first a letter, or a UUID"

// Error is an error the API answers with its own status and body
// {"error": Msg, "code": Code}; Code is left out when empty.
type Error struct {
	Status int
	Msg    string
	Code   string
	// Header is set on the response, e.g. Retry-After.
	Header http.Header
	// Role and Scope are a role_required answer's (ADR 0031): the role
	// the principal lacks, and on what ("tenant" or "environment").
	Role, Scope string
}

func (e *Error) Error() string { return e.Msg }

// New returns an Error with a status and a message.
func New(status int, format string, args ...any) *Error {
	return &Error{Status: status, Msg: fmt.Sprintf(format, args...)}
}

// WithCode returns an Error with a status, a code and a message.
func WithCode(status int, code, format string, args ...any) *Error {
	return &Error{Status: status, Code: code, Msg: fmt.Sprintf(format, args...)}
}

// RetryAfter sets the response's Retry-After, in seconds.
func (e *Error) RetryAfter(seconds int) *Error {
	if e.Header == nil {
		e.Header = http.Header{}
	}
	e.Header.Set("Retry-After", fmt.Sprint(seconds))
	return e
}

// RetryAfterOf is the Retry-After, in seconds, that err asks for: the
// first error in its chain with a method RetryAfter() time.Duration (the
// engine's "sandbox <id> is hibernating; retry in a minute" says 60),
// rounded up to whole seconds, or def when none says a positive time.
func RetryAfterOf(err error, def int) int {
	var later interface{ RetryAfter() time.Duration }
	if !errors.As(err, &later) {
		return def
	}
	d := later.RetryAfter()
	if d <= 0 {
		return def
	}
	return int((d + time.Second - 1) / time.Second)
}

// CodeRoleRequired is the code of a 403 for a principal that sees what it
// names but lacks the role for what it asks (ADR 0031).
const CodeRoleRequired = "role_required"

// RoleRequired is 403 {"code": "role_required", "role", "scope"}: the
// principal needs role on scope ("tenant" or "environment").
func RoleRequired(role, scope string) *Error {
	return &Error{Status: http.StatusForbidden, Code: CodeRoleRequired, Role: role, Scope: scope,
		Msg: fmt.Sprintf("this needs the role %s on the %s", role, scope)}
}

type body struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
	Role  string `json:"role,omitempty"`
	Scope string `json:"scope,omitempty"`
}

// Write answers err: an *Error with its status and body, anything else with
// 500 and the error's text, as the sandbox system's API did.
func Write(w http.ResponseWriter, err error) {
	var e *Error
	if !errors.As(err, &e) {
		e = &Error{Status: http.StatusInternalServerError, Msg: err.Error()}
	}
	for k, vs := range e.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	WriteJSON(w, e.Status, body{Error: e.Msg, Code: e.Code, Role: e.Role, Scope: e.Scope})
}

// WriteJSON answers v as JSON with status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
