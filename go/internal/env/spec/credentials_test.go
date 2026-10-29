// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"slices"
	"strings"
	"testing"
)

// A service's or job's image credentials (ADR 0025): a string or a secret
// each, on image services and jobs only; a secret they name is referenced
// and kept; a change of them replaces the service's pods, as a template's
// change does.
func TestImageCredentials(t *testing.T) {
	approval := func(s *Spec) {
		s.Services["approval-service"] = Service{Image: "ghcr.io/acme/approval:1.2", Ports: []Port{{Name: "http", Port: 8080}}}
	}
	creds := func(user, password EnvValue) *ImageCredentials {
		return &ImageCredentials{Username: user, Password: password}
	}
	fs := validateMutated(t, defaultVC(ModePlan), nil, func(s *Spec) {
		approval(s)
		svc(s, "approval-service", func(v *Service) {
			v.ImageCredentials = creds(String("bot"), SecretEnv("internal-token"))
		})
		svc(s, "web", func(v *Service) { v.ImageCredentials = creds(String("a"), String("b")) })
		s.Jobs["missing"] = Job{Image: "img", ImageCredentials: creds(String("bot"), SecretEnv("missing-token"))}
		s.Jobs["empty"] = Job{Image: "img", ImageCredentials: creds(EnvValue{}, String("x"))}
		s.Jobs["addon"] = Job{Image: "img", ImageCredentials: creds(String("bot"), EnvValue{Kind: EnvAddon, Addon: "nats",
			Key: "url"})}
	})
	checkFindings(t, fs, []expect{
		e("jobs.addon.imageCredentials.password", "a string or a secret"),
		e("jobs.empty.imageCredentials.username", "username is empty"),
		e("jobs.missing.imageCredentials.password", "no secret missing-token"),
		e("services.web.imageCredentials", "pulls no image"),
	})

	var s Spec
	s.Services = map[string]Service{"app": {Image: "img", ImageCredentials: creds(String("bot"), SecretEnv("pull-token"))}}
	s.Jobs = map[string]Job{"migrate": {Image: "img", ImageCredentials: creds(SecretEnv("pull-user"), String("x"))}}
	if got := Referenced(s); !slices.Equal(got, []string{"pull-token", "pull-user"}) {
		t.Fatalf("referenced %v", got)
	}

	prev, next := release7(t), release7(t)
	svc(&next, "expense-service", func(v *Service) { v.ImageCredentials = creds(String("bot"), String("x")) })
	d := Diff(&prev, &next)
	if len(d) != 1 || !strings.HasPrefix(d[0].Effect, "replaces") || d[0].Fields[0].Path != "imageCredentials" {
		t.Fatalf("diff %s", diffJSON(t, d))
	}
}
