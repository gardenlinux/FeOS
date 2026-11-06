// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import {
  canDrain, certState, createHints, fleetDefaults, fmtBytes, fmtBytesOf, fmtSeconds, joinTokenState, memBar,
  nodeRows, settingsForm, settingsPatch, stopping, tenantDeleteText, tokenRows, vcpuCapacityText,
} from './fleet.js';

// The node view's rows, from GET /api/v1/admin/nodes as ferund renders
// it: int64 values as strings, times RFC 3339 or nanoseconds.

const READY = {
  node: 'node-a', instanceId: '0d5b8a2e-0000-4000-8000-000000000001', status: 'ready', statusDescription: '',
  draining: false, hostname: 'host-a', arch: 'x86_64', cpuModelname: 'EPYC', cpuNumcores: 16,
  memoryTotalbytes: '66000000000', diskTotalbytes: '1099511627776', slots: 8, memHeadroomBytes: '1073741824',
  vcpusCapacity: 64, memoryCapacityBytes: '64926258176', cpuOvercommit: 4, memAvailBytes: '13500000000',
  diskAvailBytes: '214748364800', load1: 0.12, sandboxesRunning: 3, sandboxesPaused: 1,
  inventoryHash: '47DEQpj8HBSa', registeredAtNs: '1760000000000000000', lastSeenAtNs: '1760000005000000000',
  downSinceNs: '0', sandboxesBooked: 3, vcpusBooked: 6, memoryBookedBytes: String(1536 * 2 ** 20),
  startId: '0d5b8a2e-0000-4000-8000-000000000001', versions: { feosd: '0.3.0' }, objectStore: true,
  objectStoreId: 's3://bucket', podPrefix: 'fd00:1::/64', connected: true, connections: 2,
  certSerial: 'd76051b3b729497ad882eb42c04ca255', certNotAfter: '2026-10-23T20:08:44Z', stoppingUntil: '',
  drainStartedAt: '', identityForked: false, removedAt: '',
};

describe('nodeRows', () => {
  it('derives the telemetry, the booking and the new fields', () => {
    const [r] = nodeRows([READY]);
    expect(r.key).toBe('node-a');
    expect(r.cls).toBe('');
    expect(r.memPct).toBe(80);
    expect(r.load1).toBeCloseTo(0.12);
    expect([r.running, r.paused, r.slots, r.booked]).toEqual([3, 1, 8, 3]);
    expect([r.vcpusBooked, r.vcpusCapacity, r.cpuOvercommit]).toEqual([6, 64, 4]);
    expect(fmtBytes(r.memBooked)).toBe('1.5 GiB');
    expect(r.memHeadroom).toBe(2 ** 30);
    expect([r.connected, r.connections]).toEqual([true, 2]);
    expect(r.certSerial).toBe('d76051b3b729497ad882eb42c04ca255');
    expect(r.certNotAfter).toBe('2026-10-23T20:08:44Z');
    expect([r.objectStore, r.objectStoreId]).toEqual([true, 's3://bucket']);
    expect(r.identityForked).toBe(false);
  });

  it('marks down and removed rows, and puts removed ones last', () => {
    const rows = nodeRows([
      { node: 'gone', status: 'removed', removedAt: '2026-10-16T10:00:00Z' },
      { node: 'b', status: 'down', statusDescription: 'no telemetry', memoryTotalbytes: '100', memAvailBytes: '0' },
      { node: 'c', status: 'joined' },
    ]);
    expect(rows.map((r) => [r.node, r.cls])).toEqual([['b', 'down'], ['c', ''], ['gone', 'removed']]);
    expect(rows[0].memPct).toBe(100);
    expect(rows[0].description).toBe('no telemetry');
    expect(rows[2].removedAt).toBe('2026-10-16T10:00:00Z');
  });

  it('keeps a node’s flags', () => {
    const [d] = nodeRows([{ node: 'x', status: 'ready', draining: true, identityForked: true,
      stoppingUntil: '2026-10-16T10:00:30Z', connected: false }]);
    expect([d.draining, d.identityForked, d.connected, d.connections]).toEqual([true, true, false, 0]);
    expect(d.stoppingUntil).toBe('2026-10-16T10:00:30Z');
  });

  it('renders the overcommit as entered, not as its float32 widened', () => {
    const [r] = nodeRows([{ ...READY, cpuOvercommit: 1.100000023841858 }]);
    expect(r.cpuOvercommit).toBe(1.1);
    expect(settingsForm(r).cpuOvercommit).toBe('1.1');
    expect(nodeRows([{ ...READY, cpuOvercommit: 1.2345678806304932 }])[0].cpuOvercommit).toBe(1.23457);
  });

  it('a node without telemetry yields zeros, never NaN', () => {
    const [n] = nodeRows([{ node: 'new', status: 'joined' }]);
    for (const k of ['memPct', 'load1', 'diskAvail', 'running', 'paused', 'vcpusCapacity', 'memCapacity', 'lastSeenNs']) {
      expect(Number.isNaN(n[k]), k).toBe(false);
    }
  });
});

