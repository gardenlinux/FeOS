// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package domains

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/leader"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The codes of the domains' error answers {"error", "code"}, which
// clients (FeCode) match.
const (
	// CodeInvalidName: 400, the name is not a host name that can be
	// claimed as written.
	CodeInvalidName = "invalid_name"
	// CodeReserved: 422, the name is the apps zone, the edge's name, a
	// --domain-reserved zone, or under one of them.
	CodeReserved = "reserved"
	// CodeTooManyUnverified: 409, the project holds --domain-unverified-max
	// claims that are not verified.
	CodeTooManyUnverified = "too_many_unverified"
	// CodeDomainsOff: 409, the platform has no --edge-name.
	CodeDomainsOff = "domains_off"
	// CodeSuperseded: 409, the claim was superseded and cannot be bound.
	CodeSuperseded = "superseded"
	// CodeWildcardsOff: 409, the platform has no ACME zone (--acme-zone),
	// which a wildcard's certificate needs.
	CodeWildcardsOff = "wildcards_off"
)

// The texts of the plain answers.
const (
	textNoDomain      = "no such domain"
	textNoEnvironment = "no such environment"
	textDomainsOff    = "the platform serves no domains (--edge-name)"
	textWildcardsOff  = "the platform serves no wildcards: it has no ACME zone (--acme-zone), where a wildcard's certificate is answered"
)

// The bounds of a claim's labels (as an environment's).
const (
	maxLabels     = 64
	maxLabelBytes = 253
)

// The loops' timings.
const (
	// LoopCheck checks the domains that are due; LoopSettle derives the
	// states that changed without a write of the domain's own (an
	// environment's publicity or deletion).
	LoopCheck  = "domains-check"
	LoopSettle = "domains-settle"

	checkEvery  = 5 * time.Second
	settleEvery = 30 * time.Second
	// checkBatch is the most domains one run checks, checkParallel at
	// once; checkLease moves a claimed domain's next check ahead until
	// its result is written, so that a check whose instance died runs
	// again then.
	checkBatch    = 64
	checkParallel = 8
	checkLease    = 5 * time.Minute

	// The leader's transactions: short lock and statement timeouts.
	fencedLockTimeout = time.Second
	fencedStmtTimeout = 3 * time.Second
)

// Config is the domains' part of the instance's configuration.
type Config struct {
	// EdgeName is --edge-name, the name a domain's CNAME points at; ""
	// turns domains off (no claims, no checks).
	EdgeName string
	// EdgeAddresses are --edge-address: edge addresses beside what
	// EdgeName resolves to.
	EdgeAddresses []netip.Addr
	// ChallengeLabel is --domain-challenge-label: a claim's TXT record is
	// at <label>.<name>, a wildcard's beside the name it stands on.
	ChallengeLabel string
	// ACMEZone is --acme-zone (ADR 0045): a wildcard *.<name> delegates
	// _acme-challenge.<name> to <token>.<ACMEZone>. "": no wildcard can
	// be claimed.
	ACMEZone string
	// Grace is --domain-grace.
	Grace time.Duration
	// UnverifiedMax is --domain-unverified-max.
	UnverifiedMax int
	// Reserved are the zones no claim may name, or name a name under:
	// the apps zone and every --domain-reserved.
	Reserved []string
	Log      *slog.Logger
}

// Deps are the parts of the instance the service works with. Store and
// DNS are required; without an Elector the loops write nothing.
type Deps struct {
	Store   *store.Store
	Elector *leader.Elector
	DNS     DNS
}

// Service is the domains' core of one instance (ADR 0021): the claims,
// their bindings and checks, the states derived from them, and the
// incidents of their environments.
type Service struct {
	cfg     Config
	st      *store.Store
	elector *leader.Elector
	dns     DNS
	log     *slog.Logger
	edge    edgeCache
}

