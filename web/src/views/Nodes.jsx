// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { Ago, ArmButton, ErrBox, LiveBadge, useArm, useNow, usePoll } from '../components.jsx';
import { fmtNs, nsToDate } from '../format.js';
import { canDrain, fleetDefaults, fmtBytes, memBar, nodeRows, stopping } from './fleet.js';
import { capacityBars, fleetCapacity, nodeDot, nodeVersions, versionSpread } from './platformData.js';
import { CapBars, DotWord, Fact, PageHead } from './platformParts.jsx';

// The nodes: one row per node that joined, with what placement booked on
// it against what it may book, its telemetry, its versions, its link to
// this instance and its certificate. Refreshed every 5 s. A row opens the
// node, where a drain shows first what it moves.

export function Nodes({ navigate }) {
  const [nodes, setNodes] = useState(null);
  const [defaults, setDefaults] = useState(null);
  const [err, setErr] = useState('');
  const [armed, arm, disarm] = useArm();
  const now = useNow(5000);

  const load = useCallback(async () => {
    try {
      const res = await api.listNodes();
      setNodes(res.nodes || []);
      setDefaults(res.defaults || null);
      setErr('');
    } catch (e) {
      setErr(e.message);
    }
  }, []);
  const lastOk = usePoll(load, 5000);

  // An undrain from the list; a drain opens the node, which shows first
  // what it moves.
  const undrain = async (r) => {
    try {
      await api.nodeUndrain(r.node);
    } catch (e) {
      setErr(e.message);
    }
    await load();
  };

  const rows = nodeRows(nodes);
  const count = (f) => rows.filter(f).length;
  const ready = count((r) => r.status === 'ready' && !r.draining);
  const down = count((r) => r.status === 'down');
  const draining = count((r) => r.draining && r.status !== 'down' && r.status !== 'removed');
  const joined = count((r) => r.status === 'joined');
  const removed = count((r) => r.status === 'removed');
  const feosd = versionSpread(nodes);
  const fleet = fleetCapacity(rows);
  const open = (r, tab = '') => navigate(`/nodes/${encodeURIComponent(r.node)}${tab ? '/' + tab : ''}`);
  const join = (e) => { e.preventDefault(); navigate('/join-tokens?kind=node'); };

  return (
    <div className="platform">
      <PageHead title="Nodes"
        state={nodes && <LiveBadge lastOk={lastOk} />}
        actions={<a className="button primary" href="/join-tokens?kind=node" onClick={join}><Icon name="ticket" size={14} /> Join a node</a>}
        facts={nodes && <>
          <Fact n={rows.length - removed}>node{rows.length - removed === 1 ? '' : 's'}</Fact>
          <Fact className="ok" n={ready}>ready</Fact>
          {joined > 0 && <Fact n={joined}>joined</Fact>}
          {draining > 0 && <Fact className="warn" n={draining}>draining</Fact>}
          {down > 0 && <Fact className="bad" n={down}>down</Fact>}
          {removed > 0 && <Fact n={removed}>removed</Fact>}
          {feosd.length > 0 && (
            <span className={feosd.length > 1 ? 'warn' : ''} title={feosd.length > 1 ? 'more than one feosd version: a rollout under way, or a node left behind' : ''}>
              feosd {feosd.map((v) => (feosd.length > 1 ? `${v.version} ×${v.count}` : v.version)).join(', ')}
            </span>)}
        </>} />

      {fleet.nodes > 0 && (
        <section className="pane">
          <div className="pane-toolbar">
            <h2><Icon name="gauge" size={14} /> Booked across the fleet</h2>
            <span className="muted small">the {fleet.nodes} ready node{fleet.nodes === 1 ? '' : 's'} placement books on</span>
          </div>
          <div className="fleet-cap"><CapBars bars={fleet.bars} big /></div>
        </section>)}

      <section className="pane">
        <ErrBox error={err} />
        <NodesTable rows={rows} now={now} armed={armed} arm={arm} disarm={disarm} onUndrain={undrain} onOpen={open} />
        {nodes && rows.length === 0 && (
          <p className="empty">
            No node has joined yet. A node joins with a join token:{' '}
            <a href="/join-tokens?kind=node" onClick={join}>make one →</a>
          </p>
        )}
        {!nodes && !err && <p className="empty">Loading…</p>}
        <NodesFootnote defaults={defaults} />
      </section>
    </div>
  );
}