describe('the memory bar', () => {
  it('labels the memory in use of the total, as it fills', () => {
    const [r] = nodeRows([{ ...READY, memoryTotalbytes: String(64 * 2 ** 30), memAvailBytes: String(40.5 * 2 ** 30) }]);
    expect(r.memUsed).toBe(23.5 * 2 ** 30);
    expect(r.memPct).toBe(37);
    expect(memBar(r)).toEqual({ label: '23.5 / 64.0 GiB', title: '23.5 GiB in use of 64.0 GiB, 40.5 GiB available' });
  });

  it('a node without telemetry shows nothing in use, never a negative', () => {
    const [n] = nodeRows([{ node: 'new', status: 'joined' }]);
    expect([n.memUsed, n.memPct]).toEqual([0, 0]);
    expect(memBar(n).label).toBe('0 / 0 B');
    const [odd] = nodeRows([{ node: 'odd', memoryTotalbytes: '100', memAvailBytes: '200' }]);
    expect([odd.memUsed, odd.memPct]).toEqual([0, 0]);
  });

  it('names the unit once when both share it', () => {
    expect(fmtBytesOf(12 * 2 ** 30, 63 * 2 ** 30)).toBe('12.0 / 63.0 GiB');
    expect(fmtBytesOf(512 * 2 ** 20, 63 * 2 ** 30)).toBe('512 MiB / 63.0 GiB');
    expect(fmtBytesOf(2 ** 40, 2 * 2 ** 40)).toBe('1.0 / 2.0 TiB');
  });
});

describe('the capacity', () => {
  it('vCPU capacity 0 means not gated', () => {
    expect(vcpuCapacityText(0)).toBe('∞');
    expect(vcpuCapacityText(64)).toBe('64');
  });

  it('drain is offered for joined and ready nodes only', () => {
    expect(nodeRows([{ node: 'a', status: 'ready' }, { node: 'b', status: 'joined' }, { node: 'c', status: 'down' },
      { node: 'd', status: 'removed' }]).map(canDrain)).toEqual([true, true, false, false]);
  });
});

describe('settingsPatch', () => {
  const [row] = nodeRows([READY]);
  const base = settingsForm(row);
  it('fills the form from the row, the headroom in MiB', () => {
    expect(base).toEqual({ slots: '8', memHeadroomMiB: '1024', cpuOvercommit: '4' });
  });

  it('sends only what changed, headroom in bytes as an int64 string', () => {
    expect(settingsPatch(base, base)).toEqual({});
    expect(settingsPatch(base, { slots: '10', memHeadroomMiB: '2048', cpuOvercommit: '0' }))
      .toEqual({ slots: 10, memHeadroomBytes: String(2 * 2 ** 30), cpuOvercommit: 0 });
    expect(settingsPatch(base, { slots: '', memHeadroomMiB: '', cpuOvercommit: '1.5' })).toEqual({ cpuOvercommit: 1.5 });
    expect(settingsPatch(base, { slots: '08', memHeadroomMiB: ' 1024 ', cpuOvercommit: '4.0' })).toEqual({});
  });

  it('leaves a headroom that is not whole MiB alone when only the slots change', () => {
    const [odd] = nodeRows([{ ...READY, memHeadroomBytes: '1500000000' }]);
    const f = settingsForm(odd);
    expect(f.memHeadroomMiB).toBe('1431');
    expect(settingsPatch(f, { ...f, slots: '9' })).toEqual({ slots: 9 });
    expect(settingsPatch(f, { ...f, memHeadroomMiB: '1432' })).toEqual({ memHeadroomBytes: String(1432 * 2 ** 20) });
  });

  it('leaves an untouched overcommit alone after a float32 round trip', () => {
    const [r] = nodeRows([{ ...READY, cpuOvercommit: 1.100000023841858 }]);
    const f = settingsForm(r);
    expect(settingsPatch(f, { ...f, slots: '9' })).toEqual({ slots: 9 });
  });

  it('refuses negative and malformed values', () => {
    expect(() => settingsPatch(base, { slots: '-1', memHeadroomMiB: '', cpuOvercommit: '' })).toThrow(/slots/);
    expect(() => settingsPatch(base, { slots: '', memHeadroomMiB: '1.5', cpuOvercommit: '' })).toThrow(/headroom/);
    expect(() => settingsPatch(base, { slots: '', memHeadroomMiB: '', cpuOvercommit: 'x' })).toThrow(/overcommit/);
  });
});

describe('stopping', () => {
  const now = Date.parse('2026-10-16T12:00:00Z');
  it('holds only while the announced restart lies ahead', () => {
    expect(stopping({ stoppingUntil: '2026-10-16T12:00:30Z' }, now)).toBe(true);
    expect(stopping({ stoppingUntil: '2026-10-16T11:59:30Z' }, now)).toBe(false);
    expect(stopping({ stoppingUntil: '2026-10-16T12:00:00Z' }, now)).toBe(false);
    expect(stopping({ stoppingUntil: '' }, now)).toBe(false);
    expect(stopping(nodeRows([{ node: 'x', status: 'down', stoppingUntil: '2026-10-16T11:00:00Z' }])[0], now)).toBe(false);
  });
});

