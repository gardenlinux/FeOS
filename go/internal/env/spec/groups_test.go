// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"encoding/json"
	"net/netip"
	"strconv"
	"strings"
	"testing"
)

// The example of ADR 0034: web open to a block on 8080, api to web, db to
// api and migrate, migrate with no rules.
const groupsSpec = `{
  "securityGroups": {
    "web": {"in": [{"ports": [8080], "from": {"cidr": "203.0.113.0/24"}}]},
    "api": {"in": [{"ports": [9000], "from": {"group": "web"}}], "out": [{"to": "internet"}]},
    "db": {"in": [{"ports": [5432], "from": {"group": "api"}}, {"ports": [5432], "from": {"group": "migrate"}}]},
    "migrate": {},
    "closed": {"out": []}
  },
  "services": {
    "web": {"image": "ghcr.io/acme/web:1", "ports": [{"name": "http", "port": 8080}], "securityGroups": ["web"],
            "env": {"API": "http://api:9000/v1"}},
    "api": {"image": "ghcr.io/acme/api:1", "ports": [{"name": "http", "port": 9000}], "securityGroups": ["api"],
            "env": {"DB": {"addon": "db", "key": "url"}}}
  },
  "jobs": {"migrate": {"image": "ghcr.io/acme/api:1", "securityGroups": ["migrate"],
                       "env": {"DB": {"addon": "db", "key": "url"}}}},
  "addons": {"db": {"type": "postgres", "version": "17", "securityGroups": ["db"]}}
}`

// groupsVC is the fake context with the cluster's deny set and the
// environment's group users.
type groupsVC struct {
	fakeVC
	deny  []netip.Prefix
	users map[string][]string
}

func (g groupsVC) DenyPrefixes() []netip.Prefix    { return g.deny }
func (g groupsVC) GroupUsers() map[string][]string { return g.users }

func newGroupsVC() groupsVC {
	return groupsVC{fakeVC: defaultVC(ModePlan), deny: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("fd00::/8")}}
}

func validateGroups(t *testing.T, vc ValidateContext, change func(s *Spec)) []Finding {
	t.Helper()
	s := mustParse(t, groupsSpec)
	if change != nil {
		change(&s)
	}
	return Validate(vc, s, nil)
}

