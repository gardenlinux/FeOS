// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useMemo, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { Ago, ErrBox, LiveBadge, StateDot, useNow, usePoll } from '../components.jsx';
import { eventDescription, eventTitle, keyOf, levelOf } from '../events.jsx';
import { fmtNs, shortId } from '../format.js';
import { sandboxCounts } from './fleet.js';
import { envRole, useAccess } from '../people.js';
import { envPath } from '../routes.js';
import { QuotaCard, RoleBadge } from './People.jsx';

// A tenant at a glance: its sandboxes by state, the most recently active
// ones, the quota's use, and for operators one stream of the newest
// events of the busiest sandboxes (the activity log is theirs). A person
// with grants on environments alone sees those environments.

const FEED_SOURCES = 4; // how many of the most recently active sandboxes feed the stream
const FEED_TAIL = 40; // events per sandbox
const FEED_SHOW = 16; // events shown

// mostRecent orders sandboxes by their last activity, newest first.
export function mostRecent(sandboxes) {
  return [...(sandboxes || [])].sort((a, b) => (b.updatedAt || '').localeCompare(a.updatedAt || ''));
}

export function Overview({ tenant, navigate }) {
  const access = useAccess(tenant);
  if (!access.can.readTenant) return <EnvOnlyOverview tenant={tenant} access={access} navigate={navigate} />;
  return <TenantOverview tenant={tenant} navigate={navigate} operator={access.can.operator} role={access.role} />;
}

// EnvOnlyOverview is the overview of a person whose grants are on
// environments alone: those environments, with the role on each.
function EnvOnlyOverview({ tenant, access, navigate }) {
  return (
    <div className="overview">
      <div className="pane-toolbar">
        <h2><Icon name="dashboard" size={15} /> Overview <span className="muted dim">· tenant {tenant}</span></h2>
      </div>
      <section className="pane">
        <div className="pane-toolbar"><h2><Icon name="blocks" size={14} /> Your environments</h2></div>
        <div className="tblwrap"><table className="tbl">
          <tbody>
            {access.environments.map((e) => (
              <tr key={e.id} className="clickable" onClick={() => navigate(envPath(tenant, e.name))}>
                <td className="strong">{e.name}</td>
                <td><RoleBadge role={envRole(access, e.id)} /></td>
              </tr>
            ))}
          </tbody>
        </table></div>
        <p className="hint">Your grants are on these environments, not on the whole tenant.</p>
      </section>
    </div>
  );
}

