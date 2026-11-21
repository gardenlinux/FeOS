// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A JSON document is a YAML document: the fixtures and Parse's findings
// read the same in both.
func TestYAMLTwinsOfJSON(t *testing.T) {
	docs := [][]byte{[]byte(expensa), readFile(t, "testdata/expensa-stage-request.json"),
		readFile(t, "testdata/expensa-stage-deployment7.json"),
		[]byte(`{"unknown": {}}`),
		[]byte(`{"services": {"x": {"image": "a", "readiness": {"http": {"port": "http", "path": "/", "prt": 1}}}}}`),
		[]byte(`{"services": {"x": {"image": 3, "replicas": "two", "command": ["a", 1]}}}`),
		[]byte(`{"services": {"x": {"replicas": 1.5}}, "routes": {"r": {"path": "/", "service": "x", "port": true}}}`),
		[]byte(`{"services": {"x": {"replicas": 99999999999, "env": {"A": 3, "B": {"value": "a", "secret": "b"}}}}}`),
		[]byte(`{"services": {}, "services": {}}`),
		[]byte(`{"addons": {"a": {"type": "t", "options": {"k": 1, "k": 2, "l": [true, null, 2.5]}}}}`),
		[]byte(`{"services": {"x": {"image": null}}, "jobs": {"j": {"on": 5}}, "nope": 1}`),
		[]byte(`[1]`),
	}
	for _, raw := range docs {
		js, jfs := Parse(raw)
		ys, yfs := ParseYAML(raw)
		if !reflect.DeepEqual(js, ys) || !reflect.DeepEqual(jfs, yfs) {
			t.Fatalf("%s:\nJSON %+v %+v\nYAML %+v %+v", raw, js, jfs, ys, yfs)
		}
	}
}

// The same spec written as YAML, with comments, anchors and its own
// order, is the JSON one.
func TestYAMLBlockTwin(t *testing.T) {
	const jsonDoc = `{
  "services": {
    "api": {"image": "ghcr.io/acme/api:1.4", "replicas": 2, "idle": "15m",
      "resources": {"vcpus": 1, "memoryMib": 512},
      "env": {"MODE": "on", "DEBUG": "yes", "COUNT": "1_000", "DAY": "2026-01-01", "TOKEN": {"secret": "token"},
              "URL": {"secret": "s4", "key": "url"}},
      "ports": [{"name": "http", "port": 8080}],
      "readiness": {"http": {"port": "http", "path": "/readyz"}}},
    "worker": {"image": "ghcr.io/acme/api:1.4", "resources": {"vcpus": 1, "memoryMib": 512}}
  },
  "jobs": {"migrate": {"image": "ghcr.io/acme/api:1.4", "on": "release", "retries": 1}},
  "addons": {"pg": {"type": "postgres", "version": "17", "plan": "small",
    "options": {"databases": ["app"], "public": false, "limit": 31, "ratio": 0.5}}},
  "routes": {"root": {"path": "/", "service": "api", "port": "http", "strip": false}},
  "secrets": {"token": {"version": 3}, "s4": {"type": "http", "mode": "name", "forwardedHost": true}}
}`
	const yamlDoc = `# Acme's stage
routes:
  root: {path: /, service: api, port: http, strip: false}
services:
  api:
    image: ghcr.io/acme/api:1.4   # the release
    replicas: 2
    idle: 15m
    resources: &size
      vcpus: 1
      memoryMib: 512
    env:
      MODE: on
      DEBUG: yes
      COUNT: 1_000
      DAY: 2026-01-01
      TOKEN: {secret: token}
      URL: {secret: s4, key: url}
    ports:
      - name: http
        port: 8080
    readiness:
      http: {port: http, path: /readyz}
  worker:
    image: ghcr.io/acme/api:1.4
    resources: *size
jobs:
  migrate:
    image: ghcr.io/acme/api:1.4
    on: release
    retries: 0x1
addons:
  pg:
    type: postgres
    version: "17"
    plan: small
    options:
      databases: [app]
      public: False
      limit: 0x1F
      ratio: .5
secrets:
  token: {version: 3}
  s4:
    type: http
    mode: name
    forwardedHost: TRUE
`
	js, jfs := Parse([]byte(jsonDoc))
	if len(jfs) > 0 {
		t.Fatalf("JSON %+v", jfs)
	}
	ys, yfs := ParseYAML([]byte(yamlDoc))
	if len(yfs) > 0 {
		t.Fatalf("YAML %+v", yfs)
	}
	if !reflect.DeepEqual(js, ys) {
		t.Fatalf("JSON\n%+v\nYAML\n%+v", js, ys)
	}
	raw, fs := YAMLToJSON([]byte(yamlDoc))
	if len(fs) > 0 {
		t.Fatalf("YAMLToJSON %+v", fs)
	}
	if strings.Contains(string(raw), "size") || strings.Contains(string(raw), "release\"#") {
		t.Fatalf("an anchor or a comment is left: %s", raw)
	}
	var a, b any
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(jsonDoc), &b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("YAMLToJSON\n%s", raw)
	}
}

