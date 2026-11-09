// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useEffect, useRef, useState } from 'react';
import { Icon } from './Icon.jsx';
import { fmtAgo, fmtDateTime, fmtTime } from './format.js';
import { mcpURL } from './api.js';
import { poller } from './poll.js';
import { THEME_KEY, writeItem } from './storage.js';

// The theme: html[data-theme], remembered; index.html applies it before
// the first paint.
export function useTheme() {
  const [theme, setTheme] = useState(document.documentElement.dataset.theme || 'dark');
  const toggle = useCallback(() => {
    setTheme((cur) => {
      const next = cur === 'dark' ? 'light' : 'dark';
      document.documentElement.dataset.theme = next;
      writeItem('localStorage', THEME_KEY, next);
      return next;
    });
  }, []);
  return [theme, toggle];
}

// PageHead is a page's head (the concept's object page): what it is, its
// name, its state, its main actions on the right, a row of facts below.
export function PageHead({ kind, title, mono = false, state, actions, facts }) {
  return (
    <header className="pagehead">
      <div className="pagehead-title">
        {kind && <span className="kind">{kind}</span>}
        <h1 className={mono ? 'mono' : ''}>{title}</h1>
        {state}
        {actions && <span className="pagehead-actions">{actions}</span>}
      </div>
      {facts && <div className="facts">{facts}</div>}
    </header>
  );
}

// EnvName is an environment's name, whole: FeRun reads nothing into it.
export function EnvName({ name, className = 'envcard-name' }) {
  return <span className={className}>{String(name || '')}</span>;
}

// BrandMark is FeRun's mark, drawn in FeCode's thin open lines: a box
// (a container), its lid in FeRun's indigo on a body in FeOS's ochre,
// the two apart where the walls meet the lid. web/public/icon.svg is the
// same mark as the tab's icon.
export function BrandMark({ size = 26 }) {
  return (
    <svg className="brand-mark" viewBox="0 0 100 100" width={size} height={size} aria-hidden="true">
      <polyline className="brand-run" points="10,46 10,27 50,4 90,27 50,50 10,27" />
      <polyline className="brand-os" points="90,54 90,73 50,96 10,73" />
      <polyline className="brand-os" points="50,64 50,96" />
    </svg>
  );
}

// ------------------------------------------------------------------ hooks --

// useNow re-renders every intervalMs, for relative labels ("12s ago").
export function useNow(intervalMs = 1000) {
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), intervalMs);
    return () => clearInterval(t);
  }, [intervalMs]);
  return now;
}

// usePoll runs fn now and every ms while the tab is visible, and again
// when it becomes visible, never two runs at once (poller). It returns
// when fn last finished. A view whose fn depends on props or a filter
// orders its answers itself (newest). The views poll; none of them
// streams.
export function usePoll(fn, ms = 3000) {
  const [lastOk, setLastOk] = useState(0);
  useEffect(() => {
    const p = poller(fn, () => setLastOk(Date.now()));
    const run = () => { if (document.visibilityState === 'visible') p.run(); };
    run();
    const t = setInterval(run, ms);
    document.addEventListener('visibilitychange', run);
    return () => { p.stop(); clearInterval(t); document.removeEventListener('visibilitychange', run); };
  }, [fn, ms]);
  return lastOk;
}

// useArm is the two-click guard of destructive actions: the first click
// arms the button for 2.6 s, the second within that window acts.
export function useArm(ms = 2600) {
  const [armed, setArmed] = useState('');
  const timer = useRef(null);
  const arm = useCallback((key) => {
    clearTimeout(timer.current);
    setArmed(key);
    timer.current = setTimeout(() => setArmed((c) => (c === key ? '' : c)), ms);
  }, [ms]);
  const disarm = useCallback(() => { clearTimeout(timer.current); setArmed(''); }, []);
  useEffect(() => () => clearTimeout(timer.current), []);
  return [armed, arm, disarm];
}

// ---------------------------------------------------------------- widgets --

// The class of a state's dot, in the form of the architecture page's
// states: running filled green; paused filled grey (on its node, no
// memory); hibernated hollow (only in the object store); on its way
// (creating, pausing, resuming, hibernating) a dashed ring; failed and
// lost red; unknown amber; everything else grey.
export function stateClass(state) {
  switch (state) {
    case 'running': case 'ready': return 'running';
    case 'paused': case 'joined': return 'paused';
    case 'pending': case 'pausing': case 'resuming': case 'creating': case 'hibernating': return 'moving';
    case 'hibernated': return 'hibernated';
    case 'unknown': return 'unknown';
    case 'failed': case 'lost': case 'down': return 'failed';
    default: return 'absent';
  }
}

export function StateDot({ state, title }) {
  return <span className={'sdot ' + stateClass(state)} title={title || state || 'unknown'} />;
}

