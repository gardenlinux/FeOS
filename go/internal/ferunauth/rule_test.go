// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ferunauth

import (
	"encoding/json"
	"testing"

	"github.com/gardenlinux/feos/go/internal/oidc"
)

func TestRuleAdmits(t *testing.T) {
	bob := visitor{Sub: "u", Email: "Bob@Example.com", EmailVerified: true, Groups: []string{"staff", "guests"}}
	unverified := bob
	unverified.EmailVerified = false
	with := func(email string) visitor { v := bob; v.Email = email; return v }
	for _, tc := range []struct {
		name string
		r    rule
		v    visitor
		want bool
	}{
		{"anyone", rule{Anyone: true}, unverified, true},
		{"an email", rule{Emails: []string{"bob@example.com"}}, bob, true},
		{"an email in upper case", rule{Emails: []string{"BOB@EXAMPLE.COM"}}, bob, true},
		{"another email", rule{Emails: []string{"ann@example.com"}}, bob, false},
		{"an email not verified", rule{Emails: []string{"bob@example.com"}}, unverified, false},
		{"a domain", rule{Domains: []string{"example.com"}}, bob, true},
		{"a domain not verified", rule{Domains: []string{"example.com"}}, unverified, false},
		{"a domain's look-alike", rule{Domains: []string{"example.com"}}, with("a@evil-example.com"), false},
		{"a subdomain", rule{Domains: []string{"example.com"}}, with("a@x.example.com"), false},
		{"the last @", rule{Domains: []string{"example.com"}}, with(`"x@example.com"@evil.io`), false},
		{"the last @, admitted", rule{Domains: []string{"evil.io"}}, with(`"x@example.com"@evil.io`), true},
		{"the Kelvin sign in the claim", rule{Emails: []string{"kim@example.com"}}, with("Kim@example.com"), false},
		{"the Kelvin sign in the rule", rule{Emails: []string{"Kim@example.com"}}, with("kim@example.com"), false},
		{"a non-ASCII domain", rule{Domains: []string{"exämple.com"}}, with("a@exämple.com"), false},
		{"no domain", rule{Domains: []string{"example.com"}}, with("a@"), false},
		{"no @", rule{Domains: []string{"example.com"}}, with("example.com"), false},
		{"no email", rule{Emails: []string{""}}, with(""), false},
		{"a group", rule{Groups: []string{"staff"}}, bob, true},
		{"a group of another case", rule{Groups: []string{"Staff"}}, bob, false},
		{"another group", rule{Groups: []string{"admins"}}, bob, false},
		{"an empty rule", rule{}, bob, false},
		{"two fields", rule{Anyone: true, Groups: []string{"admins"}}, bob, false},
	} {
		if got := tc.r.admits(tc.v); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestVisitorOf(t *testing.T) {
	claims := func(s string) oidc.Claims {
		var c oidc.Claims
		if err := json.Unmarshal([]byte(s), &c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	for _, tc := range []struct {
		name, id, info string
		email          string
		verified       bool
	}{
		{"id token", `{"sub":"u","email":"a@x.com","email_verified":true,"sid":"s"}`, `{}`, "a@x.com", true},
		{"missing", `{"sub":"u","email":"a@x.com"}`, `{}`, "a@x.com", false},
		{"a string", `{"sub":"u","email":"a@x.com","email_verified":"true"}`, `{}`, "a@x.com", true},
		{"false", `{"sub":"u","email":"a@x.com","email_verified":false}`, `{}`, "a@x.com", false},
		// The email of userinfo goes with userinfo's email_verified.
		{"userinfo's", `{"sub":"u","email":"a@x.com","email_verified":true}`, `{"sub":"u","email":"b@y.com"}`, "b@y.com", false},
	} {
		v := visitorOf(claims(tc.id), claims(tc.info))
		if v.Email != tc.email || v.EmailVerified != tc.verified || v.Sub != "u" {
			t.Errorf("%s: %+v", tc.name, v)
		}
	}
	if v := visitorOf(claims(`{"sub":"u","sid":"s-1"}`), claims(`{"sid":"other"}`)); v.Session != "s-1" {
		t.Fatalf("session %q", v.Session)
	}
}

func TestClip(t *testing.T) {
	for in, want := range map[string]string{"Ada": "Ada", " Ada\x00 Lovelace\n": "Ada Lovelace", "\xffAda": "Ada"} {
		if got := clip(in, 200); got != want {
			t.Errorf("clip(%q) = %q", in, got)
		}
	}
	if got := clip("ééé", 5); got != "éé" {
		t.Errorf("clip at a boundary: %q", got)
	}
	if validText("a\x01", 10) || validText("", 10) || validText("abc", 2) || !validText("Ädä", 10) {
		t.Error("validText")
	}
}
