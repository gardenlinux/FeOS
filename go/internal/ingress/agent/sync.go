// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
)

// errStalled ends a Sync stream that delivered nothing, not even a
// heartbeat, for timing.syncIdle.
var errStalled = errors.New("the table stream went silent")

// hello says Hello on the session: it sends the WireGuard public key and
// endpoint (at the device's current port), keeps the instance's place on
// the mesh in mesh.json and configures the device for it. A device that
// cannot be configured does not end the session: the mesh loop tries
// again, and MeshConfigured reports it.
func (a *Agent) hello(ctx context.Context, s *session) error {
	a.mu.Lock()
	pub := a.wgKey.PublicKey()
	ep := a.meshEndpointLocked()
	a.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, a.tune.callTimeout)
	defer cancel()
	resp, err := s.client.Hello(cctx, &wire.HelloRequest{PublicKey: pub[:], Endpoint: ep, Version: a.cfg.Version})
	if err != nil {
		return err
	}
	if resp.Name != a.cfg.Name {
		return fmt.Errorf("the control plane answered Hello for ingress %q, not %q", resp.Name, a.cfg.Name)
	}
	mc, err := a.meshConfigFor(resp.PodNetwork, resp.Address)
	if err != nil {
		return fmt.Errorf("the Hello's answer: %w", err)
	}
	s.hello = resp
	a.mu.Lock()
	a.refusals.connected(time.Now())
	a.lastErr = ""
	a.announced = ep
	a.mu.Unlock()

	place := meshPlace{PodNetwork: resp.PodNetwork, Prefix: resp.Prefix, Address: resp.Address}
	a.mu.Lock()
	changed := a.place == nil || *a.place != place
	a.mu.Unlock()
	if changed {
		if err := a.dir.saveMesh(place); err != nil {
			a.log.Error("ingress agent: keep mesh.json", "err", err)
		}
		a.mu.Lock()
		a.place = &place
		a.mu.Unlock()
		a.log.Info("ingress agent: took its place on the mesh", "podNetwork", place.PodNetwork, "prefix", place.Prefix, "address", place.Address)
	}
	a.wantMesh(mc)
	return nil
}

// meshConfigFor is the device's configuration for a place on the mesh:
// the address as a /128 and the cluster's ULA routed through the device.
func (a *Agent) meshConfigFor(podNetwork, address string) (MeshConfig, error) {
	ula, err := netip.ParsePrefix(podNetwork)
	if err != nil || !ula.Addr().Is6() || ula.Addr().Is4In6() {
		return MeshConfig{}, fmt.Errorf("pod network %q is not an IPv6 prefix", podNetwork)
	}
	addr, err := netip.ParseAddr(address)
	if err != nil || !addr.Is6() || addr.Zone() != "" || !ula.Contains(addr) {
		return MeshConfig{}, fmt.Errorf("mesh address %q is not an IPv6 address in %s", address, ula)
	}
	a.mu.Lock()
	key, port := a.wgKey, a.meshPort
	a.mu.Unlock()
	return MeshConfig{
		Device:     a.cfg.MeshDevice,
		PrivateKey: key,
		ListenPort: port,
		MTU:        a.cfg.MeshMTU,
		Endpoint:   a.endpoint,
		Address:    netip.PrefixFrom(addr, 128),
		PodNetwork: ula.Masked(),
	}, nil
}

// serve runs the session's Sync stream and renews the certificate when it
// is due. It returns the session to continue on after a renewal, or the
// error that ended the stream.
func (a *Agent) serve(ctx context.Context, s *session) (*session, error) {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	next := make(chan *session, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		a.renewLoop(sctx, s, next, cancel)
	}()
	err := a.syncStream(sctx, s)
	cancel()
	wg.Wait()
	select {
	case ns := <-next:
		return ns, nil
	default:
		return nil, err
	}
}

// renewLoop renews the certificate at its renew time on the session's
// connection, then connects with the new certificate and hands that
// session over, ending the current one: the old connection is dropped
// only once the new one said Hello. Failures are retried every
// renewRetry.
func (a *Agent) renewLoop(ctx context.Context, s *session, next chan<- *session, stop context.CancelFunc) {
	renewed := false
	t := time.NewTimer(a.renewDelay(s.id))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if !renewed {
			if _, err := a.renew(ctx, s); err != nil {
				if ctx.Err() != nil {
					return
				}
				a.rl.warn("renew", "ingress agent: renewing the certificate failed; retrying", "err", err, "retry", a.tune.renewRetry)
				t.Reset(a.tune.renewRetry)
				continue
			}
			renewed = true
		}
		ns, err := a.connect(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			a.rl.warn("renew-connect", "ingress agent: connecting with the renewed certificate failed; keeping the old connection", "err", err, "retry", a.tune.renewRetry)
			t.Reset(a.tune.renewRetry)
			continue
		}
		next <- ns
		stop()
		return
	}
}

