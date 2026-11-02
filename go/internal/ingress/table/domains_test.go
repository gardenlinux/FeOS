// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package table

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/seal"
	"github.com/gardenlinux/feos/go/internal/store"
)

// domain is a domain with a chain and key of n bytes each.
func domain(name string, n int, fill byte) wire.Domain {
	return wire.Domain{Name: name, Chain: [][]byte{bytes.Repeat([]byte{fill}, n), {fill, 1}},
		Key: bytes.Repeat([]byte{fill}, n/10+1)}
}

// An environment's domains are its Hosts, in place of its default host,
// and come with their certificates; a name twice is taken once; a renewed
// certificate changes the hash.
func TestBuildEnvDomains(t *testing.T) {
	specs := specsOf(map[int]*spec.Spec{1: deployment1()})
	e := baseEnv(1, 1, ready("a0000000001", "web", 1, 8080))
	plain, _ := build(t, "apps.example", e, specs)
	if plain.Domains != nil {
		t.Fatalf("domains without any: %+v", plain.Domains)
	}
	e.Domains = []wire.Domain{domain("dashboard.fecore.dev", 100, 1), domain("www.fecore.dev", 100, 2),
		domain("dashboard.fecore.dev", 100, 3)}
	got, _ := build(t, "apps.example", e, specs)
	if !slices.Equal(got.Hosts, []string{"dashboard.fecore.dev", "www.fecore.dev"}) {
		t.Fatalf("hosts %v", got.Hosts)
	}
	if len(got.Domains) != 2 || got.Domains[0].Chain[0][0] != 1 || got.Domains[1].Name != "www.fecore.dev" {
		t.Fatalf("domains %+v", got.Domains)
	}
	if got.Hash == plain.Hash {
		t.Fatal("domains kept the hash")
	}
	if h, err := Hash(got); err != nil || h != got.Hash {
		t.Fatalf("hash %s, recomputed %s %v", got.Hash, h, err)
	}
	if !slices.Equal(plain.Hosts, []string{"web-acme.apps.example"}) {
		t.Fatalf("hosts without domains %v", plain.Hosts)
	}
	// Without a zone too.
	noZone, _ := build(t, "", e, specs)
	if !slices.Equal(noZone.Hosts, []string{"dashboard.fecore.dev", "www.fecore.dev"}) {
		t.Fatalf("hosts without a zone %v", noZone.Hosts)
	}
	// A renewed certificate: another hash.
	renewed := e
	renewed.Domains = slices.Clone(e.Domains)
	renewed.Domains[0] = domain("dashboard.fecore.dev", 100, 9)
	again, _ := build(t, "apps.example", renewed, specs)
	if again.Hash == got.Hash {
		t.Fatal("a renewed certificate kept the hash")
	}
}

// Domains never make an environment too large: those that do not fit
// within MaxEnvBytes, or alone take more than MaxDomainBytes, are left
// out, in their order, and the entry stays within its bound.
func TestBuildEnvDomainsBounded(t *testing.T) {
	specs := specsOf(map[int]*spec.Spec{1: deployment1()})
	e := baseEnv(1, 1, ready("a0000000001", "web", 1, 8080))
	e.Domains = []wire.Domain{domain("huge.example", MaxDomainBytes, 7)}
	for i := range 60 {
		e.Domains = append(e.Domains, domain(fmt.Sprintf("d%02d.example", i), 40<<10, byte(i)))
	}
	got, _ := build(t, "apps.example", e, specs)
	n, err := wire.Size(got)
	if err != nil {
		t.Fatal(err)
	}
	if n > MaxEnvBytes {
		t.Fatalf("%d bytes", n)
	}
	if len(got.Domains) < 20 || len(got.Domains) >= 60 || got.Domains[0].Name != "d00.example" ||
		len(got.Hosts) != len(got.Domains) {
		t.Fatalf("%d domains, first %s, %d hosts", len(got.Domains), got.Domains[0].Name, len(got.Hosts))
	}
	for i, d := range got.Domains {
		if d.Name != fmt.Sprintf("d%02d.example", i) {
			t.Fatalf("domain %d is %s: not in order", i, d.Name)
		}
	}
	t.Logf("%d domains of 40 KiB: %d bytes", len(got.Domains), n)

	// An environment that takes nearly all of its bound keeps its routes
	// and leaves its domains out.
	big := routesSpec(spec.MaxRoutes, 15000)
	full, _ := build(t, "apps.example", baseEnv(1, 0), specsOf(map[int]*spec.Spec{1: big}))
	near := baseEnv(1, 0)
	near.Domains = []wire.Domain{domain("a.example", 200<<10, 1)}
	got, _ = build(t, "apps.example", near, specsOf(map[int]*spec.Spec{1: big}))
	if size, _ := wire.Size(full); size+(200<<10) <= MaxEnvBytes {
		t.Fatalf("the routes take %d bytes: not near the bound", size)
	}
	if got.Domains != nil || !slices.Equal(got.Hosts, []string{"web-acme.apps.example"}) || got.Hash != full.Hash {
		t.Fatalf("near the bound: %d domains, hosts %v", len(got.Domains), got.Hosts)
	}
}