// The peers' and ports' forms read and write as the contract has them.
func TestGroupsParse(t *testing.T) {
	s := mustParse(t, groupsSpec)
	g := s.SecurityGroups
	if p := g["web"].In[0].From; p == nil || p.CIDR != "203.0.113.0/24" || g["web"].In[0].Ports[0] != (PortRange{8080, 8080}) {
		t.Fatalf("web %+v", g["web"])
	}
	if p := g["api"].Out[0].To; p == nil || !p.Internet {
		t.Fatalf("api %+v", g["api"])
	}
	// out left out is nil; out [] is none, not nil.
	if g["migrate"].Out != nil || g["closed"].Out == nil || len(g["closed"].Out) != 0 {
		t.Fatalf("migrate %+v closed %+v", g["migrate"], g["closed"])
	}

	s = mustParse(t, `{"securityGroups": {"x": {"in": [
		{"protocol": "udp", "ports": [53, "9000-9010"], "from": {"cidr": "2001:db8::/32", "except": ["2001:db8:1::/48"]}}]}}}`)
	r := s.SecurityGroups["x"].In[0]
	if r.Ports[1] != (PortRange{9000, 9010}) || r.From.Except[0] != "2001:db8:1::/48" {
		t.Fatalf("rule %+v", r)
	}
	b, err := json.Marshal(r)
	if err != nil || string(b) != `{"protocol":"udp","ports":[53,"9000-9010"],"from":{"cidr":"2001:db8::/32","except":["2001:db8:1::/48"]}}` {
		t.Fatalf("%s %v", b, err)
	}
	// The plain decoder reads what the spec stores.
	var back Rule
	if err := json.Unmarshal(b, &back); err != nil || back.Ports[1].Last != 9010 || back.From.CIDR != "2001:db8::/32" {
		t.Fatalf("%+v %v", back, err)
	}
	var empty SecurityGroup
	if err := json.Unmarshal([]byte(`{"out": []}`), &empty); err != nil || empty.Out == nil {
		t.Fatalf("%+v %v", empty, err)
	}
	if b, _ := json.Marshal(empty); string(b) != `{"out":[]}` {
		t.Fatalf("%s", b)
	}
	if b, _ := json.Marshal(SecurityGroup{}); string(b) != `{}` {
		t.Fatalf("%s", b)
	}

	for _, c := range []struct{ raw, path, problem string }{
		{`{"from": "everyone"}`, "securityGroups.x.in[0].from", `peer "everyone"`},
		{`{"from": {"group": "a", "cidr": "10.0.0.0/8"}}`, "securityGroups.x.in[0].from", "peer with keys cidr, group"},
		{`{"from": {"cidr": "10.0.0.0/8", "except": "10.1.0.0/16"}}`, "securityGroups.x.in[0].from", "except: want a list"},
		{`{"from": {"cidr": "10.0.0.0/8", "mask": 8}}`, "securityGroups.x.in[0].from", `not "mask"`},
		{`{"from": {"group": ""}}`, "securityGroups.x.in[0].from", "group: want a non-empty string"},
		{`{"from": ["internet"]}`, "securityGroups.x.in[0].from", "want \"internet\""},
		{`{"ports": ["80"], "from": "internet"}`, "securityGroups.x.in[0].ports[0]", `port range "80"`},
		{`{"ports": ["a-b"], "from": "internet"}`, "securityGroups.x.in[0].ports[0]", `port range "a-b"`},
		{`{"ports": [1.5], "from": "internet"}`, "securityGroups.x.in[0].ports[0]", "port 1.5"},
		{`{"port": 80, "from": "internet"}`, "securityGroups.x.in[0].port", `unknown field "port"`},
	} {
		_, fs := Parse([]byte(`{"securityGroups": {"x": {"in": [` + c.raw + `]}}}`))
		if len(fs) != 1 || fs[0].Path != c.path || !strings.Contains(fs[0].Problem, c.problem) {
			t.Errorf("%s: %s", c.raw, dump(fs))
		}
	}
	if _, fs := Parse([]byte(`{"services": {"a": {"image": "x", "securityGroups": "web"}}}`)); len(fs) != 1 ||
		fs[0].Path != "services.a.securityGroups" {
		t.Errorf("%s", dump(fs))
	}
}

// Normalise writes the defaults: default, a missing out as the internet,
// tcp, sorted ports, canonical blocks, objects in default.
func TestGroupsNormalise(t *testing.T) {
	s := mustParse(t, `{
	  "securityGroups": {"a": {"in": [{"ports": [9000, "80-81", 9000], "from": {"cidr": "2001:DB8::/32",
	    "except": ["2001:db8:2::/48", "2001:DB8:1::/48"]}}]}, "b": {"out": []}},
	  "services": {"s": {"image": "x", "securityGroups": ["b", "a", "b"]}, "t": {"image": "x"},
	    "att": {"runtime": "attached", "sandbox": "`+webSandbox+`", "command": ["x"]}},
	  "jobs": {"j": {"image": "x"}},
	  "addons": {"d": {"type": "postgres"}}
	}`)
	n, _ := Normalise(s, Pins{})
	got := map[string]any{}
	b, _ := json.Marshal(n.SecurityGroups)
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	want := `{"a":{"in":[{"from":{"cidr":"2001:db8::/32","except":["2001:db8:1::/48","2001:db8:2::/48"]},` +
		`"ports":["80-81",9000],"protocol":"tcp"}],"out":[{"protocol":"any","to":"internet"}]},"b":{"out":[]},` +
		`"default":{"out":[{"protocol":"any","to":"internet"}]}}`
	if c := canonical(t, b); c != want {
		t.Fatalf("groups\n%s\nwant\n%s", c, want)
	}
	if g := n.Services["s"].SecurityGroups; strings.Join(g, ",") != "a,b" {
		t.Fatalf("s %v", g)
	}
	if g := n.Services["t"].SecurityGroups; strings.Join(g, ",") != "default" {
		t.Fatalf("t %v", g)
	}
	if n.Services["att"].SecurityGroups != nil || strings.Join(n.Jobs["j"].SecurityGroups, ",") != "default" ||
		strings.Join(n.Addons["d"].SecurityGroups, ",") != "default" {
		t.Fatalf("%+v %+v %+v", n.Services["att"], n.Jobs["j"], n.Addons["d"])
	}
	// Normalised, it normalises to itself.
	again, _ := Normalise(n, Pins{})
	if !EqualNormalised(n, again) {
		t.Fatalf("not idempotent:\n%s\n%s", marshalSpec(t, n), marshalSpec(t, again))
	}
	// And the stored form reads back equal.
	var stored Spec
	if err := json.Unmarshal([]byte(marshalSpec(t, n)), &stored); err != nil || !EqualNormalised(stored, n) {
		t.Fatalf("stored %v\n%s", err, marshalSpec(t, stored))
	}
}

