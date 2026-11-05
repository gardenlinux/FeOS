// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package bindings

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/bindings/httpcheck"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// A pod's HTTP bindings come from one of two places: a sandbox's own set
// (http_bindings, PUT …/http), or, for an environment's pod, the bindings
// its service or job names, as the deployment its instance belongs to has
// them, their secrets at the versions that deployment pins (EnvSource).
// No other: a pod gets exactly the bindings its object names.
//
// Both reach the pod twice: sealed to the run's key with every other
// binding (PodBindings, IssuePodCredentials), and their clear part, the
// names its resolver answers and its interception CAs' certificates, in
// its spec at the create and by SetPodHttpBindings when it changes
// (PodHTTP; the engine sends it). The clear part's version is the pod's
// version that PodBindings seals, so that feosd tells the pod's
// feos-authn to fetch its bindings again when it moves.

// EnvSource gives the HTTP bindings of an environment's pod (*env.Service):
// those its object names, as the deployment its instance (or job run)
// belongs to has them, by name, and the version of their definitions (the
// secrets by name and pinned version, never a value). open: with the
// secrets' values; else the credentials' secret fields are empty. A row
// whose object names no binding: nil, "".
type EnvSource interface {
	RowHTTPBindings(ctx context.Context, row store.Sandbox, open bool) ([]*fleetv1.HttpBinding, string, error)
}

// Pusher has a pod's node sent the pod's clear part (*sandbox.Engine):
// in the background, at once.
type Pusher interface {
	PushHTTPBindings(id podid.ID)
}

// SetEnvSource sets the source of the environments' pods' bindings; nil:
// they have none.
func (s *Service) SetEnvSource(src EnvSource) {
	s.hooks.Lock()
	defer s.hooks.Unlock()
	s.envSrc = src
}

// SetPusher sets what sends a pod its clear part after a PUT; nil: the
// pod gets it at its next start.
func (s *Service) SetPusher(p Pusher) {
	s.hooks.Lock()
	defer s.hooks.Unlock()
	s.pusher = p
}

func (s *Service) envSource() EnvSource {
	s.hooks.Lock()
	defer s.hooks.Unlock()
	return s.envSrc
}

// push has the pod sent its clear part, when a pusher is set.
func (s *Service) push(id podid.ID) {
	s.hooks.Lock()
	p := s.pusher
	s.hooks.Unlock()
	if p != nil {
		p.PushHTTPBindings(id)
	}
}

// isEnvPod reports whether the row's HTTP bindings are its environment's:
// an environment's pod whose template names bindings.
func isEnvPod(row store.Sandbox) bool {
	return row.EnvironmentID != uuid.Nil && row.PodTemplate != nil && len(row.PodTemplate.HTTPBindings) > 0
}

