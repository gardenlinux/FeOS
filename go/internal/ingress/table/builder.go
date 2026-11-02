// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package table

import (
	"bytes"
	"context"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The Builder's windows unless a test sets others.
const (
	// DefaultDebounce is how long the changes of environments and of the
	// mesh collect after the first one before they are read.
	DefaultDebounce = 50 * time.Millisecond
	// DefaultFull is the time between two reads of the whole table: the
	// backstop for writes that sent no notification (an older ferund's).
	DefaultFull = 5 * time.Second
	// DefaultHealth is how long after its last successful read a
	// snapshot stays healthy.
	DefaultHealth = 30 * time.Second
)

const (
	// minBackoff and maxBackoff bound the wait after a failed read.
	minBackoff = 100 * time.Millisecond
	maxBackoff = 5 * time.Second
	// readTimeout bounds one read, so that a database that does not
	// answer never holds the loop past the health window.
	readTimeout = 10 * time.Second
	// failureLogEvery is the least time between two warnings of failed
	// reads, and between two warnings of one environment that cannot be
	// routed.
	failureLogEvery = time.Minute
	// maxWarned bounds the environments whose last spec warning is kept.
	maxWarned = 4096
	// MaxChallenges bounds the ACME challenges the table carries: orders
	// run one at a time, so a handful are pending at most.
	MaxChallenges = 1024
)

// Opener opens what ferund sealed under --secret-key-file (seal.Sealer):
// the domains' keys.
type Opener interface {
	Open(sealed, aad []byte) ([]byte, error)
}

// Snapshot is the whole table at one time. It is immutable: the Builder
// replaces it and never changes it, and its readers must not change it,
// nor the environments in it.
type Snapshot struct {
	// Envs are the environments the ingress serves, by ID.
	Envs map[string]wire.Env
	// Mesh is the nodes the ingress peers with.
	Mesh wire.Mesh
	// GroupVersion is the version of the security group ferun-ingress
	// (pod_network.ingress_group_version) as of the last read of the
	// whole table or of the mesh.
	GroupVersion uint64
	// Challenges are the pending ACME HTTP-01 answers, sorted by token.
	Challenges []wire.Challenge
	// At is when the read that made the snapshot ended.
	At time.Time
	// Healthy: the Builder's last successful read is younger than its
	// Health window. A Builder that cannot read the database replaces a
	// healthy snapshot with an unhealthy copy once the window passed.
	Healthy bool

	// bySandbox maps the sandbox of a counted instance to its
	// environment, sandboxes an environment to its counted sandboxes.
	bySandbox map[podid.ID]uuid.UUID
	sandboxes map[uuid.UUID][]podid.ID
}

func newSnapshot() *Snapshot {
	return &Snapshot{Envs: map[string]wire.Env{}, Mesh: wire.Mesh{Peers: []wire.Peer{}},
		bySandbox: map[podid.ID]uuid.UUID{}, sandboxes: map[uuid.UUID][]podid.ID{}}
}

// clone returns a copy of s to change: its maps are its own, the values
// in them shared.
func (s *Snapshot) clone() *Snapshot {
	c := *s
	c.Envs, c.bySandbox, c.sandboxes = maps.Clone(s.Envs), maps.Clone(s.bySandbox), maps.Clone(s.sandboxes)
	return &c
}

// put sets environment e, whose counted instances' sandboxes are ids; it
// reports whether its content or its sandboxes changed.
func (s *Snapshot) put(id uuid.UUID, e wire.Env, ids []podid.ID) bool {
	old, had := s.Envs[e.ID]
	oldIDs := s.sandboxes[id]
	if had && old.Hash == e.Hash && slices.Equal(oldIDs, ids) {
		return false
	}
	s.unindex(id)
	s.Envs[e.ID] = e
	s.sandboxes[id] = ids
	for _, sb := range ids {
		s.bySandbox[sb] = id
	}
	return true
}

// remove deletes environment id; it reports whether s had it.
func (s *Snapshot) remove(id uuid.UUID) bool {
	_, had := s.Envs[id.String()]
	s.unindex(id)
	delete(s.Envs, id.String())
	return had
}

// unindex forgets environment id's sandboxes.
func (s *Snapshot) unindex(id uuid.UUID) {
	for _, sb := range s.sandboxes[id] {
		if s.bySandbox[sb] == id {
			delete(s.bySandbox, sb)
		}
	}
	delete(s.sandboxes, id)
}

// sameContent reports whether s and o hold the same table: environments,
// their sandboxes, mesh, group version and challenges.
func (s *Snapshot) sameContent(o *Snapshot) bool {
	if len(s.Envs) != len(o.Envs) || s.GroupVersion != o.GroupVersion || !sameMesh(s.Mesh, o.Mesh) ||
		!slices.Equal(s.Challenges, o.Challenges) {
		return false
	}
	for id, e := range s.Envs {
		if oe, ok := o.Envs[id]; !ok || oe.Hash != e.Hash {
			return false
		}
	}
	return maps.EqualFunc(s.sandboxes, o.sandboxes, slices.Equal)
}

func sameMesh(a, b wire.Mesh) bool {
	return a.Version == b.Version && slices.EqualFunc(a.Peers, b.Peers, func(p, q wire.Peer) bool {
		return p.Name == q.Name && bytes.Equal(p.PublicKey, q.PublicKey) && p.Endpoint == q.Endpoint && p.Prefix == q.Prefix
	})
}

// Builder keeps the ingress's table up to date while it is used (Use):
// it reads the whole table at once, then follows ferun_ingress (an
// environment, the mesh, the ACME challenges, everything) and
// ferun_sandbox (an instance's sandbox) and reads what they name after
// Debounce, and reads the whole table again every Full. An environment
// whose read returns no rows has left the table: it is not public any
// more, deleting, of a deleting tenant, or without a deployment. The
// domains' keys are opened with Sealer; without one, no domain is
// served. While no one uses it, it reads nothing. It is safe for
// concurrent use; Run runs its loop.
type Builder struct {
	// The windows, set before Run (the defaults when zero): changes
	// collect for Debounce, the whole table is read every Full, and a
	// snapshot is healthy until Health after the last successful read.
	Debounce, Full, Health time.Duration
	// Sealer opens the domains' keys; set before Run. Nil: the table
	// carries no domain.
	Sealer Opener

	db    *db.DB
	st    *store.Store
	zone  string
	log   *slog.Logger
	specs *Specs

	// warned is when each environment that cannot be routed was last
	// warned of, domainWarned each domain left out; the loop's own.
	warned       map[uuid.UUID]time.Time
	domainWarned map[uuid.UUID]time.Time

	mu    sync.Mutex
	users int
	cur   *Snapshot
	// fresh: cur was read since the builder was last used by no one.
	fresh   bool
	changed chan struct{}
	kick    chan struct{} // one deep: Use and its release
}

// NewBuilder returns the builder of the table for the apps zone zone
// ("": no hosts): it follows d's notifications and reads with st (a
// Store on d when nil).
func NewBuilder(d *db.DB, st *store.Store, zone string, log *slog.Logger) *Builder {
	if st == nil {
		st = store.New(d)
	}
	if log == nil {
		log = slog.Default()
	}
	return &Builder{Debounce: DefaultDebounce, Full: DefaultFull, Health: DefaultHealth,
		db: d, st: st, zone: zone, log: log.With("component", "ingress-table"), specs: NewSpecs(st),
		warned: map[uuid.UUID]time.Time{}, domainWarned: map[uuid.UUID]time.Time{}, changed: make(chan struct{}),
		kick: make(chan struct{}, 1)}
}

// Specs returns the builder's cache of deployment specs.
func (b *Builder) Specs() *Specs { return b.specs }

// Use counts a user of the table (an ingress stream) and returns the
// function that ends the use; it may be called more than once. The first
// user starts the reads; when the last one has gone, they stop, and the
// next user waits for a new read (Wait).
func (b *Builder) Use() func() {
	b.mu.Lock()
	b.users++
	b.mu.Unlock()
	b.signal()
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			b.users--
			b.mu.Unlock()
			b.signal()
		})
	}
}

