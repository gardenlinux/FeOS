// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
)

// The sandbox the Expensa spec's attached service runs in.
const webSandbox = "0193a000-0000-7000-8000-00000000000a"

// Another attachable sandbox of the tenant.
const otherSandbox = "0193a000-0000-7000-8000-00000000000b"

// An attachable sandbox of the tenant with a short ID (ADR 0019); the two
// above have UUIDs, as sandboxes made before do.
const shortSandbox = "k3x9m2p7q4r"

// fakeBlueprint is an addon type whose versions and keys are listed.
type fakeBlueprint struct {
	versions []string
	keys     []string
	fields   []string
	port     int
}

func (b fakeBlueprint) Validate(name string, prev *Addon, next Addon) []Finding {
	var fs []Finding
	if !slices.Contains(b.versions, next.Version) {
		fs = append(fs, Finding{Severity: SeverityError, Path: "version", Problem: "no version " + next.Version,
			Remedy: "use " + strings.Join(b.versions, ", ")})
	}
	if prev != nil && prev.Version != next.Version {
		fs = append(fs, Finding{Severity: SeverityError, Path: "version", Problem: "a major version change",
			Remedy: "keep the version"})
	}
	if next.Options["bad"] != nil {
		fs = append(fs, Finding{Severity: SeverityError, Path: "options.bad", Problem: "bad", Remedy: "remove it"})
	}
	if next.Options["whole"] != nil {
		fs = append(fs, Finding{Severity: SeverityWarning, Path: "", Problem: "whole", Remedy: ""})
	}
	return fs
}

func (b fakeBlueprint) Port(string, Addon) int { return b.port }

func (b fakeBlueprint) Ref(name string, a Addon, key string, fields map[string]string) error {
	if !slices.Contains(b.keys, key) {
		return fmt.Errorf("%s has no key %q", a.Type, key)
	}
	for f := range fields {
		if !slices.Contains(b.fields, f) {
			return fmt.Errorf("a reference to %s has no field %q", a.Type, f)
		}
	}
	return nil
}

// fakeVC answers Validate from maps.
type fakeVC struct {
	mode      Mode
	sandboxes map[string]SandboxInfo
	secrets   map[string]SecretInfo
	// badImages fail to resolve with their error text.
	badImages map[string]string
	types     bool
	// volumes are the environment's volumes by addon name.
	volumes map[string]AddonData
}

func (f fakeVC) Mode() Mode { return f.mode }
func (f fakeVC) Sandbox(id string) (SandboxInfo, bool) {
	s, ok := f.sandboxes[id]
	return s, ok
}
func (f fakeVC) Secret(name string) (SecretInfo, bool) {
	s, ok := f.secrets[name]
	return s, ok
}
func (f fakeVC) Volume(addon string) (AddonData, bool) {
	d, ok := f.volumes[addon]
	return d, ok
}
func (f fakeVC) Blueprint(typ string) (Blueprint, bool) {
	switch typ {
	case "postgres":
		return fakeBlueprint{versions: []string{"16", "17"}, keys: []string{"url", "host", "port", "password"},
			fields: []string{"role", "database"}, port: 5432}, true
	case "nats":
		return fakeBlueprint{versions: []string{"2"}, keys: []string{"url", "host"}, port: 4222}, true
	}
	return nil, false
}
func (f fakeVC) Image(image string) (string, error) {
	if IsDigestRef(image) {
		return image, nil
	}
	if e, ok := f.badImages[image]; ok {
		return "", errors.New(e)
	}
	return pinnedOf(image), nil
}

// fakeTypesVC also lists its addon types.
type fakeTypesVC struct{ fakeVC }

func (fakeTypesVC) BlueprintTypes() []string { return []string{"nats", "postgres"} }

// pinnedOf is the digest reference the fake resolves an image to.
func pinnedOf(image string) string {
	repo, _, _ := strings.Cut(image, ":")
	return repo + "@sha256:" + strings.Repeat(fmt.Sprintf("%x", len(image)%16), 64)
}

func defaultVC(mode Mode) fakeVC {
	return fakeVC{mode: mode,
		sandboxes: map[string]SandboxInfo{webSandbox: {ID: webSandbox},
			otherSandbox: {ID: otherSandbox}, shortSandbox: {ID: shortSandbox}},
		secrets: map[string]SecretInfo{"internal-token": {Latest: 3, Versions: []int{2, 3}, Deleted: []int{1}}},
	}
}

// expect is one finding a case wants: severity, path and a part of the
// problem.
type expect struct {
	sev     Severity
	path    string
	problem string
}

func e(path, problem string) expect { return expect{SeverityError, path, problem} }
func w(path, problem string) expect { return expect{SeverityWarning, path, problem} }
func i(path, problem string) expect { return expect{SeverityInfo, path, problem} }

