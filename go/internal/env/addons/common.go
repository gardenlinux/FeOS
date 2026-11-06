// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package addons

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
)

// What the blueprints share: findings, the version and plan checks, the
// options' forms, and how an address goes into a host and a URL.

// passwordKey is the credential every blueprint has: the server's own
// password (postgres's superuser, valkey's default user, nats's user).
const passwordKey = "password"

func errorf(path, problem, remedy string) spec.Finding {
	return spec.Finding{Severity: spec.SeverityError, Path: path, Problem: problem, Remedy: remedy}
}

func warning(path, problem, remedy string) spec.Finding {
	return spec.Finding{Severity: spec.SeverityWarning, Path: path, Problem: problem, Remedy: remedy}
}

func info(path, problem, remedy string) spec.Finding {
	return spec.Finding{Severity: spec.SeverityInfo, Path: path, Problem: problem, Remedy: remedy}
}

// planOf is the addon's plan by name: its own, or b's default when it
// names none.
func planOf(b Blueprint, a Addon) (name string, p Plan, ok bool) {
	name = a.Plan
	if name == "" {
		name = b.DefaultPlan()
	}
	p, ok = b.Plans()[name]
	return name, p, ok
}

// SortedPlans are b's plan names, smallest first (by memory, then name).
func SortedPlans(b Blueprint) []string {
	plans := b.Plans()
	out := slices.Collect(maps.Keys(plans))
	slices.SortFunc(out, func(x, y string) int {
		if c := plans[x].MemoryMib - plans[y].MemoryMib; c != 0 {
			return c
		}
		return strings.Compare(x, y)
	})
	return out
}

// checkVersionPlan checks next's version (required: a default would
// change under a running addon when a newer ferund ships a newer one)
// and plan against b, and, with prev of the same type, the plan change:
// the pod is recreated with the new size, while a volume (hasVolume)
// keeps the size it was created with.
func checkVersionPlan(b Blueprint, prev *Addon, next Addon, hasVolume bool) []spec.Finding {
	var fs []spec.Finding
	versions := strings.Join(b.Versions(), ", ")
	switch {
	case next.Version == "":
		fs = append(fs, errorf("version", b.Type()+" needs a version", "set version to one of "+versions))
	case !slices.Contains(b.Versions(), next.Version):
		fs = append(fs, errorf("version", fmt.Sprintf("%s has no version %q", b.Type(), next.Version),
			"use one of "+versions))
	}
	name, plan, ok := planOf(b, next)
	if !ok {
		fs = append(fs, errorf("plan", fmt.Sprintf("%s has no plan %q", b.Type(), name),
			"use one of "+strings.Join(SortedPlans(b), ", ")))
		return fs
	}
	if prev == nil || !hasVolume {
		return fs
	}
	prevName, _, prevOK := planOf(b, *prev)
	if prevOK && prevName != name {
		fs = append(fs, warning("plan",
			fmt.Sprintf("the plan changes from %s to %s: the pod is recreated with %d vCPU and %d MiB, "+
				"and the volume keeps the size it was created with (%s's %d MiB is not applied)",
				prevName, name, plan.VCPUs, plan.MemoryMib, name, plan.VolumeMib),
			"nothing to do while the data fits; growing a volume is not available yet"))
	}
	return fs
}

// sameType is prev when it is an addon of type typ (or names no type),
// else nil: a Validate treats another type's addon as none.
func sameType(prev *Addon, typ string) *Addon {
	if prev == nil || (prev.Type != "" && prev.Type != typ) {
		return nil
	}
	return prev
}

// unknownOptions are the error findings of options keys that b does not
// declare (Blueprint.Options), in key order.
func unknownOptions(b Blueprint, opts map[string]any) []spec.Finding {
	var fs []spec.Finding
	typ, known := b.Type(), optionNames(b)
	for _, k := range slices.Sorted(maps.Keys(opts)) {
		if !slices.Contains(known, k) {
			fs = append(fs, errorf("options."+k, fmt.Sprintf("%s has no option %q", typ, k),
				optionsRemedy(known)))
		}
	}
	return fs
}

// optionNames are the names of b's options, in its order.
func optionNames(b Blueprint) []string {
	var out []string
	for _, o := range b.Options() {
		out = append(out, o.Name)
	}
	return out
}

func optionsRemedy(known []string) string {
	if len(known) == 0 {
		return "remove it: this type has no options"
	}
	return "remove it; the options are " + strings.Join(known, ", ")
}

// listItem is an element of an option's list: its index and, when it is
// a string, its value (ok); else what it is instead (not).
type listItem struct {
	index int
	name  string
	ok    bool
	not   string
}

// path is the item's path below option key: "options.<key>[i]".
func (it listItem) path(key string) string {
	return "options." + key + "[" + strconv.Itoa(it.index) + "]"
}

