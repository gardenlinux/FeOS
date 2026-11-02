// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package acme

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	xacme "golang.org/x/crypto/acme"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

// LoopName names the leader's loop of orders; Hint is the ferun_leader
// hint that wakes it (a domain that became due, say).
const (
	LoopName = "acme-orders"
	Hint     = "acme"
)

// ConditionCertificateFailing is the incident of a served domain whose
// renewals have failed for FailingFor.
const ConditionCertificateFailing = "certificate_failing"

// The issuer's timings unless a test sets others.
const (
	// DefaultInterval is how often the loop runs.
	DefaultInterval = 30 * time.Second
	// DefaultOrderTimeout bounds one order, from its claim to its end.
	DefaultOrderTimeout = 5 * time.Minute
	// DefaultChallengeTTL is how long a challenge stays in the table at
	// most, whatever the order did.
	DefaultChallengeTTL = 10 * time.Minute
	// DefaultDeliveryWait bounds the wait for the ingress instances to
	// hold a challenge before it is accepted.
	DefaultDeliveryWait = 30 * time.Second
	// DefaultFailingFor is how long a served domain's renewals fail
	// before its environment gets the incident.
	DefaultFailingFor = 24 * time.Hour
)

const (
	// liveIngress is how recently an ingress instance was seen to count
	// as connected (its stream's heartbeat is every 10 s).
	liveIngress = 45 * time.Second
	// deliveryPoll is how often the wait for the ingress reads again.
	deliveryPoll = 100 * time.Millisecond
	// claimMargin is how much longer than the order's bound a claim
	// holds the domain.
	claimMargin = time.Minute
	// maxOrdersPerRun bounds the orders of one run of the loop; the next
	// run goes on.
	maxOrdersPerRun = 25
	// maxIncidentsPerRun bounds the incidents one run opens.
	maxIncidentsPerRun = 64
	// The bounds of the wait after the account could not be had.
	accountBackoffMin = time.Minute
	accountBackoffMax = 30 * time.Minute
	// fencedLockTimeout and fencedStmtTimeout bound the loop's fenced
	// transactions.
	fencedLockTimeout = time.Second
	fencedStmtTimeout = 3 * time.Second
	// httpTimeout bounds one request to the CA.
	httpTimeout = 30 * time.Second
	// maxErrorText bounds issue_error.
	maxErrorText = 1024
)

// Refresher re-derives a domain's state and reason after an order ended,
// successful or not (the domains core implements it): in a transaction of
// its own, notifying the ingress's table when the state changed.
type Refresher interface {
	Refresh(ctx context.Context, domainID uuid.UUID) error
}

// NoRefresh is a Refresher that does nothing: the wiring's stand-in while
// no domains core is there.
type NoRefresh struct{}

// Refresh does nothing.
func (NoRefresh) Refresh(context.Context, uuid.UUID) error { return nil }

// Sealer seals and opens under --secret-key-file's key (seal.Sealer).
type Sealer interface {
	Seal(plaintext, aad []byte) ([]byte, error)
	Open(sealed, aad []byte) ([]byte, error)
}

// Fencer fences a leader's transaction (leader.Elector).
type Fencer interface {
	Fence(ctx context.Context, q db.Querier) error
}

// Config is the issuer's configuration.
type Config struct {
	// Directory is the ACME directory's URL (--acme-directory); empty:
	// nothing is ordered, and the loop only expires challenges.
	Directory string
	// Email is the account's contact (--acme-email); empty: none.
	Email string
	// Profile is the certificate profile the orders ask for
	// (--acme-profile); empty: the CA's default.
	Profile string
	// HTTPClient talks to the CA; nil: one with a timeout per request.
	HTTPClient *http.Client
	// UserAgent is prepended to the requests' User-Agent.
	UserAgent string
	// The timings; zero: the defaults.
	Interval, OrderTimeout, ChallengeTTL, DeliveryWait, FailingFor time.Duration
	Log                                                            *slog.Logger
}

// Deps is what the issuer uses.
type Deps struct {
	Store     *store.Store
	Sealer    Sealer
	Elector   Fencer
	Refresher Refresher
}

// Issuer obtains and renews the domains' certificates.
type Issuer struct {
	cfg Config
	d   Deps
	log *slog.Logger

	// mu guards the account's client and its backoff; the loop runs one
	// at a time, the lock is for tests reading them.
	mu        sync.Mutex
	client    *xacme.Client
	accountAt time.Time     // no account is tried before this
	accountBk time.Duration // the wait after the next failure
}

