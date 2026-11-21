// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"regexp"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// The reader of YAML (ADR 0057): a spec in YAML is read as the JSON it
// stands for and then parsed as JSON is, on the same tree, so that every
// rule and every finding's path is Parse's. The reading is strict and
// plain:
//
//   - one document; empty or only comments is no spec;
//   - a key is its scalar text, whatever its style ("true:" is the key
//     "true"), and a mapping holds it once; a key that is a mapping or a
//     list is refused;
//   - a plain scalar has the type YAML 1.2's core schema gives it: null,
//     true and false, integers (decimal, 0o octal, 0x hex) and floats;
//     everything else, "on", "yes", "1_000" and "2026-01-01" among it, is
//     a string; .inf and .nan are refused, as JSON has no such numbers;
//   - a quoted, literal or folded scalar is a string, and so is one
//     tagged !!str; every other tag is refused;
//   - aliases are followed, each use counting in full towards the size
//     of the JSON, which MaxSpecBytes bounds; merge keys (<<) are refused.
//
// The library is go.yaml.in/yaml/v3, its node API only: its own
// resolution of scalars is YAML 1.1's in parts.

// YAMLToJSON returns the JSON the one YAML document raw stands for and
// the findings of its reading, with paths as Parse gives them; json is
// nil when a finding is an error. A key held twice is an error finding,
// as Parse has it. It knows no field of the spec: the JSON may be any
// value, which Parse then reads.
func YAMLToJSON(raw []byte) (json []byte, findings []Finding) {
	root, fs := readYAML(raw)
	if HasErrors(fs) {
		return nil, fs
	}
	var p parser
	p.duplicates(root, "")
	if len(p.findings) > 0 {
		return nil, SortFindings(p.findings)
	}
	return root.bytes(), nil
}

// ParseYAML reads a spec in YAML: YAMLToJSON's reading, then the same
// strict reading as Parse's on the same tree, without a round trip
// through JSON. One rule set, one set of paths.
func ParseYAML(raw []byte) (Spec, []Finding) {
	root, fs := readYAML(raw)
	if HasErrors(fs) {
		return Spec{}, fs
	}
	var p parser
	return p.spec(root)
}

// yamlReader reads a document into Parse's tree and bounds what its
// aliases expand to: size counts the bytes of JSON the nodes read so far
// write at least (each node one or more), which MaxSpecBytes bounds, and
// the nesting counts an alias as a level.
type yamlReader struct {
	parser
	size int
}

// readYAML reads raw's one document into Parse's tree.
func readYAML(raw []byte) (*node, []Finding) {
	if len(raw) > MaxSpecBytes {
		return nil, []Finding{{Severity: SeverityError, Path: "",
			Problem: fmt.Sprintf("the spec has %d bytes; at most %d are allowed", len(raw), MaxSpecBytes),
			Remedy:  "split the environment, or move large values into secrets or the images"}}
	}
	noSpec := []Finding{{Severity: SeverityError, Path: "", Problem: "the request has no spec",
		Remedy: "send the spec as a YAML document"}}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, noSpec
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, noSpec
		}
		return nil, []Finding{yamlSyntaxFinding(err)}
	}
	var next yaml.Node
	switch err := dec.Decode(&next); {
	case errors.Is(err, io.EOF):
	case err != nil:
		return nil, []Finding{yamlSyntaxFinding(err)}
	default:
		return nil, []Finding{{Severity: SeverityError, Path: "", Problem: "send one YAML document",
			Remedy: "remove the documents after the first (\"---\")"}}
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil, noSpec
	}
	var r yamlReader
	root := r.node(doc.Content[0], "", 0)
	if len(r.findings) > 0 {
		return nil, SortFindings(r.findings)
	}
	return root, nil
}

// yamlSyntaxFinding is the finding of a document that is not YAML.
func yamlSyntaxFinding(err error) Finding {
	return Finding{Severity: SeverityError, Path: "",
		Problem: "the spec is not valid YAML: " + strings.TrimPrefix(err.Error(), "yaml: "),
		Remedy:  "send the spec as one YAML document"}
}

// add counts n bytes of JSON; false once they pass MaxSpecBytes, with
// the finding at path (once).
func (r *yamlReader) add(n int, path string) bool {
	if r.size > MaxSpecBytes {
		return false
	}
	r.size += n
	if r.size > MaxSpecBytes {
		r.errorf(path, "write the repeated parts out fewer times, or split the environment",
			"the spec as JSON is larger than %d bytes (aliases count once per use)", MaxSpecBytes)
		return false
	}
	return true
}