func checkFindings(t *testing.T, fs []Finding, want []expect) {
	t.Helper()
	if len(fs) != len(want) {
		t.Fatalf("findings\n%s\nwant %d: %+v", dump(fs), len(want), want)
	}
	for n, x := range want {
		f := fs[n]
		if f.Severity != x.sev || f.Path != x.path || !strings.Contains(f.Problem, x.problem) {
			t.Fatalf("finding %d\n%s\nwant %+v", n, dump(fs), x)
		}
		if f.Severity != SeverityInfo && f.Remedy == "" && f.Path != "addons.postgres" {
			t.Fatalf("finding without a remedy: %+v", f)
		}
	}
}

func dump(fs []Finding) string {
	var b strings.Builder
	for _, f := range fs {
		fmt.Fprintf(&b, "  %s %s: %s — %s\n", f.Severity, f.Path, f.Problem, f.Remedy)
	}
	return b.String()
}

// mutate parses the Expensa spec, changes it and validates it.
func validateMutated(t *testing.T, vc ValidateContext, prev *Spec, change func(s *Spec)) []Finding {
	t.Helper()
	s := mustParse(t, expensa)
	if change != nil {
		change(&s)
	}
	return Validate(vc, s, prev)
}

func intp(n int) *int { return &n }

func svc(s *Spec, name string, f func(*Service)) {
	v := s.Services[name]
	f(&v)
	s.Services[name] = v
}

func TestValidateExpensa(t *testing.T) {
	// approval-service as an image service makes the §3 example whole.
	approval := func(s *Spec) {
		s.Services["approval-service"] = Service{Image: "ghcr.io/acme/approval:1.2", Ports: []Port{{Name: "http", Port: 8080}}}
	}
	for _, mode := range []Mode{ModePlan, ModeApply} {
		fs := validateMutated(t, defaultVC(mode), nil, approval)
		checkFindings(t, fs, nil)
	}
	// Env-less: no secret checks; the sandbox is still checked.
	vc := defaultVC(ModeSpec)
	vc.secrets = nil
	checkFindings(t, validateMutated(t, vc, nil, approval), nil)
	vc.sandboxes = nil
	checkFindings(t, validateMutated(t, vc, nil, approval), []expect{e("services.web.sandbox", "no sandbox")})
	// The fixtures' stage spec too.
	s, fs := Parse(readFile(t, "testdata/expensa-stage-request.json"))
	checkFindings(t, append(fs, Validate(defaultVC(ModeApply), s, nil)...), nil)
	s, fs = Parse(readFile(t, "testdata/expensa-stage-deployment7.json"))
	checkFindings(t, append(fs, Validate(defaultVC(ModeApply), s, &s)...), nil)
}

