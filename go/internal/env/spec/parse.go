// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ErrNotImplemented is the error of a function that a later lane
// implements.
var ErrNotImplemented = errors.New("spec: not implemented yet")

// maxDepth bounds how deeply a spec's JSON may nest: far beyond any spec,
// low enough that a hostile one cannot make the reader recurse without
// end.
const maxDepth = 64

// Parse reads a spec strictly: raw must be one JSON object of at most
// MaxSpecBytes; an unknown field anywhere, a value of the wrong type, an
// env value of no known form, a duplicate key and trailing data are error
// findings with their paths, all of them, not only the first. A null
// stands for an absent list, map or object (as encoding/json reads it);
// for a string, a number or a boolean it is a value of the wrong type.
// The Spec holds what could be read; it is meaningful only without error
// findings.
func Parse(raw []byte) (Spec, []Finding) {
	if len(raw) > MaxSpecBytes {
		return Spec{}, []Finding{{Severity: SeverityError, Path: "",
			Problem: fmt.Sprintf("the spec has %d bytes; at most %d are allowed", len(raw), MaxSpecBytes),
			Remedy:  "split the environment, or move large values into secrets or the images"}}
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return Spec{}, []Finding{{Severity: SeverityError, Path: "", Problem: "the request has no spec",
			Remedy: "send the spec as a JSON object"}}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	root, err := readNode(dec, 0)
	if err != nil {
		return Spec{}, []Finding{syntaxFinding(err)}
	}
	var p parser
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		p.errorf("", "send exactly one JSON object", "the spec has data after its end")
	}
	if root.kind != kindObject {
		p.errorf("", "send the spec as a JSON object", "the spec is %s, not an object", root.kindName())
		return Spec{}, p.findings
	}
	p.duplicates(root, "")
	var s Spec
	p.decode(root, reflect.ValueOf(&s).Elem(), "")
	return s, SortFindings(p.findings)
}

// syntaxFinding is the finding of a spec that is not JSON.
func syntaxFinding(err error) Finding {
	msg := err.Error()
	var se *json.SyntaxError
	if errors.As(err, &se) {
		msg = fmt.Sprintf("%s (at byte %d)", se.Error(), se.Offset)
	} else if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		msg = "it ends before its last value is complete"
	}
	return Finding{Severity: SeverityError, Path: "", Problem: "the spec is not valid JSON: " + msg,
		Remedy: "send the spec as one JSON object"}
}

// EqualNormalised reports whether two normalised specs are the same
// deployment: their canonical JSON (map keys sorted by the encoder) is
// equal, a deploy job's on in either word. Specs that are not normalised
// may differ only in form.
func EqualNormalised(a, b Spec) bool {
	ja, errA := json.Marshal(oneJobWord(a))
	jb, errB := json.Marshal(oneJobWord(b))
	return errA == nil && errB == nil && string(ja) == string(jb)
}

// oneJobWord is s with each deploy job's On JobOnDeploy, whichever word
// it was stored in (JobOnStored), so that two versions in a rolling
// deploy, which store different words, see the same spec.
func oneJobWord(s Spec) Spec {
	if len(s.Jobs) == 0 {
		return s
	}
	jobs := make(map[string]Job, len(s.Jobs))
	for name, j := range s.Jobs {
		if IsJobOnDeploy(j.On) {
			j.On = JobOnDeploy
		}
		jobs[name] = j
	}
	s.Jobs = jobs
	return s
}

// SortFindings orders findings by path, keeping the order of those of one
// path, and returns them; nil stays nil.
func SortFindings(fs []Finding) []Finding {
	sort.SliceStable(fs, func(i, j int) bool { return fs[i].Path < fs[j].Path })
	return fs
}

// The kinds of a JSON value as Parse reads it.
type nodeKind int

const (
	kindNull nodeKind = iota
	kindBool
	kindNumber
	kindString
	kindArray
	kindObject
)

// node is a JSON value with every member of an object in order,
// duplicates included, so that Parse can name what a decoder would hide.
type node struct {
	kind nodeKind
	b    bool
	num  json.Number
	str  string
	arr  []*node
	obj  []member
}

type member struct {
	key string
	val *node
}

