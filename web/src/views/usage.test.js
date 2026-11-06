// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import { QUOTA_ITEMS } from '../people.js';
import { AT_LIMIT, COUNTS, booked } from './usage.js';

// Where a tenant's booked vCPUs and memory are: its sandboxes by state,
// its jobs on their way, and the environments' rest of the quota's use.

const QUOTA = {
  limits: { vcpus: 32, memoryMib: 65536 },
  use: { sandboxes: 4, environments: 2, runningJobs: 1, vcpus: 14, memoryMib: 20480 },
};
const SANDBOXES = [
  { sandboxId: 'a', clientState: 'running', vcpus: 2, memoryMib: 4096 },
  { sandboxId: 'b', clientState: 'paused', vcpus: 2, memoryMib: 4096 },
  { sandboxId: 'c', clientState: 'hibernated', vcpus: 1, memoryMib: 2048 },
  { sandboxId: 'd', clientState: 'failed', vcpus: 2, memoryMib: 4096 },
];
const JOBS = [
  { id: 'j1', phase: 'running', vcpus: 1, memoryMib: 512 },
  { id: 'j2', phase: 'succeeded', vcpus: 4, memoryMib: 8192 },
];

describe('booked', () => {
  it('splits the use by sandbox state, jobs and the environments', () => {
    const parts = booked(QUOTA, SANDBOXES, JOBS);
    expect(parts.map((p) => [p.key, p.count, p.vcpus, p.memoryMib])).toEqual([
      ['awake', 1, 2, 4096],
      ['asleep', 1, 2, 4096],
      ['hibernated', 1, 1, 2048],
      ['other', 1, 2, 4096],
      ['jobs', 1, 1, 512],
      ['environments', null, 6, 5632],
    ]);
  });

  it('never makes the rest negative, and needs the quota and the sandboxes', () => {
    const parts = booked({ use: { vcpus: 1, memoryMib: 0 } }, SANDBOXES, null);
    expect(parts.at(-1)).toMatchObject({ key: 'environments', vcpus: 0, memoryMib: 0, label: 'environments, jobs and the rest' });
    expect(booked(null, SANDBOXES, JOBS)).toBe(null);
    expect(booked(QUOTA, null, JOBS)).toBe(null);
  });

  it('says for every limit what counts and what happens at it', () => {
    for (const it of QUOTA_ITEMS) {
      expect(COUNTS[it.key]).toBeTruthy();
      expect(AT_LIMIT[it.key]).toBeTruthy();
    }
  });
});
