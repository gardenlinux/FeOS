// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	networkv1 "github.com/gardenlinux/feos/go/gen/feos/network/v1"
	"github.com/gardenlinux/feos/go/internal/env/addons"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/sandbox"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The egress table mirrors feosd's internet sets: the internet of either
// family, never ::/0 or a bare 0.0.0.0/0, each exception inside its block.
func TestEgressTable(t *testing.T) {
	rules := egressTable()
	if len(rules) != 2 {
		t.Fatalf("%d rules", len(rules))
	}
	want := map[string][]string{
		"0.0.0.0/0": {"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
			"192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24",
			"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4"},
		// 5f00::/16 (SRv6) is outside 2000::/3: no exception of it.
		"2000::/3": {"2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20"},
	}
	for _, r := range rules {
		if r.Direction != networkv1.Direction_DIRECTION_EGRESS || r.Protocol != networkv1.Protocol_PROTOCOL_ANY || len(r.Ports) != 0 {
			t.Errorf("rule %v", r)
		}
		b := r.GetBlock()
		if b == nil {
			t.Fatalf("rule %v has no block", r)
		}
		if b.Cidr == "::/0" || (b.Cidr == "0.0.0.0/0" && len(b.Except) == 0) {
			t.Errorf("a rule to everything: %v", b)
		}
		if !slices.Equal(b.Except, want[b.Cidr]) {
			t.Errorf("%s except %v, want %v", b.Cidr, b.Except, want[b.Cidr])
		}
		cidr := netip.MustParsePrefix(b.Cidr)
		for _, e := range b.Except {
			ex := netip.MustParsePrefix(e)
			if ex.Masked() != ex || ex.Addr().Is4() != cidr.Addr().Is4() || ex.Bits() < cidr.Bits() || !cidr.Contains(ex.Addr()) {
				t.Errorf("exception %s is not a prefix inside %s", e, cidr)
			}
		}
		// The table answers what it is for.
		for _, tc := range []struct {
			addr string
			out  bool
		}{{"8.8.8.8", true}, {"10.1.2.3", false}, {"192.168.1.1", false}, {"169.254.169.254", false},
			{"2a00:1450::1", true}, {"fd00::1", false}, {"2001:db8::1", false}, {"::1", false}, {"5f00::1", false}} {
			a := netip.MustParseAddr(tc.addr)
			if a.Is4() != cidr.Addr().Is4() {
				continue
			}
			in := cidr.Contains(a)
			for _, e := range b.Except {
				if netip.MustParsePrefix(e).Contains(a) {
					in = false
				}
			}
			if in != tc.out {
				t.Errorf("%s: allowed %v, want %v", tc.addr, in, tc.out)
			}
		}
	}
	// Each call gives its own copy.
	rules[0].GetBlock().Except[0] = "x"
	if egressTable()[0].GetBlock().Except[0] != "0.0.0.0/8" {
		t.Error("the table is shared")
	}
}

// The env group lets its members reach each other; the sbx group reaches
// the env groups it is attached to and takes nothing in.
func TestGroupRules(t *testing.T) {
	env := model.EnvGroupID(uuid.MustParse("0193e3a0-0000-7000-8000-000000000001"))
	rules := envRules(env)
	if len(rules) != 4 || rules[2].GetGroup() != env || rules[2].Direction != networkv1.Direction_DIRECTION_INGRESS ||
		rules[3].GetGroup() != env || rules[3].Direction != networkv1.Direction_DIRECTION_EGRESS {
		t.Fatalf("env rules %v", rules)
	}
	sbx := sbxRules([]string{env, "env-other"})
	if len(sbx) != 4 {
		t.Fatalf("sbx rules %v", sbx)
	}
	for _, r := range sbx {
		if r.Direction == networkv1.Direction_DIRECTION_INGRESS {
			t.Errorf("an sbx group lets in %v", r)
		}
	}
	short := podid.Mint()
	legacy := podid.ID(uuid.NewString())
	if len(env) != 40 || len(model.SandboxGroupID(short)) != 15 || len(model.SandboxGroupID(legacy)) != 40 ||
		len(model.IngressGroupID(short)) != 15 || len(model.IngressGroupID(legacy)) != 40 {
		t.Error("group IDs are not 40 characters, an sbx or ing group of a short ID not 15")
	}
}

