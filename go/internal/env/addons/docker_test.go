// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package addons

// The blueprints against the real images: each test below renders an
// addon, runs its image in docker the way FeOS runs a pod's container,
// waits for the rendered probe, runs the provisioning twice and connects
// with the rendered URL from a second container, as a pod of the
// environment would. They run only with FERUN_ADDON_DOCKER=1 (and a
// docker the user may use); they pull what is missing.
//
// How FeOS is emulated (see .claude/caas/spike-addons.md §0):
//   - the container runs as uid 0 (FeOS ignores the image's user);
//   - a rendered command replaces entrypoint and cmd (--entrypoint plus
//     arguments), an empty one runs the image's;
//   - the volume is a fresh directory owned by root, 0755, holding
//     lost+found (0700), as the guest's mke2fs leaves it, bind-mounted at
//     the mount path;
//   - the probe and the steps run through docker exec, which inherits the
//     container's env as FeOS's exec does, the step's env on top;
//   - pods talk over IPv6: a dual-stack network of its own, since
//     docker's default bridge has none, and every URL is the [ipv6] form;
//   - a stop is a SIGKILL (docker kill).
//
// Everything a test starts carries the label ferun-addon-test=1 and is
// removed when it ends; after a killed run:
//
//	docker rm -f $(docker ps -aq --filter label=ferun-addon-test=1)
//	docker network rm $(docker network ls -q --filter label=ferun-addon-test=1)
//
// The volume directories are temporary directories, emptied as root.

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
)

const (
	dockerGate  = "FERUN_ADDON_DOCKER"
	dockerLabel = "ferun-addon-test=1"
	// natsBox is the NATS client image (nats.go's CLI).
	natsBox = "natsio/nats-box:0.19.7"
)

// dockerLab is one test's docker network and what it started there.
type dockerLab struct {
	t       *testing.T
	network string
}

// newDockerLab skips t unless FERUN_ADDON_DOCKER=1, and makes a
// dual-stack network for it: 172.29.24<n>.0/24 and fd99:5b1e:ca7:<n>::/64.
func newDockerLab(t *testing.T, n int) *dockerLab {
	t.Helper()
	if os.Getenv(dockerGate) != "1" {
		t.Skip("set " + dockerGate + "=1 to run the addon images in docker")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatalf("%s=1, but: %v", dockerGate, err)
	}
	l := &dockerLab{t: t, network: "ferun-addon-" + strings.ToLower(t.Name()) + "-" + randomHex(t, 3)}
	l.must(t, 2*time.Minute, "network", "create", "--label", dockerLabel, "--ipv6",
		"--subnet", fmt.Sprintf("172.29.24%d.0/24", n), "--subnet", fmt.Sprintf("fd99:5b1e:ca7:%d::/64", n), l.network)
	t.Cleanup(func() { l.docker(time.Minute, "network", "rm", l.network) })
	return l
}

// docker runs the docker CLI: stdout, stderr and the exit code (-1: it
// did not run to an exit).
func (l *dockerLab) docker(timeout time.Duration, args ...string) (string, string, int) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		code = -1
		var exit *exec.ExitError
		if errors.As(err, &exit) && ctx.Err() == nil {
			code = exit.ExitCode()
		} else {
			stderr.WriteString(err.Error())
		}
	}
	return stdout.String(), stderr.String(), code
}

// must runs docker and fails t unless it exits 0; its stdout.
func (l *dockerLab) must(t *testing.T, timeout time.Duration, args ...string) string {
	t.Helper()
	out, errOut, code := l.docker(timeout, args...)
	if code != 0 {
		t.Fatalf("docker %s: exit %d\n%s%s", strings.Join(redactArgs(args), " "), code, out, errOut)
	}
	return out
}

// redactArgs keeps generated credentials out of a failure's message.
func redactArgs(args []string) []string {
	out := slices.Clone(args)
	for i, a := range out {
		if k, _, ok := strings.Cut(a, "="); ok && i > 0 && out[i-1] == "-e" {
			out[i] = k + "=…"
		}
	}
	return out
}

// ensureImage pulls image unless docker has it.
func (l *dockerLab) ensureImage(t *testing.T, image string) {
	t.Helper()
	if _, _, code := l.docker(time.Minute, "image", "inspect", image); code != 0 {
		l.must(t, 10*time.Minute, "pull", "-q", image)
	}
}

// volume is a fresh volume as the node's guest formats one: a directory
// owned by root with lost+found, emptied again (as root, with image)
// when t ends.
func (l *dockerLab) volume(t *testing.T, image string) string {
	t.Helper()
	dir := t.TempDir()
	l.must(t, 2*time.Minute, "run", "--rm", "--label", dockerLabel, "--user", "0", "-v", dir+":/m",
		"--entrypoint", "sh", image, "-c", "chown 0:0 /m && chmod 755 /m && mkdir -m 700 /m/lost+found")
	t.Cleanup(func() {
		l.docker(2*time.Minute, "run", "--rm", "--label", dockerLabel, "--user", "0", "-v", dir+":/m",
			"--entrypoint", "sh", image, "-c",
			fmt.Sprintf("find /m -mindepth 1 -delete; chown %d:%d /m", os.Getuid(), os.Getgid()))
	})
	return dir
}

