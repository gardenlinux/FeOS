// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package bindings

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The access route, GET /api/v1/sandboxes/{id}/access (project): the
// sandbox's two binding sets as their own GETs render them, each binding
// with what the audit log says it was used for, and the newest refusals.
// A project route, unlike the sets' own: the owner can mint a sandbox token
// at will, so reading the sets here shows nothing the project could not
// read already. Changing them stays the sandbox token's.

// accessDenyLimit is how many refusals the route answers, newest first.
const accessDenyLimit = 20

type bindingUseOut struct {
	Uses       int64  `json:"uses"`
	Errors     int64  `json:"errors"`
	LastUsedAt string `json:"lastUsedAt"`
	LastUse    string `json:"lastUse"`
}

type kubeAccessOut struct {
	kubeBindingOut
	Use *bindingUseOut `json:"use"`
}

type sshAccessOut struct {
	sshBindingOut
	Use *bindingUseOut `json:"use"`
}

type accessDenyOut struct {
	Kind    string `json:"kind"` // kube or ssh
	Binding string `json:"binding"`
	Reason  string `json:"reason"`
	Detail  string `json:"detail"`
	At      string `json:"at"`
	Name    string `json:"name"`
}

type kubeAccessSet struct {
	Version  string          `json:"version"`
	Bindings []kubeAccessOut `json:"bindings"`
}

type sshAccessSet struct {
	Version  string         `json:"version"`
	Bindings []sshAccessOut `json:"bindings"`
}

// accessOut's kube and ssh are null when the surfaces are off: there is no
// set, not an empty one.
type accessOut struct {
	Kube   *kubeAccessSet  `json:"kube"`
	SSH    *sshAccessSet   `json:"ssh"`
	Denies []accessDenyOut `json:"denies"`
}

// isoNS renders a nanosecond time as RFC 3339 with nanoseconds, UTC.
func isoNS(ns int64) string { return time.Unix(0, ns).UTC().Format(time.RFC3339Nano) }

// Access answers the sandbox's access for its owner; off, the empty
// object {"kube":null,"ssh":null,"denies":[]}.
func (s *Service) Access(w http.ResponseWriter, r *http.Request) {
	project, ok := auth.ProjectFrom(r.Context())
	if !ok {
		apierr.Write(w, apierr.New(http.StatusUnauthorized, "project bearer required"))
		return
	}
	raw := r.PathValue("id")
	id, err := podid.Parse(raw)
	if err != nil {
		apierr.Write(w, apierr.New(http.StatusBadRequest, apierr.MsgSandboxID))
		return
	}
	sb, err := s.st.GetSandbox(r.Context(), id)
	if errors.Is(err, model.ErrNotFound) || err == nil && sb.Owner != project.Name {
		// A row of another project is indistinguishable from an absent one.
		apierr.Write(w, apierr.New(http.StatusNotFound, "unknown sandbox %s", raw))
		return
	}
	if err != nil {
		s.fail(w, r, fmt.Errorf("get %s: %w", id, err))
		return
	}
	out := accessOut{Denies: []accessDenyOut{}}
	if !s.enabled {
		apierr.WriteJSON(w, http.StatusOK, out)
		return
	}
	uses, denies, err := s.st.BindingUsage(r.Context(), id, accessDenyLimit)
	if err != nil {
		s.fail(w, r, fmt.Errorf("binding usage %s: %w", id, err))
		return
	}
	useOf := func(kind, name string) *bindingUseOut {
		for _, u := range uses {
			if u.Kind == kind && u.Binding == name {
				return &bindingUseOut{Uses: u.Uses, Errors: u.Errors, LastUsedAt: isoNS(u.LastTsNs), LastUse: u.LastName}
			}
		}
		return nil
	}
	kube, err := s.kubeSet(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out.Kube = &kubeAccessSet{Version: kube.Version, Bindings: []kubeAccessOut{}}
	for _, b := range kube.Bindings {
		out.Kube.Bindings = append(out.Kube.Bindings, kubeAccessOut{b, useOf(store.KindKubeRequest, b.Name)})
	}
	ssh, err := s.sshSet(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out.SSH = &sshAccessSet{Version: ssh.Version, Bindings: []sshAccessOut{}}
	for _, b := range ssh.Bindings {
		out.SSH.Bindings = append(out.SSH.Bindings, sshAccessOut{b, useOf(store.KindSSHSignature, b.Name)})
	}
	for _, d := range denies {
		kind := "ssh"
		if d.Kind == store.KindKubeDeny {
			kind = "kube"
		}
		out.Denies = append(out.Denies, accessDenyOut{Kind: kind, Binding: d.Binding, Reason: d.Reason,
			Detail: d.Detail, At: isoNS(d.TsNs), Name: d.Name})
	}
	apierr.WriteJSON(w, http.StatusOK, out)
}
