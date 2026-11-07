// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { ErrBox } from '../components.jsx';
import { envPath } from '../routes.js';
import { queryOf } from '../logs.js';
import {
  attrValue, barOf, bindingOf, clockOfUs, criticalByService, criticalPath, firstFolds, foldLevel, fromJaeger, isCall,
  offsetText, selfUs, serviceHues, shownRows, spanRows, spanTitle, usText, waitParts, whereOf,
} from '../traces.js';
import { useTimeZone } from './LogSearch.jsx';

// A trace of an environment (ADR 0026 §10), opened from a log line: its
// spans as a waterfall, each span a row, its service and name above a bar placed by
// its start and duration in the trace, the bars sharing one track so that
// they line up however deep a span sits, the tree's lines in a gutter
// beside it; spans fold, each service has its colour, a bar is solid where
// its span works and hatched where it waits, and the critical path is
// summed up by service above them. A
// row opened shows the span's attributes, events, links and resource. A
// trace link in the Logs tab opens it here, and the trace leads back to
// its lines there. There is no list of traces: searching them made the
// store scan for seconds, and a trace is wanted from a line anyway. What
// a span carries is the application's own: text, never
// markup.

// useRead reads what fn answers once per key, and again on reload.
function useRead(key, fn) {
  const [s, setS] = useState({ key: '', data: null, error: null });
  const [n, setN] = useState(0);
  const ref = useRef(key);
  ref.current = key;
  useEffect(() => {
    if (!key) return;
    const asked = key;
    setS({ key: asked, data: null, error: null });
    fn()
      .then((data) => ref.current === asked && setS({ key: asked, data, error: null }))
      .catch((error) => ref.current === asked && setS({ key: asked, data: null, error }));
  }, [key, n]); // eslint-disable-line react-hooks/exhaustive-deps
  const reload = useCallback(() => setN((x) => x + 1), []);
  return s.key === key ? { ...s, reload } : { data: null, error: null, reload };
}

function Problem({ error }) {
  if (error && error.status === 404) {
    return (
      <div className="message">
        <Icon name="alert" size={14} />
        <span><strong>The store has no span of this trace in this environment.</strong>
          <span className="muted"> Spans arrive a few seconds after a request; the lines of the trace may still be there.</span></span>
      </div>
    );
  }
  return <ErrBox error={(error && error.message) || String(error)} />;
}

function Attrs({ list }) {
  if (!list || !list.length) return null;
  return (
    <dl className="tr-attrs">
      {list.map((a, i) => (
        <div key={`${a.key}:${i}`}>
          <dt>{a.key}</dt>
          <dd>{attrValue(a)}</dd>
        </div>
      ))}
    </dl>
  );
}

function SpanDetail({ span, trace, self, critical, onTrace }) {
  const [resource, setResource] = useState(false);
  return (
    <div className="tr-detail">
      <p className="tr-facts">
        {span.kind && <span><span className="k">kind</span> {span.kind}</span>}
        {bindingOf(span) && (
          <span title="the platform sent it on with the binding's credential (ADR 0028)">
            <span className="k">through the binding</span> <span className="mono">{bindingOf(span)}</span>
          </span>
        )}
        <span><span className="k">starts</span> {offsetText(span.startUs, trace.startUs)}</span>
        <span><span className="k">takes</span> {usText(span.durationUs)}</span>
        <span title="the time it spent in none of its own spans"><span className="k">itself</span> {usText(self)}</span>
        {critical > 0 && (
          <span title="the time it alone held the request up"><span className="k">on the critical path</span> {usText(critical)}</span>
        )}
        <span><span className="k">span</span> <span className="mono">{span.spanId}</span></span>
      </p>
      {span.error && <p className="tr-failed">{span.statusMessage ? `Failed: ${span.statusMessage}` : 'Failed.'}</p>}
      {span.attributes.length > 0 && (
        <section>
          <h4 className="tr-h">attributes</h4>
          <Attrs list={span.attributes} />
        </section>
      )}
      {span.events.length > 0 && (
        <section>
          <h4 className="tr-h">events</h4>
          <ol className="tr-events">
            {span.events.map((ev, i) => (
              <li key={i}>
                <span className="k">{offsetText(ev.timeUs, trace.startUs)}</span> <b>{ev.name || 'event'}</b>
                <Attrs list={ev.attributes} />
              </li>
            ))}
          </ol>
        </section>
      )}
      {span.links.length > 0 && (
        <section>
          <h4 className="tr-h">follows from</h4>
          <ul className="tr-links">
            {span.links.map((l) => (
              <li key={`${l.traceId}:${l.spanId}`}>
                {l.traceId === trace.traceId
                  ? <span className="mono">span {l.spanId}</span>
                  : <button type="button" className="linkbtn mono" onClick={() => onTrace(l.traceId)} title="open that trace">
                    trace {l.traceId.slice(0, 8)}… span {l.spanId}</button>}
              </li>
            ))}
          </ul>
        </section>
      )}
      {span.resource.length > 0 && (
        <section>
          <button type="button" className="lg-asis" aria-expanded={resource} onClick={() => setResource(!resource)}>
            <Icon name={resource ? 'chevronDown' : 'chevronRight'} size={12} /> where it ran ({span.resource.length})
          </button>
          {resource && <Attrs list={span.resource} />}
        </section>
      )}
    </div>
  );
}

