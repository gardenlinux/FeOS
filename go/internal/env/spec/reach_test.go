// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"encoding/json"
	"net/netip"
	"slices"
	"strings"
	"testing"
)

// kino is a cinema's environment as FeCode's demo has it: eight services,
// two addons and a job calling each other over 25 edges, every one a
// reference (an env reference by port name, a URL host, uses, a route).
const kino = `{
  "services": {
    "web":     {"image": "kino/web:1", "ports": [{"name": "http", "port": 8080}],
                "env": {"API_URL": {"service": "api", "port": "http", "key": "url"},
                        "AUTH": "http://auth:8080/login"}},
    "api":     {"image": "kino/api:1", "ports": [{"name": "http", "port": 8080}],
                "env": {"CATALOG_HOST": {"service": "catalog", "port": "http", "key": "host"},
                        "CATALOG_URL": {"service": "catalog", "port": "http", "key": "url"},
                        "BOOKING": {"service": "booking", "port": "http", "key": "url"},
                        "AUTH": {"service": "auth", "port": 8080, "key": "url"},
                        "SEARCH": {"service": "search", "port": "http", "key": "url"},
                        "PAYMENT": {"service": "payment", "port": "http", "key": "url"},
                        "CACHE": {"addon": "cache", "key": "url"}}},
    "catalog": {"image": "kino/catalog:1", "ports": [{"name": "http", "port": 8080}, {"name": "grpc", "port": 9090}],
                "env": {"DB": {"addon": "db", "key": "url"}, "CACHE": "redis://cache:6379/0"}},
    "booking": {"image": "kino/booking:1", "ports": [{"name": "http", "port": 8080}],
                "env": {"DB": {"addon": "db", "key": "url"},
                        "PAYMENT": {"service": "payment", "port": "http", "key": "url"},
                        "NOTIFY": "http://notify:8080/send",
                        "CATALOG": {"service": "catalog", "port": "grpc", "key": "port"}}},
    "payment": {"image": "kino/payment:1", "ports": [{"name": "http", "port": 8080}],
                "env": {"DB": {"addon": "db", "key": "url"}},
                "uses": [{"service": "notify", "port": "http"}],
                "egress": {"domains": ["api.stripe.com"]}},
    "notify":  {"image": "kino/notify:1", "ports": [{"name": "http", "port": 8080}],
                "env": {"CACHE": {"addon": "cache", "key": "url"}},
                "uses": [{"service": "auth", "port": 8080}],
                "egress": "none"},
    "search":  {"image": "kino/search:1", "ports": [{"name": "http", "port": 8080}],
                "env": {"CATALOG": "http://catalog:8080", "CACHE": {"addon": "cache", "key": "url"}}},
    "auth":    {"image": "kino/auth:1", "ports": [{"name": "http", "port": 8080}],
                "env": {"DB": {"addon": "db", "key": "url"}, "CACHE": {"addon": "cache", "key": "url"}}}
  },
  "jobs": {"migrate": {"image": "kino/migrate:1", "env": {"DB": {"addon": "db", "key": "url"}}}},
  "addons": {"db": {"type": "postgres"}, "cache": {"type": "valkey"}},
  "routes": {
    "web": {"path": "/", "service": "web", "port": "http"},
    "api": {"path": "/api", "service": "api", "port": "http", "from": ["203.0.113.0/24", "2001:db8::/32"]}
  }
}`

func fakePorts(_ string, a Addon) int {
	switch a.Type {
	case "postgres":
		return 5432
	case "valkey":
		return 6379
	}
	return 0
}

func edgeKeys(es []Edge) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.From + " → " + e.To + ":" + e.Port
	}
	return out
}