// New returns the service.
func New(cfg Config, deps Deps) (*Service, error) {
	if deps.Store == nil || deps.DNS == nil {
		return nil, errors.New("domains: Store and DNS are required")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.ChallengeLabel == "" {
		cfg.ChallengeLabel = "_ferun-challenge"
	}
	if cfg.Grace <= 0 {
		cfg.Grace = 72 * time.Hour
	}
	if cfg.UnverifiedMax <= 0 {
		cfg.UnverifiedMax = 50
	}
	return &Service{cfg: cfg, st: deps.Store, elector: deps.Elector, dns: deps.DNS,
		log: cfg.Log.With("component", "domains")}, nil
}

// Enabled reports whether the platform serves domains (--edge-name).
func (s *Service) Enabled() bool { return s.cfg.EdgeName != "" }

// Loops returns the leader's loops of the domains; none while domains are
// off.
func (s *Service) Loops() []model.Loop {
	if !s.Enabled() {
		return nil
	}
	return []model.Loop{
		{Name: LoopCheck, Interval: checkEvery, Run: s.checkDue},
		{Name: LoopSettle, Interval: settleEvery, Run: s.settleAll},
	}
}

// txtName is the name of a claim's TXT record, beside the name it
// stands on (a wildcard's beside its <name>).
func (s *Service) txtName(name string) string { return s.cfg.ChallengeLabel + "." + Base(name) }

// AcmeChallengeLabel is the label DNS-01 asks at, beside the name
// (RFC 8555 §8.4).
const AcmeChallengeLabel = "_acme-challenge"

// delegationName is the name of a wildcard's delegation, the CNAME
// _acme-challenge.<name>.
func (s *Service) delegationName(name string) string { return AcmeChallengeLabel + "." + Base(name) }

// target is where a wildcard's delegation points, its claim's own name in
// the ACME zone: <token>.<ACME zone>.
func (s *Service) target(token string) string { return Target(token, s.cfg.ACMEZone) }

// Target is a wildcard claim's DNS-01 target: <token>.<zone>, the name in
// the ACME zone its delegation names and its challenges' TXT records
// stand at (ADR 0045).
func Target(token, zone string) string { return token + "." + zone }

// probeName is a fresh name below a wildcard, whose addresses a check
// asks for (* itself is no name to resolve): a random label of 16
// characters of lowercase base32 under its <name>.
func probeName(name string) (string, error) {
	t, err := NewToken()
	if err != nil {
		return "", err
	}
	return t[:16] + "." + Base(name), nil
}

// inTx runs fn in a transaction; fenced, as the leader (short timeouts,
// the lease checked first: leader.ErrNotLeader without it).
func (s *Service) inTx(ctx context.Context, fenced bool, fn func(tx *store.Store) error) error {
	return s.st.InTx(ctx, func(tx *store.Store) error {
		if fenced {
			if s.elector == nil {
				return leader.ErrNotLeader
			}
			if err := tx.SetLocalTimeouts(ctx, fencedLockTimeout, fencedStmtTimeout); err != nil {
				return err
			}
			if err := s.elector.Fence(ctx, tx.Q()); err != nil {
				return err
			}
		}
		return fn(tx)
	})
}

// errNoDomain answers a domain the project does not have, foreign or
// absent alike.
func errNoDomain() error { return apierr.New(http.StatusNotFound, textNoDomain) }

// parseID reads a domain's ID from a path; anything but a UUID names no
// domain.
func parseID(s string) (uuid.UUID, error) {
	if len(s) != 36 {
		return uuid.Nil, errNoDomain()
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, errNoDomain()
	}
	return id, nil
}

// resolveEnv reads the environment of project that ref names, its UUID or
// its name, as the environments' routes resolve {env}, and locks it with
// lock; 404 "no such environment" otherwise.
func resolveEnv(ctx context.Context, st *store.Store, project, ref string, lock store.RowLock) (store.Environment, error) {
	var e store.Environment
	var err error
	if id, perr := uuid.Parse(ref); perr == nil && len(ref) == 36 {
		e, err = st.LockEnvironment(ctx, id, lock)
	} else {
		e, err = st.LockEnvironmentByName(ctx, project, ref, lock)
	}
	if errors.Is(err, store.ErrNoRow) || (err == nil && e.Owner != project) {
		return store.Environment{}, apierr.New(http.StatusNotFound, textNoEnvironment)
	}
	if err != nil {
		return store.Environment{}, fmt.Errorf("environment %s: %w", ref, err)
	}
	return e, nil
}

// view reads the project's domain id with lock; 404 for another's.
func (s *Service) view(ctx context.Context, tx *store.Store, project string, id uuid.UUID, lock store.RowLock) (store.DomainView, error) {
	v, err := tx.LockDomainView(ctx, id, lock)
	if errors.Is(err, store.ErrNoRow) || (err == nil && v.Owner != project) {
		return store.DomainView{}, errNoDomain()
	}
	if err != nil {
		return store.DomainView{}, fmt.Errorf("domain %s: %w", id, err)
	}
	return v, nil
}

// get answers the project's domain id.
func (s *Service) get(ctx context.Context, project string, id uuid.UUID) (Domain, error) {
	v, err := s.view(ctx, s.st, project, id, store.NoLock)
	if err != nil {
		return Domain{}, err
	}
	return s.answer(v, s.edge.get(ctx, s.dns, s.cfg, s.log)), nil
}

// reserved returns the zone that reserves name, or "": the name is the
// zone or under it, or for a wildcard its <name> is, or the wildcard
// covers the zone's name (the zone is one label below its <name>).
func (s *Service) reserved(name string) string {
	base := Base(name)
	for _, z := range append(slices.Clone(s.cfg.Reserved), s.cfg.EdgeName) {
		if Under(base, z) {
			return z
		}
		if IsWildcard(name) && z != "" && strings.HasSuffix(z, "."+base) && !strings.Contains(strings.TrimSuffix(z, "."+base), ".") {
			return z
		}
	}
	return ""
}

// checkLabels checks a claim's labels.
func checkLabels(labels map[string]string) error {
	if len(labels) > maxLabels {
		return apierr.New(http.StatusBadRequest, "%d labels; at most %d are allowed", len(labels), maxLabels)
	}
	for k, v := range labels {
		if k == "" || len(k) > maxLabelBytes || len(v) > maxLabelBytes {
			return apierr.New(http.StatusBadRequest, "label %q: keys are 1..%d bytes, values at most %d", k,
				maxLabelBytes, maxLabelBytes)
		}
	}
	return nil
}

// CreateDomain is POST /api/v1/domains: a new claim of the project on a
// name or a wildcard *.<name> (ADR 0045), unverified, checked at once and
// then with backoff. Any number of claims may name one name. 400
// invalid_name, 422 reserved, 409 too_many_unverified, 409 domains_off,
// 409 wildcards_off for a wildcard without --acme-zone.
func (s *Service) CreateDomain(ctx context.Context, project string, req CreateRequest) (Domain, error) {
	if !s.Enabled() {
		return Domain{}, apierr.WithCode(http.StatusConflict, CodeDomainsOff, textDomainsOff)
	}
	name, wildcard, err := NormalizeClaim(req.Name)
	if err != nil {
		return Domain{}, apierr.WithCode(http.StatusBadRequest, CodeInvalidName, "%s", err.Error())
	}
	if z := s.reserved(name); z != "" {
		return Domain{}, apierr.WithCode(http.StatusUnprocessableEntity, CodeReserved,
			"%s is reserved by the platform: no domain can be %s or a name under it, nor a wildcard that covers it", name, z)
	}
	if wildcard && s.cfg.ACMEZone == "" {
		return Domain{}, apierr.WithCode(http.StatusConflict, CodeWildcardsOff, textWildcardsOff)
	}
	if err := checkLabels(req.Labels); err != nil {
		return Domain{}, err
	}
	token, err := NewToken()
	if err != nil {
		return Domain{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Domain{}, fmt.Errorf("a domain's ID: %w", err)
	}
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		t, err := tx.LockProject(ctx, project, store.ForNoKeyUpdate)
		if errors.Is(err, store.ErrNoRow) {
			return apierr.New(http.StatusUnauthorized, "unknown project token")
		}
		if err != nil {
			return fmt.Errorf("project %s: %w", project, err)
		}
		if t.Deleting() {
			return apierr.New(http.StatusConflict, "project %s is being deleted", project)
		}
		n, err := tx.CountUnverifiedDomains(ctx, project)
		if err != nil {
			return fmt.Errorf("count the unverified domains of %s: %w", project, err)
		}
		if n >= s.cfg.UnverifiedMax {
			return apierr.WithCode(http.StatusConflict, CodeTooManyUnverified,
				"project %s holds %d domains that are not verified, the most it may (--domain-unverified-max): "+
					"verify or delete one first", project, n)
		}
		_, err = tx.InsertDomain(ctx, store.NewDomain{ID: id, Owner: project, Name: name, Labels: req.Labels,
			Token: token, State: StateUnverified, Reason: ReasonUnverified, MadeBy: auth.MadeBy(ctx)})
		return err
	})
	if err != nil {
		return Domain{}, err
	}
	s.log.Info("domain claimed", "project", project, "domain", name, "id", id)
	return s.get(ctx, project, id)
}