// Rails are a row's share of the tree's lines: a line on past the row for
// each level above it whose span has children further down, the bend into
// the row from its parent, and a line down to its own children while they
// show. The lines of the span the pointer is on light up.
function Rails({ row, open, step, hover }) {
  const lit = (id) => (id === hover ? ' lit' : '');
  const lines = [];
  row.rails.forEach((on, l) => {
    if (on) lines.push(<i key={l} className={`tr-rail${lit(row.path[l])}`} style={{ left: l * step }} />);
  });
  if (row.depth > 0) {
    lines.push(<i key="in" className={`tr-rail ${row.last ? 'end' : 'tee'}${row.children ? '' : ' leaf'}${lit(row.up)}`}
      style={{ left: (row.depth - 1) * step }} />);
  }
  if (open) lines.push(<i key="down" className={`tr-rail down${lit(row.span.spanId)}`} style={{ left: row.depth * step }} />);
  return <span className="tr-rails" aria-hidden="true">{lines}</span>;
}

// LABEL_GAP is the room between a row's fold button and its name, in px.
const LABEL_GAP = 4;

// hue is the class that colours a service's marks.
const hue = (hues, service) => `tr-c${hues.has(service) ? hues.get(service) : 0}`;

// The waterfall: the spans as a tree of rows, each its name above a bar
// on a track every row shares; the tree's lines and fold buttons sit in a
// gutter as wide as the trace is deep. A span's colour is its service's,
// a request out or a message sent has an arrow before its name, and a
// folded span shows its children's bars inside its own, as Jaeger draws
// them. A bar is solid where its span works itself and hatched where it
// waits: on its own spans, or, a call without one, on what the trace does
// not see (waitParts). A trace opens with
// about FOLD_BUDGET rows (firstFolds): every failure, then the longest
// spans. The arrow keys walk the tree.
function Waterfall({ trace, rows, path, hues, onTrace }) {
  const [folded, setFolded] = useState(() => firstFolds(rows));
  const [open, setOpen] = useState(() => new Set());
  const [hover, setHover] = useState('');
  const list = useRef(null);
  const kids = useMemo(() => {
    const m = new Map();
    for (const r of rows) if (r.up) m.set(r.up, [...(m.get(r.up) || []), r.span]);
    return m;
  }, [rows]);
  const byId = useMemo(() => new Map(rows.map((r) => [r.span.spanId, r])), [rows]);
  const shown = shownRows(rows, folded);
  const depth = rows.reduce((d, r) => Math.max(d, r.depth), 0);
  const step = Math.max(8, Math.min(14, Math.floor(160 / (depth + 1))));
  const gutter = (depth + 1) * step;
  const parents = rows.filter((r) => r.children > 0);
  const foldedShown = shown.some((r) => r.children > 0 && folded.has(r.span.spanId));
  const openShown = shown.some((r) => r.children > 0 && !folded.has(r.span.spanId));

  const flip = (set, setter, id) => {
    const next = new Set(set);
    if (next.has(id)) next.delete(id);
    else next.add(id);
    setter(next);
  };
  const focusRow = (id) => {
    const el = list.current && list.current.querySelector(`[data-span="${id}"]`);
    if (el) el.focus();
  };
  const onKey = (e, r, i) => {
    const id = r.span.spanId;
    const isOpen = r.children > 0 && !folded.has(id);
    if (e.key === 'ArrowDown' && shown[i + 1]) focusRow(shown[i + 1].span.spanId);
    else if (e.key === 'ArrowUp' && shown[i - 1]) focusRow(shown[i - 1].span.spanId);
    else if (e.key === 'ArrowRight' && r.children > 0) {
      if (isOpen) focusRow(shown[i + 1].span.spanId);
      else flip(folded, setFolded, id);
    } else if (e.key === 'ArrowLeft') {
      if (isOpen) flip(folded, setFolded, id);
      else if (r.up) focusRow(r.up);
      else return;
    } else return;
    e.preventDefault();
  };

  return (
    <div className="tr-fall" style={{ '--step': `${step}px` }}>
      <div className="tr-tools">
        <span className="muted small">
          {shown.length === rows.length ? `${rows.length} spans` : `${shown.length} of ${rows.length} spans shown`}
        </span>
        <span className="tr-tools-fold">
          <button type="button" className="linkbtn" disabled={!foldedShown} onClick={() => setFolded(foldLevel(rows, folded, true))}>one level more</button>
          <button type="button" className="linkbtn" disabled={!openShown} onClick={() => setFolded(foldLevel(rows, folded, false))}>one level less</button>
          <button type="button" className="linkbtn" disabled={folded.size === 0} onClick={() => setFolded(new Set())}>expand all</button>
          <button type="button" className="linkbtn" disabled={parents.every((r) => folded.has(r.span.spanId))}
            onClick={() => setFolded(new Set(parents.map((r) => r.span.spanId)))}>collapse all</button>
        </span>
      </div>
      <div className="tr-axis" aria-hidden="true" style={{ marginLeft: gutter }}>
        <span>0</span>
        <span>{usText(trace.durationUs / 4)}</span>
        <span>{usText(trace.durationUs / 2)}</span>
        <span>{usText((trace.durationUs * 3) / 4)}</span>
        <span>{usText(trace.durationUs)}</span>
      </div>
      <ol className="tr-spans" ref={list} onMouseLeave={() => setHover('')}>
        {shown.map((r, i) => {
          const s = r.span;
          const id = s.spanId;
          const { left, width } = barOf(s, trace);
          const on = open.has(id);
          const isFolded = r.children > 0 && folded.has(id);
          const where = whereOf(s);
          const above = byId.get(r.up);
          const title = spanTitle(s);
          const lit = hover && (id === hover || r.path.includes(hover));
          return (
            <li key={id} className={`tr-span${s.error ? ' bad' : ''}${on ? ' on' : ''}${lit ? ' lit' : ''}`} onMouseEnter={() => setHover(id)}>
              <Rails row={r} open={r.children > 0 && !isFolded} step={step} hover={hover} />
              {r.children > 0 && (
                <button type="button" className="tr-fold" style={{ left: r.depth * step + step / 2 - 10 }} aria-expanded={!isFolded}
                  aria-label={isFolded ? `show the ${r.below} spans below ${title}` : `fold the spans below ${title}`}
                  title={isFolded ? `show the ${r.below} spans below` : 'fold the spans below'} onClick={() => flip(folded, setFolded, id)}>
                  <Icon name={isFolded ? 'chevronRight' : 'chevronDown'} size={12} />
                </button>
              )}
              <button type="button" className="tr-row" data-span={id} aria-expanded={on} onClick={() => flip(open, setOpen, id)}
                onKeyDown={(e) => onKey(e, r, i)}
                title={`${where} · ${title} · ${offsetText(s.startUs, trace.startUs)} · ${usText(s.durationUs)}${r.orphan ? ' · its parent is not in the trace' : ''}`}>
                <span className="tr-label" style={{ paddingLeft: (r.depth + 1) * step + LABEL_GAP }}>
                  <i className={`tr-dot ${hue(hues, s.service)}`} />
                  {s.error && <Icon name="alert" size={12} />}
                  {(!above || whereOf(above.span) !== where) && <span className="tr-svc">{where}</span>}
                  <span className="tr-name">{isCall(s) && <span className="tr-call">→ </span>}{title}</span>
                  {bindingOf(s) && <span className="tr-binding" title={`a request out through the binding ${bindingOf(s)}`}>binding</span>}
                  {isFolded && <span className="tr-more">+{r.below}</span>}
                  <span className="tr-dur">{usText(s.durationUs)}</span>
                </span>
                <span className="tr-track" style={{ marginLeft: gutter }}>
                  <span className={`tr-bar ${hue(hues, s.service)}`} style={{ left: `${left}%`, width: `${width}%` }} />
                  {isFolded && (kids.get(id) || []).map((c) => {
                    const b = barOf(c, trace);
                    return <span key={c.spanId} className={`tr-sub ${hue(hues, c.service)}`} style={{ left: `${b.left}%`, width: `${b.width}%` }} />;
                  })}
                  {!isFolded && waitParts(s, kids.get(id)).map(([a, z]) => {
                    const b = barOf({ startUs: a, durationUs: z - a }, trace);
                    return <span key={a} className={`tr-wait ${hue(hues, s.service)}`} style={{ left: `${b.left}%`, width: `${b.width}%` }} />;
                  })}
                </span>
              </button>
              {on && (
                <div style={{ paddingLeft: Math.min((r.depth + 1) * step, gutter) }}>
                  <SpanDetail span={s} trace={trace} self={selfUs(s, kids.get(id))}
                    critical={(path.get(id) || []).reduce((n, [a, z]) => n + z - a, 0)} onTrace={onTrace} />
                </div>
              )}
            </li>
          );
        })}
      </ol>
    </div>
  );
}