func (b *Builder) signal() {
	select {
	case b.kick <- struct{}{}:
	default:
	}
}

// Current returns the newest snapshot; nil before the first read. While
// no one uses the builder it is not kept up to date: a user calls Wait
// first.
func (b *Builder) Current() *Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cur
}

// Changed returns a channel that is closed when the snapshot is replaced.
// A reader takes it before Current, so that it misses no replacement.
func (b *Builder) Changed() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.changed
}

// Wait waits until a snapshot read while the builder is in use exists,
// and returns it; ctx's error when ctx ends first. The caller uses the
// builder (Use).
func (b *Builder) Wait(ctx context.Context) (*Snapshot, error) {
	for {
		b.mu.Lock()
		s, fresh, ch := b.cur, b.fresh, b.changed
		b.mu.Unlock()
		if s != nil && fresh {
			return s, nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// publish makes s the current snapshot, read while in use.
func (b *Builder) publish(s *Snapshot) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cur, b.fresh = s, true
	close(b.changed)
	b.changed = make(chan struct{})
}

// inUse reports whether anyone uses the builder; when no one does, the
// current snapshot stops being fresh in the same step, so that a user
// who comes next waits for a new read.
func (b *Builder) inUse() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.users > 0 {
		return true
	}
	b.fresh = false
	return false
}

// Run keeps the table up to date while the builder is used, until ctx
// ends; it returns nil then.
func (b *Builder) Run(ctx context.Context) error {
	if b.Debounce <= 0 {
		b.Debounce = DefaultDebounce
	}
	if b.Full <= 0 {
		b.Full = DefaultFull
	}
	if b.Health <= 0 {
		b.Health = DefaultHealth
	}
	for {
		for !b.inUse() {
			select {
			case <-ctx.Done():
				return nil
			case <-b.kick:
			}
		}
		b.serve(ctx)
		if ctx.Err() != nil {
			return nil
		}
	}
}

// loop is the state of serve.
type loop struct {
	// fullAt is when the whole table is read next.
	fullAt time.Time
	// dirty are the environments to read, mesh whether to read the mesh
	// and challenges whether to read the challenges, at batchAt (zero:
	// nothing to read).
	dirty      map[uuid.UUID]struct{}
	mesh       bool
	challenges bool
	batchAt    time.Time
	// After a failed read nothing is read before retryAt; backoff is
	// the wait after the next failure.
	retryAt time.Time
	backoff time.Duration
	// lastOK is when the last read succeeded; failedAt when the reads
	// started to fail (zero: they do not), loggedAt when that was last
	// logged.
	lastOK, failedAt, loggedAt time.Time
}

// due is when the next read is due.
func (l *loop) due() time.Time {
	at := l.fullAt
	if !l.batchAt.IsZero() && l.batchAt.Before(at) {
		at = l.batchAt
	}
	if at.Before(l.retryAt) {
		at = l.retryAt
	}
	return at
}

// soon has the changes read Debounce after the first one.
func (l *loop) soon(now time.Time, debounce time.Duration) {
	if l.batchAt.IsZero() {
		l.batchAt = now.Add(debounce)
	}
}

// fullSoon has the whole table read within debounce.
func (l *loop) fullSoon(now time.Time, debounce time.Duration) {
	if at := now.Add(debounce); at.Before(l.fullAt) {
		l.fullAt = at
	}
}

// serve subscribes and keeps the table up to date until no one uses the
// builder or ctx ends.
func (b *Builder) serve(ctx context.Context) {
	ingressNotes, unsubscribeIngress := b.db.Subscribe(db.ChanIngress)
	defer unsubscribeIngress()
	sandboxNotes, unsubscribeSandbox := b.db.Subscribe(db.ChanSandbox)
	defer unsubscribeSandbox()
	// Subscribed first: the first read, at once, sees every write whose
	// notification is not on its way.
	l := &loop{fullAt: time.Now(), dirty: map[uuid.UUID]struct{}{}}
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for b.inUse() && ctx.Err() == nil {
		now := time.Now()
		due := l.due()
		if !now.Before(due) {
			b.step(ctx, l, now)
			continue
		}
		wake := due
		if h := b.healthAt(l); !h.IsZero() && h.Before(wake) {
			wake = h
		}
		timer.Reset(wake.Sub(now))
		select {
		case <-ctx.Done():
			return
		case <-b.kick:
		case p := <-ingressNotes:
			b.onIngress(l, p, now)
		case p := <-sandboxNotes:
			b.onSandbox(l, p, now)
		case <-timer.C:
			b.checkHealth(l)
		}
	}
}

// onIngress takes a ferun_ingress payload: "env:<id>" has that
// environment read, "mesh" the mesh, "challenges" the ACME challenges;
// "" (the listener reconnected) and "*" the whole table;
// "ingress:<name>" (an ingress's own row, which the table does not hold)
// and anything else are ignored.
func (b *Builder) onIngress(l *loop, p string, now time.Time) {
	switch {
	case p == "" || p == store.IngressPayloadAll:
		l.fullSoon(now, b.Debounce)
	case p == store.IngressPayloadMesh:
		l.mesh = true
		l.soon(now, b.Debounce)
	case p == store.IngressPayloadChallenges:
		l.challenges = true
		l.soon(now, b.Debounce)
	case strings.HasPrefix(p, "env:"):
		id, err := uuid.Parse(strings.TrimPrefix(p, "env:"))
		if err != nil {
			b.log.Debug("ingress table: a ferun_ingress payload names no environment", "payload", p)
			return
		}
		l.dirty[id] = struct{}{}
		l.soon(now, b.Debounce)
	}
}

// onSandbox takes a ferun_sandbox payload: a sandbox of a counted
// instance has its environment read; "" and "*" (too many to name) the
// whole table. Other sandboxes are not the table's: a new instance comes
// with its environment's ferun_ingress.
func (b *Builder) onSandbox(l *loop, p string, now time.Time) {
	if p == "" || p == "*" {
		l.fullSoon(now, b.Debounce)
		return
	}
	cur := b.Current()
	if cur == nil {
		return
	}
	if id, ok := cur.bySandbox[podid.ID(p)]; ok {
		l.dirty[id] = struct{}{}
		l.soon(now, b.Debounce)
	}
}

// step reads what is due: the whole table when its time has come (or no
// snapshot read in this use exists), else the dirty environments and the
// mesh. A failure keeps what was due and backs off.
func (b *Builder) step(ctx context.Context, l *loop, now time.Time) {
	b.mu.Lock()
	full := !now.Before(l.fullAt) || b.cur == nil || !b.fresh
	b.mu.Unlock()
	rctx, cancel := context.WithTimeout(ctx, readTimeout)
	var err error
	if full {
		err = b.readAll(rctx)
	} else {
		err = b.readSome(rctx, l.dirty, l.mesh, l.challenges)
	}
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		l.backoff = min(max(l.backoff*2, minBackoff), maxBackoff)
		l.retryAt = time.Now().Add(l.backoff)
		b.failed(l, err)
		b.checkHealth(l)
		return
	}
	done := time.Now()
	if !l.failedAt.IsZero() {
		b.log.Info("ingress table: reads the database again", "failed_for", done.Sub(l.failedAt).Round(time.Millisecond))
	}
	l.lastOK, l.failedAt, l.loggedAt = done, time.Time{}, time.Time{}
	l.backoff, l.retryAt = 0, time.Time{}
	clear(l.dirty)
	l.mesh, l.challenges, l.batchAt = false, false, time.Time{}
	if full {
		l.fullAt = done.Add(b.Full)
	}
}

