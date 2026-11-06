// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { ErrBox } from '../components.jsx';
import { readItem, writeItem } from '../storage.js';
import { envPath } from '../routes.js';
import { logReplicas } from './envs.js';
import {
  LEVELS, SOURCES, TIME_ZONES, clockOf, dayOf, dayText, defaultRange, egressParts, egressTitle, follows, inputOf,
  isEgress, isRequest, keyed, levelsOf, lineLevel, mergeLines, parseSearch, queryOf, rangeOf, rangesOf, reachesNow,
  requestParts, requestTitle, sinkParams, storeDown, streamTag, timeOfInput, toggleLevel, toggleWord, whatOf, wordIn,
} from '../logs.js';

// The Logs tab searches an environment's logs in the log store, ferun-sink
// (ADR 0026): what (every object, one, a replica of it; its output, the
// requests the ingress answered for it, the requests it sent out), words or
// /an expression/, the levels, and the time; the lines below, the newest
// at the bottom, followed live while the view is at its end and the search
// is of one object, earlier ones read in as it scrolls up. A line that is a
// record (JSON, logfmt) shows its message and then its fields as
// key=value; opened, every field, each with what finds the lines that have
// it or lack it, and the line as it came. A line with a trace ID leads to
// every line of that trace, of any object and source. Every part of the
// search is in the address, so that a search is a link.

// A search's first page and each earlier one bring LOG_PAGE lines; a view
// that follows keeps at most LOG_KEEP, the oldest going.
const LOG_PAGE = 300;
const LOG_KEEP = 10000;
const FOLLOW_AGAIN_MS = 5000;

// useTimeZone is the zone the times show in, local or utc: the viewer's
// pick, kept by the browser.
const TZ_KEY = 'ferun.logs.timezone';
export function useTimeZone() {
  const [tz, setTz] = useState(() => (readItem('localStorage', TZ_KEY) === 'utc' ? 'utc' : 'local'));
  const pick = (v) => {
    setTz(v);
    writeItem('localStorage', TZ_KEY, v);
  };
  return [tz, pick];
}

// readNdjson reads the store's follow, an object a line, and hands each
// batch of lines (and the streams they name, and what it skipped) to add.
async function readNdjson(reader, add) {
  const dec = new TextDecoder();
  let buf = '';
  for (;;) {
    const { value, done } = await reader.read();
    if (done) return;
    buf += dec.decode(value, { stream: true });
    const got = [];
    const streams = {};
    let skipped = 0;
    let i;
    while ((i = buf.indexOf('\n')) >= 0) {
      const raw = buf.slice(0, i).trim();
      buf = buf.slice(i + 1);
      if (!raw) continue;
      try {
        const m = JSON.parse(raw);
        if (m.line) got.push(m.line);
        if (m.stream && m.stream.id) streams[m.stream.id] = m.stream;
        if (m.skipped > 0) skipped += m.skipped;
      } catch {
        // Not a line of the store's.
      }
    }
    if (got.length || skipped) add(got, streams, skipped);
  }
}