// New returns the issuer. With a directory it needs a Sealer, which
// seals the keys.
func New(cfg Config, d Deps) (*Issuer, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.OrderTimeout <= 0 {
		cfg.OrderTimeout = DefaultOrderTimeout
	}
	if cfg.ChallengeTTL <= 0 {
		cfg.ChallengeTTL = DefaultChallengeTTL
	}
	if cfg.DeliveryWait <= 0 {
		cfg.DeliveryWait = DefaultDeliveryWait
	}
	if cfg.FailingFor <= 0 {
		cfg.FailingFor = DefaultFailingFor
	}
	if cfg.HTTPClient == nil {
		t, _ := http.DefaultTransport.(*http.Transport)
		var rt http.RoundTripper = http.DefaultTransport
		if t != nil {
			rt = t.Clone()
		}
		cfg.HTTPClient = &http.Client{Timeout: httpTimeout, Transport: rt}
	}
	if d.Store == nil {
		return nil, errors.New("acme: no store")
	}
	if d.Refresher == nil {
		d.Refresher = NoRefresh{}
	}
	if cfg.Directory != "" && d.Sealer == nil {
		return nil, errors.New("--acme-directory needs --secret-key-file: the certificates' keys are sealed under it")
	}
	return &Issuer{cfg: cfg, d: d, log: cfg.Log.With("component", "acme"), accountBk: accountBackoffMin}, nil
}

// Loops returns the leader's loop of orders.
func (s *Issuer) Loops() []model.Loop {
	return []model.Loop{{Name: LoopName, Interval: s.cfg.Interval, Run: s.Run}}
}

// Run is one run of the loop: expired challenges are deleted, failing
// renewals get their incidents, and with a directory every due domain is
// ordered, at most maxOrdersPerRun.
func (s *Issuer) Run(ctx context.Context) error {
	if n, err := s.d.Store.ExpireACMEChallenges(ctx); err != nil {
		return fmt.Errorf("expire the ACME challenges: %w", err)
	} else if n > 0 {
		s.log.Info("acme: expired challenges deleted", "challenges", n)
	}
	if s.cfg.Directory == "" {
		return nil
	}
	if err := s.openIncidents(ctx); err != nil {
		return err
	}
	cl, err := s.account(ctx)
	if err != nil || cl == nil {
		return err
	}
	for range maxOrdersPerRun {
		var o store.ACMEOrder
		var ok bool
		err := s.fenced(ctx, func(tx *store.Store) error {
			var err error
			o, ok, err = tx.ClaimACMEOrder(ctx, s.cfg.OrderTimeout+claimMargin)
			return err
		})
		if err != nil || !ok {
			return err
		}
		s.runOrder(ctx, cl, o)
		if ctx.Err() != nil {
			return nil
		}
	}
	return s.openIncidents(ctx)
}

// runOrder runs the order of o and records its end, then has the core
// refresh the domain's state.
func (s *Issuer) runOrder(ctx context.Context, cl *xacme.Client, o store.ACMEOrder) {
	log := s.log.With("domain", o.Name, "domain_id", o.DomainID, "environment", o.EnvID, "renewal", o.Renewal)
	log.Info("acme: ordering a certificate")
	octx, cancel := context.WithTimeout(ctx, s.cfg.OrderTimeout)
	got, err := s.order(octx, cl, o)
	cancel()
	if ctx.Err() != nil {
		// The term ended: the claim holds the domain until a leader
		// orders again, and the challenge expires.
		log.Info("acme: an order was cut off by the end of the leader's term")
		return
	}
	if err != nil {
		text, wait := describe(o.Name, err, s.cfg.OrderTimeout)
		d, ferr := s.recordFailure(ctx, o, text, wait)
		if ferr != nil {
			log.Warn("acme: cannot record a failed order", "err", ferr, "order_err", err)
		} else {
			log.Warn("acme: an order failed", "err", err, "issue_error", text, "next_issue_at", d.NextIssueAt,
				"failing_since", d.IssueFailingSince)
		}
	} else if kept, err := s.recordSuccess(ctx, o, got); err != nil {
		log.Warn("acme: cannot keep an issued certificate", "err", err)
	} else if kept {
		log.Info("acme: certificate issued", "serial", got.serial, "not_after", got.notAfter, "renew_after", got.renewAfter)
	} else {
		log.Info("acme: certificate issued for a domain no longer bound or verified; not kept", "serial", got.serial)
	}
	if err := s.d.Refresher.Refresh(ctx, o.DomainID); err != nil && ctx.Err() == nil {
		log.Warn("acme: cannot refresh the domain's state", "err", err)
	}
}