// ListDomains is GET /api/v1/domains: the project's domains, those bound
// to env (its UUID or name; "": any) with every label of labels, oldest
// first.
func (s *Service) ListDomains(ctx context.Context, project, env string, labels map[string]string) (List, error) {
	envID := uuid.Nil
	if env != "" {
		e, err := resolveEnv(ctx, s.st, project, env, store.NoLock)
		if err != nil {
			return List{}, err
		}
		envID = e.ID
	}
	views, err := s.st.ListDomainViews(ctx, project, envID, labels)
	if err != nil {
		return List{}, fmt.Errorf("list domains: %w", err)
	}
	edge := s.edge.get(ctx, s.dns, s.cfg, s.log)
	out := List{Domains: make([]Domain, 0, len(views))}
	for _, v := range views {
		out.Domains = append(out.Domains, s.answer(v, edge))
	}
	return out, nil
}

// GetDomain is GET /api/v1/domains/{id}.
func (s *Service) GetDomain(ctx context.Context, project, id string) (Domain, error) {
	did, err := parseID(id)
	if err != nil {
		return Domain{}, err
	}
	return s.get(ctx, project, did)
}

// BindDomain is PUT /api/v1/domains/{id}/environment: binds the domain to
// an environment of its project, or moves it there, also before it is
// verified. A bind starts no grace: it clears was_routed and
// dns_moved_since, and has the domain checked at once with its backoff
// reset. Binding to the environment it is bound to changes nothing. 404
// for an environment the project does not have, 409 while it is being
// deleted, 409 superseded for a superseded claim.
func (s *Service) BindDomain(ctx context.Context, project, id, env string) (Domain, error) {
	did, err := parseID(id)
	if err != nil {
		return Domain{}, err
	}
	if env == "" {
		return Domain{}, apierr.New(http.StatusBadRequest, "environment is required: the environment's ID or name")
	}
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		e, err := resolveEnv(ctx, tx, project, env, store.ForShare)
		if err != nil {
			return err
		}
		if e.Deleting {
			return apierr.New(http.StatusConflict, "environment %s is being deleted", e.Name)
		}
		v, err := s.view(ctx, tx, project, did, store.ForUpdate)
		if err != nil {
			return err
		}
		if v.Verification == store.VerificationSuperseded {
			return apierr.WithCode(http.StatusConflict, CodeSuperseded,
				"domain %s was superseded by another claim of %s and cannot be bound; claim it again", did, v.Name)
		}
		if v.EnvID == e.ID {
			return nil
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		after := v.Domain
		after.EnvID, after.BoundAt = e.ID, now
		after.WasRouted, after.DNSMovedSince = false, time.Time{}
		after.CheckBackoffS, after.NextCheckAt = int(backoffMin/time.Second), now
		_, err = s.settle(ctx, tx, v.Domain, after, schedule{}, now)
		return err
	})
	if err != nil {
		return Domain{}, err
	}
	return s.get(ctx, project, did)
}

