// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import "github.com/gardenlinux/feos/go/internal/model"

// The words of the release states and of an instance's stopping phase
// changed (ADR 0018, "The states' words"): the code and the API say
// waiting, deploying, deployed, replaced and stopping. The database still
// stores pending, rolling, realised, superseded and retiring, the words the
// previous version reads while a rolling deploy runs it beside this one: it
// would take a release in a state it does not know for one that is not
// rolling out, and move its services without waiting for its addons and
// release jobs; it would neither delete an instance in a phase it does not
// know nor leave it out of the routes and the names. So the store writes
// the earlier words and reads both, and the stored words change in two
// releases of their own:
//
//  1. The next version writes the new words; its migration lets the CHECKs
//     of env_releases.state and env_instances.phase admit both, and
//     rewrites the rows. This version, beside it, reads them.
//  2. The one after it reads the new words only; its migration rewrites
//     what the version before wrote meanwhile, and lets the CHECKs admit
//     the new words only. This file goes with it.

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

// storedWord is the word the store writes for w: its earlier word, where
// earlier has one.
func storedWord(earlier map[string]string, w string) string {
	if e, ok := earlier[w]; ok {
		return e
	}
	return w
}

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