func TestDeriveKino(t *testing.T) {
	s := mustParse(t, kino)
	r, fs := Derive(s, fakePorts)
	if len(fs) != 0 {
		t.Fatalf("findings %s", dump(fs))
	}
	want := []string{
		"ingress → service/api:http",
		"ingress → service/web:http",
		"job/migrate → addon/db:postgres",
		"service/api → addon/cache:valkey",
		"service/api → service/auth:http",
		"service/api → service/booking:http",
		"service/api → service/catalog:http",
		"service/api → service/payment:http",
		"service/api → service/search:http",
		"service/auth → addon/cache:valkey",
		"service/auth → addon/db:postgres",
		"service/booking → addon/db:postgres",
		"service/booking → service/catalog:grpc",
		"service/booking → service/notify:http",
		"service/booking → service/payment:http",
		"service/catalog → addon/cache:valkey",
		"service/catalog → addon/db:postgres",
		"service/notify → addon/cache:valkey",
		"service/notify → service/auth:http",
		"service/payment → addon/db:postgres",
		"service/payment → service/notify:http",
		"service/search → addon/cache:valkey",
		"service/search → service/catalog:http",
		"service/web → service/api:http",
		"service/web → service/auth:http",
	}
	if got := edgeKeys(r.Edges); !slices.Equal(got, want) {
		t.Fatalf("edges (%d)\n%s\nwant (%d)\n%s", len(got), strings.Join(got, "\n"), len(want), strings.Join(want, "\n"))
	}
	byKey := map[string]Edge{}
	for _, e := range r.Edges {
		byKey[e.From+" → "+e.To+":"+e.Port] = e
	}
	for key, via := range map[string]string{
		// Two variables name catalog's http: the first by name is kept.
		"service/api → service/catalog:http":     "env CATALOG_HOST",
		"service/booking → service/catalog:grpc": "env CATALOG",
		"service/booking → service/notify:http":  "env NOTIFY",
		"service/catalog → addon/cache:valkey":   "env CACHE",
		"service/payment → service/notify:http":  "uses",
		"service/notify → service/auth:http":     "uses",
		"ingress → service/api:http":             "route api",
		"service/web → service/auth:http":        "env AUTH",
	} {
		if byKey[key].Via != via {
			t.Errorf("%s via %q, want %q", key, byKey[key].Via, via)
		}
	}
	if e := byKey["service/booking → service/catalog:grpc"]; e.Number != 9090 {
		t.Errorf("by port name %+v", e)
	}
	if e := byKey["job/migrate → addon/db:postgres"]; e.Number != 5432 {
		t.Errorf("an addon's port %+v", e)
	}
	// Egress: each image service and job, resolved.
	modes := map[string]ObjectEgress{}
	for _, e := range r.Egress {
		modes[e.Object] = e
	}
	if len(r.Egress) != 9 || modes["service/web"].Mode != EgressInternet || modes["service/notify"].Mode != EgressNone ||
		modes["service/payment"].Mode != EgressRestricted ||
		!slices.Equal(modes["service/payment"].Domains, []string{"api.stripe.com"}) ||
		modes["service/payment"].Blocks == nil || modes["job/migrate"].Mode != EgressInternet ||
		modes["service/web"].Domains == nil {
		t.Errorf("egress %+v", r.Egress)
	}
	if len(r.Routes) != 2 || r.Routes[0].Route != "api" || !slices.Equal(r.Routes[0].From, []string{"203.0.113.0/24",
		"2001:db8::/32"}) || r.Routes[1].From == nil || len(r.Routes[1].From) != 0 || r.Routes[1].Port != "http" {
		t.Errorf("routes %+v", r.Routes)
	}
	// The answer's JSON as the contract has it.
	b, _ := json.Marshal(Reachability{Edges: r.Edges[:1], Egress: []ObjectEgress{modes["service/payment"]},
		Routes: r.Routes[:1]})
	const wantJSON = `{"edges":[{"from":"ingress","to":"service/api","port":"http","number":8080,"via":"route api"}],` +
		`"egress":[{"object":"service/payment","mode":"restricted","domains":["api.stripe.com"],"blocks":[]}],` +
		`"routes":[{"route":"api","service":"api","port":"http","from":["203.0.113.0/24","2001:db8::/32"]}]}`
	if string(b) != wantJSON {
		t.Errorf("JSON\n%s\nwant\n%s", b, wantJSON)
	}
}

// An attached service is "service/<name>" in the answer on either end; a
// route to it is an edge from the ingress; a reference of an object to
// itself is none; an empty spec has empty lists.
func TestDeriveAttachedAndEmpty(t *testing.T) {
	s := mustParse(t, `{"services": {
		"vite": {"runtime": "attached", "sandbox": "k3m9q7a2b4c", "command": ["vite"],
			"ports": [{"name": "http", "port": 5173}],
			"env": {"API": {"service": "api", "port": "http", "key": "url"}, "SELF": "http://vite:5173"}},
		"api": {"image": "api:1", "ports": [{"name": "http", "port": 8080}],
			"env": {"VITE": "http://vite:5173/", "ME": {"service": "api", "port": "http", "key": "url"}}}},
		"routes": {"dev": {"path": "/", "service": "vite", "port": "http"}}}`)
	r, fs := Derive(s, fakePorts)
	if len(fs) != 0 {
		t.Fatalf("findings %s", dump(fs))
	}
	if got := edgeKeys(r.Edges); !slices.Equal(got, []string{"ingress → service/vite:http",
		"service/api → service/vite:http", "service/vite → service/api:http"}) {
		t.Errorf("edges %v", got)
	}
	// An attached service has no egress of its own: its sandbox's.
	if len(r.Egress) != 1 || r.Egress[0].Object != "service/api" {
		t.Errorf("egress %+v", r.Egress)
	}
	empty, fs := Derive(Spec{}, nil)
	if b, _ := json.Marshal(empty); string(b) != `{"edges":[],"egress":[],"routes":[]}` || len(fs) != 0 {
		t.Errorf("empty %s %v", b, fs)
	}
}