// UnbindDomain is DELETE /api/v1/domains/{id}/environment: ends the
// binding. An unbound domain stays as it is.
func (s *Service) UnbindDomain(ctx context.Context, project, id string) (Domain, error) {
	did, err := parseID(id)
	if err != nil {
		return Domain{}, err
	}
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		v, err := s.view(ctx, tx, project, did, store.ForUpdate)
		if err != nil || v.EnvID == uuid.Nil {
			return err
		}
		after := v.Domain
		after.EnvID, after.BoundAt = uuid.Nil, time.Time{}
		after.WasRouted, after.DNSMovedSince = false, time.Time{}
		_, err = s.settle(ctx, tx, v.Domain, after, schedule{}, v.Now)
		return err
	})
	if err != nil {
		return Domain{}, err
	}
	return s.get(ctx, project, did)
}

// CheckDomain is POST /api/v1/domains/{id}/check: one check now, its
// lookups bounded by 15 s (a timeout counts as a failed lookup), with an
// unverified claim's backoff reset; then the domain. A superseded claim
// is not checked. The check is not cut when the caller leaves.
func (s *Service) CheckDomain(ctx context.Context, project, id string) (Domain, error) {
	did, err := parseID(id)
	if err != nil {
		return Domain{}, err
	}
	v, err := s.view(ctx, s.st, project, did, store.NoLock)
	if err != nil {
		return Domain{}, err
	}
	if !s.Enabled() {
		return Domain{}, apierr.WithCode(http.StatusConflict, CodeDomainsOff, textDomainsOff)
	}
	if v.Verification != store.VerificationSuperseded {
		err := s.check(context.WithoutCancel(ctx), v.Domain, true, false)
		if errors.Is(err, errGone) {
			return Domain{}, errNoDomain()
		}
		if err != nil {
			return Domain{}, err
		}
	}
	return s.get(ctx, project, did)
}

