// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useEffect, useMemo, useRef, useState } from 'react';
import { getObject, isObject } from './draft.js';
import { addVariable, refTargets } from './refs.js';
import { atField, unitKey, worst } from './findings.js';
import { imageView } from './pins.js';
import {
  ENTRY, SIZE, addBinding, addRoute, canDrag, diffMarks, dropAction, dropTargets, firstRoutePath, focusOf, graphOf,
  hitTest, layoutGraph, removedOf,
} from './graph.js';
import { AddReference } from './BuilderForms.jsx';

// The builder's graph (ADR 0032, "The graph", "Drawing a reference",
// "Drawn by hand"): HTML nodes placed by graph.js over an SVG layer of
// edges. A node opens its form beside the graph; it carries its
// findings' worst tone and the plan's change. A service's or job's handle
// is dragged onto an addon, a secret, a service's port or a binding to
// write a reference, the entry's onto a service's port to write a route;
// the handle pressed without a drag (or with the keyboard) lists the same
// targets. The canvas scrolls; focus dims what is not next to the open
// object.

const KIND = { entry: 'entry', services: 'service', jobs: 'job', addons: 'addon', bindings: 'binding', secrets: 'secret', sandbox: 'sandbox' };
const MARK = { add: ['+', 'added'], update: ['~', 'changed'], remove: ['−', 'removed'] };
const OFFER_W = 360;

// summary is a node's or a card's line under its name.
export function summary(key, o, pin) {
  if (key === 'secrets') {
    if (!pin) return '';
    const p = pin.pinned ? `pinned ${pin.pinned}` : 'latest when applied';
    return pin.latest ? `${p} · latest ${pin.latest}` : p;
  }
  if (!isObject(o)) return '';
  switch (key) {
    case 'services': {
      if (o.runtime === 'attached') return `attached · sandbox ${o.sandbox || '—'}`;
      const v = imageView(o);
      const ports = (Array.isArray(o.ports) ? o.ports : []).map((p) => p && `${p.name}:${p.port}`).join(' ');
      return [v.written || 'no image yet', v.digest ? 'pinned' : '', ports].filter(Boolean).join(' · ');
    }
    case 'jobs': return [imageView(o).written || 'no image yet', o.on === 'manual' ? 'when asked' : 'with deployments'].join(' · ');
    case 'addons': return [o.type, o.version, o.plan].filter(Boolean).join(' · ');
    case 'routes': return `${o.path || '?'} → ${o.service || '?'}${o.port !== undefined ? ':' + o.port : ''}`;
    case 'bindings': return o.upstream || '';
    default: return '';
  }
}

function nodeLine(n, spec, pin) {
  if (n.missing) return `not in the draft`;
  if (n.removed) return 'the plan removes it';
  if (n.key === 'sandbox') return 'outside the environment';
  const o = n.key === 'secrets' ? null : getObject(spec, n.key, n.name);
  if (n.key === 'services' && isObject(o) && o.runtime !== 'attached') return imageView(o).written || 'no image yet';
  if (n.key === 'jobs' && isObject(o)) return imageView(o).written || 'no image yet';
  return summary(n.key, o, pin);
}

