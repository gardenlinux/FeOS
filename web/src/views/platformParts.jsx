// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React from 'react';
import '../styles/platform.css';

// The pieces the platform's pages share: their head, a state's dot by
// class, and the bars of what placement booked.

// PageHead is a page's head (the concept's §6): the kind, the name, its
// state, the actions on the right, a row of facts below.
export function PageHead({ kind = 'Platform', title, mono = false, state = null, actions = null, facts = null }) {
  return (
    <header className="pagehead">
      <div className="pagehead-title">
        <span className="kind">{kind}</span>
        <h1 className={mono ? 'mono' : undefined}>{title}</h1>
        {state}
        {actions && <span className="pagehead-actions">{actions}</span>}
      </div>
      {facts && <div className="facts">{facts}</div>}
    </header>
  );
}

// Dot is a state's dot by its class (the classes of components.jsx's
// StateDot, and draining: a dashed amber ring, on its way out).
export function Dot({ cls, title }) {
  return <span className={'sdot ' + cls} title={title} />;
}

// DotWord is a dot with its word, coloured as the state.
export function DotWord({ st }) {
  return <span className={'dotword ' + st.cls} title={st.say || st.word}><Dot cls={st.cls} />{st.word}</span>;
}

// CapBars are the bars of capacityBars (platformData.js): booked against
// bookable, ochre for what runs as a pod, amber from 85%, red when full.
export function CapBars({ bars, big = false }) {
  return (
    <div className={'capbars' + (big ? ' big' : '')}>
      {bars.map((b) => (
        <div key={b.key} className={'capbar' + (b.tone ? ' t-' + b.tone : '') + (b.share === null ? ' unbounded' : '')}
          title={b.share === null ? `${b.label}: ${b.text} booked; not gated` : `${b.label}: ${b.text} booked (${b.pct}%)`}>
          <span className="capbar-k">{b.label}</span>
          <span className="capbar-track" aria-hidden="true"><span style={{ width: b.pct + '%' }} /></span>
          <span className="capbar-v mono">{b.text}</span>
        </div>
      ))}
    </div>
  );
}

// Fact is one fact of a head's row: a number in bold, then its words.
export function Fact({ n, children, className = '' }) {
  return <span className={className}>{n !== undefined && <b>{n}</b>} {children}</span>;
}