// stringList reads option key of opts as a list: a JSON list ([]any) or
// a []string. Absent or null: none. Anything but a list is an error
// finding at "options.<key>"; an element that is not a string is an item
// that is not ok, for the caller's findings, which so come in index
// order.
func stringList(opts map[string]any, key string) ([]listItem, []spec.Finding) {
	raw, ok := opts[key]
	if !ok || raw == nil {
		return nil, nil
	}
	switch v := raw.(type) {
	case []string:
		items := make([]listItem, len(v))
		for i, s := range v {
			items[i] = listItem{index: i, name: s, ok: true}
		}
		return items, nil
	case []any:
		items := make([]listItem, len(v))
		for i, e := range v {
			s, ok := e.(string)
			items[i] = listItem{index: i, name: s, ok: ok}
			if !ok {
				items[i].not = describe(e)
			}
		}
		return items, nil
	}
	return nil, []spec.Finding{errorf("options."+key, fmt.Sprintf("want a list of names, not %s", describe(raw)),
		`write it as a JSON list: ["a", "b"]`)}
}

// names are the strings of items.
func names(items []listItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		if it.ok {
			out = append(out, it.name)
		}
	}
	return out
}

// stringOption reads option key of opts as a string; absent: def.
func stringOption(opts map[string]any, key, def string) (string, []spec.Finding) {
	raw, ok := opts[key]
	if !ok || raw == nil {
		return def, nil
	}
	s, ok := raw.(string)
	if !ok {
		return def, []spec.Finding{errorf("options."+key, fmt.Sprintf("want a string, not %s", describe(raw)),
			"write it as a JSON string")}
	}
	return s, nil
}

// boolOption reads option key of opts as a boolean; absent: def.
func boolOption(opts map[string]any, key string, def bool) (bool, []spec.Finding) {
	raw, ok := opts[key]
	if !ok || raw == nil {
		return def, nil
	}
	b, ok := raw.(bool)
	if !ok {
		return def, []spec.Finding{errorf("options."+key, fmt.Sprintf("want true or false, not %s", describe(raw)),
			"write it as a JSON boolean")}
	}
	return b, nil
}

// describe names a JSON value's kind for a finding.
func describe(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case float64, int, int64:
		return "a number"
	case string:
		return strconv.Quote(x)
	case []any, []string:
		return "a list"
	case map[string]any:
		return "an object"
	}
	return fmt.Sprintf("a %T", v)
}

// firstError is the first error finding's problem as an error, nil when
// there is none: what Render refuses an addon for.
func firstError(typ string, fs []spec.Finding) error {
	for _, f := range fs {
		if f.Severity == spec.SeverityError {
			if f.Path == "" {
				return fmt.Errorf("%s: %s", typ, f.Problem)
			}
			return fmt.Errorf("%s: %s: %s", typ, f.Path, f.Problem)
		}
	}
	return nil
}

// host is how a reference reaches the instance: its address, or, before
// it has one, the addon's name, which the environment's name table
// resolves to it.
func host(c Context) string {
	if c.Address != "" {
		return c.Address
	}
	return c.Name
}

// urlHost is host as a URL's host: an IPv6 address in brackets.
func urlHost(c Context) string {
	h := host(c)
	if strings.Contains(h, ":") && !strings.HasPrefix(h, "[") {
		return "[" + h + "]"
	}
	return h
}

// escapeUserinfo escapes s for the user or the password of a URL's
// userinfo (RFC 3986 §3.2.1): everything but the unreserved characters
// is percent-encoded. The RFC would let sub-delimiters stay; encoding
// them too is as valid and leaves no client's parser to guess. ":" and
// "@" must not stay.
func escapeUserinfo(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// shellWord is s as one word of a POSIX shell command: itself when it
// has only characters the shell leaves alone, else single-quoted.
func shellWord(s string) string {
	safe := s != ""
	for i := 0; i < len(s) && safe; i++ {
		c := s[i]
		safe = c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '_' || c == '-' || c == '.' || c == '/' || c == ':' || c == '=' || c == ','
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// literal is a one-part output of the literal s.
func literal(s string) []model.TemplatePart { return []model.TemplatePart{{Value: s}} }

// credential is a one-part output of the credential key.
func credential(key string) []model.TemplatePart { return []model.TemplatePart{{Credential: key}} }

// serverURL is scheme://<user>:<password>@<host>:<port><rest>, the
// password a credential part escaped for the userinfo.
func serverURL(scheme, user, pwKey string, c Context, port int, rest string) []model.TemplatePart {
	return []model.TemplatePart{
		{Value: scheme + "://" + escapeUserinfo(user) + ":"},
		{Credential: pwKey, Escape: model.EscapeURL},
		{Value: "@" + urlHost(c) + ":" + strconv.Itoa(port) + rest},
	}
}

// checkRef refuses a reference's key that is not among keys, and its
// fields that are not among fields.
func checkRef(typ string, ref Ref, keys []string, fields ...string) error {
	if ref.Key == "" {
		return fmt.Errorf("a reference to %s needs a key: one of %s", typ, strings.Join(keys, ", "))
	}
	if !slices.Contains(keys, ref.Key) {
		return fmt.Errorf("%s has no key %q; the keys are %s", typ, ref.Key, strings.Join(keys, ", "))
	}
	for _, f := range slices.Sorted(maps.Keys(ref.Fields)) {
		if !slices.Contains(fields, f) {
			if len(fields) == 0 {
				return fmt.Errorf("a reference to %s has no field %q; it takes only addon and key", typ, f)
			}
			return fmt.Errorf("a reference to %s has no field %q; the fields are %s", typ, f, strings.Join(fields, ", "))
		}
	}
	return nil
}
