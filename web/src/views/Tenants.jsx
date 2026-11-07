// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useEffect, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { Ago, ArmButton, ErrBox, LiveBadge, Secret, useArm, useNow, usePoll } from '../components.jsx';
import { fmtDate } from '../format.js';
import { QUOTA_ITEMS, ROLES, fmtQuota, useMe } from '../people.js';
import { tenantDeleteText, tokenRows } from './fleet.js';
import { knownZones, limitsBody, limitsForm, limitsText, zoneMoveText } from './platformData.js';
import { Fact, PageHead } from './platformParts.jsx';

// The operators' tenants: every tenant with its counts, limits and zone;
// create, delete, edit the limits and the zone, and the tenant's tokens.
// ferund stores a tenant's tokens as hashes only, so a token is shown
// once, when it is made, for a client such as FeCode; the browser keeps
// none. "Open as operator" works in the tenant with the session, as its
// admin.

export function Tenants({ navigate }) {
  const { reload } = useMe();
  const [tenants, setTenants] = useState(null);
  const [people, setPeople] = useState(null);
  const [err, setErr] = useState('');
  const [name, setName] = useState('');
  // /tenants?new opens the form (the switcher's New tenant).
  const [showCreate, setShowCreate] = useState(() => new URLSearchParams(location.search).has('new'));
  const [created, setCreated] = useState(null); // {name, token}
  const [open, setOpen] = useState(null); // {name, tab}
  const [armed, arm, disarm] = useArm();
  useNow(5000);

  const refresh = useCallback(async () => {
    try {
      setTenants((await api.listTenants()).tenants || []);
      setErr('');
    } catch (e) {
      setErr(e.message);
    }
  }, []);
  const lastOk = usePoll(refresh, 5000);
  // The people name a tenant's maker; without them the ID shows.
  useEffect(() => {
    api.listPeople().then((r) => setPeople(r.people || []), () => setPeople([]));
  }, []);

  const go = (t) => navigate(`/tenants/${encodeURIComponent(t)}`);
  const all = tenants || [];
  const zones = knownZones(all);
  const maker = (id) => {
    const p = (people || []).find((x) => x.id === id);
    return p ? p.name || p.email : id.slice(0, 8);
  };
  const toggle = (t, tab) => setOpen(open && open.name === t && open.tab === tab ? null : { name: t, tab });

  return (
    <div className="platform">
      <PageHead title="Tenants" state={tenants && <LiveBadge lastOk={lastOk} />}
        actions={<button className={'primary' + (showCreate ? ' on' : '')} onClick={() => setShowCreate(!showCreate)}>
          <Icon name="plus" size={14} /> New tenant</button>}
        facts={tenants && <>
          <Fact n={all.length}>tenant{all.length === 1 ? '' : 's'}</Fact>
          <Fact n={all.filter((t) => t.madeBy).length}>made by people</Fact>
          <Fact n={all.reduce((a, t) => a + (Number(t.sandboxCount) || 0), 0)}>sandboxes</Fact>
          <Fact n={all.reduce((a, t) => a + (Number(t.environmentCount) || 0), 0)}>environments</Fact>
          {all.some((t) => t.deleting) && <Fact className="warn" n={all.filter((t) => t.deleting).length}>being deleted</Fact>}
        </>} />

      {(showCreate || created) && (
        <section className="pane">
          {showCreate && (
            <form className="pf-form" onSubmit={async (e) => {
              e.preventDefault();
              try {
                const n = name.trim();
                const res = await api.createTenant(n);
                setCreated({ name: n, token: res.token });
                reload().catch(() => {});
                setName('');
                setShowCreate(false);
                await refresh();
              } catch (e2) {
                setErr(e2.message);
              }
            }}>
              <input className="mono" placeholder="tenant name: lowercase letters, digits, '.' and '-'" value={name} autoFocus
                onChange={(e) => setName(e.target.value)} />
              <button type="submit" className="primary" disabled={!name.trim()}><Icon name="plus" size={14} /> Create</button>
              <button type="button" className="ghost" onClick={() => setShowCreate(false)}><Icon name="x" size={14} /></button>
            </form>)}
          {showCreate && <p className="muted small">A tenant an operator makes has no limits, the apps zone, and a first token (admin), shown once.</p>}
          {created && (
            <Secret value={created.token} title={`Token of tenant ${created.name}`} onDismiss={() => setCreated(null)}>
              <p className="muted small">For a client such as FeCode; this browser keeps it not.</p>
            </Secret>)}
        </section>)}

      <section className="pane">
        <ErrBox error={err} />
        <div className="tblwrap"><table className="tbl">
          <thead>
            <tr>
              <th>name</th>
              <th>zone</th>
              <th className="col-num">sandboxes</th>
              <th className="col-num">environments</th>
              <th>limits</th>
              <th className="col-num">tokens</th>
              <th className="col-when">created</th>
              <th className="col-quick" />
            </tr>
          </thead>
          <tbody>
            {all.map((t) => {
              const del = tenantDeleteText(t);
              const isOpen = open && open.name === t.name;
              const btn = (tab, icon, label, title) => (
                <button className={'ghost mini' + (isOpen && open.tab === tab ? ' on' : '')} title={title}
                  onClick={() => toggle(t.name, tab)}><Icon name={icon} size={13} /> {label}</button>);
              return (
                <React.Fragment key={t.name}>
                  <tr className={isOpen ? 'sel' : ''}>
                    <td>
                      <span className="mono strong">{t.name}</span>
                      {t.deleting && <span className="badge warn">being deleted</span>}
                      <div className="muted small nowrap">{t.madeBy ? <>by {maker(t.madeBy)}</> : 'by an operator'}</div>
                    </td>
                    <td className="mono small zone">{t.zone || <span className="muted">none served</span>}</td>
                    <td className="col-num mono">{t.sandboxCount}</td>
                    <td className="col-num mono">{t.environmentCount || 0}</td>
                    <td className="tenant-limits">{limitsText(t.limits)}</td>
                    <td className="col-num mono">{(t.tokens || []).length}</td>
                    <td className="col-when muted">{fmtDate(t.createdAt)}</td>
                    <td className="col-quick nowrap">
                      {btn('limits', 'gauge', 'limits', "the tenant's limits")}
                      {btn('zone', 'globe', 'zone', 'the zone of its default hosts')}
                      {btn('tokens', 'key', 'tokens', "the tenant's tokens")}
                      <button className="mini" title="work in this tenant, as its admin" onClick={() => go(t.name)}>
                        Open as operator <Icon name="chevronRight" size={12} /></button>
                      <ArmButton armKey={t.name} armed={armed} arm={arm} disarm={disarm} icon="trash"
                        className="mini" confirmLabel={del.confirm} title={del.title}
                        onConfirm={async () => {
                          try {
                            await api.deleteTenant(t.name);
                            await refresh();
                            reload().catch(() => {});
                          } catch (e) {
                            setErr(e.message);
                          }
                        }} />
                    </td>
                  </tr>
                  {isOpen && (
                    <tr className="subrow">
                      <td colSpan={8}>
                        <div className="pf-sub">
                          {open.tab === 'limits' && <TenantLimits tenant={t} onChange={refresh} />}
                          {open.tab === 'zone' && <TenantZone tenant={t} zones={zones} onChange={refresh} />}
                          {open.tab === 'tokens' && <TenantTokens tenant={t.name} onChange={refresh} />}
                        </div>
                      </td>
                    </tr>
                  )}
                </React.Fragment>
              );
            })}
          </tbody>
        </table></div>
        {tenants && tenants.length === 0 && <p className="empty">No tenants yet.</p>}
        {!tenants && !err && <p className="empty">Loading…</p>}

        <p className="hint">
          A tenant's token acts in the tenant with its role, for a client such as FeCode's backend; people work in it
          with their sign-in and the grants its admins give. Deleting a tenant revokes its tokens at once and deletes
          all its sandboxes and environments.
        </p>
      </section>
    </div>
  );
}

