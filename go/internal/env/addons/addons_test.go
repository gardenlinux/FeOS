// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package addons

import (
	"slices"
	"testing"

	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
)

// fake is a blueprint of its own type, for the registry's test.
type fake struct{ typ string }

func (f fake) Type() string                          { return f.typ }
func (fake) Versions() []string                      { return []string{"1"} }
func (fake) Plans() map[string]Plan                  { return map[string]Plan{"dev": {1, 128, 256}} }
func (fake) DefaultPlan() string                     { return "dev" }
func (fake) Validate(*Addon, Addon) []spec.Finding   { return nil }
func (fake) Credentials(Addon) []string              { return []string{"password"} }
func (fake) Render(Addon, Context) (Instance, error) { return Instance{Image: "fake:1"}, nil }
func (fake) OutputKeys() []string                    { return []string{"url"} }
func (fake) OutputFields() []string                  { return nil }
func (fake) Options() []Option                       { return nil }
func (fake) Provision(Addon, Context) []Step         { return nil }
func (fake) Outputs(Addon, Context, Ref) ([]model.TemplatePart, error) {
	return []model.TemplatePart{{Value: "fake://"}, {Credential: "password", Escape: model.EscapeURL}}, nil
}

// unregister takes a test's blueprint out of the registry again, so that
// the shipped blueprints' tests see only theirs.
func unregister(t *testing.T, typ string) {
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		delete(registry, typ)
	})
}

func TestRegistry(t *testing.T) {
	const a, b = "zz-test-a", "zz-test-b"
	unregister(t, a)
	unregister(t, b)
	before := Types()
	if err := Register(fake{a}); err != nil {
		t.Fatal(err)
	}
	if err := Register(fake{b}); err != nil {
		t.Fatal(err)
	}
	if err := Register(fake{a}); err == nil {
		t.Fatal("a type registered twice")
	}
	if err := Register(fake{""}); err == nil {
		t.Fatal("a blueprint without a type registered")
	}
	if err := Register(nil); err == nil {
		t.Fatal("a nil blueprint registered")
	}
	got, ok := Get(a)
	if !ok || got.Type() != a {
		t.Fatalf("Get(%q) = %v, %v", a, got, ok)
	}
	if _, ok := Get("zz-test-none"); ok {
		t.Fatal("Get of an unregistered type")
	}
	want := slices.Sorted(slices.Values(append(before, a, b)))
	if types := Types(); !slices.Equal(types, want) {
		t.Fatalf("Types() = %v, want %v", types, want)
	}
	if s := CredentialSecret("postgres", "role.expense_svc.password"); s != "addon.postgres.role.expense_svc.password" {
		t.Fatal(s)
	}
}