// The example validates without a finding but the in rule's block's note:
// every use has its rule.
func TestGroupsValidateExample(t *testing.T) {
	checkFindings(t, validateGroups(t, newGroupsVC(), nil), []expect{
		{SeverityInfo, "securityGroups.web.in[0].from.cidr", "the ingress does not check blocks yet"}})
}

func TestGroupsValidateRules(t *testing.T) {
	info := func(path, problem string) expect { return expect{SeverityInfo, path, problem} }
	for _, c := range []struct {
		name   string
		change func(s *Spec)
		want   []expect
	}{
		{"names and limits", func(s *Spec) {
			s.SecurityGroups["Web_1"] = SecurityGroup{}
			s.SecurityGroups["-x"] = SecurityGroup{}
			s.SecurityGroups[strings.Repeat("a", 33)] = SecurityGroup{}
			many := SecurityGroup{}
			for range MaxGroupRules + 1 {
				many.In = append(many.In, Rule{From: &Peer{Group: "web"}})
			}
			s.SecurityGroups["many"] = many
		}, []expect{e("securityGroups.-x", "not a security group's name"),
			e("securityGroups.Web_1", "not a security group's name"),
			e("securityGroups."+strings.Repeat("a", 33), "not a security group's name"),
			e("securityGroups.many", "61 rules; at most 60")}},
		{"too many groups", func(s *Spec) {
			for i := range MaxSecurityGroups - 5 {
				s.SecurityGroups["g"+strconv.Itoa(i)] = SecurityGroup{}
			}
		}, []expect{e("securityGroups", "51 securityGroups; at most 50")}},
		{"protocols and ports", func(s *Spec) {
			s.SecurityGroups["x"] = SecurityGroup{In: []Rule{
				{Protocol: "sctp", From: &Peer{Group: "web"}},
				{Protocol: ProtocolICMP, Ports: []PortRange{{80, 80}}, From: &Peer{Group: "web"}},
				{Protocol: ProtocolAny, Ports: []PortRange{{80, 80}}, From: &Peer{Group: "web"}},
				{Ports: []PortRange{{0, 0}, {70000, 70000}, {90, 80}}, From: &Peer{Group: "web"}},
				{Protocol: ProtocolUDP, Ports: []PortRange{{53, 53}}, From: &Peer{Group: "web"}},
			}}
		}, []expect{e("securityGroups.x.in[0].protocol", `protocol "sctp" is unknown`),
			e("securityGroups.x.in[1].ports", "protocol icmp has no ports"),
			e("securityGroups.x.in[2].ports", "protocol any has no ports"),
			e("securityGroups.x.in[3].ports[0]", "port 0 is out of range"),
			e("securityGroups.x.in[3].ports[1]", "port 70000 is out of range"),
			e("securityGroups.x.in[3].ports[2]", "range 90-80 ends before it begins")}},
		{"peers", func(s *Spec) {
			s.SecurityGroups["x"] = SecurityGroup{
				In: []Rule{{To: &Peer{Internet: true}}, {}, {From: &Peer{Group: "nope"}}, {From: &Peer{Internet: true}},
					{From: &Peer{Group: "default"}}},
				Out: []Rule{{From: &Peer{Internet: true}}, {To: &Peer{Group: "web"}}}}
		}, []expect{e("securityGroups.x.in[0].to", "an in rule's peer is from, not to"),
			e("securityGroups.x.in[1].from", "a rule needs a peer"),
			e("securityGroups.x.in[2].from.group", `no security group "nope"`),
			e("securityGroups.x.out[0].from", "an out rule's peer is to, not from"),
			info("securityGroups.x.out[1].to", "out to a group of the environment is implied")}},
		{"blocks", func(s *Spec) {
			s.SecurityGroups["x"] = SecurityGroup{Out: []Rule{
				{To: &Peer{CIDR: "203.0.113.7/24"}},
				{To: &Peer{CIDR: "203.0.113.7"}},
				{To: &Peer{CIDR: "nonsense"}},
				{To: &Peer{CIDR: "::ffff:203.0.113.0/120"}},
				{To: &Peer{CIDR: "198.51.100.0/24", Except: []string{"2001:db8::/32", "203.0.113.0/25", "198.51.100.0/23",
					"198.51.100.128/25"}}},
				{To: &Peer{CIDR: "10.1.0.0/16"}},
				{To: &Peer{CIDR: "fd12::/16"}},
				{To: &Peer{CIDR: "0.0.0.0/0", Except: []string{"10.0.0.0/8"}}},
				{To: &Peer{CIDR: "2000::/3"}},
			}}
		}, []expect{e("securityGroups.x.out[0].to.cidr", "203.0.113.7/24 has bits set past its length"),
			e("securityGroups.x.out[1].to.cidr", `"203.0.113.7" has no prefix length`),
			e("securityGroups.x.out[2].to.cidr", `"nonsense" is not a prefix`),
			e("securityGroups.x.out[3].to.cidr", "IPv4 written as IPv6"),
			e("securityGroups.x.out[4].to.except[0]", "of another family"),
			e("securityGroups.x.out[4].to.except[1]", "not inside"),
			e("securityGroups.x.out[4].to.except[2]", "not inside"),
			e("securityGroups.x.out[5].to.cidr", "10.1.0.0/16 is inside 10.0.0.0/8, which no pod of the cluster reaches"),
			e("securityGroups.x.out[6].to.cidr", "inside fd00::/8"),
			info("securityGroups.x.out[7].to.cidr", "0.0.0.0/0 stays closed inside 192.168.0.0/16: --deny-prefix comes first")}},
		{"object refs", func(s *Spec) {
			svc(s, "web", func(v *Service) { v.SecurityGroups = []string{} })
			svc(s, "api", func(v *Service) { v.SecurityGroups = []string{"api", "nope", "api"} })
			s.Jobs["many"] = Job{Image: "x", SecurityGroups: []string{"a", "b", "c", "d", "e", "f"}}
			s.Addons["db"] = Addon{Type: "postgres", Version: "17", SecurityGroups: []string{"default", "missing"}}
		}, []expect{e("addons.db.securityGroups[1]", `no security group "missing"`),
			e("jobs.many.securityGroups", "6 groups; at most 5"),
			e("jobs.many.securityGroups[0]", `no security group "a"`), e("jobs.many.securityGroups[1]", `"b"`),
			e("jobs.many.securityGroups[2]", `"c"`), e("jobs.many.securityGroups[3]", `"d"`),
			e("jobs.many.securityGroups[4]", `"e"`), e("jobs.many.securityGroups[5]", `"f"`),
			e("services.api.securityGroups[1]", `no security group "nope"`),
			e("services.api.securityGroups[2]", "group api is named twice"),
			e("services.web.securityGroups", "securityGroups is empty")}},
	} {
		t.Run(c.name, func(t *testing.T) {
			checkFindings(t, withoutExampleNote(validateGroups(t, newGroupsVC(), c.change)), c.want)
		})
	}
}

