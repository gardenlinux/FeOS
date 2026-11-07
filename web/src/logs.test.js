// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import {
  clockOf, dayOf, dayText, durationText, egressParts, focusOf, follows, inputOf, isEgress, isRequest, keyed, lineLevel,
  logfmtOf, mergeLines, parseSearch, queryOf, rangeOf, reachesNow, recordOf, requestParts, sinkParams, storeDown,
  streamTag, timeOfInput, toggleLevel, toggleWord, traceOf, valueText, whatOf, wordIn,
} from './logs.js';

const ENV = '01a199ab-4013-7851-a76a-3b3a22678630';
const NOW = Date.parse('2026-11-06T15:00:00Z');
const TRACE = '4bf92f3577b34da6a3ce929d0e0e4736';

describe('a search', () => {
  it('reads the text box as words, or /an expression/', () => {
    expect(parseSearch(' timeout -debug ')).toEqual({ q: 'timeout -debug', re: '' });
    expect(parseSearch('/time.?out/')).toEqual({ q: '', re: 'time.?out' });
    expect(parseSearch('//')).toEqual({ q: '//', re: '' });
  });

  it('toggles levels in a fixed order', () => {
    expect(toggleLevel('', 'warn')).toBe('warn');
    expect(toggleLevel('warn', 'error')).toBe('error,warn');
    expect(toggleLevel('error,warn', 'error')).toBe('warn');
    expect(toggleLevel('bogus,info', 'debug')).toBe('info,debug');
  });

  it('is the path and the query, and back', () => {
    const f = focusOf('service/api', 1, '?q=timeout&level=error&since=15m&source=ingress&x=y');
    expect(f).toEqual({ object: 'service/api', replica: 1, q: 'timeout', level: 'error', since: '15m', source: 'ingress' });
    expect(queryOf(f)).toBe('?source=ingress&q=timeout&level=error&since=15m');
    expect(focusOf('', 3, '')).toEqual({});
    expect(focusOf('', null, '?source=bogus&trace=nope')).toEqual({});
    expect(queryOf({})).toBe('');
  });

  it("in the environment's Logs has the object and the replica in the query", () => {
    const f = focusOf('', null, '?object=service%2Fapi&replica=1&q=timeout');
    expect(f).toEqual({ object: 'service/api', replica: 1, q: 'timeout' });
    expect(queryOf(f, true)).toBe('?object=service%2Fapi&replica=1&q=timeout');
    expect(focusOf('', null, '?object=nonsense&replica=1')).toEqual({});
    expect(focusOf('', null, '?object=job%2Fmigrate&replica=x')).toEqual({ object: 'job/migrate' });
    expect(focusOf('service/web', null, '?object=service%2Fapi')).toEqual({ object: 'service/web' });
  });

  it("is the store's query: the environment always, a span read back from now", () => {
    expect(sinkParams({}, ENV, { now: NOW, limit: 300 }).toString())
      .toBe(`environment=${ENV}&since=2026-11-06T14%3A00%3A00.000Z&limit=300`);
    const p = sinkParams({ object: 'service/api', q: '/5\\d\\d/', level: 'error,warn', since: '15m' }, ENV, { now: NOW });
    expect(Object.fromEntries(p)).toEqual({
      environment: ENV, kind: 'service', object: 'api', re: '5\\d\\d', level: 'error,warn', since: '2026-11-06T14:45:00.000Z',
    });
    // A replica is a container's.
    expect(sinkParams({ object: 'service/api', replica: 0 }, ENV, { now: NOW }).get('source')).toBe('container');
    expect(sinkParams({ object: 'service/api', replica: 0, source: 'egress' }, ENV, { now: NOW }).get('source')).toBe('egress');
    const custom = { q: 'boom', since: '2026-10-30T10:00:00.000Z', until: '2026-10-30T11:00:00.000Z' };
    expect(Object.fromEntries(sinkParams(custom, ENV, { now: NOW })))
      .toEqual({ environment: ENV, q: 'boom', since: custom.since, until: custom.until });
    expect(rangeOf(custom)).toBe('custom');
  });

  it("reads a trace's lines as far back as the store keeps them, unless a range is picked", () => {
    expect(rangeOf({ trace: TRACE })).toBe('all');
    expect(sinkParams({ trace: TRACE }, ENV, { now: NOW }).toString()).toBe(`environment=${ENV}&trace=${TRACE}`);
    expect(sinkParams({ trace: TRACE, since: '1d' }, ENV, { now: NOW }).get('since')).toBe('2026-11-05T15:00:00.000Z');
  });

  it('follows one object to now; an environment is searched, not followed', () => {
    expect(follows({ object: 'service/api', since: '7d' })).toBe(true);
    expect(follows({ object: 'service/api', source: 'ingress' })).toBe(true);
    expect(follows({ since: '7d' })).toBe(false);
    expect(reachesNow({ since: '7d' })).toBe(true);
    expect(follows({ object: 'service/api', since: '2026-10-30T10:00:00Z', until: '2026-10-30T11:00:00Z' })).toBe(false);
  });

  it('names what it reads', () => {
    expect(whatOf({})).toBe('every object');
    expect(whatOf({ object: 'service/api', replica: 1 })).toBe('service api, replica 1');
    expect(whatOf({ object: 'service/api', source: 'ingress' })).toBe('the requests of service api');
    expect(whatOf({ source: 'container', trace: TRACE })).toBe('every object’s output, trace 4bf92f35…');
  });

  it("tells a store that does not answer from one that refused", () => {
    expect(storeDown({ status: 503 })).toBe(true);
    expect(storeDown({ status: 0 })).toBe(true);
    expect(storeDown({ status: 404 })).toBe(true);
    expect(storeDown({ status: 400 })).toBe(false);
    expect(storeDown({ status: 401 })).toBe(false);
  });
});