// DeleteDomain is DELETE /api/v1/domains/{id}: the claim and its binding
// go; the incidents it left open in its environment are closed.
func (s *Service) DeleteDomain(ctx context.Context, project, id string) error {
	did, err := parseID(id)
	if err != nil {
		return err
	}
	var name string
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		v, err := s.view(ctx, tx, project, did, store.ForUpdate)
		if err != nil {
			return err
		}
		name = v.Name
		if v.EnvID != uuid.Nil {
			after := v.Domain
			after.EnvID = uuid.Nil
			if err := s.leaveIncidents(ctx, tx, v.Domain, after); err != nil {
				return err
			}
		}
		_, err = tx.DeleteDomain(ctx, did)
		return err
	})
	if err != nil {
		return err
	}
	s.log.Info("domain deleted", "project", project, "domain", name, "id", did)
	return nil
}

// EnvChanged settles the domains bound to the environment, in tx, the
// transaction that changed its publicity or marked it deleting (the
// environments service's hook): their states follow at once.
func (s *Service) EnvChanged(ctx context.Context, tx *store.Store, envID uuid.UUID) error {
	ids, err := tx.LockDomainsOfEnv(ctx, envID)
	if err != nil {
		return fmt.Errorf("the domains of environment %s: %w", envID, err)
	}
	for _, id := range ids {
		if err := s.refreshIn(ctx, tx, id); err != nil && !errors.Is(err, errGone) {
			return err
		}
	}
	return nil
}

// Refresh derives a domain's state again, in a fenced transaction of its
// own, after ACME kept its certificate or recorded a failed order
// (acme.Refresher); a domain that is gone is no error.
func (s *Service) Refresh(ctx context.Context, id uuid.UUID) error {
	err := s.inTx(ctx, true, func(tx *store.Store) error { return s.refreshIn(ctx, tx, id) })
	if errors.Is(err, errGone) {
		return nil
	}
	return err
}

// refreshIn derives the domain's state again in tx and writes it with what
// follows from it (settle); the table is told when the state changed.
func (s *Service) refreshIn(ctx context.Context, tx *store.Store, id uuid.UUID) error {
	v, err := tx.LockDomainView(ctx, id, store.ForUpdate)
	if errors.Is(err, store.ErrNoRow) {
		return errGone
	}
	if err != nil {
		return fmt.Errorf("domain %s: %w", id, err)
	}
	_, err = s.settle(ctx, tx, v.Domain, v.Domain, schedule{}, v.Now)
	return err
}

