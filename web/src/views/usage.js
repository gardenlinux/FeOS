// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// The tenant's use against its limits, in words (ADR 0031, "Quotas per
// tenant"; control-plane.md, Quotas; go/internal/store/quotas.go,
// TenantUse): what counts towards each limit, and where the booked vCPUs
// and memory are. Pure, for the tests.

// COUNTS say what counts towards each limit, by QUOTA_ITEMS' key.
export const COUNTS = {
  sandboxes: 'The tenant\'s own sandboxes in every state, awake, asleep or hibernated. Not an environment\'s instances, not a job\'s pod.',
  environments: 'Environments not being deleted.',
  runningJobs: 'The tenant\'s jobs that have not ended: queued or running. Environments\' own jobs do not count.',
  vcpus: 'Booked by every sandbox in every state, the replicas of the environments\' image services by their current deployments, their addons\' plans, and the jobs that have not ended. An attached service runs in a sandbox, which counts already; environments\' own jobs do not count.',
  memoryMib: 'Booked by the same as the vCPUs: sandboxes in every state, image services\' replicas, addons, jobs that have not ended.',
};

// AT_LIMIT says what happens at a limit, by its key.
export const AT_LIMIT = {
  sandboxes: 'A new sandbox is refused (422 quota_exceeded).',
  environments: 'A new environment or a clone is refused (422 quota_exceeded).',
  runningJobs: 'A new job is refused (422 quota_exceeded) until one ends.',
  vcpus: 'A new sandbox or job is refused (422 quota_exceeded); a deployment that asks for more has an error finding. One that asks for no more still deploys.',
  memoryMib: 'As with vCPUs: new sandboxes and jobs are refused, deployments that ask for more have an error finding.',
};

// SANDBOX_STATES group a sandbox's clientState for the breakdown.
const SANDBOX_STATES = [
  { key: 'awake', label: 'awake or waking sandboxes', states: ['running', 'resuming', 'creating', 'pending'] },
  { key: 'asleep', label: 'asleep on their node', states: ['paused', 'pausing'] },
  { key: 'hibernated', label: 'hibernated, in the object store', states: ['hibernated', 'hibernating'] },
];

// booked splits the quota's vCPU and memory use into the sandboxes by
// state (the summary's list), the jobs that have not ended, and the
// rest: the environments' services and addons, which no list here
// carries. Each part {key, label, count, vcpus, memoryMib}; null without
// the quota or the sandboxes. The lists and the quota are read apart, so
// the rest is never below 0.
export function booked(quota, sandboxes, jobs) {
  if (!quota || !quota.use || !sandboxes) return null;
  const parts = SANDBOX_STATES.map((g) => ({ key: g.key, label: g.label, count: 0, vcpus: 0, memoryMib: 0 }));
  const other = { key: 'other', label: 'sandboxes in another state', count: 0, vcpus: 0, memoryMib: 0 };
  for (const s of sandboxes) {
    const i = SANDBOX_STATES.findIndex((g) => g.states.includes(s.clientState));
    const p = i < 0 ? other : parts[i];
    p.count++;
    p.vcpus += Number(s.vcpus) || 0;
    p.memoryMib += Number(s.memoryMib) || 0;
  }
  if (other.count) parts.push(other);
  if (jobs) {
    const j = { key: 'jobs', label: 'jobs queued or running', count: 0, vcpus: 0, memoryMib: 0 };
    for (const x of jobs) {
      if (x.phase !== 'queued' && x.phase !== 'running') continue;
      j.count++;
      j.vcpus += Number(x.vcpus) || 0;
      j.memoryMib += Number(x.memoryMib) || 0;
    }
    parts.push(j);
  }
  const sum = (k) => parts.reduce((a, p) => a + p[k], 0);
  const use = quota.use;
  parts.push({
    key: 'environments', label: jobs ? 'environments\' services and addons' : 'environments, jobs and the rest',
    count: null,
    vcpus: Math.max(0, (Number(use.vcpus) || 0) - sum('vcpus')),
    memoryMib: Math.max(0, (Number(use.memoryMib) || 0) - sum('memoryMib')),
  });
  return parts;
}
