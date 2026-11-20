// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import {
  deriveRows, eventDescription, eventTitle, keyOf, kindLabel, lanesOf, levelOf,
} from './events.jsx';

// The event model of the activity views. The fixtures are events as GET
// …/activity returns them: camelCase, int64 values as strings, eventId
// always set. ferund's own rows are v 2 with spans; the records of nodes
// are v 1 without.

const ns = (ms) => String(ms * 1e6);
let seq = 0;
const CATEGORY = { tool_start: 'tool', tool_end: 'tool' };
// ferund's row.
const fe = (over) => {
  seq += 1;
  const e = { eventId: `ferund:${seq}`, node: 'node-a', source: 'ferund', tsNs: ns(0), v: 2, name: '', parentSpanId: '', ...over };
  e.category = over.category ?? (CATEGORY[e.kind] || 'lifecycle');
  return e;
};
// A node's record.
const rec = (over) => {
  seq += 1;
  return { eventId: `node-a:start-1:${seq}`, node: 'node-a', source: 'feosd', tsNs: ns(0), v: 1,
    category: 'lifecycle', requestId: '', spanId: '', parentSpanId: '', detailJson: '{}', ...over };
};
const traffic = (kind, body, over = {}) => rec({
  kind, source: 'traffic', category: 'network', name: '',
  detailJson: JSON.stringify({ time: '2026-10-16T20:00:00Z', pod_id: 'p', [kind]: body }), ...over,
});

const html = (node) => renderToStaticMarkup(<>{node}</>);

describe('kindLabel and eventTitle', () => {
  it('the producer’s name wins, verbatim', () => {
    expect(eventTitle(fe({ kind: 'run_end', name: 'ended: deleted' }))).toBe('ended: deleted');
    expect(eventTitle(rec({ kind: 'pod_state', name: 'pod RUNNING' }))).toBe('pod RUNNING');
  });

  it('the new kinds have labels', () => {
    for (const [kind, label] of [
      ['pod_state', 'pod state changed'], ['node_start', 'feosd started'],
      ['pod_credentials', 'pod certificate issued'], ['overbooked', 'node overbooked'],
      ['tombstone', 'pod tombstoned'], ['gap', 'gap in stream'],
    ]) {
      expect(kindLabel(rec({ kind })), kind).toBe(label);
      expect(eventTitle(rec({ kind })), kind).toBe(label);
    }
  });

  it('a kind nobody mapped is shown as its token', () => {
    expect(kindLabel(fe({ kind: 'run_start' }))).toBe('run started');
    expect(kindLabel(fe({ kind: 's3_upload' }))).toBe('s3_upload');
  });

  it('a traffic record is labelled from its body', () => {
    expect(eventTitle(traffic('flow', { ended: true }))).toBe('flow ended');
    expect(eventTitle(traffic('flow', {}))).toBe('flow began');
    expect(eventTitle(traffic('dns', { name: 'example.com' }))).toBe('DNS query');
    expect(eventTitle(traffic('drop', {}))).toBe('packet dropped');
    expect(eventTitle(traffic('server_name', { source: 'SERVER_NAME_SOURCE_TLS' }))).toBe('server name (TLS)');
  });
});

describe('keyOf', () => {
  it('is ferund’s eventId', () => {
    expect(keyOf(rec({ eventId: 'node-a:s:3' }))).toBe('node-a:s:3');
  });
});