export function GraphView({ spec, groups, diff, open, onOpen, entry, catalog, knownSecrets, pins, edit, focus }) {
  const graph = useMemo(() => graphOf(spec, { entry, removed: removedOf(diff, spec) }),
    [spec, diff, entry && entry.public, entry && (entry.hosts || []).join(' '), entry && entry.env]); // eslint-disable-line react-hooks/exhaustive-deps
  const layout = useMemo(() => layoutGraph(graph), [graph]);
  const marks = useMemo(() => diffMarks(diff), [diff]);
  const near = focus ? focusOf(graph, open) : null;
  const pinByName = new Map((pins || []).map((p) => [p.name, p]));
  const scroll = useRef(null);
  const plane = useRef(null);
  const [drag, setDrag] = useState(null);
  const [offer, setOffer] = useState(null);
  const dragged = useRef(false);
  const dragEnd = useRef(0);
  useEffect(() => setOffer(null), [spec]);

  const at = (e) => {
    const r = plane.current.getBoundingClientRect();
    return { x: e.clientX - r.left, y: e.clientY - r.top };
  };
  const startDrag = (e, id) => {
    if (e.button !== 0) return;
    e.preventDefault();
    e.currentTarget.setPointerCapture(e.pointerId);
    const p = layout.nodes.get(id);
    const s = { x: p.x + p.w, y: p.y + SIZE.head / 2 };
    dragged.current = false;
    setOffer(null);
    setDrag({ from: id, s, p: s, start: at(e), action: null, hit: null });
  };
  const moveDrag = (e) => {
    if (!drag) return;
    const p = at(e);
    if (Math.abs(p.x - drag.start.x) + Math.abs(p.y - drag.start.y) > 4) dragged.current = true;
    const hit = hitTest(layout, p.x, p.y);
    setDrag({ ...drag, p, hit, action: dropAction(layout, drag.from, hit, spec) });
    // Scroll the canvas while the pointer is at its edge.
    const c = scroll.current;
    const r = c.getBoundingClientRect();
    if (e.clientX > r.right - 28) c.scrollLeft += 16;
    else if (e.clientX < r.left + 28) c.scrollLeft -= 16;
    if (e.clientY > r.bottom - 28) c.scrollTop += 16;
    else if (e.clientY < r.top + 28) c.scrollTop -= 16;
  };
  const endDrag = () => {
    if (!drag) return;
    if (dragged.current && drag.action) setOffer({ from: drag.from, action: drag.action, x: drag.p.x, y: drag.p.y });
    if (dragged.current) dragEnd.current = Date.now();
    dragged.current = false;
    setDrag(null);
  };
  // pick lists the handle's targets, for the keyboard and a click.
  const pick = (id) => {
    // The click that ends a drag is no click.
    if (Date.now() - dragEnd.current < 400) return;
    const p = layout.nodes.get(id);
    setOffer({ from: id, pick: dropTargets(layout, id, spec), x: p.x + p.w + 8, y: p.y });
  };

  // offerLeft keeps an offer inside the canvas's visible part.
  const offerLeft = (x) => {
    const c = scroll.current;
    const right = c ? c.scrollLeft + c.clientWidth : layout.width;
    return Math.max(4, Math.min(x, right - OFFER_W - 8, layout.width - OFFER_W - 4));
  };

  const tone = (id) => worst(groups.objects.get(id) || []);
  const edgeTone = (e) => {
    const fs = groups.objects.get(e.unit) || [];
    return worst(e.via === 'route' ? fs : atField(fs, e.field));
  };
  const dim = (id, edge) => (near ? !(edge ? near.edges.has(id) : near.nodes.has(id)) : false);

  return (
    <div className="g-canvas" ref={scroll} onKeyDown={(e) => {
      // Escape closes an offer (and goes no level up while one is open).
      if (e.key === 'Escape' && (offer || drag)) { e.stopPropagation(); setOffer(null); setDrag(null); }
    }}>
      <div className={'g-plane' + (drag ? ' dragging' : '')} ref={plane} style={{ width: layout.width, height: layout.height }}
        onClick={(e) => { if (e.target === e.currentTarget && !offer) onOpen(''); }}>
        <div className="g-frame" style={{ left: layout.frame.x, top: layout.frame.y, width: layout.frame.w, height: layout.frame.h }}>
          <span className="g-framelabel">environment <span className="mono">{entry && entry.env ? entry.env : '(new)'}</span></span>
        </div>
        <svg className="g-edges" width={layout.width} height={layout.height} aria-hidden="true">
          <defs>
            <marker id="g-arrow" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="7" markerHeight="7" orient="auto">
              <path d="M0,0 L8,4 L0,8 z" className="g-arrowhead" />
            </marker>
          </defs>
          {layout.edges.map((e) => {
            const t = edgeTone(e);
            const m = marks.get(e.unit);
            return (
              <path key={e.id} d={e.d} markerEnd="url(#g-arrow)"
                className={'g-edge v-' + e.via + (e.dashed ? ' dashed' : '') + (t ? ' ' + t : '') + (m && e.via === 'route' ? ' m-' + m : '')
                  + (dim(e.id, true) ? ' dim' : '') + (open === e.unit ? ' on' : '')} />
            );
          })}
          {drag && dragged.current && (
            <path className={'g-edge g-drawing' + (drag.action ? ' ok' : '')}
              d={`M${drag.s.x},${drag.s.y} C${drag.s.x + 60},${drag.s.y} ${drag.p.x - 60},${drag.p.y} ${drag.p.x},${drag.p.y}`} />)}
        </svg>
        {layout.edges.map((e) => {
          if (e.via === 'sandbox') return null;
          const t = edgeTone(e);
          return (
            <button key={'l' + e.id} type="button"
              className={'g-label mono' + (t ? ' ' + t : '') + (dim(e.id, true) ? ' dim' : '') + (open === e.from ? ' full' : '')}
              style={{ left: e.sx + 10, top: e.sy - 15, maxWidth: open === e.from ? undefined : SIZE.gapX - 18 }}
              title={`${e.label} (${e.field})`}
              onClick={() => onOpen(e.unit)}>{e.label}</button>
          );
        })}
        {[...layout.nodes.values()].map((n) => (
          <GraphNode key={n.id} n={n} spec={spec} pin={pinByName.get(n.name)} tone={tone(n.id)}
            findings={(groups.objects.get(n.id) || []).length} mark={marks.get(n.id)} on={open === n.id} dim={dim(n.id)}
            target={drag && drag.hit && drag.hit.id === n.id ? (drag.action ? 'ok' : 'no') : ''}
            targetPort={drag && drag.action && drag.hit && drag.hit.id === n.id ? drag.hit.port : -1}
            onOpen={onOpen} onPointerDown={startDrag} onPointerMove={moveDrag} onPointerUp={endDrag} onPick={pick} />
        ))}
        {offer && (
          <div className="g-offer" style={{ left: offerLeft(offer.x), top: offer.y + 6, width: OFFER_W }}>
            <Offer offer={offer} layout={layout} spec={spec} catalog={catalog} knownSecrets={knownSecrets}
              onChoose={(action) => setOffer({ ...offer, action, pick: null })}
              onDone={(next, openUnit) => { setOffer(null); edit(next); if (openUnit) onOpen(openUnit); }}
              onCancel={() => setOffer(null)} />
          </div>)}
      </div>
    </div>
  );
}