// useLogSearch reads a search's first page, earlier pages on demand, and,
// while live and the search follows (one object, to now), new lines.
function useLogSearch(envId, focus, live) {
  const key = envId + ' ' + JSON.stringify(focus);
  const empty = { key, params: '', lines: [], streams: {}, cursor: '', trimmed: false, loading: true, error: null, partial: false, skipped: 0 };
  const [s, setS] = useState(empty);
  const keyRef = useRef(key);
  keyRef.current = key;
  const linesRef = useRef(s.lines);
  linesRef.current = s.key === key ? s.lines : [];
  const mine = (asked, fn) => (x) => (x.key === asked && asked === keyRef.current ? fn(x) : x);
  const keep = (x) => (x.lines.length > LOG_KEEP ? { ...x, lines: x.lines.slice(-LOG_KEEP), trimmed: true } : x);

  useEffect(() => {
    if (!envId) return;
    const asked = key;
    // A span back from now is read as a time once; earlier pages go on
    // from the same query.
    const params = sinkParams(focus, envId, { limit: LOG_PAGE }).toString();
    setS({ ...empty, key: asked, params });
    api.searchLogs(params)
      .then((d) => setS(mine(asked, (x) => ({
        ...x, lines: keyed(d.lines), streams: d.streams || {}, cursor: d.cursor || '', partial: !!d.partial, loading: false,
      }))))
      .catch((error) => setS(mine(asked, (x) => ({ ...x, loading: false, error }))));
  }, [key]); // eslint-disable-line react-hooks/exhaustive-deps

  const cur = s.key === key ? s : empty;
  const more = cur.trimmed ? cur.lines.length > 0 : cur.cursor !== '';
  const earlier = useCallback(() => {
    if (cur.loading || !more) return;
    const asked = key;
    const p = new URLSearchParams(cur.params);
    if (cur.trimmed) p.set('until', cur.lines[0].ts || cur.lines[0].time);
    else p.set('cursor', cur.cursor);
    setS(mine(asked, (x) => ({ ...x, loading: true })));
    api.searchLogs(p.toString())
      .then((d) => setS(mine(asked, (x) => ({
        ...x,
        lines: mergeLines(x.lines, keyed(d.lines), 'before'),
        streams: { ...d.streams, ...x.streams },
        cursor: d.cursor || '',
        trimmed: x.trimmed && (d.lines || []).length > 0,
        loading: false,
      }))))
      .catch((error) => setS(mine(asked, (x) => ({ ...x, loading: false, error }))));
  }, [key, cur.loading, more, cur.trimmed, cur.cursor, cur.lines, cur.params]); // eslint-disable-line react-hooks/exhaustive-deps

  const loaded = cur.key === key && !cur.loading && !cur.error && cur.params !== '';
  const following = live && loaded && follows(focus);
  useEffect(() => {
    if (!following) return undefined;
    const asked = key;
    const params = cur.params;
    const ctl = new AbortController();
    const add = (lines, streams, skipped = 0) => setS(mine(asked, (x) => keep({
      ...x,
      lines: mergeLines(x.lines, keyed(lines), 'after'),
      streams: { ...x.streams, ...streams },
      skipped: x.skipped + skipped,
    })));
    (async () => {
      while (!ctl.signal.aborted) {
        try {
          // What came since the last line, before the follow's new ones.
          const last = linesRef.current[linesRef.current.length - 1];
          if (last && last.ts) {
            const p = new URLSearchParams(params);
            p.set('since', last.ts);
            p.set('direction', 'forward');
            p.set('limit', '1000');
            const d = await api.searchLogs(p.toString());
            if (ctl.signal.aborted) return;
            add(d.lines || [], d.streams || {});
          }
          const reader = await api.followLogs(params, ctl.signal);
          await readNdjson(reader, add);
        } catch {
          if (ctl.signal.aborted) return;
        }
        await new Promise((r) => setTimeout(r, FOLLOW_AGAIN_MS));
      }
    })();
    return () => ctl.abort();
  }, [following, key]); // eslint-disable-line react-hooks/exhaustive-deps

  return { ...cur, more, earlier, following, loaded };
}

// useEndAnchor keeps a scrolled log where its reader left it: at its end
// while they are there, so that a log growing at its end follows it;
// elsewhere at the same distance from the end, so that earlier lines read
// in above keep the lines in sight where they were. onScrolled hears, at
// every scroll, whether the view is at the end and whether it is near the
// top.
function useEndAnchor(dep, onScrolled) {
  const box = useRef(null);
  const fromEnd = useRef(0);
  useLayoutEffect(() => {
    const el = box.current;
    if (!el) return;
    el.scrollTop = fromEnd.current < 4 ? el.scrollHeight : el.scrollHeight - el.clientHeight - fromEnd.current;
  }, [dep]);
  const onScroll = (e) => {
    const el = e.currentTarget;
    fromEnd.current = el.scrollHeight - el.clientHeight - el.scrollTop;
    if (onScrolled) onScrolled({ atEnd: fromEnd.current < 4, nearTop: el.scrollTop < 160 });
  };
  return { box, onScroll };
}