func TestValidateRules(t *testing.T) {
	approval := func(s *Spec) {
		s.Services["approval-service"] = Service{Image: "ghcr.io/acme/approval:1.2", Ports: []Port{{Name: "http", Port: 8080}}}
	}
	cases := []struct {
		name   string
		mode   Mode
		change func(s *Spec)
		want   []expect
	}{
		{"missing service reference", ModePlan, nil, []expect{e("services.expense-service.env.EXPENSA_APPROVAL_URL",
			"no service approval-service")}},
		{"limits", ModePlan, func(s *Spec) {
			approval(s)
			for n := range 65 {
				s.Services[fmt.Sprintf("s%d", n)] = Service{Image: "img"}
			}
			for n := range 129 {
				s.Routes[fmt.Sprintf("r%d", n)] = Route{Path: fmt.Sprintf("/r%d", n), Service: "web", Port: PortRef{Name: "http"}}
			}
		}, []expect{e("routes", "131 routes; at most 128"), e("services", "68 services; at most 64")}},
		{"volumes", ModePlan, func(s *Spec) {
			approval(s)
			s.Volumes = []byte(`{}`)
			svc(s, "expense-service", func(v *Service) { v.Mounts = []byte(`[]`) })
		}, []expect{e("services.expense-service.mounts", "volumes for services are not available yet"),
			e("volumes", "volumes for services are not available yet")}},
		{"names", ModePlan, func(s *Spec) {
			approval(s)
			s.Services["Bad_Name"] = Service{Image: "img"}
			s.Jobs["-j"] = Job{Image: "img"}
			s.Addons["a.b"] = Addon{Type: "nats", Version: "2"}
			s.Routes["R"] = Route{Path: "/r", Service: "web", Port: PortRef{Name: "http"}}
		}, []expect{e("addons.a.b", "not a DNS label"), e("jobs.-j", "not a DNS label"), e("routes.R", "not a DNS label"),
			e("services.Bad_Name", "not a DNS label")}},
		{"both words of a deploy job", ModePlan, func(s *Spec) {
			approval(s)
			s.Jobs["deploy"] = Job{Image: "img", On: JobOnDeploy}
			s.Jobs["release"] = Job{Image: "img", On: JobOnRelease}
		}, nil},
		{"service and addon share a name", ModePlan, func(s *Spec) {
			approval(s)
			s.Services["nats"] = Service{Image: "img"}
		}, []expect{e("services.nats", "share the environment's names")}},
		{"jobs may share names", ModePlan, func(s *Spec) {
			approval(s)
			s.Jobs["nats"] = Job{Image: "img"}
			s.Jobs["web"] = Job{Image: "img"}
		}, nil},
		{"image service needs an image", ModePlan, func(s *Spec) {
			approval(s)
			svc(s, "expense-service", func(v *Service) { v.Image, v.Runtime = "", "" })
		}, []expect{e("services.expense-service.image", "needs an image")}},
		{"image service fields of attached", ModePlan, func(s *Spec) {
			approval(s)
			svc(s, "expense-service", func(v *Service) { v.Sandbox, v.Cwd = webSandbox, "/x" })
		}, []expect{e("services.expense-service.cwd", "for attached"), e("services.expense-service.sandbox", "for attached")}},
		{"unknown runtime", ModePlan, func(s *Spec) {
			approval(s)
			svc(s, "approval-service", func(v *Service) { v.Runtime = "vm" })
		}, []expect{e("services.approval-service.runtime", `runtime "vm" is unknown`)}},
		{"ranges", ModePlan, func(s *Spec) {
			approval(s)
			svc(s, "expense-service", func(v *Service) {
				v.Replicas = intp(21)
				v.Resources = &Resources{VCPUs: 33, MemoryMib: 64}
				v.Rollout = &Rollout{Timeout: "-1m"}
			})
			s.Jobs["migrate"] = Job{Image: "img", Retries: 6, Timeout: "3h", On: "never", Resources: &Resources{VCPUs: -1}}
		}, []expect{e("jobs.migrate.on", `on "never"`), e("jobs.migrate.resources.vcpus", "out of range"),
			e("jobs.migrate.retries", "out of range"), e("jobs.migrate.timeout", "out of range"),
			e("services.expense-service.replicas", "replicas 21"), e("services.expense-service.resources.memoryMib", "64"),
			e("services.expense-service.resources.vcpus", "33"), e("services.expense-service.rollout.timeout", "not positive")}},
		{"zero replicas and bounds", ModePlan, func(s *Spec) {
			approval(s)
			svc(s, "expense-service", func(v *Service) {
				v.Replicas = intp(0)
				v.Resources = &Resources{VCPUs: 32, MemoryMib: 256}
			})
		}, nil},
		{"idle", ModePlan, func(s *Spec) {
			approval(s)
			svc(s, "approval-service", func(v *Service) { v.Idle = "15m" })
			svc(s, "expense-service", func(v *Service) { v.Idle = "30s" })
		}, []expect{e("services.approval-service.idle", "nothing would wake it: no route targets approval-service"),
			e("services.expense-service.idle", "shorter than 1m")}},
		{"bad durations", ModePlan, func(s *Spec) {
			approval(s)
			svc(s, "expense-service", func(v *Service) {
				v.Idle = "soon"
				v.Readiness.Period = "500ms"
				v.Readiness.InitialDelay = "-1s"
				v.Readiness.FailureThreshold = -1
			})
		}, []expect{e("services.expense-service.idle", `"soon" is not a duration`),
			e("services.expense-service.readiness.failureThreshold", "negative"), e("services.expense-service.readiness.initialDelay", "negative"),
			e("services.expense-service.readiness.period", "shorter than 1s")}},
		{"attached service rules", ModeSpec, func(s *Spec) {
			approval(s)
			svc(s, "web", func(v *Service) {
				v.Command = nil
				v.Replicas = intp(2)
				v.Resources = &Resources{VCPUs: 1}
				v.Idle = "30s"
				v.Rollout = &Rollout{}
				v.Image = "img"
				v.Cwd = "relative"
			})
		}, []expect{e("services.web.command", "an attached service needs a command"),
			e("services.web.cwd", "not an absolute path"), e("services.web.idle", "shorter than 1m"),
			e("services.web.image", "runs no image"), e("services.web.replicas", "an attached service runs once"),
			e("services.web.resources", "sandbox's resources"), e("services.web.rollout", "no rollout")}},
		{"attached replicas 1", ModePlan, func(s *Spec) {
			approval(s)
			svc(s, "web", func(v *Service) { v.Replicas = intp(1) })
		}, nil},
		{"attached idle", ModePlan, func(s *Spec) {
			// A duration of 1m or more, or never; no route needed: the
			// sandbox's own activity wakes it.
			approval(s)
			svc(s, "web", func(v *Service) { v.Idle = "15m" })
			s.Services["web2"] = Service{Runtime: RuntimeAttached, Command: []string{"x"}, Sandbox: webSandbox, Idle: IdleNever}
			s.Services["web3"] = Service{Runtime: RuntimeAttached, Command: []string{"x"}, Sandbox: webSandbox, Idle: "soon"}
		}, []expect{e("services.web3.idle", `"soon" is not a duration`)}},
		{"never is for attached services", ModeSpec, func(s *Spec) {
			approval(s)
			svc(s, "expense-service", func(v *Service) { v.Idle = IdleNever })
		}, []expect{e("services.expense-service.idle", `"never" is not a duration`)}},
		{"attached sandbox", ModePlan, func(s *Spec) {
			approval(s)
			s.Services["web2"] = Service{Runtime: RuntimeAttached, Command: []string{"x"}}
			s.Services["web3"] = Service{Runtime: RuntimeAttached, Command: []string{"x"}, Sandbox: "nope"}
			s.Services["web4"] = Service{Runtime: RuntimeAttached, Command: []string{"x"},
				Sandbox: "0193a000-0000-7000-8000-0000000000ff"}
			s.Services["web5"] = Service{Runtime: RuntimeAttached, Command: []string{"x"},
				Sandbox: strings.ToUpper(webSandbox), Ports: []Port{{Name: "http", Port: 5174}}}
			s.Services["web6"] = Service{Runtime: RuntimeAttached, Command: []string{"x"}, Sandbox: shortSandbox}
			s.Services["web7"] = Service{Runtime: RuntimeAttached, Command: []string{"x"}, Sandbox: "zzzzzzzzzzz"}
			s.Services["web8"] = Service{Runtime: RuntimeAttached, Command: []string{"x"},
				Sandbox: strings.ToUpper(shortSandbox)}
		}, []expect{e("services.web2.sandbox", "needs a sandbox"), e("services.web3.sandbox", "not a sandbox ID"),
			e("services.web4.sandbox", "no sandbox 0193a000-0000-7000-8000-0000000000ff of yours"),
			e("services.web7.sandbox", "no sandbox zzzzzzzzzzz of yours"),
			e("services.web8.sandbox", "not a sandbox ID")}},
		{"ports", ModePlan, func(s *Spec) {
			approval(s)
			svc(s, "approval-service", func(v *Service) {
				v.Ports = []Port{{Name: "http", Port: 8080}, {Name: "http", Port: 0}, {Name: "", Port: 8080}, {Name: "A", Port: 70000}}
			})
		}, []expect{e("services.approval-service.ports[1].name", "given twice"),
			e("services.approval-service.ports[1].port", "port 0 is out of range"),
			e("services.approval-service.ports[2].name", "needs a name"),
			e("services.approval-service.ports[2].port", "port 8080 is given twice"),
			e("services.approval-service.ports[3].name", "not a DNS label"),
			e("services.approval-service.ports[3].port", "out of range")}},
		{"readiness", ModePlan, func(s *Spec) {
			approval(s)
			svc(s, "expense-service", func(v *Service) {
				v.Readiness = &Readiness{HTTP: &HTTPCheck{Port: PortRef{Name: "grpc"}, Path: "readyz"}, TCP: &TCPCheck{Port: PortRef{Number: 8080}}}
			})
			svc(s, "approval-service", func(v *Service) { v.Readiness = &Readiness{Exec: &ExecCheck{}} })
			svc(s, "web", func(v *Service) { v.Readiness = &Readiness{} })
		}, []expect{e("services.approval-service.readiness.exec.command", "needs a command"),
			e("services.expense-service.readiness", "exactly one of http, tcp and exec, not 2"),
			e("services.expense-service.readiness.http.path", "does not begin with /"),
			e("services.expense-service.readiness.http.port", "has no port grpc"),
			e("services.web.readiness", "not 0")}},
		{"env references", ModePlan, func(s *Spec) {
			approval(s)
			svc(s, "expense-service", func(v *Service) {
				v.Env["1BAD"] = String("x")
				v.Env["NO_ADDON"] = EnvValue{Kind: EnvAddon, Addon: "mysql", Key: "url"}
				v.Env["BAD_KEY"] = EnvValue{Kind: EnvAddon, Addon: "postgres", Key: "dsn"}
				v.Env["BAD_FIELD"] = EnvValue{Kind: EnvAddon, Addon: "nats", Key: "url", Fields: map[string]string{"role": "r"}}
				v.Env["SVC_KEY"] = EnvValue{Kind: EnvService, Service: "approval-service", Port: PortRef{Name: "http"}, Key: "ip"}
				v.Env["SVC_PORT"] = EnvValue{Kind: EnvService, Service: "approval-service", Port: PortRef{Name: "grpc"}, Key: "url"}
				v.Env["SVC_NOPORT"] = EnvValue{Kind: EnvService, Service: "approval-service", Key: "port"}
				v.Env["SVC_HOST"] = EnvValue{Kind: EnvService, Service: "approval-service", Key: "host"}
				v.Env["SVC_NUM"] = EnvValue{Kind: EnvService, Service: "approval-service", Port: PortRef{Number: 8080}, Key: "url"}
				v.Env["TO_ATTACHED"] = EnvValue{Kind: EnvService, Service: "web", Port: PortRef{Name: "http"}, Key: "url"}
			})
		}, []expect{e("services.expense-service.env.1BAD", "not an environment variable's name"),
			e("services.expense-service.env.BAD_FIELD", `has no field "role"`),
			e("services.expense-service.env.BAD_KEY", `has no key "dsn"`),
			e("services.expense-service.env.NO_ADDON", "no addon mysql"),
			e("services.expense-service.env.SVC_KEY", `not "ip"`),
			e("services.expense-service.env.SVC_NOPORT", "a port is needed"),
			e("services.expense-service.env.SVC_PORT", "approval-service has no port grpc")}},
		{"attached may reference attached and image", ModePlan, func(s *Spec) {
			approval(s)
			s.Services["api"] = Service{Runtime: RuntimeAttached, Sandbox: webSandbox, Command: []string{"x"},
				Ports: []Port{{Name: "http", Port: 8081}}}
			svc(s, "web", func(v *Service) {
				v.Env = map[string]EnvValue{"API": {Kind: EnvService, Service: "api", Port: PortRef{Name: "http"}, Key: "url"},
					"APPROVAL": {Kind: EnvService, Service: "approval-service", Port: PortRef{Name: "http"}, Key: "url"}}
			})
		}, nil},
		// ADR 0035: an attached service is a name of the environment with
		// its sandbox's address, reached where its sandbox's groups let in.
		{"attached references attached in another sandbox", ModePlan, func(s *Spec) {
			approval(s)
			s.Services["api"] = Service{Runtime: RuntimeAttached, Sandbox: otherSandbox, Command: []string{"x"},
				Ports: []Port{{Name: "http", Port: 8081}}}
			svc(s, "web", func(v *Service) {
				v.Env = map[string]EnvValue{"API": {Kind: EnvService, Service: "api", Port: PortRef{Name: "http"}, Key: "url"}}
			})
		}, nil},
		{"attached references attached in its sandbox written in capitals", ModePlan, func(s *Spec) {
			approval(s)
			s.Services["api"] = Service{Runtime: RuntimeAttached, Sandbox: strings.ToUpper(webSandbox), Command: []string{"x"},
				Ports: []Port{{Name: "http", Port: 8081}}}
			svc(s, "web", func(v *Service) {
				v.Env = map[string]EnvValue{"API": {Kind: EnvService, Service: "api", Port: PortRef{Name: "http"}, Key: "url"}}
			})
		}, nil},
		{"job references attached", ModePlan, func(s *Spec) {
			approval(s)
			s.Jobs["migrate"] = Job{Image: "img", Env: map[string]EnvValue{"W": {Kind: EnvService, Service: "web", Key: "host"}}}
		}, nil},
		{"job needs an image", ModePlan, func(s *Spec) {
			approval(s)
			s.Jobs["seed"] = Job{On: JobOnManual}
		}, []expect{e("jobs.seed.image", "a job needs an image")}},
		{"secrets", ModePlan, func(s *Spec) {
			approval(s)
			svc(s, "expense-service", func(v *Service) {
				v.Env["NONE"] = SecretEnv("absent")
				v.Env["SYS"] = SecretEnv("addon.postgres.password")
				v.Env["GONE"] = SecretEnv("gone")
			})
			s.Secrets["unused"] = SecretRef{Version: 9}
			s.Secrets["Bad"] = SecretRef{}
		}, []expect{e("secrets.Bad", "not a secret name"), e("services.expense-service.env.GONE", "secret gone was deleted"),
			e("services.expense-service.env.NONE", "no secret absent"),
			e("services.expense-service.env.SYS", "not a secret name you can use")}},
		{"secret versions", ModeApply, func(s *Spec) {
			approval(s)
			s.Secrets["internal-token"] = SecretRef{Version: 1}
		}, []expect{e("secrets.internal-token", "version 1 of secret internal-token was deleted")}},
		{"secret version absent", ModeApply, func(s *Spec) {
			approval(s)
			s.Secrets["internal-token"] = SecretRef{Version: 7}
		}, []expect{e("secrets.internal-token", "secret internal-token has no version 7")}},
		{"secret version negative", ModeApply, func(s *Spec) {
			approval(s)
			s.Secrets["internal-token"] = SecretRef{Version: -1}
		}, []expect{e("secrets.internal-token.version", "negative")}},
		{"secrets in spec mode", ModeSpec, func(s *Spec) {
			approval(s)
			s.Secrets["internal-token"] = SecretRef{Version: 99}
			svc(s, "expense-service", func(v *Service) { v.Env["NONE"] = SecretEnv("absent") })
		}, nil},
		{"addons", ModePlan, func(s *Spec) {
			approval(s)
			s.Addons["cache"] = Addon{Type: "valkey", Version: "8"}
			s.Addons["notype"] = Addon{}
			s.Addons["postgres"] = Addon{Type: "postgres", Version: "15", Options: map[string]any{"bad": true, "whole": 1}}
		}, []expect{e("addons.cache.type", `no addon type "valkey"`), e("addons.notype.type", "needs a type"),
			w("addons.postgres", "whole"), e("addons.postgres.options.bad", "bad"), e("addons.postgres.version", "no version 15")}},
		{"routes", ModePlan, func(s *Spec) {
			approval(s)
			s.Routes["a"] = Route{Path: "api", Service: "web", Port: PortRef{Name: "http"}}
			s.Routes["b"] = Route{Path: "/api/expense/", Service: "expense-service", Port: PortRef{Name: "http"}}
			s.Routes["c"] = Route{Path: "/x?y", Service: "nope", Port: PortRef{Name: "http"}}
			s.Routes["d"] = Route{Path: "/x/../y", Service: "web", Port: PortRef{Name: "grpc"}}
			s.Routes["e"] = Route{Path: "/e", Service: "web", Host: "example.com", Port: PortRef{Number: 5173}}
			s.Routes["f"] = Route{Path: "/f//g", Service: "web"}
		}, []expect{e("routes.a.path", "does not begin with /"), e("routes.b.path", "is route api-expense's too"),
			e("routes.c.path", "needs escaping"), e("routes.c.service", `no service "nope"`),
			e("routes.d.path", "segment"), e("routes.d.port", "web has no port grpc"), i("routes.e.host", "not used"),
			e("routes.f.path", "segment"), e("routes.f.port", "a port is needed")}},
		{"images", ModePlan, func(s *Spec) {
			approval(s)
			svc(s, "approval-service", func(v *Service) { v.Image = "ghcr.io/acme/missing:1" })
		}, []expect{w("services.approval-service.image", "image ghcr.io/acme/missing:1 could not be resolved: manifest unknown")}},
		{"images on apply", ModeApply, func(s *Spec) {
			approval(s)
			svc(s, "approval-service", func(v *Service) { v.Image = "ghcr.io/acme/missing:1" })
			s.Jobs["migrate"] = Job{Image: "ghcr.io/acme/missing:1"}
		}, []expect{e("jobs.migrate.image", "could not be resolved"), e("services.approval-service.image", "could not be resolved")}},
		{"digest images need no registry", ModeApply, func(s *Spec) {
			approval(s)
			svc(s, "approval-service", func(v *Service) {
				v.Image = "ghcr.io/acme/missing@sha256:" + strings.Repeat("a", 64)
				v.ImageAsWritten = "ghcr.io/acme/missing:1"
			})
		}, nil},
		{"attached port clash in the spec", ModePlan, func(s *Spec) {
			approval(s)
			s.Services["api"] = Service{Runtime: RuntimeAttached, Sandbox: webSandbox, Command: []string{"x"},
				Ports: []Port{{Name: "http", Port: 5173}}}
		}, []expect{e("services.web.ports[0].port", "port 5173 is attached service api's too")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			vc := defaultVC(c.mode)
			vc.secrets["gone"] = SecretInfo{Deleted: []int{1, 2}}
			vc.badImages = map[string]string{"ghcr.io/acme/missing:1": "manifest unknown"}
			checkFindings(t, validateMutated(t, vc, nil, c.change), c.want)
		})
	}
}

