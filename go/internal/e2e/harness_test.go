// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	networkv1 "github.com/gardenlinux/feos/go/gen/feos/network/v1"
	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/config"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/env"
	"github.com/gardenlinux/feos/go/internal/ingress"
	"github.com/gardenlinux/feos/go/internal/ingress/agent"
	"github.com/gardenlinux/feos/go/internal/ingress/wg"
	"github.com/gardenlinux/feos/go/internal/mesh"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/server"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/testnode"
)

// The world every test runs in.
const (
	trustDomain = "feos.test"
	dnsDomain   = "example.test"
	// tenantToken is the bootstrap token: the tenant "operator", as
	// FeCode's FECODE_SANDBOX_API_TOKEN.
	tenantToken = "e2e-tenant-token-0123456789abcdef"
	tenant      = "operator"
	adminUser   = "admin"
	adminPass   = "pw"
	// testImage is pinned by digest, so that no registry is asked.
	testImage = "example.test/img@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	// poolSize bounds every instance's database pool: a test runs up to
	// three instances, and the tests run in parallel against one server.
	poolSize = 10
	// maxClusters bounds how many clusters of this package run at once,
	// whatever -parallel says: a cluster holds up to about 44 connections
	// to PostgreSQL (the test's pool, and a pool and a listener per
	// instance), and the server is shared with the other packages' tests.
	maxClusters = 4
	// processRoot is where the process tools keep their records in a
	// container; the tests point it at a directory of their own, since
	// the test node runs commands on the host.
	processRoot = "/var/lib/sandbox-processes"
)

var bg = context.Background()

// clusterSlots holds a token for every cluster that runs (maxClusters).
var clusterSlots = make(chan struct{}, maxClusters)

var (
	tenantAuth = "Bearer " + tenantToken
	adminAuth  = "Basic " + base64.StdEncoding.EncodeToString([]byte(adminUser+":"+adminPass))
)

// cluster is one control plane: a database, a CA, the instances that run
// on them and the test nodes joined to them. Everything stops, and the
// database is dropped, when the test ends.
type cluster struct {
	t       *testing.T
	log     *slog.Logger
	url     string
	caDir   string
	issuer  *pki.Issuer
	db      *db.DB
	st      *store.Store
	objects *testnode.ObjectStore
	// cfg is the instances' configuration; start sets each instance's id
	// and addresses.
	cfg config.Config

	mu        sync.Mutex
	instances []*instance
	nodes     []*node
	next      int
	// ends run after the instances and the nodes have stopped.
	ends []func()
}

// instance is one ferund instance of a cluster, running.
type instance struct {
	*server.Instance
	name   string
	cancel context.CancelFunc
	done   chan error

	stopOnce sync.Once
	stopErr  error
	stopped  chan struct{} // closed when the stop begins
	finished chan struct{} // closed when Run has returned
}

// node is a test node of a cluster, running.
type node struct {
	*testnode.Node
	name   string
	cancel context.CancelFunc
	done   chan struct{}
	err    error // Run's result, once done is closed
}

// newCluster makes the database and the CA of a cluster; tune changes the
// instances' configuration. No instance runs yet.
func newCluster(t *testing.T, tune func(*config.Config)) *cluster {
	t.Helper()
	// The slot is taken first and so given back last, after the
	// cluster has stopped and its database is dropped.
	clusterSlots <- struct{}{}
	t.Cleanup(func() { <-clusterSlots })
	u := withParams(t, dbtest.NewDatabase(t), "pool_max_conns", strconv.Itoa(poolSize))
	base := t.TempDir()
	caDir := filepath.Join(base, "ca")
	if err := pki.InitCA(pki.InitCAOptions{RootDir: filepath.Join(base, "root"), Dir: caDir, TrustDomain: trustDomain,
		DNSDomains: []string{dnsDomain}}); err != nil {
		t.Fatal(err)
	}
	iss, err := pki.LoadIssuer(caDir, trustDomain)
	if err != nil {
		t.Fatal(err)
	}
	log := dbtest.Logger(t)
	d, err := db.Open(bg, u, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	c := &cluster{t: t, log: log, url: u, caDir: caDir, issuer: iss, db: d, st: store.New(d),
		objects: testnode.NewObjectStore("e2e-objects-" + uuid.NewString()[:8])}
	c.cfg = config.Default()
	c.cfg.DatabaseURL, c.cfg.CADir, c.cfg.TrustDomain = u, caDir, trustDomain
	c.cfg.Listen = "127.0.0.1:0"
	c.cfg.BootstrapToken = tenantToken
	c.cfg.AdminUsers = map[string]string{adminUser: adminPass}
	c.cfg.DefaultImage = testImage
	c.cfg.IdlePause = 0
	c.cfg.ReadinessDelay = 0
	c.cfg.ShutdownGrace = 30 * time.Second
	c.cfg.Version = "e2e"
	if tune != nil {
		tune(&c.cfg)
	}
	t.Cleanup(c.shutdown)
	return c
}

// withParams sets connection parameters, given as name and value pairs,
// on the database URL u, in either of its forms (URL or key=value); they
// take precedence over what u says.
func withParams(t *testing.T, u string, kv ...string) string {
	t.Helper()
	if !strings.Contains(u, "://") {
		for i := 0; i+1 < len(kv); i += 2 {
			u += " " + kv[i] + "=" + kv[i+1]
		}
		return u
	}
	p, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	q := p.Query()
	for i := 0; i+1 < len(kv); i += 2 {
		q.Set(kv[i], kv[i+1])
	}
	p.RawQuery = q.Encode()
	return p.String()
}

// shutdown stops the instances, all at once, then the nodes: the
// instances' last ops still reach the nodes. Then the ends run.
func (c *cluster) shutdown() {
	c.mu.Lock()
	instances := append([]*instance(nil), c.instances...)
	nodes := append([]*node(nil), c.nodes...)
	ends := c.ends
	c.mu.Unlock()
	defer func() {
		for _, f := range ends {
			f()
		}
	}()
	var wg sync.WaitGroup
	for _, in := range instances {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = in.stop()
		}()
	}
	wg.Wait()
	for _, n := range nodes {
		n.stop()
	}
}