// start runs inst as FeOS would: uid 0, its command in place of the
// image's entrypoint and cmd (none: the image's), env resolved, the
// volume dir ("" none) at its mount, memory as the plan's. The
// container's name; it is removed when t ends.
func (l *dockerLab) start(t *testing.T, name string, inst Instance, env map[string]string, memoryMib int,
	dir string) string {
	t.Helper()
	name = l.network + "-" + name
	args := []string{"run", "-d", "--name", name, "--label", dockerLabel, "--network", l.network, "--user", "0",
		"--shm-size", "64m", "--memory", strconv.Itoa(memoryMib) + "m"}
	if inst.Mount != "" {
		if dir == "" {
			t.Fatal("the instance has a mount and no volume was made")
		}
		args = append(args, "-v", dir+":"+inst.Mount)
	}
	args = append(args, envArgs(env)...)
	if len(inst.Command) > 0 {
		args = append(args, "--entrypoint", inst.Command[0], inst.Image)
		args = append(args, inst.Command[1:]...)
	} else {
		args = append(args, inst.Image)
	}
	t.Cleanup(func() { l.docker(time.Minute, "rm", "-f", "-v", name) })
	l.must(t, 2*time.Minute, args...)
	return name
}

func envArgs(env map[string]string) []string {
	var args []string
	for _, k := range slices.Sorted(maps.Keys(env)) {
		args = append(args, "-e", k+"="+env[k])
	}
	return args
}

// exec runs argv in container with env on top of its own: output
// (stdout and stderr) and exit code.
func (l *dockerLab) exec(container string, env map[string]string, argv ...string) (string, int) {
	args := append([]string{"exec"}, envArgs(env)...)
	args = append(args, container)
	out, errOut, code := l.docker(2*time.Minute, append(args, argv...)...)
	return out + errOut, code
}

// waitReady polls the probe until it exits 0, failing t when the
// container stops or a minute and a half passes.
func (l *dockerLab) waitReady(t *testing.T, container string, probe []string) time.Duration {
	t.Helper()
	begin := time.Now()
	for {
		out, code := l.exec(container, nil, probe...)
		if code == 0 {
			return time.Since(begin)
		}
		running, _, _ := l.docker(time.Minute, "inspect", "-f", "{{.State.Running}}", container)
		if strings.TrimSpace(running) != "true" || time.Since(begin) > 90*time.Second {
			logs, logErr, _ := l.docker(time.Minute, "logs", "--tail", "40", container)
			t.Fatalf("%s not ready (running %s, probe exit %d: %s)\n%s%s", container, strings.TrimSpace(running),
				code, out, logs, logErr)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// address is the container's IPv6 address on the lab's network.
func (l *dockerLab) address(t *testing.T, container string) string {
	t.Helper()
	addr := strings.TrimSpace(l.must(t, time.Minute, "inspect", "-f",
		`{{(index .NetworkSettings.Networks "`+l.network+`").GlobalIPv6Address}}`, container))
	if !strings.Contains(addr, ":") {
		t.Fatalf("%s has no IPv6 address: %q", container, addr)
	}
	return addr
}

// client runs a one-off container on the lab's network (a pod of the
// environment): output and exit code.
func (l *dockerLab) client(entrypoint, image string, args ...string) (string, int) {
	all := []string{"run", "--rm", "--label", dockerLabel, "--network", l.network, "--entrypoint", entrypoint, image}
	out, errOut, code := l.docker(2*time.Minute, append(all, args...)...)
	return out + errOut, code
}

// provision runs the addon's steps in container as the controller
// would, each with its env resolved; each must exit 0 without a word.
func (l *dockerLab) provision(t *testing.T, container string, steps []Step, creds map[string]string) {
	t.Helper()
	for _, st := range steps {
		out, code := l.exec(container, resolveEnv(t, st.Env, creds), st.Argv...)
		if code != 0 || out != "" {
			t.Fatalf("step %s: exit %d\n%s", st.Name, code, redact(out, creds))
		}
	}
}

// credentials generates each of a's credentials as the controller does:
// 32 hex characters.
func credentials(t *testing.T, b Blueprint, a Addon) map[string]string {
	creds := map[string]string{}
	for _, k := range b.Credentials(a) {
		creds[k] = randomHex(t, 16)
	}
	return creds
}

func randomHex(t *testing.T, n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func resolveEnv(t *testing.T, env map[string]model.TemplateValue, creds map[string]string) map[string]string {
	t.Helper()
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = resolve(t, v, creds)
	}
	return out
}

// redact replaces the credentials' values in s, as ctl must before it
// stores a step's output.
func redact(s string, creds map[string]string) string {
	for _, v := range creds {
		s = strings.ReplaceAll(s, v, "<redacted>")
	}
	return s
}

// outputURL is the addon's rendered url reference, resolved.
func outputURL(t *testing.T, b Blueprint, a Addon, c Context, fields map[string]string, creds map[string]string) string {
	t.Helper()
	parts, err := b.Outputs(a, c, Ref{Key: "url", Fields: fields})
	if err != nil {
		t.Fatal(err)
	}
	u := resolveParts(t, parts, creds)
	if !strings.Contains(u, "@["+c.Address+"]:") {
		t.Fatalf("url %s is not the [ipv6] form", redact(u, creds))
	}
	return u
}

// testPKI is a root and leaves under it, as the platform's issuer makes
// them (ADR 0036, 0038): Ed25519 keys, a workload's subject CN=<td>/<pod>,
// OU=<workload>, its SPIFFE ID as the one URI name.
type testPKI struct {
	t    *testing.T
	root *x509.Certificate
	key  ed25519.PrivateKey
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test root"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testPKI{t: t, root: root, key: key}
}

// leaf issues a certificate for usage to subject pod and workload with
// the URI id: its PEM and its key's PKCS#8 PEM.
func (p *testPKI) leaf(pod, workload, id string, usage ...x509.ExtKeyUsage) (certPEM, keyPEM string) {
	p.t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		p.t.Fatal(err)
	}
	k, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		p.t.Fatal(err)
	}
	return p.issue(pub, pod, workload, id, usage), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: k}))
}