// fakeIngressVC also says the environment's default host (IngressHost).
type fakeIngressVC struct {
	fakeVC
	host       string
	public, ok bool
}

func (f fakeIngressVC) IngressHost() (string, bool, bool) { return f.host, f.public, f.ok }

// Which doors serve a route: info findings at the route while the
// platform has an apps zone, for each route whose target exists; none
// without a zone, or from a context that does not say. A stored host is
// noted either way.
func TestValidateRouteDoors(t *testing.T) {
	routes := func(s *Spec) {
		s.Services["approval-service"] = Service{Image: "ghcr.io/acme/approval:1.2", Ports: []Port{{Name: "http", Port: 8080}}}
		s.Routes["approval"] = Route{Path: "/api/approval/", Service: "approval-service", Port: PortRef{Name: "http"},
			Host: "approval.example.com"}
		s.Routes["nope"] = Route{Path: "/nope", Service: "nope", Port: PortRef{Name: "http"}}
	}
	const (
		host     = "stage-acme.apps.test"
		attached = "served by the route door alone: an attached service runs in a sandbox, which the ingress does not reach"
		later    = "served by the route door; the ingress serves it once the environment is public"
		both     = "served by the ingress and by the route door"
	)
	stored := i("routes.approval.host", "host is stored, not used yet: the ingress and the route door serve the route "+
		"on every host of the environment")
	nope := e("routes.nope.service", `no service "nope"`)
	cases := []struct {
		name string
		vc   ValidateContext
		want []expect
	}{
		{"no zone", fakeIngressVC{fakeVC: defaultVC(ModePlan), host: host, public: true}, []expect{stored, nope}},
		{"no IngressHost", defaultVC(ModePlan), []expect{stored, nope}},
		{"public with host", fakeIngressVC{fakeVC: defaultVC(ModePlan), host: host, public: true, ok: true}, []expect{
			i("routes.api-expense", "served by the ingress at https://stage-acme.apps.test/api/expense and by the route door"),
			i("routes.approval", "served by the ingress at https://stage-acme.apps.test/api/approval and by the route door"),
			stored, nope, i("routes.root", attached)}},
		{"public without host", fakeIngressVC{fakeVC: defaultVC(ModeApply), public: true, ok: true}, []expect{
			i("routes.api-expense", both), i("routes.approval", both), stored, nope, i("routes.root", attached)}},
		{"not public", fakeIngressVC{fakeVC: defaultVC(ModePlan), host: host, ok: true}, []expect{
			i("routes.api-expense", later), i("routes.approval", later), stored, nope, i("routes.root", attached)}},
		{"without an environment", fakeIngressVC{fakeVC: defaultVC(ModeSpec), ok: true}, []expect{
			i("routes.api-expense", later), i("routes.approval", later), stored, nope, i("routes.root", attached)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			checkFindings(t, validateMutated(t, c.vc, nil, routes), c.want)
		})
	}
	// The finding names the URL in full.
	fs := validateMutated(t, fakeIngressVC{fakeVC: defaultVC(ModePlan), host: host, public: true, ok: true}, nil, routes)
	if fs[0].Problem != "served by the ingress at https://stage-acme.apps.test/api/expense and by the route door" ||
		fs[0].Remedy != "" {
		t.Fatalf("finding %+v", fs[0])
	}
}

