// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import "github.com/gardenlinux/feos/go/internal/model"

// The words of the release states and of an instance's stopping phase
// changed (ADR 0018, "The states' words"): waiting, deploying, deployed,
// replaced and stopping, where they were pending, rolling, realised,
// superseded and retiring. The store writes the new words (migration 0022
// rewrote the rows) and reads both: the version of 0021, which a rolling
// deploy runs beside this one, still writes the earlier ones. The next
// version reads the new words only; its migration rewrites what the
// version of 0021 wrote meanwhile and lets the CHECKs of env_releases.state
// and env_instances.phase admit the new words only. This file goes with it.

// The earlier words of the release states (env_releases.state) and of the
// phases (env_instances.phase) that changed, by their current words.
var (
	earlierReleaseStates = map[string]string{
		model.ReleaseWaiting:   "pending",
		model.ReleaseDeploying: "rolling",
		model.ReleaseDeployed:  "realised",
		model.ReleaseReplaced:  "superseded",
	}
	earlierPhases = map[string]string{
		model.PhaseStopping: "retiring",
	}
)

// currentWord is the current word of a stored one, earlier or current.
func currentWord(earlier map[string]string, stored string) string {
	for cur, e := range earlier {
		if e == stored {
			return cur
		}
	}
	return stored
}

// bothWords are words with the earlier word of each that has one: what a
// query matches the stored words against.
func bothWords(earlier map[string]string, words []string) []string {
	out := make([]string, 0, 2*len(words))
	for _, w := range words {
		out = append(out, w)
		if e, ok := earlier[w]; ok {
			out = append(out, e)
		}
	}
	return out
}