// checkDue is the loop LoopCheck: it claims the domains whose check is
// due and checks them, checkParallel at once; each writes its own result.
func (s *Service) checkDue(ctx context.Context) error {
	var due []store.Domain
	err := s.inTx(ctx, true, func(tx *store.Store) error {
		var err error
		due, err = tx.ClaimDueDomains(ctx, checkBatch, checkLease)
		return err
	})
	if err != nil || len(due) == 0 {
		return err
	}
	var g errgroup.Group
	g.SetLimit(checkParallel)
	for _, d := range due {
		g.Go(func() error {
			if err := s.check(ctx, d, false, true); err != nil && !errors.Is(err, errGone) && ctx.Err() == nil {
				s.log.Warn("cannot record a domain's check", "domain", d.Name, "id", d.ID, "err", err)
			}
			return nil
		})
	}
	return g.Wait()
}

// settleAll is the loop LoopSettle: it settles every domain whose stored
// state is no longer what its facts say.
func (s *Service) settleAll(ctx context.Context) error {
	views, err := s.st.DomainViewsToSettle(ctx)
	if err != nil {
		return fmt.Errorf("read the domains to settle: %w", err)
	}
	var errs []error
	for _, v := range views {
		if !s.unsettled(v) {
			continue
		}
		err := s.inTx(ctx, true, func(tx *store.Store) error { return s.refreshIn(ctx, tx, v.ID) })
		if err != nil && !errors.Is(err, errGone) {
			if errors.Is(err, leader.ErrNotLeader) || ctx.Err() != nil {
				return err
			}
			errs = append(errs, fmt.Errorf("domain %s: %w", v.Name, err))
		}
	}
	return errors.Join(errs...)
}

// unsettled reports whether a settle would change the domain. A grace
// that ran out is left to the check, which is due at its end and looks
// first whether the record is right again.
func (s *Service) unsettled(v store.DomainView) bool {
	if v.Verification == store.VerificationVerified {
		ended := func(since time.Time) bool { return !since.IsZero() && !since.Add(s.cfg.Grace).After(v.Now) }
		if ended(v.RecordMissingSince) || (ended(v.DNSMovedSince) && v.State != StateInactive) {
			return false
		}
	}
	state, reason := Derive(FactsOf(v, s.cfg.Grace))
	return state != v.State || reason != v.Reason
}

// The edge's addresses are what --edge-name resolves to, read again every
// edgeTTL (a failed read is tried again after edgeRetry, the last known
// addresses kept meanwhile), and every --edge-address.
const (
	edgeTTL     = 5 * time.Minute
	edgeRetry   = 30 * time.Second
	edgeTimeout = 5 * time.Second
)

// edgeCache keeps what the edge's name resolved to. Its times are the
// instance's: it is a cache of DNS, not a deadline of the platform's.
type edgeCache struct {
	mu           sync.Mutex
	addrs        []netip.Addr
	resolved     time.Time
	tried        time.Time
	resolvedOnce bool
}

// get returns the edge's addresses, resolving the edge's name when what
// it knows is old.
func (c *edgeCache) get(ctx context.Context, dns DNS, cfg Config, log *slog.Logger) []netip.Addr {
	if cfg.EdgeName == "" {
		return sortAddrs(slices.Clone(cfg.EdgeAddresses))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if (!c.resolvedOnce || now.Sub(c.resolved) >= edgeTTL) && now.Sub(c.tried) >= edgeRetry {
		c.tried = now
		rctx, cancel := context.WithTimeout(ctx, edgeTimeout)
		addrs, err := dns.Resolve(rctx, cfg.EdgeName)
		cancel()
		if err != nil {
			log.Warn("cannot resolve the edge's name", "edge", cfg.EdgeName, "err", err)
		} else {
			c.addrs, c.resolved, c.resolvedOnce = addrs, now, true
		}
	}
	return sortAddrs(append(slices.Clone(c.addrs), cfg.EdgeAddresses...))
}