// Chips is a row of one choice of several.
function Chips({ label, value, options, onChange, mono }) {
  return (
    <div className="chiprow lg-chips" role="group" aria-label={label}>
      <span className="k">{label}</span>
      {options.map(([v, text]) => (
        <button key={v} type="button" className={'chipbtn' + (mono ? ' mono' : '') + (v === value ? ' on' : '')}
          aria-pressed={v === value} onClick={() => onChange(v)}>{text}</button>
      ))}
    </div>
  );
}

// What picks the object, its replica and the source.
function What({ objects, focus, set }) {
  const o = focus.object ? objects.find((x) => `${x.kind}/${x.name}` === focus.object) : null;
  const replicas = o ? logReplicas(o) : [];
  const kind = o ? o.kind : focus.object ? focus.object.split('/')[0] : '';
  // An addon has output only; a job no requests in; a replica is a
  // container's, so its requests in are none of its own.
  const replica = focus.replica != null;
  const sources = SOURCES.filter(([s]) => (!replica || (s !== '' && s !== 'ingress'))
    && (!kind || kind === 'service' || s === '' || s === 'container' || (kind === 'job' && s === 'egress')));
  const listed = !focus.object || o;
  return (
    <>
      <div className="chiprow lg-chips">
        <span className="k">object</span>
        <button type="button" className={'chipbtn' + (!focus.object ? ' on' : '')} onClick={() => set({ object: undefined, replica: undefined })}>
          every object
        </button>
        {objects.map((x) => {
          const it = `${x.kind}/${x.name}`;
          return (
            <button key={it} type="button" className={'chipbtn mono objchip' + (it === focus.object ? ' on' : '')}
              onClick={() => set({ object: it, replica: undefined, source: undefined })}>
              <span className="muted">{x.kind}/</span>{x.name}
            </button>
          );
        })}
        {!listed && <button type="button" className="chipbtn mono on">{focus.object} <span className="muted">(gone)</span></button>}
      </div>
      {(replicas.length > 1 || focus.replica != null) && (
        <Chips label="replica" mono value={focus.replica == null ? 'all' : String(focus.replica)}
          options={[['all', 'every'], ...replicas.map((r) => [String(r), String(r)])]}
          onChange={(v) => set({ replica: v === 'all' ? undefined : Number(v) })} />
      )}
      {sources.length > 1 && (
        <Chips label="source" value={focus.source || (replica ? 'container' : '')} options={sources}
          onChange={(v) => set({ source: v || undefined })} />
      )}
    </>
  );
}

function Levels({ value, onChange }) {
  const on = new Set(levelsOf(value));
  return (
    <div className="lg-levels" role="group" aria-label="levels">
      {LEVELS.map((l) => (
        <button key={l} type="button" className={`chipbtn mono lg-lvlchip lvl-${l}${on.has(l) ? ' on' : ''}`}
          aria-pressed={on.has(l)} onClick={() => onChange(toggleLevel(value, l))}>{l}</button>
      ))}
    </div>
  );
}

// Range is the search's time, and beside it the zone its times show in.
function Range({ focus, set, tz, onTz }) {
  const utc = tz === 'utc';
  const range = rangeOf(focus);
  const pick = (k) => {
    if (k !== 'custom') {
      set({ since: k === defaultRange(focus) ? undefined : k, until: undefined });
      return;
    }
    // From an hour ago to now, to be moved.
    set({ since: new Date(Date.now() - 3600e3).toISOString(), until: new Date().toISOString() });
  };
  return (
    <div className="lg-range">
      <Chips label="time" value={range} options={rangesOf(focus)} onChange={pick} />
      {range === 'custom' && (
        <div className="lg-fromto">
          <label>
            <span className="k">from{utc && ' (UTC)'}</span>
            <input type="datetime-local" value={inputOf(focus.since, utc)}
              onChange={(e) => e.target.value && set({ since: timeOfInput(e.target.value, utc) })} />
          </label>
          <label>
            <span className="k">to{utc && ' (UTC)'}</span>
            <input type="datetime-local" value={inputOf(focus.until, utc)}
              onChange={(e) => set({ until: timeOfInput(e.target.value, utc) || undefined })} />
          </label>
        </div>
      )}
      <span className="spacer" />
      <Chips label="zone" value={tz} options={TIME_ZONES} onChange={onTz} />
    </div>
  );
}