func TestMembersAndVersions(t *testing.T) {
	got := members([]string{"fd00::0005", "", "10.0.0.1", "fd00::3", "fd00::5", "::ffff:10.0.0.1", "not"})
	if !slices.Equal(got, []string{"fd00::3", "fd00::5"}) {
		t.Fatalf("members %v", got)
	}
	a := &networkv1.SecurityGroup{Id: "sbx-x", Rules: sbxRules(nil), Members: []string{"fd00::5", "fd00::3"}}
	b := &networkv1.SecurityGroup{Id: "sbx-x", Rules: sbxRules(nil), Members: []string{"fd00::3", "fd00::0005"}, Version: 9}
	if !sameContent(a, b) || hashVersion(a) != hashVersion(b) {
		t.Error("equal groups differ")
	}
	c := &networkv1.SecurityGroup{Id: "sbx-x", Rules: sbxRules([]string{"env-y"}), Members: a.Members}
	if sameContent(a, c) || hashVersion(a) == hashVersion(c) {
		t.Error("groups with other rules compare equal")
	}
	d := &networkv1.SecurityGroup{Id: "sbx-x", Rules: sbxRules(nil), Members: []string{"fd00::3"}}
	if sameContent(a, d) || hashVersion(a) == hashVersion(d) {
		t.Error("groups with other members compare equal")
	}
	if hashVersion(a) >= 1<<63 {
		t.Error("a version beyond int64")
	}
}

// The env table names addons and image services by the addresses of
// their live replicas (stopping ones never; when none is live, those of
// the best ranked); its version is its content's, and a delta between two
// tables turns one into the other.
func TestNameTable(t *testing.T) {
	envID := uuid.New()
	row := func(kind, name string, replica int, phase, addr string) store.EnvInstanceRow {
		return store.EnvInstanceRow{
			Instance: store.EnvInstance{SandboxID: podid.Mint(), Kind: kind, Object: name, Replica: replica, Phase: phase},
			Sandbox:  store.Sandbox{Address: addr, ClientState: model.StateRunning, Node: "n1"},
		}
	}
	v := &view{
		env: store.Environment{ID: envID, Name: "stage"},
		target: &spec.Spec{
			Services: map[string]spec.Service{
				"api": {Image: "img"}, "web": {Runtime: spec.RuntimeAttached, Sandbox: "s"}, "worker": {Image: "img"},
			},
			Addons: map[string]spec.Addon{"postgres": {Type: "postgres"}},
		},
		rows: []store.EnvInstanceRow{
			row(spec.KindService, "api", 0, model.PhaseStarting, "fd00::10"),
			row(spec.KindService, "api", 1, model.PhaseReady, "fd00::11"),
			row(spec.KindService, "api", 2, model.PhaseStopping, "fd00::12"),
			row(spec.KindService, "api", 4, model.PhaseReady, "fd00:0::13"),
			row(spec.KindService, "worker", 0, model.PhaseStopping, "fd00::20"),
			row(spec.KindAddon, "postgres", 0, model.PhaseReady, "fd00::5"),
			row(spec.KindAddon, "gone", 0, model.PhaseReady, "fd00::6"),
			row(spec.KindJob, "migrate", 0, model.PhaseStarting, "fd00::7"),
			row(spec.KindService, "api", 3, model.PhaseReady, ""),
			row(spec.KindService, "api", 5, model.PhaseReady, "10.0.0.1"),
		},
	}
	got := nameEntries(v)
	want := map[string][]string{"api": {"fd00::11", "fd00::13"}, "postgres": {"fd00::5"}}
	if !maps.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Fatalf("entries %v, want %v", got, want)
	}
	// No live replica: the others, never a stopping one.
	v.rows[1].Instance.Phase, v.rows[3].Instance.Phase = model.PhaseUnready, model.PhaseUnready
	if got := nameEntries(v)["api"]; !slices.Equal(got, []string{"fd00::10", "fd00::11", "fd00::13"}) {
		t.Errorf("api without a live replica %v", got)
	}

	id := model.EnvTableID(envID)
	a := newTable(id, want, nil)
	if a.Version == 0 || a.Version >= 1<<63 || len(a.Entries) != 2 || a.Entries[0].Name != "api" {
		t.Fatalf("table %v", a)
	}
	same := newTable(id, map[string][]string{"postgres": {"fd00:0:0::5"}, "API": {"fd00::13", "fd00::11"}}, nil)
	if !sameTable(a, same) || a.Version != same.Version {
		t.Errorf("the same names in another order or case: %v %v", a, same)
	}
	b := newTable(id, map[string][]string{"api": {"fd00::11"}, "redis": {"fd00::7"}}, nil)
	if sameTable(a, b) || a.Version == b.Version {
		t.Error("other names compare equal")
	}
	if c := newTable(id, want, []string{"env-x"}); sameTable(a, c) || a.Version == c.Version {
		t.Error("other includes compare equal")
	}
	put, remove := tableDelta(a, b)
	if len(put) != 2 || put[0].Name != "api" || !slices.Equal(put[0].Addresses, []string{"fd00::11"}) ||
		put[1].Name != "redis" || !slices.Equal(remove, []string{"postgres"}) {
		t.Errorf("delta %v %v", put, remove)
	}
	if put, remove := tableDelta(a, same); len(put) != 0 || len(remove) != 0 {
		t.Errorf("a delta between equal tables %v %v", put, remove)
	}
	if got := addressSet([]string{"::1", "127.0.0.1", "::ffff:127.0.0.1", "x", "fe80::1%eth0"}); !slices.Equal(got,
		[]string{"127.0.0.1", "::1"}) {
		t.Errorf("addresses %v", got)
	}
}