// podVersionHTTP is a pod's version with its HTTP set and the CAs it is
// given: base (podVersion of its other sets) when it has neither, else the
// lowercase hex SHA-256 of base, the HTTP set's version and each CA's
// certificate digest, oldest first, each followed by a zero byte.
func podVersionHTTP(base, httpVersion string, cas []store.InterceptCA) string {
	if httpVersion == "" && len(cas) == 0 {
		return base
	}
	h := sha256.New()
	h.Write([]byte("http v1\x00" + base + "\x00" + httpVersion + "\x00"))
	for _, d := range caDigest(cas) {
		h.Write([]byte(d + "\x00"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// clearPart is the pod's PodHttpBindings: the name mode's names, the
// intercept mode's hosts, the CAs' certificates (when it intercepts) and
// the version; nil without HTTP bindings.
func clearPart(bs []httpBinding, cas []store.InterceptCA, version string) *podv1.PodHttpBindings {
	if len(bs) == 0 {
		return nil
	}
	out := &podv1.PodHttpBindings{Version: version, Names: []string{}, InterceptHosts: interceptHosts(bs),
		InterceptCaPems: []string{}}
	for _, b := range bs {
		if b.Mode != httpcheck.ModeIntercept {
			out.Names = append(out.Names, b.Name)
		}
	}
	slices.Sort(out.Names)
	if len(out.InterceptHosts) == 0 {
		out.InterceptHosts = []string{}
	} else {
		for _, c := range cas {
			out.InterceptCaPems = append(out.InterceptCaPems, c.CertPEM)
		}
	}
	return out
}

// givenCAs are the CAs a pod is given: all of them while it intercepts,
// none otherwise.
func givenCAs(bs []httpBinding, cas []store.InterceptCA) []store.InterceptCA {
	if len(interceptHosts(bs)) == 0 {
		return nil
	}
	return cas
}

// envPod reads an environment's pod's bindings (opened when open), makes
// its CA when it intercepts and has none for its names, and returns them
// with the CAs it is given and its version.
func (s *Service) envPod(ctx context.Context, row store.Sandbox, open bool) ([]httpBinding, []store.InterceptCA,
	string, error) {
	src := s.envSource()
	if src == nil {
		return nil, nil, "", nil
	}
	ps, defs, err := src.RowHTTPBindings(ctx, row, open)
	if err != nil {
		return nil, nil, "", fmt.Errorf("the bindings of %s: %w", row.ID, err)
	}
	bs := make([]httpBinding, 0, len(ps))
	for _, p := range ps {
		bs = append(bs, httpBindingOf(p))
	}
	var cas []store.InterceptCA
	if hosts := interceptHosts(bs); len(hosts) > 0 {
		if cas, err = s.ensureCAs(ctx, s.st, row.ID, hosts); err != nil {
			return nil, nil, "", err
		}
	}
	if len(bs) == 0 {
		return nil, nil, "", nil
	}
	return bs, cas, podVersionHTTP("", defs, cas), nil
}

// PodHTTP is the clear part of the pod's HTTP bindings as the row has
// them now, which its spec carries at the create and SetPodHttpBindings
// afterwards: nil when it has none, or the surfaces are off. An
// environment's pod that intercepts gets its CA here, at its first
// create.
func (s *Service) PodHTTP(ctx context.Context, row store.Sandbox) (*podv1.PodHttpBindings, error) {
	if !s.enabled {
		return nil, nil
	}
	if isEnvPod(row) {
		bs, cas, version, err := s.envPod(ctx, row, false)
		if err != nil {
			return nil, err
		}
		return clearPart(bs, givenCAs(bs, cas), version), nil
	}
	if row.IsInstance() {
		return nil, nil
	}
	snap, err := s.snapshot(ctx, row.ID)
	if errors.Is(err, errNoSandbox) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	bs := httpPlain(snap.http)
	cas := givenCAs(bs, snap.cas)
	version := podVersionHTTP(podVersion(snap.kubeVersion, snap.sshVersion, snapSSH(snap)), snap.httpVersion, cas)
	return clearPart(bs, cas, version), nil
}

// httpPlain are a stored set's bindings without their credentials: what
// the clear part needs.
func httpPlain(rows []store.HTTPBinding) []httpBinding {
	out := make([]httpBinding, 0, len(rows))
	for _, r := range rows {
		b := httpBinding{Name: r.Name, Upstream: r.Upstream, Mode: r.Mode}
		if up, err := httpcheck.ParseUpstream("upstream", r.Upstream); err == nil {
			b.Host = up.Host
		}
		out = append(out, b)
	}
	return out
}

// snapSSH is the SSH set of a snapshot as podVersion takes it: name and
// server.
func snapSSH(snap podSnapshot) []sshStored {
	out := make([]sshStored, 0, len(snap.ssh))
	for _, r := range snap.ssh {
		out = append(out, sshStored{sshBinding: sshBinding{Name: r.Name}, server: r.Server})
	}
	return out
}

// envPodBindings is PodBindings of an environment's pod.
func (s *Service) envPodBindings(ctx context.Context, row store.Sandbox) (*fleetv1.PodBindings, string, error) {
	bs, cas, version, err := s.envPod(ctx, row, true)
	if err != nil || len(bs) == 0 {
		return nil, "", err
	}
	out := &fleetv1.PodBindings{}
	for _, b := range bs {
		out.Http = append(out.Http, b.proto())
	}
	if out.InterceptCas, err = s.openCAs(row.ID, givenCAs(bs, cas)); err != nil {
		return nil, "", err
	}
	return out, version, nil
}

// CheckDestination is the destination rule on a binding's host, as a PUT
// applies it: nil for a destination, else why not. The environments' plans
// ask it of their bindings' hosts.
func (s *Service) CheckDestination(ctx context.Context, host string) error {
	if berr := s.dest.checkDestination(ctx, host); berr != nil {
		return berr
	}
	return nil
}

// rowOf reads a pod's row; nil when there is none.
func (s *Service) rowOf(ctx context.Context, id podid.ID) (*store.Sandbox, error) {
	row, err := s.st.GetSandbox(ctx, id)
	if errors.Is(err, model.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sandbox %s: %w", id, err)
	}
	return &row, nil
}
