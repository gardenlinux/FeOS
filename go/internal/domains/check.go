// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package domains

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The checks' timings (the shared contract).
const (
	// backoffMin and backoffMax bound an unverified or lapsed claim's
	// backoff, which doubles after every check.
	backoffMin = 60 * time.Second
	backoffMax = 3600 * time.Second
	// verifiedEvery is a verified domain's interval; graceEvery its
	// interval while a grace runs.
	verifiedEvery = time.Hour
	graceEvery    = 15 * time.Minute
	// checkTimeout bounds one check's lookups; a lookup still waiting
	// then has failed.
	checkTimeout = 15 * time.Second
)

// result is what one check's lookups found, judged.
type result struct {
	// txtOK: the TXT lookup answered, with txt.
	txtOK bool
	txt   []string
	// routeOK: the routing lookup answered, and the name is routed or
	// not.
	routeOK bool
	routed  bool
	cname   []string
	addrs   []string
	// errText is the failed lookups' errors; "" when every one answered.
	errText string
}

// judge turns a lookup's answer into a result: the name is routed when it
// has addresses and every one is an edge address, whether it gets them
// through a CNAME, an ALIAS or A and AAAA records of its own. With no
// edge address known, addresses decide nothing, and the routing lookup
// counts as failed.
func judge(a Answer, edgeName string, edge []netip.Addr) result {
	var r result
	var errs []string
	if a.TXTErr == nil {
		r.txtOK, r.txt = true, nonNilStrings(a.TXT)
	} else {
		errs = append(errs, "TXT: "+a.TXTErr.Error())
	}
	switch {
	case a.RouteErr != nil:
		errs = append(errs, "routing: "+a.RouteErr.Error())
	case len(a.Addresses) == 0:
		r.routeOK = true
	case len(edge) == 0:
		errs = append(errs, fmt.Sprintf("routing: the edge's addresses are not known (%s resolves to none, and no --edge-address is set)",
			edgeName))
	default:
		r.routeOK, r.routed = true, true
		for _, ad := range a.Addresses {
			if !slices.Contains(edge, ad.Unmap()) {
				r.routed = false
			}
		}
	}
	if r.routeOK {
		r.cname, r.addrs = nonNilStrings(a.CNAME), addrStrings(a.Addresses)
	}
	r.errText = strings.Join(errs, "; ")
	return r
}

// errGone is a check of a domain deleted since it was claimed.
var errGone = errors.New("the domain is gone")

// check runs one check of domain d now: its lookups, outside any
// transaction and bounded by checkTimeout, then the result written in a
// transaction of its own (record). manual is POST …/check's, which
// resets an unverified claim's backoff; fenced writes as the leader.
func (s *Service) check(ctx context.Context, d store.Domain, manual, fenced bool) error {
	lctx, cancel := context.WithTimeout(ctx, checkTimeout)
	edge := s.edge.get(lctx, s.dns, s.cfg, s.log)
	a := s.dns.Lookup(lctx, Query{Name: d.Name, TXTName: s.txtName(d.Name)})
	cancel()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	r := judge(a, s.cfg.EdgeName, edge)
	return s.record(ctx, d.ID, r, manual, fenced)
}

// claims are the claims of one name in one transaction, locked, as read
// (before) and as they are to be written (after), oldest first.
type claims struct {
	before []store.Domain
	after  []*store.Domain
}

func (c *claims) get(id uuid.UUID) *store.Domain {
	for _, d := range c.after {
		if d.ID == id {
			return d
		}
	}
	return nil
}

func (c *claims) verified() *store.Domain {
	for _, d := range c.after {
		if d.Verification == store.VerificationVerified {
			return d
		}
	}
	return nil
}