// NodesTable is the nodes' table: rows from nodeRows, now in ms for the
// restart notes, the arm state of the undrain buttons, what an undrain
// and a click on a row do (onOpen(row, tab)). A drain opens the node,
// whose page shows what it moves. It scrolls sideways in a narrow window.
export function NodesTable({ rows, now, armed, arm, disarm, onUndrain, onOpen }) {
  return (
    <div className="tblwrap"><table className="tbl nodes-tbl">
      <thead>
        <tr>
          <th>node</th>
          <th title="what placement booked on the node, against what it may book: vCPUs, memory, sandboxes against slots">booked</th>
          <th title="the host's memory in use of its total, load1, disk free">host</th>
          <th title="sandboxes on the node, from its telemetry">running / paused</th>
          <th>versions</th>
          <th title="the node's last telemetry; connected to the instance that answered, and its node connections there">seen</th>
          <th>certificate</th>
          <th className="col-quick" />
        </tr>
      </thead>
      <tbody>
        {rows.map((r) => {
          const mem = memBar(r);
          const st = nodeDot(r);
          const versions = nodeVersions(r.versions).filter(([k]) => k === 'feosd' || k === 'kernel');
          return (
            <tr key={r.key} className={'clickable ' + r.cls} onClick={() => onOpen(r)}>
              <td className="col-node">
                <span className="mono strong">{r.node}</span>
                <div><DotWord st={st} /></div>
                {r.hostname && r.hostname !== r.node && <div className="muted small mono">{r.hostname}</div>}
                {r.identityForked && <div className="bad small" title="two holders of the node's identity were seen">identity forked</div>}
                {stopping(r, now) && <div className="muted small">feosd restarting</div>}
                {r.description && <div className="muted small">{r.description}</div>}
              </td>
              <td>{r.status === 'joined' && !r.lastSeenNs
                ? <span className="muted small">no telemetry yet</span>
                : <CapBars bars={capacityBars(r)} />}</td>
              <td className="small nowrap">
                <div className="membar" title={mem.title}>
                  <div className="membar-fill" style={{ width: r.memPct + '%' }} />
                  <span className="membar-label mono">{mem.label}</span>
                </div>
                <div className="mono muted">load {r.load1.toFixed(2)} · {fmtBytes(r.diskAvail)} free</div>
              </td>
              <td className="mono nowrap">{r.running} / {r.paused}</td>
              <td>
                {versions.length
                  ? <div className="pf-versions mono">{versions.map(([k, v]) => <React.Fragment key={k}><span className="k">{k}</span><span>{v}</span></React.Fragment>)}</div>
                  : <span className="muted">—</span>}
              </td>
              <td className="small nowrap">
                <div className="muted" title={nsToDate(r.lastSeenNs)}>{r.lastSeenNs ? fmtNs(r.lastSeenNs) : '—'}</div>
                {r.connected
                  ? <div className="ok" title={`connected to this instance, ${r.connections} node connection${r.connections === 1 ? '' : 's'}`}>connected{r.connections > 1 ? ` ×${r.connections}` : ''}</div>
                  : r.status === 'down' || r.status === 'removed'
                    ? <div className="muted">not connected</div>
                    : <div className="muted" title="not connected to this instance; it may be to another">—</div>}
              </td>
              <td className="small nowrap">
                {r.certSerial
                  ? <><span className="mono muted" title={r.certSerial}>{r.certSerial.slice(0, 8)}</span>
                    {r.status === 'removed'
                      ? <div className="muted">revoked</div>
                      : r.certNotAfter && <div>expires <Ago iso={r.certNotAfter} /></div>}</>
                  : <span className="muted">—</span>}
              </td>
              <td className="col-quick" onClick={(e) => e.stopPropagation()}>
                {canDrain(r) && r.draining && (
                  <ArmButton armKey={'undrain:' + r.node} armed={armed} arm={arm} disarm={disarm}
                    className="mini" onConfirm={() => onUndrain(r)} confirmLabel="undrain?">undrain</ArmButton>
                )}
                {canDrain(r) && !r.draining && (
                  <button className="mini" title="see what a drain moves, then drain" onClick={() => onOpen(r, 'overview?drain=1')}>drain…</button>
                )}
                <button className="iconbtn mini" title="open the node" onClick={() => onOpen(r)}>
                  <Icon name="chevronRight" size={13} /></button>
              </td>
            </tr>
          );
        })}
      </tbody>
    </table></div>
  );
}

// Flag names one of ferund's flags with this instance's value, when the
// API told it.
function Flag({ name, value }) {
  return <><code>{name}</code>{value ? ` (${value})` : ''}</>;
}

// NodesFootnote explains the statuses, the bookings and a drain with the
// timings of the instance that answered (GET /api/v1/admin/nodes's
// defaults), not with built-in numbers.
export function NodesFootnote({ defaults }) {
  const fd = fleetDefaults(defaults);
  return (
    <p className="muted small footnote">
      A node is <code>joined</code> from its join until its first telemetry, then <code>ready</code>; it reports
      telemetry{fd.telemetryInterval ? ` every ${fd.telemetryInterval}` : ''} and goes <code>down</code> when its
      telemetry stops for longer than ferund's <Flag name="--node-ttl" value={fd.nodeTtl} />; once it has been
      down for <Flag name="--lost-grace" value={fd.lostGrace} />, its sandboxes with a checkpoint
      become <code>hibernated</code> and are restored elsewhere on their next use, those without one
      count as <code>lost</code>. It is{' '}
      <code>removed</code> when an operator removes it. Memory is the host's. <code>booked</code> is what
      placement promised the node's running and starting sandboxes, against floor(cores × CPU overcommit)
      vCPUs (∞: CPU not gated) and memory less the headroom; a paused sandbox books nothing. A drain
      hibernates every sandbox of the node into the object store and keeps placement off it; execs that still
      run after <Flag name="--drain-timeout" value={fd.drainTimeout} /> are cancelled. Drain, wait for the node
      to be empty, act, undrain.
    </p>
  );
}
