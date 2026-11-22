// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import "maps"

// Portable is s carried to another place (ADR 0058), for a clone or a
// plan of one environment's deployment at another: every secret's pin
// keeps what the code expects, Type, Mode and ForwardedHost, and loses
// Version and Upstream, which are the place's; every binding that names
// a credential (credentialRef) loses its version and the binding's
// upstream and mode, which are the credential's version's. s itself is
// not changed.
func Portable(s Spec) Spec {
	if s.Secrets != nil {
		pins := make(map[string]SecretRef, len(s.Secrets))
		for name, ref := range s.Secrets {
			pins[name] = SecretRef{Type: ref.Type, Mode: ref.Mode, ForwardedHost: ref.ForwardedHost}
		}
		s.Secrets = pins
	}
	if s.Bindings != nil {
		bs := maps.Clone(s.Bindings)
		for name, b := range bs {
			if ref := b.CredentialRef; ref != nil {
				b.CredentialRef = &CredentialRef{Name: ref.Name}
				b.Upstream, b.Mode = "", ""
				bs[name] = b
			}
		}
		s.Bindings = bs
	}
	return s
}