function GraphNode({ n, spec, pin, tone, findings, mark, on, dim, target, targetPort, onOpen, onPointerDown, onPointerMove, onPointerUp, onPick }) {
  const opens = !n.missing && !n.removed && n.key !== 'entry' && n.key !== 'sandbox';
  const cls = 'g-node k-' + n.key + (tone ? ' ' + tone : '') + (mark ? ' m-' + mark : '') + (on ? ' on' : '') + (dim ? ' dim' : '')
    + (n.missing ? ' missing' : '') + (n.removed ? ' removed' : '') + (target ? ' t-' + target : '');
  const head = (
    <>
      <span className="g-head">
        <span className="g-kind">{n.key === 'addons' && n.type ? n.type : KIND[n.key]}</span>
        <span className="mono strong g-name" title={n.name}>{n.key === 'entry' ? (n.public ? 'public' : 'not public') : n.name}</span>
        {mark && <span className={'g-mark m-' + mark} title={MARK[mark][1] + ' by this draft'}>{MARK[mark][0]}</span>}
        {findings > 0 && <span className={'badge ' + (tone === 'error' ? 'bad' : tone === 'warning' ? 'warn' : '')}>{findings}</span>}
        {pin && pin.missing && <span className="badge bad">missing</span>}
        {pin && pin.newer && <span className="badge warn">v{pin.latest}</span>}
      </span>
      {n.key !== 'entry' && <span className="g-sub muted small" title={nodeLine(n, spec, pin)}>{nodeLine(n, spec, pin)}</span>}
      {n.key === 'entry' && (
        <span className="g-sub muted small">{n.public ? 'its hosts' : 'the route door, with route tokens'}</span>)}
    </>
  );
  return (
    <div className={cls} style={{ left: n.x, top: n.y, width: n.w, height: n.h }}>
      {opens
        ? <button type="button" className="g-body" onClick={() => onOpen(n.id)} title={`open ${KIND[n.key]} ${n.name}`}>{head}</button>
        : <div className="g-body">{head}</div>}
      {n.ports.map((p, i) => (
        <span key={i} className={'g-port mono small' + (targetPort === i ? ' t-ok' : '')} style={{ top: SIZE.head + i * SIZE.row, height: SIZE.row }}>
          <span className="g-dot" />{p.name || 'port'} <span className="muted">:{p.port}</span>
        </span>
      ))}
      {n.key === 'entry' && (n.hosts.length ? n.hosts : [`/api/v1/environments/${n.env || '{env}'}/http/…`]).map((h, i) => (
        <span key={i} className="g-port g-host mono small" style={{ top: SIZE.head + i * SIZE.row, height: SIZE.row }} title={h}>{h}</span>
      ))}
      {canDrag(n) && (
        <button type="button" className="g-handle" style={{ top: SIZE.head / 2 - 7 }}
          title={n.key === 'entry' ? 'drag onto a service\'s port: a route' : 'drag onto an addon, a secret, a service or a binding: a reference'}
          aria-label={n.key === 'entry' ? 'add a route' : `add a reference from ${n.name}`}
          onPointerDown={(e) => onPointerDown(e, n.id)} onPointerMove={onPointerMove} onPointerUp={onPointerUp}
          onPointerCancel={onPointerUp} onClick={() => onPick(n.id)} />)}
    </div>
  );
}

