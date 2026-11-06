// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useEffect, useMemo, useRef, useState } from 'react';
import { Icon } from '../Icon.jsx';
import { getObject, imageBaseName, isObject } from './draft.js';
import { addVariable, refTargets } from './refs.js';
import { atField, unitKey, worst } from './findings.js';
import { imageView } from './pins.js';
import {
  ENTRY, SIZE, addBinding, addRoute, canDrag, dropAction, dropTargets, firstRoutePath, focusOf, graphOf,
  hitTest, layoutGraph,
} from './graph.js';
import { AddReference } from './BuilderForms.jsx';

// The builder's graph (ADR 0032, "The graph", "Drawing a reference",
// "Drawn by hand"): HTML nodes placed by graph.js over an SVG layer of
// edges, one edge per pair of objects. A node opens its form beside the
// graph; it carries its findings' worst tone, the draft's change against
// its base (+ ~ −) and its secrets and registry login as markers. Hovering
// a node (or a secret in the list) highlights what it is joined to; focus
// dims what is not next to the open object. A service's or job's handle
// is dragged onto an addon, a service or a binding to write a reference,
// the entry's onto a service to write a route; the handle pressed without
// a drag (or with the keyboard) lists the same targets.

const KIND = { entry: 'entry', services: 'service', jobs: 'job', addons: 'addon', bindings: 'binding', secrets: 'secret', sandbox: 'sandbox' };
export const MARK = { add: ['+', 'added'], update: ['~', 'changed'], remove: ['−', 'removed'] };
const OFFER_W = 360;