// CriticalPath says where the request's time went: how long each service
// alone held it up, a bar of their shares and their names.
function CriticalPath({ rows, path, hues }) {
  const by = criticalByService(rows, path);
  const total = by.reduce((n, [, us]) => n + us, 0);
  if (!total) return null;
  return (
    <section className="tr-path">
      <p className="tr-path-head">
        <span className="k">critical path</span> <b>{usText(total)}</b>
        <span className="tr-key">the time each service alone held the request up</span>
      </p>
      <div className="tr-share" aria-hidden="true">
        {by.map(([svc, us]) => <span key={svc} className={hue(hues, svc)} style={{ flexGrow: us }} title={`${svc} · ${usText(us)}`} />)}
      </div>
      <ul className="tr-legend">
        {by.map(([svc, us]) => (
          <li key={svc}>
            <i className={`tr-dot ${hue(hues, svc)}`} />
            {svc} <span className="tr-legend-us">{usText(us)}</span>
            <span className="muted">{Math.round((us / total) * 100)} %</span>
          </li>
        ))}
      </ul>
    </section>
  );
}

// envServices are the names of the environment's services, which keep
// their colours from trace to trace.
const envServices = (st) => ((st && st.objects) || []).filter((o) => o.kind === 'service').map((o) => o.name);

