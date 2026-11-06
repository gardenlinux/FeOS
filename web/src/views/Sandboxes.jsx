// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import {
  Ago, ArmButton, CopyBtn, ErrBox, LiveBadge, StateDot, TokenOnce, useArm, useNow, usePoll,
} from '../components.jsx';
import { fmtDate } from '../format.js';
import { useAccess } from '../people.js';
import { useSummary } from '../summary.jsx';
import { createHints, fleetDefaults } from './fleet.js';
import {
  STATE_BUCKETS, activeTab, canPause, canResume, createRequest, filterSandboxes, ownerShown, sandboxTabs, sizeWords,
  stateCounts, stateWords,
} from './sandboxes.js';
import { Activity } from './Activity.jsx';
import { Network } from './Network.jsx';
import { Access, ImageCredentials } from './Access.jsx';
import { Connect } from './SandboxConnect.jsx';
import '../styles/sandboxes.css';

// A tenant's sandboxes: the list, the create form and a sandbox's page
// with its tabs (overview, connect, credentials, network, activity,
// settings). The selection and the tab are the route's:
// /tenants/:t/sandboxes[/new | /:id[/:tab]] ("new" is no sandbox ID).
// The changes need a tenant grant write; the activity and the network
// are the operators' (the activity log carries tool params and
// outputs), as is a node.

// NEW is the create form's place in the route.
const NEW = 'new';

// WRITE_REASON is the title of what a role without write may not do.
const WRITE_REASON = 'needs write on this tenant';