describe('levelOf', () => {
  it('is error only when the event reports a failure', () => {
    expect(levelOf(fe({ kind: 'tool_end', exitCode: '0' }))).toBe('info');
    expect(levelOf(fe({ kind: 'tool_end', exitCode: '2' }))).toBe('error');
    expect(levelOf(fe({ kind: 'lifecycle_end', detailJson: '{"error":"node gone"}' }))).toBe('error');
    expect(levelOf(rec({ kind: 'pod_state', detailJson: '{"state":"POD_STATE_FAILED"}' }))).toBe('error');
    expect(levelOf(rec({ kind: 'pod_state', detailJson: '{"state":"POD_STATE_RUNNING"}' }))).toBe('info');
  });

  it('is warn for a loss or a refusal', () => {
    expect(levelOf(rec({ kind: 'gap' }))).toBe('warn');
    expect(levelOf(fe({ kind: 'overbooked' }))).toBe('warn');
    expect(levelOf(rec({ kind: 'kube_deny', source: 'authn' }))).toBe('warn');
    expect(levelOf(traffic('drop', { reason: 'DROP_REASON_NO_RULE' }))).toBe('warn');
    expect(levelOf(traffic('flow', {}))).toBe('info');
  });
});

describe('eventDescription', () => {
  it('renders a gap with its range', () => {
    const out = html(eventDescription(rec({ kind: 'gap', name: 'gap in stream: 3 events lost',
      detailJson: '{"from":4,"to":6,"startId":"0d5b8a2e-0000-4000-8000-000000000001"}' })));
    expect(out).toContain('records 4–6 of start 0d5b8a2e never arrived');
  });

  it('renders a tool call’s end with exit code and duration', () => {
    const out = html(eventDescription(fe({ kind: 'tool_end', tool: 'bash', exitCode: '3', durationNs: ns(1500) })));
    expect(out).toContain('bash');
    expect(out).toContain('exit 3');
    expect(out).toContain('1.5 s');
  });

  it('renders a traffic record from its body', () => {
    const out = html(eventDescription(traffic('flow', {
      tuple: { protocol: 6, source_address: 'fd00::2', source_port: 40000, destination_address: '2606:4700::1', destination_port: 443 },
      outbound: true, ended: true, bytes_sent: '2048', bytes_received: '512',
    })));
    expect(out).toContain('TCP [fd00::2]:40000 → [2606:4700::1]:443 (out) ↑2.0 KiB ↓512 B');
  });

  it('renders a pod state with its pod and reason', () => {
    const out = html(eventDescription(rec({ kind: 'pod_state', podId: '7f3c1d2e-aaaa-4bbb-8ccc-000000000001',
      detailJson: '{"state":"POD_STATE_FAILED","reason":"guest crashed"}' })));
    expect(out).toContain('pod 7f3c1d2e');
    expect(out).toContain('guest crashed');
  });
});

describe('lanesOf', () => {
  it('orders the known lanes first and gives an unknown category its own', () => {
    const lanes = lanesOf([rec({ category: 'zz' }), traffic('dns', {}), fe({ kind: 'tool_end' }), fe({ kind: 'run_start' }),
      rec({ kind: 'kube_request', category: 'credential' })]);
    expect(lanes.map((l) => l.key)).toEqual(['lifecycle', 'tool', 'network', 'credential', 'zz']);
  });
});

// ---------------------------------------------------------------------
// The span tree: the envelope decides everything. The fixtures use kinds
// the UI has no table for, and nothing but spanId, parentSpanId,
// durationNs and name decides the shape.
// ---------------------------------------------------------------------

// A run containing a create op containing steps, the create opener
// stamped before the run it opens: the union extent contains it anyway.
function runScene() {
  return [
    fe({ kind: 'lifecycle_start', spanId: 'op-c', parentSpanId: 'run-1', requestId: 'op-c',
      name: 'create on node-a', tsNs: ns(90) }),
    fe({ kind: 'run_start', spanId: 'run-1', requestId: 'op-c', name: 'run on node-a', tsNs: ns(100) }),
    fe({ kind: 'step', spanId: 'st-op-c/net', parentSpanId: 'op-c', requestId: 'op-c',
      name: 'network created', tsNs: ns(120), durationNs: ns(20) }),
    fe({ kind: 'step', spanId: 'st-op-c/boot', parentSpanId: 'op-c', requestId: 'op-c',
      name: 'microVM booted', tsNs: ns(160), durationNs: ns(30) }),
    fe({ kind: 'lifecycle_end', spanId: 'op-c', parentSpanId: 'run-1', requestId: 'op-c',
      name: 'create on node-a: running', tsNs: ns(200), durationNs: ns(110) }),
    fe({ kind: 'run_end', spanId: 'run-1', name: 'ended: idle pause', tsNs: ns(500), durationNs: ns(400) }),
  ];
}