// SHOWN is how many of a record's fields its line shows; opened, it shows
// all.
const SHOWN = 24;

// RecordBody is a record's message and fields. The application's output:
// text, never markup.
function RecordBody({ record }) {
  const out = [];
  if (record.msg) out.push(<span key="m" className="lg-msg">{record.msg}</span>);
  record.shown.slice(0, SHOWN).forEach((f, i) => {
    if (out.length) out.push(' ');
    out.push(<span key={`${f.key}:${i}`} className="lg-kv"><span className="lg-k">{f.key}=</span>{f.short}</span>);
  });
  if (record.shown.length > SHOWN) out.push(' ', <span key="more" className="lg-k">+{record.shown.length - SHOWN} more</span>);
  return out;
}

// RequestBody is a request the ingress answered: ⇄ method path → status ·
// duration, the rest of its record on hover.
function RequestBody({ rec }) {
  const r = requestParts(rec);
  return (
    <span title={requestTitle(rec)}>
      <span className="lg-mark" aria-label="request">⇄</span> {r.ask} → <b className="lg-status">{r.end}</b>{' '}
      <span className="lg-rest">· {r.rest}</span>
    </span>
  );
}

// EgressBody is a request a pod sent out through an HTTP binding: →
// binding method path → status · duration, and the platform's refusal
// where it answered itself.
function EgressBody({ rec }) {
  const r = egressParts(rec);
  const status = Number(rec.status) || 0;
  const tone = r.refused || status >= 500 || !status ? ' bad' : status >= 400 ? ' warn' : '';
  return (
    <span title={egressTitle(rec)}>
      <span className="lg-mark" aria-label="outbound request">→</span> <b className="lg-binding">{r.binding}</b> {r.ask} →{' '}
      <b className={'lg-status' + tone}>{r.end}</b> <span className="lg-rest">· {r.rest}</span>
      {r.refused && <span className="lg-refused"> refused by the platform: {r.refused}</span>}
    </span>
  );
}

// Fields is an opened record's every field, each with what finds the
// lines that have its value (only) or lack it (not) — a word of the
// search, which an expression does not take —, and the line as it came.
function Fields({ l, q, onWord }) {
  const [raw, setRaw] = useState(false);
  const expr = !!parseSearch(q).re;
  return (
    <div className="lg-fields">
      <dl>
        {l.record.fields.map((f, i) => {
          const has = f.word ? wordIn(q, f.word) : '';
          return (
            <div key={`${f.key}:${i}`}>
              <dt className="mono">{f.key}</dt>
              <dd className="mono">{f.text}</dd>
              <span className="lg-picks">
                {f.word && [['in', 'only', `only lines with ${f.word}`], ['out', 'not', `lines without ${f.word}`]].map(([how, label, title]) => (
                    <button key={how} type="button" className={'lg-pick' + (has === how ? ' on' : '')} aria-pressed={has === how}
                      disabled={expr} onClick={() => onWord(f.word, how === 'out')}
                      title={expr ? 'the search is an expression; clear it to search by a field' : has === how ? `search without ${f.word} again` : title}>
                      {label}
                    </button>
                  ))}
              </span>
            </div>
          );
        })}
      </dl>
      <button type="button" className="lg-asis" aria-expanded={raw} onClick={() => setRaw(!raw)}>
        <Icon name={raw ? 'chevronDown' : 'chevronRight'} size={12} /> the line as it came
      </button>
      {raw && <pre className="lg-raw">{l.text}</pre>}
    </div>
  );
}

