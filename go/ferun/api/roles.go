// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

// The roles of a grant (ADR 0031), each including the one before.
const (
	RoleRead  = "read"
	RoleWrite = "write"
	RoleAdmin = "admin"
)

// rank orders the roles; 0 for "" and any other word.
func rank(role string) int {
	switch role {
	case RoleRead:
		return 1
	case RoleWrite:
		return 2
	case RoleAdmin:
		return 3
	}
	return 0
}

// AtLeast reports whether role have reaches want: admin reaches write
// and read, write reaches read. "" and an unknown word reach nothing,
// and nothing reaches them.
func AtLeast(have, want string) bool {
	w := rank(want)
	return w > 0 && rank(have) >= w
}

// Lower is the lower of two roles; "" when either is none.
func Lower(a, b string) string {
	if rank(a) <= rank(b) {
		if rank(a) == 0 {
			return ""
		}
		return a
	}
	if rank(b) == 0 {
		return ""
	}
	return b
}