// Readiness counts live rows only (§13.7): a ready phase left from a pod
// whose row is unknown, stopped, failed or has an op in flight is not
// ready, for the names and the crash loop's end alike; a paused
// or hibernated replica of an idle service is.
func TestLiveReadiness(t *testing.T) {
	row := func(replica int, phase, state, addr string) store.EnvInstanceRow {
		id := podid.Mint()
		return store.EnvInstanceRow{
			Instance: store.EnvInstance{SandboxID: id, Kind: spec.KindService, Object: "web", Replica: replica, Phase: phase},
			Sandbox:  store.Sandbox{ID: id, ClientState: state, Node: "n1", Address: addr},
		}
	}
	for _, tc := range []struct {
		state, op string
		idles     bool
		want      bool
	}{
		{model.StateRunning, "", false, true},
		{model.StateRunning, "pause", false, false},
		{model.StateUnknown, "", false, false},
		{model.StateStopped, "", false, false},
		{model.StateFailed, "", false, false},
		{model.StateLost, "", false, false},
		{model.StatePending, "", false, false},
		{model.StatePaused, "", false, false},
		{model.StatePaused, "", true, true},
		{model.StateHibernated, "", true, true},
		{model.StatePaused, "resume", true, false},
		{model.StateUnknown, "", true, false},
	} {
		r := row(0, model.PhaseReady, tc.state, "fd00::1")
		r.Sandbox.Op = tc.op
		if got := live(r, tc.idles); got != tc.want {
			t.Errorf("%s op %q idles %v: live %v", tc.state, tc.op, tc.idles, got)
		}
		r.Instance.Phase = model.PhaseUnready
		if live(r, tc.idles) {
			t.Errorf("%s: an unready phase is live", tc.state)
		}
	}

	// web's replica 0 was ready when its node went down (unknown, its
	// address kept); replica 1 runs: the name is replica 1's.
	v := &view{
		env:    store.Environment{ID: uuid.New(), Name: "stage"},
		target: &spec.Spec{Services: map[string]spec.Service{"web": {Image: "img"}}},
		rows: []store.EnvInstanceRow{
			row(0, model.PhaseReady, model.StateUnknown, "fd00::10"),
			row(1, model.PhaseReady, model.StateRunning, "fd00::11"),
		},
	}
	if got := nameEntries(v)["web"]; !slices.Equal(got, []string{"fd00::11"}) {
		t.Errorf("web -> %s, want the running replica's fd00::11", got)
	}
	// Replica 1 is still starting: its address is still better than one
	// that may be stale.
	v.rows[1].Instance.Phase = model.PhaseStarting
	if got := nameEntries(v)["web"]; !slices.Equal(got, []string{"fd00::11"}) {
		t.Errorf("web -> %s, want the known replica's fd00::11", got)
	}
	// Only the unknown one has an address: it is named, nothing better.
	v.rows[1].Sandbox.Address = ""
	if got := nameEntries(v)["web"]; !slices.Equal(got, []string{"fd00::10"}) {
		t.Errorf("web -> %q, want the only address fd00::10", got)
	}
	// An idle service's paused ready replica is ready.
	v.target.Services["web"] = spec.Service{Image: "img", Idle: "15m"}
	v.rows[0].Sandbox.ClientState = model.StatePaused
	v.rows[1].Sandbox.Address = "fd00::11"
	if got := nameEntries(v)["web"]; !slices.Equal(got, []string{"fd00::10"}) {
		t.Errorf("web -> %s, want the idle service's paused ready replica fd00::10", got)
	}

	// The crash loop ends only once every instance has been ready for the
	// crash window, each one live.
	c := &Controller{t: defaultTimings()}
	v.target.Services["web"] = spec.Service{Image: "img"}
	v.now = time.Now()
	long := v.now.Add(-2 * c.t.crashWindow)
	for i := range v.rows {
		v.rows[i].Instance.Phase, v.rows[i].Instance.ReadySince = model.PhaseReady, long
		v.rows[i].Sandbox.ClientState = model.StateRunning
	}
	probed := v.rows[1].Instance
	if !c.objectSteady(v, spec.KindService, "web", probed) {
		t.Fatal("not steady with every replica ready and running")
	}
	v.rows[0].Sandbox.ClientState = model.StateUnknown
	if c.objectSteady(v, spec.KindService, "web", probed) {
		t.Error("steady while a replica is unknown")
	}
	v.rows[0].Sandbox.ClientState = model.StatePaused
	if c.objectSteady(v, spec.KindService, "web", probed) {
		t.Error("steady while a replica of a service that does not idle is paused")
	}
	v.target.Services["web"] = spec.Service{Image: "img", Idle: "15m"}
	if !c.objectSteady(v, spec.KindService, "web", probed) {
		t.Error("not steady while an idle service's ready replica is paused")
	}
}