describe('a line', () => {
  it("has the store's level, else its text's", () => {
    expect(lineLevel({ level: 'warn', text: 'ERROR' })).toBe('warn');
    expect(lineLevel({ level: '', text: 'ERROR boom' })).toBe('error');
    expect(lineLevel({ level: '', text: 'hello' })).toBe('unknown');
  });

  it('merges by stream and offset, each line once', () => {
    const a = keyed([{ stream: 's', offset: '2', text: 'b' }, { stream: 's', offset: '3', text: 'c' }]);
    const before = keyed([{ stream: 's', offset: '1', text: 'a' }, { stream: 's', offset: '2', text: 'b' }]);
    expect(mergeLines(a, before, 'before').map((l) => l.text)).toEqual(['a', 'b', 'c']);
    expect(mergeLines(a, keyed([{ stream: 's', offset: '4', text: 'd' }]), 'after').map((l) => l.text)).toEqual(['b', 'c', 'd']);
  });

  it("reads a trace ID as the store reads it", () => {
    const id = '0af7651916cd43dd8448eb211c80319c';
    const ls = keyed([
      { stream: 's', offset: '1', text: `{"msg":"x","trace_id":"${id}"}` },
      { stream: 's', offset: '2', text: `{"traceparent":"00-${id}-b7ad6b7169203331-01"}` },
      { stream: 's', offset: '3', text: `{"traceId":"${'0'.repeat(32)}"}` },
      { stream: 's', offset: '4', text: `msg=x trace_id=${id}` },
      { stream: 's', offset: '5', text: '{"broken' },
    ]);
    expect(ls.map((l) => l.trace)).toEqual([id, id, '', id, '']);
    expect(traceOf(null)).toBe('');
  });

  it('reads as its message and fields, without what its head says', () => {
    const text = '{"time":"2026-11-06T15:15:54.558Z","level":"INFO","msg":"order created","order_id":"424b7ab5","seats":"10/5",'
      + `"total_cents":900,"trace_id":"${TRACE}","span_id":"09a2c3190b003b86"}`;
    const [l] = keyed([{ stream: 's', offset: '1', time: '2026-11-06T15:15:54.558Z', level: 'info', text }]);
    expect(l.trace).toBe(TRACE);
    expect(l.record.msg).toBe('order created');
    expect(l.record.shown.map((f) => `${f.key}=${f.short}`)).toEqual(['order_id=424b7ab5', 'seats=10/5', 'total_cents=900']);
    expect(l.record.fields.map((f) => f.key))
      .toEqual(['time', 'level', 'msg', 'order_id', 'seats', 'total_cents', 'trace_id', 'span_id']);
    // A field's word is the pair as the line writes it; a time has none.
    const by = Object.fromEntries(l.record.fields.map((f) => [f.key, f.word]));
    expect([by.time, by.total_cents, by.order_id]).toEqual(['', '"total_cents":900', '"order_id":"424b7ab5"']);
  });

  it('keeps as fields what its head does not say', () => {
    const r = recordOf('{"level":"notice","msg":"m","trace_id":"nope"}', { level: 'notice', msg: 'm', trace_id: 'nope' }, null,
      { time: '2026-11-01T20:00:00Z', level: 'unknown', trace: '' });
    expect(r.shown.map((f) => f.key)).toEqual(['level', 'trace_id']);
  });

  it('reads nested objects as dotted keys, arrays whole, long values cut', () => {
    const rec = { msg: 'failed', err: { type: 'Timeout' }, tags: ['a', 'b'], note: 'x'.repeat(200), spaced: 'a b' };
    const by = Object.fromEntries(recordOf(JSON.stringify(rec), rec, null, {}).fields.map((f) => [f.key, f]));
    expect(Object.keys(by)).toEqual(['msg', 'err.type', 'tags', 'note', 'spaced']);
    expect(by.tags.text).toBe('["a","b"]');
    expect(by.note.short.length).toBe(160);
    expect(by.note.text.length).toBe(200);
    expect([by.spaced.short, by.spaced.text]).toEqual(['"a b"', 'a b']);
    expect(by['err.type'].word).toBe('"Timeout"');
    expect(valueText(null)).toBe('null');
  });

  it('reads logfmt as pairs; other text stays text', () => {
    const text = 'time=2026-11-01T20:00:00Z level=INFO msg="order paid" order_id=f7d4 seats="2/4 2/5" ok=true';
    const [l] = keyed([{ stream: 's', offset: '1', time: '2026-11-01T20:00:00Z', level: 'info', text }]);
    expect(l.record.msg).toBe('order paid');
    expect(l.record.shown.map((f) => `${f.key}=${f.short}`)).toEqual(['order_id=f7d4', 'seats="2/4 2/5"', 'ok=true']);
    expect(logfmtOf(String.raw`msg="say \"hi\"" n=1`).map((p) => p.value)).toEqual(['say "hi"', '1']);
    for (const other of ['2026-11-01 20:00:59.844 UTC [50] LOG:  checkpoint complete: write=4.473 s', 'a=1',
      'listening addr=:8080 tls=off', 'k="open', '']) {
      expect(logfmtOf(other)).toBe(null);
    }
    expect(keyed([{ stream: 's', offset: '3', text: '127.0.0.1 - - "GET / HTTP/1.1" 200' }])[0].record).toBe(null);
  });

  it("puts a field's word into the search, -word for not, and out again", () => {
    const w = '"status":201';
    expect(toggleWord('timeout', w, false)).toBe(`timeout ${w}`);
    expect(toggleWord(`timeout ${w}`, w, false)).toBe('timeout');
    expect(toggleWord(`timeout ${w}`, w, true)).toBe(`timeout -${w}`);
    expect(toggleWord('/time.?out/', w, false)).toBe(null);
    expect([wordIn(`a ${w}`, w), wordIn(`a -${w}`, w), wordIn('a', w)]).toEqual(['in', 'out', '']);
  });

  it('of a request reads in a few words', () => {
    const rec = { method: 'GET', path: '/orders', status: 503, outcome: '5xx', durationNs: 12_400_000, instance: 'i-3f2a9c1b77', attempts: 2 };
    expect(requestParts(rec)).toEqual({ ask: 'GET /orders', end: '503', rest: '12 ms · 2 attempts · i-3f2a9c' });
    expect(requestParts({ method: 'GET', path: '/ws', status: 101, outcome: 'upgraded', durationNs: 4e9 }).end).toBe('101 upgraded');
    expect(durationText(840_000)).toBe('840 µs');
    const [l] = keyed([{ stream: 'g', offset: '0', text: JSON.stringify(rec) }]);
    expect(isRequest(l, { source: 'ingress' })).toBe(true);
    expect(isRequest(l, { source: 'container' })).toBe(false);
    expect(streamTag({ source: 'ingress', kind: 'service', object: 'api', replica: 0 })).toBe('api');
    expect(streamTag({ source: 'container', kind: 'service', object: 'api', replica: 1 })).toBe('api#1');
  });

  it('of a request out names its binding and how it ended', () => {
    const rec = { binding: 'erp', method: 'GET', path: '/odata/Orders', status: 429, durationMs: 120.4 };
    expect(isEgress({ rec }, { source: 'egress' })).toBe(true);
    expect(egressParts(rec)).toEqual({ binding: 'erp', ask: 'GET /odata/Orders', end: '429', refused: '', rest: '120 ms' });
    expect(egressParts({ ...rec, status: 0, error: { code: 'binding_denied' } }).end).toBe('refused');
  });

  it('shows its time in UTC or local time, the from and to fields too', () => {
    const t = '2026-11-01T23:30:05.123456789Z';
    expect(clockOf(t, true)).toBe('23:30:05.123Z');
    expect(dayOf(t, true)).toBe('2026-11-01');
    expect(dayText(t, true)).toBe('Sun, 1 Nov 2026');
    expect(inputOf('1793575805123456789', true)).toBe('2026-11-01T23:30');
    expect(timeOfInput('2026-11-01T23:30', true)).toBe('2026-11-01T23:30:00.000Z');
    expect(timeOfInput(inputOf(t))).toBe('2026-11-01T23:30:00.000Z');
    expect(clockOf('nonsense', true)).toBe('');
  });
});