describe('deriveRows: the span tree', () => {
  it('run > op > steps: one root row, the subtree nested by depth', () => {
    const rows = deriveRows(runScene());
    expect(rows).toHaveLength(1);
    const [run] = rows;
    expect(run.id).toBe('span:run-1');
    expect(run.lane).toBe('lifecycle');
    expect(run.title).toBe('run on node-a');
    expect(run.bar).toBe(true);
    expect(run.state).toBe('closed');
    // The extent is the own members' and the children's: the op's
    // opener at 90 precedes run_start at 100.
    expect(run.startMs).toBe(90);
    expect(run.endMs).toBe(500);
    expect(run.ownStartMs).toBe(100);
    expect(run.ownEndMs).toBe(500);

    expect(run.subs.map((s) => [s.title, s.depth])).toEqual([
      ['create on node-a', 1],
      ['network created', 2],
      ['microVM booted', 2],
    ]);
    const op = run.subs[0];
    expect(op.startMs).toBe(90);
    expect(op.endMs).toBe(200);
    expect(op.state).toBe('closed');
    expect(op.parentSpanId).toBe('run-1');
    expect(run.subs[1].startMs).toBe(100); // ts − duration
    expect(run.subs[1].endMs).toBe(120);
  });

  it('requestId groups nothing', () => {
    const rows = deriveRows(runScene());
    expect(rows.some((r) => r.id.startsWith('op:'))).toBe(false);
    expect(rows[0].members.map((m) => m.kind)).toEqual(['run_start', 'run_end']);
  });

  it('a tool call under a run keeps its own lane; the run’s extent covers it', () => {
    const rows = deriveRows([
      fe({ kind: 'run_start', spanId: 'run-2', name: 'run on node-a', tsNs: ns(0) }),
      fe({ kind: 'tool_start', spanId: 'call-1', parentSpanId: 'run-2', tool: 'bash', name: 'bash: make', tsNs: ns(10) }),
      fe({ kind: 'tool_end', spanId: 'call-1', parentSpanId: 'run-2', tool: 'bash', name: 'bash exited 0',
        tsNs: ns(90), durationNs: ns(80), exitCode: '0' }),
    ]);
    const run = rows.find((r) => r.id === 'span:run-2');
    const call = rows.find((r) => r.id === 'span:call-1');
    expect(rows).toHaveLength(2);
    expect(call.lane).toBe('tool');
    expect(call.parentSpanId).toBe('run-2');
    expect(run.subs).toHaveLength(0);
    expect(run.startMs).toBeLessThanOrEqual(call.startMs);
    expect(run.endMs).toBeGreaterThanOrEqual(call.endMs);
    expect(run.open).toBe(true); // no closer of its own: an open bar
  });

  it('a lone opener stays a hollow point', () => {
    const [row] = deriveRows([fe({ kind: 'run_start', spanId: 'run-9', name: 'run on node-b', tsNs: ns(10) })]);
    expect(row.bar).toBe(false);
    expect(row.open).toBe(true);
    expect(row.endMs).toBeNull();
    expect(row.startMs).toBe(10);
  });

  it('closer missing but children arrived: an open bar with a real extent', () => {
    const [row] = deriveRows([
      fe({ kind: 'lifecycle_start', spanId: 'op-x', name: 'resume on node-a', tsNs: ns(0) }),
      fe({ kind: 'step', spanId: 'st-x', parentSpanId: 'op-x', name: 'restored', tsNs: ns(50), durationNs: ns(20) }),
    ]);
    expect(row.bar).toBe(true);
    expect(row.open).toBe(true);
    expect(row.startMs).toBe(0);
    expect(row.endMs).toBe(50);
  });

  it('a parent with no rows here leaves the child a root; the pointer is kept', () => {
    const rows = deriveRows([
      fe({ kind: 'tool_start', spanId: 'call-t', parentSpanId: 'run-aged-out', tool: 'bash', name: 'bash: ls', tsNs: ns(0) }),
      fe({ kind: 'tool_end', spanId: 'call-t', parentSpanId: 'run-aged-out', tool: 'bash', name: 'bash exited 0',
        tsNs: ns(20), durationNs: ns(20), exitCode: '0' }),
    ]);
    expect(rows).toHaveLength(1);
    expect(rows[0].parentSpanId).toBe('run-aged-out');
    expect([rows[0].startMs, rows[0].endMs]).toEqual([0, 20]);
  });

  it('a marker inside a span nests in its lane; one of another lane stays a root point', () => {
    const rows = deriveRows([
      fe({ kind: 'run_start', spanId: 'run-3', name: 'run on node-a', tsNs: ns(100) }),
      fe({ kind: 'sandbox_state', parentSpanId: 'run-3', name: 'running → paused', tsNs: ns(150) }),
      fe({ kind: 'pod_credentials', parentSpanId: 'run-3', category: 'credential', name: 'credentials issued', tsNs: ns(160) }),
      fe({ kind: 'run_end', spanId: 'run-3', name: 'ended: paused', tsNs: ns(200), durationNs: ns(100) }),
    ]);
    const run = rows.find((r) => r.id === 'span:run-3');
    expect(run.subs.map((s) => [s.kind, s.depth, s.bar])).toEqual([['sandbox_state', 1, false]]);
    const cred = rows.find((r) => r.kind === 'pod_credentials');
    expect(cred.lane).toBe('credential');
    expect(cred.bar).toBe(false);
  });

  it('the records of nodes, gaps and traffic are points', () => {
    const rows = deriveRows([
      rec({ kind: 'pod_state', name: 'pod RUNNING', tsNs: ns(10) }),
      rec({ kind: 'gap', name: 'gap in stream: 2 events lost', tsNs: ns(20) }),
      traffic('dns', { name: 'example.com', addresses: ['2606:4700::1'] }, { tsNs: ns(30) }),
      rec({ kind: 'node_start', name: 'feosd started (start s)', tsNs: ns(40) }),
    ]);
    expect(rows).toHaveLength(4);
    expect(rows.every((r) => !r.bar && !r.open)).toBe(true);
    expect(rows.map((r) => r.lane)).toEqual(['lifecycle', 'lifecycle', 'network', 'lifecycle']);
    expect(rows[1].title).toBe('gap in stream: 2 events lost');
  });

  it('a cycle of parent pointers does not hang', () => {
    const rows = deriveRows([
      fe({ kind: 'a', spanId: 'x', parentSpanId: 'y', tsNs: ns(0) }),
      fe({ kind: 'b', spanId: 'y', parentSpanId: 'x', tsNs: ns(5), durationNs: ns(5) }),
    ]);
    expect(rows.length).toBeGreaterThan(0);
  });

  it('a failed member colours its span', () => {
    const [row] = deriveRows([
      fe({ kind: 'lifecycle_start', spanId: 'op-f', name: 'start on node-a', tsNs: ns(0) }),
      fe({ kind: 'lifecycle_end', spanId: 'op-f', name: 'start on node-a: failed', tsNs: ns(9), durationNs: ns(9),
        detailJson: '{"error":"no capacity"}' }),
    ]);
    expect(row.err).toBe(true);
    expect(row.end.name).toBe('start on node-a: failed');
  });
});