// URLs that would fail are warnings, uses that name nothing errors; a
// host reference without a port opens each declared port.
func TestDeriveFindings(t *testing.T) {
	s := mustParse(t, `{"services": {
		"a": {"image": "a:1", "ports": [{"name": "http", "port": 8080}],
			"env": {"NOPORT": "grpc://b", "WRONG": "http://b:81/x", "DEFAULT": "http://c/", "ADDON": "postgres://u@db:6000/x",
				"HOST": {"service": "b", "key": "host"}, "OTHER": "http://example.com:81"},
			"uses": [{"service": "nope", "port": "http"}, {"service": "b", "port": "none"}, {"service": "b"},
				{"addon": "nope"}, {"addon": "db", "port": 1}, {}, {"service": "b", "addon": "db"}, {"addon": "db"}]},
		"b": {"image": "b:1", "ports": [{"name": "http", "port": 8080}, {"name": "admin", "port": 9000}]},
		"c": {"image": "c:1", "ports": [{"name": "web", "port": 80}]}},
		"addons": {"db": {"type": "postgres"}}}`)
	r, fs := Derive(s, fakePorts)
	checkFindings(t, fs, []expect{
		w("services.a.env.ADDON", "names addon db on port 6000; it serves on 5432"),
		w("services.a.env.NOPORT", "names service b without a port"),
		w("services.a.env.WRONG", "names service b on port 81, which it does not declare"),
		e("services.a.uses[0].service", "no service nope"),
		e("services.a.uses[1].port", "service b has no port none"),
		e("services.a.uses[2].port", "an entry of uses names the service's port"),
		e("services.a.uses[3].addon", "no addon nope"),
		e("services.a.uses[4].port", "an addon has no port to name"),
		e("services.a.uses[5]", "an entry of uses names nothing"),
		e("services.a.uses[6]", "names a service and an addon"),
	})
	if got := edgeKeys(r.Edges); !slices.Equal(got, []string{"service/a → addon/db:postgres",
		"service/a → service/b:http", "service/a → service/b:admin", "service/a → service/c:web"}) {
		t.Errorf("edges %v", got)
	}
}