// withoutExampleNote drops the example's own note on web's block.
func withoutExampleNote(fs []Finding) []Finding {
	var out []Finding
	for _, f := range fs {
		if f.Path != "securityGroups.web.in[0].from.cidr" {
			out = append(out, f)
		}
	}
	return out
}

// A group a sandbox or a running job of the environment names cannot go
// (ADR 0035); in ModeSpec there are none.
func TestGroupsValidateUsers(t *testing.T) {
	vc := newGroupsVC()
	vc.users = map[string][]string{"gone": {"sandbox k3x9m2p7q4r", "job a1b2c3d4e5f"}, "api": {"sandbox x"},
		"default": {"sandbox y"}}
	checkFindings(t, withoutExampleNote(validateGroups(t, vc, nil)), []expect{e("securityGroups.gone",
		"security group gone is named by job a1b2c3d4e5f, sandbox k3x9m2p7q4r")})
	vc.mode = ModeSpec
	checkFindings(t, withoutExampleNote(validateGroups(t, vc, nil)), nil)
}

// An object that uses another while no in rule lets it is warned of, with
// the rule to write.
func TestGroupsValidateReach(t *testing.T) {
	fs := validateGroups(t, newGroupsVC(), func(s *Spec) {
		// db no longer admits migrate; web's URL names api on 9100; api
		// names web as a service.
		s.SecurityGroups["db"] = SecurityGroup{In: s.SecurityGroups["db"].In[:1]}
		svc(s, "web", func(v *Service) { v.Env["API"] = String("http://api:9100/v1, https://example.com") })
		svc(s, "api", func(v *Service) {
			v.Env["WEB"] = EnvValue{Kind: EnvService, Service: "web", Port: PortRef{Name: "http"}, Key: "url"}
			v.Env["WEB_AGAIN"] = EnvValue{Kind: EnvService, Service: "web", Port: PortRef{Number: 8080}, Key: "url"}
			v.Env["SELF"] = String("http://api:9000")
		})
	})
	checkFindings(t, withoutExampleNote(fs), []expect{
		w("jobs.migrate.env.DB", "job migrate reaches addon db on port 5432, but no in rule of db's groups (db) admits "+
			"migrate's (migrate)"),
		w("services.api.env.WEB", "service api reaches service web on port 8080"),
		w("services.web.env.API", "service web reaches service api on port 9100"),
	})
	if r := fs[0].Remedy; r != `add to securityGroups.db.in: {"protocol":"tcp","ports":[5432],"from":{"group":"migrate"}}` {
		t.Fatalf("remedy %s", r)
	}

	// An attached service is reached in its sandbox's groups, and reaches
	// from them; two in one sandbox reach each other over its loopback.
	vc := newGroupsVC()
	vc.sandboxes[webSandbox] = SandboxInfo{ID: webSandbox, SecurityGroups: []string{"api"}}
	fs = validateGroups(t, vc, func(s *Spec) {
		s.Services["dev"] = Service{Runtime: RuntimeAttached, Sandbox: webSandbox, Command: []string{"vite"},
			Ports: []Port{{Name: "http", Port: 5173}}, Env: map[string]EnvValue{"DB": {Kind: EnvAddon, Addon: "db", Key: "url"},
				"SIDE": String("http://side:3000")}}
		s.Services["side"] = Service{Runtime: RuntimeAttached, Sandbox: webSandbox, Command: []string{"side"},
			Ports: []Port{{Name: "http", Port: 3000}}}
		svc(s, "web", func(v *Service) { v.Env["DEV"] = String("http://dev:5173") })
	})
	checkFindings(t, withoutExampleNote(fs), []expect{w("services.web.env.DEV",
		"service web reaches service dev on port 5173, but no in rule of dev's groups (api) admits web's (web)")})
}