export function Sandboxes({ tenant, sel, tab, navigate }) {
  const [sandboxes, setSandboxes] = useState(null);
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState('');
  // created is the create's answer, {sandboxId, token}: its token shows
  // once, on the new sandbox's Connect tab.
  const [created, setCreated] = useState(null);
  const base = `/tenants/${encodeURIComponent(tenant)}/sandboxes`;
  const { can: c } = useAccess(tenant);
  const summary = useSummary();

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

  // Escape goes back to the list, from a sandbox or the form.
  useEffect(() => {
    if (!sel) return undefined;
    const onKey = (e) => {
      if (e.key === 'Escape' && !e.target.closest('input,textarea,select')) navigate(base);
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [sel, base, navigate]);

  // act runs a change, then reads the list and the summary again.
  const act = async (label, fn) => {
    setBusy(label);
    setErr('');
    try {
      await fn();
      await refresh();
      summary.refresh();
    } catch (e) {
      setErr(e.message);
    } finally {
      setBusy('');
    }
  };
  const forgetCreated = useCallback(() => setCreated(null), []);

  if (sel === NEW) {
    return (
      <NewSandbox base={base} navigate={navigate} can={c} busy={busy} err={err}
        onCreate={(args) => act('create', async () => {
          const res = await api.createSandbox(...args);
          const id = res.status && res.status.sandboxId;
          if (!id) return;
          setCreated({ sandboxId: id, token: res.token || '' });
          await refresh();
          navigate(`${base}/${id}/connect`);
        })} />
    );
  }

  const selSb = sel ? (sandboxes || []).find((s) => s.sandboxId === sel) : null;
  if (sel && selSb) {
    // Keyed by the sandbox, so that its tabs start afresh for another one
    // and no answer for the previous sandbox lands in them.
    return (
      <SandboxPage key={selSb.sandboxId} tenant={tenant} sb={selSb} tab={tab} busy={busy} act={act} err={err}
        navigate={navigate} base={base} can={c}
        fresh={created && created.sandboxId === selSb.sandboxId ? created.token : ''} onFreshSeen={forgetCreated} />
    );
  }

  return (
    <SandboxList tenant={tenant} sandboxes={sandboxes} err={err} busy={busy} act={act} navigate={navigate} base={base}
      can={c} lastOk={lastOk} missing={sel && sandboxes ? sel : ''}
      created={created && created.sandboxId === sel ? created : null} onCreatedSeen={forgetCreated} />
  );
}

// StateCounts are the list's counts by state, each as its dot.
export function StateCounts({ counts }) {
  return (
    <span className="sbx-counts">
      {STATE_BUCKETS.filter((b) => b.key !== 'other' || counts.other > 0).map((b) => (
        <span key={b.key} className={'sbx-count' + (counts[b.key] ? '' : ' zero')} title={b.states.join(', ')}>
          <StateDot state={b.dot} title={b.label} /> <b>{counts[b.key]}</b> {b.label}
        </span>
      ))}
    </span>
  );
}

export function SandboxList({ tenant, sandboxes, err, busy, act, navigate, base, can: c, lastOk, missing, created, onCreatedSeen }) {
  const [q, setQ] = useState('');
  const list = useMemo(() => filterSandboxes(sandboxes, q), [sandboxes, q]);
  const counts = useMemo(() => stateCounts(sandboxes), [sandboxes]);
  const showOwner = (sandboxes || []).some((s) => ownerShown(s, tenant));
  const newPath = `${base}/${NEW}`;

  return (
    <div className="sbx-page">
      <header className="pagehead">
        <div className="pagehead-title">
          <h1>Sandboxes</h1>
          <span className="pagehead-actions">
            <LiveBadge lastOk={lastOk} />
            {c.writeTenant
              ? <a className="button primary" href={newPath} onClick={(e) => { e.preventDefault(); navigate(newPath); }}>
                <Icon name="plus" size={14} /> New sandbox</a>
              : <button className="primary" disabled title={WRITE_REASON}><Icon name="plus" size={14} /> New sandbox</button>}
          </span>
        </div>
        <div className="facts">
          {sandboxes ? <StateCounts counts={counts} /> : <span>…</span>}
        </div>
      </header>

      <section className="pane">
        {missing && <p className="error">No sandbox {missing} in this tenant.</p>}
        {created && created.token && (
          <TokenOnce token={created.token} context={`sandbox ${created.sandboxId}`} onDismiss={onCreatedSeen} />)}
        <ErrBox error={err} />

        <div className="filterbar slim">
          <span className="searchwrap grow">
            <Icon name="search" size={13} />
            <input className="searchbare" placeholder="ID, owner, image, node, state…" value={q}
              onChange={(e) => setQ(e.target.value)} />
            {q && <button className="ghost mini icon-only" onClick={() => setQ('')}><Icon name="x" size={12} /></button>}
          </span>
        </div>

        <div className="tblwrap">
          <table className="tbl sbx-table">
            <thead>
              <tr>
                <th>sandbox</th>
                {showOwner && <th>owner</th>}
                <th>image</th>
                <th>state</th>
                <th>size</th>
                <th className="col-when">last activity</th>
                <th className="col-quick" />
              </tr>
            </thead>
            <tbody>
              {list.map((sb) => (
                <tr key={sb.sandboxId} className="clickable" onClick={() => navigate(`${base}/${sb.sandboxId}`)}>
                  <td className="mono" title={sb.sandboxId}>
                    <span className="sbx-id">{sb.sandboxId}<CopyBtn text={sb.sandboxId} mini title="copy the sandbox ID" /></span>
                  </td>
                  {showOwner && <td className="small">{ownerShown(sb, tenant) || <span className="muted">—</span>}</td>}
                  <td className="mono small muted trunc" title={sb.image}>{sb.image || 'the default'}</td>
                  <td>
                    <span className="cell-state" title={sb.statusDescription || sb.clientState}>
                      <StateDot state={sb.clientState} /> {stateWords(sb).short}
                    </span>
                  </td>
                  <td className="mono small muted">{sizeWords(sb) || '—'}</td>
                  <td className="col-when"><Ago iso={sb.updatedAt} /></td>
                  <td className="col-quick" onClick={(e) => e.stopPropagation()}>
                    <QuickAction sb={sb} busy={busy} act={act} write={c.writeTenant} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {sandboxes && sandboxes.length === 0 && (
            <p className="empty">No sandboxes yet. {c.writeTenant && (
              <a href={newPath} onClick={(e) => { e.preventDefault(); navigate(newPath); }}>Create one and connect an agent →</a>)}</p>)}
          {sandboxes && sandboxes.length > 0 && list.length === 0 && <p className="empty">No sandbox matches this filter.</p>}
          {!sandboxes && !err && <p className="empty">Loading…</p>}
        </div>
      </section>
    </div>
  );
}

// QuickAction is a row's pause or resume.
function QuickAction({ sb, busy, act, write }) {
  if (!write) return null;
  if (canPause(sb)) {
    return (
      <button className="iconbtn mini" title="pause" disabled={!!busy}
        onClick={() => act('pause', () => api.pauseSandbox(sb.sandboxId))}><Icon name="pause" size={12} /></button>);
  }
  if (canResume(sb)) {
    return (
      <button className="iconbtn mini" title={sb.clientState === 'hibernated' ? 'resume from the checkpoint' : 'resume'}
        disabled={!!busy} onClick={() => act('resume', () => api.resumeSandbox(sb.sandboxId))}>
        <Icon name="play" size={12} /></button>);
  }
  return null;
}

// NewSandbox is the create form's page.
function NewSandbox({ base, navigate, can: c, busy, err, onCreate }) {
  const [defaults, setDefaults] = useState(null);
  // The platform's defaults and bounds are in the nodes view's answer
  // (GET /api/v1/admin/nodes), an operator's; others see "the default".
  useEffect(() => {
    if (!c.operator) return undefined;
    let live = true;
    api.listNodes().then((res) => { if (live && res.defaults) setDefaults(res.defaults); }, () => {});
    return () => { live = false; };
  }, [c.operator]);
  return (
    <div className="sbx-page">
      <header className="pagehead">
        <div className="pagehead-title">
          <span className="kind">Sandbox</span>
          <h1>New sandbox</h1>
          <span className="pagehead-actions">
            <a className="button" href={base} onClick={(e) => { e.preventDefault(); navigate(base); }}>
              <Icon name="arrowLeft" size={14} /> Sandboxes</a>
          </span>
        </div>
        <div className="facts"><span>a microVM for an agent to work in · it sleeps when idle and costs nothing then</span></div>
      </header>
      <section className="pane">
        <ErrBox error={err} />
        {!c.writeTenant && <p className="error">Your role does not allow this: it needs write on this tenant.</p>}
        <CreateForm defaults={defaults} busy={busy} disabled={!c.writeTenant} onSubmit={onCreate}
          onCancel={() => navigate(base)} />
      </section>
    </div>
  );
}

// CreateForm is the create form. An empty image or size takes the
// platform's default, which the fields name from defaults (the nodes
// view's), as they name the bounds ferund accepts. onSubmit gets
// createSandbox's arguments.
export function CreateForm({ defaults, busy, disabled = false, onSubmit, onCancel }) {
  const hints = createHints(fleetDefaults(defaults));
  const [f, setF] = useState({ image: '', vcpus: '', memoryMib: '', attachable: false, username: '', password: '' });
  const [bad, setBad] = useState('');
  const set = (k) => (e) => setF({ ...f, [k]: e.target.type === 'checkbox' ? e.target.checked : e.target.value });
  return (
    <form className="sbx-form" onSubmit={(e) => {
      e.preventDefault();
      try {
        const args = createRequest(f);
        setBad('');
        onSubmit(args);
      } catch (e2) {
        setBad(e2.message);
      }
    }}>
      <label className="sbx-field">
        <span className="sbx-label">image</span>
        <input className="mono" placeholder={hints.image} title={hints.image} value={f.image} onChange={set('image')} autoFocus />
        <span className="sbx-hint">An OCI image reference; empty: the platform's default image. It is pinned to its digest at the first create.</span>
      </label>
      <div className="sbx-row">
        <label className="sbx-field">
          <span className="sbx-label">vCPUs</span>
          <input className="num" type="number" min={hints.vcpus.min} max={hints.vcpus.max} placeholder={hints.vcpus.placeholder}
            value={f.vcpus} onChange={set('vcpus')} title={hints.vcpus.title} />
        </label>
        <label className="sbx-field">
          <span className="sbx-label">memory, MiB</span>
          <input className="num" type="number" min={hints.memory.min} max={hints.memory.max} step="256"
            placeholder={hints.memory.placeholder} value={f.memoryMib} onChange={set('memoryMib')} title={hints.memory.title} />
        </label>
        <span className="sbx-hint sbx-row-hint">Empty: the default size. Fixed for the sandbox's life; it is booked on a node only while awake.</span>
      </div>
      <label className="sbx-check">
        <input type="checkbox" checked={f.attachable} onChange={set('attachable')} />
        <span><b>attachable</b> <span className="muted">— an environment's attached service may run in it, and reaches that environment's addons. Fixed for its life.</span></span>
      </label>
      <fieldset className="sbx-fieldset">
        <legend>private image <span className="muted">· optional</span></legend>
        <div className="sbx-row">
          <label className="sbx-field">
            <span className="sbx-label">registry username</span>
            <input autoComplete="off" value={f.username} onChange={set('username')} />
          </label>
          <label className="sbx-field">
            <span className="sbx-label">password or token</span>
            <input type="password" autoComplete="new-password" value={f.password} onChange={set('password')} />
          </label>
        </div>
        <span className="sbx-hint">Sealed in ferund and never shown again; rotate or remove it later under the sandbox's Credentials.</span>
      </fieldset>
      {bad && <p className="error">{bad}</p>}
      <div className="sbx-formactions">
        <button type="submit" className="primary" disabled={disabled || !!busy} title={disabled ? WRITE_REASON : undefined}>
          <Icon name="plus" size={14} /> {busy === 'create' ? 'Creating…' : 'Create sandbox'}</button>
        <button type="button" className="ghost" onClick={onCancel}>Cancel</button>
        <span className="muted small">You get its first token once, on its Connect tab.</span>
      </div>
    </form>
  );
}

// SandboxHead is a sandbox page's head: the kind, the ID, the owner, the
// state in words with what it costs, the main actions, the facts.
export function SandboxHead({ sb, tenant, busy, act, write, onConnect }) {
  const words = stateWords(sb);
  const owner = ownerShown(sb, tenant);
  const hibernated = sb.clientState === 'hibernated';
  const why = write ? undefined : WRITE_REASON;
  return (
    <header className="pagehead">
      <div className="pagehead-title">
        <span className="kind">Sandbox</span>
        <h1 className="mono">{sb.sandboxId}</h1>
        <CopyBtn text={sb.sandboxId} title="copy the sandbox ID" />
        {owner && <span className="muted">{owner}</span>}
        <span className={'sbx-state s-' + sb.clientState}>
          <StateDot state={sb.clientState} />
          <span className="mono sbx-state-word">{sb.clientState || 'unknown'}</span>
          <span>{words.long}</span>
        </span>
        <span className="pagehead-actions">
          {canPause(sb) && (
            <button disabled={!write || !!busy} title={why || 'pause now: it frees its memory on the node'}
              onClick={() => act('pause', () => api.pauseSandbox(sb.sandboxId))}>
              <Icon name="pause" size={14} /> {busy === 'pause' ? 'Pausing…' : 'Pause'}</button>)}
          {canResume(sb) && (
            <button disabled={!write || !!busy}
              title={why || (hibernated ? 'restore from the checkpoint, on any node of the same object store' : 'resume on its node')}
              onClick={() => act('resume', () => api.resumeSandbox(sb.sandboxId))}>
              <Icon name="play" size={14} /> {busy === 'resume' ? 'Resuming…' : 'Resume'}</button>)}
          <button className="primary" onClick={onConnect}><Icon name="plug" size={14} /> Connect an agent</button>
        </span>
      </div>
      {sb.statusDescription && <p className="sbx-desc">{sb.statusDescription}</p>}
      <div className="facts">
        <span>image <b className="mono">{sb.image || 'the default'}</b></span>
        {sizeWords(sb) && <span>size <b>{sizeWords(sb)}</b></span>}
        {sb.attachable && <span><b>attachable</b></span>}
        <span>created <b><Ago iso={sb.createdAt} /></b></span>
        <span>last activity <b><Ago iso={sb.updatedAt} /></b></span>
        {Number(sb.checkpointEpoch) > 0 && (
          <span title={sb.checkpointHead || ''}>checkpoint <b>epoch {sb.checkpointEpoch}</b>{sb.checkpointAt && <>, <Ago iso={sb.checkpointAt} /></>}</span>)}
      </div>
    </header>
  );
}

function SandboxPage({ tenant, sb, tab, busy, act, err, navigate, base, can: c, fresh, onFreshSeen }) {
  const here = `${base}/${sb.sandboxId}`;
  const tabs = sandboxTabs(c);
  const cur = activeTab(tab, c);
  const go = (t) => navigate(t === 'overview' ? here : `${here}/${t}`);
  const load = useCallback((source) => api.listActivity(sb.sandboxId, { source }), [sb.sandboxId]);

  return (
    <div className="sbx-page">
      <SandboxHead sb={sb} tenant={tenant} busy={busy} act={act} write={c.writeTenant} onConnect={() => go('connect')} />
      <section className="pane pane-detail">
        <div className="tabbar">
          {tabs.map((t) => (
            <button key={t.key} className={'tab' + (cur === t.key ? ' on' : '')} onClick={() => go(t.key)}>
              <Icon name={t.icon} size={14} /> {t.label}
            </button>
          ))}
        </div>
        <div className="tabbody">
          <ErrBox error={err} />
          {cur === 'overview' && <Overview sb={sb} tenant={tenant} can={c} navigate={navigate} />}
          {cur === 'connect' && <Connect sandboxId={sb.sandboxId} write={c.writeTenant} fresh={fresh} onFreshSeen={onFreshSeen} />}
          {cur === 'credentials' && (
            <>
              <Access sandboxId={sb.sandboxId} />
              <h3 className="sect"><Icon name="key" size={13} /> Image credentials</h3>
              <ImageCredentials sandboxId={sb.sandboxId} write={c.writeTenant} />
            </>)}
          {cur === 'network' && <Network sandboxId={sb.sandboxId} />}
          {cur === 'activity' && <Activity load={load} />}
          {cur === 'settings' && <Settings sb={sb} busy={busy} act={act} write={c.writeTenant} navigate={navigate} base={base} />}
        </div>
      </section>
    </div>
  );
}

// Overview is the facts and what the sandbox does when idle.
export function Overview({ sb, tenant, can: c, navigate }) {
  const node = sb.node && c.operator
    ? <a href={`/nodes/${encodeURIComponent(sb.node)}`} onClick={(e) => { e.preventDefault(); navigate(`/nodes/${encodeURIComponent(sb.node)}`); }}>{sb.node}</a>
    : sb.node || '—';
  return (
    <div className="sbx-overview">
      <div className="kv-grid sbx-kv">
        <span className="k">state</span>
        <span className="v"><span className="mono">{sb.clientState}</span> · {stateWords(sb).long}</span>
        {sb.statusDescription && <><span className="k">status</span><span className="v small">{sb.statusDescription}</span></>}
        <span className="k">sandbox ID</span><span className="v mono">{sb.sandboxId} <CopyBtn text={sb.sandboxId} mini /></span>
        <span className="k">owner</span><span className="v">{ownerShown(sb, tenant) || <span className="muted">the tenant {tenant}</span>}</span>
        <span className="k">image</span><span className="v mono">{sb.image || 'the default image'}</span>
        <span className="k">node</span><span className="v mono">{node}</span>
        <span className="k">size</span><span className="v">{sizeWords(sb) || 'the default'}</span>
        <span className="k">attachable</span>
        <span className="v">{sb.attachable
          ? 'yes · an environment\'s attached service may run in it'
          : <span className="muted">no · fixed at its create</span>}</span>
        <span className="k">created</span><span className="v">{fmtDate(sb.createdAt)} <Ago iso={sb.createdAt} /></span>
        <span className="k">last activity</span><span className="v">{fmtDate(sb.updatedAt)} <Ago iso={sb.updatedAt} /></span>
        {Number(sb.checkpointEpoch) > 0 && (
          <>
            <span className="k">checkpoint</span>
            <span className="v" title={sb.checkpointHead || ''}>
              epoch {sb.checkpointEpoch} in the object store {sb.checkpointAt && <Ago iso={sb.checkpointAt} />}
            </span>
          </>)}
      </div>
      <IdleRules />
    </div>
  );
}

// IdleRules say what a sandbox does when nothing uses it, as the control
// plane's policies have it (docs/architecture/control-plane.md, Policies
// and Ensure).
export function IdleRules() {
  return (
    <div className="sbx-idle">
      <h3 className="sect"><Icon name="history" size={13} /> When it is idle</h3>
      <ul>
        <li><span className="sbx-li-dot"><StateDot state="paused" /></span>
          <span><b>It pauses</b> after a while without a call, as the platform sets it (<code className="mono">--idle-pause</code>,
            10 s unless set otherwise). Paused, it stays on its node and books no room there.</span></li>
        <li><span className="sbx-li-dot"><StateDot state="hibernated" /></span>
          <span><b>It hibernates</b> after it has been paused for a while, where the platform sets one
            (<code className="mono">--hibernate-after</code>, off unless set): its disk and memory go to the object store,
            and it comes back on any node of that store.</span></li>
        <li><span className="sbx-li-dot"><StateDot state="running" /></span>
          <span><b>A call wakes it</b>: a tool call, a port forward or Resume brings it back, and the call waits until it runs.
            While a call or a forward runs, it stays awake.</span></li>
        <li><span className="sbx-li-dot"><StateDot state="running" /></span>
          <span><b>A process holds it awake</b>: one started with <code className="mono">process_start</code> until it ends or
            its timeout passes (timeout 0: until it is stopped), and an environment's attached service while it runs.</span></li>
      </ul>
    </div>
  );
}

// Settings has the rare and destructive actions: delete.
function Settings({ sb, busy, act, write, navigate, base }) {
  const [armed, arm, disarm] = useArm();
  return (
    <div className="sbx-settings">
      <div className="sbx-danger">
        <div>
          <b>Delete this sandbox</b>
          <p className="muted small">Its pod goes from its node, with what is on its disk, its tokens and its bindings; an agent
            connected to it is cut off. This cannot be undone.</p>
        </div>
        <ArmButton armKey={sb.sandboxId} armed={armed} arm={arm} disarm={disarm} icon="trash"
          confirmLabel="really delete?" disabled={!write || !!busy} title={write ? undefined : WRITE_REASON}
          onConfirm={() => act('delete', async () => {
            await api.deleteSandbox(sb.sandboxId);
            navigate(base);
          })}>Delete sandbox</ArmButton>
      </div>
    </div>
  );
}
