// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { api, mcpURL } from '../api.js';
import { Icon } from '../Icon.jsx';
import {
  Ago, ArmButton, CopyBtn, ErrBox, LiveBadge, StateDot, StateLabel, TokenOnce, useArm, useNow, usePoll,
} from '../components.jsx';
import { fmtDate } from '../format.js';
import { createHints, fleetDefaults, fmtSize, sandboxCounts } from './fleet.js';
import { Activity } from './Activity.jsx';
import { Network } from './Network.jsx';
import { Access } from './Access.jsx';
import { useAccess } from '../people.js';

// A tenant's sandboxes: the list, and a sandbox's detail with its tabs
// (overview, activity, tokens, access, network). The selection and the
// tab are the route's: /tenants/:t/sandboxes/:id/:tab. The changes need a
// tenant grant write; the activity and the network are the operators'
// (the activity log carries tool params and outputs), as is a node.

export function Sandboxes({ tenant, sel, tab, navigate }) {
  const [sandboxes, setSandboxes] = useState(null);
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState('');
  const [showCreate, setShowCreate] = useState(false);
  const [defaults, setDefaults] = useState(null);
  const [createdToken, setCreatedToken] = useState(null);
  const [q, setQ] = useState('');
  const base = `/tenants/${encodeURIComponent(tenant)}/sandboxes`;
  const { can: c } = useAccess(tenant);

  const refresh = useCallback(async () => {
    try {
      const res = await api.listSandboxes();
      setSandboxes(res.sandboxes || []);
      setErr('');
    } catch (e) {
      setErr(e.message);
    }
  }, []);
  const lastOk = usePoll(refresh, 3000);
  useNow(1000);

  // The create form names the platform's defaults and bounds, which the
  // nodes view's answer carries (GET /api/v1/admin/nodes); read once, when
  // the form first opens, by an operator only. Without them the form says
  // "the default".
  useEffect(() => {
    if (!showCreate || defaults || !c.operator) return undefined;
    let live = true;
    api.listNodes().then((res) => { if (live && res.defaults) setDefaults(res.defaults); }, () => {});
    return () => { live = false; };
  }, [showCreate, defaults, c.operator]);

  // Escape closes the detail.
  useEffect(() => {
    if (!sel) return undefined;
    const onKey = (e) => {
      if (e.key === 'Escape' && !e.target.closest('input,textarea,select')) navigate(base);
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [sel, base, navigate]);

  const act = async (label, fn) => {
    setBusy(label);
    setErr('');
    try {
      await fn();
      await refresh();
    } catch (e) {
      setErr(e.message);
    } finally {
      setBusy('');
    }
  };

  const selSb = (sandboxes || []).find((s) => s.sandboxId === sel);
  const list = useMemo(() => {
    let out = sandboxes || [];
    if (q.trim()) {
      const n = q.trim().toLowerCase();
      out = out.filter((s) => s.sandboxId.toLowerCase().includes(n)
        || (s.image || '').toLowerCase().includes(n)
        || (s.node || '').toLowerCase().includes(n)
        || (s.clientState || '').toLowerCase().includes(n));
    }
    return out;
  }, [sandboxes, q]);

  const counts = useMemo(() => sandboxCounts(sandboxes), [sandboxes]);

  // The detail is keyed by the sandbox, so that its tabs start afresh for
  // another one and no answer for the previous sandbox lands in them.
  if (sel && selSb) {
    return (
      <SandboxDetail key={selSb.sandboxId} tenant={tenant} sb={selSb} tab={tab} busy={busy} act={act} err={err}
        refresh={refresh} navigate={navigate} base={base} can={c} />
    );
  }

  return (
    <div className="single">
      <section className="pane">
        <div className="pane-toolbar">
          <h2>
            <Icon name="box" size={15} /> Sandboxes
            <span className="count">{sandboxes ? sandboxes.length : '…'}</span>
          </h2>
          <span className="mini-stats muted">
            <span className="ok" title="running">● {counts.running}</span>
            <span className="warn" title="paused">● {counts.paused}</span>
            {counts.hibernated > 0 && <span className="acc" title="hibernated: the checkpoint is in the object store, nothing on a node">◆ {counts.hibernated}</span>}
            {counts.error > 0 && <span className="bad" title="failed or lost">● {counts.error}</span>}
          </span>
          <span className="spacer" />
          <LiveBadge lastOk={lastOk} />
          {c.writeTenant && (
            <button className={'ghost mini' + (showCreate ? ' on' : '')} onClick={() => setShowCreate(!showCreate)}>
              <Icon name="plus" size={13} /> new
            </button>)}
        </div>

        {showCreate && c.writeTenant && (
          <CreatePanel busy={busy} act={act} defaults={defaults}
            onDone={(tok) => { setCreatedToken(tok); setShowCreate(false); }}
            onCancel={() => setShowCreate(false)} />
        )}
        {createdToken && (
          <TokenOnce token={createdToken.token} context={`sandbox ${createdToken.sandboxId}`}
            onDismiss={() => setCreatedToken(null)} />
        )}
        {sel && sandboxes && !selSb && <p className="error">No sandbox {sel} in this tenant.</p>}
        <ErrBox error={err} />

        <div className="filterbar slim">
          <span className="searchwrap grow">
            <Icon name="search" size={13} />
            <input className="searchbare" placeholder="ID, image, node, state…" value={q}
              onChange={(e) => setQ(e.target.value)} />
            {q && <button className="ghost mini icon-only" onClick={() => setQ('')}><Icon name="x" size={12} /></button>}
          </span>
        </div>

        <div className="tblwrap">
          <table className="tbl">
            <thead>
              <tr>
                <th className="col-state">state</th>
                <th>sandbox ID</th>
                <th>image</th>
                <th>node</th>
                <th>size</th>
                <th className="col-when">active</th>
                <th className="col-when">created</th>
                <th className="col-quick" />
              </tr>
            </thead>
            <tbody>
              {list.map((sb) => (
                <tr key={sb.sandboxId} className="clickable" onClick={() => navigate(`${base}/${sb.sandboxId}`)}>
                  <td className="col-state"><StateDot state={sb.clientState} /></td>
                  <td className="mono idcell" title={sb.sandboxId}>
                    {sb.sandboxId}
                    <CopyBtn text={sb.sandboxId} mini />
                  </td>
                  <td className="muted trunc" title={sb.image}>{sb.image || 'default'}</td>
                  <td className="muted mono">{sb.node || '—'}</td>
                  <td className="muted mono">{fmtSize(sb)}</td>
                  <td className="col-when"><Ago iso={sb.updatedAt} /></td>
                  <td className="col-when muted">{fmtDate(sb.createdAt)}</td>
                  <td className="col-quick" onClick={(e) => e.stopPropagation()}>
                    {c.writeTenant && sb.clientState === 'running' && (
                      <button className="iconbtn mini" title="pause" disabled={!!busy}
                        onClick={() => act('pause', () => api.pauseSandbox(sb.sandboxId))}>
                        <Icon name="pause" size={12} /></button>)}
                    {c.writeTenant && (sb.clientState === 'paused' || sb.clientState === 'hibernated') && (
                      <button className="iconbtn mini"
                        title={sb.clientState === 'hibernated' ? 'resume from the checkpoint' : 'resume'}
                        disabled={!!busy}
                        onClick={() => act('resume', () => api.resumeSandbox(sb.sandboxId))}>
                        <Icon name="play" size={12} /></button>)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {sandboxes && sandboxes.length === 0 && <p className="empty">No sandboxes yet.</p>}
          {sandboxes && sandboxes.length > 0 && list.length === 0 && <p className="empty">No sandbox matches this filter.</p>}
          {!sandboxes && !err && <p className="empty">Loading…</p>}
        </div>
      </section>
    </div>
  );
}

// CreatePanel is the create form. An empty image or size takes the
// platform's default, which the fields name from defaults (the nodes
// view's), as they name the bounds ferund accepts.
export function CreatePanel({ act, busy, defaults, onDone, onCancel }) {
  const hints = createHints(fleetDefaults(defaults));
  const [newImage, setNewImage] = useState('');
  const [vcpus, setVcpus] = useState('');
  const [memMiB, setMemMiB] = useState('');
  return (
    <form className="createpanel" onSubmit={(e) => {
      e.preventDefault();
      act('create', async () => {
        const res = await api.createSandbox(newImage.trim(), Number(vcpus) || 0, Number(memMiB) || 0);
        setNewImage(''); setVcpus(''); setMemMiB('');
        if (res.token) onDone({ token: res.token, sandboxId: res.status?.sandboxId });
      });
    }}>
      <input placeholder={hints.image} title={hints.image} value={newImage}
        onChange={(e) => setNewImage(e.target.value)} autoFocus />
      <input className="num" type="number" min={hints.vcpus.min} max={hints.vcpus.max} placeholder={hints.vcpus.placeholder}
        value={vcpus} onChange={(e) => setVcpus(e.target.value)} title={hints.vcpus.title} />
      <input className="num" type="number" min={hints.memory.min} max={hints.memory.max} step="256"
        placeholder={hints.memory.placeholder} value={memMiB}
        onChange={(e) => setMemMiB(e.target.value)} title={hints.memory.title} />
      <button type="submit" className="primary" disabled={!!busy}><Icon name="plus" size={14} /> Create</button>
      <button type="button" className="ghost" onClick={onCancel}><Icon name="x" size={14} /></button>
    </form>
  );
}

// TABS are a sandbox's tabs; operator: an operator's only (admin routes).
export const TABS = [
  { key: 'overview', label: 'Overview', icon: 'gauge' },
  { key: 'activity', label: 'Activity', icon: 'activity', operator: true },
  { key: 'tokens', label: 'Tokens', icon: 'key' },
  { key: 'access', label: 'Access', icon: 'lock' },
  { key: 'network', label: 'Network', icon: 'globe', operator: true },
];

// sandboxTabs are the tabs can offers.
export function sandboxTabs(can) {
  return TABS.filter((t) => !t.operator || (can && can.operator));
}

function SandboxDetail({ sb, tab, busy, act, err, refresh, navigate, base, can: c }) {
  const [armed, arm, disarm] = useArm();
  const here = `${base}/${sb.sandboxId}`;
  const tabs = sandboxTabs(c);
  const activeTab = tabs.some((t) => t.key === tab) ? tab : 'overview';
  const load = useCallback((source) => api.listActivity(sb.sandboxId, { source }), [sb.sandboxId]);

  return (
    <section className="pane pane-detail">
      <div className="detail-head">
        <button className="iconbtn" title="back to the list (Esc)" onClick={() => navigate(base)}>
          <Icon name="arrowLeft" size={15} /></button>
        <StateDot state={sb.clientState} />
        <div className="detail-title">
          <span className="mono uuid" title={sb.sandboxId}>{sb.sandboxId}</span>
          <span className="muted small">{sb.image || 'default image'} · node {sb.node || '—'}</span>
        </div>
        <CopyBtn text={sb.sandboxId} title="copy the sandbox ID" />
      </div>

      <div className="tabbar">
        {tabs.map((t) => (
          <button key={t.key} className={'tab' + (activeTab === t.key ? ' on' : '')}
            onClick={() => navigate(`${here}/${t.key}`)}>
            <Icon name={t.icon} size={14} /> {t.label}
          </button>
        ))}
      </div>

      {activeTab === 'overview' && (
        <div className="tabbody">
          <ErrBox error={err} />
          <div className="kv-grid">
            <span className="k">state</span><StateLabel state={sb.clientState} />
            {sb.statusDescription && <>
              <span className="k">status</span><span className="v small">{sb.statusDescription}</span>
            </>}
            <span className="k">sandbox ID</span>
            <span className="mono v">{sb.sandboxId} <CopyBtn text={sb.sandboxId} mini /></span>
            <span className="k">image</span><span className="v mono">{sb.image || 'the default image'}</span>
            <span className="k">node</span>
            <span className="mono v">
              {sb.node && c.operator
                ? <a href={`/nodes/${encodeURIComponent(sb.node)}`} onClick={(e) => { e.preventDefault(); navigate(`/nodes/${encodeURIComponent(sb.node)}`); }}>{sb.node}</a>
                : sb.node || '—'}
            </span>
            <span className="k">size</span><span className="mono v">{fmtSize(sb)}</span>
            <span className="k">owner</span><span className="mono v">{sb.owner || '—'}</span>
            <span className="k">created</span>
            <span className="v">{fmtDate(sb.createdAt)} <Ago iso={sb.createdAt} /></span>
            <span className="k">last activity</span>
            <span className="v">{fmtDate(sb.updatedAt)} <Ago iso={sb.updatedAt} /></span>
            {Number(sb.checkpointEpoch) > 0 && (
              <>
                <span className="k">checkpoint</span>
                <span className="v acc" title={sb.checkpointHead || ''}>
                  epoch {sb.checkpointEpoch} in the object store {sb.checkpointAt && <Ago iso={sb.checkpointAt} />}
                </span>
              </>)}
          </div>

          <div className="actionrow">
            {c.writeTenant && sb.clientState === 'running' && (
              <button className="primary" disabled={!!busy}
                onClick={() => act('pause', () => api.pauseSandbox(sb.sandboxId))}>
                <Icon name="pause" size={14} /> Pause</button>)}
            {c.writeTenant && (sb.clientState === 'paused' || sb.clientState === 'hibernated') && (
              <button className="primary" disabled={!!busy}
                title={sb.clientState === 'hibernated' ? 'restore from the checkpoint, on a node with the same object store' : 'resume'}
                onClick={() => act('resume', () => api.resumeSandbox(sb.sandboxId))}>
                <Icon name="play" size={14} /> {sb.clientState === 'hibernated' ? 'Resume from the checkpoint' : 'Resume'}</button>)}
            <button className="ghost" disabled={!!busy} onClick={refresh}>
              <Icon name="refresh" size={14} /> Refresh</button>
            {busy && <span className="muted small">{busy}…</span>}
            <span className="spacer" />
            {c.writeTenant && (
              <ArmButton armKey={sb.sandboxId} armed={armed} arm={arm} disarm={disarm} icon="trash"
                confirmLabel="really delete?" disabled={!!busy}
                onConfirm={() => act('delete', async () => {
                  await api.deleteSandbox(sb.sandboxId);
                  navigate(base);
                })}>Delete</ArmButton>)}
          </div>

          <div className="connectbox">
            <h3><Icon name="terminal" size={14} /> Connect agents</h3>
            <p className="muted small">
              The MCP endpoint of the sandbox's tools; it takes a token of the sandbox (the Tokens tab).
            </p>
            <div className="snippet">
              <pre>{mcpURL()}</pre>
              <CopyBtn text={mcpURL()} />
            </div>
          </div>
        </div>
      )}

      {activeTab === 'activity' && (
        <div className="tabbody"><Activity load={load} /></div>
      )}
      {activeTab === 'tokens' && (
        <div className="tabbody"><Tokens sandboxId={sb.sandboxId} write={c.writeTenant} /></div>
      )}
      {activeTab === 'access' && (
        <div className="tabbody"><Access sandboxId={sb.sandboxId} /></div>
      )}
      {activeTab === 'network' && (
        <div className="tabbody"><Network sandboxId={sb.sandboxId} /></div>
      )}
    </section>
  );
}

function Tokens({ sandboxId, write }) {
  const [tokens, setTokens] = useState(null);
  const [label, setLabel] = useState('');
  const [fresh, setFresh] = useState('');
  const [err, setErr] = useState('');
  const [armed, arm, disarm] = useArm();

  const refresh = useCallback(async () => {
    try {
      setTokens((await api.listTokens(sandboxId)).tokens || []);
      setErr('');
    } catch (e) {
      setErr(e.message);
    }
  }, [sandboxId]);
  useEffect(() => { refresh(); }, [refresh]);

  return (
    <div className="tokens">
      <ErrBox error={err} />
      {fresh && <TokenOnce token={fresh} context="the sandbox" onDismiss={() => setFresh('')} />}

      <div className="tblwrap"><table className="tbl">
        <thead>
          <tr><th>hash</th><th>label</th><th className="col-when">created</th><th className="col-when">last used</th><th className="col-quick" /></tr>
        </thead>
        <tbody>
          {(tokens || []).map((t) => (
            <tr key={t.hash}>
              <td className="mono muted" title={t.hash}>{t.hashPrefix}…</td>
              <td>{t.label || <span className="muted">(no label)</span>}</td>
              <td className="col-when muted">{fmtDate(t.createdAt)}</td>
              <td className="col-when">{t.lastUsedAt ? <Ago iso={t.lastUsedAt} /> : <span className="muted">never</span>}</td>
              <td className="col-quick">
                {write && (
                  <ArmButton armKey={t.hash} armed={armed} arm={arm} disarm={disarm} icon="trash"
                    className="mini" confirmLabel="revoke?"
                    onConfirm={() => api.revokeToken(sandboxId, t.hash).then(refresh, (e) => setErr(e.message))}>Revoke</ArmButton>)}
              </td>
            </tr>
          ))}
        </tbody>
      </table></div>
      {tokens && tokens.length === 0 && <p className="empty">No tokens.</p>}

      {write && <form className="createpanel" onSubmit={async (e) => {
        e.preventDefault();
        try {
          const res = await api.createToken(sandboxId, label.trim());
          setFresh(res.token);
          setLabel('');
          await refresh();
        } catch (e2) {
          setErr(e2.message);
        }
      }}>
        <input placeholder="label (e.g. laptop, claude code)" value={label} onChange={(e) => setLabel(e.target.value)} />
        <button type="submit" className="primary"><Icon name="key" size={14} /> Mint a token</button>
      </form>}
    </div>
  );
}