// issue is a certificate's PEM for pub (leaf).
func (p *testPKI) issue(pub crypto.PublicKey, pod, workload, id string, usage []x509.ExtKeyUsage) string {
	p.t.Helper()
	u, err := url.Parse(id)
	if err != nil {
		p.t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 64))
	if err != nil {
		p.t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{SerialNumber: serial,
		Subject: pkix.Name{CommonName: "run.test/" + pod, OrganizationalUnit: []string{workload}},
		URIs:    []*url.URL{u}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: usage}, p.root, pub, p.key)
	if err != nil {
		p.t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// leafFor is leaf for the key of keyPEM (a renewal): the certificate's
// PEM and keyPEM.
func (p *testPKI) leafFor(keyPEM, pod, workload, id string, usage ...x509.ExtKeyUsage) (certPEM, samePEM string) {
	p.t.Helper()
	b, _ := pem.Decode([]byte(keyPEM))
	if b == nil {
		p.t.Fatal("no key PEM")
	}
	k, err := x509.ParsePKCS8PrivateKey(b.Bytes)
	if err != nil {
		p.t.Fatal(err)
	}
	return p.issue(k.(ed25519.PrivateKey).Public(), pod, workload, id, usage), keyPEM
}

func (p *testPKI) rootPEM() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.root.Raw}))
}

// clientWith is client with dir mounted read-only at /c.
func (l *dockerLab) clientWith(dir, entrypoint, image string, args ...string) (string, int) {
	all := []string{"run", "--rm", "--label", dockerLabel, "--network", l.network, "-v", dir + ":/c:ro",
		"--entrypoint", entrypoint, image}
	out, errOut, code := l.docker(2*time.Minute, append(all, args...)...)
	return out + errOut, code
}

