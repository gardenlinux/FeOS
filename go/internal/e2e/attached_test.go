// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	containerv1 "github.com/gardenlinux/feos/go/gen/feos/container/v1"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/testnode"
)

// FeCode's replaceLostSandbox over REST: the workspace sandbox of a
// preview's attached service is deleted and created again under the same
// ID. The service comes back in the new incarnation (its restarts reset,
// the new incarnation recorded, a new process), and the route door
// reaches the sandbox again (§13.6).
func TestAttachedReplacedSandbox(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed; the process scripts need it")
	}
	c := newCluster(t, nil)
	a := c.start("a")
	n1 := c.join("n1", func(cfg *testnode.Config) { cfg.CheckSecurityGroups = true })
	root := t.TempDir()
	// Each pod's process records in a directory of its own, as each
	// container has its own disk (the test node runs the commands on the
	// host).
	n1.Script().SetExec(func(_ context.Context, pod podid.ID, req *containerv1.ExecRequest, _ testnode.ExecOutput) (int32, bool, error) {
		if argv := req.Command; len(argv) > 2 && argv[0] == "python3" {
			argv[2] = strings.ReplaceAll(argv[2], processRoot, filepath.Join(root, pod.String()))
		}
		return 0, false, nil
	})
	app := newGuestApp(t)
	n1.Script().SetPortAddr(5173, app.srv.Listener.Addr().String())

	r := c.call(a, "POST", "/api/v1/sandboxes", tenantAuth, `{"attachable":true}`)
	if r.code != http.StatusCreated {
		t.Fatalf("create the workspace sandbox: %d %s", r.code, r.body)
	}
	var created struct {
		Status struct {
			SandboxID string `json:"sandboxId"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(r.body), &created); err != nil {
		t.Fatal(err)
	}
	id, err := podid.Parse(created.Status.SandboxID)
	if err != nil || id.IsLegacy() {
		t.Fatalf("the workspace sandbox's ID %q: %v", created.Status.SandboxID, err)
	}
	env, err := c.st.InsertEnvironment(bg, store.NewEnvironment{ID: uuid.New(), Owner: tenant, Name: "preview"})
	if err != nil {
		t.Fatal(err)
	}
	r1 := c.applyRelease(env.ID, fmt.Sprintf(`{"services":{
		"web":{"runtime":"attached","sandbox":%q,"command":["sleep","120"],"ports":[{"name":"http","port":5173}]}},
		"routes":{"root":{"path":"/","service":"web","port":"http"}}}`, id))
	c.waitRelease(env.ID, r1, model.ReleaseDeployed, 60*time.Second)
	first, err := c.st.GetAttached(bg, env.ID, "web")
	if err != nil || first.ProcessID == "" {
		t.Fatalf("attached %+v %v", first, err)
	}

	r = c.call(a, "POST", "/api/v1/environments/preview/route-tokens", tenantAuth, "")
	var tok struct {
		Token string `json:"token"`
	}
	if r.code/100 != 2 || json.Unmarshal([]byte(r.body), &tok) != nil || tok.Token == "" {
		t.Fatalf("route token: %d %s", r.code, r.body)
	}
	route := func() (int, string) {
		t.Helper()
		req, err := http.NewRequest("GET", a.url("/api/v1/environments/preview/http/src/main.ts?x=1"), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+tok.Token)
		resp, err := plainClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	const want = "path=/src/main.ts query=x=1"
	if code, body := route(); code != http.StatusOK || body != want {
		t.Fatalf("the route door: %d %q", code, body)
	}

	// Lost and replaced under the same ID (no client can since ADR 0019;
	// a lost sandbox comes back as a new incarnation under its ID too).
	if r := c.call(a, "DELETE", "/api/v1/sandboxes/"+id.String(), tenantAuth, ""); r.code != http.StatusOK {
		t.Fatalf("delete: %d %s", r.code, r.body)
	}
	c.createID(id, true)
	again := c.row(id)
	if again.Incarnation == first.SandboxIncarnation {
		t.Fatal("the same incarnation")
	}
	var b store.EnvAttached
	eventually(t, 60*time.Second, "the service back in the new incarnation", func() bool {
		b, err = c.st.GetAttached(bg, env.ID, "web")
		return err == nil && b.SandboxIncarnation == again.Incarnation && b.ProcessID != "" && b.Phase == model.PhaseReady
	})
	if b.Restarts != 0 || b.ProbeFailures != 0 || !b.NextRestartAt.IsZero() {
		t.Errorf("not reset: %+v", b)
	}
	if code, body := route(); code != http.StatusOK || body != want {
		t.Fatalf("the route door after the replacement: %d %q", code, body)
	}
}
