// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useEffect, useMemo, useRef, useState } from 'react';
import { Icon } from '../Icon.jsx';
import { search } from '../search.js';

// The search (⌘K or Ctrl+K anywhere, or the sidebar's field): jumps to a
// page, a tenant or a thing of the tenant by its name or ID (search.js).
// Arrows move, Enter opens, Escape closes.

// usePaletteKey opens the search on ⌘K / Ctrl+K.
export function usePaletteKey(open) {
  useEffect(() => {
    const onKey = (e) => {
      if ((e.metaKey || e.ctrlKey) && (e.key === 'k' || e.key === 'K')) {
        e.preventDefault();
        open();
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [open]);
}

export function Palette({ all, tenant, environments, onClose, navigate }) {
  const [q, setQ] = useState('');
  const [at, setAt] = useState(0);
  const input = useRef(null);
  const results = useMemo(() => search(all, q, { tenant, environments }), [all, q, tenant, environments]);

  useEffect(() => { input.current && input.current.focus(); }, []);
  useEffect(() => { setAt(0); }, [q]);

  const go = (e) => {
    if (!e) return;
    onClose();
    navigate(e.to);
  };
  const onKey = (e) => {
    if (e.key === 'Escape') { e.preventDefault(); onClose(); }
    if (e.key === 'ArrowDown') { e.preventDefault(); setAt((i) => Math.min(results.length - 1, i + 1)); }
    if (e.key === 'ArrowUp') { e.preventDefault(); setAt((i) => Math.max(0, i - 1)); }
    if (e.key === 'Enter') { e.preventDefault(); go(results[at]); }
  };

  return (
    <div className="palette-back" onMouseDown={onClose}>
      <div className="palette" role="dialog" aria-label="Search" onMouseDown={(e) => e.stopPropagation()}>
        <div className="palette-input">
          <Icon name="search" size={15} />
          <input ref={input} id="palette-q" value={q} onChange={(e) => setQ(e.target.value)} onKeyDown={onKey}
            placeholder="A page, a sandbox ID, an environment, prod#13, a domain…" autoComplete="off" spellCheck="false" />
          <kbd>esc</kbd>
        </div>
        <div className="palette-list" role="listbox">
          {results.length === 0 && <p className="empty">Nothing by that name or ID{tenant ? ` in ${tenant}` : ''}.</p>}
          {results.map((r, i) => (
            <button key={r.key} role="option" aria-selected={i === at}
              className={'palette-item' + (i === at ? ' on' : '')}
              onMouseEnter={() => setAt(i)} onClick={() => go(r)}>
              <Icon name={r.icon || 'arrowRight'} size={14} />
              <span className={r.kind === 'sandbox' || r.kind === 'job' || r.kind === 'domain' || r.kind === 'credential' ? 'mono' : ''}>{r.label}</span>
              {r.hint && <span className="palette-hint">{r.hint}</span>}
              <span className="palette-kind">{r.kind}</span>
            </button>
          ))}
        </div>
      </div>
    </div>
  );
}
