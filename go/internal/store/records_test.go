// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

func TestNodeStartsAndGaps(t *testing.T) {
	s, _ := newStore(t)
	mkNode(t, s, "n1")
	start := uuid.New()
	var ns store.NodeStart
	tx(t, s, func(tx *store.Store) (err error) {
		ns, err = tx.LockNodeStart(bg, "n1", start)
		return err
	})
	if ns.LastSeq != 0 || ns.Dropped != 0 || !ns.LastEventAt.IsZero() {
		t.Fatalf("new start %+v", ns)
	}
	tx(t, s, func(tx *store.Store) error {
		if _, err := tx.LockNodeStart(bg, "n1", start); err != nil {
			return err
		}
		if err := tx.InsertEventGap(bg, "n1", start, 3, 4); err != nil {
			return err
		}
		return tx.AdvanceNodeStart(bg, "n1", start, 10, 2)
	})
	// dropped never goes back.
	must(t, s.AdvanceNodeStart(bg, "n1", start, 11, 1))
	tx(t, s, func(tx *store.Store) (err error) {
		ns, err = tx.LockNodeStart(bg, "n1", start)
		return err
	})
	if ns.LastSeq != 11 || ns.Dropped != 2 || ns.LastEventAt.IsZero() {
		t.Fatalf("advanced start %+v", ns)
	}
	must(t, s.InsertEventGap(bg, "n1", start, 3, 4)) // a repeat is kept as it was
	other := uuid.New()
	must(t, s.InsertEventGap(bg, "n1", other, 1, 1))
	now, _ := s.Now(bg)
	gaps, err := s.EventGaps(bg, []store.NodeStartKey{{Node: "n1", StartID: start}}, now.Add(-time.Minute), now.Add(time.Minute))
	if err != nil || len(gaps) != 1 || gaps[0].FromSeq != 3 || gaps[0].ToSeq != 4 {
		t.Fatalf("gaps %+v %v", gaps, err)
	}
	if gaps, _ := s.EventGaps(bg, []store.NodeStartKey{{Node: "n1", StartID: start}}, now.Add(time.Minute), now.Add(time.Hour)); len(gaps) != 0 {
		t.Fatalf("gaps outside the range %+v", gaps)
	}
	if gaps, _ := s.EventGaps(bg, nil, now, now); gaps == nil || len(gaps) != 0 {
		t.Fatal("gaps of no starts")
	}

	// Retention keeps the node's current start.
	_, err = s.SetNodeStart(bg, "n1", start)
	must(t, err)
	tx(t, s, func(tx *store.Store) error { _, err := tx.LockNodeStart(bg, "n1", other); return err })
	exec(t, s, "UPDATE node_starts SET first_seen = now() - interval '40 days', last_event_at = now() - interval '40 days'")
	if n, err := s.DeleteOldNodeStarts(bg, 30*24*time.Hour); err != nil || n != 1 {
		t.Fatalf("old starts %d %v", n, err)
	}
	exec(t, s, "UPDATE event_gaps SET detected_at = now() - interval '40 days'")
	if n, err := s.DeleteOldEventGaps(bg, 30*24*time.Hour); err != nil || n != 2 {
		t.Fatalf("old gaps %d %v", n, err)
	}
}

func i64(n int64) *int64 { return &n }