// F13: a route's path has at most MaxRoutePathBytes, since every
// instance of the ingress holds it. A longer one is refused with its
// length alone, whatever else is wrong with it: it is neither quoted nor
// compared with the other routes' paths, nor said to be served.
func TestValidateRoutePathLength(t *testing.T) {
	approval := func(s *Spec) {
		s.Services["approval-service"] = Service{Image: "ghcr.io/acme/approval:1.2", Ports: []Port{{Name: "http", Port: 8080}}}
	}
	route := func(path string) Route {
		return Route{Path: path, Service: "expense-service", Port: PortRef{Name: "http"}}
	}
	longest := "/" + strings.Repeat("a", MaxRoutePathBytes-1)
	// The attack's path: '&' passes the characters' check, and HTML
	// escaping made it six bytes on the wire.
	attack := "/" + strings.Repeat("&", 8130) + "123"
	fs := validateMutated(t, defaultVC(ModeApply), nil, func(s *Spec) {
		approval(s)
		s.Routes["longest"] = route(longest)
		s.Routes["long"] = route(longest + "b")
		s.Routes["twin"] = route(longest + "b")
		s.Routes["attack"] = route(attack)
		s.Routes["bad"] = route(strings.Repeat("?", MaxRoutePathBytes+1))
	})
	tooLong := func(n int) string {
		return fmt.Sprintf("the path has %d bytes; at most %d are allowed", n, MaxRoutePathBytes)
	}
	checkFindings(t, fs, []expect{e("routes.attack.path", tooLong(len(attack))), e("routes.bad.path", tooLong(MaxRoutePathBytes+1)),
		e("routes.long.path", tooLong(MaxRoutePathBytes+1)), e("routes.twin.path", tooLong(MaxRoutePathBytes+1))})
	for _, f := range fs {
		if len(f.Problem)+len(f.Remedy) > 200 {
			t.Errorf("finding of %d bytes: %.80s…", len(f.Problem)+len(f.Remedy), f.Problem)
		}
	}

	vc := fakeIngressVC{fakeVC: defaultVC(ModeApply), host: "stage-acme.apps.test", public: true, ok: true}
	fs = validateMutated(t, vc, nil, func(s *Spec) {
		approval(s)
		s.Routes["long"] = route(longest + "b")
	})
	for _, f := range fs {
		if f.Path == "routes.long" {
			t.Errorf("a refused path is said to be served: %.80s…", f.Problem)
		}
	}
}