func (n *node) kindName() string {
	switch n.kind {
	case kindBool:
		return "a boolean"
	case kindNumber:
		return "a number"
	case kindString:
		return "a string"
	case kindArray:
		return "a list"
	case kindObject:
		return "an object"
	}
	return "null"
}

// readNode reads the next value of dec.
func readNode(dec *json.Decoder, depth int) (*node, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("it nests deeper than %d levels", maxDepth)
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case nil:
		return &node{kind: kindNull}, nil
	case bool:
		return &node{kind: kindBool, b: t}, nil
	case json.Number:
		return &node{kind: kindNumber, num: t}, nil
	case string:
		return &node{kind: kindString, str: t}, nil
	case json.Delim:
		switch t {
		case '{':
			n := &node{kind: kindObject}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("an object key is %v, not a string", kt)
				}
				v, err := readNode(dec, depth+1)
				if err != nil {
					return nil, err
				}
				n.obj = append(n.obj, member{key: key, val: v})
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return n, nil
		case '[':
			n := &node{kind: kindArray}
			for dec.More() {
				v, err := readNode(dec, depth+1)
				if err != nil {
					return nil, err
				}
				n.arr = append(n.arr, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return n, nil
		}
	}
	return nil, fmt.Errorf("unexpected %v", tok)
}

// encode writes n as JSON (a duplicate key's last value wins, as for a
// decoder).
func (n *node) encode(buf *bytes.Buffer) {
	switch n.kind {
	case kindNull:
		buf.WriteString("null")
	case kindBool:
		buf.WriteString(strconv.FormatBool(n.b))
	case kindNumber:
		buf.WriteString(n.num.String())
	case kindString:
		b, _ := json.Marshal(n.str)
		buf.Write(b)
	case kindArray:
		buf.WriteByte('[')
		for i, v := range n.arr {
			if i > 0 {
				buf.WriteByte(',')
			}
			v.encode(buf)
		}
		buf.WriteByte(']')
	case kindObject:
		buf.WriteByte('{')
		for i, m := range n.obj {
			if i > 0 {
				buf.WriteByte(',')
			}
			b, _ := json.Marshal(m.key)
			buf.Write(b)
			buf.WriteByte(':')
			m.val.encode(buf)
		}
		buf.WriteByte('}')
	}
}

func (n *node) bytes() []byte {
	var buf bytes.Buffer
	n.encode(&buf)
	return buf.Bytes()
}

// value is n as encoding/json decodes it into an any: numbers float64.
func (n *node) value() any {
	switch n.kind {
	case kindBool:
		return n.b
	case kindNumber:
		f, _ := n.num.Float64()
		return f
	case kindString:
		return n.str
	case kindArray:
		out := make([]any, len(n.arr))
		for i, v := range n.arr {
			out[i] = v.value()
		}
		return out
	case kindObject:
		out := make(map[string]any, len(n.obj))
		for _, m := range n.obj {
			out[m.key] = m.val.value()
		}
		return out
	}
	return nil
}

// parser collects the findings of one Parse.
type parser struct {
	findings []Finding
}

func (p *parser) errorf(path, remedy, format string, args ...any) {
	p.findings = append(p.findings, Finding{Severity: SeverityError, Path: path, Problem: fmt.Sprintf(format, args...),
		Remedy: remedy})
}

// duplicates reports every key that an object of the tree has twice.
func (p *parser) duplicates(n *node, path string) {
	switch n.kind {
	case kindObject:
		seen := make(map[string]bool, len(n.obj))
		for _, m := range n.obj {
			mp := join(path, m.key)
			if seen[m.key] {
				p.errorf(mp, "write each key once", "the key %q is given twice", m.key)
			}
			seen[m.key] = true
			p.duplicates(m.val, mp)
		}
	case kindArray:
		for i, v := range n.arr {
			p.duplicates(v, index(path, i))
		}
	}
}

// join is path's member key: "a.b", or "b" at the root.
func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// index is path's list element i: "a[2]".
func index(path string, i int) string { return path + "[" + strconv.Itoa(i) + "]" }

var (
	unmarshalerType = reflect.TypeFor[json.Unmarshaler]()
	rawMessageType  = reflect.TypeFor[json.RawMessage]()
)