// failed logs a failed read: when the reads start to fail, then at most
// every failureLogEvery.
func (b *Builder) failed(l *loop, err error) {
	now := time.Now()
	if l.failedAt.IsZero() {
		l.failedAt = now
	}
	if !l.loggedAt.IsZero() && now.Sub(l.loggedAt) < failureLogEvery {
		return
	}
	l.loggedAt = now
	b.log.Warn("ingress table: cannot read the database", "err", err,
		"failing_for", now.Sub(l.failedAt).Round(time.Millisecond), "retry_in", l.backoff)
}

// healthAt is when the current snapshot turns unhealthy unless a read
// succeeds before; zero when it is not healthy or not this use's.
func (b *Builder) healthAt(l *loop) time.Time {
	cur := b.Current()
	if cur == nil || !cur.Healthy || l.lastOK.IsZero() {
		return time.Time{}
	}
	return l.lastOK.Add(b.Health)
}

// checkHealth replaces a healthy snapshot with an unhealthy copy once the
// last successful read is Health old.
func (b *Builder) checkHealth(l *loop) {
	at := b.healthAt(l)
	if at.IsZero() || time.Now().Before(at) {
		return
	}
	next := b.Current().clone()
	next.Healthy = false
	b.publish(next)
}

// readAll reads the whole table and publishes it when it differs from
// the current snapshot, or when that one is unhealthy or stale.
func (b *Builder) readAll(ctx context.Context) error {
	rows, err := b.st.IngressRouting(ctx, nil)
	if err != nil {
		return err
	}
	envs := Group(rows)
	if err := b.addDomains(ctx, envs, nil); err != nil {
		return err
	}
	m, version, err := b.readMesh(ctx)
	if err != nil {
		return err
	}
	challenges, err := b.readChallenges(ctx)
	if err != nil {
		return err
	}
	next := newSnapshot()
	next.Mesh, next.GroupVersion, next.Challenges = m, version, challenges
	for _, e := range envs {
		env, ids, err := b.build(ctx, e)
		if unroutable(err) {
			b.warnSpec(e.ID, err)
			continue
		}
		if err != nil {
			return err
		}
		next.put(e.ID, env, ids)
	}
	next.At, next.Healthy = time.Now(), true
	b.mu.Lock()
	cur, fresh := b.cur, b.fresh
	b.mu.Unlock()
	if cur == nil || !fresh || !cur.Healthy || !cur.sameContent(next) {
		b.publish(next)
	}
	return nil
}

