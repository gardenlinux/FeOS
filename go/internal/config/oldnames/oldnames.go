// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package oldnames refuses a start under the environment variables of
// before ADR 0061. It is a package of its own so that the binaries that
// read no ferund flag (ferun-sink, ferun-ingress, ferun-auth) do not link
// ferund's configuration.
package oldnames

import (
	"fmt"
	"sort"
	"strings"
)

// prefix begins every variable of FeRun's binaries (config.EnvPrefix).
const prefix = "FERUN_"

// oldWord is the word ADR 0061 took out of every flag and so of every
// environment variable, whose names say PROJECT in its place.
const oldWord = "TENANT" // KEEP-TENANT: the start refusal's pattern

// Refuse refuses to start when an environment variable whose name
// begins with FERUN_ holds the word of before ADR 0061, naming each such
// variable and its new name. Unknown FERUN_ variables are otherwise
// ignored, so a landscape that kept an old name would fall back to the
// default without a word. ferund, ferun-sink, ferun-ingress and ferun-auth
// call it with os.Environ() before they read their flags.
func Refuse(environ []string) error {
	var old []string
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, prefix) && strings.Contains(name, oldWord) {
			old = append(old, name)
		}
	}
	if len(old) == 0 {
		return nil
	}
	sort.Strings(old)
	parts := make([]string, len(old))
	for i, name := range old {
		parts[i] = name + " is " + strings.ReplaceAll(name, oldWord, "PROJECT") + " now"
	}
	return fmt.Errorf("environment variables of an earlier name (ADR 0061): %s", strings.Join(parts, "; "))
}