func TestRedactAndEscape(t *testing.T) {
	pw := "p@ss:w/rd%?#&=+ x"
	if got := escapeUserinfo(pw); got != "p%40ss%3Aw%2Frd%25%3F%23&=+%20x" {
		t.Errorf("escape %q", got)
	}
	u, err := url.Parse("postgres://user:" + escapeUserinfo(pw) + "@[fd00::5]:5432/db")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := u.User.Password(); got != pw {
		t.Errorf("the password reads back as %q", got)
	}
	secret := "0123456789abcdef0123456789abcdef"
	out := redact("ERROR: password "+secret+" and "+escapeUserinfo(pw)+" and "+pw, []string{secret, pw, "ab"})
	if strings.Contains(out, secret) || strings.Contains(out, pw) || strings.Contains(out, escapeUserinfo(pw)) {
		t.Errorf("redacted %q", out)
	}
	if strings.Count(out, redactedMark) != 3 {
		t.Errorf("redacted %q", out)
	}
	if redact("ab cd", []string{"ab"}) != "ab cd" {
		t.Error("a short value was redacted everywhere")
	}
	if v, err := newCredential(); err != nil || len(v) != 32 || strings.Trim(v, "0123456789abcdef") != "" {
		t.Errorf("credential %q, %v", v, err)
	}
}