func TestValidateContextDependent(t *testing.T) {
	approval := func(s *Spec) {
		s.Services["approval-service"] = Service{Image: "ghcr.io/acme/approval:1.2", Ports: []Port{{Name: "http", Port: 8080}}}
	}
	// A sandbox of another environment: an error (ADR 0035); without an
	// environment there is none to compare with.
	vc := defaultVC(ModePlan)
	vc.sandboxes[webSandbox] = SandboxInfo{ID: webSandbox, OtherEnvironment: "dev"}
	checkFindings(t, validateMutated(t, vc, nil, approval), []expect{e("services.web.sandbox",
		"sandbox "+webSandbox+" is in environment dev: an attached service runs in a sandbox of its own environment")})

	// An attached service is its sandbox's pod: no uses, no egress.
	vc = defaultVC(ModePlan)
	vc.sandboxes[webSandbox] = SandboxInfo{ID: webSandbox}
	checkFindings(t, validateMutated(t, vc, nil, func(s *Spec) {
		approval(s)
		web := s.Services["web"]
		web.Uses = []Use{{Service: "approval-service", Port: PortRef{Name: "http"}}}
		web.Egress = &Egress{Mode: EgressNone}
		s.Services["web"] = web
	}), []expect{e("services.web.egress", "an attached service runs in sandbox "+webSandbox+"'s pod and has its egress"),
		e("services.web.uses", "an attached service runs in sandbox "+webSandbox+"'s pod, which reaches what the "+
			"sandbox's reaches names")})

	// An addon against its previous self; a changed type deletes data.
	prev := mustParse(t, expensa)
	vc = defaultVC(ModePlan)
	checkFindings(t, validateMutated(t, vc, &prev, func(s *Spec) {
		approval(s)
		s.Addons["postgres"] = Addon{Type: "postgres", Version: "16", Options: s.Addons["postgres"].Options}
		s.Addons["nats"] = Addon{Type: "postgres", Version: "17"}
	}), []expect{w("addons.nats.type", "changes its type from nats to postgres"),
		e("addons.postgres.version", "a major version change")})
	// Env-less: no previous deployment.
	vc.mode = ModeSpec
	checkFindings(t, validateMutated(t, vc, &prev, func(s *Spec) {
		approval(s)
		s.Addons["postgres"] = Addon{Type: "postgres", Version: "16"}
	}), nil)

	// The registered types are named.
	tvc := fakeTypesVC{defaultVC(ModePlan)}
	fs := validateMutated(t, tvc, nil, func(s *Spec) {
		approval(s)
		s.Addons["cache"] = Addon{Type: "valkey"}
	})
	if len(fs) != 1 || fs[0].Remedy != "use one of: nats, postgres" {
		t.Fatalf("%s", dump(fs))
	}
}