// testSealer returns a sealer of a random key.
func testSealer(t *testing.T) *seal.Sealer {
	t.Helper()
	k := make([]byte, seal.KeySize)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	s, err := seal.New(k)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// mkDomain inserts a domain of acme bound to env with state, verified
// and routed, and with a certificate valid until notAfter (none when
// zero) whose chain is chain and key key, sealed with sl. It returns
// the domain's ID.
func (f *fixture) mkDomain(t *testing.T, env uuid.UUID, name, state string, notAfter time.Time, chain [][]byte,
	key []byte, sl *seal.Sealer) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := f.d.Pool.Exec(bg, `INSERT INTO domains (id, owner, name, token, env_id, bound_at, verification, verified_at,
			routed, was_routed, state)
		VALUES ($1, 'acme', $2, 'abcdefghijklmnopqrstuvwxyz234567', $3, now(), 'verified', now(), true, true, $4)`,
		id, name, env, state); err != nil {
		t.Fatal(err)
	}
	if !notAfter.IsZero() {
		f.putCert(t, id, notAfter, chain, key, sl)
	}
	return id
}

// putCert keeps a domain's certificate as ACME does, and notifies the
// table of its environment.
func (f *fixture) putCert(t *testing.T, id uuid.UUID, notAfter time.Time, chain [][]byte, key []byte, sl *seal.Sealer) {
	t.Helper()
	sealed, err := sl.Seal(key, store.DomainCertAAD(id))
	if err != nil {
		t.Fatal(err)
	}
	err = f.st.InTx(bg, func(tx *store.Store) error {
		d, err := tx.LockACMEDomain(bg, id)
		if err != nil {
			return err
		}
		return tx.ACMEOrderSucceeded(bg, d, store.DomainCert{DomainID: id, Chain: chain, KeySealed: sealed,
			NotBefore: notAfter.Add(-90 * 24 * time.Hour), NotAfter: notAfter, RenewAfter: notAfter.Add(-30 * 24 * time.Hour),
			Serial: "1"}, "certificate_failing")
	})
	if err != nil {
		t.Fatal(err)
	}
}

func envOf(s *Snapshot, id uuid.UUID) wire.Env { return s.Envs[id.String()] }