// node reads y at path; nil (with a finding) for what it refuses.
func (r *yamlReader) node(y *yaml.Node, path string, depth int) *node {
	if depth > maxDepth {
		r.errorf(path, "nest the spec less deeply", "the spec nests deeper than %d levels", maxDepth)
		return nil
	}
	if r.size > MaxSpecBytes {
		return nil
	}
	if y.Kind == yaml.AliasNode {
		if y.Alias == nil {
			r.errorf(path, "define the anchor before its alias", "an alias names no anchor")
			return nil
		}
		return r.node(y.Alias, path, depth+1)
	}
	if y.Style&yaml.TaggedStyle != 0 && !(y.Kind == yaml.ScalarNode && y.Tag == "!!str") {
		r.errorf(path, "quote a string", "tags are not read (%s)", y.Tag)
		return nil
	}
	switch y.Kind {
	case yaml.ScalarNode:
		n := r.scalar(y, path)
		if n != nil && !r.add(n.size(), path) {
			return nil
		}
		return n
	case yaml.SequenceNode:
		if !r.add(2, path) {
			return nil
		}
		n := &node{kind: kindArray, arr: make([]*node, 0, len(y.Content))}
		for i, c := range y.Content {
			v := r.node(c, index(path, i), depth+1)
			if v == nil {
				continue
			}
			n.arr = append(n.arr, v)
		}
		return n
	case yaml.MappingNode:
		if !r.add(2, path) {
			return nil
		}
		n := &node{kind: kindObject, obj: make([]member, 0, len(y.Content)/2)}
		for i := 0; i+1 < len(y.Content); i += 2 {
			k, ok := r.key(y.Content[i], path)
			if !ok {
				continue
			}
			kp := join(path, k)
			if !r.add(len(k)+3, kp) {
				return nil
			}
			v := r.node(y.Content[i+1], kp, depth+1)
			if v == nil {
				continue
			}
			n.obj = append(n.obj, member{key: k, val: v})
		}
		return n
	}
	r.errorf(path, "send the spec as a YAML document", "this YAML node cannot be read")
	return nil
}

// key is a mapping key's text; false (with a finding) for a key that is
// no name.
func (r *yamlReader) key(y *yaml.Node, path string) (string, bool) {
	for y.Kind == yaml.AliasNode && y.Alias != nil {
		y = y.Alias
	}
	switch {
	case y.Kind != yaml.ScalarNode:
		r.errorf(path, "write it as text", "a key is a name: this one is a mapping or a list")
		return "", false
	case y.Style&yaml.TaggedStyle == 0 && y.Tag == "!!merge":
		r.errorf(join(path, "<<"), "write the keys out", "merge keys (<<) are not read")
		return "", false
	case y.Style&yaml.TaggedStyle != 0 && y.Tag != "!!str":
		r.errorf(join(path, y.Value), "quote a string", "tags are not read (%s)", y.Tag)
		return "", false
	}
	return y.Value, true
}

const quotedStyles = yaml.DoubleQuotedStyle | yaml.SingleQuotedStyle | yaml.LiteralStyle | yaml.FoldedStyle

// The plain scalars of YAML 1.2's core schema (§10.3.2).
var (
	yamlInt   = regexp.MustCompile(`^[-+]?[0-9]+$`)
	yamlOct   = regexp.MustCompile(`^0o[0-7]+$`)
	yamlHex   = regexp.MustCompile(`^0x[0-9a-fA-F]+$`)
	yamlFloat = regexp.MustCompile(`^([-+]?)(\.[0-9]+|[0-9]+(\.[0-9]*)?)([eE][-+]?[0-9]+)?$`)
	yamlInf   = regexp.MustCompile(`^[-+]?\.(inf|Inf|INF)$`)
	yamlNaN   = regexp.MustCompile(`^\.(nan|NaN|NAN)$`)
)

// scalar is a scalar's value: a string when it is quoted, literal,
// folded or tagged !!str, else the core schema's.
func (r *yamlReader) scalar(y *yaml.Node, path string) *node {
	v := y.Value
	if y.Style&(quotedStyles|yaml.TaggedStyle) != 0 {
		return &node{kind: kindString, str: v}
	}
	switch v {
	case "", "~", "null", "Null", "NULL":
		return &node{kind: kindNull}
	case "true", "True", "TRUE":
		return &node{kind: kindBool, b: true}
	case "false", "False", "FALSE":
		return &node{kind: kindBool, b: false}
	}
	var i big.Int
	switch {
	case yamlInt.MatchString(v):
		i.SetString(strings.TrimPrefix(v, "+"), 10)
		return &node{kind: kindNumber, num: json.Number(i.String())}
	case yamlOct.MatchString(v):
		i.SetString(v[2:], 8)
		return &node{kind: kindNumber, num: json.Number(i.String())}
	case yamlHex.MatchString(v):
		i.SetString(v[2:], 16)
		return &node{kind: kindNumber, num: json.Number(i.String())}
	case yamlInf.MatchString(v) || yamlNaN.MatchString(v):
		r.errorf(path, `quote it for a string: "`+v+`"`, "%s is not a number JSON can carry", v)
		return nil
	}
	if m := yamlFloat.FindStringSubmatch(v); m != nil {
		num := jsonFloat(m[1], m[2], m[4])
		if _, err := strconv.ParseFloat(num, 64); err != nil {
			r.errorf(path, `quote it for a string: "`+v+`"`, "%s is not a number JSON can carry", v)
			return nil
		}
		return &node{kind: kindNumber, num: json.Number(num)}
	}
	return &node{kind: kindString, str: v}
}

// jsonFloat is a YAML float's sign, mantissa and exponent as a JSON
// number: no "+", no leading zeros, a digit on both sides of the point
// (".5" is 0.5, "1." is 1.0).
func jsonFloat(sign, mantissa, exp string) string {
	whole, frac, dot := strings.Cut(mantissa, ".")
	whole = strings.TrimLeft(whole, "0")
	if whole == "" {
		whole = "0"
	}
	out := whole
	if dot {
		if frac == "" {
			frac = "0"
		}
		out += "." + frac
	}
	if sign == "-" {
		out = "-" + out
	}
	return out + exp
}

// size is the bytes of JSON a scalar node writes, at least.
func (n *node) size() int {
	switch n.kind {
	case kindBool:
		if n.b {
			return 4
		}
		return 5
	case kindNumber:
		return len(n.num)
	case kindString:
		return len(n.str) + 2
	}
	return 4
}