// The plain scalars are YAML 1.2 core's.
func TestYAMLScalars(t *testing.T) {
	for in, want := range map[string]string{
		`~`: `null`, `null`: `null`, `Null`: `null`, `NULL`: `null`, ``: `null`,
		`true`: `true`, `True`: `true`, `TRUE`: `true`, `false`: `false`, `False`: `false`, `FALSE`: `false`,
		`on`: `"on"`, `off`: `"off"`, `yes`: `"yes"`, `no`: `"no"`, `y`: `"y"`, `n`: `"n"`, `tRUE`: `"tRUE"`,
		`7`: `7`, `-7`: `-7`, `+5`: `5`, `007`: `7`, `0777`: `777`, `0o17`: `15`, `0x1F`: `31`, `0xff`: `255`,
		`1_000`: `"1_000"`, `0b101`: `"0b101"`, `0O17`: `"0O17"`, `0X1F`: `"0X1F"`,
		`1.5`: `1.5`, `.5`: `0.5`, `-.5`: `-0.5`, `1.`: `1.0`, `+1.25e3`: `1.25e3`, `1e3`: `1e3`, `007.50`: `7.50`,
		`1.20`: `1.20`, `2026-01-01`: `"2026-01-01"`, `12:30`: `"12:30"`, `1.2.3`: `"1.2.3"`,
		`'7'`: `"7"`, `"true"`: `"true"`, `!!str 7`: `"7"`, `!!str true`: `"true"`, `"~"`: `"~"`,
		`-123456789012345678901234567890`: `-123456789012345678901234567890`,
	} {
		raw, fs := YAMLToJSON([]byte("v: " + in))
		if len(fs) > 0 {
			t.Errorf("%q: %+v", in, fs)
			continue
		}
		if got := string(raw); got != `{"v":`+want+`}` {
			t.Errorf("%q: %s, want %s", in, got, want)
		}
	}
	// A literal and a folded scalar are strings.
	raw, fs := YAMLToJSON([]byte("a: |\n  x\n  7\nb: >\n  y\n  z\n"))
	if len(fs) > 0 || string(raw) != `{"a":"x\n7\n","b":"y z\n"}` {
		t.Fatalf("%s %+v", raw, fs)
	}
	// A key is its text, whatever it would be as a value.
	raw, fs = YAMLToJSON([]byte("true: 1\n1: 2\n~: 3\nnull: 4\n'q': 5\n"))
	if len(fs) > 0 || string(raw) != `{"true":1,"1":2,"~":3,"null":4,"q":5}` {
		t.Fatalf("%s %+v", raw, fs)
	}
	// A version written unquoted is a number, which a string field refuses.
	_, fs = ParseYAML([]byte("addons:\n  pg: {type: postgres, version: 17}\n"))
	if len(fs) != 1 || fs[0].Path != "addons.pg.version" || !strings.Contains(fs[0].Problem, "want a string, not a number") {
		t.Fatalf("version: %+v", fs)
	}
	// "on" is a job's word, never a boolean.
	s, fs := ParseYAML([]byte("jobs:\n  m: {image: a, on: deploy}\n"))
	if len(fs) > 0 || s.Jobs["m"].On != "deploy" {
		t.Fatalf("on: %+v %+v", s.Jobs, fs)
	}
}

