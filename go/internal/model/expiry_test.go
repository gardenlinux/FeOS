// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package model

import "testing"

func TestParseIdleTime(t *testing.T) {
	for in, want := range map[string]string{"never": "never", "30d": "30d", "12h": "12h", "48h": "2d", "1h": "1h"} {
		got, err := ParseIdleTime(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if IdleTimeText(got) != want {
			t.Fatalf("%s: %s, want %s", in, IdleTimeText(got), want)
		}
	}
	for _, in := range []string{"", "0d", "-1d", "30", "30m", "1.5d", "d", "never ", "3661d", "99999999999999999h"} {
		if _, err := ParseIdleTime(in); err == nil {
			t.Fatalf("%q: no error", in)
		}
	}
}
