// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import { deriveRows } from '../events.jsx';
import { packLane, tipPlace } from './Timeline.jsx';

// The render reads packLane for both the bars and their nested spans, so
// its packing is what lands on screen: no two entries share a sub-row
// while they overlap in time, and nested spans never draw inside their
// parent's bar.

const T = Date.parse('2026-10-16T11:36:00Z');
let n = 1;
const ev = (o) => ({
  sandboxId: 's1', eventId: `ferund:${n++}`, tsNs: String((BigInt(T) + BigInt(o.t || 0)) * 1000000n),
  kind: o.kind, category: o.category || 'lifecycle', source: 'ferund', v: 2,
  requestId: '', spanId: o.span || '', parentSpanId: o.parent || '', name: o.name || '', tool: o.tool || '',
  detailJson: '{}', ...(o.dur != null ? { durationNs: String(o.dur) } : {}),
});

function entries(lane) {
  return [
    ...lane.map((r) => ({ id: r.id, s: r.startMs, e: r.bar ? r.endMs : r.startMs + 1 })),
    ...lane.flatMap((r) => (r.subs || []).map((s) => ({ id: `${r.id}|${s.id}`, s: s.startMs, e: s.endMs ?? s.startMs + 1 }))),
  ];
}

function expectNoOverlap(pk, list) {
  for (const en of list) expect(pk.assign.get(en.id)).toBeTypeOf('number');
  for (let i = 0; i < list.length; i++) {
    for (let j = i + 1; j < list.length; j++) {
      const a = list[i];
      const b = list[j];
      if (pk.assign.get(a.id) === pk.assign.get(b.id)) {
        expect(a.e <= b.s || b.e <= a.s, `same sub-row overlap: ${a.id} vs ${b.id}`).toBe(true);
      }
    }
  }
}

// A run with an op of steps, a step inside a step, and a tool call of
// its own category.
function treeScene() {
  return [
    ev({ kind: 'run_start', span: 'run-1', name: 'run on node-a', t: 0 }),
    ev({ kind: 'lifecycle_start', span: 'op-c', parent: 'run-1', name: 'create on node-a', t: 0 }),
    ev({ kind: 'step', span: 'st-net', parent: 'op-c', name: 'network created', t: 120, dur: 20e6 }),
    ev({ kind: 'step', span: 'st-pull', parent: 'op-c', name: 'image pulled', t: 130 }),
    ev({ kind: 'step', span: 'st-bake', parent: 'st-pull', name: 'layer baked', t: 170, dur: 20e6 }),
    ev({ kind: 'step', span: 'st-pull', parent: 'op-c', name: 'image pulled', t: 180, dur: 50e6 }),
    ev({ kind: 'lifecycle_end', span: 'op-c', parent: 'run-1', name: 'create on node-a: running', t: 200, dur: 200e6 }),
    ev({ kind: 'tool_start', span: 'call-1', parent: 'run-1', category: 'tool', tool: 'bash', name: 'bash: ls -la', t: 220 }),
    ev({ kind: 'tool_end', span: 'call-1', parent: 'run-1', category: 'tool', tool: 'bash', name: 'bash exited 0', t: 260, dur: 40e6 }),
    ev({ kind: 'run_end', span: 'run-1', name: 'ended: idle pause', t: 400, dur: 400e6 }),
  ];
}

describe('packLane with the span tree', () => {
  it('the run, its op and the op’s steps each get a sub-row', () => {
    const rows = deriveRows(treeScene());
    const run = rows.find((r) => r.id === 'span:run-1');
    expect(run?.bar).toBe(true);
    expect(run.subs.map((s) => [s.id, s.depth])).toEqual([
      ['span:op-c', 1],
      ['span:st-net', 2],
      ['span:st-pull', 2],
      ['span:st-bake', 3],
    ]);
    const call = rows.find((r) => r.id === 'span:call-1');
    expect(call.lane).toBe('tool');
    expect(run.subs.some((s) => s.id === 'span:call-1')).toBe(false);

    const lane = rows.filter((r) => r.lane === 'lifecycle');
    const pk = packLane(lane);
    expectNoOverlap(pk, entries(lane));
    expect(pk.assign.get('span:run-1')).not.toBe(pk.assign.get('span:run-1|span:op-c'));
  });

  it('a nested span without an extent packs as a sliver', () => {
    const rows = deriveRows([
      ev({ kind: 'run_start', span: 'run-2', name: 'run on node-b', t: 0 }),
      ev({ kind: 'lifecycle_start', span: 'op-p', parent: 'run-2', name: 'pause on node-b', t: 100 }),
      ev({ kind: 'run_end', span: 'run-2', name: 'ended: paused', t: 300, dur: 300e6 }),
    ]);
    const run = rows.find((r) => r.id === 'span:run-2');
    const op = run.subs[0];
    expect(op.bar).toBe(false);
    expect(op.endMs).toBeNull();
    expect(op.depth).toBe(1);
    const pk = packLane(rows.filter((r) => r.lane === 'lifecycle'));
    expect(pk.assign.get('span:run-2|span:op-p')).toBeTypeOf('number');
    expect(pk.assign.get('span:run-2')).not.toBe(pk.assign.get('span:run-2|span:op-p'));
  });

  it('overlapping tool calls go on different sub-rows; points at the same time stack', () => {
    const rows = deriveRows([
      ev({ kind: 'tool_start', span: 'a', category: 'tool', name: 'bash: a', t: 0 }),
      ev({ kind: 'tool_start', span: 'b', category: 'tool', name: 'bash: b', t: 10 }),
      ev({ kind: 'tool_end', span: 'a', category: 'tool', name: 'bash exited 0', t: 50, dur: 50e6 }),
      ev({ kind: 'tool_end', span: 'b', category: 'tool', name: 'bash exited 0', t: 60, dur: 50e6 }),
      ev({ kind: 'sandbox_state', name: 'pending → running', t: 70 }),
      ev({ kind: 'sandbox_state', name: 'running → paused', t: 70 }),
    ]);
    const tool = rows.filter((r) => r.lane === 'tool');
    const pk = packLane(tool);
    expect(pk.n).toBe(2);
    expectNoOverlap(pk, entries(tool));
    const life = rows.filter((r) => r.lane === 'lifecycle');
    const pl = packLane(life);
    expect(pl.n).toBe(2);
  });
});

describe('the hover card', () => {
  // A card of 300×120 in a window of 1440×900.
  it('sits right of and below the pointer while it fits', () => {
    expect(tipPlace(400, 300, 300, 120, 1440, 900)).toEqual({ left: 414, top: 316 });
  });

  it('goes left of the pointer at the right edge, above it at the bottom', () => {
    expect(tipPlace(1300, 300, 300, 120, 1440, 900)).toEqual({ left: 986, top: 316 });
    expect(tipPlace(400, 850, 300, 120, 1440, 900)).toEqual({ left: 414, top: 714 });
  });

  it('never leaves the window, even when wider than the room on either side', () => {
    expect(tipPlace(200, 300, 300, 120, 400, 900)).toEqual({ left: 8, top: 316 });
    expect(tipPlace(5, 5, 300, 120, 1440, 900).left).toBe(19);
  });
});