func TestActivityRows(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	sb := mkSandbox(t, s, "acme")
	start := uuid.New()
	var rows []store.ActivityRow
	for i := range 600 {
		rows = append(rows, store.ActivityRow{TsNs: int64(1000 + i), SandboxID: sb.ID, TenantID: "acme",
			Source: store.SourceFerund, Kind: "tool_start", Category: "tool", SpanID: "s1", Name: "bash: ls"})
	}
	rows = append(rows,
		store.ActivityRow{TsNs: 5000, SandboxID: sb.ID, TenantID: "acme", Node: "n1", StartID: start, Seq: 1,
			Source: store.SourceFeosd, Kind: "pod_state", Detail: json.RawMessage(`{"state":"RUNNING"}`)},
		store.ActivityRow{TsNs: 5001, PodID: podid.Mint(), ForeignPod: true, Node: "n1", StartID: start, Seq: 2,
			Source: store.SourceFeosd, Kind: "pod_state"},
		store.ActivityRow{TsNs: 5002, Node: "n1", Source: store.SourceFeosd, Kind: "node_started"},
		store.ActivityRow{TsNs: 5003, SandboxID: sb.ID, Source: store.SourceFerund, Kind: "tool_end", SpanID: "s1",
			Params: json.RawMessage(`{"command":"ls"}`), ExitCode: i64(0), DurationNs: i64(42), Output: "out", OutputTruncated: true},
	)
	must(t, s.InsertActivity(bg, rows...))
	// The same node event again is skipped, not an error.
	must(t, s.InsertActivity(bg, rows[600]))

	page, err := s.ListSandboxActivity(bg, sb.ID, store.ActivityQuery{Limit: 3})
	must(t, err)
	if len(page) != 3 || page[0].TsNs != 1599 || page[1].TsNs != 5000 || page[2].TsNs != 5003 {
		t.Fatalf("newest page %+v", page)
	}
	last := page[2]
	if last.ExitCode == nil || *last.ExitCode != 0 || *last.DurationNs != 42 || !last.OutputTruncated ||
		string(last.Params) != `{"command": "ls"}` || string(last.Detail) != "{}" || last.Day.IsZero() || last.ID == 0 {
		t.Fatalf("tool_end row %+v", last)
	}
	if page[1].Seq != 1 || page[1].StartID != start || string(page[1].Detail) != `{"state": "RUNNING"}` {
		t.Fatalf("node row %+v", page[1])
	}
	before, err := s.ListSandboxActivity(bg, sb.ID, store.ActivityQuery{Limit: 2, BeforeTsNs: 1002})
	if err != nil || len(before) != 2 || before[0].TsNs != 1000 || before[1].TsNs != 1001 {
		t.Fatalf("page before %+v %v", before, err)
	}
	feosd, err := s.ListSandboxActivity(bg, sb.ID, store.ActivityQuery{Limit: 100, Source: store.SourceFeosd})
	if err != nil || len(feosd) != 1 {
		t.Fatalf("feosd rows %+v %v", feosd, err)
	}
	all, _ := s.ListSandboxActivity(bg, sb.ID, store.ActivityQuery{Limit: 1000})
	if len(all) != 602 {
		t.Fatalf("all rows: %d", len(all))
	}
	nodeRows, err := s.ListNodeActivity(bg, "n1", store.ActivityQuery{Limit: 100})
	if err != nil || len(nodeRows) != 2 || !nodeRows[0].ForeignPod || nodeRows[1].Kind != "node_started" {
		t.Fatalf("node rows %+v %v", nodeRows, err)
	}
	span, err := s.ListSpanActivity(bg, "s1")
	if err != nil || len(span) != 601 {
		t.Fatalf("span rows %d %v", len(span), err)
	}
	// Activity outlives its sandbox.
	_, err = s.DeleteSandbox(bg, sb.ID)
	must(t, err)
	if rows, _ := s.ListSandboxActivity(bg, sb.ID, store.ActivityQuery{Limit: 10}); len(rows) != 10 {
		t.Fatal("activity went with the sandbox")
	}
}

