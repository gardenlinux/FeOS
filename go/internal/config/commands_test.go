// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"

	"github.com/gardenlinux/feos/go/internal/model"
)

// join-token's --kind: node by default, ingress for an instance of
// ferun-ingress (with or without a pinned name), from the flag or its
// variable; anything else is refused.
func TestJoinTokenKind(t *testing.T) {
	t.Parallel()
	vars := map[string]string{"FERUN_DATABASE_URL": testURL}
	jt, err := ParseJoinToken(nil, env(vars))
	if err != nil || jt.Kind != model.JoinKindNode {
		t.Fatalf("default %+v %v", jt, err)
	}
	jt, err = ParseJoinToken([]string{"--kind", "ingress", "--node", "ferun-ingress-0"}, env(vars))
	if err != nil || jt.Kind != model.JoinKindIngress || jt.Node != "ferun-ingress-0" {
		t.Fatalf("ingress %+v %v", jt, err)
	}
	jt, err = ParseJoinToken([]string{"--kind=node"}, env(map[string]string{"FERUN_DATABASE_URL": testURL, "FERUN_KIND": "ingress"}))
	if err != nil || jt.Kind != model.JoinKindNode {
		t.Fatalf("the flag over its variable %+v %v", jt, err)
	}
	jt, err = ParseJoinToken(nil, env(map[string]string{"FERUN_DATABASE_URL": testURL, "FERUN_KIND": "ingress"}))
	if err != nil || jt.Kind != model.JoinKindIngress || jt.Node != "" {
		t.Fatalf("the variable %+v %v", jt, err)
	}
	for _, kind := range []string{"", "pod", "Ingress", "nodes"} {
		if _, err := ParseJoinToken([]string{"--kind", kind}, env(vars)); err == nil || !strings.Contains(err.Error(), "--kind") {
			t.Errorf("--kind %q: %v", kind, err)
		}
	}
	if _, err := ParseJoinToken([]string{"--kind", "ingress", "--node", "Bad_Name"}, env(vars)); err == nil {
		t.Error("a bad ingress name parsed")
	}
	var b strings.Builder
	Usage(&b, CmdJoinToken)
	if !strings.Contains(b.String(), `--kind (FERUN_KIND, default "node")`) {
		t.Errorf("help %s", b.String())
	}
}

// service-token's --kind: sink by default, auth for ferun-auth (ADR
// 0054), from the flag or its variable; anything else is refused, naming
// both; the help names both too.
func TestServiceTokenKind(t *testing.T) {
	t.Parallel()
	vars := map[string]string{"FERUN_DATABASE_URL": testURL}
	st, err := ParseServiceToken([]string{"--name", "sink-0"}, env(vars))
	if err != nil || st.Kind != model.ServiceSink || st.Name != "sink-0" {
		t.Fatalf("default %+v %v", st, err)
	}
	st, err = ParseServiceToken([]string{"--kind", "auth", "--name", "ferun-auth"}, env(vars))
	if err != nil || st.Kind != model.ServiceAuth || st.Name != "ferun-auth" {
		t.Fatalf("auth %+v %v", st, err)
	}
	st, err = ParseServiceToken([]string{"--name", "ferun-auth"},
		env(map[string]string{"FERUN_DATABASE_URL": testURL, "FERUN_KIND": "auth"}))
	if err != nil || st.Kind != model.ServiceAuth {
		t.Fatalf("from its variable %+v %v", st, err)
	}
	for _, kind := range []string{"other", "node", "", "Auth"} {
		_, err := ParseServiceToken([]string{"--kind", kind, "--name", "a"}, env(vars))
		if err == nil || !strings.Contains(err.Error(), `--kind "`+kind+`": sink or auth`) {
			t.Errorf("--kind %q: %v", kind, err)
		}
	}
	var b strings.Builder
	Usage(&b, CmdServiceToken)
	for _, want := range []string{"--kind sink|auth", `--kind (FERUN_KIND, default "sink")`,
		"sink (an instance of ferun-sink), or auth (ferun-auth, ADR 0054)", "feos-auth-<secret>"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("help lacks %q:\n%s", want, b.String())
		}
	}
	b.Reset()
	Usage(&b, "")
	if !strings.Contains(b.String(), "service-token  issue a service token for an instance of ferun-sink or ferun-auth") {
		t.Errorf("overview %s", b.String())
	}
}