// decode sets v from n, reporting what does not fit v's type at path.
func (p *parser) decode(n *node, v reflect.Value, path string) {
	t := v.Type()
	if t == rawMessageType {
		if n.kind != kindNull {
			v.SetBytes(n.bytes())
		}
		return
	}
	if t.Kind() != reflect.Pointer && reflect.PointerTo(t).Implements(unmarshalerType) {
		if err := v.Addr().Interface().(json.Unmarshaler).UnmarshalJSON(n.bytes()); err != nil {
			msg := err.Error()
			if k := lastKey(path); k != "" {
				msg = strings.TrimPrefix(msg, k+": ")
			}
			p.errorf(path, "see the deployment spec's forms in ADR 0018", "%s", msg)
		}
		return
	}
	switch t.Kind() {
	case reflect.Pointer:
		if n.kind == kindNull {
			return
		}
		e := reflect.New(t.Elem())
		p.decode(n, e.Elem(), path)
		v.Set(e)
	case reflect.Struct:
		if n.kind != kindObject {
			p.wrongType(n, path, "an object")
			return
		}
		fields := fieldsOf(t)
		for _, m := range n.obj {
			i, ok := fields.byName[m.key]
			if !ok {
				p.errorf(join(path, m.key), "remove it or correct its name; the fields here are "+
					strings.Join(fields.names, ", "), "unknown field %q", m.key)
				continue
			}
			p.decode(m.val, v.Field(i), join(path, m.key))
		}
	case reflect.Slice:
		if n.kind == kindNull {
			return
		}
		if n.kind != kindArray {
			p.wrongType(n, path, "a list")
			return
		}
		s := reflect.MakeSlice(t, len(n.arr), len(n.arr))
		for i, e := range n.arr {
			p.decode(e, s.Index(i), index(path, i))
		}
		v.Set(s)
	case reflect.Map:
		if n.kind == kindNull {
			return
		}
		if n.kind != kindObject {
			p.wrongType(n, path, "an object")
			return
		}
		m := reflect.MakeMapWithSize(t, len(n.obj))
		for _, mb := range n.obj {
			e := reflect.New(t.Elem()).Elem()
			p.decode(mb.val, e, join(path, mb.key))
			m.SetMapIndex(reflect.ValueOf(mb.key).Convert(t.Key()), e)
		}
		v.Set(m)
	case reflect.Interface:
		if val := n.value(); val != nil {
			v.Set(reflect.ValueOf(val))
		}
	case reflect.String:
		if n.kind != kindString {
			p.wrongType(n, path, "a string")
			return
		}
		v.SetString(n.str)
	case reflect.Bool:
		if n.kind != kindBool {
			p.wrongType(n, path, "true or false")
			return
		}
		v.SetBool(n.b)
	case reflect.Int:
		if n.kind != kindNumber {
			p.wrongType(n, path, "a number")
			return
		}
		i, err := n.num.Int64()
		if err != nil || i < math.MinInt32 || i > math.MaxInt32 {
			p.errorf(path, "write a whole number", "%s is not a whole number in range", n.num)
			return
		}
		v.SetInt(i)
	default:
		p.errorf(path, "", "cannot read a value of type %s", t)
	}
}

func (p *parser) wrongType(n *node, path, want string) {
	p.errorf(path, "write "+want+" here", "want %s, not %s", want, n.kindName())
}

// lastKey is the last member key of a path ("port" of
// "routes.r.port"), "" for an index.
func lastKey(path string) string {
	if strings.HasSuffix(path, "]") {
		return ""
	}
	if i := strings.LastIndexByte(path, '.'); i >= 0 {
		return path[i+1:]
	}
	return path
}

// structFields are a struct type's JSON fields.
type structFields struct {
	byName map[string]int
	names  []string
}

var fieldCache sync.Map // reflect.Type → structFields

func fieldsOf(t reflect.Type) structFields {
	if f, ok := fieldCache.Load(t); ok {
		return f.(structFields)
	}
	f := structFields{byName: map[string]int{}}
	for i := range t.NumField() {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(sf.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = sf.Name
		}
		f.byName[name] = i
		f.names = append(f.names, name)
	}
	slices.Sort(f.names)
	fieldCache.Store(t, f)
	return f
}