// TenantLimits edits a tenant's limits (ADR 0031): an empty field is no
// limit, 0 is a limit. A save replaces all of them.
export function TenantLimits({ tenant, onChange }) {
  const [form, setForm] = useState(() => limitsForm(tenant.limits));
  const [err, setErr] = useState('');
  const [msg, setMsg] = useState('');
  const [busy, setBusy] = useState(false);
  const set = (k) => (e) => { setMsg(''); setForm({ ...form, [k]: e.target.value }); };

  return (
    <form onSubmit={async (e) => {
      e.preventDefault();
      let body;
      try {
        body = limitsBody(form);
      } catch (e2) {
        setErr(e2.message);
        return;
      }
      setBusy(true);
      try {
        const res = await api.setTenantLimits(tenant.name, body);
        setForm(limitsForm(res.limits));
        setErr('');
        setMsg('saved');
        await onChange();
      } catch (e2) {
        setErr(e2.message);
      } finally {
        setBusy(false);
      }
    }}>
      <div className="limits-grid">
        {QUOTA_ITEMS.map((it) => (
          <label key={it.key}>
            {it.label}{it.unit ? ` (${it.unit})` : ''}
            <input className="num mono" value={form[it.key]} onChange={set(it.key)} inputMode="numeric" placeholder="no limit" />
            {it.unit && form[it.key] && /^\d+$/.test(form[it.key].trim()) && (
              <span className="use mono">{fmtQuota(it, Number(form[it.key]))}</span>)}
          </label>
        ))}
      </div>
      <ErrBox error={err} />
      <div className="pf-form">
        <button type="submit" className="primary" disabled={busy}>Save limits</button>
        <button type="button" className="ghost" onClick={() => { setForm(limitsForm(tenant.limits)); setMsg(''); setErr(''); }}>Reset</button>
        {msg && <span className="ok small">{msg}</span>}
      </div>
      <p className="muted small">
        Empty: no limit; 0 is a limit. What runs over a lowered limit stays; the next create that asks for more is
        refused (<code>quota_exceeded</code>). vCPUs and memory sum the tenant's sandboxes, its environments' services,
        addons and running jobs.
      </p>
    </form>
  );
}