// build builds an environment's entry (BuildEnv) and warns of the
// domains it left out.
func (b *Builder) build(ctx context.Context, e EnvRows) (wire.Env, []podid.ID, error) {
	env, ids, err := BuildEnv(b.zone, e, b.specs.For(ctx, e))
	if err == nil && len(env.Domains) < len(e.Domains) {
		b.warnDomain(e.ID, "ingress table: domains of an environment are left out: its entry cannot take them",
			"environment", e.ID, "domains", len(e.Domains), "served", len(env.Domains))
	}
	return env, ids, err
}

// addDomains reads the served domains of envs (all of them when ids is
// nil, else of ids) and adds each to its environment, its key opened. A
// key that does not open leaves its domain out, with a warning; without
// a Sealer no domain is read.
func (b *Builder) addDomains(ctx context.Context, envs []EnvRows, ids []uuid.UUID) error {
	if b.Sealer == nil || len(envs) == 0 {
		return nil
	}
	ds, err := b.st.IngressDomains(ctx, ids)
	if err != nil {
		return err
	}
	at := make(map[uuid.UUID]int, len(envs))
	for i, e := range envs {
		at[e.ID] = i
	}
	for _, d := range ds {
		i, ok := at[d.EnvID]
		if !ok {
			continue
		}
		key, err := b.Sealer.Open(d.KeySealed, store.DomainCertAAD(d.DomainID))
		if err != nil {
			b.warnDomain(d.DomainID, "ingress table: a domain's key does not open; the ingress does not serve it",
				"domain", d.Name, "environment", d.EnvID, "err", err)
			continue
		}
		envs[i].Domains = append(envs[i].Domains, wire.Domain{Name: d.Name, Chain: d.Chain, Key: key})
	}
	return nil
}

