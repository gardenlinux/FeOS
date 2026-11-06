// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { it as test } from 'vitest'
import assert from 'node:assert/strict'
import {
  attrValue, barOf, bindingOf, criticalByService, criticalPath, firstFolds, foldLevel, fromJaeger, isCall, offsetText, selfUs,
  serviceHues, shownRows, spanRows, spanTitle, summaryOf, summaryText, traceFocusOf, traceQueryOf, traceRangeOf,
  traceSearchParams, waitParts, whereOf,
} from './traces.js'

const T = 1793361600000000
const span = (spanId, parentId, startUs, durationUs, extra = {}) => ({ spanId, parentId, startUs: T + startUs, durationUs, service: 'api', name: spanId, ...extra })

test('the waterfall: each root by start, its children under it by start', () => {
  const rows = spanRows([
    span('db', 'srv', 5000, 1300000),
    span('in', '', 0, 1400000),
    span('pub', 'srv', 3000, 900),
    span('srv', 'in', 2000, 1395000),
    span('late', 'gone', 9000, 10),
  ])
  assert.deepEqual(
    rows.map((r) => `${r.span.spanId}:${r.depth}:${r.children}${r.orphan ? ':orphan' : ''}`),
    ['in:0:1', 'srv:1:2', 'pub:2:0', 'db:2:0', 'late:0:0:orphan'],
  )
  assert.deepEqual(spanRows([]), [])
  assert.deepEqual(spanRows(null), [])
})

test('a cycle the store should never answer still shows each span once', () => {
  const rows = spanRows([span('a', 'b', 0, 1), span('b', 'a', 1, 1)])
  assert.deepEqual(
    rows.map((r) => r.span.spanId),
    ['a', 'b'],
  )
})

test('the tree\'s lines: parent, path, last child, spans below, rails', () => {
  const rows = spanRows([
    span('in', '', 0, 100),
    span('a', 'in', 1, 50),
    span('a1', 'a', 2, 10),
    span('a2', 'a', 20, 10),
    span('b', 'in', 60, 30),
    span('b1', 'b', 61, 5),
  ])
  const by = Object.fromEntries(rows.map((r) => [r.span.spanId, r]))
  assert.deepEqual(by.a1.path, ['in', 'a'])
  assert.equal(by.a1.up, 'a')
  assert.equal(by.in.up, '')
  assert.deepEqual(
    rows.map((r) => `${r.span.spanId}:${r.last ? 'last' : 'more'}:${r.below}`),
    ['in:last:5', 'a:more:2', 'a1:more:0', 'a2:last:0', 'b:last:1', 'b1:last:0'],
  )
  // a's line runs on past its children, as b follows it; b's does not.
  assert.deepEqual(by.a1.rails, [true])
  assert.deepEqual(by.b1.rails, [false])
  assert.deepEqual(by.a.rails, [])
})

test('folding hides what hangs below a span, and opening one level at a time', () => {
  const rows = spanRows([span('in', '', 0, 100), span('a', 'in', 1, 50), span('a1', 'a', 2, 10), span('b', 'in', 60, 30)])
  const ids = (list) => list.map((r) => r.span.spanId)
  assert.deepEqual(ids(shownRows(rows, new Set())), ['in', 'a', 'a1', 'b'])
  assert.deepEqual(ids(shownRows(rows, new Set(['a']))), ['in', 'a', 'b'])
  assert.deepEqual(ids(shownRows(rows, new Set(['in', 'a']))), ['in'])
  // a leaf folded hides nothing
  assert.deepEqual(ids(shownRows(rows, new Set(['b']))), ['in', 'a', 'a1', 'b'])
  const all = new Set(['in', 'a'])
  assert.deepEqual([...foldLevel(rows, all, true)], ['a'])
  assert.deepEqual([...foldLevel(rows, new Set(['a']), true)], [])
  assert.deepEqual([...foldLevel(rows, new Set(), false)].sort(), ['a'])
  assert.deepEqual([...foldLevel(rows, new Set(['a']), false)].sort(), ['a', 'in'])
})