// TenantZone moves a tenant to another zone, and with it its
// environments' default hosts: the old hosts answer 404 at once, so the
// move is armed. The answer lists the environments whose hosts changed.
export function TenantZone({ tenant, zones, onChange }) {
  const [zone, setZone] = useState(tenant.zone || '');
  const [moved, setMoved] = useState(null);
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);
  const [armed, arm, disarm] = useArm();
  const changed = zone.trim() && zone.trim() !== tenant.zone;

  return (
    <div>
      <div className="pf-form">
        <span className="muted small">zone of its default hosts</span>
        <input className="mono" list={`zones-${tenant.name}`} value={zone} onChange={(e) => { setZone(e.target.value); setMoved(null); }}
          placeholder="the apps zone's or the tenant zone's name" />
        <datalist id={`zones-${tenant.name}`}>{zones.map((z) => <option key={z} value={z} />)}</datalist>
        <ArmButton armKey="zone" armed={armed} arm={arm} disarm={disarm} disabled={busy || !changed}
          className="zone-move" confirmLabel="move? old hosts answer 404"
          title={changed ? 'move the tenant and its environments\' default hosts' : 'pick another zone'}
          onConfirm={async () => {
            setBusy(true);
            try {
              const res = await api.setTenantZone(tenant.name, zone.trim());
              setMoved(res);
              setErr('');
              await onChange();
            } catch (e) {
              setErr(e.message);
            } finally {
              setBusy(false);
            }
          }}>Move to this zone</ArmButton>
      </div>
      <ErrBox error={err} />
      {moved && (
        <div>
          <p className="small"><span className="ok">Moved</span> to <span className="mono zone">{moved.zone}</span>.{' '}
            {moved.environments.length === 0 ? 'No environment holds a host label: no host changed.' : 'The hosts that changed:'}</p>
          {moved.environments.length > 0 && (
            <ul className="zone-moves">
              {moved.environments.map((m) => <li key={m.id}><span className="mono strong">{m.name}</span>: <span className="mono">{zoneMoveText(m)}</span></li>)}
            </ul>)}
        </div>)}
      <p className="muted small">
        Every environment that holds a host label moves it into the new zone: kept where it is free there, else its
        natural label or a fallback. The old hosts answer 404 at once; the labels left behind stay the tenant's in
        their zone. Domains are not touched.
      </p>
    </div>
  );
}