func TestTrafficRows(t *testing.T) {
	s, _ := newStore(t)
	sb := legacyID()
	start := uuid.New()
	must(t, s.InsertTraffic(bg,
		store.TrafficRow{TsNs: 2, Node: "n1", StartID: start, Seq: 2, PodID: sb, SandboxID: sb, TenantID: "acme", Kind: "dns",
			Detail: json.RawMessage(`{"dns":{"name":"example.com"}}`)},
		store.TrafficRow{TsNs: 1, Node: "n1", StartID: start, Seq: 1, PodID: sb, SandboxID: sb, Kind: "flow", Missed: 3},
		store.TrafficRow{TsNs: 3, Node: "n1", StartID: start, Seq: 3, PodID: podid.Mint(), ForeignPod: true, Kind: "drop"},
	))
	must(t, s.InsertTraffic(bg, store.TrafficRow{TsNs: 1, Node: "n1", StartID: start, Seq: 1, Kind: "flow"}))
	rows, err := s.ListSandboxTraffic(bg, sb, store.ActivityQuery{Limit: 10})
	if err != nil || len(rows) != 2 || rows[0].Kind != "flow" || rows[0].Missed != 3 || rows[1].TenantID != "acme" ||
		string(rows[1].Detail) != `{"dns": {"name": "example.com"}}` {
		t.Fatalf("traffic %+v %v", rows, err)
	}
	if rows, _ := s.ListSandboxTraffic(bg, sb, store.ActivityQuery{Limit: 10, BeforeTsNs: 2}); len(rows) != 1 {
		t.Fatalf("traffic before %+v", rows)
	}
	if err := s.InsertTraffic(bg, store.TrafficRow{TsNs: 1, Node: "n1", StartID: start, Seq: 9, Kind: "bogus"}); err == nil {
		t.Fatal("an unknown traffic kind was stored")
	}

	// A node's traffic is the foreign records it sent: never another
	// node's, never attributed ones.
	foreign := podid.Mint()
	must(t, s.InsertTraffic(bg,
		store.TrafficRow{TsNs: 4, Node: "n1", StartID: start, Seq: 4, PodID: foreign, ForeignPod: true, Kind: "flow",
			Detail: json.RawMessage(`{"flow":{}}`)},
		store.TrafficRow{TsNs: 5, Node: "n2", StartID: uuid.New(), Seq: 1, PodID: foreign, ForeignPod: true, Kind: "dns"},
	))
	rows, err = s.ListNodeTraffic(bg, "n1", store.ActivityQuery{Limit: 10})
	if err != nil || len(rows) != 2 || rows[0].TsNs != 3 || rows[1].TsNs != 4 || !rows[0].ForeignPod ||
		rows[1].PodID != foreign || !rows[1].SandboxID.IsZero() || string(rows[1].Detail) != `{"flow": {}}` {
		t.Fatalf("node traffic %+v %v", rows, err)
	}
	if rows, err := s.ListNodeTraffic(bg, "n1", store.ActivityQuery{Limit: 1}); err != nil || len(rows) != 1 || rows[0].TsNs != 4 {
		t.Fatalf("newest node traffic %+v %v", rows, err)
	}
	if rows, err := s.ListNodeTraffic(bg, "n1", store.ActivityQuery{Limit: 10, BeforeTsNs: 4}); err != nil || len(rows) != 1 || rows[0].TsNs != 3 {
		t.Fatalf("node traffic before %+v %v", rows, err)
	}
}

// The inserts of activity and traffic records have one statement text for
// every batch size: a connection prepares each once, however the batches
// vary.
func TestInsertStatementShape(t *testing.T) {
	_, d := newStore(t)
	conn, err := d.Pool.Acquire(bg)
	must(t, err)
	defer conn.Release()
	s := store.On(conn)
	start := uuid.New()
	seq := int64(0)
	for _, n := range []int{1, 2, 3, 7, 501} {
		var act []store.ActivityRow
		var traffic []store.TrafficRow
		for range n {
			seq++
			act = append(act, store.ActivityRow{TsNs: seq, Source: store.SourceFerund, Kind: "k"})
			traffic = append(traffic, store.TrafficRow{TsNs: seq, Node: "n1", StartID: start, Seq: seq, Kind: "flow"})
		}
		must(t, s.InsertActivity(bg, act...))
		must(t, s.InsertTraffic(bg, traffic...))
	}
	for table, want := range map[string]int64{"activity": 1, "traffic_records": 1} {
		var prepared int64
		must(t, conn.QueryRow(bg, "SELECT count(*) FROM pg_prepared_statements WHERE statement LIKE $1",
			"INSERT INTO "+table+" %").Scan(&prepared))
		if prepared != want {
			t.Errorf("%d statements prepared for inserts into %s, want %d", prepared, table, want)
		}
	}
	var stored int64
	must(t, conn.QueryRow(bg, "SELECT (SELECT count(*) FROM activity) + (SELECT count(*) FROM traffic_records)").Scan(&stored))
	if stored != 2*seq {
		t.Fatalf("%d rows stored, want %d", stored, 2*seq)
	}
}