// start builds an instance with the cluster's configuration (tune changes
// it for this instance), on free ports, runs it and waits until it
// serves.
func (c *cluster) start(name string, tune ...func(*config.Config)) *instance {
	c.t.Helper()
	cfg := c.cfg
	cfg.InstanceID = "e2e-" + name
	for _, f := range tune {
		f(&cfg)
	}
	var si *server.Instance
	for attempt := 0; ; attempt++ {
		addr := "127.0.0.1:" + freePort(c.t)
		cfg.NodeListen, cfg.NodeAdvertiseAddress = addr, addr
		var err error
		si, err = server.New(bg, cfg, c.log)
		if err == nil {
			break
		}
		// Another listener may have taken the free port in between.
		if errors.Is(err, syscall.EADDRINUSE) && attempt < 5 {
			continue
		}
		c.t.Fatalf("instance %s: %v", name, err)
	}
	// The test image names no registry that answers (ADR 0025).
	si.Engine.SetImageAccess(func(context.Context, string, model.RegistryCredentials) error { return nil })
	ctx, cancel := context.WithCancel(bg)
	in := &instance{Instance: si, name: name, cancel: cancel, done: make(chan error, 1), stopped: make(chan struct{}),
		finished: make(chan struct{})}
	go func() { in.done <- si.Run(ctx) }()
	c.mu.Lock()
	c.instances = append(c.instances, in)
	c.mu.Unlock()
	eventually(c.t, 30*time.Second, "instance "+name+" serves", func() bool {
		r, err := plainClient.Get(in.url("/healthz"))
		if err != nil {
			return false
		}
		r.Body.Close()
		return r.StatusCode == http.StatusOK
	})
	return in
}

// freePort returns a TCP port that was free a moment ago.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}

// url is the URL of path on the instance's front doors.
func (in *instance) url(path string) string { return "http://" + in.HTTPAddr() + path }

// stop stops the instance as SIGTERM does (§7) and returns Run's result.
func (in *instance) stop() error { return <-in.stopAsync() }

// stopAsync begins the stop and returns at once; the channel receives
// Run's result. From its call on, the instance is not live.
func (in *instance) stopAsync() <-chan error {
	in.stopOnce.Do(func() {
		close(in.stopped)
		in.cancel()
		go func() {
			in.stopErr = <-in.done
			close(in.finished)
		}()
	})
	ch := make(chan error, 1)
	go func() {
		<-in.finished
		ch <- in.stopErr
	}()
	return ch
}

// isStopped reports whether the instance's stop has begun.
func (in *instance) isStopped() bool {
	select {
	case <-in.stopped:
		return true
	default:
		return false
	}
}

// live returns the instances that are not stopping.
func (c *cluster) live() []*instance {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*instance
	for _, in := range c.instances {
		if !in.isStopped() {
			out = append(out, in)
		}
	}
	return out
}

