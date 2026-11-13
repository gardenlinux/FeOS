// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	containerv1 "github.com/gardenlinux/feos/go/gen/feos/container/v1"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/testnode"
)

// attachedTLSSpec is a postgres and a valkey addon with two attached
// services in sandbox: web references postgres' role svc, worker (with
// worker) valkey; one pod, so their addresses are numbered together.
func attachedTLSSpec(sandbox podid.ID, worker bool) string {
	w := ""
	if worker {
		w = fmt.Sprintf(`,"worker":{"runtime":"attached","sandbox":%q,"command":["sh","-c","exec sleep 60"],"cwd":"/app",
			"env":{"CACHE":{"addon":"cache","key":"url"}}}`, sandbox)
	}
	return fmt.Sprintf(`{"addons":{"postgres":{"type":"postgres","version":"17","plan":"dev",
		"options":{"databases":["app"],"roles":["svc"]}},"cache":{"type":"valkey","version":"9","plan":"dev"}},
		"services":{"web":{"runtime":"attached","sandbox":%q,"command":["sh","-c","echo DB=$DB; exec sleep 60"],
			"cwd":"/app","env":{"DB":{"addon":"postgres","key":"url","role":"svc","database":"app"}}}%s}}`, sandbox, w)
}

// An attached service whose reference reaches its addon through a TLS
// binding (ADR 0036) runs in a sandbox made before it: the sandbox gets
// the binding before the process starts, in its template (for its pod's
// later creates) and in its running pod; the addresses are numbered over
// every attached service of the sandbox; the addon admits the sandbox's
// workload with the role (ADR 0038).
func TestAttachedTLSBindings(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed; the process scripts need it")
	}
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	execs := &execLog{}
	root := t.TempDir()
	var sbID podid.ID
	n1.Script().SetExec(func(ctx context.Context, pod podid.ID, req *containerv1.ExecRequest, out testnode.ExecOutput) (int32, bool, error) {
		if pod != sbID {
			return execs.fn(ctx, pod, req, out)
		}
		argv := req.Command
		if len(argv) > 2 && argv[0] == "python3" {
			start := strings.Contains(argv[2], "no such working directory")
			argv[2] = strings.ReplaceAll(argv[2], processRoot, root)
			if start && len(argv) > 6 && argv[6] != "" {
				argv[6] = filepath.Join(n1.PodDir(pod), argv[6])
			}
		}
		return 0, false, nil
	})
	sbID = podid.Mint()
	sb := h.sandboxAs(testTenant, sbID)
	if err := os.MkdirAll(filepath.Join(n1.PodDir(sb.ID), "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := h.pod("n1", sb.ID).GetSpec().GetTlsBindings(); len(got) != 0 {
		t.Fatalf("the sandbox's pod has TLS bindings before any service: %v", got)
	}
	env := h.envOf(testTenant, "dev")

	r1 := h.apply(env.ID, attachedTLSSpec(sb.ID, false))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 90*time.Second)
	var lines []string
	eventually(t, 10*time.Second, "web's log", func() bool {
		var err error
		lines, _, err = h.c.LogTail(bg, env.ID, spec.KindService, "web", 0, 10)
		return err == nil && len(lines) == 1
	})
	if lines[0] != "DB=postgres://svc@127.64.0.1:5432/app?sslmode=disable" {
		t.Errorf("web's env: %q", lines)
	}
	pg := pki.WorkloadID(testTD, env.Owner, env.ID, spec.KindAddon, "postgres")
	got := h.pod("n1", sb.ID).GetSpec().GetTlsBindings()
	if len(got) != 1 || got[0].GetListen() != "127.64.0.1:5432" || got[0].GetHost() != "postgres" ||
		got[0].GetPort() != 5432 || got[0].GetAlpn() != "postgresql" || got[0].GetServerId() != pg {
		t.Fatalf("the sandbox's pod's TLS bindings: %v", got)
	}
	row, err := h.st.GetSandbox(bg, sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []model.TemplateTLSBinding{{Listen: "127.64.0.1:5432", Host: "postgres", Port: 5432, ALPN: "postgresql",
		Addon: "postgres"}}
	if row.PodTemplate == nil || !slices.Equal(row.PodTemplate.TLSBindings, want) ||
		!slices.Equal(row.PodTemplate.Command, sb.PodTemplate.Command) ||
		!slices.Equal(row.PodTemplate.SecurityGroups, sb.PodTemplate.SecurityGroups) {
		t.Fatalf("the sandbox's template: %+v", row.PodTemplate)
	}
	// The addon admits the sandbox's workload as svc.
	own := model.SandboxWorkloadGroupID(sb.ID.String())
	eventually(t, 20*time.Second, "the sandbox's grant", func() bool {
		s := execs.step("tls")
		return len(s) > 0 && strings.Contains(s[len(s)-1].Env["FERUN_PG_IDENT"], `OU=`+own+`$"  svc`)
	})

	// A second attached service in the sandbox: valkey's name sorts first,
	// so it takes 127.64.0.1, postgres moves, and web starts anew.
	r2 := h.apply(env.ID, attachedTLSSpec(sb.ID, true))
	h.waitDeployment(env.ID, r2, model.DeploymentDeployed, 90*time.Second)
	eventually(t, 20*time.Second, "both bindings in the pod", func() bool {
		got := h.pod("n1", sb.ID).GetSpec().GetTlsBindings()
		return len(got) == 2 && got[0].GetListen() == "127.64.0.1:6379" && got[0].GetHost() == "cache" &&
			got[1].GetListen() == "127.64.0.2:5432" && got[1].GetHost() == "postgres"
	})
	eventually(t, 20*time.Second, "web's new env", func() bool {
		lines, _, err := h.c.LogTail(bg, env.ID, spec.KindService, "web", 0, 10)
		return err == nil && slices.Contains(lines, "DB=postgres://svc@127.64.0.2:5432/app?sslmode=disable")
	})
}
