// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"

	fapi "github.com/gardenlinux/feos/go/ferun/api"
	"github.com/gardenlinux/feos/go/internal/auth"
)

// The headers and codes ferund defines for itself are those the published
// module gives FeCode: one changed alone, they would disagree without a
// fixture noticing.
func TestWordsAreTheModules(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, ferund, module string }{
		{"project header", auth.ProjectHeader, fapi.ProjectHeader},
		{"person header", auth.PersonHeader, fapi.PersonHeader},
		{"person header refused", codePersonHeaderRefused, fapi.CodePersonHeaderRefused},
	} {
		if c.ferund != c.module {
			t.Errorf("%s: ferund %q, the module %q", c.name, c.ferund, c.module)
		}
	}
}
