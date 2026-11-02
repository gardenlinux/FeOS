// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	containerv1 "github.com/gardenlinux/feos/go/gen/feos/container/v1"
	networkv1 "github.com/gardenlinux/feos/go/gen/feos/network/v1"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/env/ctl"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/testnode"
)

// applyRelease inserts the next release of an environment with spec, as
// the environment service's apply does (its API is another lane's), and
// wakes the controller. It returns the release's number.
func (c *cluster) applyRelease(envID uuid.UUID, spec string) int {
	c.t.Helper()
	var n int
	err := c.st.InTx(bg, func(tx *store.Store) error {
		e, err := tx.LockEnvironment(bg, envID, store.ForUpdate)
		if err != nil {
			return err
		}
		n = e.Release + 1
		if _, err := tx.InsertRelease(bg, store.EnvRelease{EnvID: envID, Number: n, Spec: json.RawMessage(spec),
			Base: e.Release, Origin: model.OriginApply}); err != nil {
			return err
		}
		after := e
		after.Release = n
		if _, err := tx.UpdateEnvironment(bg, e, after); err != nil {
			return err
		}
		if _, err := tx.ReplaceReleases(bg, envID, n, model.ReleaseWaiting, model.ReleaseDeploying); err != nil {
			return err
		}
		return tx.Notify(bg, db.ChanLeader, ctl.HintReconcile)
	})
	if err != nil {
		c.t.Fatal(err)
	}
	return n
}

// waitRelease waits until a release of an environment is in state.
func (c *cluster) waitRelease(envID uuid.UUID, n int, state string, within time.Duration) {
	c.t.Helper()
	eventually(c.t, within, fmt.Sprintf("release %d %s", n, state), func() bool {
		r, err := c.st.GetRelease(bg, envID, n)
		return err == nil && r.State == state
	})
}

// nodeTable is a name table of a test node; nil when it has none.
func nodeTable(n *node, id string) *networkv1.NameTable {
	for _, t := range n.NameTables() {
		if t.Id == id {
			return t
		}
	}
	return nil
}

// resolves reports whether the pod resolves name on its test node, as
// feosd's resolver would, to exactly addrs (none: not a name of the pod's).
func resolves(n *node, pod podid.ID, name string, addrs ...string) bool {
	got, ok := n.Resolve(pod, name)
	return ok == (len(addrs) > 0) && slices.Equal(got, addrs)
}

// nodeGroup is a security group of a test node; nil when it has none.
func nodeGroup(n *node, id string) *networkv1.SecurityGroup {
	for _, g := range n.SecurityGroups() {
		if g.Id == id {
			return g
		}
	}
	return nil
}