// A group's rules change in place; an object's groups are its template.
func TestGroupsDiff(t *testing.T) {
	prev, _ := Normalise(mustParse(t, groupsSpec), Pins{})
	next, _ := Normalise(mustParse(t, groupsSpec), Pins{})
	next.SecurityGroups["web"] = SecurityGroup{In: nil, Out: internetOut()}
	web := next.Services["web"]
	web.SecurityGroups = []string{"default", "web"}
	next.Services["web"] = web
	j := next.Jobs["migrate"]
	j.SecurityGroups = []string{"api"}
	next.Jobs["migrate"] = j
	a := next.Addons["db"]
	a.SecurityGroups = []string{"api", "db"}
	next.Addons["db"] = a
	delete(next.SecurityGroups, "closed")
	got := diffJSON(t, Diff(&prev, &next))
	want := `[{"kind":"service","name":"web","change":"update","fields":[{"path":"securityGroups","from":["web"],` +
		`"to":["default","web"]}],"effect":"replaces 1 pod"},` +
		`{"kind":"job","name":"migrate","change":"update","fields":[{"path":"securityGroups","from":["migrate"],"to":["api"]}],` +
		`"effect":"restarts"},` +
		`{"kind":"addon","name":"db","change":"update","fields":[{"path":"securityGroups","from":["db"],"to":["api","db"]}],` +
		`"effect":"recreates (downtime)"},` +
		`{"kind":"securityGroup","name":"closed","change":"remove","fields":[],"effect":"in place"},` +
		`{"kind":"securityGroup","name":"web","change":"update","fields":[{"path":"in","from":[{"from":{"cidr":"203.0.113.0/24"},` +
		`"ports":[8080],"protocol":"tcp"}],"to":null}],"effect":"in place"}]`
	if got != want {
		t.Fatalf("diff\n%s\nwant\n%s", got, want)
	}
}