function TenantOverview({ tenant, navigate, operator, role }) {
  const [sandboxes, setSandboxes] = useState(null);
  const [feed, setFeed] = useState(null);
  const [err, setErr] = useState('');
  const base = `/tenants/${encodeURIComponent(tenant)}/sandboxes`;

  const refresh = useCallback(async () => {
    try {
      const res = await api.listSandboxes();
      const list = res.sandboxes || [];
      setSandboxes(list);
      setErr('');
      if (!operator) return;
      const hot = mostRecent(list).slice(0, FEED_SOURCES);
      const chunks = await Promise.all(hot.map(async (sb) => {
        try {
          const r = await api.listActivity(sb.sandboxId);
          return (r.events || []).slice(-FEED_TAIL).map((ev) => ({ ev, sb }));
        } catch {
          return [];
        }
      }));
      setFeed(chunks.flat()
        .sort((a, b) => Number(b.ev.tsNs) - Number(a.ev.tsNs))
        .slice(0, FEED_SHOW));
    } catch (e) {
      setErr(e.message);
    }
  }, [operator]);
  const lastOk = usePoll(refresh, 4000);
  useNow(1000);

  const counts = useMemo(() => sandboxCounts(sandboxes), [sandboxes]);
  const nodes = useMemo(() => new Set((sandboxes || []).map((s) => s.node).filter(Boolean)).size, [sandboxes]);
  const hotList = useMemo(() => mostRecent(sandboxes).slice(0, 6), [sandboxes]);

  return (
    <div className="overview">
      <div className="pane-toolbar">
        <h2><Icon name="dashboard" size={15} /> Overview <span className="muted dim">· tenant {tenant}</span></h2>
        <span className="muted small">your role: <RoleBadge role={role} /></span>
        <span className="spacer" />
        <LiveBadge lastOk={lastOk} />
      </div>
      <ErrBox error={err} />

      <div className="statgrid">
        <StatCard label="sandboxes" value={sandboxes ? sandboxes.length : '…'} icon="box"
          hint={counts.other ? `${counts.other} pending or changing` : 'in the tenant'} />
        <StatCard label="running" value={counts.running} icon="activity" tone="ok" hint="their microVMs run" />
        <StatCard label="paused" value={counts.paused + counts.hibernated} icon="pause" tone={counts.hibernated ? 'acc' : 'warn'}
          hint={counts.hibernated ? `${counts.paused} on a node, ${counts.hibernated} hibernated` : 'suspended on their node'} />
        <StatCard label="errors" value={counts.error} icon="x" tone={counts.error ? 'bad' : ''} hint="failed or lost" />
        <StatCard label="nodes" value={nodes} icon="server" hint="that hold the tenant's sandboxes" />
      </div>

      <QuotaCard />

      <div className="overview-grid">
        <section className="pane">
          <div className="pane-toolbar">
            <h2><Icon name="box" size={14} /> Recently active</h2>
            <span className="spacer" />
            <button className="ghost mini" onClick={() => navigate(base)}>all →</button>
          </div>
          <div className="tblwrap"><table className="tbl">
            <tbody>
              {hotList.map((sb) => (
                <tr key={sb.sandboxId} className="clickable" onClick={() => navigate(`${base}/${sb.sandboxId}`)}>
                  <td className="col-state"><StateDot state={sb.clientState} /></td>
                  <td className="mono">{shortId(sb.sandboxId)}</td>
                  <td className="muted trunc">{sb.image || 'default'}</td>
                  <td className="muted mono">{sb.node || '—'}</td>
                  <td className="col-when"><Ago iso={sb.updatedAt} /></td>
                </tr>
              ))}
            </tbody>
          </table></div>
          {sandboxes && sandboxes.length === 0 && (
            <p className="empty">
              No sandboxes yet.{' '}
              <a href={base} onClick={(e) => { e.preventDefault(); navigate(base); }}>Create the first →</a>
            </p>)}
        </section>

        {operator && <section className="pane">
          <div className="pane-toolbar">
            <h2><Icon name="activity" size={14} /> Activity</h2>
            <span className="muted small">of the {FEED_SOURCES} most recently active sandboxes</span>
          </div>
          <div className="feedlist">
            {(feed || []).map(({ ev, sb }) => (
              <button key={sb.sandboxId + '/' + keyOf(ev)} className={'feedrow' + (levelOf(ev) === 'error' ? ' err' : '')}
                title={`${sb.sandboxId}: open its activity`}
                onClick={() => navigate(`${base}/${sb.sandboxId}/activity`)}>
                <span className="mono col-time">{fmtNs(ev.tsNs)}</span>
                <span className="mono sbxref">{shortId(sb.sandboxId)}</span>
                <span className={'evkind' + (levelOf(ev) === 'error' ? ' errtext' : '')}>{eventTitle(ev)}</span>
                <span className="evdesc">{eventDescription(ev)}</span>
              </button>
            ))}
            {feed && feed.length === 0 && <p className="empty">No events yet.</p>}
            {!feed && !err && <p className="empty">Loading…</p>}
          </div>
        </section>}
      </div>
    </div>
  );
}

function StatCard({ label, value, icon, hint, tone }) {
  return (
    <div className={'statcard' + (tone ? ' t-' + tone : '')}>
      <div className="statcard-top"><Icon name={icon} size={14} /><span>{label}</span></div>
      <div className="statcard-value">{value}</div>
      <div className="statcard-hint muted">{hint}</div>
    </div>
  );
}
