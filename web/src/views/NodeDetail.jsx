// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useEffect, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { Ago, ArmButton, CopyBtn, ErrBox, LiveBadge, StateLabel, useArm, useNow, usePoll } from '../components.jsx';
import { fmtDateTime, nsToIso } from '../format.js';
import { Activity } from './Activity.jsx';
import {
  canDrain, certState, fleetDefaults, fmtBytes, nodeRows, settingsForm, settingsPatch, stopping,
} from './fleet.js';
import { capacityBars, drainPreview, nodeDot, nodeVersions } from './platformData.js';
import { CapBars, DotWord, PageHead } from './platformParts.jsx';

// One node: a head with its state and its drain, what placement booked
// on it, everything ferund knows of it, its capacity settings and its
// removal; its certificates (the renewal history) and its activity (the
// node's own records, and those about pods that are not ferund's or not
// on it). The tab is the route's: /nodes/:node/:tab; ?drain=1 opens the
// drain's preview (the nodes' list links so).

const TABS = [
  { key: 'overview', label: 'Overview', icon: 'gauge' },
  { key: 'certs', label: 'Certificates', icon: 'shield' },
  { key: 'activity', label: 'Activity', icon: 'activity' },
];

export function NodeDetail({ node, tab, navigate }) {
  const [raw, setRaw] = useState(null);
  const [defaults, setDefaults] = useState(null);
  const [err, setErr] = useState('');
  const [preview, setPreview] = useState(() => typeof location !== 'undefined' && /[?&]drain=1/.test(location.search));
  const [armed, arm, disarm] = useArm();
  const [busy, setBusy] = useState(false);
  const base = `/nodes/${encodeURIComponent(node)}`;
  const activeTab = TABS.some((t) => t.key === tab) ? tab : 'overview';
  useNow(5000);

  const load = useCallback(async () => {
    try {
      const res = await api.listNodes();
      setRaw((res.nodes || []).find((n) => n.node === node) || false);
      setDefaults(res.defaults || null);
      setErr('');
    } catch (e) {
      setErr(e.message);
    }
  }, [node]);
  const lastOk = usePoll(load, 5000);
  const loadActivity = useCallback((source) => api.nodeActivity(node, { source }), [node]);

  const act = async (fn) => {
    setBusy(true);
    setErr('');
    try {
      await fn();
      await load();
    } catch (e) {
      setErr(e.message);
      return e;
    } finally {
      setBusy(false);
    }
    return null;
  };

  if (raw === false) {
    return (
      <section className="pane">
        <p className="error">No node {node}.</p>
        <button className="ghost" onClick={() => navigate('/nodes')}><Icon name="arrowLeft" size={14} /> the nodes</button>
      </section>
    );
  }
  const row = raw ? nodeRows([raw])[0] : null;
  const st = row ? nodeDot(row) : null;
  const fd = fleetDefaults(defaults);
  const drain = row ? drainPreview(row, fd.drainTimeout) : null;
  const feosd = row ? (nodeVersions(row.versions).find(([k]) => k === 'feosd') || [])[1] : '';

  return (
    <div className="platform">
      <PageHead kind="Node" title={node} mono
        state={st && <DotWord st={st} />}
        actions={<>
          <LiveBadge lastOk={lastOk} />
          {row && canDrain(row) && row.draining && (
            <ArmButton armKey="undrain" armed={armed} arm={arm} disarm={disarm} disabled={busy}
              title="placement books on the node again" confirmLabel="really undrain?"
              onConfirm={() => act(() => api.nodeUndrain(row.node))}>Undrain</ArmButton>)}
          {row && canDrain(row) && !row.draining && (
            <button className={preview ? 'on' : ''} disabled={busy} onClick={() => { setPreview(!preview); if (activeTab !== 'overview') navigate(base); }}
              title="see what a drain moves, then drain">Drain…</button>)}
        </>}
        facts={raw && <>
          <span className="mono">{raw.hostname || '—'}</span>
          <span>{row.cpus} cores · {raw.arch || '—'}</span>
          <span>{fmtBytes(row.memTotal)} memory</span>
          {feosd && <span>feosd <span className="mono">{feosd}</span></span>}
          <span>{row.connected ? <span className="ok">connected</span> : 'not connected to this instance'}</span>
          <span>seen <Ago iso={nsToIso(raw.lastSeenAtNs)} /></span>
        </>} />

      {row && preview && !row.draining && canDrain(row) && (
        <div className="drain-preview">
          <h3><Icon name="alert" size={14} /> What a drain of {node} moves</h3>
          <ul>{drain.lines.map((l) => <li key={l}>{l}</li>)}</ul>
          {drain.blocked && <p className="bad small">It cannot: {drain.blocked}.</p>}
          <div className="row">
            <button className="primary" disabled={busy || !!drain.blocked} title={drain.blocked || ''}
              onClick={async () => { const e = await act(() => api.nodeDrain(row.node)); if (!e) setPreview(false); }}>
              Drain {node}</button>
            <button className="ghost" onClick={() => setPreview(false)}>Cancel</button>
          </div>
        </div>)}

      <section className="pane pane-detail">
        <div className="tabbar">
          {TABS.map((t) => (
            <button key={t.key} className={'tab' + (activeTab === t.key ? ' on' : '')} onClick={() => navigate(`${base}/${t.key}`)}>
              <Icon name={t.icon} size={14} /> {t.label}
            </button>
          ))}
        </div>
        <ErrBox error={err} />
        {activeTab === 'overview' && raw && <NodeOverview raw={raw} row={row} act={act} busy={busy} armed={armed} arm={arm} disarm={disarm} />}
        {activeTab === 'overview' && !raw && !err && <p className="empty">Loading…</p>}
        {activeTab === 'certs' && <NodeCerts node={node} current={raw ? raw.certSerial : ''} />}
        {activeTab === 'activity' && <div className="tabbody"><Activity load={loadActivity} /></div>}
      </section>
    </div>
  );
}

