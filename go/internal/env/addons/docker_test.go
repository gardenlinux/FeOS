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
func (p *testPKI) leaf(pod, workload, id string, usage x509.ExtKeyUsage) (certPEM, keyPEM string) {
	p.t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		p.t.Fatal(err)
	}
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
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}, p.root, pub, p.key)
	if err != nil {
		p.t.Fatal(err)
	}
	k, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		p.t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: k}))
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
				{"the reference's url, no certificate", outputURL(t, b, a, c, map[string]string{"role": "expense_svc"}, creds),
					"connection requires a valid client certificate"},
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

func TestValkeyDocker(t *testing.T) {
	lab := newDockerLab(t, 2)
	b, _ := Get("valkey")
	for _, version := range b.Versions() {
		t.Run(version, func(t *testing.T) {
			a := addon(t, "cache", `{"type":"valkey","version":"`+version+`","plan":"dev",
				"options":{"maxmemoryPolicy":"allkeys-lru"}}`)
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
			pod := lab.start(t, "valkey"+version, inst, resolveEnv(t, inst.Env, creds), b.Plans()[a.Plan].MemoryMib,
				lab.volume(t, inst.Image))
			t.Logf("ready after %s", lab.waitReady(t, pod, inst.Probe).Round(time.Millisecond))
			// The probe fails on a wrong password: valkey-cli exits 0 on
			// NOAUTH, the grep does not.
			if out, code := lab.exec(pod, map[string]string{"VALKEY_PASSWORD": "wrong"}, inst.Probe...); code == 0 {
				t.Fatalf("the probe passed with a wrong password: %s", out)
			} else {
				t.Logf("the probe with a wrong password: exit %d: %s", code, strings.TrimSpace(out))
			}
			lab.provision(t, pod, b.Provision(a, c), creds)
			lab.provision(t, pod, b.Provision(a, c), creds)

			c.Address = lab.address(t, pod)
			cli := func(url string, args ...string) (string, int) {
				out, code := lab.client("valkey-cli", inst.Image, append([]string{"--no-auth-warning", "-u", url}, args...)...)
				return strings.TrimSpace(redact(out, creds)), code
			}
			u := outputURL(t, b, a, c, nil, creds)
			t.Logf("url %s", redact(u, creds))
			for _, check := range []struct {
				args []string
				want string
			}{
				{[]string{"ping"}, "PONG"},
				{[]string{"set", "persisted", "yes"}, "OK"},
				{[]string{"get", "persisted"}, "yes"},
				{[]string{"config", "get", "maxmemory"}, "maxmemory\n" + strconv.Itoa(89<<20)},
				{[]string{"config", "get", "maxmemory-policy"}, "maxmemory-policy\nallkeys-lru"},
				{[]string{"config", "get", "appendonly"}, "appendonly\nyes"},
				{[]string{"config", "get", "dir"}, "dir\n/data"},
				{[]string{"acl", "whoami"}, "default"},
			} {
				if out, code := cli(u, check.args...); code != 0 || out != check.want {
					t.Fatalf("%v: exit %d: %q, want %q", check.args, code, out, check.want)
				}
			}
			if out, _ := cli(outputURL(t, b, a, c, nil, map[string]string{"password": "wrong"}), "ping"); out == "PONG" ||
				!strings.Contains(out, "WRONGPASS") {
				t.Fatalf("a wrong password: %s", out)
			} else {
				t.Logf("a wrong password: %s", strings.ReplaceAll(out, "\n", " / "))
			}
			// The entrypoint dropped to valkey and handed /data to it.
			if out, code := lab.exec(pod, nil, "stat", "-c", "%U", "/data/appendonlydir"); code != 0 ||
				strings.TrimSpace(out) != "valkey" {
				t.Fatalf("appendonlydir: exit %d: %s", code, out)
			}
		})
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