// The forms of egress, reaches and uses in JSON, strictly.
func TestReachForms(t *testing.T) {
	for _, tc := range []struct {
		in, mode string
		restrict bool
		domains  []string
	}{
		{`"internet"`, EgressInternet, false, nil},
		{`"none"`, EgressNone, true, nil},
		{`{}`, EgressNone, true, nil},
		{`{"domains": ["b.example.com", "a.example.com", "a.example.com"]}`, EgressRestricted, true,
			[]string{"a.example.com", "b.example.com"}},
		{`{"blocks": [{"cidr": "203.0.113.0/24", "except": ["203.0.113.8/29"]}]}`, EgressRestricted, true, nil},
	} {
		var e Egress
		if err := json.Unmarshal([]byte(tc.in), &e); err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if e.EffectiveMode() != tc.mode || e.RestrictsNames() != tc.restrict || !slices.Equal(e.NameDomains(), tc.domains) {
			t.Errorf("%s: %s %v %v", tc.in, e.EffectiveMode(), e.RestrictsNames(), e.NameDomains())
		}
		b, _ := json.Marshal(e)
		var again Egress
		if err := json.Unmarshal(b, &again); err != nil || again.EffectiveMode() != tc.mode {
			t.Errorf("%s round trip %s %v", tc.in, b, err)
		}
	}
	var nilEgress *Egress
	if nilEgress.EffectiveMode() != EgressInternet || nilEgress.RestrictsNames() {
		t.Error("an absent egress is not the internet")
	}
	for _, bad := range []string{`"open"`, `1`, `[]`, `{"domain": []}`, `{"domains": "x"}`} {
		var e Egress
		if err := json.Unmarshal([]byte(bad), &e); err == nil {
			t.Errorf("%s read as %+v", bad, e)
		}
	}
	var r Reaches
	if err := json.Unmarshal([]byte(`"all"`), &r); err != nil || !r.All {
		t.Errorf("all %+v %v", r, err)
	}
	if err := json.Unmarshal([]byte(`[{"service": "api", "port": "http"}, {"addon": "db"}, {"service": "x", "port": 80}]`),
		&r); err != nil || r.All || len(r.Entries) != 3 || r.Entries[2].Port.Number != 80 {
		t.Errorf("entries %+v %v", r, err)
	}
	for _, bad := range []string{`"some"`, `{}`, `[{"svc": "x"}]`, `3`} {
		var r Reaches
		if err := json.Unmarshal([]byte(bad), &r); err == nil {
			t.Errorf("%s read as %+v", bad, r)
		}
	}
	var body struct {
		Reaches Reaches `json:"reaches"`
		Egress  *Egress `json:"egress"`
	}
	if err := json.Unmarshal([]byte(`{"reaches": null, "egress": null}`), &body); err != nil || body.Reaches.All ||
		body.Egress != nil {
		t.Errorf("null %+v %v", body, err)
	}
	if b, _ := json.Marshal(Reaches{}); string(b) != `[]` {
		t.Errorf("nothing %s", b)
	}
	if b, _ := json.Marshal(Reaches{All: true}); string(b) != `"all"` {
		t.Errorf("all %s", b)
	}
	// In a spec, a wrong form is a finding at its path.
	_, fs := Parse([]byte(`{"services": {"a": {"image": "a", "egress": "open", "uses": [{"svc": "b"}]}}}`))
	if len(fs) != 2 || fs[0].Path != "services.a.egress" || fs[1].Path != "services.a.uses[0].svc" {
		t.Errorf("findings %s", dump(fs))
	}
}

// What a sandbox's or a job's create says of its network.
func TestCheckPodAndReaches(t *testing.T) {
	deny := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	for _, tc := range []struct {
		groups  []byte
		reaches Reaches
		egress  *Egress
		want    string
	}{
		{nil, Reaches{}, nil, ""},
		{nil, Reaches{All: true}, &Egress{Mode: EgressNone}, ""},
		{[]byte(`["default"]`), Reaches{}, nil, "securityGroups is gone (ADR 0034, revised); use reaches"},
		{[]byte(`null`), Reaches{}, nil, "securityGroups is gone"},
		{nil, Reaches{Entries: []Use{{Service: "api"}}}, nil, "reaches[0]: name the service's port"},
		{nil, Reaches{Entries: []Use{{}}}, nil, "reaches[0] names nothing"},
		{nil, Reaches{Entries: []Use{{Addon: "db", Port: PortRef{Number: 1}}}}, nil, "reaches[0]: an addon is reached"},
		{nil, Reaches{}, &Egress{Domains: []string{"Bad"}}, `egress.domains[0]: "Bad" is not a domain`},
		{nil, Reaches{}, &Egress{Blocks: []Block{{CIDR: "10.1.0.0/16"}}}, "egress.blocks[0].cidr: 10.1.0.0/16 is inside 10.0.0.0/8"},
	} {
		if got := CheckPod(tc.groups, tc.reaches, tc.egress, deny); !strings.HasPrefix(got, tc.want) || (tc.want == "") != (got == "") {
			t.Errorf("%+v: %q, want %q", tc, got, tc.want)
		}
	}
	s := mustParse(t, `{"services": {"api": {"image": "a", "ports": [{"name": "http", "port": 8080}]}},
		"addons": {"db": {"type": "postgres"}}}`)
	if msg := UnknownReaches(Reaches{Entries: []Use{{Service: "api", Port: PortRef{Name: "http"}}, {Addon: "db"},
		{Service: "api", Port: PortRef{Number: 8080}}}}, &s); msg != "" {
		t.Errorf("known %q", msg)
	}
	msg := UnknownReaches(Reaches{Entries: []Use{{Service: "web", Port: PortRef{Name: "http"}},
		{Service: "api", Port: PortRef{Name: "grpc"}}, {Addon: "cache"}}}, &s)
	if msg != "reaches: the environment's deployed deployment has no service web, port grpc of service api, addon cache; "+
		"it has service api (http), addon db" {
		t.Errorf("unknown %q", msg)
	}
	if msg := UnknownReaches(Reaches{Entries: []Use{{Addon: "db"}}}, nil); msg !=
		"reaches: the environment's deployed deployment has no addon db; it has nothing" {
		t.Errorf("no deployment %q", msg)
	}
	if UnknownReaches(Reaches{All: true}, nil) != "" {
		t.Error("all refused")
	}
}