// The environment controller in a running ferund, on a node that checks
// security groups and name tables: an attachable sandbox made over REST
// gets its group and its name table before its pod; an environment's image
// service and an attached service in that sandbox come up and the release
// is deployed; the env group has both as members and the sandbox's group
// reaches it; the node's resolver answers each pod the names it should;
// the controller reads the process's log; a deleted environment leaves
// its sandbox alone and nothing of its own.
func TestEnvironments(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed; the process scripts need it")
	}
	c := newCluster(t, nil)
	a := c.start("a")
	n1 := c.join("n1", func(cfg *testnode.Config) { cfg.CheckSecurityGroups, cfg.CheckNameTables = true, true })
	root := t.TempDir()
	// The process scripts' records go to a directory of the test, and an
	// attached service's working directory beneath the pod's directory:
	// the test node runs the commands on the host.
	n1.Script().SetExec(func(_ context.Context, pod podid.ID, req *containerv1.ExecRequest, _ testnode.ExecOutput) (int32, bool, error) {
		if argv := req.Command; len(argv) > 2 && argv[0] == "python3" {
			start := strings.Contains(argv[2], "no such working directory")
			argv[2] = strings.ReplaceAll(argv[2], processRoot, root)
			if start && len(argv) > 6 && argv[6] != "" {
				argv[6] = filepath.Join(n1.PodDir(pod), argv[6])
			}
		}
		return 0, false, nil
	})

	r := c.call(a, "POST", "/api/v1/sandboxes", tenantAuth, `{"attachable":true}`)
	if r.code != http.StatusCreated {
		t.Fatalf("create an attachable sandbox: %d %s", r.code, r.body)
	}
	var created struct {
		Token  string `json:"token"`
		Status struct {
			SandboxID  string `json:"sandboxId"`
			Attachable bool   `json:"attachable"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(r.body), &created); err != nil || !created.Status.Attachable {
		t.Fatalf("create: %s (%v)", r.body, err)
	}
	sb := sandbox{id: podid.ID(created.Status.SandboxID), token: created.Token}
	c.run(a, sb, "mkdir -p app")
	if g := nodeGroup(n1, model.SandboxGroupID(sb.id)); g == nil {
		t.Fatal("the attachable sandbox runs without its group on the node")
	}
	if nt := nodeTable(n1, model.SandboxTableID(sb.id)); nt == nil {
		t.Fatal("the attachable sandbox runs without its name table on the node")
	}
	if err := os.MkdirAll(filepath.Join(n1.PodDir(sb.id), "app"), 0o755); err != nil {
		t.Fatal(err)
	}

	env, err := c.st.InsertEnvironment(bg, store.NewEnvironment{ID: uuid.New(), Owner: tenant, Name: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	r1 := c.applyRelease(env.ID, fmt.Sprintf(`{"services":{
		"api":{"runtime":"image","image":%q,"ports":[{"name":"http","port":8080}]},
		"web":{"runtime":"attached","sandbox":%q,"command":["sh","-c","echo up $API; exec sleep 60"],"cwd":"/app",
			"env":{"API":{"service":"api","port":"http","key":"url"}},"ports":[{"name":"http","port":5173}]}}}`,
		testImage, sb.id))
	c.waitRelease(env.ID, r1, model.ReleaseDeployed, 60*time.Second)

	rows, err := c.st.ListEnvInstanceRows(bg, env.ID)
	if err != nil || len(rows) != 1 || rows[0].Instance.Object != "api" || rows[0].Instance.Phase != model.PhaseReady {
		t.Fatalf("instances %+v %v", rows, err)
	}
	api := rows[0].Sandbox
	host := c.row(sb.id)
	eventually(t, 30*time.Second, "the groups", func() bool {
		eg := nodeGroup(n1, model.EnvGroupID(env.ID))
		sg := nodeGroup(n1, model.SandboxGroupID(sb.id))
		return eg != nil && sg != nil && slices.Contains(eg.Members, api.Address) && slices.Contains(eg.Members, host.Address) &&
			slices.ContainsFunc(sg.Rules, func(r *networkv1.Rule) bool { return r.GetGroup() == model.EnvGroupID(env.ID) })
	})
	eventually(t, 10*time.Second, "the names", func() bool {
		return resolves(n1, sb.id, "api", api.Address) && resolves(n1, sb.id, "web", "127.0.0.1", "::1") &&
			resolves(n1, api.ID, "api", api.Address) && resolves(n1, api.ID, "web")
	})
	var lines []string
	eventually(t, 10*time.Second, "the process's log", func() bool {
		lines, _, err = a.Env.Controller().LogTail(bg, env.ID, "service", "web", 0, 5)
		return err == nil && len(lines) == 1
	})
	if lines[0] != "up http://api:8080" {
		t.Errorf("log %q", lines)
	}
	if _, _, err := a.Env.Controller().LogTail(bg, env.ID, "service", "nope", 0, 5); !errors.Is(err, ctl.ErrNoObject) {
		t.Errorf("logs of an unknown object: %v", err)
	}

	// Deleted, the environment takes its instance, process, names, group
	// and name table along, and leaves the sandbox running.
	c.sql(`UPDATE environments SET deleting = true WHERE id = $1`, env.ID)
	c.wake(ctl.LoopDelete)
	eventually(t, 60*time.Second, "the environment gone", func() bool {
		_, err := c.st.GetEnvironment(bg, env.ID)
		return errors.Is(err, store.ErrNoRow)
	})
	if p, ok := n1.pod(api.ID); ok {
		t.Errorf("the instance's pod stays: %v", p.State)
	}
	if nodeGroup(n1, model.EnvGroupID(env.ID)) != nil {
		t.Error("the env group stays")
	}
	if nodeTable(n1, model.EnvTableID(env.ID)) != nil {
		t.Error("the env table stays")
	}
	if resolves(n1, sb.id, "api", api.Address) || resolves(n1, sb.id, "web", "127.0.0.1", "::1") {
		t.Error("the sandbox resolves the deleted environment's names")
	}
	if out := c.run(a, sb, "echo alive"); strings.TrimSpace(out) != "alive" {
		t.Errorf("the sandbox after the deletion: %q", out)
	}
}