function NodeOverview({ raw, row, act, busy, armed, arm, disarm }) {
  const [needsForce, setNeedsForce] = useState(false);
  const versions = nodeVersions(row.versions);
  const removed = row.status === 'removed';

  return (
    <div className="tabbody">
      <div className="pf-grid">
        <div>
          <h3 className="pf-sect">Booked</h3>
          <CapBars bars={capacityBars(row)} big />
          <p className="muted small">
            {row.running} running, {row.paused} paused on the node (telemetry); placement booked {row.booked}.
            {row.vcpusCapacity > 0 ? ` vCPUs: ${row.cpus} cores × ${row.cpuOvercommit}.` : ' CPU is not gated.'}
            {' '}Memory: the total less {fmtBytes(row.memHeadroom)} headroom.
          </p>
        </div>
        <div>
          <h3 className="pf-sect">Versions</h3>
          {versions.length
            ? <div className="pf-versions mono">{versions.map(([k, v]) => <React.Fragment key={k}><span className="k">{k}</span><span>{v}</span></React.Fragment>)}</div>
            : <p className="muted small">None reported yet.</p>}
        </div>
      </div>

      <h3 className="pf-sect" style={{ marginTop: 16 }}>Details</h3>
      <div className="kv-grid">
        <span className="k">status</span>
        <span className="v"><StateLabel state={row.status} />{row.description && <span className="muted small"> {row.description}</span>}</span>
        {row.draining && <><span className="k">draining</span><span className="v warn">since <Ago iso={row.drainStartedAt} /></span></>}
        {row.identityForked && <><span className="k">identity</span><span className="v bad">forked: two holders of the node's key were seen; re-join it with a pinned token</span></>}
        {stopping(row) && <><span className="k">feosd restarting</span><span className="v">until {fmtDateTime(row.stoppingUntil)}</span></>}
        {removed && <><span className="k">removed</span><span className="v">{fmtDateTime(row.removedAt)} <Ago iso={row.removedAt} /></span></>}
        <span className="k">link</span>
        <span className="v">{row.connected ? `connected to this instance (${row.connections} connection${row.connections === 1 ? '' : 's'})` : 'not connected to this instance'}</span>
        <span className="k">hostname</span><span className="v mono">{raw.hostname || '—'}</span>
        <span className="k">CPU</span><span className="v">{row.cpus} cores · {raw.cpuModelname || '—'} · {raw.arch || '—'}</span>
        <span className="k">memory</span><span className="v mono">{fmtBytes(row.memAvail)} available of {fmtBytes(row.memTotal)}</span>
        <span className="k">disk</span><span className="v mono">{fmtBytes(row.diskAvail)} free of {fmtBytes(raw.diskTotalbytes)}</span>
        <span className="k">load</span><span className="v mono">{row.load1.toFixed(2)}</span>
        <span className="k">object store</span>
        <span className="v">{row.objectStore ? <span className="mono">{row.objectStoreId || 'yes'}</span> : <span className="muted">none: its sandboxes cannot hibernate</span>}</span>
        <span className="k">pod prefix</span><span className="v mono">{raw.podPrefix || '—'}</span>
        {(row.meshPrefix || row.meshEndpoint) && <><span className="k">mesh</span>
          <span className="v mono net">{row.meshPrefix || '—'}{row.meshEndpoint && <span className="muted"> · endpoint {row.meshEndpoint}</span>}</span></>}
        <span className="k">inventory</span><span className="v mono muted">{row.inventoryHash || '—'}</span>
        <span className="k">feosd start</span>
        <span className="v mono">{raw.startId || '—'}{raw.startId && <CopyBtn text={raw.startId} mini />}</span>
        <span className="k">certificate</span>
        <span className="v mono small">
          {row.certSerial || '—'}
          {row.certNotAfter && <div className="muted">valid until {fmtDateTime(row.certNotAfter)} (<Ago iso={row.certNotAfter} />)</div>}
        </span>
        <span className="k">joined</span><span className="v">{fmtDateTime(nsToIso(raw.registeredAtNs))}</span>
        <span className="k">last seen</span><span className="v">{fmtDateTime(nsToIso(raw.lastSeenAtNs))} <Ago iso={nsToIso(raw.lastSeenAtNs)} /></span>
        {Number(raw.downSinceNs) > 0 && <><span className="k">down since</span><span className="v bad">{fmtDateTime(nsToIso(raw.downSinceNs))}</span></>}
      </div>

      {!removed && <Settings row={row} act={act} busy={busy} />}

      {!removed && (
        <div className="dangerzone">
          <p className="muted small">
            Removing the node revokes its certificates and closes its connections; it must join again to come back.
            {row.booked > 0 && ` ${row.booked} sandbox${row.booked === 1 ? ' is' : 'es are'} booked on it: drain it first, or remove it anyway, and they are marked lost and placed again elsewhere.`}
          </p>
          <div className="actionrow">
            {!needsForce && (
              <ArmButton armKey="remove" armed={armed} arm={arm} disarm={disarm} icon="trash" disabled={busy}
                title="revoke the node's certificates and close its connections; it must join again to come back"
                confirmLabel="really remove?"
                onConfirm={async () => {
                  const e = await act(() => api.removeNode(row.node, false));
                  if (e && e.status === 409) setNeedsForce(true);
                }}>Remove</ArmButton>
            )}
            {needsForce && (
              <ArmButton armKey="force" armed={armed} arm={arm} disarm={disarm} icon="trash" disabled={busy}
                title="its sandboxes are marked lost at once and placed again elsewhere"
                confirmLabel="remove with its sandboxes?"
                onConfirm={() => act(() => api.removeNode(row.node, true))}>Remove anyway</ArmButton>
            )}
          </div>
        </div>)}
    </div>
  );
}

// Settings edits the node's capacity settings: slots, the memory placement
// leaves free, and the CPU overcommit (0: CPU not gated). The form keeps
// the values it was filled from (base); a save sends the fields that
// differ from them.
function Settings({ row, act, busy }) {
  const [base, setBase] = useState(() => settingsForm(row));
  const [form, setForm] = useState(base);
  const [msg, setMsg] = useState('');
  const [dirty, setDirty] = useState(false);
  // The polls bring new values; an untouched form follows them, and a
  // saved or reset one takes the node's current values.
  useEffect(() => {
    if (dirty) return;
    const f = settingsForm(row);
    setBase(f);
    setForm(f);
  }, [dirty, row.slots, row.memHeadroom, row.cpuOvercommit]);
  const set = (k) => (e) => { setDirty(true); setForm({ ...form, [k]: e.target.value }); };

  return (
    <form className="settings" onSubmit={async (e) => {
      e.preventDefault();
      setMsg('');
      let patch;
      try {
        patch = settingsPatch(base, form);
      } catch (e2) {
        setMsg(e2.message);
        return;
      }
      if (!Object.keys(patch).length) { setMsg('nothing changed'); setDirty(false); return; }
      const failed = await act(() => api.patchNode(row.node, patch));
      if (!failed) { setDirty(false); setMsg('saved'); }
    }}>
      <h3 className="sect"><Icon name="settings" size={13} /> Capacity</h3>
      <div className="settings-grid">
        <label>slots<input className="num" value={form.slots} onChange={set('slots')} inputMode="numeric" /></label>
        <label>memory headroom (MiB)<input className="num" value={form.memHeadroomMiB} onChange={set('memHeadroomMiB')} inputMode="numeric" /></label>
        <label>CPU overcommit<input className="num" value={form.cpuOvercommit} onChange={set('cpuOvercommit')} inputMode="decimal" /></label>
        <button type="submit" className="primary" disabled={busy}>Save</button>
        {dirty && <button type="button" className="ghost" onClick={() => { setDirty(false); setMsg(''); }}>Reset</button>}
        {msg && <span className="muted small">{msg}</span>}
      </div>
      <p className="muted small">
        Placement books at most <code>slots</code> sandboxes, floor(cores × CPU overcommit) vCPUs (0: CPU not
        gated) and the memory less the headroom.
      </p>
    </form>
  );
}

function NodeCerts({ node, current }) {
  const [certs, setCerts] = useState(null);
  const [err, setErr] = useState('');
  useEffect(() => {
    let alive = true;
    api.nodeCerts(node)
      .then((res) => { if (alive) { setCerts(res.certs || []); setErr(''); } })
      .catch((e) => { if (alive) setErr(e.message); });
    return () => { alive = false; };
  }, [node, current]);

  return (
    <div className="tabbody">
      <ErrBox error={err} />
      <p className="muted small">The node's certificates, newest first: each renewal names the one it replaced.</p>
      <div className="tblwrap">
        <table className="tbl">
          <thead>
            <tr><th>serial</th><th>kind</th><th>issued</th><th>valid until</th><th>first seen</th><th>replaces</th><th>peer</th><th>state</th></tr>
          </thead>
          <tbody>
            {(certs || []).map((c) => {
              const st = certState(c);
              return (
                <tr key={c.serial} className={c.serial === current ? 'sel' : ''}>
                  <td className="mono small" title={c.serial}>
                    {c.serial.slice(0, 12)}…{c.serial === current && <span className="badge ok">current</span>}
                  </td>
                  <td className="small">{c.kind}</td>
                  <td className="small">{fmtDateTime(c.issuedAt)}</td>
                  <td className="small">{fmtDateTime(c.notAfter)}</td>
                  <td className="small">{c.firstSeenAt ? <Ago iso={c.firstSeenAt} /> : <span className="muted">never used</span>}</td>
                  <td className="mono small muted" title={c.previousSerial}>{c.previousSerial ? c.previousSerial.slice(0, 12) + '…' : '—'}</td>
                  <td className="mono small muted">{c.peer || '—'}</td>
                  <td className="small">
                    {st === 'revoked' && <span className="bad" title={fmtDateTime(c.revokedAt)}>revoked: {c.revokedReason || '—'}</span>}
                    {st === 'expired' && <span className="muted">expired</span>}
                    {st === 'expiring' && <span className="warn">expires <Ago iso={c.notAfter} /></span>}
                    {st === 'valid' && <span className="ok">valid</span>}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
      {certs && certs.length === 0 && <p className="empty">No certificates.</p>}
      {!certs && !err && <p className="empty">Loading…</p>}
    </div>
  );
}