// Offer is what a drop (or the handle's list) offers: the list of
// targets, a reference with its key and variable name, a binding, a route
// with its path.
function Offer({ offer, layout, spec, catalog, knownSecrets, onChoose, onDone, onCancel }) {
  const from = layout.nodes.get(offer.from);
  const [path, setPath] = useState(() => firstRoutePath(spec));
  if (!from) return null;
  if (offer.pick) {
    return (
      <div className="g-offerbody">
        <div className="sect">{from.key === 'entry' ? 'a route to' : `${from.name} references`}</div>
        {offer.pick.length === 0 && <p className="muted small">Nothing to reach yet: add {from.key === 'entry' ? 'a service with a port' : 'an addon, a secret or another service'}.</p>}
        <div className="g-picks">
          {offer.pick.map((t, i) => {
            const n = layout.nodes.get(t.id);
            const port = t.port >= 0 ? ` :${n.ports[t.port].name || n.ports[t.port].port}` : '';
            return (
              <button key={i} type="button" className="ghost mini" autoFocus={i === 0} onClick={() => onChoose(t.action)}>
                {KIND[n.key]} <span className="mono">{n.name}{port}</span>
              </button>
            );
          })}
        </div>
        <div className="b-row"><span className="spacer" /><button type="button" className="ghost mini" onClick={onCancel}>Cancel</button></div>
      </div>
    );
  }
  const a = offer.action;
  if (a.kind === 'ref') {
    const targets = refTargets(spec, catalog, { self: from.key === 'services' ? from.name : '', knownSecrets });
    return (
      <div className="g-offerbody">
        <div className="sect">{from.name} → {a.target.name}</div>
        <AddReference targets={targets} catalog={catalog} initial={{ id: unitKey(a.target.key, a.target.name), port: a.port }}
          onAdd={(name, value) => onDone(addVariable(spec, from.key, from.name, name, value), from.id)} onCancel={onCancel} />
      </div>
    );
  }
  if (a.kind === 'binding') {
    return (
      <div className="g-offerbody">
        <div className="sect">{from.name} → binding {a.name}</div>
        <div className="b-row">
          <button type="button" className="primary mini" autoFocus onClick={() => onDone(addBinding(spec, from.key, from.name, a.name), from.id)}>Bind</button>
          <button type="button" className="ghost mini" onClick={onCancel}>Cancel</button>
        </div>
      </div>
    );
  }
  const ok = path.startsWith('/');
  const add = () => {
    if (!ok) return;
    const r = addRoute(spec, a.service, a.port, path);
    onDone(r.spec, unitKey('routes', r.name));
  };
  return (
    <div className="g-offerbody">
      <div className="sect">a route to {a.service}{a.port !== '' ? `:${a.port}` : ''}</div>
      <div className="b-row">
        <input className="mono grow" autoFocus placeholder="/api" value={path} onChange={(e) => setPath(e.target.value.trim())}
          onKeyDown={(e) => { if (e.key === 'Enter') add(); }} />
        <button type="button" className="primary mini" disabled={!ok} onClick={add}>Add route</button>
        <button type="button" className="ghost mini" onClick={onCancel}>Cancel</button>
      </div>
      {path && !ok && <p className="bad small">a path starts with /</p>}
    </div>
  );
}

export { ENTRY };