export function StateLabel({ state }) {
  const s = state || 'unknown';
  return <span className={'state-label ' + stateClass(s)}><StateDot state={s} />{s}</span>;
}

export function CopyBtn({ text, title = 'copy', mini = false }) {
  const [ok, setOk] = useState(false);
  return (
    <button className={'iconbtn' + (mini ? ' mini' : '')} title={ok ? 'copied' : title}
      onClick={(e) => {
        e.stopPropagation();
        if (!navigator.clipboard) return;
        navigator.clipboard.writeText(text).then(() => {
          setOk(true);
          setTimeout(() => setOk(false), 1200);
        }, () => {});
      }}>
      <Icon name={ok ? 'check' : 'copy'} size={mini ? 12 : 14} />
    </button>
  );
}

export function ArmButton({ armKey, armed, arm, disarm, onConfirm, className = '', disabled, icon, children, confirmLabel, title }) {
  const isArmed = armed === armKey;
  return (
    <button
      className={className + ' danger' + (isArmed ? ' armed' : '')}
      disabled={disabled}
      title={title}
      onClick={(e) => {
        e.stopPropagation();
        if (!isArmed) { arm(armKey); return; }
        disarm();
        onConfirm();
      }}>
      {icon && <Icon name={icon} size={14} />}
      {isArmed ? (confirmLabel || 'really?') : children}
    </button>
  );
}

export function LiveBadge({ lastOk, label = 'LIVE' }) {
  const now = useNow(1000);
  const ago = lastOk ? Math.max(0, Math.round((now - lastOk) / 1000)) : null;
  return (
    <span className="live" title={lastOk ? 'last updated ' + fmtTime(new Date(lastOk)) : 'not loaded yet'}>
      <span className="live-dot" />
      {label}
      {ago !== null && <span className="muted"> · {ago === 0 ? 'now' : `${ago}s`}</span>}
    </span>
  );
}

export function Empty({ children }) {
  return <div className="empty">{children}</div>;
}

export function ErrBox({ error }) {
  if (!error) return null;
  return <div className="error"><strong>Error:</strong> {error}</div>;
}

// Ago renders how long ago an RFC 3339 time was; the caller re-renders
// it (useNow).
export function Ago({ iso }) {
  if (!iso) return <span className="muted">—</span>;
  return <span className="muted" title={fmtDateTime(iso)}>{fmtAgo(iso)}</span>;
}

// Avatar is a person's picture, or the first letter of their name.
export function Avatar({ src, name, size = 22 }) {
  const [broken, setBroken] = useState(false);
  if (src && !broken) {
    return <img className="avatar" src={src} alt="" width={size} height={size}
      referrerPolicy="no-referrer" onError={() => setBroken(true)} />;
  }
  const letter = (name || '?').trim().charAt(0).toUpperCase() || '?';
  return <span className="avatar avatar-letter" style={{ width: size, height: size }}>{letter}</span>;
}

// Secret is a value ferund shows once (a token): copy it now, it is never
// shown again. children add what to do with it.
export function Secret({ value, title, onDismiss, children }) {
  return (
    <div className="token-once">
      <div className="token-once-head">
        <Icon name="key" size={15} />
        <strong>{title}</strong>
        <span className="muted">— shown once; ferund stores only its hash</span>
        <span className="spacer" />
        <CopyBtn text={value} title="copy" />
        <button className="ghost mini" onClick={onDismiss}><Icon name="x" size={13} /> done</button>
      </div>
      <code className="token">{value}</code>
      {children}
    </div>
  );
}

// TokenOnce is a new sandbox token with the configuration of MCP clients
// that reach the sandbox with it.
export function TokenOnce({ token, context, onDismiss }) {
  const url = mcpURL();
  const claude = `claude mcp add --transport http ferun ${url} --header "Authorization: Bearer ${token}"`;
  const mcpJson = JSON.stringify({ mcpServers: { ferun: { url, headers: { Authorization: `Bearer ${token}` } } } }, null, 2);
  const messagesApi = JSON.stringify({ type: 'url', url, name: 'ferun', authorization_token: token }, null, 2);
  return (
    <Secret value={token} title={`Token for ${context}`} onDismiss={onDismiss}>
      <details>
        <summary>Client configuration</summary>
        <h4>Claude Code</h4>
        <div className="snippet"><pre>{claude}</pre><CopyBtn text={claude} /></div>
        <h4>mcp.json (editors)</h4>
        <div className="snippet"><pre>{mcpJson}</pre><CopyBtn text={mcpJson} /></div>
        <h4>Claude Messages API (<code>mcp_servers</code>)</h4>
        <div className="snippet"><pre>{messagesApi}</pre><CopyBtn text={messagesApi} /></div>
      </details>
    </Secret>
  );
}

// KV is one row of a key-value grid.
export function KV({ k, children, className = '' }) {
  return <><span className="k">{k}</span><span className={'v ' + className}>{children}</span></>;
}