// TestPostgresDocker runs the blueprint as ADR 0038 has it, from the
// state an addon of before has: a running password session, then the
// steps; after them only a workload's certificate gets in, as the role
// its grant names, and nothing takes a password.
func TestPostgresDocker(t *testing.T) {
	lab := newDockerLab(t, 1)
	b, _ := Get("postgres")
	for _, version := range b.Versions() {
		t.Run(version, func(t *testing.T) {
			a := addon(t, "postgres", `{"type":"postgres","version":"`+version+`","plan":"dev",
				"options":{"databases":["expensa","reports"],"roles":["expense_svc","approval_svc"]}}`)
			if fs := b.Validate(nil, a); len(fs) != 0 {
				t.Fatalf("findings:\n%s", findingText(fs))
			}
			ca := newTestPKI(t)
			creds := credentials(t, b, a)
			serverCert, serverKey := ca.leaf("pgpod", "wl-00000000000000000000000000000000",
				"spiffe://run.test/tenant/t/env/e/addon/postgres", x509.ExtKeyUsageServerAuth)
			creds[TLSKey] = serverKey
			const (
				expenseWL  = "wl-0123456789abcdef0123456789abcdef"
				approvalWL = "wl-s-xmxfgckmkt6"
			)
			c := Context{Env: "test", Name: a.Name, TLS: &TLS{CertPEM: serverCert, CAPEM: ca.rootPEM(), Grants: []Grant{
				{Workload: expenseWL, ID: "spiffe://run.test/tenant/t/env/e/service/expense", Role: "expense_svc"},
				{Workload: approvalWL, ID: "spiffe://run.test/tenant/t/env/e/sandbox/xmxfgckmkt6", Role: "approval_svc"},
			}}}
			inst, err := b.Render(a, c)
			if err != nil {
				t.Fatal(err)
			}
			lab.ensureImage(t, inst.Image)
			pod := lab.start(t, "pg"+version, inst, resolveEnv(t, inst.Env, creds), b.Plans()[a.Plan].MemoryMib,
				lab.volume(t, inst.Image))
			t.Logf("ready after %s", lab.waitReady(t, pod, inst.Probe).Round(time.Millisecond))
			c.Address = lab.address(t, pod)

			// Before: the image's rules take the superuser's password over
			// the network. A session of that kind runs while the steps do.
			pw := url.QueryEscape(creds["password"])
			before := lab.network + "-before"
			lab.must(t, time.Minute, "run", "-d", "--name", before, "--label", dockerLabel, "--network", lab.network,
				"--entrypoint", "psql", inst.Image, "postgres://postgres:"+pw+"@["+c.Address+"]:5432/postgres?sslmode=disable",
				"-X", "-q", "-c", "SELECT pg_sleep(120)")
			t.Cleanup(func() { lab.docker(time.Minute, "rm", "-f", before) })
			waitSession(t, lab, pod)

			steps := b.Provision(a, c)
			if len(steps) != 2 {
				t.Fatalf("steps %v", steps)
			}
			lab.provision(t, pod, steps, creds)
			lab.provision(t, pod, steps, creds) // idempotent, silent
			out, _, _ := lab.docker(2*time.Minute, "wait", before)
			logs, logErr, _ := lab.docker(time.Minute, "logs", before)
			if strings.TrimSpace(out) == "0" || !strings.Contains(logs+logErr, "terminating connection due to administrator command") {
				t.Fatalf("the password session outlived the rules: exit %s: %s%s", strings.TrimSpace(out), logs, logErr)
			}

			dir := t.TempDir()
			write := func(name, content string) {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			write("root.crt", ca.rootPEM())
			expCert, expKey := ca.leaf("pod1", expenseWL, "spiffe://run.test/tenant/t/env/e/service/expense", x509.ExtKeyUsageClientAuth)
			write("expense.crt", expCert)
			write("expense.key", expKey)
			appCert, appKey := ca.leaf("xmxfgckmkt6", approvalWL, "spiffe://run.test/tenant/t/env/e/sandbox/xmxfgckmkt6", x509.ExtKeyUsageClientAuth)
			write("approval.crt", appCert)
			write("approval.key", appKey)
			// The same workload's ID under another root.
			other := newTestPKI(t)
			strCert, strKey := other.leaf("pod1", expenseWL, "spiffe://run.test/tenant/t/env/e/service/expense", x509.ExtKeyUsageClientAuth)
			write("stranger.crt", strCert)
			write("stranger.key", strKey)
			// Another workload: the same name in another environment has
			// another OU.
			elseCert, elseKey := ca.leaf("pod2", "wl-ffffffffffffffffffffffffffffffff",
				"spiffe://run.test/tenant/u/env/f/service/expense", x509.ExtKeyUsageClientAuth)
			write("else.crt", elseCert)
			write("else.key", elseKey)

			conninfo := func(user, db, who string) string {
				s := "host=" + c.Address + " port=5432 dbname=" + db + " user=" + user +
					" sslmode=verify-ca sslrootcert=/c/root.crt"
				if who != "" {
					s += " sslcert=/c/" + who + ".crt sslkey=/c/" + who + ".key"
				}
				if version != "16" {
					s += " sslnegotiation=direct" // ALPN postgresql, as the guest's binding (ADR 0036)
				}
				return s
			}
			psql := func(target, sql string) (string, int) {
				out, code := lab.clientWith(dir, "psql", inst.Image, target, "-X", "-q", "-At", "-v", "ON_ERROR_STOP=1", "-c", sql)
				return strings.TrimSpace(redact(out, creds)), code
			}
			if out, code := psql(conninfo("expense_svc", "expensa", "expense"),
				"CREATE TABLE expenses (id int); INSERT INTO expenses VALUES (1); "+
					"SELECT current_user, current_database(), current_schema(), s.ssl, s.version "+
					"FROM pg_stat_ssl s WHERE s.pid = pg_backend_pid()"); code != 0 ||
				out != "expense_svc|expensa|expense_svc|t|TLSv1.3" {
				t.Fatalf("expense_svc by its certificate: exit %d: %s", code, out)
			}
			if out, code := psql(conninfo("approval_svc", "reports", "approval"), "SELECT current_user, current_database()"); code != 0 ||
				out != "approval_svc|reports" {
				t.Fatalf("approval_svc by its sandbox's certificate: exit %d: %s", code, out)
			}
			if out, code := psql(conninfo("approval_svc", "expensa", "approval"), "SELECT * FROM expense_svc.expenses"); code == 0 ||
				!strings.Contains(out, "permission denied for schema expense_svc") {
				t.Fatalf("approval_svc read expense_svc's schema: exit %d: %s", code, out)
			}
			for _, refused := range []struct{ name, target, says string }{
				// The lab has no guest: TLS straight to the addon, without the
				// pod's certificate the binding would present.
				{"TLS without a certificate", "postgres://expense_svc@[" + c.Address + "]:5432/expensa?sslmode=require",
					"connection requires a valid client certificate"},
				// The reference's url without its binding: the addon takes no
				// connection without TLS.
				{"no encryption", "postgres://expense_svc@[" + c.Address + "]:5432/expensa?sslmode=disable", "no encryption"},
				{"another role than granted", conninfo("approval_svc", "expensa", "expense"), "certificate authentication failed"},
				{"the superuser by a certificate", conninfo("postgres", "postgres", "expense"), "certificate authentication failed"},
				{"another workload", conninfo("expense_svc", "expensa", "else"), "certificate authentication failed"},
				{"another root", conninfo("expense_svc", "expensa", "stranger"), ""},
				{"the password of before", "postgres://postgres:" + pw + "@[" + c.Address + "]:5432/postgres?sslmode=disable",
					"no encryption"},
				{"a password over TLS", "postgres://postgres:" + pw + "@[" + c.Address + "]:5432/postgres?sslmode=require",
					"connection requires a valid client certificate"},
			} {
				out, code := psql(refused.target, "SELECT 1")
				if code == 0 || !strings.Contains(out, refused.says) {
					t.Fatalf("%s: exit %d: %s", refused.name, code, out)
				}
				t.Logf("%s: exit %d: %s", refused.name, code, out)
			}
			// Loopback TCP takes no password either (a port-forward dials
			// 127.0.0.1 in the pod).
			if out, code := lab.exec(pod, map[string]string{"PGPASSWORD": creds["password"]},
				"psql", "-X", "-h", "127.0.0.1", "-U", "postgres", "-c", "SELECT 1"); code == 0 ||
				!strings.Contains(out, "no encryption") {
				t.Fatalf("loopback with the password: exit %d: %s", code, out)
			}
			// The role has none any more: not even the socket takes it as
			// another user.
			if out, code := lab.exec(pod, nil, "psql", "-X", "-At", "-h", "/var/run/postgresql", "-U", "postgres",
				"-c", "SELECT count(*) FROM pg_authid WHERE rolpassword IS NOT NULL"); code != 0 || strings.TrimSpace(out) != "0" {
				t.Fatalf("passwords left: exit %d: %s", code, out)
			}

			// A node-side stop is a kill: crash recovery, then TLS from the
			// volume, the same data, and provisioning still silent.
			lab.must(t, time.Minute, "kill", pod)
			restarted := time.Now().UTC().Format(time.RFC3339Nano)
			lab.must(t, time.Minute, "start", pod)
			lab.waitReady(t, pod, inst.Probe)
			c.Address = lab.address(t, pod)
			lab.provision(t, pod, steps, creds)
			if out, code := psql(conninfo("expense_svc", "expensa", "expense"), "SELECT count(*) FROM expenses"); code != 0 || out != "1" {
				t.Fatalf("after a restart: exit %d: %s", code, out)
			}
			// Neither the probe, which ran all along, nor the steps, nor a
			// workload's session leave a complaint in the log.
			for range 3 {
				lab.waitReady(t, pod, inst.Probe)
			}
			logs, logErr, _ = lab.docker(time.Minute, "logs", "--since", restarted, pod)
			for _, line := range strings.Split(logs+logErr, "\n") {
				// The probe meets crash recovery first, as it always did.
				if strings.Contains(line, "not yet accepting connections") || strings.Contains(line, "Consistent recovery state") {
					continue
				}
				if strings.Contains(line, "FATAL") || strings.Contains(line, "ERROR") || strings.Contains(line, "could not") ||
					strings.Contains(line, "contains errors") {
					t.Fatalf("a complaint in the log after the restart: %s\n%s%s", line, logs, logErr)
				}
			}

			// A role added later whose schema another role made first, open
			// to everyone: provisioning fails naming the schema rather than
			// adopt it.
			if out, code := psql(conninfo("expense_svc", "expensa", "expense"),
				"CREATE SCHEMA late_svc; GRANT USAGE, CREATE ON SCHEMA late_svc TO PUBLIC"); code != 0 {
				t.Fatalf("expense_svc makes late_svc's schema: exit %d: %s", code, out)
			}
			later := addon(t, "postgres", `{"type":"postgres","version":"`+version+`","plan":"dev",
				"options":{"databases":["expensa","reports"],"roles":["expense_svc","approval_svc","late_svc"]}}`)
			if fs := b.Validate(&a, later); len(fs) != 0 {
				t.Fatalf("findings:\n%s", findingText(fs))
			}
			st := b.Provision(later, c)[0]
			out, code := lab.exec(pod, resolveEnv(t, st.Env, creds), st.Argv...)
			if code == 0 || !strings.Contains(out, "schema late_svc in database expensa belongs to role expense_svc, not to role late_svc") {
				t.Fatalf("step %s past a squatted schema: exit %d\n%s", st.Name, code, out)
			}
		})
	}
}

// waitSession waits until pod's server has a client session over the
// network.
func waitSession(t *testing.T, lab *dockerLab, pod string) {
	t.Helper()
	for begin := time.Now(); time.Since(begin) < 30*time.Second; time.Sleep(200 * time.Millisecond) {
		out, _ := lab.exec(pod, nil, "psql", "-X", "-At", "-h", "/var/run/postgresql", "-U", "postgres", "-c",
			"SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend' AND client_addr IS NOT NULL")
		if strings.TrimSpace(out) == "1" {
			return
		}
	}
	t.Fatal("no session over the network")
}

// TestValkeyDocker runs the blueprint as ADR 0039 has it, from the state
// an addon of before has: valkey 8 with a password and data on the
// volume. The addon, which still names 8, runs 9 on that volume, on its
// socket alone until the step; after it, TLS 1.3 alone, and only a
// workload's certificate gets in, as the user of its ID with the data
// commands; nothing takes a password, and a removed grant's session ends.
func TestValkeyDocker(t *testing.T) {
	lab := newDockerLab(t, 2)
	b, _ := Get("valkey")
	a := addon(t, "cache", `{"type":"valkey","version":"8","plan":"dev","options":{"maxmemoryPolicy":"allkeys-lru"}}`)
	if fs := b.Validate(nil, a); len(fs) != 1 || fs[0].Severity != spec.SeverityWarning {
		t.Fatalf("findings:\n%s", findingText(fs))
	}
	inst, err := b.Render(a, Context{Env: "test", Name: a.Name})
	if err != nil {
		t.Fatal(err)
	}
	lab.ensureImage(t, inst.Image)
	dir := lab.volume(t, inst.Image)

	// Before: valkey 8 as the blueprint ran it until ADR 0039, its
	// password the default user's, with data.
	const before = "valkey/valkey:8-alpine"
	lab.ensureImage(t, before)
	password := randomHex(t, 16)
	old := lab.start(t, "valkey8", Instance{Image: before, Mount: "/data"}, map[string]string{
		"VALKEY_EXTRA_FLAGS": "--appendonly yes --requirepass " + password + " --maxmemory 89mb --maxmemory-policy allkeys-lru",
		"REDISCLI_AUTH":      password,
	}, 128, dir)
	lab.waitReady(t, old, []string{"sh", "-c", "valkey-cli ping | grep -qx PONG"})
	if out, code := lab.exec(old, nil, "valkey-cli", "set", "persisted", "yes"); code != 0 || strings.TrimSpace(out) != "OK" {
		t.Fatalf("set before: exit %d: %s", code, out)
	}
	lab.must(t, time.Minute, "stop", old)

	creds := credentials(t, b, a)
	pod := lab.start(t, "valkey", inst, resolveEnv(t, inst.Env, creds), b.Plans()[a.Plan].MemoryMib, dir)
	t.Logf("ready after %s", lab.waitReady(t, pod, inst.Probe).Round(time.Millisecond))
	addr := lab.address(t, pod)
	if out, code := lab.exec(pod, nil, "valkey-server", "--version"); code != 0 || !strings.Contains(out, "v=9.") {
		t.Fatalf("version: %s", out)
	}
	// Until the step, nothing listens but the socket.
	if out, code := lab.client("valkey-cli", inst.Image, "-h", addr, "-p", "6379", "ping"); code == 0 ||
		!strings.Contains(out, "Connection refused") {
		t.Fatalf("before the step, from the network: exit %d: %s", code, out)
	}

	ca := newTestPKI(t)
	const (
		addonID   = "spiffe://run.test/tenant/t/env/e/addon/cache"
		catalogID = "spiffe://run.test/tenant/t/env/e/service/catalog"
		sandboxID = "spiffe://run.test/tenant/t/env/e/sandbox/xmxfgckmkt6"
		catalogWL = "wl-0123456789abcdef0123456789abcdef"
		sandboxWL = "wl-s-xmxfgckmkt6"
	)
	serverCert, serverKey := ca.leaf("vkpod", "wl-00000000000000000000000000000000", addonID,
		x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth)
	creds[TLSKey] = serverKey
	grants := []Grant{
		{Workload: catalogWL, ID: catalogID, Role: "data"},
		{Workload: sandboxWL, ID: sandboxID, Role: "data"},
	}
	c := Context{Env: "test", Name: a.Name, Address: addr,
		TLS: &TLS{CertPEM: serverCert, CAPEM: ca.rootPEM(), ID: addonID, Grants: grants}}
	steps := b.Provision(a, c)
	if len(steps) != 1 {
		t.Fatalf("steps %v", steps)
	}
	lab.provision(t, pod, steps, creds)
	lab.provision(t, pod, steps, creds) // idempotent, silent
	lab.waitReady(t, pod, inst.Probe)

	certs := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(certs, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("root.crt", ca.rootPEM())
	for _, w := range []struct{ name, pod, workload, id string }{
		{"catalog", "pod1", catalogWL, catalogID},
		{"sandbox", "xmxfgckmkt6", sandboxWL, sandboxID},
		// Another workload: the same name in another environment.
		{"else", "pod2", "wl-ffffffffffffffffffffffffffffffff", "spiffe://run.test/tenant/u/env/f/service/catalog"},
		// The addon's own ID, by a pod of elsewhere: only a certificate
		// with ClientAuth.
		{"self", "pod3", "wl-00000000000000000000000000000000", addonID},
	} {
		crt, key := ca.leaf(w.pod, w.workload, w.id, x509.ExtKeyUsageClientAuth)
		write(w.name+".crt", crt)
		write(w.name+".key", key)
	}
	// The catalog's ID under another root.
	other := newTestPKI(t)
	strCert, strKey := other.leaf("pod1", catalogWL, catalogID, x509.ExtKeyUsageClientAuth)
	write("stranger.crt", strCert)
	write("stranger.key", strKey)
	// What a workload's certificate cannot be: the server's, ServerAuth
	// alone.
	srvCert, srvKey := ca.leaf("vkpod", "wl-00000000000000000000000000000000", catalogID, x509.ExtKeyUsageServerAuth)
	write("server-only.crt", srvCert)
	write("server-only.key", srvKey)

	// TLS from the first byte, no ALPN: what the guest's binding does.
	tls := func(who string, args ...string) (string, int) {
		all := []string{"--tls", "--cacert", "/c/root.crt", "-h", addr, "-p", "6379"}
		if who != "" {
			all = append(all, "--cert", "/c/"+who+".crt", "--key", "/c/"+who+".key")
		}
		out, code := lab.clientWith(certs, "valkey-cli", inst.Image, append(all, args...)...)
		return strings.TrimSpace(redact(out, creds)), code
	}
	for _, check := range []struct {
		who  string
		args []string
		want string
	}{
		{"catalog", []string{"acl", "whoami"}, catalogID},
		{"catalog", []string{"get", "persisted"}, "yes"}, // 8's data, in 9
		{"catalog", []string{"set", "k", "v"}, "OK"},
		{"catalog", []string{"eval", "return redis.call('incr', KEYS[1])", "1", "n"}, "1"},
		{"catalog", []string{"client", "setname", "catalog"}, "OK"},
		{"sandbox", []string{"acl", "whoami"}, sandboxID},
		{"sandbox", []string{"get", "k"}, "v"},
	} {
		if out, code := tls(check.who, check.args...); code != 0 || out != check.want {
			t.Fatalf("%s %v: exit %d: %q, want %q", check.who, check.args, code, out, check.want)
		}
	}
	// The data commands, not the administrative ones.
	for _, args := range [][]string{
		{"config", "get", "dir"}, {"config", "set", "tls-auth-clients", "no"}, {"acl", "setuser", "default", "on", "nopass"},
		{"acl", "list"}, {"acl", "load"}, {"client", "list"}, {"monitor"}, {"shutdown"}, {"debug", "sleep", "0"},
		{"bgsave"}, {"replicaof", "fd00::1", "6379"}, {"module", "list"},
		{"migrate", "fd00::1", "6379", "k", "0", "1000"},
	} {
		// DEBUG and MODULE are off for everyone (enable-*-command no), before
		// any user's rights.
		if out, _ := tls("catalog", args...); !strings.Contains(out, "NOPERM") && !strings.Contains(out, "command not allowed") {
			t.Fatalf("catalog %v: %s", args, out)
		}
	}
	for _, refused := range []struct {
		name string
		who  string
		args []string
		says string
	}{
		{"another workload", "else", []string{"get", "k"}, "NOAUTH"},
		{"AUTH as the default user", "else", []string{"--user", "default", "--pass", password, "get", "k"},
			"WRONGPASS"},
		{"the password of before", "else", []string{"--pass", password, "get", "k"}, "WRONGPASS"},
		{"a granted user by a password", "else", []string{"--user", catalogID, "--pass", "x", "get", "k"}, "WRONGPASS"},
		// The handshake fails: the connection is closed or reset.
		{"no certificate", "", []string{"ping"}, "Error: "},
		{"another root", "stranger", []string{"ping"}, "Error: "},
		{"a server's certificate", "server-only", []string{"ping"}, "Error: "},
	} {
		out, _ := tls(refused.who, refused.args...)
		if !strings.Contains(out, refused.says) || strings.Contains(out, "\nv\n") || out == "v" || out == "PONG" {
			t.Fatalf("%s: %s", refused.name, out)
		}
		t.Logf("%s: %s", refused.name, strings.ReplaceAll(out, "\n", " / "))
	}
	// The addon's own ID is its administrator's, whoever holds a
	// certificate of it; the network reaches the port from no other pod
	// than the references' (ADR 0034), and no other pod has the ID.
	if out, code := tls("self", "acl", "whoami"); code != 0 || out != addonID {
		t.Fatalf("the addon's own ID: exit %d: %s", code, out)
	}
	// Plaintext to the TLS port, the reference's url without its binding,
	// and the url of before.
	for _, u := range []string{"redis://[" + addr + "]:6379/0", "redis://default:" + password + "@[" + addr + "]:6379/0"} {
		out, _ := lab.client("valkey-cli", inst.Image, "--no-auth-warning", "-u", u, "get", "k")
		if strings.Contains(out, "\nv") || strings.TrimSpace(out) == "v" {
			t.Fatalf("plaintext %s: %s", redact(u, creds), out)
		}
		t.Logf("plaintext: %s", strings.ReplaceAll(strings.TrimSpace(redact(out, map[string]string{"p": password})), "\n", " / "))
	}
	// TLS 1.2 is refused; an ALPN offer is ignored (Valkey has none), and
	// the guest makes none.
	sclient := func(args ...string) string {
		out, _ := lab.clientWith(certs, "sh", inst.Image, "-c", `printf 'PING\r\n' | timeout 5 openssl s_client -connect "[`+
			addr+`]:6379" -CAfile /c/root.crt -cert /c/catalog.crt -key /c/catalog.key `+strings.Join(args, " ")+` 2>&1`)
		return out
	}
	if out := sclient("-tls1_2"); !strings.Contains(out, "alert protocol version") {
		t.Fatalf("TLS 1.2: %s", out)
	}
	if out := sclient("-tls1_3", "-alpn", "redis"); !strings.Contains(out, "Protocol: TLSv1.3") ||
		!strings.Contains(out, "No ALPN negotiated") || !strings.Contains(out, "Verification: OK") {
		t.Fatalf("an ALPN offer: %s", out)
	}
	admin := func(args ...string) (string, int) {
		out, code := lab.exec(pod, nil, append([]string{"valkey-cli", "--tls", "--insecure", "--cert", valkeyDir + "/server.crt",
			"--key", valkeyDir + "/server.key", "-h", "127.0.0.1", "-p", "6379"}, args...)...)
		return strings.TrimSpace(out), code
	}
	for _, check := range []struct {
		args []string
		want string
	}{
		{[]string{"config", "get", "maxmemory"}, "maxmemory\n" + strconv.Itoa(89<<20)},
		{[]string{"config", "get", "maxmemory-policy"}, "maxmemory-policy\nallkeys-lru"},
		{[]string{"config", "get", "appendonly"}, "appendonly\nyes"},
		{[]string{"config", "get", "port"}, "port\n0"},
		{[]string{"config", "get", "tls-port"}, "tls-port\n6379"},
		{[]string{"config", "get", "protected-mode"}, "protected-mode\nyes"},
		{[]string{"acl", "users"}, "default\n" + addonID + "\n" + sandboxID + "\n" + catalogID}, // sorted
	} {
		if out, code := admin(check.args...); code != 0 || out != check.want {
			t.Fatalf("%v: exit %d: %q, want %q", check.args, code, out, check.want)
		}
	}
	// The socket takes no one either, now.
	if out, _ := lab.exec(pod, nil, "valkey-cli", "-s", valkeySocket, "get", "k"); !strings.Contains(out, "NOAUTH") {
		t.Fatalf("the socket: %s", out)
	}

	// A removed grant: its session ends with the step, and it gets in no
	// more; the others' stay.
	session := lab.network + "-session"
	lab.must(t, time.Minute, "run", "-d", "--name", session, "--label", dockerLabel, "--network", lab.network,
		"-v", certs+":/c:ro", "--entrypoint", "valkey-cli", inst.Image, "--tls", "--cacert", "/c/root.crt",
		"--cert", "/c/sandbox.crt", "--key", "/c/sandbox.key", "-h", addr, "-p", "6379", "blpop", "never", "120")
	t.Cleanup(func() { lab.docker(time.Minute, "rm", "-f", session) })
	for begin := time.Now(); ; time.Sleep(200 * time.Millisecond) {
		if out, _ := admin("client", "list"); strings.Contains(out, "user="+sandboxID) {
			break
		}
		if time.Since(begin) > 30*time.Second {
			t.Fatal("no session of the sandbox")
		}
	}
	c.TLS.Grants = grants[:1]
	lab.provision(t, pod, b.Provision(a, c), creds)
	if out, _, _ := lab.docker(time.Minute, "wait", session); strings.TrimSpace(out) == "0" {
		logs, _, _ := lab.docker(time.Minute, "logs", session)
		t.Fatalf("the removed grant's session outlived it: %s", logs)
	}
	if out, _ := tls("sandbox", "get", "k"); !strings.Contains(out, "NOAUTH") {
		t.Fatalf("the removed grant: %s", out)
	}
	if out, code := tls("catalog", "get", "k"); code != 0 || out != "v" {
		t.Fatalf("the kept grant: exit %d: %s", code, out)
	}

	// A renewed certificate is in effect at once, without a restart.
	serial := func() string {
		out, _ := lab.clientWith(certs, "sh", inst.Image, "-c", `printf '' | timeout 5 openssl s_client -connect "[`+addr+
			`]:6379" -cert /c/catalog.crt -key /c/catalog.key 2>/dev/null | openssl x509 -noout -serial`)
		return strings.TrimSpace(out)
	}
	was := serial()
	renewed, _ := ca.leafFor(serverKey, "vkpod", "wl-00000000000000000000000000000000", addonID,
		x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth)
	c.TLS.CertPEM = renewed
	lab.provision(t, pod, b.Provision(a, c), creds)
	if now := serial(); now == was || !strings.HasPrefix(now, "serial=") {
		t.Fatalf("the server's certificate after the renewal: %s, before %s", now, was)
	}

	// A node-side stop is a kill: TLS and the users from the volume, the
	// same data, and the steps still silent.
	lab.must(t, time.Minute, "kill", pod)
	restarted := time.Now().UTC().Format(time.RFC3339Nano)
	lab.must(t, time.Minute, "start", pod)
	lab.waitReady(t, pod, inst.Probe)
	addr = lab.address(t, pod)
	c.Address = addr
	if out, code := tls("catalog", "get", "k"); code != 0 || out != "v" {
		t.Fatalf("after a restart: exit %d: %s", code, out)
	}
	lab.provision(t, pod, b.Provision(a, c), creds)
	for range 3 {
		lab.waitReady(t, pod, inst.Probe)
	}
	// Neither the probe, which ran all along, nor the steps, nor a
	// workload's session leave a complaint in the log.
	logs, logErr, _ := lab.docker(time.Minute, "logs", "--since", restarted, pod)
	for _, line := range strings.Split(logs+logErr, "\n") {
		if strings.Contains(line, "Error") || strings.Contains(line, "ACL") || strings.Contains(line, "Unable") {
			t.Fatalf("a complaint in the log after the restart: %s\n%s%s", line, logs, logErr)
		}
	}
	// The entrypoint dropped to valkey and handed /data to it.
	if out, code := lab.exec(pod, nil, "stat", "-c", "%U %a", "/data/appendonlydir", valkeyDir, valkeyDir+"/server.key",
		valkeyDir+"/users.acl"); code != 0 || strings.Fields(out)[0] != "valkey" ||
		strings.Join(strings.Fields(out), " ") != "valkey 700 valkey 700 valkey 600 valkey 600" {
		t.Fatalf("owners and modes: exit %d: %s", code, out)
	}
}

func TestNATSDocker(t *testing.T) {
	lab := newDockerLab(t, 3)
	lab.ensureImage(t, natsBox)
	b, _ := Get("nats")
	for _, jetstream := range []bool{true, false} {
		t.Run("jetstream="+strconv.FormatBool(jetstream), func(t *testing.T) {
			a := addon(t, "bus", `{"type":"nats","version":"2","plan":"dev","options":{"jetstream":`+
				strconv.FormatBool(jetstream)+`}}`)
			if fs := b.Validate(nil, a); len(fs) != 0 {
				t.Fatalf("findings:\n%s", findingText(fs))
			}
			creds := credentials(t, b, a)
			c := Context{Env: "test", Name: a.Name}
			inst, err := b.Render(a, c)
			if err != nil {
				t.Fatal(err)
			}
			lab.ensureImage(t, inst.Image)
			dir := ""
			if inst.Mount != "" {
				dir = lab.volume(t, inst.Image)
			}
			pod := lab.start(t, "nats-"+strconv.FormatBool(jetstream), inst, resolveEnv(t, inst.Env, creds),
				b.Plans()[a.Plan].MemoryMib, dir)
			t.Logf("ready after %s", lab.waitReady(t, pod, inst.Probe).Round(time.Millisecond))
			lab.provision(t, pod, b.Provision(a, c), creds)
			lab.provision(t, pod, b.Provision(a, c), creds)

			c.Address = lab.address(t, pod)
			nats := func(url string, args ...string) (string, int) {
				out, code := lab.client("nats", natsBox, append([]string{"--server", url}, args...)...)
				return redact(out, creds), code
			}
			u := outputURL(t, b, a, c, nil, creds)
			t.Logf("url %s", redact(u, creds))
			if out, code := nats(u, "server", "check", "connection"); code != 0 || !strings.Contains(out, "OK Connection OK") {
				t.Fatalf("connect: exit %d: %s", code, out)
			}
			if out, code := nats(u, "pub", "orders.new", "hello"); code != 0 {
				t.Fatalf("publish: exit %d: %s", code, out)
			}
			if jetstream {
				if out, code := nats(u, "stream", "add", "ORDERS", "--subjects", "orders.>", "--storage", "file",
					"--defaults"); code != 0 {
					t.Fatalf("stream add: exit %d: %s", code, out)
				}
				if out, code := nats(u, "pub", "orders.new", "hello"); code != 0 {
					t.Fatalf("publish: exit %d: %s", code, out)
				}
				if out, code := nats(u, "stream", "info", "ORDERS", "--json"); code != 0 ||
					!strings.Contains(out, `"messages": 1`) {
					t.Fatalf("stream info: exit %d: %s", code, out)
				}
				if out, code := lab.exec(pod, nil, "ls", "/data/jetstream"); code != 0 {
					t.Fatalf("the store is not on the volume: exit %d: %s", code, out)
				}
			} else if out, code := nats(u, "stream", "ls"); code == 0 || !strings.Contains(out, "no responders") {
				// Nothing serves JetStream's API.
				t.Fatalf("streams without JetStream: exit %d: %s", code, out)
			} else {
				t.Logf("streams without JetStream: exit %d: %s", code, strings.TrimSpace(out))
			}
			if out, code := nats(outputURL(t, b, a, c, nil, map[string]string{"password": "wrong"}),
				"server", "check", "connection"); code == 0 || !strings.Contains(out, "Authorization Violation") {
				t.Fatalf("a wrong password: exit %d: %s", code, out)
			} else {
				t.Logf("a wrong password: exit %d: %s", code, strings.TrimSpace(out))
			}
			// The monitor (no authentication) is on loopback only.
			if out, code := lab.client("wget", inst.Image, "-qO-", "-T", "3",
				"http://["+c.Address+"]:8222/healthz"); code == 0 || !strings.Contains(out, "Connection refused") {
				t.Fatalf("the monitor answers env peers: exit %d: %s", code, out)
			} else {
				t.Logf("the monitor from a peer: exit %d: %s", code, strings.TrimSpace(out))
			}
		})
	}
}