function Lines({ search, onAtEnd, many, trace, onTrace, q, onWord, utc, allKept }) {
  const { box, onScroll } = useEndAnchor(search.lines, ({ atEnd, nearTop }) => {
    onAtEnd(atEnd);
    if (nearTop && search.more && !search.loading) search.earlier();
  });
  const [open, setOpen] = useState(() => new Set());
  const toggle = (key) => setOpen((was) => {
    const next = new Set(was);
    if (next.has(key)) next.delete(key);
    else next.add(key);
    return next;
  });
  // A click on a record's line opens it; a click on a button, or one that
  // ends selecting text, does not.
  const onRow = (e, l) => {
    if (!l.record || e.target.closest('button, a')) return;
    if (String(window.getSelection ? window.getSelection() : '')) return;
    toggle(l.key);
  };
  let day = '';
  return (
    <div ref={box} className={'lg-view' + (many ? '' : ' notag')} onScroll={onScroll} tabIndex={0} aria-label="the lines, the newest at the bottom">
      <p className="lg-edge">
        {search.more
          ? <button type="button" className="linkbtn" onClick={search.earlier} disabled={search.loading}>
            {search.loading ? 'reading earlier lines …' : 'load earlier lines'}</button>
          : allKept ? 'The start of what the store keeps.' : 'The start of the time searched.'}
      </p>
      <ol className="lg-lines">
        {search.lines.map((l) => {
          const lvl = lineLevel(l);
          const d = dayOf(l.time, utc);
          const divider = d && d !== day ? (day = d) : null;
          const st = search.streams[l.stream];
          const req = isRequest(l, st);
          const out = !req && isEgress(l, st);
          const opened = !!l.record && open.has(l.key);
          return [
            divider && <li key={`d:${d}`} className="lg-day">{dayText(l.time, utc)}</li>,
            <li key={l.key} className={`lvl-${lvl}${req || out ? ' lg-req' : ''}${opened ? ' open' : ''}`}>
              <div className={'lg-row' + (l.record ? ' lg-opens' : '')} onClick={(e) => onRow(e, l)}>
                <time dateTime={l.time} title={l.time}>{clockOf(l.time, utc)}</time>
                {many && <span className="lg-tag" title={streamTag(st)}>{streamTag(st)}</span>}
                <span className="lg-lvl">{lvl === 'unknown' ? '' : lvl}</span>
                <span className="lg-body">
                  {l.source === 'stderr' && <span className="lg-err">stderr </span>}
                  {/* The application's output: text, never markup. */}
                  {req ? <RequestBody rec={l.rec} /> : out ? <EgressBody rec={l.rec} /> : l.record ? <RecordBody record={l.record} /> : l.text}
                </span>
                <span className="lg-acts">
                  {l.trace && l.trace !== trace && (
                    <button type="button" className="lg-trace" onClick={() => onTrace(l.trace)} title={`the spans of trace ${l.trace}`}>trace</button>
                  )}
                  {l.record && (
                    <button type="button" className="lg-open" aria-expanded={opened} onClick={() => toggle(l.key)}
                      title={opened ? 'close its fields' : 'every field of this line'}>
                      <Icon name={opened ? 'chevronDown' : 'chevronRight'} size={12} />
                    </button>
                  )}
                </span>
              </div>
              {opened && <Fields l={l} q={q} onWord={onWord} />}
            </li>,
          ];
        })}
      </ol>
    </div>
  );
}