// readChallenges reads the pending ACME challenges.
func (b *Builder) readChallenges(ctx context.Context) ([]wire.Challenge, error) {
	rows, err := b.st.IngressChallenges(ctx, MaxChallenges)
	if err != nil {
		return nil, err
	}
	out := make([]wire.Challenge, 0, len(rows))
	for _, r := range rows {
		out = append(out, wire.Challenge{Token: r.Token, Host: r.Host, KeyAuth: r.KeyAuth})
	}
	return out, nil
}

// readSome reads the environments of dirty, the mesh when mesh and the
// challenges when challenges into a copy of the current snapshot, and
// publishes it when anything changed or the current one is unhealthy. A dirty
// environment without rows leaves the table, and so does one that
// cannot be routed (no usable spec, too large).
func (b *Builder) readSome(ctx context.Context, dirty map[uuid.UUID]struct{}, mesh, challenges bool) error {
	cur := b.Current()
	next := cur.clone()
	changed := false
	if mesh {
		m, version, err := b.readMesh(ctx)
		if err != nil {
			return err
		}
		if !sameMesh(next.Mesh, m) || next.GroupVersion != version {
			next.Mesh, next.GroupVersion, changed = m, version, true
		}
	}
	if challenges {
		cs, err := b.readChallenges(ctx)
		if err != nil {
			return err
		}
		if !slices.Equal(next.Challenges, cs) {
			next.Challenges, changed = cs, true
		}
	}
	if len(dirty) > 0 {
		ids := slices.SortedFunc(maps.Keys(dirty), func(a, c uuid.UUID) int { return bytes.Compare(a[:], c[:]) })
		rows, err := b.st.IngressRouting(ctx, ids)
		if err != nil {
			return err
		}
		type built struct {
			env wire.Env
			ids []podid.ID
		}
		got := map[uuid.UUID]built{}
		envs := Group(rows)
		if err := b.addDomains(ctx, envs, ids); err != nil {
			return err
		}
		for _, e := range envs {
			env, sbs, err := b.build(ctx, e)
			if unroutable(err) {
				b.warnSpec(e.ID, err)
				continue
			}
			if err != nil {
				return err
			}
			got[e.ID] = built{env: env, ids: sbs}
		}
		for _, id := range ids {
			if g, ok := got[id]; ok {
				changed = next.put(id, g.env, g.ids) || changed
			} else {
				changed = next.remove(id) || changed
			}
		}
	}
	if !changed && cur.Healthy {
		return nil
	}
	next.At, next.Healthy = time.Now(), true
	b.publish(next)
	return nil
}