// summary is a node's or a card's line under its name.
export function summary(key, o, pin) {
  if (key === 'secrets') {
    if (!pin) return '';
    const p = pin.pinned ? `pinned ${pin.pinned}` : 'latest when deployed';
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

// shortImage is an image as a node shows it: its last segment and tag.
function shortImage(o) {
  const w = imageView(o).written;
  if (!w) return '';
  const tag = /:([^/:@]+)$/.exec(w.split('@')[0]);
  return imageBaseName(w) + (tag ? `:${tag[1]}` : '');
}

function nodeLine(n, spec) {
  if (n.missing) return 'not in the draft';
  if (n.removed) return 'removed by this draft';
  if (n.key === 'sandbox') return 'outside the environment';
  const o = getObject(spec, n.key, n.name);
  if (n.key === 'services' && isObject(o) && o.runtime === 'attached') return `attached · ${o.sandbox || '—'}`;
  if ((n.key === 'services' || n.key === 'jobs') && isObject(o)) return shortImage(o) || 'no image yet';
  if (n.key === 'addons' && isObject(o)) return [o.version, o.plan].filter(Boolean).join(' · ');
  if (n.key === 'bindings' && isObject(o)) return String(o.upstream || '').replace(/^https?:\/\//, '');
  return '';
}

export function GraphView({ spec, groups, marks, removed, open, onOpen, entry, catalog, knownSecrets, pins, edit, focus, hover, onHover, onAddSecret }) {
  const graph = useMemo(() => graphOf(spec, { entry, removed }),
    [spec, removed, entry && entry.public, entry && (entry.hosts || []).join(' '), entry && entry.env]); // eslint-disable-line react-hooks/exhaustive-deps
  const layout = useMemo(() => layoutGraph(graph), [graph]);
  const near = focusOf(graph, hover || (focus ? open : '')) || null;
  const pinByName = new Map((pins || []).map((p) => [p.name, p]));
  const scroll = useRef(null);
  const plane = useRef(null);
  const [drag, setDrag] = useState(null);
  const [offer, setOffer] = useState(null);
  const dragged = useRef(false);
  const dragEnd = useRef(0);
  useEffect(() => setOffer(null), [spec]);
  // The open node comes into view.
  useEffect(() => {
    const p = layout.nodes.get(open);
    const c = scroll.current;
    if (!p || !c) return;
    if (p.x < c.scrollLeft || p.x + p.w > c.scrollLeft + c.clientWidth) c.scrollLeft = Math.max(0, p.x - 24);
    if (p.y < c.scrollTop || p.y + p.h > c.scrollTop + c.clientHeight) c.scrollTop = Math.max(0, p.y - 24);
  }, [open]); // eslint-disable-line react-hooks/exhaustive-deps

  const at = (e) => {
    const r = plane.current.getBoundingClientRect();
    return { x: e.clientX - r.left, y: e.clientY - r.top };
  };
  const startDrag = (e, id) => {
    if (e.button !== 0) return;
    e.preventDefault();
    e.currentTarget.setPointerCapture(e.pointerId);
    const p = layout.nodes.get(id);
    const s = { x: p.x + p.w, y: p.y + p.h / 2 };
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
  const edgeTone = (e) => worst(e.refs.flatMap((r) => {
    const fs = groups.objects.get(r.unit) || [];
    return r.unit.startsWith('routes/') ? fs : atField(fs, r.field);
  }));
  const edgeMark = (e) => {
    for (const u of e.units) if (u.startsWith('routes/') && marks && marks.get(u)) return marks.get(u);
    return '';
  };
  const dim = (id, edge) => (near ? !(edge ? near.edges.has(id) : near.nodes.has(id)) : false);
  const lit = (id) => !!(near && hover && near.edges.has(id));

  return (
    <div className="g-wrap">
      <div className="g-canvas" ref={scroll} onKeyDown={(e) => {
        // Escape closes an offer (and goes no level up while one is open).
        if (e.key === 'Escape' && (offer || drag)) { e.stopPropagation(); setOffer(null); setDrag(null); }
      }}>
        <div className={'g-plane' + (drag ? ' dragging' : '')} ref={plane} style={{ width: layout.width, height: layout.height }}
          onClick={(e) => { if (e.target === e.currentTarget && !offer) onOpen(''); }}>
          {layout.lane && (
            <div className="g-lane" style={{ top: layout.lane.y }}>
              <span className="g-lanelabel">no route reaches these</span>
            </div>)}
          <svg className="g-edges" width={layout.width} height={layout.height} aria-hidden="true">
            <defs>
              <marker id="g-arrow" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="7" markerHeight="7" orient="auto">
                <path d="M0,0 L8,4 L0,8 z" className="g-arrowhead" />
              </marker>
            </defs>
            {layout.edges.map((e) => {
              const t = edgeTone(e);
              const m = edgeMark(e);
              return (
                <path key={e.id} d={e.d} markerEnd="url(#g-arrow)"
                  className={'g-edge v-' + e.via + (e.dashed ? ' dashed' : '') + (t ? ' ' + t : '') + (m ? ' m-' + m : '')
                    + (dim(e.id, true) ? ' dim' : '') + (lit(e.id) || e.units.includes(open) ? ' on' : '')} />
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
                className={'g-label v-' + e.via + (t ? ' ' + t : '') + (dim(e.id, true) ? ' dim' : '') + (lit(e.id) ? ' on' : '')}
                style={{ left: e.sx + 6, top: e.sy - 7, maxWidth: SIZE.gapX - 10 }}
                title={e.title} onClick={() => onOpen(e.unit)}
                onMouseEnter={() => onHover && onHover(e.from)} onMouseLeave={() => onHover && onHover('')}>
                <span className="g-labeltext mono">{e.label}</span>{e.more > 0 && <span className="g-more">+{e.more}</span>}
              </button>
            );
          })}
          {[...layout.nodes.values()].map((n) => (
            <GraphNode key={n.id} n={n} spec={spec} pins={pinByName} tone={tone(n.id)}
              findings={(groups.objects.get(n.id) || []).length} mark={n.removed ? 'remove' : marks && marks.get(n.id)}
              on={open === n.id} dim={dim(n.id)} lit={!!(hover && near && near.nodes.has(n.id))}
              target={drag && drag.hit && drag.hit.id === n.id ? (drag.action ? 'ok' : 'no') : ''}
              onOpen={onOpen} onHover={onHover} drawable={!!edit} onPointerDown={startDrag} onPointerMove={moveDrag} onPointerUp={endDrag} onPick={pick} />
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
      <SecretList graph={graph} pins={pins} marks={marks} open={open} onOpen={onOpen} onHover={onHover || (() => {})} onAdd={edit ? onAddSecret : null} />
    </div>
  );
}

// Markers are what a node holds that is no edge: its secrets and its
// registry login.
function Markers({ n, pins }) {
  const secretPins = n.secrets.map((s) => pins.get(s)).filter(Boolean);
  const loginPins = n.logins.map((s) => pins.get(s)).filter(Boolean);
  const missing = [...secretPins, ...loginPins].some((p) => p.missing);
  const newer = secretPins.some((p) => p.newer);
  if (!n.secrets.length && !n.logins.length && !n.ports.length) return null;
  return (
    <span className="g-markers small">
      {n.secrets.length > 0 && (
        <span className={'g-marker' + (missing ? ' bad' : newer ? ' warn' : '')} title={`secrets: ${n.secrets.join(', ')}`
          + (missing ? '\none has no value yet' : newer ? '\na newer version than the pin exists' : '')}>
          <Icon name="lock" size={11} />{n.secrets.length} secret{n.secrets.length === 1 ? '' : 's'}</span>)}
      {n.logins.length > 0 && (
        <span className="g-marker" title={`registry login: secret ${n.logins.join(', ')}, read at its latest version whenever a pod is created`}>
          <Icon name="key" size={11} /></span>)}
      <span className="spacer" />
      {n.ports.length > 0 && <span className="g-ports mono muted" title={n.ports.map((p) => `${p.name || 'port'} ${p.port}`).join(', ')}>
        {n.ports.map((p) => p.port).join(' ')}</span>}
    </span>
  );
}

function GraphNode({ n, spec, pins, tone, findings, mark, on, dim, lit, target, onOpen, onHover, drawable = true, onPointerDown, onPointerMove, onPointerUp, onPick }) {
  const opens = !n.missing && !n.removed && n.key !== 'entry' && n.key !== 'sandbox';
  const cls = 'g-node k-' + n.key + (tone ? ' ' + tone : '') + (mark ? ' m-' + mark : '') + (on ? ' on' : '') + (dim ? ' dim' : '')
    + (lit ? ' lit' : '') + (n.missing ? ' missing' : '') + (n.removed ? ' removed' : '') + (target ? ' t-' + target : '');
  const kind = n.key === 'addons' && n.type ? n.type : n.key === 'entry' ? (n.public ? 'internet' : 'route door') : KIND[n.key];
  const head = (
    <>
      <span className="g-head">
        <span className="g-kind">{kind}</span>
        {mark && <span className={'g-mark m-' + mark} title={MARK[mark][1] + ' by this draft'}>{MARK[mark][0]}</span>}
        {findings > 0 && <span className={'badge ' + (tone === 'error' ? 'bad' : tone === 'warning' ? 'warn' : '')}>{findings}</span>}
      </span>
      <span className="mono strong g-name" title={n.name}>{n.key === 'entry' ? (n.public ? 'public' : 'not public') : n.name}</span>
      {n.key !== 'entry' && nodeLine(n, spec) && <span className="g-sub muted small mono" title={nodeLine(n, spec)}>{nodeLine(n, spec)}</span>}
      {(n.key === 'services' || n.key === 'jobs' || n.key === 'bindings') && !n.missing && !n.removed && <Markers n={n} pins={pins} />}
    </>
  );
  return (
    <div className={cls} style={{ left: n.x, top: n.y, width: n.w, height: n.h }}
      onMouseEnter={() => onHover && onHover(n.id)} onMouseLeave={() => onHover && onHover('')}>
      {opens
        ? <button type="button" className="g-body" onClick={() => onOpen(n.id)} title={`open ${KIND[n.key]} ${n.name}`}>{head}</button>
        : <div className="g-body">{head}</div>}
      {n.key === 'entry' && (
        <span className="g-hosts">
          {(n.hosts.length ? n.hosts : [`/api/v1/environments/${n.env || '{env}'}/http/…`]).map((h) => (
            <span key={h} className="g-host mono small" title={h}>{h}</span>))}
        </span>)}
      {drawable && canDrag(n) && (
        <button type="button" className="g-handle" style={{ top: n.h / 2 - 7 }}
          title={n.key === 'entry' ? 'drag onto a service: a route' : 'drag onto an addon, a service or a binding: a reference'}
          aria-label={n.key === 'entry' ? 'add a route' : `add a reference from ${n.name}`}
          onPointerDown={(e) => onPointerDown(e, n.id)} onPointerMove={onPointerMove} onPointerUp={onPointerUp}
          onPointerCancel={onPointerUp} onClick={() => onPick(n.id)} />)}
    </div>
  );
}

// SecretList is the draft's secrets beside the graph, where one sets
// values: each with its pin and latest version, who uses it, and a mark
// when it is missing or newer than its pin. Hovering one highlights its
// users.
export function SecretList({ graph, pins, marks, open, onOpen, onHover, onAdd }) {
  const pinByName = new Map((pins || []).map((p) => [p.name, p]));
  return (
    <div className="g-secrets">
      <span className="sect"><Icon name="lock" size={12} /> secrets <span className="count">· {graph.secrets.length}</span></span>
      {graph.secrets.length === 0 && <span className="muted small">none: a variable or a registry login names one</span>}
      {graph.secrets.map((s) => {
        const pin = pinByName.get(s.name);
        const u = unitKey('secrets', s.name);
        const m = marks && marks.get(u);
        const login = s.users.length > 0 && s.users.every((x) => x.login);
        const users = [...new Set(s.users.map((x) => x.id.slice(x.id.indexOf('/') + 1)))];
        return (
          <button key={s.name} type="button" className={'g-secret' + (open === u ? ' on' : '') + (pin && pin.missing ? ' bad' : '')}
            onClick={() => onOpen(u)} onMouseEnter={() => onHover(u)} onMouseLeave={() => onHover('')}
            title={users.length ? `${login ? 'registry login of' : 'used by'} ${users.join(', ')}` : 'used by nothing in the draft'}>
            {login ? <Icon name="key" size={11} /> : null}
            <span className="mono">{s.name}</span>
            {m && <span className={'g-mark m-' + m}>{MARK[m][0]}</span>}
            {pin && pin.missing && <span className="badge bad">set a value</span>}
            {pin && pin.newer && <span className="badge warn">v{pin.latest} newer</span>}
            {pin && !pin.missing && !pin.newer && <span className="muted small">{pin.pinned ? `v${pin.pinned}` : 'latest'}</span>}
            <span className="muted small">· {users.length || 'no'} user{users.length === 1 ? '' : 's'}</span>
          </button>
        );
      })}
      {onAdd && <button type="button" className="ghost mini" onClick={onAdd}><Icon name="plus" size={12} /> secret</button>}
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
        {offer.pick.length === 0 && <p className="muted small">Nothing to reach yet: add {from.key === 'entry' ? 'a service with a port' : 'an addon or another service'}.</p>}
        <div className="g-picks">
          {offer.pick.map((t, i) => {
            const n = layout.nodes.get(t.id);
            return (
              <button key={i} type="button" className="ghost mini" autoFocus={i === 0} onClick={() => onChoose(t.action)}>
                {KIND[n.key]} <span className="mono">{n.name}</span>
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

export { ENTRY, graphOf };