// What the reader refuses is an error finding at its path.
func TestYAMLRefusals(t *testing.T) {
	for _, c := range []struct {
		name, raw, path, problem string
	}{
		{"empty", "", "", "the request has no spec"},
		{"comments", "# nothing\n\n# here\n", "", "the request has no spec"},
		{"syntax", "services:\n  web: [a\n", "", "the spec is not valid YAML: line "},
		{"tab", "services:\n\tweb: {}\n", "", "the spec is not valid YAML: line "},
		{"two documents", "services: {}\n---\njobs: {}\n", "", "send one YAML document"},
		{"a mapping key", "services:\n  ? {a: 1}\n  : x\n", "services", "a key is a name"},
		{"a list key", "services:\n  ? [a]\n  : x\n", "services", "a key is a name"},
		{"merge key", "base: &b {image: a}\nservices:\n  web:\n    <<: *b\n    replicas: 1\n", "services.web.<<",
			"merge keys (<<) are not read"},
		{"tag", "services:\n  web:\n    replicas: !!int 2\n", "services.web.replicas", "tags are not read"},
		{"custom tag", "services: !thing {}\n", "services", "tags are not read"},
		{"binary", "v: !!binary aGk=\n", "v", "tags are not read"},
		{"timestamp", "v: !!timestamp 2026-01-01\n", "v", "tags are not read"},
		{"inf", "v: .inf\n", "v", "not a number JSON can carry"},
		{"-inf", "v: -.Inf\n", "v", "not a number JSON can carry"},
		{"nan", "v: .NaN\n", "v", "not a number JSON can carry"},
		{"float out of range", "v: 1e999\n", "v", "not a number JSON can carry"},
		{"duplicate", "services:\n  web: {}\n  web: {}\n", "services.web", `the key "web" is given twice`},
		{"unknown anchor", "v: *nope\n", "", "the spec is not valid YAML"},
		{"too large", "v: " + strings.Repeat("a", MaxSpecBytes) + "\n", "", "at most"},
		{"non-specific tag", "v: ! 123\n", "v", "tags are not read (!)"},
		{"non-specific tag, anchored", "v: &x ! 123\nw: *x\n", "v", "tags are not read (!)"},
		{"non-specific tag, then an anchor", "v: ! &x 123\n", "v", "tags are not read (!)"},
		{"non-specific tag on its own line", "v: &x\n  ! 2\n", "v", "tags are not read (!)"},
		{"non-specific tag, empty", "v: !\n", "v", "tags are not read (!)"},
		{"non-specific tag in a flow list", "v: [1, ! 2]\n", "v[1]", "tags are not read (!)"},
		{"non-specific tag on a mapping", "v: !\n  k: 1\n", "v", "tags are not read (!)"},
		{"non-specific tag on a flow mapping", "v: ! {k: 1}\n", "v", "tags are not read (!)"},
		{"non-specific tag on a key", "! k: 1\n", "k", "tags are not read (!)"},
		{"non-specific tag on a key in a list", "- ! k: 1\n", "[0].k", "tags are not read (!)"},
		{"non-specific tag after characters", "ä: [ö, ! 1]\n", "ä[1]", "tags are not read (!)"},
		{"non-specific tag, CRLF and BOM", "\ufeffa: 1\r\nb: ! 2\r\n", "b", "tags are not read (!)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			raw, fs := YAMLToJSON([]byte(c.raw))
			if raw != nil || len(fs) == 0 {
				t.Fatalf("%s %+v", raw, fs)
			}
			f := fs[0]
			if f.Severity != SeverityError || f.Path != c.path || !strings.Contains(f.Problem, c.problem) || f.Remedy == "" {
				t.Fatalf("%+v, want %q at %q", fs, c.problem, c.path)
			}
			if _, pfs := ParseYAML([]byte(c.raw)); !reflect.DeepEqual(pfs, fs) {
				t.Fatalf("ParseYAML %+v, YAMLToJSON %+v", pfs, fs)
			}
		})
	}
	// What only looks like the tag "!" is text.
	raw, fs := YAMLToJSON([]byte("a: x ! y\nb: \"! 1\"\nc: |\n  ! 2\n!!str d: '!'\ne: [!!str 1, '!']\n"))
	if len(fs) > 0 || string(raw) != `{"a":"x ! y","b":"! 1","c":"! 2\n","d":"!","e":["1","!"]}` {
		t.Fatalf("%s %+v", raw, fs)
	}
	// A quoted "<<" is an ordinary key.
	raw, fs = YAMLToJSON([]byte(`"<<": 1`))
	if len(fs) > 0 || string(raw) != `{"\u003c\u003c":1}` {
		t.Fatalf("%s %+v", raw, fs)
	}
	// The root may be any value; ParseYAML wants an object, as Parse.
	if raw, fs := YAMLToJSON([]byte("- a\n- 1\n")); len(fs) > 0 || string(raw) != `["a",1]` {
		t.Fatalf("%s %+v", raw, fs)
	}
	if _, fs := ParseYAML([]byte("- a\n")); len(fs) != 1 || !strings.Contains(fs[0].Problem, "a list, not an object") {
		t.Fatalf("%+v", fs)
	}
}

