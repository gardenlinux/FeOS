// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"
	"time"
)

// wakeResend is how often a held request looks again while it waits:
// it asks again to wake what is still asleep (the agent sends one Wake
// per service at a time and drops it while not connected), and finds an
// endpoint whose cooldown ended.
const wakeResend = 5 * time.Second

// holdClass is why a request is held, and so how long it may be: the
// wake bound while instances of its service are asleep or waking, the
// pending bound while they are only pending (D10).
type holdClass int

const (
	noHold holdClass = iota
	pendingHold
	wakeHold
)

// waiter is one request's hold: its class, which only ever grows, and its
// deadline, measured from the request's start.
type waiter struct {
	start    time.Time
	class    holdClass
	deadline time.Time
	// held is its place among the held requests (MaxHeldPerEnv,
	// MaxHeldPerTenant, MaxHeld).
	held slot
}

// raise sets the request's class to c unless it is higher already.
func (w *waiter) raise(c holdClass, cfg Config) {
	if c <= w.class {
		return
	}
	w.class = c
	bound := cfg.PendingHold
	if c == wakeHold {
		bound = cfg.WakeHold
	}
	if d := w.start.Add(bound); d.After(w.deadline) {
		w.deadline = d
	}
}

// enter counts a request of the environment env of tenant among the
// held ones: against its environment's share, MaxHeldPerEnv, its
// tenant's, MaxHeldPerTenant, and against MaxHeld. It returns fits, or
// the cap that is taken.
func (p *Proxy) enter(w *waiter, tenant, env string) full {
	if w.held.counted {
		return fits
	}
	f := p.held.enter(&w.held, tenant, env)
	switch f {
	case fits:
		p.m.heldTotal.Add(1)
	case envFull:
		p.m.rejectedHeldEnv.Add(1)
	case tenantFull:
		p.m.rejectedHeldTenant.Add(1)
	case allFull:
		p.m.rejectedHeld.Add(1)
	}
	return f
}

// leave ends the request's count among the held ones.
func (p *Proxy) leave(w *waiter) {
	p.held.leave(&w.held)
}

// hold is what a request does when no endpoint of its service is usable:
// it wakes what is asleep and waits for the routing to change, within
// its class's bound, or it answers. It reports whether the request goes
// on (to pick again); false after it answered or its client went away.
func (q *request) hold(v *view, tg target, info pickInfo) bool {
	p := q.p
	svc := tg.svc
	if p.draining() {
		reply{http.StatusServiceUnavailable, textDraining, 1}.write(q.w)
		return false
	}
	c := noHold
	switch {
	case svc.asleep+svc.waking > 0:
		c = wakeHold
	case svc.pending > 0 || info.waiting > 0:
		c = pendingHold
	}
	if c == noHold {
		r := reply{http.StatusServiceUnavailable, "no ready instance of " + svc.name, 5}
		if !info.coolUntil.IsZero() {
			// Instances that only cool down after a failed dial: as
			// that dial's answer, the client's connection closes.
			r.writeClosing(q.w)
		} else {
			r.write(q.w)
		}
		return false
	}
	q.wait.raise(c, p.cfg)
	now := time.Now()
	if !now.Before(q.wait.deadline) {
		p.m.holdTimeouts.Add(1)
		if q.wait.class == wakeHold {
			reply{http.StatusServiceUnavailable, "no instance of " + svc.name + " woke in time, try again", 10}.write(q.w)
		} else {
			reply{http.StatusServiceUnavailable, "no ready instance of " + svc.name, 5}.write(q.w)
		}
		return false
	}
	if svc.asleep > 0 && p.d.Waker != nil {
		p.m.wakes.Add(1)
		p.d.Waker.Wake(tg.env.id, svc.name)
	}
	switch p.enter(&q.wait, tg.env.tenant, tg.env.id) {
	case envFull, tenantFull:
		reply{http.StatusServiceUnavailable, textTooManyHere, 1}.write(q.w)
		return false
	case allFull:
		reply{http.StatusServiceUnavailable, textTooMany, 1}.write(q.w)
		return false
	}
	wait := min(q.wait.deadline.Sub(now), wakeResend)
	if !info.coolUntil.IsZero() {
		wait = min(wait, max(info.coolUntil.Sub(now), time.Millisecond))
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-v.changed:
		return true
	case <-timer.C:
		return true
	case <-q.r.Context().Done():
		q.w.aborted = true
		return false
	case <-p.drain:
		reply{http.StatusServiceUnavailable, textDraining, 1}.write(q.w)
		return false
	}
}
