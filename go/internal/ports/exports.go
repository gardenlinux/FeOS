// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ports

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// What the environments (ADR 0018) use of the door (envdeps.Ports): the
// readiness checks of their instances and attached services, which take
// no hold and count as no activity, and the route door's forward.

// ProbeSlots is how many checks (Probe, DialPod) of one sandbox may be
// open at once on one instance: the probe slots, apart from the sandbox's
// stream cap of 32 forwarded connections, so that forwarded traffic that
// fills the cap (websockets, event streams, a page load) never makes a
// check of a busy sandbox fail. A check holds its slot for at most its
// timeout (a DialPod's connection until Close). Four let the checks of
// one pass overlap (an instance's readiness, a sandbox's attached
// services) while a sandbox's checks add at most four Forward streams to
// its 32; a caller probes one sandbox at most ProbeSlots at a time.
const ProbeSlots = 4

// ErrProbeBusy is a check that found every probe slot of the sandbox
// taken by its other checks: it fails at once rather than wait for one,
// so that a check never stalls. It says nothing about the sandbox.
var ErrProbeBusy = errors.New("every probe slot of the sandbox on this instance is in use")

// DialPod opens a port-forward to the pod's port of the lease's sandbox,
// answered by the guest's ready (something listens): a TCP readiness
// check, or the connection of an HTTP one. It takes no hold and touches
// nothing; the dial has its own timeout (2 s) and takes a probe slot of
// the sandbox, never one of its stream cap: it fails at once with
// ErrProbeBusy when every probe slot is taken, and never waits. The
// connection is the caller's alone (never one of the forwarding pools'),
// and holds its probe slot until Close.
func (h *Handler) DialPod(ctx context.Context, lease model.Lease, port int) (net.Conn, error) {
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("port %d is out of range", port)
	}
	ctx, cancel := context.WithTimeout(ctx, h.t.probeTimeout)
	defer cancel()
	release, ok := h.tryProbeSlot(lease.SandboxID)
	if !ok {
		return nil, ErrProbeBusy
	}
	c, err := h.openStream(ctx, tkey{lease.SandboxID, port}, lease.Node, release)
	if err != nil && ctx.Err() != nil {
		return nil, &slowError{port: port, after: h.t.probeTimeout, err: err}
	}
	return c, err
}

// Probe GETs path (with its query) from the pod's port over a
// port-forward, with Host localhost:<port> (dev servers check it) and
// Connection: close, and returns the answer's status code; the body is
// not read. It takes no hold and touches nothing; the whole check has its
// own timeout (2 s) and its connection is a DialPod's: a probe slot,
// never a slot of the sandbox's stream cap, ErrProbeBusy at once when
// every probe slot is taken.
func (h *Handler) Probe(ctx context.Context, lease model.Lease, port int, path string) (int, error) {
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		return 0, fmt.Errorf("probe path %q does not begin with /", path)
	}
	host := "localhost:" + strconv.Itoa(port)
	req, err := http.NewRequest(http.MethodGet, "http://"+host+path, nil)
	if err != nil {
		return 0, fmt.Errorf("probe path %q: %w", path, err)
	}
	req.Close = true
	req.Header.Set("User-Agent", "ferun-probe")
	ctx, cancel := context.WithTimeout(ctx, h.t.probeTimeout)
	defer cancel()
	conn, err := h.DialPod(ctx, lease, port)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	// The stream ignores deadlines: the end of ctx closes it.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	fail := func(err error) (int, error) {
		if ctx.Err() != nil {
			return 0, &slowError{port: port, after: h.t.probeTimeout}
		}
		return 0, fmt.Errorf("port %d: %w", port, err)
	}
	if err := req.Write(conn); err != nil {
		return fail(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return fail(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// textDotDot answers a route door's path with a ".." segment.
const textDotDot = "the path has a .. segment"

// ForwardTo forwards the request to the sandbox's port as the door does
// for a sandbox token, for the environments' route door, which has
// authorised the request: the sandbox is brought up and held with a
// forward hold for the request, and touched after it (a route to a paused
// or hibernated instance wakes it); the Host the client named goes on,
// upgrades and streams are kept, and a port of the sandbox's Kubernetes
// bindings is refused (403). credentials are the request headers that
// carried the route door's credential: they stop here, as
// Proxy-Authorization always does, and every other header goes on; nil
// is Authorization, as for a sandbox token. upstreamPath is the path the
// pod sees ("" is "/"), rawPath its escaped form ("" when it needs none);
// the query is kept. A path with a ".." segment, also one
// percent-encoded, is refused with 400. Every answer of the door's own is
// plain text.
func (h *Handler) ForwardTo(w http.ResponseWriter, r *http.Request, sandboxID podid.ID, port int, upstreamPath, rawPath string,
	credentials []string) {
	if port < 1 || port > 65535 {
		reply{http.StatusBadGateway, fmt.Sprintf("port %d is out of range", port), 0}.write(w)
		return
	}
	if dotDot(upstreamPath, rawPath) {
		reply{http.StatusBadRequest, textDotDot, 0}.write(w)
		return
	}
	if !strings.HasPrefix(upstreamPath, "/") {
		upstreamPath = "/" + upstreamPath
		if rawPath != "" {
			rawPath = "/" + rawPath
		}
	}
	if rp := h.kubeRefusal(r.Context(), sandboxID, port); rp != nil {
		rp.write(w)
		return
	}
	h.forward(w, r, sandboxID, target{id: sandboxID.String(), port: port, path: upstreamPath, rawPath: rawPath,
		credentials: credentials})
}

// dotDot reports whether a path has a ".." segment: in the path, in its
// escaped form once unescaped (an encoded "/" splits a segment), or in
// either unescaped once more (a server that decodes twice).
func dotDot(path, rawPath string) bool {
	paths := []string{path}
	if rawPath != "" {
		dec, err := url.PathUnescape(rawPath)
		if err != nil {
			return true
		}
		paths = append(paths, dec)
	}
	for _, p := range paths {
		if hasDotDot(p) {
			return true
		}
		if dec, err := url.PathUnescape(p); err == nil && hasDotDot(dec) {
			return true
		}
	}
	return false
}

func hasDotDot(p string) bool {
	for seg := range strings.SplitSeq(p, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}