func TestIssuedCerts(t *testing.T) {
	s, _ := newStore(t)
	pod := podid.Mint()
	for i, serial := range []string{"01", "02", "03"} {
		c, err := s.InsertIssuedCert(bg, store.IssuedCert{Serial: serial, PodID: pod, Incarnation: uuid.New(), Node: "n1",
			SPIFFEID: "spiffe://feos.local/tenant/acme/sandbox/" + pod.String(), HPKEPublicKey: []byte{byte(i)},
			BindingsVersion: "v", NotAfter: time.Now().Add(time.Hour)})
		if err != nil || c.IssuedAt.IsZero() {
			t.Fatalf("insert %+v %v", c, err)
		}
	}
	if n, err := s.CountIssuedCerts(bg, pod, time.Minute); err != nil || n != 3 {
		t.Fatalf("count %d %v", n, err)
	}
	exec(t, s, "UPDATE issued_certs SET issued_at = now() - interval '2 minutes' WHERE serial = '01'")
	if n, _ := s.CountIssuedCerts(bg, pod, time.Minute); n != 2 {
		t.Fatalf("count within a minute %d", n)
	}
	if c, err := s.LatestIssuedCert(bg, pod); err != nil || c.Serial == "01" {
		t.Fatalf("latest %+v %v", c, err)
	}
	if _, err := s.LatestIssuedCert(bg, podid.Mint()); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("latest of none: %v", err)
	}
	if l, err := s.ListIssuedCerts(bg, pod); err != nil || len(l) != 3 || l[2].Serial != "01" {
		t.Fatalf("list %+v %v", l, err)
	}
	// The newest of each pod that has one: of the same time, by serial.
	other, none := podid.Mint(), podid.Mint()
	if _, err := s.InsertIssuedCert(bg, store.IssuedCert{Serial: "0a", PodID: other, Incarnation: uuid.New(), Node: "n2",
		SPIFFEID: "spiffe://feos.local/tenant/acme/env/x/service/api", HPKEPublicKey: []byte{9},
		NotAfter: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	exec(t, s, "UPDATE issued_certs SET issued_at = now() WHERE pod_id = $1", pod.String())
	latest, err := s.LatestIssuedCerts(bg, []podid.ID{pod, other, none})
	if err != nil || len(latest) != 2 || latest[pod].Serial != "01" || latest[other].Serial != "0a" ||
		latest[other].Node != "n2" || latest[other].NotAfter.IsZero() {
		t.Fatalf("latest of each %+v %v", latest, err)
	}
	if latest, err := s.LatestIssuedCerts(bg, nil); err != nil || len(latest) != 0 {
		t.Fatalf("latest of none %+v %v", latest, err)
	}
	exec(t, s, "UPDATE issued_certs SET not_after = now() - interval '100 days' WHERE serial = '01'")
	if n, err := s.DeleteOldIssuedCerts(bg, 90*24*time.Hour); err != nil || n != 1 {
		t.Fatalf("old %d %v", n, err)
	}
}

func TestWatches(t *testing.T) {
	s, d := newStore(t)
	mkTenant(t, s, "acme")
	sb := mkSandbox(t, s, "acme")
	ch, unsubscribe := d.Subscribe(db.ChanWatch)
	defer unsubscribe()
	id := uuid.New()
	w, err := s.CreateWatch(bg, store.NewWatch{ID: id, SandboxID: sb.ID, Paths: []string{"/a"}}, "i1", 2*time.Minute, 30*time.Second)
	if err != nil || w.Container != "main" || w.Gen != 1 || w.Epoch != 1 || w.Owner != "i1" || !w.OwnerLive || w.Result != nil {
		t.Fatalf("create %+v %v", w, err)
	}
	if _, err := s.CreateWatch(bg, store.NewWatch{ID: uuid.New(), SandboxID: podid.Mint()}, "i1", time.Minute, time.Minute); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("watch of a missing sandbox: %v", err)
	}
	// A PUT on another instance: new paths, gen + 1, the owner told.
	w2, err := s.SetWatchPaths(bg, id, []string{"/a", "/b"}, 2*time.Minute)
	if err != nil || w2.Gen != 2 || len(w2.Paths) != 2 {
		t.Fatalf("paths %+v %v", w2, err)
	}
	expectPayload(t, ch, id.String())
	// The owner acks generation 2.
	if ok, err := s.SetWatchResult(bg, id, "i1", 1, 2, json.RawMessage(`{"watching":["/a","/b"]}`)); err != nil || !ok {
		t.Fatalf("result %v %v", ok, err)
	}
	expectPayload(t, ch, id.String())
	got, _ := s.GetWatch(bg, id)
	if got.ResultGen != 2 || len(got.Result) == 0 {
		t.Fatalf("after result %+v", got)
	}
	// Another instance takes it over; the old owner's acks are refused.
	w3, err := s.TakeWatchOwnership(bg, id, "i2", 2*time.Minute, 30*time.Second)
	if err != nil || w3.Owner != "i2" || w3.Epoch != 2 {
		t.Fatalf("take %+v %v", w3, err)
	}
	expectPayload(t, ch, id.String())
	if ok, _ := s.SetWatchResult(bg, id, "i1", 1, 2, json.RawMessage(`{}`)); ok {
		t.Fatal("an old owner's ack was stored")
	}
	if ok, _ := s.SetWatchResult(bg, id, "i2", 2, 1, json.RawMessage(`{}`)); ok {
		t.Fatal("an ack of an older generation was stored")
	}
	if n, err := s.RenewWatchOwnership(bg, "i2", 30*time.Second); err != nil || n != 1 {
		t.Fatalf("renew owner %d %v", n, err)
	}
	if ok, err := s.RenewWatchLease(bg, id, 2*time.Minute); err != nil || !ok {
		t.Fatalf("renew lease %v %v", ok, err)
	}
	if l, _ := s.ListWatchesOwnedBy(bg, "i2"); len(l) != 1 {
		t.Fatal("owned by i2")
	}
	if l, _ := s.ListWatchesOfSandbox(bg, sb.ID); len(l) != 1 {
		t.Fatal("of sandbox")
	}
	if n, _ := s.CountWatches(bg, sb.ID); n != 1 {
		t.Fatal("count")
	}
	// Expiry.
	other := uuid.New()
	_, err = s.CreateWatch(bg, store.NewWatch{ID: other, SandboxID: sb.ID}, "i1", time.Minute, time.Minute)
	must(t, err)
	exec(t, s, "UPDATE watches SET expires_at = now() - interval '1 second' WHERE id = $1", other)
	ids, err := s.DeleteExpiredWatches(bg, 10)
	if err != nil || len(ids) != 1 || ids[0] != other {
		t.Fatalf("expired %v %v", ids, err)
	}
	expectPayload(t, ch, other.String())
	if ok, err := s.DeleteWatch(bg, id); err != nil || !ok {
		t.Fatalf("delete %v %v", ok, err)
	}
	expectPayload(t, ch, id.String())
	if _, err := s.GetWatch(bg, id); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("deleted watch: %v", err)
	}
	if _, err := s.SetWatchPaths(bg, id, nil, time.Minute); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("paths of a deleted watch: %v", err)
	}
}

