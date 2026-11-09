// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { Icon } from '../Icon.jsx';
import { deriveRows, eventDescription, keyOf, lanesOf } from '../events.jsx';
import { fmtMsTime, fmtNs } from '../format.js';

// The waterfall timeline of one page of events. Lanes hold spans (runs,
// ops, tool calls) and point markers (state changes, node records,
// traffic). The wheel zooms around the pointer, a drag pans, and the
// presets follow the present.

const MIN_WINDOW_MS = 250; // the tightest zoom
const SUB_ROW_H = 26; // the pitch of one sub-row of a lane
const LANE_BASE_H = 38; // a lane's height with one sub-row

// packRows partitions a lane's rows into sub-rows: rows that overlap in
// time go on different sub-rows, stacked top to bottom. The rows are
// sorted by start, so the greedy earliest-finish packing is the same on
// every refresh. Points take a 1 ms sliver, so that points at the same
// time stack instead of covering each other.
function packRows(laneRows) {
  const ends = [];
  const assign = new Map();
  for (const r of laneRows) {
    const s = r.startMs;
    const e = r.bar ? r.endMs : r.startMs + 1;
    let sub = ends.findIndex((end) => end <= s);
    if (sub === -1) { sub = ends.length; ends.push(e); } else ends[sub] = Math.max(ends[sub], e);
    assign.set(r.id, sub);
  }
  return { n: Math.max(1, ends.length), assign };
}

// packLane packs a lane: each root row and each of its nested sub-rows as
// an entry of its own (a sub-row's id is `${row.id}|${sub.id}`), so that a
// nested span gets a sub-row below its parent instead of drawing inside
// it, and no two bars ever overlap on screen. A nested span without an
// extent (a lone opener) packs as a sliver, like a point.
export function packLane(laneRows) {
  const flat = [];
  for (const r of laneRows) {
    flat.push(r);
    for (const s of r.subs || []) {
      flat.push({ id: `${r.id}|${s.id}`, bar: true, startMs: s.startMs, endMs: s.endMs ?? s.startMs + 1 });
    }
  }
  flat.sort((a, b) => a.startMs - b.startMs);
  return packRows(flat);
}

function niceStep(rangeMs, target = 6) {
  const steps = [5, 10, 25, 50, 100, 250, 500, 1000, 2000, 5000, 10000, 15000, 30000,
    60000, 120000, 300000, 600000, 900000, 1800000, 3600000, 7200000, 21600000, 86400000];
  const want = rangeMs / target;
  for (const s of steps) if (s >= want) return s;
  return steps[steps.length - 1];
}

const pad = (n) => String(n).padStart(2, '0');

