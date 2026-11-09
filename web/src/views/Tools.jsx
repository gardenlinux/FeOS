// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useEffect, useMemo, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { ErrBox } from '../components.jsx';
import { fmtDurMs, prettyJson } from '../format.js';
import { toolSummary } from './sandboxes.js';
import '../styles/sandboxes.css';

// The tools a sandbox offers its agents, over MCP and REST alike: each
// with its description and input schema, folded. A sandbox's Connect tab
// shows the same reference; /tenants/:t/tools is it on a page of its own.

export function Tools() {
  return (
    <div className="sbx-page">
      <header className="pagehead">
        <div className="pagehead-title">
          <span className="kind">Reference</span>
          <h1>Tools</h1>
        </div>
        <div className="facts">
          <span>what an agent can call in a sandbox, over MCP and REST alike</span>
          <span>connect one on a sandbox&apos;s Connect tab, in its environment</span>
        </div>
      </header>
      <section className="pane"><ToolReference /></section>
    </div>
  );
}

// ToolReference lists the tools of GET /api/v1/tools, searchable, each
// folded to its name, its first sentence and its parameters.
export function ToolReference() {
  const [tools, setTools] = useState(null);
  const [waitMs, setWaitMs] = useState(0);
  const [err, setErr] = useState('');
  const [q, setQ] = useState('');

  useEffect(() => {
    let live = true;
    api.listTools()
      .then((res) => {
        if (!live) return;
        setTools(res.tools || []);
        setWaitMs(Number(res.preparationWaitMs || 0));
      })
      .catch((e) => { if (live) setErr(e.message); });
    return () => { live = false; };
  }, []);

  const list = useMemo(() => {
    const out = tools || [];
    const n = q.trim().toLowerCase();
    return n ? out.filter((t) => t.name.toLowerCase().includes(n) || (t.description || '').toLowerCase().includes(n)) : out;
  }, [tools, q]);

  return <ToolList tools={tools} list={list} waitMs={waitMs} err={err} q={q} setQ={setQ} />;
}

// ToolList renders the reference; tools null while loading.
export function ToolList({ tools, list, waitMs = 0, err = '', q = '', setQ = () => {} }) {
  return (
    <div className="sbx-tools">
      <div className="pane-toolbar">
        <h2><Icon name="terminal" size={14} /> Tools <span className="count">{tools ? tools.length : '…'}</span></h2>
        {waitMs > 0 && (
          <span className="muted small" title="the longest a tool call waits for its sandbox to be ready">
            a call waits up to {fmtDurMs(waitMs)} for its sandbox to wake
          </span>)}
        <span className="spacer" />
        <span className="searchwrap">
          <Icon name="search" size={13} />
          <input className="searchbare" placeholder="search the tools…" value={q} onChange={(e) => setQ(e.target.value)} />
          {q && <button className="ghost mini icon-only" onClick={() => setQ('')}><Icon name="x" size={12} /></button>}
        </span>
      </div>
      <ErrBox error={err} />
      {(list || []).map((t) => (
        <details key={t.name} className="sbx-tool">
          <summary>
            <span className="mono strong">{t.name}</span>
            <span className="muted trunc-line">{firstSentence(t.description)}</span>
            <span className="mono muted small sbx-tool-params">{toolSummary(t.inputSchema)}</span>
          </summary>
          <p className="tooldesc">{t.description}</p>
          <h3 className="sect">input schema</h3>
          <pre className="schemapre">{prettyJson(t.inputSchema)}</pre>
        </details>
      ))}
      {tools && (list || []).length === 0 && <p className="empty">Nothing found.</p>}
      {!tools && !err && <p className="empty">Loading…</p>}
    </div>
  );
}

// firstSentence is a description up to its first full stop.
function firstSentence(s) {
  const t = s || '';
  const i = t.search(/\.(\s|$)/);
  return i > 0 ? t.slice(0, i + 1) : t;
}
