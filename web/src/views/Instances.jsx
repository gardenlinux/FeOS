// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { Ago, ErrBox, LiveBadge, StateDot, useNow, usePoll } from '../components.jsx';
import { fmtDateTime } from '../format.js';
import { instanceVersions } from './platformData.js';
import { Dot, Fact, PageHead } from './platformParts.jsx';

// The control plane: its instances (every ferund that serves, or served
// recently) and which of them leads, running the loops that only one may
// run.

// instanceState is an instance's dot: running while it is live; stopping
// while it is stopping but still writes its row, for it serves its
// requests and ops until its last step; gone once its row aged past the
// TTL. The database decides fresh and live, never the browser's clock.
export function instanceState(i) {
  if (i.live) return { state: 'running', title: 'live' };
  if (i.fresh) return { state: 'paused', title: 'stopping' };
  return { state: 'absent', title: 'gone' };
}

// InstancesTable lists the instances; a row is dimmed once the instance is
// gone.
export function InstancesTable({ instances, leader }) {
  return (
    <div className="tblwrap">
      <table className="tbl">
        <thead>
          <tr>
            <th className="col-state" /><th>instance</th><th>address</th><th>nodes' address</th>
            <th>version</th><th>started</th><th>last seen</th><th>stopping</th>
          </tr>
        </thead>
        <tbody>
          {instances.map((i) => {
            const st = instanceState(i);
            return (
              <tr key={i.id} className={st.state === 'absent' ? 'dim' : ''}>
                <td className="col-state"><StateDot state={st.state} title={st.title} /></td>
                <td className="mono">
                  {i.id}
                  {leader && leader.id === i.id && <span className="badge acc"><Icon name="crown" size={11} /> leader</span>}
                </td>
                <td className="mono small">{i.address || '—'}</td>
                <td className="mono small">{i.nodeAddress || <span className="muted" title="not listed to nodes">—</span>}</td>
                <td className="mono small">{i.version || '—'}</td>
                <td className="small">{fmtDateTime(i.startedAt)}</td>
                <td className="small"><Ago iso={i.lastSeen} /></td>
                <td className="small">{i.stopping ? <span className="warn">since <Ago iso={i.stopping} /></span> : <span className="muted">—</span>}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

export function Instances() {
  const [data, setData] = useState(null);
  const [err, setErr] = useState('');
  useNow(2000);

  const refresh = useCallback(async () => {
    try {
      setData(await api.listInstances());
      setErr('');
    } catch (e) {
      setErr(e.message);
    }
  }, []);
  const lastOk = usePoll(refresh, 5000);

  const leader = data && data.leader;
  const instances = (data && data.instances) || [];
  const live = instances.filter((i) => i.live).length;
  const stoppingN = instances.filter((i) => !i.live && i.fresh).length;
  const gone = instances.length - live - stoppingN;
  const versions = instanceVersions(instances);
  const leaderRow = leader && instances.find((i) => i.id === leader.id);

  return (
    <div className="platform">
      <PageHead title="Control plane"
        state={data && (leader
          ? <span className="dotword running" title="an instance holds the leader's lease"><Dot cls="running" />led</span>
          : <span className="dotword unknown" title="no instance holds the lease"><Dot cls="unknown" />no leader</span>)}
        actions={<LiveBadge lastOk={lastOk} />}
        facts={data && <>
          <Fact className="ok" n={live}>live</Fact>
          {stoppingN > 0 && <Fact className="warn" n={stoppingN}>stopping</Fact>}
          {gone > 0 && <Fact n={gone}>gone</Fact>}
          {versions.length > 0 && (
            <span className={versions.length > 1 ? 'warn' : ''} title={versions.length > 1 ? 'more than one version serves: a rollout under way' : ''}>
              ferund <span className="mono">{versions.map((v) => (versions.length > 1 ? `${v.version} ×${v.count}` : v.version)).join(', ')}</span>
            </span>)}
        </>} />

      {data && (
        <section className="pane">
          <div className="leader-card">
            <span className="crown"><Icon name="crown" size={18} /></span>
            {leader
              ? <>
                <div>
                  <div className="muted small">leader</div>
                  <div className="mono big">{leader.id}</div>
                </div>
                <div><div className="muted small">epoch</div><div className="mono big">{leader.epoch}</div></div>
                <div><div className="muted small">since</div><div>{fmtDateTime(leader.since)} <Ago iso={leader.since} /></div></div>
                {leaderRow && <div><div className="muted small">version</div><div className="mono">{leaderRow.version || '—'}</div></div>}
              </>
              : <span className="warn">No instance holds the leader's lease: the loops that only one may run (node expiry, idle
                pause, drains, repairs, tenant deletion) wait until one takes it.</span>}
          </div>
        </section>)}

      <section className="pane">
        <div className="pane-toolbar"><h2><Icon name="layers" size={14} /> Instances <span className="count">{data ? instances.length : '…'}</span></h2></div>
        <ErrBox error={err} />
        <InstancesTable instances={instances} leader={leader} />
        {data && instances.length === 0 && <p className="empty">No instances.</p>}
        {!data && !err && <p className="empty">Loading…</p>}
        <p className="muted small footnote">
          An instance writes its row every few seconds and counts as live while the row is fresh and it is not
          stopping. A stopping instance finishes its requests and ops, and stays listed to the nodes until it
          closes their connections; it is gone once its row is older than 15 s. The leader holds a lease in the
          database; each new holder takes the next epoch.
        </p>
      </section>
    </div>
  );
}