// Aliases count once per use: a billion laughs passes MaxSpecBytes and is
// refused without being built, and nesting deeper than maxDepth is.
func TestYAMLAliasBudget(t *testing.T) {
	var b strings.Builder
	b.WriteString("a0: &a0 [lol, lol, lol, lol, lol, lol, lol, lol, lol, lol]\n")
	for i := 1; i < 10; i++ {
		fmt.Fprintf(&b, "a%d: &a%d [", i, i)
		for j := range 10 {
			if j > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "*a%d", i-1)
		}
		b.WriteString("]\n")
	}
	raw, fs := YAMLToJSON([]byte(b.String()))
	if raw != nil || len(fs) != 1 || !strings.Contains(fs[0].Problem, "the spec as JSON is larger than") ||
		!strings.Contains(fs[0].Problem, "aliases count once per use") {
		t.Fatalf("%d bytes, %+v", len(raw), fs)
	}
	if _, pfs := ParseYAML([]byte(b.String())); !reflect.DeepEqual(pfs, fs) {
		t.Fatalf("ParseYAML %+v", pfs)
	}
	// A few uses that fit are expanded in full.
	raw, fs = YAMLToJSON([]byte("a: &x [1, 2]\nb: *x\nc: [*x, *x]\n"))
	if len(fs) > 0 || string(raw) != `{"a":[1,2],"b":[1,2],"c":[[1,2],[1,2]]}` {
		t.Fatalf("%s %+v", raw, fs)
	}
	deep := strings.Repeat("[", maxDepth+2) + strings.Repeat("]", maxDepth+2)
	if _, fs := YAMLToJSON([]byte("v: " + deep)); len(fs) != 1 || !strings.Contains(fs[0].Problem, "nests deeper") {
		t.Fatalf("deep: %+v", fs)
	}
	// An alias is a level: aliases of aliases nest too.
	b.Reset()
	b.WriteString("a0: &a0 [x]\n")
	for i := 1; i <= maxDepth; i++ {
		fmt.Fprintf(&b, "a%d: &a%d [*a%d]\n", i, i, i-1)
	}
	if _, fs := YAMLToJSON([]byte(b.String())); len(fs) == 0 || !strings.Contains(fs[0].Problem, "nests deeper") {
		t.Fatalf("deep aliases: %+v", fs)
	}
}