// syncStream receives the table on the session until the stream ends. The
// first message must be whole; every message is applied in sequence, a
// message in parts once its last part has arrived.
func (a *Agent) syncStream(ctx context.Context, s *session) error {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var stalled atomic.Bool
	idle := time.AfterFunc(a.tune.syncIdle, func() {
		stalled.Store(true)
		cancel()
	})
	defer idle.Stop()
	stream, err := s.client.Sync(sctx)
	if err != nil {
		if stalled.Load() {
			return errStalled
		}
		return err
	}
	first := true
	for {
		t, err := stream.Recv()
		if err != nil {
			if stalled.Load() {
				return errStalled
			}
			return err
		}
		idle.Reset(a.tune.syncIdle)
		if first && !t.Whole {
			return fmt.Errorf("%w: the stream's first message %d is not whole", wire.ErrSequence, t.Seq)
		}
		first = false
		applied, err := a.apply(t)
		if err != nil {
			return err
		}
		if applied != nil && applied.Whole {
			s.synced.Store(true)
			a.log.Info("ingress agent: receiving the table", "addr", s.addr, "instance", s.hello.Instance,
				"envs", len(applied.Envs))
		}
	}
}

// apply applies a message of the Sync stream, or a part of one, and
// returns the message applied (wire.State.Apply): nil for a part before
// the last, which changes nothing yet. A message that changed anything
// goes to OnState as a copy and to the cache; a new mesh version sets the
// device's peers. A heartbeat, which confirms the table, goes to the
// cache when it was written cacheRefresh ago or longer: table.json's
// savedAt then tells a restarted instance how old its table is, also in a
// cluster where nothing changes.
func (a *Agent) apply(part *wire.Table) (*wire.Table, error) {
	a.mu.Lock()
	if a.state == nil {
		a.state = wire.NewState()
	}
	t, err := a.state.Apply(part)
	if err != nil || t == nil {
		a.mu.Unlock()
		return nil, err
	}
	now := time.Now()
	a.tableAt = now
	changed := t.Whole || len(t.Envs) > 0 || len(t.Removed) > 0 || t.Mesh != nil || t.Self != nil || t.Challenges != nil
	var clone *wire.State
	if changed {
		clone = a.state.Clone()
	}
	refresh := !changed && now.Sub(a.cachedAt) >= a.tune.cacheRefresh
	peers := !a.peersSet || a.peersVer != a.state.Mesh.Version
	a.mu.Unlock()
	if !changed {
		if refresh {
			a.kickCache()
		}
		return t, nil
	}
	if peers {
		a.syncPeers()
	}
	if a.deps.OnState != nil {
		a.deps.OnState(clone)
	}
	a.kickCache()
	return t, nil
}

// kickCache asks the cache loop to write the table.
func (a *Agent) kickCache() {
	select {
	case a.cacheKick <- struct{}{}:
	default:
	}
}

// cacheLoop writes the table to table.json cacheDelay after a change, and
// once more when ctx ends if a write is pending.
func (a *Agent) cacheLoop(ctx context.Context) {
	var (
		t       *time.Timer
		fire    <-chan time.Time
		pending bool
	)
	defer func() {
		if t != nil {
			t.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			if pending {
				a.saveCache()
			}
			return
		case <-a.cacheKick:
			pending = true
			if fire == nil {
				t = time.NewTimer(a.tune.cacheDelay)
				fire = t.C
			}
		case <-fire:
			fire, pending = nil, false
			a.saveCache()
		}
	}
}

func (a *Agent) saveCache() {
	a.mu.Lock()
	var s *wire.State
	if a.state != nil {
		s = a.state.Clone()
	}
	a.mu.Unlock()
	if s == nil {
		return
	}
	at := time.Now()
	if err := a.dir.saveTable(s, at); err != nil {
		a.rl.warn("cache", "ingress agent: write the table cache", "err", err)
		return
	}
	a.mu.Lock()
	a.cachedAt = at
	a.mu.Unlock()
}

// checkTableAge warns, once an hour, while the table is older than
// tableAgeWarn: the control plane has not been reached for that long. It
// is silent until an attempt to reach the control plane failed, so a
// restart does not warn of the cache before its first connection
// answered, and while a session is up, whose stream confirms the table or
// ends within syncIdle.
func (a *Agent) checkTableAge(now time.Time) {
	a.mu.Lock()
	at := a.tableAt
	warn := a.reachFailed && a.cur == nil && a.state != nil && !at.IsZero() && now.Sub(at) > a.tune.tableAgeWarn &&
		(a.tableWarnAt.IsZero() || now.Sub(a.tableWarnAt) >= a.tune.tableAgeWarn)
	if warn {
		a.tableWarnAt = now
	}
	a.mu.Unlock()
	if warn {
		a.log.Warn("ingress agent: the table is old; the control plane has not been reached", "age", now.Sub(at).Round(time.Second), "since", at)
	}
}