test('a trace opens with its failures shown, then its longest spans, within the budget', () => {
  const spans = [span('in', '', 0, 1000), span('slow', 'in', 1, 600), span('fast', 'in', 700, 100)]
  for (let i = 0; i < 5; i++) spans.push(span(`s${i}`, 'slow', 2 + i, 10))
  spans.push(span('f0', 'fast', 701, 10), span('bad', 'f0', 702, 5, { error: true }))
  const rows = spanRows(spans)
  const shown = (budget) => shownRows(rows, firstFolds(rows, budget)).map((r) => r.span.spanId)
  // the failure's ancestors open first, then nothing more fits in 6
  assert.deepEqual(shown(6), ['in', 'slow', 'fast', 'f0', 'bad'])
  // with room, the longest folded span opens next
  assert.deepEqual(shown(10), ['in', 'slow', 's0', 's1', 's2', 's3', 's4', 'fast', 'f0', 'bad'])
  // a small trace opens whole
  assert.equal(firstFolds(spanRows([span('in', '', 0, 10), span('x', 'in', 1, 1)])).size, 0)
})

test('the critical path: the last child to finish holds the parent, then the one before it', () => {
  // in [0,100): a [10,40) and b [50,90) one after the other; c [55,60)
  // inside b; d [20,95) beside them ends last, so it holds the end.
  const rows = spanRows([
    span('in', '', 0, 100),
    span('a', 'in', 10, 30),
    span('b', 'in', 50, 40),
    span('c', 'b', 55, 5),
    span('late', 'in', 120, 50), // a job left behind: after its parent ended
  ])
  const p = criticalPath(rows)
  const at = (id) => (p.get(id) || []).map(([x, y]) => [x - T, y - T])
  assert.deepEqual(at('in'), [
    [0, 10],
    [40, 50],
    [90, 100],
  ])
  assert.deepEqual(at('a'), [[10, 40]])
  assert.deepEqual(at('b'), [
    [50, 55],
    [60, 90],
  ])
  assert.deepEqual(at('c'), [[55, 60]])
  assert.deepEqual(at('late'), [])
  assert.deepEqual(criticalByService(rows, p), [['api', 100]])

  // Two children at once: the one that ends last holds; the other ran beside it.
  const two = spanRows([span('in', '', 0, 100), span('x', 'in', 10, 80), span('y', 'in', 20, 50)])
  const q = criticalPath(two)
  assert.deepEqual(q.get('x').map(([a, b]) => [a - T, b - T]), [[10, 90]])
  assert.equal(q.has('y'), false)

  // A clock that runs ahead puts a child before its caller: only its
  // part inside the caller counts.
  const skew = spanRows([span('in', '', 10, 50), span('srv', 'in', 0, 30, { service: 'srv' })])
  const r = criticalPath(skew)
  assert.deepEqual(r.get('srv').map(([a, b]) => [a - T, b - T]), [[10, 30]])
  assert.deepEqual(criticalByService(skew, r), [
    ['api', 30],
    ['srv', 20],
  ])
})

test('the time a span spent in none of its own children', () => {
  const s = span('in', '', 0, 100)
  assert.equal(selfUs(s, []), 100)
  assert.equal(selfUs(s, undefined), 100)
  // [10,40) and [30,50) overlap: 40 busy; [90,130) counts up to the end
  assert.equal(selfUs(s, [span('a', 'in', 10, 30), span('b', 'in', 30, 20), span('c', 'in', 90, 40)]), 50)
  assert.equal(selfUs(s, [span('late', 'in', 200, 10)]), 100)
})

test('services keep their hues: the environment\'s by name, then the trace\'s, grey past seven', () => {
  const h = serviceHues(['web', 'api', 'db'], [{ service: 'ferun-ingress' }, { service: 'api' }])
  assert.deepEqual(Object.fromEntries(h), { api: 1, db: 2, web: 3, 'ferun-ingress': 4 })
  const many = serviceHues(['a', 'b', 'c', 'd', 'e', 'f', 'g', 'h'], [])
  assert.equal(many.get('g'), 7)
  assert.equal(many.get('h'), 0)
  assert.equal(isCall({ kind: 'client' }), true)
  assert.equal(isCall({ kind: 'producer' }), true)
  assert.equal(isCall({ kind: 'server' }), false)
})