// record writes a check's result r of the domain id: in one transaction
// that locks every claim of its name, it decides the proof (verify,
// supersede, a TXT grace's start and end), the routing (routed, a
// routing grace's start and end) and the next check, and settles every
// claim it changed.
func (s *Service) record(ctx context.Context, id uuid.UUID, r result, manual, fenced bool) error {
	return s.inTx(ctx, fenced, func(tx *store.Store) error {
		d0, err := tx.GetDomain(ctx, id)
		if errors.Is(err, store.ErrNoRow) {
			return errGone
		}
		if err != nil {
			return err
		}
		rows, err := tx.LockDomainsOfName(ctx, d0.Name)
		if err != nil {
			return fmt.Errorf("lock the claims of %s: %w", d0.Name, err)
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		c := &claims{before: rows}
		for _, row := range rows {
			cp := row
			c.after = append(c.after, &cp)
		}
		d := c.get(id)
		if d == nil {
			return errGone
		}
		if d.Verification == store.VerificationSuperseded {
			return nil // not checked
		}
		if r.txtOK {
			s.decideProof(c, d, r.txt, now)
		}
		if r.routeOK {
			decideRouting(d, r.routed, now)
		}
		noteLookup(d, r, now)
		return s.settleClaims(ctx, tx, c, id, manual, now)
	})
}

// decideProof applies what the TXT record shows to the claims of a name,
// for the checked claim d. A claim whose token it shows is proven:
//
//   - d verified and its token there: its TXT grace, if one ran, ends;
//   - d verified, its token gone, and the token of an unverified or lapsed
//     claim there: that claim (the oldest of them) is verified and d
//     superseded, at once;
//   - d verified, its token gone and no other: its TXT grace starts;
//   - d unverified or lapsed and its token there, while no claim of the
//     name is verified or the verified one's token is gone: the oldest
//     unverified or lapsed claim whose token is there (d, or an older
//     one) is verified, and the verified one, if any, superseded.
//
// If the record shows the tokens of the verified claim and of others, the
// verified one stays so.
func (s *Service) decideProof(c *claims, d *store.Domain, txt []string, now time.Time) {
	shown := func(x *store.Domain) bool { return slices.Contains(txt, x.Token) }
	var candidates []*store.Domain
	for _, x := range c.after {
		if (x.Verification == store.VerificationUnverified || x.Verification == store.VerificationLapsed) && shown(x) {
			candidates = append(candidates, x)
		}
	}
	v := c.verified()
	switch d.Verification {
	case store.VerificationVerified:
		switch {
		case shown(d):
			d.RecordMissingSince = time.Time{}
		case len(candidates) > 0:
			supersede(d, candidates[0])
			verify(candidates[0], d, now)
		case d.RecordMissingSince.IsZero():
			d.RecordMissingSince = now
		}
	case store.VerificationUnverified, store.VerificationLapsed:
		if !shown(d) || (v != nil && shown(v)) {
			return
		}
		w := candidates[0] // d is among them
		if v != nil {
			supersede(v, w)
		}
		verify(w, d, now)
	}
}

// verify makes w the verified claim of its name; a claim other than the
// checked one, d, is checked at once, for its routing.
func verify(w, d *store.Domain, now time.Time) {
	w.Verification, w.VerifiedAt, w.SupersededBy = store.VerificationVerified, now, uuid.Nil
	w.RecordMissingSince = time.Time{}
	w.CheckBackoffS = int(backoffMin / time.Second)
	if w.ID != d.ID {
		w.NextCheckAt = now
	}
}

// supersede takes v's name from it for w: v's binding and graces end;
// settle opens the incident.
func supersede(v, w *store.Domain) {
	v.Verification, v.SupersededBy = store.VerificationSuperseded, w.ID
	v.EnvID, v.BoundAt, v.WasRouted = uuid.Nil, time.Time{}, false
	v.RecordMissingSince, v.DNSMovedSince = time.Time{}, time.Time{}
}

// decideRouting applies a routing answer to d: routed or not, and for a
// bound domain whether it has been routed since it was bound; a verified
// one that was and is not now starts its routing grace, one routed again
// ends it.
func decideRouting(d *store.Domain, routed bool, now time.Time) {
	d.Routed = routed
	if d.EnvID == uuid.Nil {
		return
	}
	switch {
	case routed:
		d.WasRouted, d.DNSMovedSince = true, time.Time{}
	case d.WasRouted && d.Verification == store.VerificationVerified && d.DNSMovedSince.IsZero():
		d.DNSMovedSince = now
	}
}

// noteLookup records what the lookups found and whether they answered:
// a part that failed keeps what it found before, and sets check_error,
// with check_failing_since from its first failure.
func noteLookup(d *store.Domain, r result, now time.Time) {
	if r.txtOK {
		d.Found.TXT = r.txt
	}
	if r.routeOK {
		d.Found.CNAME, d.Found.Addresses = r.cname, r.addrs
	}
	if r.txtOK || r.routeOK {
		d.CheckedAt = now
	}
	d.CheckError = r.errText
	switch {
	case r.errText == "":
		d.CheckFailingSince = time.Time{}
	case d.CheckFailingSince.IsZero():
		d.CheckFailingSince = now
	}
}

// settleClaims settles the claims of a name that changed, the checked
// one (checked) rescheduled: the ones no longer verified first, so that
// the name never has two verified claims at once (domains_verified_name).
func (s *Service) settleClaims(ctx context.Context, tx *store.Store, c *claims, checked uuid.UUID, manual bool,
	now time.Time) error {
	order := make([]int, 0, len(c.after))
	for i := range c.after {
		order = append(order, i)
	}
	slices.SortStableFunc(order, func(a, b int) int {
		va := c.after[a].Verification == store.VerificationVerified
		vb := c.after[b].Verification == store.VerificationVerified
		switch {
		case va == vb:
			return 0
		case vb:
			return -1
		}
		return 1
	})
	for _, i := range order {
		before, after := c.before[i], *c.after[i]
		sched := schedule{}
		if after.ID == checked {
			sched = schedule{checked: true, manual: manual}
		} else if domainEqual(before, after) {
			continue
		}
		if _, err := s.settle(ctx, tx, before, after, sched, now); err != nil {
			return err
		}
	}
	return nil
}

// domainEqual reports whether a write of after over before would change
// nothing.
func domainEqual(a, b store.Domain) bool {
	return a.EnvID == b.EnvID && a.BoundAt.Equal(b.BoundAt) && a.Verification == b.Verification &&
		a.VerifiedAt.Equal(b.VerifiedAt) && a.SupersededBy == b.SupersededBy && a.NextCheckAt.Equal(b.NextCheckAt) &&
		a.CheckBackoffS == b.CheckBackoffS && a.CheckedAt.Equal(b.CheckedAt) && a.CheckError == b.CheckError &&
		a.CheckFailingSince.Equal(b.CheckFailingSince) && a.Found.Equal(b.Found) &&
		a.Routed == b.Routed && a.WasRouted == b.WasRouted && a.RecordMissingSince.Equal(b.RecordMissingSince) &&
		a.DNSMovedSince.Equal(b.DNSMovedSince) && a.State == b.State && a.Reason == b.Reason
}

// schedule says whether a settle reschedules the domain's next check: a
// check's (checked), and a manual one's (POST …/check), which resets the
// backoff.
type schedule struct{ checked, manual bool }

// envInfo is what settle needs of an environment: its row, or none.
type envInfo struct {
	ok  bool
	env store.Environment
}

func (s *Service) envOf(ctx context.Context, tx *store.Store, id uuid.UUID) (envInfo, error) {
	if id == uuid.Nil {
		return envInfo{}, nil
	}
	e, err := tx.GetEnvironment(ctx, id)
	if errors.Is(err, store.ErrNoRow) {
		return envInfo{}, nil
	}
	if err != nil {
		return envInfo{}, fmt.Errorf("environment %s: %w", id, err)
	}
	return envInfo{ok: true, env: e}, nil
}

// settle writes after over before, the domain's row as read under its
// lock in tx at the database's time now, after bringing it in line with
// the rules every writer keeps:
//
//   - a verified domain whose TXT grace ran out lapses;
//   - its state and reason are derived (Derive) from the row and its
//     environment;
//   - the next check is scheduled when sched says so;
//   - the incidents of its environments follow: domain_record_wrong while
//     a grace runs, domain_deactivated when one ran out,
//     domain_superseded when another claim took the name; those of an
//     environment the domain left are closed.
//
// The write sends ferun_ingress for the environments when the binding or
// the state changed (UpdateDomain), and the deletions theirs.
func (s *Service) settle(ctx context.Context, tx *store.Store, before, after store.Domain, sched schedule,
	now time.Time) (store.Domain, error) {
	if after.Verification == store.VerificationVerified && !after.RecordMissingSince.IsZero() &&
		!after.RecordMissingSince.Add(s.cfg.Grace).After(now) {
		after.Verification = store.VerificationLapsed
	}
	env, err := s.envOf(ctx, tx, after.EnvID)
	if err != nil {
		return store.Domain{}, err
	}
	if !env.ok {
		after.EnvID, after.BoundAt = uuid.Nil, time.Time{} // its environment is gone
	}
	iss, err := tx.GetDomainIssuance(ctx, after.ID)
	if err != nil {
		return store.Domain{}, err
	}
	after.State, after.Reason = Derive(Facts{Verification: after.Verification, Bound: after.EnvID != uuid.Nil,
		EnvPublic: env.env.Public, EnvDeleting: env.env.Deleting, DNSMovedSince: after.DNSMovedSince, Grace: s.cfg.Grace,
		Now: now, Routed: after.Routed, CertValid: iss.CertValid(now), IssueError: iss.IssueError})
	if sched.checked {
		s.reschedule(&after, sched.manual, now)
	}
	if err := s.incidents(ctx, tx, before, after, env); err != nil {
		return store.Domain{}, err
	}
	out, err := tx.UpdateDomain(ctx, before, after)
	if err != nil {
		return store.Domain{}, err
	}
	if before.State != out.State || before.EnvID != out.EnvID || before.Verification != out.Verification {
		s.log.Info("domain changed", "tenant", out.Owner, "domain", out.Name, "id", out.ID,
			"verification", out.Verification, "state", out.State, "environment", out.EnvID, "reason", out.Reason)
	}
	return out, nil
}

// reschedule sets a checked domain's next check: an unverified or lapsed
// claim's after its backoff, which doubles up to an hour (a manual check
// starts it again at a minute); a verified one's in an hour, or while a
// grace runs every quarter of an hour and at the grace's end.
func (s *Service) reschedule(d *store.Domain, manual bool, now time.Time) {
	switch d.Verification {
	case store.VerificationVerified:
		next := now.Add(verifiedEvery)
		if d.State != StateInactive {
			for _, since := range []time.Time{d.RecordMissingSince, d.DNSMovedSince} {
				if since.IsZero() {
					continue
				}
				next = minTime(next, now.Add(graceEvery))
				if end := since.Add(s.cfg.Grace); end.After(now) {
					next = minTime(next, end)
				}
			}
		}
		d.NextCheckAt = next
		d.CheckBackoffS = int(backoffMin / time.Second)
	case store.VerificationUnverified, store.VerificationLapsed:
		b := time.Duration(d.CheckBackoffS) * time.Second
		if manual || b < backoffMin {
			b = backoffMin
		}
		d.NextCheckAt = now.Add(b)
		d.CheckBackoffS = int(min(2*b, backoffMax) / time.Second)
	}
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// object is a domain's object in its environment's incidents.
func object(name string) string { return "domain/" + name }

// graceRuns reports whether a grace of d runs, in its state: a verified
// domain's record answers wrong and it is not deactivated yet.
func graceRuns(d store.Domain) bool {
	return d.Verification == store.VerificationVerified &&
		(!d.RecordMissingSince.IsZero() || (!d.DNSMovedSince.IsZero() && d.State != StateInactive))
}

// incidents keeps the incidents of the domain's environments in line
// with the change from before to after (settle's last rule). env is
// after's environment.
func (s *Service) incidents(ctx context.Context, tx *store.Store, before, after store.Domain, env envInfo) error {
	obj := object(after.Name)
	if before.EnvID != uuid.Nil && before.EnvID != after.EnvID {
		if err := s.leaveIncidents(ctx, tx, before, after); err != nil {
			return err
		}
	}
	if !env.ok {
		return nil
	}
	moved := before.EnvID != after.EnvID
	switch {
	case graceRuns(after):
		started := moved || !graceRuns(before) ||
			(before.RecordMissingSince.IsZero() && !after.RecordMissingSince.IsZero()) ||
			(before.DNSMovedSince.IsZero() && !after.DNSMovedSince.IsZero())
		if started {
			if err := s.openIncident(ctx, tx, env.env, obj, model.ConditionDomainRecordWrong, s.wrongSummary(after)); err != nil {
				return err
			}
		}
	case graceRuns(before) && !moved:
		if _, err := tx.EnvCtlCloseObjectIncidents(ctx, env.env.ID, obj, model.ConditionDomainRecordWrong); err != nil {
			return fmt.Errorf("close incidents of %s: %w", obj, err)
		}
	}
	switch {
	case deactivated(after.State) && (moved || !deactivated(before.State)):
		if err := s.openIncident(ctx, tx, env.env, obj, model.ConditionDomainDeactivated, s.deactivatedSummary(after)); err != nil {
			return err
		}
	case !deactivated(after.State) && deactivated(before.State) && !moved:
		if _, err := tx.EnvCtlCloseObjectIncidents(ctx, env.env.ID, obj, model.ConditionDomainDeactivated); err != nil {
			return fmt.Errorf("close incidents of %s: %w", obj, err)
		}
	}
	return nil
}

// leaveIncidents closes the grace's and the deactivation's incidents of
// the environment the domain left (before's), and opens
// domain_superseded there when another claim took the name.
func (s *Service) leaveIncidents(ctx context.Context, tx *store.Store, before, after store.Domain) error {
	old, err := s.envOf(ctx, tx, before.EnvID)
	if err != nil || !old.ok {
		return err
	}
	obj := object(after.Name)
	for _, cond := range []string{model.ConditionDomainRecordWrong, model.ConditionDomainDeactivated} {
		if _, err := tx.EnvCtlCloseObjectIncidents(ctx, old.env.ID, obj, cond); err != nil {
			return fmt.Errorf("close incidents of %s: %w", obj, err)
		}
	}
	if after.Verification == store.VerificationSuperseded && before.Verification != store.VerificationSuperseded {
		summary := fmt.Sprintf("the TXT record %s shows the token of another claim of %s and not this one's: "+
			"that claim took the name, and the binding ended", s.txtName(after.Name), after.Name)
		return s.openIncident(ctx, tx, old.env, obj, model.ConditionDomainSuperseded, summary)
	}
	return nil
}

// openIncident opens an incident of condition on the environment's
// object, on its latest deployment, or, while one is open (of any deployment),
// writes the new summary into it.
func (s *Service) openIncident(ctx context.Context, tx *store.Store, e store.Environment, obj, condition, summary string) error {
	open, ok, err := tx.OpenIncidentOf(ctx, e.ID, obj, condition)
	if err != nil {
		return fmt.Errorf("incidents of %s: %w", obj, err)
	}
	if ok {
		if open.Summary == summary {
			return nil
		}
		after := open
		after.Summary = summary
		_, err := tx.UpdateIncident(ctx, open, after)
		return err
	}
	if _, _, err := tx.OpenIncident(ctx, store.EnvIncident{EnvID: e.ID, Object: obj, Deployment: e.Deployment,
		Condition: condition, Summary: summary}); err != nil {
		return err
	}
	s.log.Info("incident opened", "environment", e.ID, "object", obj, "condition", condition, "summary", summary)
	return nil
}

// wrongSummary says which record of d answers wrong since when, and when
// d is deactivated for it.
func (s *Service) wrongSummary(d store.Domain) string {
	var parts []string
	if !d.RecordMissingSince.IsZero() {
		parts = append(parts, fmt.Sprintf("the TXT record %s has not shown the claim's token since %s",
			s.txtName(d.Name), rfc3339(d.RecordMissingSince)))
	}
	if !d.DNSMovedSince.IsZero() {
		parts = append(parts, fmt.Sprintf("%s has resolved elsewhere than %s since %s", d.Name, s.cfg.EdgeName,
			rfc3339(d.DNSMovedSince)))
	}
	return strings.Join(parts, "; ") + fmt.Sprintf("; %s is deactivated at %s unless it is right again", d.Name,
		rfc3339(deactivatesAt(d, s.cfg.Grace)))
}

// deactivatedSummary says why d was deactivated.
func (s *Service) deactivatedSummary(d store.Domain) string {
	if d.State == StateLapsed {
		return fmt.Sprintf("the TXT record %s did not show the claim's token for %s (since %s): the claim of %s lapsed",
			s.txtName(d.Name), s.cfg.Grace, rfc3339(d.RecordMissingSince), d.Name)
	}
	return fmt.Sprintf("%s resolved elsewhere than %s for %s (since %s): it is inactive until it resolves to the edge again",
		d.Name, s.cfg.EdgeName, s.cfg.Grace, rfc3339(d.DNSMovedSince))
}