func TestDefaults(t *testing.T) {
	two := 2
	if replicasOf(spec.Service{}) != 1 || replicasOf(spec.Service{Replicas: &two}) != 2 {
		t.Error("replicas")
	}
	if v, m := resourcesOf(nil); v != 1 || m != 512 {
		t.Error("resources")
	}
	if v, m := resourcesOf(&spec.Resources{MemoryMib: 1024}); v != 1 || m != 1024 {
		t.Error("resources with memory")
	}
	if rolloutOf(spec.Service{}) != 10*time.Minute || rolloutOf(spec.Service{Rollout: &spec.Rollout{Timeout: "30s"}}) != 30*time.Second {
		t.Error("rollout")
	}
	if jobTimeout(spec.Job{}) != 10*time.Minute || jobTimeout(spec.Job{Timeout: "5h"}) != 2*time.Hour {
		t.Error("job timeout")
	}
	if jobOn(spec.Job{}) != spec.JobOnDeploy || jobOn(spec.Job{On: spec.JobOnRelease}) != spec.JobOnDeploy || jobOn(spec.Job{On: spec.JobOnManual}) != spec.JobOnManual {
		t.Error("job on")
	}
	if idleOf(spec.Service{}) != 0 || idleOf(spec.Service{Idle: "15m"}) != 900 {
		t.Error("idle")
	}
	if runtimeOf(spec.Service{Image: "x"}) != spec.RuntimeImage || runtimeOf(spec.Service{Sandbox: "s"}) != spec.RuntimeAttached {
		t.Error("runtime")
	}
	svc := spec.Service{Ports: []spec.Port{{Name: "http", Port: 8080}}, Readiness: &spec.Readiness{
		HTTP: &spec.HTTPCheck{Port: spec.PortRef{Name: "http"}, Path: "/readyz"}, InitialDelay: "1s"}}
	if c := checkOf(svc); c.kind != "http" || c.port != 8080 || c.path != "/readyz" || c.delay != time.Second ||
		c.period != 2*time.Second || c.threshold != 3 {
		t.Errorf("check %+v", c)
	}
	if c := checkOf(spec.Service{}); c.kind != "" || c.period != 2*time.Second {
		t.Errorf("no check %+v", c)
	}
	if portOf(svc, spec.PortRef{Number: 8080}) != 8080 || portOf(svc, spec.PortRef{Name: "grpc"}) != 0 {
		t.Error("ports")
	}
	ctl := &Controller{t: defaultTimings()}
	for n, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 9: 256 * time.Second,
		10: 5 * time.Minute, 50: 5 * time.Minute} {
		if got := ctl.backoff(n); got != want {
			t.Errorf("backoff(%d) %v, want %v", n, got, want)
		}
	}
}

// A hook's error for a node that cannot take the pod wraps
// sandbox.ErrNodeUnfit beside its cause, and reads as its cause.
func TestUnfit(t *testing.T) {
	cause := errors.New("volume v is attached to pod p on node n1")
	err := error(unfit{cause})
	if !errors.Is(err, sandbox.ErrNodeUnfit) || !errors.Is(err, cause) || err.Error() != cause.Error() {
		t.Errorf("unfit %v", err)
	}
}

func TestSpell(t *testing.T) {
	for d, want := range map[time.Duration]string{10 * time.Minute: "10 minutes", time.Minute: "1 minute",
		90 * time.Second: "1m30s", 2 * time.Second: "2s"} {
		if got := spell(d); got != want {
			t.Errorf("spell(%v) %q, want %q", d, got, want)
		}
	}
}

func TestApplyCheck(t *testing.T) {
	chk := check{threshold: 2}
	now := time.Now()
	phase, since, failures, detail := model.PhaseStarting, time.Time{}, 0, ""
	applyCheck(false, "refused", chk, now, &phase, &since, &failures, &detail)
	if phase != model.PhaseStarting || failures != 1 || detail != "refused" {
		t.Fatalf("a failure while starting: %s %d %q", phase, failures, detail)
	}
	applyCheck(true, "", chk, now, &phase, &since, &failures, &detail)
	if phase != model.PhaseReady || !since.Equal(now) || failures != 0 || detail != "" {
		t.Fatalf("the first success: %s %v %d", phase, since, failures)
	}
	applyCheck(false, "500", chk, now, &phase, &since, &failures, &detail)
	if phase != model.PhaseReady {
		t.Fatal("one failure made it unready")
	}
	applyCheck(false, "500", chk, now, &phase, &since, &failures, &detail)
	if phase != model.PhaseUnready || !since.IsZero() {
		t.Fatalf("threshold failures: %s", phase)
	}
	applyCheck(true, "", chk, now.Add(time.Second), &phase, &since, &failures, &detail)
	if phase != model.PhaseReady || !since.Equal(now.Add(time.Second)) || detail != "" {
		t.Fatalf("back: %s %q", phase, detail)
	}
	// A ready instance passing its checks keeps a detail the checks did
	// not write (a failed provisioning's).
	detail = "provisioning step provision exited 1"
	applyCheck(true, "", chk, now.Add(2*time.Second), &phase, &since, &failures, &detail)
	if phase != model.PhaseReady || detail != "provisioning step provision exited 1" {
		t.Fatalf("a passing check of a ready instance: %s %q", phase, detail)
	}
}

