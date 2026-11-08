// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package addons

import "fmt"

// TLSBinder is a blueprint whose references reach the addon through the
// referencing pod's outbound TLS binding (ADR 0036, 0038): the guest
// listens on a loopback address of the pod (Context.Loopback) at the
// addon's port and dials the addon with the pod's certificate, which the
// addon maps to the role the reference grants; the reference's values
// name the loopback address and no password.
type TLSBinder interface {
	// TLSBinding is what the binding of a reference needs: the addon's
	// port and the ALPN protocol the guest offers; ok false when the
	// reference needs none.
	TLSBinding(ref Ref) (port int, alpn string, ok bool)
}

// Loopback is the n-th loopback address of a pod's bindings, from 1:
// 127.64.0.1, 127.64.0.2, and on. The addons a pod binds are numbered in
// the order of their names, so that a re-render gives each the address it
// had; the addresses are the pod's own, in its microVM.
func Loopback(n int) string {
	return fmt.Sprintf("127.64.%d.%d", n>>8&0xff, n&0xff)
}