test('a bar is placed by start and duration in the trace, at least a sliver', () => {
  const trace = { startUs: T, durationUs: 1000 }
  assert.deepEqual(barOf(span('x', '', 250, 500), trace), { left: 25, width: 50 })
  assert.deepEqual(barOf(span('x', '', 1000, 0), trace), { left: 100, width: 0 })
  assert.equal(barOf(span('x', '', 0, 0), trace).width, 0.4)
  assert.deepEqual(barOf(span('x', '', 0, 5), { startUs: T, durationUs: 0 }), { left: 0, width: 100 })
})

test('where a span ran: object#replica, the ingress by its service', () => {
  assert.equal(whereOf({ service: 'expense-service', resource: [{ key: 'ferun.object', value: 'expense-service' }, { key: 'ferun.replica', value: 1 }] }), 'expense-service#1')
  assert.equal(whereOf({ service: 'ferun-ingress', resource: [{ key: 'ferun.source', value: 'ingress' }, { key: 'ferun.object', value: 'api' }] }), 'ferun-ingress')
  assert.equal(whereOf({ service: 'worker', resource: [] }), 'worker')
})

test('values, offsets and a trace in a few words', () => {
  assert.equal(attrValue({ value: 'x' }), 'x')
  assert.equal(attrValue({ value: 503 }), '503')
  assert.equal(attrValue({ value: true }), 'true')
  assert.equal(offsetText(T + 2000, T), '+2 ms')
  assert.equal(offsetText(T + 1400000, T), '+1.4 s')
  assert.equal(summaryText({ name: 'POST', path: '/expenses', statusCode: 503 }), 'POST /expenses → 503')
  assert.equal(summaryText({ name: 'publish', path: '', statusCode: 0 }), 'publish')
  assert.equal(spanTitle({ name: 'POST', attributes: [{ key: 'url.path', value: '/expenses' }] }), 'POST /expenses')
  assert.equal(spanTitle({ name: 'POST /expenses', attributes: [{ key: 'url.path', value: '/expenses' }] }), 'POST /expenses')
  assert.equal(spanTitle({ name: 'INSERT expenses', attributes: [] }), 'INSERT expenses')
})

test('the recent traces: the range and service in the address, start and end in the store\'s query', () => {
  assert.equal(traceRangeOf({}), '1h')
  assert.equal(traceRangeOf({ since: '2026-10-30T12:00:00Z' }), '1h')
  assert.deepEqual(traceFocusOf('?service=api&since=1d&x=1'), { service: 'api', since: '1d' })
  assert.deepEqual(traceFocusOf('?since=nonsense'), {})
  assert.equal(traceQueryOf({ service: 'api', since: '1h' }), '?service=api')
  const now = Date.parse('2026-11-06T15:00:00Z')
  assert.equal(
    traceSearchParams({ since: '1d', service: 'api' }, 'env-1', now).toString(),
    'environment=env-1&service=api&start=1793890800000000&end=1793977200000000&limit=50',
  )
})

test('a trace of Jaeger\'s shape reads as spans: parent, links, kind, status, events, resource', () => {
  const t = fromJaeger({
    traceID: 'aa',
    spans: [
      { traceID: 'aa', spanID: 's2', operationName: 'SELECT', references: [{ refType: 'CHILD_OF', traceID: 'aa', spanID: 's1' }, { refType: 'FOLLOWS_FROM', traceID: 'bb', spanID: 'x' }],
        startTime: 1100, duration: 300, tags: [{ key: 'span.kind', type: 'string', value: 'client' }, { key: 'otel.status_code', type: 'string', value: 'ERROR' },
          { key: 'otel.status_description', type: 'string', value: 'timeout' }, { key: 'db.system', type: 'string', value: 'postgresql' }],
        logs: [{ timestamp: 1200, fields: [{ key: 'event', type: 'string', value: 'exception' }, { key: 'exception.message', type: 'string', value: 'boom' }] }], processID: 'p2' },
      { traceID: 'aa', spanID: 's1', operationName: 'GET /orders', references: [], startTime: 1000, duration: 500,
        tags: [{ key: 'http.response.status_code', type: 'int64', value: 503 }, { key: 'url.path', type: 'string', value: '/orders' }], logs: [], processID: 'p1' },
      { traceID: 'aa', spanID: 's1', operationName: 'again', references: [], startTime: 0, duration: 1, tags: [], logs: [], processID: 'p1' },
    ],
    processes: { p1: { serviceName: 'orders', tags: [{ key: 'ferun.object', type: 'string', value: 'orders' }] }, p2: { serviceName: 'orders', tags: [] } },
    warnings: null,
  })
  assert.deepEqual([t.startUs, t.durationUs, t.spans.map((s) => s.spanId)], [1000, 500, ['s1', 's2']])
  const db = t.spans[1]
  assert.deepEqual([db.parentId, db.kind, db.error, db.statusMessage, db.service], ['s1', 'client', true, 'timeout', 'orders'])
  assert.deepEqual(db.links, [{ traceId: 'bb', spanId: 'x' }])
  assert.deepEqual(db.attributes.map((a) => a.key), ['db.system'])
  assert.deepEqual(db.events, [{ timeUs: 1200, name: 'exception', attributes: [{ key: 'exception.message', type: 'string', value: 'boom' }] }])
  assert.deepEqual(t.spans[0].resource.map((a) => a.key), ['ferun.object'])
  const sm = summaryOf({ traceID: 'aa', spans: [], processes: {} })
  assert.deepEqual([sm.spans, sm.name], [0, ''])
})