// An addon is validated against the data its environment's volume holds
// when it has one (the controller keeps a volume by the addon's name, and
// mounts it whatever the latest deployment says), else against the latest
// deployment's addon.
func TestValidateAgainstVolume(t *testing.T) {
	prev := mustParse(t, expensa)
	pg16 := Addon{Type: "postgres", Version: "16"}
	pg17 := Addon{Type: "postgres", Version: "17"}
	withDB := func(a Addon) func(s *Spec) {
		return func(s *Spec) {
			s.Services["approval-service"] = Service{Image: "ghcr.io/acme/approval:1.2", Ports: []Port{{Name: "http", Port: 8080}}}
			s.Addons["db"] = a
		}
	}
	for _, c := range []struct {
		name   string
		latest *Addon
		volume *AddonData
		next   Addon
		mode   Mode
		want   []expect
	}{
		// The latest deployment removed db, but the controller has not yet
		// deleted its volume: re-added, the volume's data decides.
		{name: "removed then re-added", volume: &AddonData{Type: "postgres", Version: "16"}, next: pg17,
			want: []expect{e("addons.db.version", "a major version change")}},
		{name: "re-added as it was", volume: &AddonData{Type: "postgres", Version: "16"}, next: pg16},
		// postgres to nats and back before the controller acted: the
		// volume still holds postgres 16's data, nothing is deleted.
		{name: "type round trip", latest: &Addon{Type: "nats", Version: "2"}, volume: &AddonData{Type: "postgres", Version: "16"},
			next: pg16},
		{name: "type round trip, other version", latest: &Addon{Type: "nats", Version: "2"},
			volume: &AddonData{Type: "postgres", Version: "16"}, next: pg17,
			want: []expect{e("addons.db.version", "a major version change")}},
		// The volume holds another type's data: it is deleted, whatever the
		// latest deployment says, and its version is no constraint.
		{name: "volume of another type", latest: &pg17, volume: &AddonData{Type: "nats", Version: "2"}, next: pg16,
			want: []expect{w("addons.db.type", "holds nats data, not postgres: its data is deleted")}},
		// The version that ran ready on the volume decides, not the latest
		// deployment's (a failed upgrade never wrote the data).
		{name: "upgrade that never ran", latest: &Addon{Type: "postgres", Version: "17"},
			volume: &AddonData{Type: "postgres", Version: "16"}, next: pg16},
		// No version recorded yet: the latest deployment's.
		{name: "no data version", latest: &pg17, volume: &AddonData{Type: "postgres"}, next: pg16,
			want: []expect{e("addons.db.version", "a major version change")}},
		// No volume: the latest deployment's addon, as before.
		{name: "no volume", latest: &pg17, next: pg16, want: []expect{e("addons.db.version", "a major version change")}},
		{name: "no volume, type change", latest: &Addon{Type: "nats", Version: "2"}, next: pg16,
			want: []expect{w("addons.db.type", "changes its type from nats to postgres: its data is deleted")}},
		// Without an environment there is no volume.
		{name: "env-less", volume: &AddonData{Type: "postgres", Version: "16"}, next: pg17, mode: ModeSpec},
	} {
		t.Run(c.name, func(t *testing.T) {
			vc := defaultVC(ModePlan)
			if c.mode != "" {
				vc.mode = c.mode
			}
			p := prev
			p.Addons = maps.Clone(prev.Addons)
			if c.latest != nil {
				p.Addons["db"] = *c.latest
			}
			if c.volume != nil {
				vc.volumes = map[string]AddonData{"db": *c.volume}
			}
			checkFindings(t, validateMutated(t, vc, &p, withDB(c.next)), c.want)
		})
	}
}