describe('certState and joinTokenState', () => {
  const now = Date.parse('2026-10-16T12:00:00Z');
  it('reads a certificate', () => {
    expect(certState({ notAfter: '2026-10-20T12:00:00Z' }, now)).toBe('valid');
    expect(certState({ notAfter: '2026-10-16T20:00:00Z' }, now)).toBe('expiring');
    expect(certState({ notAfter: '2026-10-16T11:00:00Z' }, now)).toBe('expired');
    expect(certState({ notAfter: '2026-10-20T12:00:00Z', revokedAt: '2026-10-16T11:00:00Z' }, now)).toBe('revoked');
  });

  it('reads a join token', () => {
    expect(joinTokenState({ expiresAt: '2026-10-16T13:00:00Z', usedAt: '' }, now)).toBe('open');
    expect(joinTokenState({ expiresAt: '2026-10-16T11:00:00Z', usedAt: '' }, now)).toBe('expired');
    expect(joinTokenState({ expiresAt: '2026-10-16T11:00:00Z', usedAt: '2026-10-16T10:00:00Z' }, now)).toBe('used');
  });
});

describe('tokenRows', () => {
  it('sorts newest first', () => {
    const rows = tokenRows([
      { hash: 'aa', hashPrefix: 'aa', createdAt: '2026-10-16T10:00:00Z' },
      { hash: 'bb', hashPrefix: 'bb', createdAt: '2026-10-16T11:00:00Z' },
    ]);
    expect(rows.map((r) => r.hash)).toEqual(['bb', 'aa']);
    expect(tokenRows(null)).toEqual([]);
  });
});

describe('tenantDeleteText', () => {
  it('warns that the environments and their data go too', () => {
    expect(tenantDeleteText({ sandboxCount: 0, environmentCount: 3 }).confirm)
      .toBe('delete with 3 environments and their data?');
    expect(tenantDeleteText({ sandboxCount: 2, environmentCount: 1 }).confirm)
      .toBe('delete with 2 sandboxes and 1 environment and their data?');
    expect(tenantDeleteText({ sandboxCount: 1, environmentCount: 0 }).confirm).toBe('delete with 1 sandbox?');
    expect(tenantDeleteText({ sandboxCount: 0 }).confirm).toBe('delete?');
    expect(tenantDeleteText({}).title).toContain('every environment of it, with the environments\' addon data');
  });
});

describe('sizes', () => {
  it('formats host sizes', () => {
    expect(fmtBytes(66000000000)).toBe('61.5 GiB');
    expect(fmtBytes('214748364800')).toBe('200.0 GiB');
    expect(fmtBytes(3 * 2 ** 40)).toBe('3.0 TiB');
    expect(fmtBytes(0)).toBe('0 B');
  });
});

describe('fmtSeconds', () => {
  it('renders whole minutes and hours as such, anything else in seconds', () => {
    expect([5, 1.5, 15, 59, 60, 90, 420, 600, 3600, 5400, 7200].map(fmtSeconds)).toEqual([
      '5 s', '1.5 s', '15 s', '59 s', '1 min', '90 s', '7 min', '10 min', '1 h', '1 h 30 min', '2 h',
    ]);
  });

  it('is empty when unset', () => {
    expect([0, -1, undefined, null, 'x'].map(fmtSeconds)).toEqual(['', '', '', '', '']);
  });
});

describe('fleetDefaults and createHints', () => {
  it('read the defaults of GET /api/v1/admin/nodes', () => {
    const fd = fleetDefaults({
      telemetryIntervalSeconds: 5, nodeTtlSeconds: 15, lostGraceSeconds: 600, drainTimeoutSeconds: 3600,
      defaultVcpus: 2, defaultMemoryMib: 512, defaultImage: 'img', minVcpus: 1, maxVcpus: 32,
      minMemoryMib: 256, maxMemoryMib: 65536,
    });
    expect(fd).toEqual({
      telemetryInterval: '5 s', nodeTtl: '15 s', lostGrace: '10 min', drainTimeout: '1 h', vcpus: 2,
      memoryMib: 512, image: 'img', minVcpus: 1, maxVcpus: 32, minMemoryMib: 256, maxMemoryMib: 65536,
    });
    const h = createHints(fd);
    expect(h.image).toBe('image (empty: img)');
    expect(h.vcpus).toEqual({ placeholder: 'vCPUs (2)', title: 'vCPUs, 1–32 (empty: 2)', min: 1, max: 32 });
    expect(h.memory).toEqual({
      placeholder: 'MiB (512)', title: 'memory in MiB, 256–65536 (empty: 512)', min: 256, max: 65536,
    });
  });

  it('without an answer: nothing invented', () => {
    const fd = fleetDefaults(undefined);
    expect([fd.nodeTtl, fd.image, fd.vcpus, fd.maxVcpus]).toEqual(['', '', 0, 0]);
    const h = createHints(fd);
    expect(h.vcpus).toEqual({ placeholder: 'vCPUs', title: 'vCPUs (empty: the default)', min: undefined, max: undefined });
    expect(h.image).toBe('image (empty: the default)');
  });
});