// recordSuccess keeps the certificate an order obtained, unless its
// domain is gone, no longer verified or unbound; it reports whether it
// kept it.
func (s *Issuer) recordSuccess(ctx context.Context, o store.ACMEOrder, got *issued) (bool, error) {
	kept := false
	err := s.fenced(ctx, func(tx *store.Store) error {
		kept = false
		d, err := tx.LockACMEDomain(ctx, o.DomainID)
		if errors.Is(err, store.ErrNoRow) {
			return nil
		}
		if err != nil {
			return err
		}
		if d.Verification != "verified" || d.EnvID == uuid.Nil {
			_, err := tx.DeleteACMEChallenges(ctx, d.ID)
			return err
		}
		sealed, err := s.d.Sealer.Seal(got.key, store.DomainCertAAD(d.ID))
		if err != nil {
			return fmt.Errorf("seal the key: %w", err)
		}
		if err := tx.ACMEOrderSucceeded(ctx, d, store.DomainCert{DomainID: d.ID, Chain: got.chain, KeySealed: sealed,
			NotBefore: got.notBefore, NotAfter: got.notAfter, RenewAfter: got.renewAfter, Serial: got.serial},
			ConditionCertificateFailing); err != nil {
			return err
		}
		kept = true
		return nil
	})
	return kept, err
}

// recordFailure records a failed order of o with text and the CA's wait.
func (s *Issuer) recordFailure(ctx context.Context, o store.ACMEOrder, text string, wait time.Duration) (store.ACMEDomain, error) {
	var out store.ACMEDomain
	err := s.fenced(ctx, func(tx *store.Store) error {
		d, err := tx.LockACMEDomain(ctx, o.DomainID)
		if errors.Is(err, store.ErrNoRow) {
			return nil
		}
		if err != nil {
			return err
		}
		out, err = tx.ACMEOrderFailed(ctx, d, text, wait)
		return err
	})
	return out, err
}

// openIncidents opens certificate_failing on the environments of the
// served domains whose renewals have failed for FailingFor.
func (s *Issuer) openIncidents(ctx context.Context) error {
	failing, err := s.d.Store.ACMERenewalsFailing(ctx, s.cfg.FailingFor, ConditionCertificateFailing, maxIncidentsPerRun)
	if err != nil {
		return err
	}
	for _, f := range failing {
		summary := fmt.Sprintf("the certificate of %s has not been renewed since %s: %s; it expires at %s",
			f.Name, f.FailingSince.UTC().Format(time.RFC3339), f.IssueError, f.NotAfter.UTC().Format(time.RFC3339))
		var opened bool
		err := s.fenced(ctx, func(tx *store.Store) error {
			var err error
			_, opened, err = tx.OpenIncident(ctx, store.EnvIncident{EnvID: f.EnvID, Object: store.DomainObject(f.Name),
				Deployment: f.EnvDeployment, Condition: ConditionCertificateFailing, Summary: summary})
			return err
		})
		if err != nil {
			return fmt.Errorf("open the incident of %s: %w", f.Name, err)
		}
		if opened {
			s.log.Warn("acme: incident opened: a certificate's renewals keep failing", "domain", f.Name,
				"environment", f.EnvID, "failing_since", f.FailingSince, "not_after", f.NotAfter)
		}
	}
	return nil
}

// fenced runs fn in a transaction fenced by the leader's lease.
func (s *Issuer) fenced(ctx context.Context, fn func(tx *store.Store) error) error {
	return s.d.Store.InTx(ctx, func(tx *store.Store) error {
		if err := tx.SetLocalTimeouts(ctx, fencedLockTimeout, fencedStmtTimeout); err != nil {
			return err
		}
		if s.d.Elector != nil {
			if err := s.d.Elector.Fence(ctx, tx.Q()); err != nil {
				return err
			}
		}
		return fn(tx)
	})
}
