// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package routing

import "testing"

// A wildcard covers the names one label below its name, and no other.
func TestCovers(t *testing.T) {
	for _, c := range []struct {
		pattern, host string
		want          bool
	}{
		{"app.example.com", "app.example.com", true},
		{"app.example.com", "x.app.example.com", false},
		{"*.boxed.example.com", "8080--abc.boxed.example.com", true},
		{"*.boxed.example.com", "boxed.example.com", false},
		{"*.boxed.example.com", "a.b.boxed.example.com", false},
		{"*.boxed.example.com", "x.other.example.com", false},
		{"", "", false},
	} {
		if got := Covers(c.pattern, c.host); got != c.want {
			t.Errorf("Covers(%q, %q) = %v, want %v", c.pattern, c.host, got, c.want)
		}
	}
	for host, want := range map[string]string{"a.b.c": "*.b.c", "b.c": "*.c", "c": "", "*.b.c": "", ".c": "", "c.": ""} {
		if got := WildcardOf(host); got != want {
			t.Errorf("WildcardOf(%q) = %q, want %q", host, got, want)
		}
	}
}
