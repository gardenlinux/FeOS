// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// i64 is an int64 rendered as a JSON string, protojson's encoding, which
// the clients read: nanosecond times and byte counts run past what a JSON
// number survives in a browser.
type i64 int64

func (n i64) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(strconv.FormatInt(int64(n), 10))), nil
}

// UnmarshalJSON reads an int64 written as a string or a number.
func (n *i64) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	*n = i64(v)
	return nil
}

// writeJSON answers v with status, as JSON with a trailing newline.
func writeJSON(w http.ResponseWriter, status int, v any) error {
	apierr.WriteJSON(w, status, v)
	return nil
}

// rfc3339 renders a time as the API does for sandboxes, tokens, projects
// and the other row times: RFC 3339 in seconds, UTC; "" for the zero time.
func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// unixNs renders a time as nanoseconds since the epoch (node and activity
// times); 0 for the zero time.
func unixNs(t time.Time) i64 {
	if t.IsZero() {
		return 0
	}
	return i64(t.UnixNano())
}

// decodeJSON reads a request body (at most 32 MiB) into v. An empty body
// is {}; a body that is not a JSON object, or does not fit v, is a 400.
// It returns the keys the body has, which tell an absent field from its
// zero value.
func decodeJSON(r *http.Request, v any) (map[string]json.RawMessage, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return nil, apierr.New(http.StatusBadRequest, "read body: %v", err)
	}
	if len(raw) > maxBody {
		return nil, apierr.New(http.StatusRequestEntityTooLarge, "request body larger than %d bytes", maxBody)
	}
	if strings.TrimSpace(string(raw)) == "" {
		raw = []byte("{}")
	}
	sent := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &sent); err != nil {
		return nil, apierr.New(http.StatusBadRequest, "request body must be a JSON object: %v", err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return nil, apierr.New(http.StatusBadRequest, "invalid request body: %v", err)
	}
	return sent, nil
}

// specBody reads the body of a route that takes a spec (plan, plan
// without an environment, apply, clone; ADR 0057): with a YAML content
// type (application/yaml, application/x-yaml, text/yaml; any charset,
// the body UTF-8) the body is the spec document, returned as doc with
// spec.FormatYAML, and the envelope's other fields are the caller's to
// read from the query; with any other content type, or none, it is the
// JSON envelope as before, decoded into v (decodeJSON), and doc is nil.
func specBody(r *http.Request, v any) (spec.Format, []byte, error) {
	if !isYAML(r.Header.Get("Content-Type")) {
		_, err := decodeJSON(r, v)
		return spec.FormatJSON, nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return spec.FormatYAML, nil, apierr.New(http.StatusBadRequest, "read body: %v", err)
	}
	if len(raw) > maxBody {
		return spec.FormatYAML, nil, apierr.New(http.StatusRequestEntityTooLarge, "request body larger than %d bytes",
			maxBody)
	}
	return spec.FormatYAML, raw, nil
}

// isYAML reports whether a Content-Type names YAML.
func isYAML(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	switch mt {
	case "application/yaml", "application/x-yaml", "text/yaml":
		return true
	}
	return false
}

// queryIntPtr reads an optional integer query parameter; nil when the
// query does not have it.
func queryIntPtr(r *http.Request, name string) (*int, error) {
	if !r.URL.Query().Has(name) {
		return nil, nil
	}
	n, err := queryInt(r, name)
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// queryInt reads an optional integer query parameter.
func queryInt(r *http.Request, name string) (int, error) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, apierr.New(http.StatusBadRequest, "%s must be an integer, got %q", name, s)
	}
	return n, nil
}

// queryInt64 reads an optional int64 query parameter.
func queryInt64(r *http.Request, name string) (int64, error) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, apierr.New(http.StatusBadRequest, "%s must be an integer, got %q", name, s)
	}
	return n, nil
}

// queryBool reads an optional boolean query parameter; a bare flag
// ("?force") counts as true.
func queryBool(r *http.Request, name string) (bool, error) {
	q := r.URL.Query()
	if !q.Has(name) {
		return false, nil
	}
	s := q.Get(name)
	if s == "" {
		return true, nil
	}
	b, err := strconv.ParseBool(s)
	if err != nil {
		return false, apierr.New(http.StatusBadRequest, "%s must be a boolean, got %q", name, s)
	}
	return b, nil
}

