// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package addons

import (
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gardenlinux/feos/go/internal/env/spec"
)

// withOptions is a (an addon with no options) with options opts, read
// from JSON as a spec's are.
func withOptions(t *testing.T, a Addon, opts map[string]any) Addon {
	t.Helper()
	b, err := json.Marshal(opts)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	a.Options = m
	return a
}

// errorsAt are the error findings at path or below it ("options.x",
// "options.x[0]").
func errorsAt(fs []spec.Finding, path string) []spec.Finding {
	var out []spec.Finding
	for _, f := range fs {
		if f.Severity == spec.SeverityError && (f.Path == path || strings.HasPrefix(f.Path, path+"[") ||
			strings.HasPrefix(f.Path, path+".")) {
			out = append(out, f)
		}
	}
	return out
}

// TestOptionsMatchValidation: what a blueprint declares (the catalog,
// GET /api/v1/addons) is what its validation does, for every registered
// blueprint and each of its options: a default that changes nothing,
// the kind, the values, the pattern, the reserved names, uniqueness; an
// undeclared option is an error, a declared reference field is one
// Outputs takes.
func TestOptionsMatchValidation(t *testing.T) {
	for _, typ := range Types() {
		b, _ := Get(typ)
		t.Run(typ, func(t *testing.T) {
			versions := b.Versions()
			base := Addon{Name: "db", Type: typ, Version: versions[len(versions)-1]}
			if fs := b.Validate(nil, base); spec.HasErrors(fs) {
				t.Fatalf("an addon without options:\n%s", findingText(fs))
			}
			c := Context{Env: "stage", Name: "db", Address: "fd6b::1"}
			want, err := b.Render(base, c)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, o := range b.Options() {
				path := "options." + o.Name
				if o.Name == "" || slices.Contains(names, o.Name) || o.Description == "" {
					t.Fatalf("option %+v: a name of its own and a description", o)
				}
				names = append(names, o.Name)

				// The default is what an addon without the option gets.
				switch o.Kind {
				case OptionString, OptionOneOf:
					d, ok := o.Default.(string)
					if !ok || o.Kind == OptionOneOf && !slices.Contains(o.Values, d) {
						t.Fatalf("%s: default %#v", o.Name, o.Default)
					}
				case OptionList:
					if _, ok := o.Default.([]string); !ok {
						t.Fatalf("%s: default %#v", o.Name, o.Default)
					}
				case OptionBoolean:
					if _, ok := o.Default.(bool); !ok {
						t.Fatalf("%s: default %#v", o.Name, o.Default)
					}
				default:
					t.Fatalf("%s: kind %q", o.Name, o.Kind)
				}
				for _, v := range []any{o.Default, nil} {
					a := withOptions(t, base, map[string]any{o.Name: v})
					if fs := b.Validate(nil, a); len(fs) != 0 {
						t.Fatalf("%s %#v:\n%s", o.Name, v, findingText(fs))
					}
					got, err := b.Render(a, c)
					if err != nil || jsonOf(t, got) != jsonOf(t, want) ||
						jsonOf(t, b.Provision(a, c)) != jsonOf(t, b.Provision(base, c)) ||
						!slices.Equal(b.Credentials(a), b.Credentials(base)) {
						t.Fatalf("%s %#v changes the addon: %v", o.Name, v, err)
					}
				}
				if o.Required {
					// An addon without it is refused.
					if len(errorsAt(b.Validate(nil, base), path)) == 0 {
						t.Fatalf("%s is required, and an addon without it passes", o.Name)
					}
				}

				// A value of another kind is an error at the option.
				wrong := map[string]any{OptionString: 5, OptionOneOf: 5, OptionList: "x", OptionBoolean: "yes"}[o.Kind]
				if fs := b.Validate(nil, withOptions(t, base, map[string]any{o.Name: wrong})); len(errorsAt(fs, path)) == 0 {
					t.Fatalf("%s %#v passes:\n%s", o.Name, wrong, findingText(fs))
				}

				// A OneOf takes its values and no other.
				for _, v := range o.Values {
					if fs := b.Validate(nil, withOptions(t, base, map[string]any{o.Name: v})); len(errorsAt(fs, path)) != 0 {
						t.Fatalf("%s %q:\n%s", o.Name, v, findingText(fs))
					}
				}
				if o.Kind == OptionOneOf {
					if fs := b.Validate(nil, withOptions(t, base, map[string]any{o.Name: "zz-none"})); len(errorsAt(fs,
						path)) == 0 {
						t.Fatalf("%s zz-none passes", o.Name)
					}
				}

				// A list's elements: the pattern, the reserved names and
				// prefixes, uniqueness.
				if o.Kind != OptionList {
					continue
				}
				// at reports an error finding at the element i of elems.
				at := func(elems []string, i int) bool {
					fs := b.Validate(nil, withOptions(t, base, map[string]any{o.Name: elems}))
					p := path + "[" + strconv.Itoa(i) + "]"
					return slices.ContainsFunc(fs, func(f spec.Finding) bool {
						return f.Severity == spec.SeverityError && f.Path == p
					})
				}
				if o.Pattern != "" {
					re, err := regexp.Compile(o.Pattern)
					if err != nil {
						t.Fatalf("%s: pattern %q: %v", o.Name, o.Pattern, err)
					}
					for _, ok := range []string{"a", "abc_1"} {
						if !re.MatchString(ok) || at([]string{ok}, 0) {
							t.Fatalf("%s: %q", o.Name, ok)
						}
					}
					for _, bad := range []string{"", "Not valid!", strings.Repeat("a", 300)} {
						if re.MatchString(bad) || !at([]string{bad}, 0) {
							t.Fatalf("%s: %q passes, or the pattern takes it", o.Name, bad)
						}
					}
				}
				for _, r := range o.Reserved {
					if !at([]string{r}, 0) {
						t.Fatalf("%s: reserved %q passes", o.Name, r)
					}
				}
				for _, p := range o.ReservedPrefixes {
					if !at([]string{p + "x"}, 0) {
						t.Fatalf("%s: reserved prefix %q passes", o.Name, p)
					}
				}
				if o.Unique && !at([]string{"a", "a"}, 1) {
					t.Fatalf("%s: a duplicate passes", o.Name)
				}
			}

			// An option the blueprint does not declare is an error.
			if fs := b.Validate(nil, withOptions(t, base, map[string]any{"zzUndeclared": true})); len(errorsAt(fs,
				"options.zzUndeclared")) == 0 {
				t.Fatalf("an undeclared option passes:\n%s", findingText(fs))
			}

			// The reference fields: the declared ones are taken, another is
			// refused.
			for _, f := range b.OutputFields() {
				_, err := b.Outputs(base, c, Ref{Key: b.OutputKeys()[0], Fields: map[string]string{f: "x"}})
				if err != nil && strings.Contains(err.Error(), "has no field") {
					t.Fatalf("field %s: %v", f, err)
				}
			}
			if _, err := b.Outputs(base, c, Ref{Key: b.OutputKeys()[0], Fields: map[string]string{"zz": "x"}}); err == nil ||
				!strings.Contains(err.Error(), "has no field") {
				t.Fatalf("an undeclared field: %v", err)
			}
		})
	}
}
