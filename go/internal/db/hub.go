// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// subscriberBuffer is how many payloads a subscriber may fall behind before
// it gets "" instead of the ones that do not fit. Its channel holds one more
// slot, kept free for that "".
const subscriberBuffer = 256

// listenerIdle is how long the listener waits for a notification before it
// checks its connection with a statement.
const listenerIdle = 15 * time.Second

// hub fans the listener's notifications out to in-process subscribers.
type hub struct {
	mu   sync.Mutex
	subs map[string]map[*subscriber]struct{}
}

type subscriber struct {
	ch chan string // subscriberBuffer + 1 deep
	// overflowed is set while the "" for lost payloads is the newest one
	// queued; guarded by hub.mu.
	overflowed bool
}

// send delivers payload without blocking; the caller holds hub.mu. The
// payloads already queued are kept. A subscriber whose subscriberBuffer
// slots are full loses payload and gets one "" (re-read everything) in the
// reserved last slot instead, which also covers every later payload lost
// before it reads again. Payloads such as ferun_cancel's cannot be re-read,
// so only those that do not fit are lost.
func (s *subscriber) send(payload string) {
	if len(s.ch) < subscriberBuffer {
		select {
		case s.ch <- payload:
			s.overflowed = false
		default:
		}
		return
	}
	if s.overflowed {
		return
	}
	select {
	case s.ch <- "":
		s.overflowed = true
	default:
	}
}

func (h *hub) deliver(channel, payload string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs[channel] {
		s.send(payload)
	}
}

// reset tells every subscriber of every channel to re-read.
func (h *hub) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, subs := range h.subs {
		for s := range subs {
			s.send("")
		}
	}
}

// Subscribe returns the payloads of channel's notifications, buffered, and
// the function that ends the subscription. The payload "" means "you may
// have missed something; re-read": it comes after the listener (re)connects
// and after the payloads already buffered, in place of those a full buffer
// had no room for. The channel is never closed; a subscriber stops reading
// after calling the function.
func (d *DB) Subscribe(channel string) (<-chan string, func()) {
	s := &subscriber{ch: make(chan string, subscriberBuffer+1)}
	d.hub.mu.Lock()
	m := d.hub.subs[channel]
	if m == nil {
		m = map[*subscriber]struct{}{}
		d.hub.subs[channel] = m
	}
	m[s] = struct{}{}
	d.hub.mu.Unlock()
	var once sync.Once
	return s.ch, func() {
		once.Do(func() {
			d.hub.mu.Lock()
			delete(d.hub.subs[channel], s)
			d.hub.mu.Unlock()
		})
	}
}

// RunListener holds the instance's one LISTEN connection until ctx ends,
// and returns nil then. It listens to every channel of Channels and hands
// each notification to the subscribers of its channel. A lost connection is
// recorded as a failure (LastFailure) and replaced with backoff. After every
// connection, the first included (a subscriber may have subscribed before
// it), every subscriber gets "".
func (d *DB) RunListener(ctx context.Context) error {
	backoff := 100 * time.Millisecond
	for {
		conn, err := d.listen(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			d.noteFailure()
			d.log.Warn("database listener cannot connect", "err", err, "retry_in", backoff)
			if !sleep(ctx, backoff) {
				return nil
			}
			backoff = min(backoff*2, 5*time.Second)
			continue
		}
		backoff = 100 * time.Millisecond
		// Tell the subscribers to re-read before WaitListening returns,
		// so that one who subscribes after it gets no stale "".
		d.hub.reset()
		d.setListening(true)
		err = d.serveListener(ctx, conn)
		d.setListening(false)
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		_ = conn.Close(cctx)
		cancel()
		if ctx.Err() != nil {
			return nil
		}
		d.noteFailure()
		d.log.Warn("database listener lost its connection; reconnecting", "err", err)
	}
}

func (d *DB) listen(ctx context.Context) (*pgx.Conn, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lc := d.listenConfig
	if d.password != nil {
		lc = lc.Copy()
		if err := d.password(cctx, lc); err != nil {
			return nil, err
		}
	}
	conn, err := pgx.ConnectConfig(cctx, lc)
	if err != nil {
		return nil, err
	}
	for _, ch := range Channels {
		if _, err := conn.Exec(cctx, "LISTEN "+pgx.Identifier{ch}.Sanitize()); err != nil {
			_ = conn.Close(context.WithoutCancel(cctx))
			return nil, fmt.Errorf("listen %s: %w", ch, err)
		}
	}
	return conn, nil
}

// serveListener delivers the notifications of conn until it fails or ctx
// ends. A connection idle for listenerIdle is checked with a statement, so
// that a dead server is noticed without waiting for TCP.
func (d *DB) serveListener(ctx context.Context, conn *pgx.Conn) error {
	for {
		wctx, cancel := context.WithTimeout(ctx, listenerIdle)
		n, err := conn.WaitForNotification(wctx)
		cancel()
		if err == nil {
			d.hub.deliver(n.Channel, n.Payload)
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !pgconn.Timeout(err) {
			return err
		}
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err = conn.Exec(pctx, "SELECT 1")
		cancel()
		if err != nil {
			return err
		}
	}
}

func (d *DB) setListening(on bool) {
	d.listenMu.Lock()
	defer d.listenMu.Unlock()
	if on == d.listenOK {
		return
	}
	d.listenOK = on
	if on {
		close(d.listening)
	} else {
		d.listening = make(chan struct{})
	}
}

// WaitListening waits until the listener listens, or ctx ends.
func (d *DB) WaitListening(ctx context.Context) error {
	d.listenMu.Lock()
	ch := d.listening
	d.listenMu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// DefaultPoll is WaitFor's timer when the caller passes none.
const DefaultPoll = 500 * time.Millisecond

// WaitFor waits for another transaction's write. It subscribes to channel
// first, then calls check; until check reports done or fails, it waits for
// a payload equal to key, "" or "*", or for poll (DefaultPoll when 0), and
// calls check again. It returns check's error, or ctx's when ctx ends.
// Because it subscribes before the first check, a notification sent
// between a check and the wait that follows is never missed.
func (d *DB) WaitFor(ctx context.Context, channel, key string, poll time.Duration,
	check func(ctx context.Context) (done bool, err error)) error {
	if poll <= 0 {
		poll = DefaultPoll
	}
	ch, unsubscribe := d.Subscribe(channel)
	defer unsubscribe()
	timer := time.NewTimer(poll)
	defer timer.Stop()
	for {
		done, err := check(ctx)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		timer.Reset(poll)
	wait:
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case p := <-ch:
				if p == key || p == "" || p == "*" {
					break wait
				}
			case <-timer.C:
				break wait
			}
		}
	}
}

// sleep waits for d or ctx; false when ctx ended.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