// The plan's findings of reachability: securityGroups anywhere, egress
// domains and blocks, a route's blocks; a block inside --deny-prefix.
func TestValidateReach(t *testing.T) {
	vc := defaultVC(ModePlan)
	checkFindings(t, validateMutated(t, vc, nil, func(s *Spec) {
		s.Services["approval-service"] = Service{Image: "ghcr.io/acme/approval:1.2", Ports: []Port{{Name: "http", Port: 8080}}}
		s.SecurityGroups = []byte(`{"default": {}}`)
		api := s.Services["expense-service"]
		api.SecurityGroups = []byte(`["default"]`)
		api.Egress = &Egress{Domains: []string{"api.stripe.com", "*.githubusercontent.com", "x.*.com", "api.stripe.com",
			"localhost", "Upper.com"}, Blocks: []Block{{CIDR: "203.0.113.0/24", Except: []string{"203.0.113.8/29",
			"198.51.100.0/24", "2001:db8::/32"}}, {CIDR: "203.0.113.1/24"}, {CIDR: "::ffff:1.2.3.0/120"}}}
		api.Uses = []Use{{Service: "web", Port: PortRef{Name: "http"}}}
		s.Services["expense-service"] = api
		j := s.Jobs["migrate"]
		j.SecurityGroups = []byte(`["default"]`)
		j.Egress = &Egress{Mode: EgressNone}
		s.Jobs["migrate"] = j
		a := s.Addons["nats"]
		a.SecurityGroups = []byte(`["default"]`)
		s.Addons["nats"] = a
		r := s.Routes["root"]
		r.From = []string{"203.0.113.0/24", "2001:db8::/32", "203.0.113.0/24", "10.0.0.1", "nope"}
		s.Routes["root"] = r
		r2 := s.Routes["api-expense"]
		r2.From = []string{}
		s.Routes["api-expense"] = r2
	}), []expect{
		e("addons.nats.securityGroups", "securityGroups is gone"),
		e("jobs.migrate.securityGroups", "securityGroups is gone"),
		e("routes.api-expense.from", "from is empty"),
		e("routes.root.from[2]", "block 203.0.113.0/24 is named twice"),
		e("routes.root.from[3]", `"10.0.0.1" has no prefix length`),
		e("routes.root.from[4]", `"nope" is not a prefix`),
		e("securityGroups", "securityGroups is gone"),
		e("services.expense-service.egress.blocks[0].except[1]", "198.51.100.0/24 is not inside 203.0.113.0/24"),
		e("services.expense-service.egress.blocks[0].except[2]", "of another family"),
		e("services.expense-service.egress.blocks[1].cidr", "has bits set past its length"),
		e("services.expense-service.egress.blocks[2].cidr", "IPv4 written as IPv6"),
		e("services.expense-service.egress.domains[2]", `"x.*.com" is not a domain`),
		e("services.expense-service.egress.domains[3]", "named twice"),
		e("services.expense-service.egress.domains[4]", `"localhost" is not a domain`),
		e("services.expense-service.egress.domains[5]", `"Upper.com" is not a domain`),
		e("services.expense-service.securityGroups", "securityGroups is gone"),
	})
	// --deny-prefix comes first.
	fs := EgressFindings("egress", &Egress{Blocks: []Block{{CIDR: "10.1.0.0/16"}, {CIDR: "0.0.0.0/0"},
		{CIDR: "0.0.0.0/0", Except: []string{"10.0.0.0/8"}}}}, []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")})
	checkFindings(t, fs, []expect{
		e("egress.blocks[0].cidr", "10.1.0.0/16 is inside 10.0.0.0/8"),
		i("egress.blocks[1].cidr", "0.0.0.0/0 stays closed inside 10.0.0.0/8"),
	})
	// Normalisation keeps them as written and drops the gone field.
	s := mustParse(t, kino)
	n, _ := Normalise(s, Pins{})
	if n.Services["notify"].Egress.Mode != EgressNone || len(n.Services["payment"].Uses) != 1 ||
		len(n.Routes["api"].From) != 2 || n.Services["web"].Egress != nil || n.Services["web"].Uses != nil {
		t.Errorf("normalised %+v", n.Services)
	}
}