function fmtTick(ms, step) {
  const d = new Date(ms);
  if (step >= 86400000) return `${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
  if (step >= 60000) return `${pad(d.getHours())}:${pad(d.getMinutes())}`;
  if (step >= 1000) return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
  return fmtMsTime(ms);
}

const PRESETS = [
  { label: '30 s', ms: 30e3 },
  { label: '2 min', ms: 120e3 },
  { label: '10 min', ms: 600e3 },
  { label: '1 h', ms: 3600e3 },
  { label: 'all', ms: 0 },
];

export function Timeline({ events, selected, onSelect }) {
  const rows = useMemo(() => deriveRows(events), [events]);
  const [tip, setTip] = useState(null); // {x, y, node}: the hover card
  const showTip = (e, node) => setTip({ x: e.clientX, y: e.clientY, node });
  const hideTip = () => setTip(null);
  const laneDefs = useMemo(() => lanesOf(events), [events]);
  const lanes = useMemo(() => {
    const out = {};
    for (const lane of laneDefs) out[lane.key] = packLane(rows.filter((r) => r.lane === lane.key));
    return out;
  }, [rows, laneDefs]);
  const [win, setWin] = useState(null); // [startMs, endMs]; null: fit
  const [follow, setFollow] = useState(false);
  const [followMs, setFollowMs] = useState(120e3);
  const bodyRef = useRef(null);
  // The axis's track: as wide as the lanes' tracks, right of their labels;
  // the pointer's place in time is measured on it.
  const trackRef = useRef(null);
  const drag = useRef(null);
  const lastMoved = useRef(false);

  const dataMin = rows.length ? rows[0].startMs : Date.now() - 1000;
  const dataMax = rows.reduce((m, r) => Math.max(m, r.endMs || r.startMs), dataMin + 1);

  const view = win || [dataMin - Math.max(50, (dataMax - dataMin) * 0.04),
    dataMax + Math.max(50, (dataMax - dataMin) * 0.04)];
  const dur = Math.max(1, view[1] - view[0]);

  // Following the present: the window moves with every new page.
  useEffect(() => {
    if (follow) setWin([Date.now() - followMs, Date.now() + 250]);
  }, [follow, followMs, events]);

  const zoomAt = (fraction, factor) => {
    setFollow(false);
    const anchor = view[0] + fraction * dur;
    const ns = Math.max(anchor - (anchor - view[0]) * factor, dataMin - (dataMax - dataMin));
    const ne = anchor + (view[1] - anchor) * factor;
    if (ne - ns < MIN_WINDOW_MS) return;
    setWin([ns, ne]);
  };

  // A non-passive wheel listener, so that preventDefault keeps the page
  // from scrolling.
  useEffect(() => {
    const el = bodyRef.current;
    if (!el) return undefined;
    const onWheel = (e) => {
      e.preventDefault();
      hideTip();
      const rect = (trackRef.current || el).getBoundingClientRect();
      zoomAt(Math.min(1, Math.max(0, (e.clientX - rect.left) / rect.width)), e.deltaY > 0 ? 1.25 : 0.8);
    };
    el.addEventListener('wheel', onWheel, { passive: false });
    return () => el.removeEventListener('wheel', onWheel);
  });

  const onPointerDown = (e) => {
    if (e.button !== 0) return;
    hideTip();
    drag.current = { x: e.clientX, win: view, moved: false };
    e.currentTarget.setPointerCapture(e.pointerId);
  };
  const onPointerMove = (e) => {
    if (!drag.current) return;
    const dx = e.clientX - drag.current.x;
    if (Math.abs(dx) > 3) drag.current.moved = true;
    const pxPerMs = (trackRef.current || bodyRef.current).clientWidth / dur;
    const shift = -dx / pxPerMs;
    setFollow(false);
    setWin([drag.current.win[0] + shift, drag.current.win[1] + shift]);
  };
  const onPointerUp = () => { lastMoved.current = !!drag.current?.moved; drag.current = null; };
  const onClickEmpty = () => { if (!lastMoved.current) onSelect(null); lastMoved.current = false; };

  const toPct = (t) => ((t - view[0]) / dur) * 100;

  const ticks = useMemo(() => {
    const step = niceStep(dur);
    const out = [];
    for (let t = Math.ceil(view[0] / step) * step; t <= view[1]; t += step) {
      out.push({ t, pct: toPct(t), label: fmtTick(t, step) });
    }
    return out;
  }, [view[0], view[1], dur]);

  const now = Date.now();
  const nowPct = toPct(now);

  const setPreset = (ms) => {
    if (ms === 0) { setWin(null); setFollow(false); return; }
    setFollowMs(ms);
    setFollow(true);
    setWin([Date.now() - ms, Date.now() + 250]);
  };

  const selK = selected ? keyOf(selected) : null;

  return (
    <div className="tl">
      <div className="tl-bar">
        <button className="ghost mini icon-only" title="zoom in" onClick={() => zoomAt(0.5, 0.5)}><Icon name="plus" size={13} /></button>
        <button className="ghost mini icon-only" title="zoom out" onClick={() => zoomAt(0.5, 2)}><span className="minus">−</span></button>
        <button className="ghost mini" onClick={() => setWin(null)}>fit</button>
        <span className="vsep" />
        {PRESETS.map((p) => (
          <button key={p.label}
            className={'ghost mini' + ((follow && followMs === p.ms && p.ms !== 0) || (p.ms === 0 && !win) ? ' on' : '')}
            onClick={() => setPreset(p.ms)}>{p.label}</button>
        ))}
        <span className="spacer" />
        <span className="tl-status muted">{fmtMsTime(view[0])} → {fmtMsTime(view[1])}</span>
      </div>

      <div className="tl-body" ref={bodyRef}
        onPointerDown={onPointerDown} onPointerMove={onPointerMove}
        onPointerUp={onPointerUp} onClick={onClickEmpty}>
        <div className="tl-axis">
          <div className="tl-axis-label" />
          <div className="tl-axis-track" ref={trackRef}>
            {ticks.map((tk) => (
              <span key={tk.t} className="tl-tick" style={{ left: tk.pct + '%' }}>{tk.label}</span>
            ))}
          </div>
        </div>
        <div className="tl-lanes">
          {laneDefs.map((lane) => {
            const pk = lanes[lane.key] || { n: 1, assign: new Map() };
            return (
              <div className="tl-lane" key={lane.key}>
                <div className="tl-lane-label">{lane.label}</div>
                <div className="tl-lane-track" style={{ height: (LANE_BASE_H + (pk.n - 1) * SUB_ROW_H) + 'px' }}>
                  {ticks.map((tk) => (
                    <span key={tk.t} className="tl-gridline" style={{ left: tk.pct + '%' }} />
                  ))}
                  {rows.filter((r) => r.lane === lane.key && (r.endMs ?? r.startMs) >= view[0] && r.startMs <= view[1])
                    .map((r) => {
                      // A bar is selected when one of its members is.
                      const isSel = !!selK && r.members.some((m) => keyOf(m) === selK);
                      const sub = pk.assign.get(r.id) || 0;
                      const left = toPct(r.startMs);
                      const hover = {
                        onMouseEnter: (e) => showTip(e, tipRow(r)),
                        onMouseMove: (e) => showTip(e, tipRow(r)),
                        onMouseLeave: hideTip,
                      };
                      if (r.bar) {
                        const width = Math.max(0.4, toPct(Math.min(r.endMs, view[1])) - left);
                        const own = ownBand(r);
                        return (
                          <button key={r.id} {...hover}
                            className={'tl-span' + (r.err ? ' err' : '') + (r.open ? ' open' : '') + (isSel ? ' sel' : '')}
                            style={{ left: Math.max(-1, left) + '%', width: width + '%', top: (9 + sub * SUB_ROW_H) + 'px' }}
                            onClick={(e) => { hideTip(); e.stopPropagation(); onSelect(isSel ? null : r.pick); }}>
                            {own && <span className="tl-own" style={own} />}
                            {width > 7 && <span className="tl-span-label">{r.title || r.tool || ''}</span>}
                          </button>
                        );
                      }
                      return (
                        <button key={r.id} {...hover}
                          className={'tl-mark' + (r.err ? ' err' : '') + (r.open ? ' hollow' : '') + (isSel ? ' sel' : '')}
                          style={{ left: left + '%', top: (13 + sub * SUB_ROW_H) + 'px' }}
                          onClick={(e) => { hideTip(); e.stopPropagation(); onSelect(isSel ? null : r.ev); }} />
                      );
                    })}
                  {/* the in-lane subtree of each row (ops under a run,
                      markers in a span): sub-rows below the parent bar,
                      stepped by depth, never drawn inside it */}
                  {rows.filter((r) => r.lane === lane.key && r.subs.length)
                    .flatMap((r) => r.subs.map((s) => {
                      const sEnd = s.endMs ?? s.startMs;
                      if (sEnd < view[0] || s.startMs > view[1]) return null;
                      const isSel = !!selK && s.members.some((m) => keyOf(m) === selK);
                      const sub = pk.assign.get(`${r.id}|${s.id}`) ?? 0;
                      const left = toPct(s.startMs);
                      const key = `${r.id}|${s.id}`;
                      const hover = {
                        onMouseEnter: (e) => showTip(e, tipRow(s)),
                        onMouseMove: (e) => showTip(e, tipRow(s)),
                        onMouseLeave: hideTip,
                        onClick: (e) => { hideTip(); e.stopPropagation(); onSelect(isSel ? null : s.pick); },
                      };
                      if (s.endMs == null) {
                        return (
                          <button key={key} {...hover}
                            className={'tl-mark tl-submark' + (s.err ? ' err' : '') + (s.open ? ' hollow' : '') + (isSel ? ' sel' : '')}
                            style={{ left: left + '%', top: (14 + sub * SUB_ROW_H) + 'px' }} />
                        );
                      }
                      const width = Math.max(0.4, toPct(Math.min(sEnd, view[1])) - left);
                      return (
                        <button key={key} {...hover}
                          className={'tl-span tl-subspan' + depthClass(s) + (s.err ? ' err' : '') + (s.open ? ' open' : '') + (isSel ? ' sel' : '')}
                          style={{ left: Math.max(-1, left) + '%', width: width + '%', top: (12 + sub * SUB_ROW_H) + 'px' }}>
                          {width > 7 && <span className="tl-span-label">{s.title}</span>}
                        </button>
                      );
                    }))}
                  {nowPct >= 0 && nowPct <= 100 && (follow || !win || view[1] >= now) && (
                    <span className="tl-now" style={{ left: nowPct + '%' }} />
                  )}
                </div>
              </div>
            );
          })}
        </div>
      </div>
      <div className="tl-hint muted">scroll = zoom · drag = pan · click = inspect</div>
      {tip && <TipCard x={tip.x} y={tip.y}>{tip.node}</TipCard>}
    </div>
  );
}

// TIP_GAP is the hover card's distance from the pointer, TIP_EDGE the
// least room it keeps to the window's edge.
const TIP_GAP = { x: 14, y: 16 };
const TIP_EDGE = 8;

// tipPlace is where a hover card of w×h goes for the pointer at (x, y)
// in a window of vw×vh: right of and below the pointer, left of it where
// it would leave the window on the right, above it where it would leave
// it at the bottom, and never past an edge.
export function tipPlace(x, y, w, h, vw, vh) {
  let left = x + TIP_GAP.x;
  if (left + w > vw - TIP_EDGE) left = x - TIP_GAP.x - w;
  let top = y + TIP_GAP.y;
  if (top + h > vh - TIP_EDGE) top = y - TIP_GAP.y - h;
  return {
    left: Math.max(TIP_EDGE, Math.min(left, vw - TIP_EDGE - w)),
    top: Math.max(TIP_EDGE, Math.min(top, vh - TIP_EDGE - h)),
  };
}

// TipCard is the hover card at the pointer (x, y), measured before it is
// painted and placed by tipPlace.
function TipCard({ x, y, children }) {
  const ref = useRef(null);
  const [pos, setPos] = useState({ left: x + TIP_GAP.x, top: y + TIP_GAP.y });
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    const r = el.getBoundingClientRect();
    const p = tipPlace(x, y, r.width, r.height, window.innerWidth, window.innerHeight);
    setPos((cur) => (cur.left === p.left && cur.top === p.top ? cur : p));
  }, [x, y, children]);
  return <div ref={ref} className="tl-tip" style={pos}>{children}</div>;
}

function fmtMs(ms) {
  if (ms >= 60000) return (ms / 60000).toFixed(1) + ' min';
  if (ms >= 1000) return (ms / 1000).toFixed(1) + ' s';
  return ms.toFixed(1) + ' ms';
}

// ownBand is the span's own extent inside its bar: the bar covers the
// whole subtree, while the span's own members may cover less (a run's
// own opener and closer inside the ops that widen it). In bar-relative
// percent; null when they coincide.
function ownBand(r) {
  if (!r.bar || !(r.endMs > r.startMs) || r.ownStartMs == null) return null;
  if (!(r.ownStartMs > r.startMs || r.ownEndMs < r.endMs)) return null;
  const span = r.endMs - r.startMs;
  return {
    left: ((r.ownStartMs - r.startMs) / span) * 100 + '%',
    width: Math.max(0.5, ((r.ownEndMs - r.ownStartMs) / span) * 100) + '%',
  };
}

// depthClass steps a nested span by its depth; three steps are drawn,
// anything deeper looks like the third.
const depthClass = (s) => ' tl-d' + Math.min(3, Math.max(1, s.depth || 1));

// tipIds are a row's IDs, as shown metadata.
function tipIds(pick) {
  return [pick?.requestId && `req ${pick.requestId.slice(0, 8)}`,
    pick?.spanId && `span …${pick.spanId.slice(-8)}`,
    pick?.parentSpanId && `parent …${pick.parentSpanId.slice(-8)}`,
  ].filter(Boolean).join(' · ');
}

// tipRow is the hover card of a bar or point: the span's title (the name
// of its first member), the closer's name as its outcome, its times.
function tipRow(r) {
  const pick = r.pick || r.ev;
  const desc = eventDescription(r.bar ? r.start : pick);
  const outcome = r.end && r.end !== r.start ? r.end.name : '';
  const band = ownBand(r);
  return (
    <>
      <div className="tl-tip-title">
        {r.title}
        {r.open && <span className="muted"> {r.bar ? '(open: no closer)' : '(no end yet)'}</span>}
        {r.err && <span className="errtext"> — failed</span>}
      </div>
      <div className="muted">
        {r.bar && r.endMs != null
          ? <>{fmtMsTime(r.startMs)} → {fmtMsTime(r.endMs)} · {fmtMs(r.endMs - r.startMs)}</>
          : fmtNs(pick?.tsNs)}
      </div>
      {band && <div className="muted small">own {fmtMs(r.ownEndMs - r.ownStartMs)} · subtree {fmtMs(r.endMs - r.startMs)}</div>}
      {outcome && <div>{outcome}</div>}
      {desc && <div>{desc}</div>}
      {tipIds(pick) && <div className="muted mono small">{tipIds(pick)}</div>}
    </>
  );
}
