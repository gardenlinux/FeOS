// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package proxy

import "sync"

// full is the cap a request found taken when it entered a budget.
type full int

const (
	// fits: the request is counted.
	fits full = iota
	// envFull: its environment's share is taken.
	envFull
	// projectFull: its project's share is taken.
	projectFull
	// allFull: the cap of the whole instance is taken.
	allFull
)

// budget counts requests against a share per project, within it a share
// per environment, and a cap for the whole instance, so that one project
// cannot take what the others need, nor one of its environments what
// its others need: the held requests (MaxHeldPerEnv, MaxHeldPerProject,
// MaxHeld) and the forwarded ones (MaxInflightPerEnv,
// MaxInflightPerProject, MaxInflight).
type budget struct {
	perEnv, perProject, total int

	mu       sync.Mutex
	envs     map[string]int
	projects map[string]int
	n        int
}

func newBudget(perEnv, perProject, total int) *budget {
	return &budget{perEnv: perEnv, perProject: perProject, total: total, envs: map[string]int{}, projects: map[string]int{}}
}

// slot is one request's place in a budget; only the request's own
// goroutine uses it.
type slot struct {
	project, env string
	counted      bool
}

// enter counts a request of the environment env of project unless it is
// counted already. It returns fits, or the cap that is taken; then the
// request is not counted.
func (b *budget) enter(s *slot, project, env string) full {
	if s.counted {
		return fits
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case b.envs[env] >= b.perEnv:
		return envFull
	case b.projects[project] >= b.perProject:
		return projectFull
	case b.n >= b.total:
		return allFull
	}
	b.envs[env]++
	b.projects[project]++
	b.n++
	s.project, s.env, s.counted = project, env, true
	return fits
}

// leave ends the request's count, if it has one.
func (b *budget) leave(s *slot) {
	if !s.counted {
		return
	}
	s.counted = false
	b.mu.Lock()
	defer b.mu.Unlock()
	b.n--
	if b.envs[s.env]--; b.envs[s.env] <= 0 {
		delete(b.envs, s.env)
	}
	if b.projects[s.project]--; b.projects[s.project] <= 0 {
		delete(b.projects, s.project)
	}
}

// count is how many requests are counted.
func (b *budget) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.n
}

// of is how many requests of the environment env are counted.
func (b *budget) of(env string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.envs[env]
}

// ofProject is how many requests of the project are counted.
func (b *budget) ofProject(project string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.projects[project]
}
