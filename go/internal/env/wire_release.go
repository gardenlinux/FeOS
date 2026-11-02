// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// The deployments' earlier word (ADR 0018, "The deployments' word"). For
// one version, the first of two, the REST answers carry every number they
// call deployment under its earlier key release too, and a list of
// deployments as releases too, so that a client of the earlier words
// (FeCode before its move) keeps reading them while it moves; a request
// gives a number under either key, both only when they are equal. The
// next version answers and reads the new keys only and removes this file.

// marshalPlain is v's JSON as encoding/json writes it, without escaping
// HTML: the encoder of the whole answer escapes as it does elsewhere.
func marshalPlain(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// withEarlierKey is the JSON object v with key set to val, last.
func withEarlierKey(v any, key string, val any) ([]byte, error) {
	obj, err := marshalPlain(v)
	if err != nil {
		return nil, err
	}
	b, err := marshalPlain(val)
	if err != nil {
		return nil, err
	}
	if len(obj) < 2 || obj[len(obj)-1] != '}' {
		return nil, fmt.Errorf("%T is not a JSON object", v)
	}
	out := make([]byte, 0, len(obj)+len(key)+len(b)+4)
	out = append(out, obj[:len(obj)-1]...)
	if len(obj) > 2 {
		out = append(out, ',')
	}
	out = append(out, '"')
	out = append(out, key...)
	out = append(out, '"', ':')
	out = append(out, b...)
	return append(out, '}'), nil
}

// eitherKey is the number given under deployment, under release, or under
// both when they are equal; nil for neither.
func eitherKey(deployment, release *int) (*int, error) {
	if deployment != nil && release != nil && *deployment != *release {
		return nil, fmt.Errorf("deployment %d and release %d differ: give deployment alone", *deployment, *release)
	}
	if deployment != nil {
		return deployment, nil
	}
	return release, nil
}

// numberOf is n's value, 0 for none.
func numberOf(n *int) int {
	if n == nil {
		return 0
	}
	return *n
}

// MarshalJSON writes the summary with release beside deployment.
func (s EnvSummary) MarshalJSON() ([]byte, error) {
	type plain EnvSummary
	return withEarlierKey(plain(s), "release", s.Deployment)
}

// UnmarshalJSON reads deployment or release.
func (s *EnvSummary) UnmarshalJSON(b []byte) error {
	type plain EnvSummary
	var v struct {
		plain
		Deployment *int `json:"deployment"`
		Release    *int `json:"release"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	n, err := eitherKey(v.Deployment, v.Release)
	*s = EnvSummary(v.plain)
	s.Deployment = numberOf(n)
	return err
}

// MarshalJSON writes the status with release beside deployment.
func (s EnvStatus) MarshalJSON() ([]byte, error) {
	type plain EnvStatus
	return withEarlierKey(plain(s), "release", s.Deployment)
}

// UnmarshalJSON reads deployment or release.
func (s *EnvStatus) UnmarshalJSON(b []byte) error {
	type plain EnvStatus
	var v struct {
		plain
		Deployment *int `json:"deployment"`
		Release    *int `json:"release"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	n, err := eitherKey(v.Deployment, v.Release)
	*s = EnvStatus(v.plain)
	s.Deployment = numberOf(n)
	return err
}

// MarshalJSON writes the rollout with release beside deployment.
func (r RolloutStatus) MarshalJSON() ([]byte, error) {
	type plain RolloutStatus
	return withEarlierKey(plain(r), "release", r.Deployment)
}

// UnmarshalJSON reads deployment or release.
func (r *RolloutStatus) UnmarshalJSON(b []byte) error {
	type plain RolloutStatus
	var v struct {
		plain
		Deployment *int `json:"deployment"`
		Release    *int `json:"release"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	n, err := eitherKey(v.Deployment, v.Release)
	*r = RolloutStatus(v.plain)
	r.Deployment = numberOf(n)
	return err
}

// MarshalJSON writes the instance with release beside deployment.
func (i InstanceStatus) MarshalJSON() ([]byte, error) {
	type plain InstanceStatus
	return withEarlierKey(plain(i), "release", i.Deployment)
}

// UnmarshalJSON reads deployment or release.
func (i *InstanceStatus) UnmarshalJSON(b []byte) error {
	type plain InstanceStatus
	var v struct {
		plain
		Deployment *int `json:"deployment"`
		Release    *int `json:"release"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	n, err := eitherKey(v.Deployment, v.Release)
	*i = InstanceStatus(v.plain)
	i.Deployment = numberOf(n)
	return err
}

// MarshalJSON writes the run with release beside deployment.
func (r JobRunStatus) MarshalJSON() ([]byte, error) {
	type plain JobRunStatus
	return withEarlierKey(plain(r), "release", r.Deployment)
}

// UnmarshalJSON reads deployment or release.
func (r *JobRunStatus) UnmarshalJSON(b []byte) error {
	type plain JobRunStatus
	var v struct {
		plain
		Deployment *int `json:"deployment"`
		Release    *int `json:"release"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	n, err := eitherKey(v.Deployment, v.Release)
	*r = JobRunStatus(v.plain)
	r.Deployment = numberOf(n)
	return err
}

// MarshalJSON writes the list with releases beside deployments.
func (l DeploymentList) MarshalJSON() ([]byte, error) {
	type plain DeploymentList
	return withEarlierKey(plain(l), "releases", l.Deployments)
}

// UnmarshalJSON reads deployments or releases.
func (l *DeploymentList) UnmarshalJSON(b []byte) error {
	var v struct {
		Deployments []DeploymentInfo `json:"deployments"`
		Releases    []DeploymentInfo `json:"releases"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	l.Deployments = v.Deployments
	if l.Deployments == nil {
		l.Deployments = v.Releases
	}
	return nil
}

// UnmarshalJSON reads deployment or release; both must be equal.
func (r *CloneRequest) UnmarshalJSON(b []byte) error {
	type plain CloneRequest
	var v struct {
		plain
		Deployment *int `json:"deployment"`
		Release    *int `json:"release"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	n, err := eitherKey(v.Deployment, v.Release)
	*r = CloneRequest(v.plain)
	r.Deployment = n
	return err
}

// MarshalJSON writes the tag with release beside deployment.
func (t Tag) MarshalJSON() ([]byte, error) {
	type plain Tag
	return withEarlierKey(plain(t), "release", t.Deployment)
}

// UnmarshalJSON reads deployment or release.
func (t *Tag) UnmarshalJSON(b []byte) error {
	type plain Tag
	var v struct {
		plain
		Deployment *int `json:"deployment"`
		Release    *int `json:"release"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	n, err := eitherKey(v.Deployment, v.Release)
	*t = Tag(v.plain)
	t.Deployment = numberOf(n)
	return err
}

// UnmarshalJSON reads deployment or release; both must be equal.
func (r *PutTagRequest) UnmarshalJSON(b []byte) error {
	type plain PutTagRequest
	var v struct {
		plain
		Deployment *int `json:"deployment"`
		Release    *int `json:"release"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	n, err := eitherKey(v.Deployment, v.Release)
	*r = PutTagRequest(v.plain)
	r.Deployment = numberOf(n)
	return err
}

// MarshalJSON writes the incident with release beside deployment.
func (i Incident) MarshalJSON() ([]byte, error) {
	type plain Incident
	return withEarlierKey(plain(i), "release", i.Deployment)
}

// UnmarshalJSON reads deployment or release.
func (i *Incident) UnmarshalJSON(b []byte) error {
	type plain Incident
	var v struct {
		plain
		Deployment *int `json:"deployment"`
		Release    *int `json:"release"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	n, err := eitherKey(v.Deployment, v.Release)
	*i = Incident(v.plain)
	i.Deployment = numberOf(n)
	return err
}