// LogSearch is the Logs tab of environment env (its name; st its status)
// for a search: focus (logs.js, focusOf). tail is the node's last lines
// of one object: what stands in for the store while it does not answer,
// and all there is of an attached service (tailOnly), whose process
// writes into its sandbox, not the store.
export function LogSearch({ tenant, env, st, focus, navigate, tail, tailOnly }) {
  const objects = (st && st.objects) || [];
  const [live, setLive] = useState(true);
  const [tz, setTz] = useTimeZone();
  const search = useLogSearch(st && !tailOnly ? st.id : '', focus, live);
  const [text, setText] = useState(focus.q || '');
  useEffect(() => setText(focus.q || ''), [focus.q]);

  // A change of the search is a change of the address.
  const set = (patch) => {
    const next = { ...focus, ...patch };
    for (const k of Object.keys(next)) if (next[k] == null || next[k] === '') delete next[k];
    navigate(envPath(tenant, env, 'logs', next.object || '', next.object && next.replica != null ? next.replica : null, queryOf(next)));
  };
  // A trace link opens the trace's spans in the Traces tab, which lead
  // back to every line of it here.
  const onTrace = (trace) => navigate(envPath(tenant, env, 'traces', trace));
  const onWord = (word, not) => {
    const q = toggleWord(focus.q, word, not);
    if (q != null) set({ q });
  };

  const down = search.error && storeDown(search.error);
  const tailServes = focus.object && focus.source !== 'ingress' && focus.source !== 'egress';
  const many = !(focus.object && focus.replica != null);
  const what = whatOf(focus);
  return (
    <div className="lg">
      <form className="lg-form" onSubmit={(e) => { e.preventDefault(); set({ q: text.trim() }); }}>
        {focus.trace && (
          <div className="lg-tracebar">
            <span className="k">trace</span> <code>{focus.trace}</code>
            <span className="muted small">every line of this request: its record at the ingress, what the containers logged with its ID, what they called out</span>
            <span className="spacer" />
            <button type="button" className="linkbtn" onClick={() => onTrace(focus.trace)}>its spans</button>
            <button type="button" className="linkbtn" onClick={() => set({ trace: undefined })}>leave the trace</button>
          </div>
        )}
        <What objects={objects} focus={focus} set={set} />
        {!tailOnly && <>
        <div className="lg-searchrow">
          <label className="searchwrap grow">
            <Icon name="search" size={13} />
            <input className="searchbare mono" value={text} onChange={(e) => setText(e.target.value)}
              placeholder="words (-word: not), or /an expression/ — Enter searches" aria-label="search the logs" />
            {text && (
              <button type="button" className="iconbtn" aria-label="clear the search" onClick={() => { setText(''); set({ q: undefined }); }}>
                <Icon name="x" size={12} />
              </button>
            )}
          </label>
          <Levels value={focus.level || ''} onChange={(level) => set({ level })} />
        </div>
        <Range focus={focus} set={set} tz={tz} onTz={setTz} />
        </>}
      </form>
      {tailOnly ? tail : down && tailServes ? (
        <>
          <div className="message">
            <Icon name="alert" size={14} />
            <span><strong>The log store does not answer</strong> <span className="muted">({search.error.message}): the object’s last lines from its node, unfiltered.</span></span>
          </div>
          {tail}
        </>
      ) : search.error ? (
        <ErrBox error={search.error.message || String(search.error)} />
      ) : (
        <>
          <p className="lg-count">
            {search.loading && !search.lines.length
              ? 'Searching …'
              : !search.lines.length
                ? `No lines of ${what} match.`
                : `${search.lines.length}${search.more ? '+' : ''} lines of ${what}`}
            {search.following && <span className="live"><span className="live-dot" /> FOLLOWING</span>}
            {!search.following && !live && follows(focus) && search.loaded && <span className="muted">scroll to the end to follow</span>}
            {!focus.object && reachesNow(focus) && search.loaded && <span className="muted">pick an object to follow it live</span>}
            {search.skipped > 0 && <span className="warntext">{search.skipped.toLocaleString()} lines skipped live (too many a second; errors first)</span>}
            {search.partial && <span className="muted">the store stopped at its scan limit; scroll up for more</span>}
          </p>
          {search.lines.length > 0 && (
            <Lines search={search} onAtEnd={setLive} many={many} trace={focus.trace} onTrace={onTrace}
              q={focus.q || ''} onWord={onWord} utc={tz === 'utc'} allKept={rangeOf(focus) === 'all'} />
          )}
        </>
      )}
    </div>
  );
}