// pick returns the next live instance, round robin: a balancer without
// stickiness, which FeCode's new connection per request meets anyway.
func (c *cluster) pick() *instance {
	live := c.live()
	if len(live) == 0 {
		c.t.Error("no instance runs")
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	return live[c.next%len(live)]
}

// leader returns the live instance that leads, waiting for one.
func (c *cluster) leader() *instance {
	c.t.Helper()
	var l *instance
	eventually(c.t, 30*time.Second, "a leader", func() bool {
		for _, in := range c.live() {
			if in.Elector.IsLeader() {
				l = in
				return true
			}
		}
		return false
	})
	return l
}

// joinToken issues a join token pinned to name ("" for none).
func (c *cluster) joinToken(name string) string {
	c.t.Helper()
	live := c.live()
	if len(live) == 0 {
		c.t.Fatal("no instance runs")
	}
	_, token, _, err := live[0].Fleet.IssueJoinToken(bg, "e2e", name, time.Hour)
	if err != nil {
		c.t.Fatal(err)
	}
	return token
}

// nodeConfig is the configuration of a test node named name that joins
// with token through the live instances: the shared object store, a quick
// bring-up.
func (c *cluster) nodeConfig(name, token string) testnode.Config {
	var addrs []string
	for _, in := range c.live() {
		addrs = append(addrs, in.NodeAddr())
	}
	return testnode.Config{
		Name: name, JoinToken: token, ServerName: c.issuer.ControlPlaneDomain(), TrustDomain: trustDomain,
		Addrs: addrs, ObjectStore: c.objects, PendingDelay: 50 * time.Millisecond, Log: c.log.With("node", name),
	}
}

// join joins a test node named name with a token pinned to it (tune
// changes its configuration) and waits until it is ready and connected to
// every live instance.
func (c *cluster) join(name string, tune ...func(*testnode.Config)) *node {
	c.t.Helper()
	cfg := c.nodeConfig(name, c.joinToken(name))
	for _, f := range tune {
		f(&cfg)
	}
	n := c.runNode(cfg)
	c.waitNode(n)
	return n
}

// runNode runs a test node until the test ends; it does not wait for its
// join.
func (c *cluster) runNode(cfg testnode.Config) *node {
	c.t.Helper()
	tn, err := testnode.New(cfg)
	if err != nil {
		c.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(bg)
	n := &node{Node: tn, name: cfg.Name, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(n.done)
		n.err = tn.Run(ctx)
	}()
	c.mu.Lock()
	c.nodes = append(c.nodes, n)
	c.mu.Unlock()
	return n
}

// waitNode waits until the node is ready and every live instance has a
// current connection to it.
func (c *cluster) waitNode(n *node) {
	c.t.Helper()
	eventually(c.t, 60*time.Second, "node "+n.name+" ready and connected", func() bool {
		select {
		case <-n.done:
			c.t.Fatalf("node %s ended: %v", n.name, n.err)
		default:
		}
		row, err := c.st.GetNode(bg, n.name)
		if err != nil || row.Status != model.NodeReady {
			return false
		}
		for _, in := range c.live() {
			if !in.Links.Connected(n.name) {
				return false
			}
		}
		return true
	})
}

// stop ends the node's Run and waits for it.
func (n *node) stop() {
	n.cancel()
	<-n.done
}

// ended waits up to within for the node's Run to end and returns its
// result.
func (n *node) ended(t *testing.T, within time.Duration) error {
	t.Helper()
	select {
	case <-n.done:
		return n.err
	case <-time.After(within):
		t.Fatalf("node %s still runs", n.name)
		return nil
	}
}

// pod returns the node's record of the pod; false when it has none.
func (n *node) pod(id podid.ID) (*podv1.PodInfo, bool) {
	for _, p := range n.Pods() {
		if p.GetPodId() == id.String() {
			return p, true
		}
	}
	return nil, false
}

// ------------------------------------------------------------------ HTTP --

// plainClient makes every request on a new connection, as FeCode's
// clients do.
var plainClient = &http.Client{Transport: newTransport(), Timeout: 3 * time.Minute}

func newTransport() *http.Transport { return &http.Transport{DisableKeepAlives: true} }

// reply is an HTTP answer.
type reply struct {
	code   int
	body   string
	header http.Header
}

// call sends a request to in, or through the balancer when in is nil,
// with auth as its Authorization header ("" for none).
func (c *cluster) call(in *instance, method, path, auth, body string) reply {
	c.t.Helper()
	r, err := c.callErr(bg, in, method, path, auth, body)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	return r
}

// callErr is call that returns the transport's error.
func (c *cluster) callErr(ctx context.Context, in *instance, method, path, auth, body string) (reply, error) {
	if in == nil {
		if in = c.pick(); in == nil {
			return reply{}, errors.New("no instance runs")
		}
	}
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, in.url(path), rd)
	if err != nil {
		return reply{}, err
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := plainClient.Do(req)
	if err != nil {
		return reply{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return reply{}, err
	}
	return reply{code: resp.StatusCode, body: string(b), header: resp.Header}, nil
}

// sandbox is a sandbox as a client holds it: its ID and a token.
type sandbox struct {
	id    podid.ID
	token string
}

func (sb sandbox) auth() string { return "Bearer " + sb.token }
func (sb sandbox) path(p string) string {
	return "/api/v1/sandboxes/" + sb.id.String() + p
}

// create creates a sandbox through in (nil: the balancer) as FeCode
// does: ferund mints its ID, a short one (ADR 0019), which the answer
// carries.
func (c *cluster) create(in *instance) sandbox {
	c.t.Helper()
	r := c.call(in, "POST", "/api/v1/sandboxes", tenantAuth, "{}")
	if r.code != http.StatusCreated {
		c.t.Fatalf("create: %d %s", r.code, r.body)
	}
	var out struct {
		Token  string `json:"token"`
		Status struct {
			SandboxID   string `json:"sandboxId"`
			ClientState string `json:"clientState"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(r.body), &out); err != nil || out.Token == "" {
		c.t.Fatalf("create: %s (%v)", r.body, err)
	}
	id, err := podid.Parse(out.Status.SandboxID)
	if err != nil || id.IsLegacy() {
		c.t.Fatalf("create: the new ID %q is not a short ID (%v)", out.Status.SandboxID, err)
	}
	return sandbox{id: id, token: out.Token}
}

// createID makes a sandbox under a given ID, which no client can any more
// (ADR 0019): its row and a token written to the database directly, as
// for an ID used again (a sandbox with a UUID made before, or a short ID
// drawn again after its sandbox was deleted). The ID must be new;
// attachable gives the row the template of an attachable sandbox.
func (c *cluster) createID(id podid.ID, attachable bool) sandbox {
	c.t.Helper()
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		c.t.Fatal(err)
	}
	token := base64.RawURLEncoding.EncodeToString(b[:])
	n := store.NewSandbox{ID: id, Owner: tenant, Image: testImage, VCPUs: c.cfg.DefaultVCPUs, MemoryMiB: c.cfg.DefaultMemoryMiB}
	if attachable {
		n.PodTemplate = &model.PodTemplate{Command: []string{"sleep", "infinity"},
			SecurityGroups: []string{model.SandboxGroupID(id)}, NameTables: []string{model.SandboxTableID(id)}}
	}
	err := c.st.InTx(bg, func(tx *store.Store) error {
		_, created, err := tx.CreateSandbox(bg, n)
		if err != nil {
			return err
		}
		if !created {
			return fmt.Errorf("sandbox %s exists", id)
		}
		_, err = tx.CreateSandboxToken(bg, store.HashToken(token), id, "default")
		return err
	})
	if err != nil {
		c.t.Fatalf("create %s: %v", id, err)
	}
	return sandbox{id: id, token: token}
}

// execResult is the REST answer of a tool call.
type execResult struct {
	ExitCode  int    `json:"exitCode"`
	Output    string `json:"output"`
	Truncated bool   `json:"truncated"`
}

// exec runs command in the sandbox over REST through in (nil: the
// balancer).
func (c *cluster) exec(in *instance, sb sandbox, command string) reply {
	c.t.Helper()
	r, err := c.execErr(bg, in, sb, command)
	if err != nil {
		c.t.Fatalf("exec %q: %v", command, err)
	}
	return r
}

func (c *cluster) execErr(ctx context.Context, in *instance, sb sandbox, command string) (reply, error) {
	b, _ := json.Marshal(map[string]any{"command": command})
	return c.callErr(ctx, in, "POST", sb.path("/exec"), sb.auth(), string(b))
}

// run runs command in the sandbox and returns its output; anything but
// a 200 with exit code 0 fails the test.
func (c *cluster) run(in *instance, sb sandbox, command string) string {
	c.t.Helper()
	r := c.exec(in, sb, command)
	res, err := decodeExec(r)
	if err != nil {
		c.t.Fatalf("exec %q in %s: %v", command, sb.id, err)
	}
	if res.ExitCode != 0 {
		c.t.Fatalf("exec %q in %s: exit code %d: %s", command, sb.id, res.ExitCode, res.Output)
	}
	return res.Output
}

// decodeExec reads a 200 answer of exec.
func decodeExec(r reply) (execResult, error) {
	if r.code != http.StatusOK {
		return execResult{}, fmt.Errorf("%d %s", r.code, r.body)
	}
	var res execResult
	if err := json.Unmarshal([]byte(r.body), &res); err != nil {
		return execResult{}, fmt.Errorf("%v: %s", err, r.body)
	}
	return res, nil
}

// pause pauses the sandbox through REST and expects the 200.
func (c *cluster) pause(in *instance, sb sandbox) {
	c.t.Helper()
	if r := c.call(in, "POST", sb.path("/pause"), tenantAuth, ""); r.code != http.StatusOK {
		c.t.Fatalf("pause %s: %d %s", sb.id, r.code, r.body)
	}
}

// ------------------------------------------------------------------- MCP --

// bearer is FeCode's MCP transport: the sandbox token on every request.
type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

// balancer sends every request to the next live instance on a new
// connection: an HTTP balancer without sticky sessions.
type balancer struct{ c *cluster }

func (b balancer) RoundTrip(r *http.Request) (*http.Response, error) {
	in := b.c.pick()
	if in == nil {
		return nil, errors.New("no instance runs")
	}
	r = r.Clone(r.Context())
	r.URL.Host = in.HTTPAddr()
	r.Host = ""
	return plainClient.Transport.RoundTrip(r)
}

// lbURL is the front door's URL behind the balancer.
const lbURL = "http://ferun.lb.test"

// legacy is the protocol version of an initialize-only client.
var legacy = &mcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"}

// mcpConnect opens an MCP session as FeCode does: go-sdk's streamable
// client without the standalone SSE stream, the sandbox token on every
// request; opts nil is FeCode's own call. endpoint lbURL goes through the
// balancer.
func (c *cluster) mcpConnect(endpoint, token string, opts *mcp.ClientSessionOptions) (*mcp.ClientSession, error) {
	var base http.RoundTripper = plainClient.Transport
	if strings.HasPrefix(endpoint, lbURL) {
		base = balancer{c}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "fecode-backend", Version: "v1"}, nil)
	ctx, cancel := context.WithTimeout(bg, time.Minute)
	defer cancel()
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             endpoint,
		DisableStandaloneSSE: true,
		HTTPClient:           &http.Client{Transport: bearer{token: token, base: base}},
	}, opts)
	if err != nil {
		return nil, err
	}
	c.t.Cleanup(func() { _ = cs.Close() })
	return cs, nil
}

// mcpSession is mcpConnect that fails the test on an error.
func (c *cluster) mcpSession(endpoint, token string, opts *mcp.ClientSessionOptions) *mcp.ClientSession {
	c.t.Helper()
	cs, err := c.mcpConnect(endpoint, token, opts)
	if err != nil {
		c.t.Fatalf("MCP session at %s: %v", endpoint, err)
	}
	return cs
}

// callTool calls a tool and returns its text, its isError and the call's
// error.
func callTool(cs *mcp.ClientSession, tool string, args map[string]any) (string, bool, error) {
	ctx, cancel := context.WithTimeout(bg, 2*time.Minute)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return "", false, err
	}
	var b strings.Builder
	for _, ct := range res.Content {
		if tc, ok := ct.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError, nil
}

// -------------------------------------------------------------- database --

// row reads a sandbox's row.
func (c *cluster) row(id podid.ID) store.Sandbox {
	c.t.Helper()
	sb, err := c.st.GetSandbox(bg, id)
	if err != nil {
		c.t.Fatalf("sandbox %s: %v", id, err)
	}
	return sb
}

// waitState waits until the sandbox's row is in state without an op in
// flight, and returns it.
func (c *cluster) waitState(id podid.ID, state string, within time.Duration) store.Sandbox {
	c.t.Helper()
	var sb store.Sandbox
	eventually(c.t, within, "sandbox "+id.String()+" "+state, func() bool {
		var err error
		sb, err = c.st.GetSandbox(bg, id)
		return err == nil && sb.ClientState == state && sb.Op == ""
	})
	return sb
}

// count runs a query that returns one integer.
func (c *cluster) count(query string, args ...any) int {
	c.t.Helper()
	var n int
	if err := c.db.Pool.QueryRow(bg, query, args...).Scan(&n); err != nil {
		c.t.Fatalf("%s: %v", query, err)
	}
	return n
}

// sql runs a statement.
func (c *cluster) sql(query string, args ...any) {
	c.t.Helper()
	if _, err := c.db.Pool.Exec(bg, query, args...); err != nil {
		c.t.Fatalf("%s: %v", query, err)
	}
}

// wake wakes the leader's loops whose names contain one of hints, as the
// instances' own writes do, instead of waiting for their intervals.
func (c *cluster) wake(hints ...string) {
	c.t.Helper()
	for _, h := range hints {
		if err := db.Notify(bg, c.db.Pool, db.ChanLeader, h); err != nil {
			c.t.Fatal(err)
		}
	}
}

// onEnd runs f when the cluster has stopped: after its instances and its
// nodes, before its database is dropped.
func (c *cluster) onEnd(f func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ends = append(c.ends, f)
}

// dbProxy forwards connections to the cluster's PostgreSQL, so that a test
// can cut an instance off its database. While cut, nothing passes in
// either direction and the connections stay open, as on a network that
// drops every packet: the instance's statements hang, and to the database
// and the other instances it looks crashed. Healing closes the connections
// that lost bytes in the cut and forwards new ones again.
type dbProxy struct {
	// dsn is the cluster's database URL through the proxy.
	dsn             string
	ln              net.Listener
	network, target string
	wg              sync.WaitGroup

	mu     sync.Mutex
	cut    bool
	healed chan struct{} // closed when the cut ends
	closed bool
	conns  map[net.Conn]struct{}
}

// newDBProxy starts a proxy to the cluster's database. It is healed before
// the cluster stops, so that its instances stop with their database, and
// closed when the cluster has stopped.
func (c *cluster) newDBProxy() *dbProxy {
	c.t.Helper()
	pc, err := pgconn.ParseConfig(c.url)
	if err != nil {
		c.t.Fatal(err)
	}
	port := strconv.Itoa(int(pc.Port))
	network, target := "tcp", net.JoinHostPort(pc.Host, port)
	if strings.HasPrefix(pc.Host, "/") {
		network, target = "unix", filepath.Join(pc.Host, ".s.PGSQL."+port)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		c.t.Fatal(err)
	}
	_, lport, _ := net.SplitHostPort(ln.Addr().String())
	p := &dbProxy{dsn: withParams(c.t, c.url, "host", "127.0.0.1", "port", lport), ln: ln, network: network,
		target: target, conns: map[net.Conn]struct{}{}}
	p.wg.Add(1)
	go p.serve()
	c.t.Cleanup(p.heal)
	c.onEnd(p.close)
	return p
}

// cutOff stops all traffic through the proxy until heal.
func (p *dbProxy) cutOff() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.cut {
		p.cut, p.healed = true, make(chan struct{})
	}
}

// heal ends a cut.
func (p *dbProxy) heal() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cut {
		p.cut = false
		close(p.healed)
	}
}

// close stops the proxy and closes its connections.
func (p *dbProxy) close() {
	p.heal()
	p.mu.Lock()
	p.closed = true
	conns := make([]net.Conn, 0, len(p.conns))
	for conn := range p.conns {
		conns = append(conns, conn)
	}
	p.mu.Unlock()
	_ = p.ln.Close()
	for _, conn := range conns {
		_ = conn.Close()
	}
	p.wg.Wait()
}

func (p *dbProxy) serve() {
	defer p.wg.Done()
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			return
		}
		if !p.track(conn) {
			return
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			defer p.untrack(conn)
			// A connection made in the cut is held, as its SYN would be.
			if !p.pass() {
				return
			}
			up, err := net.DialTimeout(p.network, p.target, 10*time.Second)
			if err != nil || !p.track(up) {
				return
			}
			defer p.untrack(up)
			done := make(chan struct{})
			go func() {
				defer close(done)
				p.pipe(conn, up)
			}()
			p.pipe(up, conn)
			<-done
		}()
	}
}

// pipe copies from src to dst until either fails or a cut swallowed bytes;
// then it closes both, which ends the other direction too.
func (p *dbProxy) pipe(dst, src net.Conn) {
	defer dst.Close()
	defer src.Close()
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if !p.pass() {
				return
			}
			if _, err := dst.Write(buf[:n]); err != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// pass reports whether traffic may flow. In a cut it holds the caller
// until the cut ends and then reports false.
func (p *dbProxy) pass() bool {
	p.mu.Lock()
	if !p.cut {
		p.mu.Unlock()
		return true
	}
	healed := p.healed
	p.mu.Unlock()
	<-healed
	return false
}

// track registers a connection for close; false (and the connection
// closed) once the proxy is closed.
func (p *dbProxy) track(conn net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		_ = conn.Close()
		return false
	}
	p.conns[conn] = struct{}{}
	return true
}

func (p *dbProxy) untrack(conn net.Conn) {
	_ = conn.Close()
	p.mu.Lock()
	delete(p.conns, conn)
	p.mu.Unlock()
}

// --------------------------------------------------------------- ingress --

// appsZone is the apps zone of the clusters that ferun-ingress serves:
// public environments' hosts are <label>.apps.test.
const appsZone = "apps.test"

// ingressCluster tunes a cluster for ferun-ingress: the pods' mesh, which
// the ingress joins, and the apps zone; extra changes it further.
func ingressCluster(extra ...func(*config.Config)) func(*config.Config) {
	return func(cfg *config.Config) {
		cfg.PodNetwork, cfg.AppsZone = config.PodNetworkMesh, appsZone
		for _, f := range extra {
			f(cfg)
		}
	}
}

// meshNode tunes a test node for the ingress's clusters: on the mesh,
// checking the security groups its pods name, as feosd does.
func meshNode(cfg *testnode.Config) { cfg.Mesh, cfg.CheckSecurityGroups = true, true }

// ingressToken issues a join token of kind ingress pinned to name over
// the admin API, as the operator does, and checks that the answer names
// the kind.
func (c *cluster) ingressToken(name string) string {
	c.t.Helper()
	b, _ := json.Marshal(map[string]string{"kind": model.JoinKindIngress, "name": name})
	r := c.call(nil, "POST", "/api/v1/admin/join-tokens", adminAuth, string(b))
	if r.code != http.StatusCreated {
		c.t.Fatalf("ingress join token: %d %s", r.code, r.body)
	}
	var out struct {
		Token string `json:"token"`
		Kind  string `json:"kind"`
		Name  string `json:"name"`
	}
	if err := json.Unmarshal([]byte(r.body), &out); err != nil || out.Token == "" || out.Kind != model.JoinKindIngress ||
		out.Name != name {
		c.t.Fatalf("ingress join token: %s (%v)", r.body, err)
	}
	return out.Token
}

// setPublic makes an environment public, or not, with the tenant's token
// through the balancer (PUT …/public), and returns the answer. (publish
// is the events' already.)
func (c *cluster) setPublic(envID uuid.UUID, public bool) env.PublicAnswer {
	c.t.Helper()
	r := c.call(nil, "PUT", "/api/v1/environments/"+envID.String()+"/public", tenantAuth,
		fmt.Sprintf(`{"public":%t}`, public))
	if r.code != http.StatusOK {
		c.t.Fatalf("public %t: %d %s", public, r.code, r.body)
	}
	var ans env.PublicAnswer
	if err := json.Unmarshal([]byte(r.body), &ans); err != nil || ans.Public != public {
		c.t.Fatalf("public %t: %s (%v)", public, r.body, err)
	}
	return ans
}

// ingressProc is a ferun-ingress instance of a cluster in the test's
// process: an ingress.Program on loopback listeners with a self-signed
// *.apps.test certificate, which joins with a token of its own, connects
// to the instances' nodes' port, and reaches the pods through a fake mesh
// (fakeMesh) and a dial hook (dialPod). It stops before the cluster does;
// restart runs it again on the same state directory.
type ingressProc struct {
	c    *cluster
	name string
	cfg  ingress.Config
	tr   *http.Transport

	mu     sync.Mutex
	prog   *ingress.Program
	mesh   *fakeMesh
	cancel context.CancelFunc
	done   chan error
	// cut: the ingress is cut off from the control plane (cutOff);
	// cpConns are its connections there.
	cut     bool
	cpConns map[net.Conn]struct{}
}

// runIngress runs a ferun-ingress instance named name (tune changes its
// configuration) until the test ends: its token file holds a token pinned
// to name, --control-plane lists every live instance's nodes' port, and
// its listeners are on loopback. It does not wait for its join.
func (c *cluster) runIngress(name string, tune ...func(*ingress.Config)) *ingressProc {
	c.t.Helper()
	dir := c.t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte(c.ingressToken(name)+"\n"), 0o600); err != nil {
		c.t.Fatal(err)
	}
	certFile, keyFile, pool := wildcardPair(c.t, dir, "*."+appsZone)
	cfg := ingress.Default()
	cfg.Name = name
	cfg.StateDir = filepath.Join(dir, "state")
	cfg.JoinTokenFile = tokenFile
	for _, in := range c.live() {
		cfg.ControlPlane = append(cfg.ControlPlane, in.NodeAddr())
	}
	cfg.ControlPlaneName = c.issuer.ControlPlaneDomain()
	cfg.TrustDomain = trustDomain
	cfg.ListenHTTP, cfg.ListenHTTPS, cfg.HealthListen = "127.0.0.1:0", "127.0.0.1:0", "127.0.0.1:0"
	cfg.TLSCert, cfg.TLSKey = certFile, keyFile
	cfg.MeshEndpoint = "127.0.0.9:51820"
	cfg.ShutdownDelay, cfg.ShutdownGrace = 100*time.Millisecond, 5*time.Second
	cfg.Version = "e2e"
	for _, f := range tune {
		f(&cfg)
	}
	p := &ingressProc{c: c, name: name, cfg: cfg, cpConns: map[net.Conn]struct{}{}}
	p.tr = &http.Transport{
		// SNI is the URL's host; the connection goes to the HTTPS
		// listener whatever the host, as through the balancer.
		TLSClientConfig: &tls.Config{RootCAs: pool},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, p.program().Addrs().HTTPS)
		},
		ForceAttemptHTTP2: true,
	}
	p.start()
	c.t.Cleanup(func() {
		p.stop()
		p.tr.CloseIdleConnections()
	})
	return p
}

// start makes a Program of the configuration, with a new fake mesh
// device, and runs it.
func (p *ingressProc) start() {
	p.c.t.Helper()
	m := &fakeMesh{}
	prog, err := ingress.New(p.cfg, ingress.Deps{Mesh: m, Dial: p.dialPod, DialControlPlane: p.dialControlPlane,
		Log: p.c.log.With("ingress", p.name)})
	if err != nil {
		p.c.t.Fatalf("ingress %s: %v", p.name, err)
	}
	ctx, cancel := context.WithCancel(bg)
	done := make(chan error, 1)
	go func() { done <- prog.Run(ctx) }()
	p.mu.Lock()
	p.prog, p.mesh, p.cancel, p.done = prog, m, cancel, done
	p.mu.Unlock()
}

// stop stops the program as SIGTERM does and waits for its end; a stopped
// one stays stopped.
func (p *ingressProc) stop() {
	p.mu.Lock()
	cancel, done := p.cancel, p.done
	p.cancel = nil
	p.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			p.c.t.Errorf("ingress %s: %v", p.name, err)
		}
	case <-time.After(time.Minute):
		p.c.t.Errorf("ingress %s did not stop", p.name)
	}
}

// restart stops the program and runs a new one on the same state
// directory, as a restarted pod does with its volume.
func (p *ingressProc) restart() {
	p.c.t.Helper()
	p.stop()
	p.tr.CloseIdleConnections()
	p.start()
}

func (p *ingressProc) program() *ingress.Program {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.prog
}

func (p *ingressProc) device() *fakeMesh {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mesh
}

// status is the program's status (/statusz).
func (p *ingressProc) status() ingress.Status { return p.program().Status() }

// ready is the program's readiness (/readyz) and why not.
func (p *ingressProc) ready() (bool, string) { return p.program().Ready() }

// waitReady waits until the program is ready.
func (p *ingressProc) waitReady(within time.Duration) {
	p.c.t.Helper()
	deadline := time.Now().Add(within)
	for {
		ok, why := p.ready()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			p.c.t.Fatalf("ingress %s is not ready after %v: %s", p.name, within, why)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// health GETs a path of the program's health listener.
func (p *ingressProc) health(path string) (int, string) {
	resp, err := plainClient.Get("http://" + p.program().Addrs().Health + path)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// client is an HTTPS client of the program's HTTPS listener: the SNI and
// the Host are the URL's host, and the certificate is checked against the
// test's wildcard. Connections are kept alive, over HTTP/2.
func (p *ingressProc) client() *http.Client {
	return &http.Client{Transport: p.tr, Timeout: 3 * time.Minute}
}

// get GETs https://<host><path> through the program.
func (p *ingressProc) get(ctx context.Context, host, path string) (reply, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+path, nil)
	if err != nil {
		return reply{}, err
	}
	resp, err := p.client().Do(req)
	if err != nil {
		return reply{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return reply{}, err
	}
	return reply{code: resp.StatusCode, body: string(b), header: resp.Header}, nil
}

// address is the program's address on the mesh, as its device was
// configured; "" before.
func (p *ingressProc) address() string {
	c, ok := p.device().config()
	if !ok {
		return ""
	}
	return c.Address.Addr().String()
}

// dialControlPlane is the program's dial to the control plane: a plain
// TCP dial, which fails while the ingress is cut off.
func (p *ingressProc) dialControlPlane(ctx context.Context, addr string) (net.Conn, error) {
	p.mu.Lock()
	cut := p.cut
	p.mu.Unlock()
	if cut {
		return nil, fmt.Errorf("dial %s: the test cut the ingress off the control plane: %w", addr, syscall.ENETUNREACH)
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cut {
		_ = conn.Close()
		return nil, fmt.Errorf("dial %s: the test cut the ingress off the control plane: %w", addr, syscall.ENETUNREACH)
	}
	p.cpConns[conn] = struct{}{}
	return conn, nil
}

// cutOff cuts the ingress off the control plane, as a partition does:
// its connections there close and new ones fail, until heal. The nodes
// and the pods stay reachable.
func (p *ingressProc) cutOff() {
	p.mu.Lock()
	p.cut = true
	conns := p.cpConns
	p.cpConns = map[net.Conn]struct{}{}
	p.mu.Unlock()
	for conn := range conns {
		_ = conn.Close()
	}
}

// heal ends a cut.
func (p *ingressProc) heal() {
	p.mu.Lock()
	p.cut = false
	p.mu.Unlock()
}

// dialPod is the program's dial to the instances: the pods' mesh and the
// nodes' firewalls as the tests play them. It reaches [address]:port of a
// pod that runs on a test node of the cluster when the program's device
// has the node as a peer and the node has the program (its key and its
// /64), the node's group ing-<pod> lets TCP in on the port from the group
// ferun-ingress, which lists the program's address, and the test serves
// the pod's port (testnode.Script.PortAddrOf). Anything else is refused,
// as nothing would answer there.
func (p *ingressProc) dialPod(ctx context.Context, network, addr string) (net.Conn, error) {
	refuse := func(format string, args ...any) (net.Conn, error) {
		return nil, fmt.Errorf("dial %s: %s: %w", addr, fmt.Sprintf(format, args...), syscall.ECONNREFUSED)
	}
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return refuse("not an IP address")
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return refuse("not a port")
	}
	m := p.device()
	mc, ok := m.config()
	if !ok {
		return refuse("the mesh device is not configured")
	}
	n, pod := p.c.podAt(ip)
	if n == nil {
		return refuse("no pod runs at %s", ip)
	}
	nm := n.Mesh()
	if !slices.ContainsFunc(m.peerList(), func(peer wg.Peer) bool { return bytes.Equal(peer.PublicKey[:], nm.GetPublicKey()) }) {
		return refuse("node %s is not a peer of the ingress's device", n.name)
	}
	ownKey := mc.PrivateKey.PublicKey()
	if !slices.ContainsFunc(nm.GetPeers(), func(peer *networkv1.MeshPeer) bool {
		pfx, err := netip.ParsePrefix(peer.GetPodPrefix())
		return peer.GetNode() == mesh.IngressPeerName(p.name) && bytes.Equal(peer.GetPublicKey(), ownKey[:]) &&
			err == nil && pfx.Contains(mc.Address.Addr())
	}) {
		return refuse("node %s does not have the ingress as a peer", n.name)
	}
	if !admits(nodeGroup(n, model.IngressGroupID(pod)), uint32(port)) {
		return refuse("node %s does not let the ingress in to port %d of %s", n.name, port, pod)
	}
	peers := nodeGroup(n, model.IngressPeersGroup)
	if peers == nil || !slices.ContainsFunc(peers.GetMembers(), func(s string) bool {
		a, err := netip.ParseAddr(s)
		return err == nil && a == mc.Address.Addr()
	}) {
		return refuse("node %s's group %s does not list the ingress", n.name, model.IngressPeersGroup)
	}
	target, ok := n.Script().PortAddrOf(pod, uint32(port))
	if !ok {
		return refuse("nothing listens on port %d of %s", port, pod)
	}
	var d net.Dialer
	return d.DialContext(ctx, network, target)
}

// admits reports whether a group ing-<pod> lets TCP in on port from the
// group ferun-ingress.
func admits(g *networkv1.SecurityGroup, port uint32) bool {
	if g == nil {
		return false
	}
	for _, r := range g.GetRules() {
		if r.GetDirection() != networkv1.Direction_DIRECTION_INGRESS || r.GetProtocol() != networkv1.Protocol_PROTOCOL_TCP ||
			r.GetGroup() != model.IngressPeersGroup {
			continue
		}
		for _, pr := range r.GetPorts() {
			if port == pr.GetFirst() || pr.GetFirst() <= port && port <= pr.GetLast() {
				return true
			}
		}
	}
	return false
}

// podAt returns the running test node whose pod at ip runs, and the pod;
// nil when none.
func (c *cluster) podAt(ip netip.Addr) (*node, podid.ID) {
	c.mu.Lock()
	nodes := append([]*node(nil), c.nodes...)
	c.mu.Unlock()
	for _, n := range nodes {
		select {
		case <-n.done:
			continue
		default:
		}
		for _, info := range n.Pods() {
			a, err := netip.ParseAddr(info.GetIpv6Address())
			if err != nil || a != ip || info.GetState() != podv1.PodState_POD_STATE_RUNNING {
				continue
			}
			id, err := podid.Parse(info.GetPodId())
			if err != nil {
				continue
			}
			return n, id
		}
	}
	return nil, ""
}

// fakeMesh is ferun-ingress's WireGuard device in the tests: it keeps the
// configuration and the peers it is given, and reports a handshake of now
// with every peer, as a mesh on which every node answers.
type fakeMesh struct {
	mu      sync.Mutex
	configs []agent.MeshConfig
	peers   []wg.Peer
}

func (m *fakeMesh) Configure(c agent.MeshConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.configs = append(m.configs, c)
	return nil
}

func (m *fakeMesh) SetPeers(peers []wg.Peer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.peers = slices.Clone(peers)
	return nil
}

func (m *fakeMesh) Status() (wg.Device, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := wg.Device{Name: agent.DefaultMeshDevice}
	if len(m.configs) > 0 {
		c := m.configs[len(m.configs)-1]
		d.Name, d.PublicKey, d.ListenPort = c.Device, c.PrivateKey.PublicKey(), c.ListenPort
	}
	now := time.Now()
	for _, p := range m.peers {
		d.Peers = append(d.Peers, wg.PeerState{Peer: p, LastHandshake: now})
	}
	return d, nil
}

// config is the device's last configuration; false before the first.
func (m *fakeMesh) config() (agent.MeshConfig, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.configs) == 0 {
		return agent.MeshConfig{}, false
	}
	return m.configs[len(m.configs)-1], true
}

// peerList is the device's peers.
func (m *fakeMesh) peerList() []wg.Peer {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.peers)
}

// wildcardPair writes a self-signed certificate for name and its key into
// dir, and returns a pool that trusts it.
func wildcardPair(t *testing.T, dir, name string) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		DNSNames:              []string{name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}), 0o600); err != nil {
		t.Fatal(err)
	}
	pool = x509.NewCertPool()
	pool.AddCert(cert)
	return certFile, keyFile, pool
}

// ------------------------------------------------------------------ wait --

// eventually waits up to within for cond, checking every 50 ms.
func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", within, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// never checks for d that cond stays false.
func never(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			t.Fatalf("%s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