// parseSandboxID parses a sandbox ID (ADR 0019): a short ID, or the UUID
// of a sandbox made before, in its hyphenated form in either case
// (rendered in lowercase from then on).
func parseSandboxID(s string) (podid.ID, error) {
	id, err := podid.Parse(s)
	if err != nil {
		return "", apierr.New(http.StatusBadRequest, apierr.MsgSandboxID)
	}
	return id, nil
}

// pathSandboxID is the {id} of the path as a sandbox ID.
func pathSandboxID(r *http.Request) (podid.ID, error) {
	return parseSandboxID(r.PathValue("id"))
}

// The desired states the status shows: every row is active (a deleted
// sandbox has no row); the DELETE answer shows deleted.
const (
	desiredActive  = "active"
	desiredDeleted = "deleted"
	// stateAbsent is the client state of a DELETE answer that left a
	// tombstone: the pod is going.
	stateAbsent = "absent"
)

// sandboxStatus is a sandbox as the API shows it: always every field.
type sandboxStatus struct {
	SandboxID string `json:"sandboxId"`
	// State is a deprecated alias of ClientState, kept for old clients.
	State     string `json:"state"`
	Image     string `json:"image"`
	Node      string `json:"node"`
	Owner     string `json:"owner"`
	CreatedAt string `json:"createdAt"`
	// UpdatedAt doubles as the last activity.
	UpdatedAt         string `json:"updatedAt"`
	DesiredState      string `json:"desiredState"`
	ClientState       string `json:"clientState"`
	StatusDescription string `json:"statusDescription"`
	CheckpointEpoch   i64    `json:"checkpointEpoch"`
	CheckpointHead    string `json:"checkpointHead"`
	CheckpointAt      string `json:"checkpointAt"`
	Vcpus             int    `json:"vcpus"`
	MemoryMib         int    `json:"memoryMib"`
	// DiskGib is its disk of its own size; absent: the guest's, 8 GiB.
	DiskGib int `json:"diskGib,omitempty"`
	// ExpiresAfterIdle is a caller's sandbox's idle time (ADR 0065),
	// "<n>d", "<n>h" or "never"; absent for an instance and a project's
	// job. ExpiresAt is when it expires if nothing uses it before, absent
	// while it runs and for never; ExpiredAt when it expired.
	ExpiresAfterIdle string `json:"expiresAfterIdle,omitempty"`
	ExpiresAt        string `json:"expiresAt,omitempty"`
	ExpiredAt        string `json:"expiredAt,omitempty"`
}

// statusOf renders a row.
func statusOf(sb store.Sandbox) sandboxStatus {
	updated := sb.UpdatedAt
	if sb.LastActiveAt.After(updated) {
		updated = sb.LastActiveAt
	}
	st := sandboxStatus{
		SandboxID:         sb.ID.String(),
		State:             sb.ClientState,
		Image:             sb.Image,
		Node:              sb.Node,
		Owner:             sb.Owner,
		CreatedAt:         rfc3339(sb.CreatedAt),
		UpdatedAt:         rfc3339(updated),
		DesiredState:      desiredActive,
		ClientState:       sb.ClientState,
		StatusDescription: sb.StatusDescription,
		CheckpointEpoch:   i64(sb.CheckpointEpoch),
		CheckpointHead:    sb.CheckpointHead,
		CheckpointAt:      rfc3339(sb.CheckpointAt),
		Vcpus:             sb.VCPUs,
		MemoryMib:         sb.MemoryMiB,
		DiskGib:           sb.DiskGiB,
	}
	if sb.IsOwn() {
		st.ExpiresAfterIdle = model.IdleTimeText(sb.ExpiresAfterIdleS)
		if t, ok := sb.ExpiresAt(); ok {
			st.ExpiresAt = rfc3339(t)
		}
		st.ExpiredAt = rfc3339(sb.ExpiredAt)
	}
	return st
}

// internalf is a 500 with a text of its own, whatever err wraps (an
// *apierr.Error inside err does not take over the answer).
func internalf(format string, args ...any) error {
	return apierr.New(http.StatusInternalServerError, "%s", fmt.Sprintf(format, args...))
}

// isNoRow reports a lookup of a row that does not exist.
func isNoRow(err error) bool { return errors.Is(err, store.ErrNoRow) }