// TenantTokens lists, mints and revokes a tenant's tokens: a token has a
// role, on the tenant or on one environment (by name or ID). A minted
// token is shown once.
export function TenantTokens({ tenant, onChange }) {
  const [tokens, setTokens] = useState(null);
  const [label, setLabel] = useState('');
  const [role, setRole] = useState('admin');
  const [environment, setEnvironment] = useState('');
  const [fresh, setFresh] = useState(null);
  const [err, setErr] = useState('');
  const [armed, arm, disarm] = useArm();

  const refresh = useCallback(async () => {
    try {
      setTokens((await api.listTenantTokens(tenant)).tokens || []);
      setErr('');
    } catch (e) {
      setErr(e.message);
    }
  }, [tenant]);
  useEffect(() => { refresh(); }, [refresh]);

  const rows = tokenRows(tokens);

  return (
    <div>
      <ErrBox error={err} />
      {fresh && (
        <Secret value={fresh.token} title={`New token of tenant ${tenant}`} onDismiss={() => setFresh(null)}>
          <p className="muted small">Role {fresh.role}{fresh.environment ? ` on environment ${fresh.environment}` : ' on the tenant'}.</p>
        </Secret>)}
      <div className="tblwrap"><table className="tbl">
        <thead>
          <tr><th>hash</th><th>label</th><th>role</th><th>made by</th><th className="col-when">created</th><th className="col-when">last used</th><th className="col-quick" /></tr>
        </thead>
        <tbody>
          {rows.map((t) => (
            <tr key={t.hash}>
              <td className="mono muted" title={t.hash}>{t.hashPrefix}…</td>
              <td>{t.label || <span className="muted">(no label)</span>}</td>
              <td className="small">{t.role || 'admin'}{t.environment ? <span className="muted"> on <span className="mono" title={t.environment}>{t.environment.slice(0, 8)}</span></span> : ''}</td>
              <td className="small muted">{t.madeBy || '—'}</td>
              <td className="col-when muted">{fmtDate(t.createdAt)}</td>
              <td className="col-when">{t.lastUsedAt ? <Ago iso={t.lastUsedAt} /> : <span className="muted">never</span>}</td>
              <td className="col-quick">
                <ArmButton armKey={t.hash} armed={armed} arm={arm} disarm={disarm} icon="trash"
                  className="mini" confirmLabel="revoke?"
                  onConfirm={async () => {
                    try {
                      await api.revokeTenantToken(tenant, t.hash);
                      await refresh();
                      onChange();
                    } catch (e) {
                      setErr(e.message);
                    }
                  }}>Revoke</ArmButton>
              </td>
            </tr>
          ))}
        </tbody>
      </table></div>
      {tokens && tokens.length === 0 && <p className="empty">No tokens: nothing but people can act in this tenant.</p>}
      <form className="pf-form" style={{ marginTop: 10 }} onSubmit={async (e) => {
        e.preventDefault();
        try {
          const body = { label: label.trim(), role, environment: environment.trim() };
          const res = await api.mintTenantToken(tenant, body);
          setFresh({ token: res.token, role, environment: body.environment });
          setLabel('');
          setEnvironment('');
          await refresh();
          onChange();
        } catch (e2) {
          setErr(e2.message);
        }
      }}>
        <input placeholder="label (e.g. fecode)" value={label} onChange={(e) => setLabel(e.target.value)} />
        <select value={role} onChange={(e) => setRole(e.target.value)} title="the token's role">
          {ROLES.map((r) => <option key={r} value={r}>{r}</option>)}
        </select>
        <input className="mono" placeholder="environment (empty: the tenant)" value={environment}
          onChange={(e) => setEnvironment(e.target.value)} title="an environment's name or ID: the token acts on it alone" />
        <button type="submit" className="primary"><Icon name="key" size={14} /> Mint a token</button>
      </form>
    </div>
  );
}