func TestFreeReplicas(t *testing.T) {
	rows := []store.EnvInstanceRow{{Instance: store.EnvInstance{Replica: 0}}, {Instance: store.EnvInstance{Replica: 2}}}
	if got := freeReplicas(rows, 3); !slices.Equal(got, []int{1, 3, 4}) {
		t.Errorf("free %v", got)
	}
	if got := freeReplicas(nil, 2); !slices.Equal(got, []int{0, 1}) {
		t.Errorf("free %v", got)
	}
}

// The pool runs a key once at a time, at most its workers at once, and
// every task with its context.
func TestPool(t *testing.T) {
	p := newPool(2, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var running, peak atomic.Int32
	release := make(chan struct{})
	var ran atomic.Int32
	task := func(context.Context) {
		n := running.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		<-release
		running.Add(-1)
		ran.Add(1)
	}
	if !p.run(ctx, "a", task) || p.run(ctx, "a", task) {
		t.Fatal("a key queued twice")
	}
	p.run(ctx, "b", task)
	p.run(ctx, "c", task)
	if !p.busy("c") {
		t.Error("a queued key is not busy")
	}
	time.Sleep(50 * time.Millisecond)
	if peak.Load() != 2 {
		t.Errorf("%d tasks at once", peak.Load())
	}
	close(release)
	p.wait()
	if ran.Load() != 3 || p.busy("a") {
		t.Errorf("%d tasks ran", ran.Load())
	}
	if !p.run(ctx, "a", task) {
		t.Error("a key did not run again after its task")
	}
	p.wait()
	cancel()
	if p.run(ctx, "z", task) {
		t.Error("a task was queued with an ended context")
	}
}

// fakeBlueprints is a registry of one fake addon type for render tests.
type fakeBP struct{}

func (fakeBP) Type() string       { return "fake" }
func (fakeBP) Versions() []string { return []string{"1"} }
func (fakeBP) Plans() map[string]addons.Plan {
	return map[string]addons.Plan{"small": {VCPUs: 1, MemoryMib: 256, VolumeMib: 1024}, "big": {VCPUs: 2, MemoryMib: 1024, VolumeMib: 4096}}
}
func (fakeBP) DefaultPlan() string                                 { return "small" }
func (fakeBP) Validate(*addons.Addon, addons.Addon) []spec.Finding { return nil }
func (fakeBP) Credentials(addons.Addon) []string                   { return []string{"password"} }
func (fakeBP) OutputKeys() []string                                { return []string{"url"} }
func (fakeBP) Render(a addons.Addon, _ addons.Context) (addons.Instance, error) {
	return addons.Instance{Image: "fake:" + a.Version, Command: []string{"serve", a.Plan},
		Env: map[string]model.TemplateValue{"PW": model.CredentialValue("password")}, Port: 1, Mount: "/data",
		Probe: []string{"check"}}, nil
}
func (fakeBP) Outputs(a addons.Addon, c addons.Context, ref addons.Ref) ([]model.TemplatePart, error) {
	return []model.TemplatePart{{Value: "fake://u:"}, {Credential: "password", Escape: model.EscapeURL},
		{Value: "@[" + c.Address + "]/" + ref.Fields["db"]}}, nil
}
func (fakeBP) Provision(a addons.Addon, c addons.Context) []addons.Step {
	if a.Options["db"] == nil {
		return nil
	}
	return []addons.Step{{Name: "db", Argv: []string{"mk", a.Options["db"].(string)},
		Env: map[string]model.TemplateValue{"PW": model.CredentialValue("password")}}}
}

// Rendering: credentials map to system secrets, an addon's address and a
// service's port resolve, the hash changes with what the pod runs and not
// with its labels, and a dependent waits for its addon's address.
func TestRender(t *testing.T) {
	c := &Controller{d: Deps{Blueprint: func(typ string) (addons.Blueprint, bool) {
		if typ == "fake" {
			return fakeBP{}, true
		}
		return nil, false
	}}}
	envID := uuid.New()
	vol := &store.EnvVolume{VolumeID: uuid.New()}
	v := &view{
		env: store.Environment{ID: envID, Name: "stage"},
		target: &spec.Spec{
			Services: map[string]spec.Service{
				"api": {Image: "img@sha256:1", Ports: []spec.Port{{Name: "http", Port: 8080}}, Env: map[string]spec.EnvValue{
					"DB":      {Kind: spec.EnvAddon, Addon: "db", Key: "url", Fields: map[string]string{"db": "app"}},
					"SELF":    {Kind: spec.EnvService, Service: "api", Port: spec.PortRef{Name: "http"}, Key: "url"},
					"HOST":    {Kind: spec.EnvService, Service: "api", Port: spec.PortRef{Name: "http"}, Key: "host"},
					"PORT":    {Kind: spec.EnvService, Service: "api", Port: spec.PortRef{Name: "http"}, Key: "port"},
					"TOKEN":   {Kind: spec.EnvSecret, Secret: "token"},
					"PLAIN":   {Value: "x"},
					"LITERAL": {Kind: spec.EnvLiteral, Value: "y"},
				}},
			},
			Addons:  map[string]spec.Addon{"db": {Type: "fake", Version: "1", Options: map[string]any{"db": "app"}}},
			Secrets: map[string]spec.SecretRef{"token": {Version: 3}},
		},
		secrets: map[string]int{"token": 4},
	}
	p := c.planService(v, "api", v.target.Services["api"])
	if p.ok() || !strings.Contains(p.wait, "addon db") {
		t.Fatalf("a dependent rendered without its addon's address: %+v", p)
	}
	v.rows = []store.EnvInstanceRow{{Instance: store.EnvInstance{Kind: spec.KindAddon, Object: "db", Phase: model.PhaseReady},
		Sandbox: store.Sandbox{Address: "fd00::5"}}}
	p = c.planService(v, "api", v.target.Services["api"])
	if !p.ok() {
		t.Fatal(p.why())
	}
	env := p.template.Env
	if got := env["DB"].Parts; len(got) != 3 || got[1].Secret != "addon.db.password" || got[1].Version != 1 ||
		got[1].Escape != model.EscapeURL || got[2].Value != "@[fd00::5]/app" {
		t.Errorf("DB %+v", got)
	}
	if env["SELF"].Value != "http://api:8080" || env["HOST"].Value != "api" || env["PORT"].Value != "8080" ||
		env["TOKEN"].Secret != "token" || env["TOKEN"].Version != 3 || env["PLAIN"].Value != "x" || env["LITERAL"].Value != "y" {
		t.Errorf("env %+v", env)
	}
	if p.template.HasCredential() || p.template.Labels[model.LabelTemplate] != p.hash ||
		p.template.Labels[model.LabelObject] != "service/api" || p.template.Labels[model.LabelEnvironment] != envID.String() ||
		!slices.Equal(p.template.SecurityGroups, []string{model.EnvGroupID(envID)}) {
		t.Errorf("template %+v", p.template)
	}
	if p.vcpus != 1 || p.memoryMB != 512 {
		t.Errorf("resources %d %d", p.vcpus, p.memoryMB)
	}
	again := c.planService(v, "api", v.target.Services["api"])
	if again.hash != p.hash {
		t.Error("the hash is not deterministic")
	}
	// A new address of the addon is a new template.
	v.rows[0].Sandbox.Address = "fd00::6"
	if moved := c.planService(v, "api", v.target.Services["api"]); moved.hash == p.hash {
		t.Error("the addon's address is not in the hash")
	}
	// The secret's version without a pin: the latest.
	delete(v.target.Secrets, "token")
	if latest := c.planService(v, "api", v.target.Services["api"]); latest.template.Env["TOKEN"].Version != 4 {
		t.Errorf("unpinned secret %+v", latest.template.Env["TOKEN"])
	}

	a := c.planAddon(v, "db", v.target.Addons["db"], nil, "fd00::6")
	if a.ok() {
		t.Fatal("an addon with a mount rendered without its volume")
	}
	a = c.planAddon(v, "db", v.target.Addons["db"], vol, "fd00::6")
	if !a.ok() {
		t.Fatal(a.why())
	}
	if a.image != "fake:1" || a.vcpus != 1 || a.memoryMB != 256 || a.addon.Plan != "small" ||
		len(a.template.Volumes) != 1 || a.template.Volumes[0].VolumeID != vol.VolumeID || a.template.Mounts[0].Path != "/data" ||
		a.template.Env["PW"].Parts[0].Secret != "addon.db.password" || a.check.kind != "exec" || len(a.steps) != 1 ||
		a.steps[0].Env["PW"].Parts[0].Secret != "addon.db.password" || a.stepsHash == "" {
		t.Errorf("addon plan %+v", a)
	}
	if a.template.HasCredential() {
		t.Error("an addon's template has a credential")
	}
	// Options that change provisioning only do not change the hash.
	opt := v.target.Addons["db"]
	opt.Options = map[string]any{"db": "other"}
	b := c.planAddon(v, "db", opt, vol, "fd00::6")
	if b.hash != a.hash || b.stepsHash == a.stepsHash {
		t.Error("a provisioning option recreates the addon, or does not reprovision it")
	}
	// A plan change does.
	big := v.target.Addons["db"]
	big.Plan = "big"
	if p := c.planAddon(v, "db", big, vol, "fd00::6"); p.hash == a.hash || p.memoryMB != 1024 {
		t.Error("a plan change kept the template")
	}
	if p := c.planAddon(v, "x", spec.Addon{Type: "nope"}, nil, ""); p.err == nil {
		t.Error("an unknown type rendered")
	}
	// The hashes of the templates ignore their labels.
	tmpl := a.template
	tmpl.Labels = map[string]string{"x": "y"}
	if tmpl.Hash(a.image, a.vcpus, a.memoryMB) != a.hash {
		t.Error("labels are in the hash")
	}

	att := c.planAttached(v, "web", spec.Service{Runtime: spec.RuntimeAttached, Sandbox: podid.Mint().String(),
		Command: []string{"npm", "run", "dev"}, Cwd: "/workspace/web"})
	if !att.ok() || att.hash == "" {
		t.Fatal(att.why())
	}
	// A sandbox made before ADR 0019, named by its UUID in either case,
	// renders the same.
	legacy := uuid.NewString()
	lower := c.planAttached(v, "web", spec.Service{Runtime: spec.RuntimeAttached, Sandbox: legacy,
		Command: []string{"npm", "run", "dev"}, Cwd: "/workspace/web"})
	upper := c.planAttached(v, "web", spec.Service{Runtime: spec.RuntimeAttached, Sandbox: strings.ToUpper(legacy),
		Command: []string{"npm", "run", "dev"}, Cwd: "/workspace/web"})
	if !lower.ok() || !upper.ok() || lower.sandbox.String() != legacy || upper.hash != lower.hash {
		t.Fatalf("legacy sandbox: %v %v %s", lower.why(), upper.why(), lower.sandbox)
	}
	// Its hash is the one the sandbox's service had when the ID was a
	// uuid.UUID: the service is not started anew by the change of type.
	if old := hashJSON(struct {
		Scheme  int                            `json:"scheme"`
		Sandbox uuid.UUID                      `json:"sandbox"`
		Command []string                       `json:"command"`
		Cwd     string                         `json:"cwd"`
		Env     map[string]model.TemplateValue `json:"env"`
	}{1, uuid.MustParse(legacy), lower.argv, lower.cwd, map[string]model.TemplateValue{}}); old != lower.hash {
		t.Errorf("the hash of a legacy sandbox's service changed: %s, was %s", lower.hash, old)
	}
	if bad := c.planAttached(v, "web", spec.Service{Runtime: spec.RuntimeAttached, Sandbox: "x", Command: []string{"a"}}); bad.ok() {
		t.Error("an attached service with a bad sandbox rendered")
	}
}

// The deployment spec reads from the store's JSON with every default
// applied where the spec leaves a field out.
func TestSpecFromJSON(t *testing.T) {
	raw := `{"services":{"web":{"image":"img","ports":[{"name":"http","port":80}],
		"readiness":{"tcp":{"port":"http"}}}},"jobs":{"m":{"image":"img"}},
		"addons":{"pg":{"type":"postgres","version":"17"}}}`
	var s spec.Spec
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatal(err)
	}
	web := s.Services["web"]
	if runtimeOf(web) != spec.RuntimeImage || replicasOf(web) != 1 || checkOf(web).kind != "tcp" || checkOf(web).port != 80 {
		t.Errorf("web %+v", web)
	}
	if jobOn(s.Jobs["m"]) != spec.JobOnDeploy || s.Jobs["m"].Retries != 0 {
		t.Error("job defaults")
	}
	bp, ok := addons.Get("postgres")
	if !ok {
		t.Skip("no postgres blueprint")
	}
	if a := specAddon(bp, "pg", s.Addons["pg"]); a.Plan != bp.DefaultPlan() {
		t.Errorf("plan %q", a.Plan)
	}
}