function TraceView({ st, trace, utc, onTrace, onLines }) {
  const { data: raw, error } = useRead(st ? `${st.id}/${trace}` : '', () => api.getTrace(st.id, trace));
  const data = useMemo(() => {
    const t = raw && raw.data && raw.data[0];
    return t && t.spans && t.spans.length ? fromJaeger(t) : null;
  }, [raw]);
  const services = data ? [...new Set(data.spans.map((s) => s.service))] : [];
  const errors = data ? data.spans.filter((s) => s.error).length : 0;
  const rows = useMemo(() => spanRows(data ? data.spans : []), [data]);
  const path = useMemo(() => criticalPath(rows), [rows]);
  const hues = useMemo(() => serviceHues(envServices(st), data ? data.spans : []), [st, data]);
  const none = raw && !data;
  return (
    <div className="tr">
      <div className="lg-tracebar tr-head">
        <span className="k">trace</span> <code>{trace}</code>
        {data && (
          <span className="tr-facts">
            <span><span className="k">at</span> {clockOfUs(data.startUs, utc)}</span>
            <span><span className="k">takes</span> {usText(data.durationUs)}</span>
            <span>{data.spans.length} {data.spans.length === 1 ? 'span' : 'spans'}{errors > 0 && <b className="bad"> · {errors} failed</b>}</span>
            <span className="tr-services">
              {services.map((s) => <span key={s}><i className={`tr-dot ${hue(hues, s)}`} />{s}</span>)}
            </span>
          </span>
        )}
        <span className="spacer" />
        <button type="button" className="linkbtn" onClick={onLines}><Icon name="file" size={12} /> its lines in the logs</button>
      </div>
      {(error || none) && <Problem error={error || { status: 404 }} />}
      {!error && !raw && <p className="muted small">Reading the trace …</p>}
      {data && data.warnings.length > 0 && <p className="muted small">{data.warnings.join(' · ')}</p>}
      {data && <CriticalPath rows={rows} path={path} hues={hues} />}
      {data && <Waterfall key={trace} trace={data} rows={rows} path={path} hues={hues} onTrace={onTrace} />}
    </div>
  );
}

// Traces is a trace (item, its ID) of the environment.
export function Traces({ tenant, env, st, item, navigate }) {
  const [tz] = useTimeZone();
  const onTrace = (trace) => navigate(envPath(tenant, env, 'traces', trace));
  if (!st) return <p className="empty">Loading…</p>;
  return (
    <TraceView st={st} trace={item} utc={tz === 'utc'} onTrace={onTrace}
      onLines={() => navigate(envPath(tenant, env, 'logs', '', null, queryOf({ trace: item })))} />
  );
}