// A refusal behind aliases is not repeated without end: every node
// visited counts, and the reading stops at maxYAMLErrors errors.
func TestYAMLRefusalsBehindAliases(t *testing.T) {
	laughs := func(leaf string, width int) string {
		var b strings.Builder
		fmt.Fprintf(&b, "l0: &l0 [%s", leaf)
		for range width - 1 {
			b.WriteString(", " + leaf)
		}
		b.WriteString("]\n")
		for i := 1; i < 9; i++ {
			fmt.Fprintf(&b, "l%d: &l%d [*l%d", i, i, i-1)
			for range width - 1 {
				fmt.Fprintf(&b, ", *l%d", i-1)
			}
			b.WriteString("]\n")
		}
		return b.String()
	}
	for _, doc := range []string{laughs("!!int 1", 10), laughs(".inf", 10), laughs("{[a]: 1}", 10),
		laughs("! 1", 10), laughs("!!int 1", 20), laughs("{<<: {}}", 10), laughs("{!!int 1: x}", 10)} {
		start := time.Now()
		_, fs := ParseYAML([]byte(doc))
		if len(fs) == 0 || len(fs) > maxYAMLErrors+1 || time.Since(start) > 5*time.Second {
			t.Fatalf("%.40q: %d findings in %s", doc, len(fs), time.Since(start))
		}
		if last := fs[len(fs)-1]; len(fs) == maxYAMLErrors+1 && !strings.Contains(fs[0].Problem+last.Problem,
			"the reading stopped after") {
			t.Fatalf("%.40q: %+v", doc, fs[:2])
		}
	}
	// A wide list of refusals used many times: one document of 22 KB.
	var b strings.Builder
	b.WriteString("a: &a [!!int 1" + strings.Repeat(", !!int 1", 1999) + "]\nb: [*a" + strings.Repeat(", *a", 1999) + "]\n")
	if _, fs := YAMLToJSON([]byte(b.String())); len(fs) != maxYAMLErrors+1 {
		t.Fatalf("%d findings", len(fs))
	}
	// Every node counts: aliases of an empty list, which writes little.
	b.Reset()
	b.WriteString("l0: &l0 []\n")
	for i := 1; i < 9; i++ {
		fmt.Fprintf(&b, "l%d: &l%d [*l%d%s]\n", i, i, i-1, strings.Repeat(fmt.Sprintf(", *l%d", i-1), 9))
	}
	if _, fs := YAMLToJSON([]byte(b.String())); len(fs) != 1 || !strings.Contains(fs[0].Problem, "larger than") &&
		!strings.Contains(fs[0].Problem, "nodes") {
		t.Fatalf("%+v", fs)
	}
}

// The reader counts the bytes of the JSON exactly: separators, escapes
// and characters included, so MaxSpecBytes bounds the JSON as Parse does.
func TestYAMLSizeIsTheJSONs(t *testing.T) {
	for _, doc := range []string{
		"a: 1\n", "[]\n", "{}\n", "[1, 2, [3, {}], {a: [], b: {}}]\n",
		"a: \"<>&\\u00e9\\n\\t\\x01\\u2028 \\\\ \\\"\"\n\"k<>\\\"\": \"\\xff\"\n",
		"ä: [ö, ü, \"\\U0001F600\"]\nb: &x {c: [true, false, null, -1.5e3, 0x1f]}\nd: [*x, *x]\n",
		string(readFile(t, "testdata/expensa-stage-request.json")),
		expensa,
	} {
		var r yamlReader
		root, fs := r.read([]byte(doc))
		if len(fs) > 0 {
			t.Fatalf("%.40q: %+v", doc, fs)
		}
		if n := len(root.bytes()); r.size != n {
			t.Fatalf("%.40q: counted %d, JSON has %d: %s", doc, r.size, n, root.bytes())
		}
	}
	// Aliases that stand for a JSON larger than MaxSpecBytes, separators
	// included, are refused.
	doc := "a: &a [1" + strings.Repeat(",1", 160000-1) + "]\nb: *a\nc: *a\nd: *a\ne: *a\nf: *a\n"
	raw, fs := YAMLToJSON([]byte(doc))
	if raw != nil || len(fs) != 1 || !strings.Contains(fs[0].Problem, "the spec as JSON is larger than") {
		t.Fatalf("%d bytes, %+v", len(raw), fs)
	}
}