// The builder serves a domain that is served or dns-moved, verified,
// with a certificate still valid, its key opened; not one that is
// issuing, whose certificate expired, or whose key does not open; a
// renewal (ACME's write notifies the environment) reaches the snapshot.
// Without a sealer no domain is served. An environment's domains are its
// hosts; its default host is its host only while none is served.
func TestBuilderDomains(t *testing.T) {
	f := newFixture(t)
	sl := testSealer(t)
	ahead := time.Now().Add(60 * 24 * time.Hour)
	chain := [][]byte{[]byte("leaf-1"), []byte("ca")}
	served := f.mkDomain(t, f.env.ID, "dashboard.fecore.dev", "served", ahead, chain, []byte("key-1"), sl)
	f.mkDomain(t, f.env.ID, "moved.fecore.dev", "dns-moved", ahead, chain, []byte("key-2"), sl)
	f.mkDomain(t, f.env.ID, "issuing.fecore.dev", "issuing", time.Time{}, nil, nil, sl)
	f.mkDomain(t, f.env.ID, "expired.fecore.dev", "served", time.Now().Add(-time.Hour), chain, []byte("key-3"), sl)
	f.mkDomain(t, f.env.ID, "otherkey.fecore.dev", "served", ahead, chain, []byte("key-4"), testSealer(t))

	b := f.builder(t, func(b *Builder) { b.Sealer = sl })
	s, _ := use(t, b)
	e := envOf(s, f.env.ID)
	if !slices.Equal(e.Hosts, []string{"dashboard.fecore.dev", "moved.fecore.dev"}) {
		t.Fatalf("hosts %v", e.Hosts)
	}
	if len(e.Domains) != 2 || !bytes.Equal(e.Domains[0].Key, []byte("key-1")) || !slices.EqualFunc(e.Domains[0].Chain,
		chain, bytes.Equal) {
		t.Fatalf("domains %+v", e.Domains)
	}

	// A renewal: a new chain and key, in the next snapshot.
	f.putCert(t, served, ahead.Add(time.Hour), [][]byte{[]byte("leaf-2"), []byte("ca")}, []byte("key-5"), sl)
	await(t, b, time.Second, "the renewed certificate", func(s *Snapshot) bool {
		d := envOf(s, f.env.ID).Domains
		return len(d) == 2 && bytes.Equal(d[0].Key, []byte("key-5")) && bytes.Equal(d[0].Chain[0], []byte("leaf-2"))
	})

	// The core's state: issuing again (its certificate deleted, say).
	if _, err := f.d.Pool.Exec(bg, `UPDATE domains SET state = 'issuing' WHERE id = $1`, served); err != nil {
		t.Fatal(err)
	}
	if err := f.st.Notify(bg, "ferun_ingress", store.IngressEnvPayload(f.env.ID)); err != nil {
		t.Fatal(err)
	}
	await(t, b, time.Second, "the domain no longer served", func(s *Snapshot) bool {
		e := envOf(s, f.env.ID)
		return len(e.Domains) == 1 && e.Domains[0].Name == "moved.fecore.dev" && slices.Equal(e.Hosts, []string{"moved.fecore.dev"})
	})

	// None served: the default host again.
	if _, err := f.d.Pool.Exec(bg, `UPDATE domains SET state = 'inactive' WHERE env_id = $1`, f.env.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.st.Notify(bg, "ferun_ingress", store.IngressEnvPayload(f.env.ID)); err != nil {
		t.Fatal(err)
	}
	await(t, b, time.Second, "the default host back", func(s *Snapshot) bool {
		e := envOf(s, f.env.ID)
		return len(e.Domains) == 0 && slices.Equal(e.Hosts, []string{"web-acme.apps.example"})
	})

	// Without a sealer: none.
	nb := f.builder(t)
	s, _ = use(t, nb)
	if e := envOf(s, f.env.ID); len(e.Domains) != 0 || len(e.Hosts) != 1 {
		t.Fatalf("without a sealer: %v %+v", e.Hosts, e.Domains)
	}
}

// The challenges reach the snapshot on their notification; an expired
// one is not carried; a deletion takes them out.
func TestBuilderChallenges(t *testing.T) {
	f := newFixture(t)
	b := f.builder(t)
	s, _ := use(t, b)
	if len(s.Challenges) != 0 {
		t.Fatalf("challenges %+v", s.Challenges)
	}
	id := f.mkDomain(t, f.env.ID, "dashboard.fecore.dev", "issuing", time.Time{}, nil, nil, nil)
	if _, err := f.d.Pool.Exec(bg, `INSERT INTO acme_challenges (token, host, key_auth, domain_id, expires_at)
		VALUES ('old', 'dashboard.fecore.dev', 'old.k', $1, now() - interval '1 second')`, id); err != nil {
		t.Fatal(err)
	}
	if err := f.st.PutACMEChallenge(bg, store.ACMEChallenge{Token: "tok-1", Host: "dashboard.fecore.dev",
		KeyAuth: "tok-1.thumb", DomainID: id}, time.Minute); err != nil {
		t.Fatal(err)
	}
	s = await(t, b, time.Second, "the challenge", func(s *Snapshot) bool { return len(s.Challenges) == 1 })
	if s.Challenges[0] != (wire.Challenge{Token: "tok-1", Host: "dashboard.fecore.dev", KeyAuth: "tok-1.thumb"}) {
		t.Fatalf("challenge %+v", s.Challenges[0])
	}
	if n, err := f.st.DeleteACMEChallenges(bg, id); err != nil || n != 2 {
		t.Fatalf("delete %d %v", n, err)
	}
	await(t, b, time.Second, "no challenge", func(s *Snapshot) bool { return len(s.Challenges) == 0 })
}