// readMesh reads the nodes the ingress peers with, and the version of
// the security group ferun-ingress.
func (b *Builder) readMesh(ctx context.Context) (wire.Mesh, uint64, error) {
	peers, err := b.st.MeshPeers(ctx)
	if err != nil {
		return wire.Mesh{}, 0, err
	}
	_, version, err := b.st.IngressGroup(ctx)
	if err != nil {
		return wire.Mesh{}, 0, err
	}
	return BuildMesh(peers), uint64(max(version, 0)), nil
}

// warnSpec logs that environment id cannot be routed (its routing deployment
// has no usable spec, or it is too large), at most every failureLogEvery
// per environment.
func (b *Builder) warnSpec(id uuid.UUID, err error) {
	if !due(b.warned, id) {
		return
	}
	b.log.Warn("ingress table: an environment cannot be routed; the ingress does not serve it",
		"environment", id, "err", err)
}

// warnDomain logs msg about a domain or an environment (id), at most
// every failureLogEvery per ID.
func (b *Builder) warnDomain(id uuid.UUID, msg string, args ...any) {
	if due(b.domainWarned, id) {
		b.log.Warn(msg, args...)
	}
}

// due reports whether id was not warned of within failureLogEvery in
// warned, and notes it as warned of now when so; warned keeps at most
// maxWarned IDs.
func due(warned map[uuid.UUID]time.Time, id uuid.UUID) bool {
	now := time.Now()
	if last, ok := warned[id]; ok && now.Sub(last) < failureLogEvery {
		return false
	}
	if len(warned) >= maxWarned {
		for k, last := range warned {
			if now.Sub(last) >= failureLogEvery {
				delete(warned, k)
			}
		}
		if len(warned) >= maxWarned {
			clear(warned)
		}
	}
	warned[id] = now
	return true
}
