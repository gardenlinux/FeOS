// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"fmt"
	"strconv"
	"strings"
)

// IdleNever is the idle time of a sandbox that never expires (ADR 0065).
const IdleNever = "never"

// DefaultIdleTime is the idle time of a sandbox whose create names none,
// unless the installation sets another (--sandbox-expires-after-idle).
const DefaultIdleTime = "30d"

// MinIdleSeconds and MaxIdleSeconds bound a sandbox's idle time: an hour,
// and ten years.
const (
	MinIdleSeconds = 3600
	MaxIdleSeconds = 10 * 366 * 86400
)

// ParseIdleTime reads a sandbox's idle time as the API and the flag take
// it (ADR 0065): "<n>d", "<n>h" or "never". It returns the seconds, nil
// for never.
func ParseIdleTime(s string) (*int64, error) {
	if s == IdleNever {
		return nil, nil
	}
	unit := int64(0)
	switch {
	case strings.HasSuffix(s, "d"):
		unit = 86400
	case strings.HasSuffix(s, "h"):
		unit = 3600
	}
	n, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSuffix(s, "d"), "h"), 10, 64)
	if unit == 0 || err != nil || n <= 0 || n > MaxIdleSeconds/unit {
		return nil, fmt.Errorf("an idle time is \"<n>d\", \"<n>h\" or %q, at least 1h and at most 3660d: %q", IdleNever, s)
	}
	secs := n * unit
	return &secs, nil
}

// IdleTimeText writes an idle time as ParseIdleTime reads it: nil as
// "never", whole days as "<n>d", else hours.
func IdleTimeText(secs *int64) string {
	switch {
	case secs == nil:
		return IdleNever
	case *secs%86400 == 0:
		return fmt.Sprintf("%dd", *secs/86400)
	}
	return fmt.Sprintf("%dh", *secs/3600)
}