test('a trace of a search in a few facts: its root, path, status, services, failures', () => {
  const sm = summaryOf({
    traceID: 'aa',
    spans: [
      { traceID: 'aa', spanID: 'b', operationName: 'charge', references: [{ refType: 'CHILD_OF', traceID: 'aa', spanID: 'a' }], startTime: 2, duration: 1,
        tags: [{ key: 'error', type: 'bool', value: true }], processID: 'q' },
      { traceID: 'aa', spanID: 'a', operationName: 'POST', references: [], startTime: 1, duration: 9,
        tags: [{ key: 'url.path', type: 'string', value: '/orders' }, { key: 'http.response.status_code', type: 'int64', value: 201 }], processID: 'p' },
    ],
    processes: { p: { serviceName: 'ferun-ingress' }, q: { serviceName: 'payment' } },
  })
  assert.deepEqual(sm, { traceId: 'aa', startUs: 1, durationUs: 9, service: 'ferun-ingress', name: 'POST', path: '/orders', statusCode: 201,
    services: ['ferun-ingress', 'payment'], spans: 2, errors: 1 })
  assert.equal(summaryText(sm), 'POST /orders → 201')
  assert.equal(summaryText({ name: 'GET /orders', path: '/orders', statusCode: 0 }), 'GET /orders')
})

test('a request out through an HTTP secret is a client span that names it', () => {
  const span = {
    spanId: 'b1',
    parentId: 'a1',
    service: 'api',
    name: 'GET erp',
    kind: 'client',
    startUs: 10,
    durationUs: 120000,
    attributes: [
      { key: 'ferun.binding', type: 'string', value: 'erp' },
      { key: 'url.path', type: 'string', value: '/odata/Orders' },
      { key: 'http.response.status_code', type: 'int64', value: 429 },
    ],
    resource: [
      { key: 'ferun.object', type: 'string', value: 'api' },
      { key: 'ferun.replica', type: 'int64', value: 1 },
    ],
  }
  assert.equal(bindingOf(span), 'erp')
  assert.equal(isCall(span), true)
  assert.equal(spanTitle(span), 'GET erp /odata/Orders')
  assert.equal(whereOf(span), 'api#1')
  assert.equal(bindingOf({ ...span, kind: 'server' }), '')
  assert.equal(bindingOf({ ...span, attributes: [] }), '')
})

test('a span waits where its own spans run, and a call without one waits whole', () => {
  const s = span('in', '', 0, 100)
  // [10,40) and [30,50) merge; [90,130) is cut at the end; one after the end is none.
  assert.deepEqual(
    waitParts(s, [span('a', 'in', 10, 30), span('b', 'in', 30, 20), span('c', 'in', 90, 40), span('late', 'in', 200, 5)]).map(([a, b]) => [a - T, b - T]),
    [[10, 50], [90, 100]],
  )
  assert.deepEqual(waitParts(s, []), [])
  assert.deepEqual(waitParts({ ...s, kind: 'client' }, []).map(([a, b]) => [a - T, b - T]), [[0, 100]])
  assert.deepEqual(waitParts({ ...s, kind: 'producer', durationUs: 0 }, []), [])
})