func TestBindings(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	sb := mkSandbox(t, s, "acme")
	v, kube, err := s.KubeBindings(bg, sb.ID)
	if err != nil || v != "" || kube == nil || len(kube) != 0 {
		t.Fatalf("empty kube set %q %v %v", v, kube, err)
	}
	set := []store.KubeBinding{
		{Name: "prod", Port: 6443, Server: "https://k8s", AuthType: "token", Sealed: []byte{1, 2}, Fingerprint: "sha256:aa"},
		{Name: "dev", Port: 6000, Server: "https://dev", CAPEM: "pem", AuthType: "client_cert", Sealed: []byte{3}, Fingerprint: "sha256:bb"},
	}
	tx(t, s, func(tx *store.Store) error {
		if _, err := tx.LockSandbox(bg, sb.ID, store.ForUpdate); err != nil {
			return err
		}
		return tx.SetKubeBindings(bg, sb.ID, "v1", set)
	})
	v, kube, err = s.KubeBindings(bg, sb.ID)
	if err != nil || v != "v1" || len(kube) != 2 || kube[0].Name != "dev" || kube[0].CAPEM != "pem" || string(kube[1].Sealed) != "\x01\x02" {
		t.Fatalf("kube set %q %+v %v", v, kube, err)
	}
	if ports, err := s.KubePorts(bg, sb.ID); err != nil || len(ports) != 2 || ports[0] != 6000 {
		t.Fatalf("ports %v %v", ports, err)
	}
	if get(t, s, sb.ID).Gen != 0 {
		t.Fatal("a binding write bumped gen")
	}
	if _, _, err := s.KubeBindings(bg, podid.Mint()); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("kube of a missing sandbox: %v", err)
	}
	if err := s.SetKubeBindings(bg, podid.Mint(), "v", nil); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("set kube of a missing sandbox: %v", err)
	}
	// Delete is the empty set.
	must(t, s.SetKubeBindings(bg, sb.ID, "", nil))
	if v, kube, _ := s.KubeBindings(bg, sb.ID); v != "" || len(kube) != 0 {
		t.Fatalf("after delete %q %v", v, kube)
	}

	sshSet := []store.SSHBinding{{Name: "git", Server: "ssh://git.example.com", User: "git", HostKeys: []string{"ssh-ed25519 AAAA"},
		AuthType: "generate", KeyType: "ssh-ed25519", Fingerprint: "SHA256:x", PublicKey: "ssh-ed25519 BBBB", Sealed: []byte{9}},
		{Name: "any", User: "me", AuthType: "private_key", KeyType: "ssh-rsa", Fingerprint: "SHA256:y", PublicKey: "ssh-rsa C", Sealed: []byte{8}}}
	must(t, s.SetSSHBindings(bg, sb.ID, "s1", sshSet))
	v, ssh, err := s.SSHBindings(bg, sb.ID)
	if err != nil || v != "s1" || len(ssh) != 2 || ssh[0].Name != "any" || len(ssh[0].HostKeys) != 0 || ssh[1].HostKeys[0] != "ssh-ed25519 AAAA" {
		t.Fatalf("ssh set %q %+v %v", v, ssh, err)
	}
	if _, _, err := s.SSHBindings(bg, podid.Mint()); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("ssh of a missing sandbox: %v", err)
	}
	// The bindings go with the sandbox.
	_, err = s.DeleteSandbox(bg, sb.ID)
	must(t, err)
	var n int
	must(t, s.Q().QueryRow(bg, "SELECT (SELECT count(*) FROM kube_bindings) + (SELECT count(*) FROM ssh_bindings)").Scan(&n))
	if n != 0 {
		t.Fatalf("%d bindings outlived their sandbox", n)
	}
}

func TestClusterSecret(t *testing.T) {
	s, _ := newStore(t)
	v, err := s.ClusterSecret(bg, db.SecretMCPSession)
	if err != nil || len(v) != 32 {
		t.Fatalf("secret %x %v", v, err)
	}
	if _, err := s.ClusterSecret(bg, "nope"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("missing secret: %v", err)
	}
}
