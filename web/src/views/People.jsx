// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useEffect, useState } from 'react';
import { api, notAvailable } from '../api.js';
import { Icon } from '../Icon.jsx';
import { Ago, ArmButton, ErrBox, LiveBadge, Secret, useArm, useNow, usePoll } from '../components.jsx';
import { fmtDate } from '../format.js';
import {
  ROLES, grantableRoles, invitationState, inviteLink, myTenants, personLabel, quotaRows, fmtQuota, roleLabel, scopeLabel,
  tenantAccess, tenantHome, useAccess, useMe,
} from '../people.js';

// The people's pages (ADR 0031, "The UI is the tenants' console"): a
// person's tenants and a new one, a tenant's members and invitations, its
// tokens, the quota's use, and an invitation's acceptance. Each offers
// what the role allows; ferund decides. A ferund without these routes
// answers 404 "no such route": the page says it is not available.

// NotAvailable is a page whose routes this ferund does not have.
export function NotAvailable({ what }) {
  return (
    <p className="empty">
      <Icon name="alert" size={13} /> {what} are not available on this ferund: it has no route for them yet.
    </p>
  );
}

// ------------------------------------------------------------ my tenants --

// MyTenants is a person's tenants, from /auth/me, with their role, and
// the form for a new one.
export function MyTenants({ navigate }) {
  const { me, reload } = useMe();
  const tenants = myTenants(me);
  const [showCreate, setShowCreate] = useState(tenants.length === 0);
  const [name, setName] = useState('');
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);

  return (
    <div className="tenants single">
      <section className="pane">
        <div className="pane-toolbar">
          <h2><Icon name="users" size={15} /> Your tenants <span className="count">{tenants.length}</span></h2>
          <span className="spacer" />
          <button className={'ghost mini' + (showCreate ? ' on' : '')} onClick={() => setShowCreate(!showCreate)}>
            <Icon name="plus" size={13} /> New tenant
          </button>
        </div>

        {showCreate && (
          <form className="createpanel" onSubmit={async (e) => {
            e.preventDefault();
            const n = name.trim();
            setBusy(true);
            setErr('');
            try {
              await api.createOwnTenant(n);
              await reload();
              setName('');
              setShowCreate(false);
              navigate(`/tenants/${encodeURIComponent(n)}`);
            } catch (e2) {
              setErr(notAvailable(e2) ? 'Making tenants is not available on this ferund yet.' : e2.message);
            } finally {
              setBusy(false);
            }
          }}>
            <input placeholder="name: lowercase letters, digits and '-'" value={name} autoFocus
              aria-label="the new tenant's name" onChange={(e) => setName(e.target.value)} />
            <button type="submit" className="primary" disabled={busy || !name.trim()}>
              <Icon name="plus" size={14} /> Create</button>
          </form>
        )}
        {showCreate && (
          <p className="hint">
            The name is part of the tenant's hosts. You become its admin, and can invite others.
          </p>
        )}
        <ErrBox error={err} />

        <div className="tblwrap"><table className="tbl">
          <thead>
            <tr><th>tenant</th><th>your role</th><th>environments</th><th className="col-quick" /></tr>
          </thead>
          <tbody>
            {tenants.map((t) => (
              <tr key={t.tenant} className="clickable" onClick={() => navigate(tenantHome(tenantAccess(me, t.tenant)))}>
                <td className="strong">{t.tenant}</td>
                <td><RoleBadge role={roleLabel(t)} /></td>
                <td className="muted small">
                  {(t.environments || []).length === 0 ? (t.role ? 'all' : '—')
                    : t.environments.map((e) => `${e.name} (${e.role})`).join(', ')}
                </td>
                <td className="col-quick"><Icon name="chevronRight" size={13} /></td>
              </tr>
            ))}
          </tbody>
        </table></div>
        {tenants.length === 0 && (
          <p className="empty">
            You are a member of no tenant yet. Make one, or ask a tenant's admin for an invitation link.
          </p>
        )}
      </section>
    </div>
  );
}

// RoleBadge shows a role.
export function RoleBadge({ role }) {
  if (!role) return <span className="muted">—</span>;
  const tone = role === 'admin' ? 'acc' : role === 'write' ? 'ok' : '';
  return <span className={'badge role ' + tone}>{role}</span>;
}

// --------------------------------------------------------------- members --

function useEnvironments(enabled) {
  const [envs, setEnvs] = useState([]);
  useEffect(() => {
    if (!enabled) return undefined;
    let live = true;
    api.listEnvironments().then((r) => { if (live) setEnvs(r.environments || []); }, () => {});
    return () => { live = false; };
  }, [enabled]);
  return envs;
}

// Members is a tenant's members with their grants and, for its admin,
// the changes of grants and the invitations; every member may leave.
export function Members({ tenant, navigate }) {
  const { me, reload } = useMe();
  const access = useAccess(tenant);
  const c = access.can;
  const [members, setMembers] = useState(null);
  const [err, setErr] = useState('');
  const [missing, setMissing] = useState(false);
  const [armed, arm, disarm] = useArm();
  const envs = useEnvironments(c.adminTenant);
  useNow(10000);

  const refresh = useCallback(async () => {
    try {
      setMembers((await api.listMembers()).members || []);
      setErr('');
    } catch (e) {
      if (notAvailable(e)) setMissing(true);
      else setErr(e.message);
    }
  }, []);
  const lastOk = usePoll(refresh, 10000);

  const act = async (fn) => {
    setErr('');
    try {
      await fn();
      await refresh();
    } catch (e) {
      setErr(notAvailable(e) ? 'This change is not available on this ferund yet.' : e.message);
    }
  };

  return (
    <div className="single">
      <section className="pane">
        <div className="pane-toolbar">
          <h2><Icon name="users" size={15} /> Members {!missing && <span className="count">{members ? members.length : '…'}</span>}</h2>
          <span className="muted small">your role: <RoleBadge role={roleLabel(access)} /></span>
          <span className="spacer" />
          {!missing && <LiveBadge lastOk={lastOk} />}
        </div>
        <ErrBox error={err} />
        {missing && <NotAvailable what="A tenant's members" />}
        {!missing && (
          <div className="tblwrap"><table className="tbl members">
            <thead>
              <tr><th>person</th><th>on the tenant</th><th>on environments</th><th className="col-quick" /></tr>
            </thead>
            <tbody>
              {(members || []).map((m) => {
                const self = !!me && !!me.person && m.person === me.person;
                return (
                  <tr key={m.person}>
                    <td>
                      <span className="strong">{personLabel(m)}</span>
                      {self && <span className="badge">you</span>}
                      {m.email && m.name && <div className="muted small">{m.email}</div>}
                    </td>
                    <td>
                      {c.adminTenant && !self ? (
                        <select value={m.role || ''} aria-label={`${personLabel(m)}'s role on the tenant`}
                          onChange={(e) => act(() => (e.target.value
                            ? api.putGrant(m.person, { role: e.target.value })
                            : api.deleteGrant(m.person)))}>
                          <option value="">none</option>
                          {ROLES.map((r) => <option key={r} value={r}>{r}</option>)}
                        </select>
                      ) : <RoleBadge role={m.role} />}
                    </td>
                    <td className="small">
                      {(m.environments || []).map((e) => (
                        <span key={e.id} className="grantchip">
                          {e.name} · {e.role}
                          {c.adminTenant && !self && (
                            <button className="iconbtn mini" title={`remove the grant on ${e.name}`}
                              onClick={() => act(() => api.deleteGrant(m.person, e.id))}><Icon name="x" size={11} /></button>)}
                        </span>
                      ))}
                      {(m.environments || []).length === 0 && <span className="muted">—</span>}
                      {c.adminTenant && !self && envs.length > 0 && (
                        <EnvGrantForm envs={envs} onGrant={(environment, role) => act(() => api.putGrant(m.person, { role, environment }))} />
                      )}
                    </td>
                    <td className="col-quick">
                      {self && c.leave && (
                        <ArmButton armKey={'leave'} armed={armed} arm={arm} disarm={disarm} icon="logout" className="mini"
                          confirmLabel="leave the tenant?" title="give up every grant you hold in this tenant"
                          onConfirm={() => act(async () => {
                            await api.leaveTenant();
                            await reload();
                            navigate('/tenants');
                          })}>Leave</ArmButton>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table></div>
        )}
        {members && members.length === 0 && <p className="empty">No members.</p>}
        {!members && !err && !missing && <p className="empty">Loading…</p>}
        <p className="hint">
          Members see each other's name and e-mail address in this tenant only. A role on the tenant reaches every
          environment; a role on an environment that one alone. A tenant always keeps an admin.
        </p>
      </section>
      {c.adminTenant && <Invitations envs={envs} role={access.role} />}
    </div>
  );
}

// EnvGrantForm adds a grant on an environment.
function EnvGrantForm({ envs, onGrant }) {
  const [open, setOpen] = useState(false);
  const [env, setEnv] = useState('');
  const [role, setRole] = useState('read');
  if (!open) {
    return <button className="ghost mini" title="grant a role on an environment" onClick={() => setOpen(true)}>
      <Icon name="plus" size={11} /> environment</button>;
  }
  return (
    <form className="inlineform" onSubmit={(e) => { e.preventDefault(); if (env) { onGrant(env, role); setOpen(false); } }}>
      <select value={env} aria-label="environment" onChange={(e) => setEnv(e.target.value)}>
        <option value="">environment…</option>
        {envs.map((x) => <option key={x.id} value={x.id}>{x.name}</option>)}
      </select>
      <select value={role} aria-label="role" onChange={(e) => setRole(e.target.value)}>
        {ROLES.map((r) => <option key={r} value={r}>{r}</option>)}
      </select>
      <button type="submit" className="mini primary" disabled={!env}>Grant</button>
      <button type="button" className="ghost mini" onClick={() => setOpen(false)}><Icon name="x" size={11} /></button>
    </form>
  );
}

// invitationState lives in people.js; this module re-exports it.
export { invitationState };

// ScopeRoleFields are the role and scope of an invitation or a token,
// never above the maker's role.
function ScopeRoleFields({ role, setRole, env, setEnv, envs, max }) {
  return (
    <>
      <select value={role} aria-label="role" onChange={(e) => setRole(e.target.value)}>
        {grantableRoles(max).map((r) => <option key={r} value={r}>{r}</option>)}
      </select>
      <span className="muted small">on</span>
      <select value={env} aria-label="scope" onChange={(e) => setEnv(e.target.value)}>
        <option value="">the tenant</option>
        {envs.map((x) => <option key={x.id} value={x.id}>environment {x.name}</option>)}
      </select>
    </>
  );
}

// Invitations are a tenant's single-use links: made with a role and a
// scope, shown once, valid for 7 days. FeRun sends no mail: the admin
// passes the link on.
function Invitations({ envs, role: myRole }) {
  const [list, setList] = useState(null);
  const [err, setErr] = useState('');
  const [missing, setMissing] = useState(false);
  const [role, setRole] = useState('read');
  const [env, setEnv] = useState('');
  const [fresh, setFresh] = useState(null);
  const [armed, arm, disarm] = useArm();
  useNow(30000);

  const refresh = useCallback(async () => {
    try {
      setList((await api.listInvitations()).invitations || []);
      setErr('');
    } catch (e) {
      if (notAvailable(e)) setMissing(true);
      else setErr(e.message);
    }
  }, []);
  usePoll(refresh, 15000);

  const origin = globalThis.location ? globalThis.location.origin : '';
  return (
    <section className="pane">
      <div className="pane-toolbar">
        <h2><Icon name="ticket" size={15} /> Invitations {!missing && <span className="count">{list ? list.length : '…'}</span>}</h2>
      </div>
      {missing && <NotAvailable what="Invitations" />}
      {!missing && (
        <>
          <form className="createpanel" onSubmit={async (e) => {
            e.preventDefault();
            setErr('');
            try {
              const inv = await api.createInvitation({ role, environment: env });
              setFresh(inv);
              await refresh();
            } catch (e2) {
              setErr(e2.message);
            }
          }}>
            <span className="muted small">Invite as</span>
            <ScopeRoleFields role={role} setRole={setRole} env={env} setEnv={setEnv} envs={envs} max={myRole} />
            <button type="submit" className="primary"><Icon name="plus" size={14} /> Make a link</button>
          </form>
          {fresh && (
            <Secret value={inviteLink(origin, fresh)} title={`Invitation as ${fresh.role || role} on ${scopeLabel(fresh.environment, envs)}`}
              onDismiss={() => setFresh(null)}>
              <p className="muted small">
                Pass the link on. The first person who opens it while signed in gets the grant; it is valid for 7 days.
              </p>
            </Secret>
          )}
          <ErrBox error={err} />
          <div className="tblwrap"><table className="tbl">
            <thead>
              <tr><th>role</th><th>on</th><th>state</th><th className="col-when">made</th><th className="col-when">expires</th><th className="col-quick" /></tr>
            </thead>
            <tbody>
              {(list || []).map((inv) => {
                const st = invitationState(inv);
                return (
                  <tr key={inv.id} className={st === 'open' ? '' : 'dim'}>
                    <td><RoleBadge role={inv.role} /></td>
                    <td className="small">{inv.environmentName ? `environment ${inv.environmentName}` : scopeLabel(inv.environment, envs)}</td>
                    <td className="small">
                      {st === 'used' ? <span>used{inv.usedByName ? ` by ${inv.usedByName}` : ''}</span> : <span className={st === 'open' ? 'ok' : 'muted'}>{st}</span>}
                    </td>
                    <td className="col-when"><Ago iso={inv.createdAt} /></td>
                    <td className="col-when muted">{fmtDate(inv.expiresAt)}</td>
                    <td className="col-quick">
                      {st === 'open' && (
                        <ArmButton armKey={inv.id} armed={armed} arm={arm} disarm={disarm} icon="trash" className="mini"
                          confirmLabel="revoke?" onConfirm={async () => {
                            try {
                              await api.revokeInvitation(inv.id);
                              await refresh();
                            } catch (e) {
                              setErr(e.message);
                            }
                          }}>Revoke</ArmButton>)}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table></div>
          {list && list.length === 0 && <p className="empty">No invitations.</p>}
        </>
      )}
    </section>
  );
}

// ---------------------------------------------------------------- tokens --

// Tokens are a tenant's tokens: for clients (FeCode, an agent, a CI) to
// act in the tenant with a role and a scope, never above the minter's.
// A token is shown once; the browser keeps none.
export function Tokens({ tenant }) {
  const access = useAccess(tenant);
  const [tokens, setTokens] = useState(null);
  const [err, setErr] = useState('');
  const [missing, setMissing] = useState(false);
  const [label, setLabel] = useState('');
  const [role, setRole] = useState('read');
  const [env, setEnv] = useState('');
  const [fresh, setFresh] = useState(null);
  const [armed, arm, disarm] = useArm();
  const envs = useEnvironments(true);
  useNow(30000);

  const refresh = useCallback(async () => {
    try {
      setTokens((await api.listTokensOfTenant()).tokens || []);
      setErr('');
    } catch (e) {
      if (notAvailable(e)) setMissing(true);
      else setErr(e.message);
    }
  }, []);
  const lastOk = usePoll(refresh, 15000);
  const envName = (id) => (envs.find((x) => x.id === id) || {}).name || id;

  return (
    <div className="single">
      <section className="pane">
        <div className="pane-toolbar">
          <h2><Icon name="key" size={15} /> Tokens {!missing && <span className="count">{tokens ? tokens.length : '…'}</span>}</h2>
          <span className="spacer" />
          {!missing && <LiveBadge lastOk={lastOk} />}
        </div>
        {missing && <NotAvailable what="A tenant's tokens" />}
        {!missing && (
          <>
            <form className="createpanel" onSubmit={async (e) => {
              e.preventDefault();
              setErr('');
              try {
                const res = await api.mintTokenOfTenant({ label: label.trim(), role, environment: env });
                setFresh({ ...res, role: res.role || role, environment: res.environment || env });
                setLabel('');
                await refresh();
              } catch (e2) {
                setErr(e2.message);
              }
            }}>
              <input placeholder="label (e.g. ci, an agent)" value={label} aria-label="label"
                onChange={(e) => setLabel(e.target.value)} />
              <ScopeRoleFields role={role} setRole={setRole} env={env} setEnv={setEnv} envs={envs} max={access.role} />
              <button type="submit" className="primary"><Icon name="key" size={14} /> Mint a token</button>
            </form>
            {fresh && (
              <Secret value={fresh.token} title={`Token as ${fresh.role} on ${scopeLabel(fresh.environment, envs)}`}
                onDismiss={() => setFresh(null)}>
                <p className="muted small">A client sends it as <span className="mono">Authorization: Bearer …</span>; this browser keeps it not.</p>
              </Secret>
            )}
            <ErrBox error={err} />
            <div className="tblwrap"><table className="tbl">
              <thead>
                <tr><th>hash</th><th>label</th><th>role</th><th>on</th><th className="col-when">created</th><th className="col-when">last used</th><th className="col-quick" /></tr>
              </thead>
              <tbody>
                {[...(tokens || [])].sort((a, b) => (b.createdAt || '').localeCompare(a.createdAt || '')).map((t) => (
                  <tr key={t.hash}>
                    <td className="mono muted" title={t.hash}>{(t.hashPrefix || t.hash || '').slice(0, 8)}…</td>
                    <td>{t.label || <span className="muted">(no label)</span>}</td>
                    <td><RoleBadge role={t.role || 'admin'} /></td>
                    <td className="small">{t.environment ? `environment ${t.environmentName || envName(t.environment)}` : 'the tenant'}</td>
                    <td className="col-when muted">{fmtDate(t.createdAt)}</td>
                    <td className="col-when">{t.lastUsedAt ? <Ago iso={t.lastUsedAt} /> : <span className="muted">never</span>}</td>
                    <td className="col-quick">
                      <ArmButton armKey={t.hash} armed={armed} arm={arm} disarm={disarm} icon="trash" className="mini"
                        confirmLabel="revoke?" onConfirm={async () => {
                          try {
                            await api.revokeTokenOfTenant(t.hash);
                            await refresh();
                          } catch (e) {
                            setErr(e.message);
                          }
                        }}>Revoke</ArmButton>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table></div>
            {tokens && tokens.length === 0 && <p className="empty">No tokens: only members act in this tenant.</p>}
          </>
        )}
        <p className="hint">
          FeCode's and FeChat's tokens are admins of their tenant. Sandbox tokens are a sandbox's own, under Sandboxes.
        </p>
      </section>
    </div>
  );
}

// ----------------------------------------------------------------- quota --

// QuotaCard is the tenant's use against its limits; nothing when this
// ferund has no quotas.
export function QuotaCard() {
  const [q, setQ] = useState(null);
  const [gone, setGone] = useState(false);
  const refresh = useCallback(async () => {
    try {
      setQ(await api.getQuota());
    } catch (e) {
      if (notAvailable(e) || e.status === 403) setGone(true);
    }
  }, []);
  usePoll(refresh, 30000);
  if (gone || !q) return null;
  const rows = quotaRows(q);
  return (
    <section className="pane quota">
      <div className="pane-toolbar">
        <h2><Icon name="gauge" size={14} /> Quota</h2>
        <span className="muted small">what the tenant asks for, against its limits</span>
      </div>
      <div className="quotagrid">
        {rows.map((r) => (
          <div key={r.key} className={'quotarow' + (r.tone ? ' t-' + r.tone : '')}>
            <span className="quota-label">{r.label}</span>
            <span className="quota-num mono">
              {fmtQuota(r, r.use)}{r.limit !== null ? <span className="muted"> / {fmtQuota(r, r.limit)}</span> : <span className="muted"> · no limit</span>}
            </span>
            <span className="quotabar" aria-hidden="true">
              <span style={{ width: r.share === null ? '0%' : `${Math.min(100, Math.round(r.share * 100))}%` }} />
            </span>
          </div>
        ))}
      </div>
    </section>
  );
}

// ------------------------------------------------------------ invitation --

// InviteAccept is /invite/<token>: what the invitation grants, and its
// acceptance by the signed-in person. Signed out, the sign-in brings the
// browser back here.
export function InviteAccept({ token, navigate }) {
  const { me, reload } = useMe();
  const [inv, setInv] = useState(null);
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);
  const basic = me && me.method === 'basic';

  useEffect(() => {
    if (!token || basic) return undefined;
    let live = true;
    api.getInvitation(token).then((r) => { if (live) setInv(r); }, (e) => {
      // Without a preview route the acceptance still works.
      if (live && !notAvailable(e)) setErr(e.status === 404 ? 'This invitation is not valid: used, expired or revoked.' : e.message);
    });
    return () => { live = false; };
  }, [token, basic]);

  const accept = async () => {
    setBusy(true);
    setErr('');
    try {
      const res = await api.acceptInvitation(token);
      const fresh = await reload();
      const name = (res && res.tenant) || (inv && inv.tenant) || '';
      navigate(name ? tenantHome(tenantAccess(fresh || me, name)) : '/tenants');
    } catch (e) {
      if (notAvailable(e)) setErr('Invitations are not available on this ferund yet.');
      else if (e.status === 404) setErr('This invitation is not valid: used, expired or revoked.');
      else setErr(e.message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="pane gate">
      <h2><Icon name="ticket" size={15} /> An invitation</h2>
      {basic && <p className="muted">A password sign-in cannot take an invitation: sign out, and sign in with the provider.</p>}
      {!basic && (
        <>
          {inv ? (
            <p>
              You are invited to tenant <span className="mono strong">{inv.tenant}</span> as{' '}
              <RoleBadge role={inv.role} /> on {inv.environmentName ? `environment ${inv.environmentName}` : scopeLabel(inv.environment)}.
              {inv.expiresAt && <span className="muted small"> Valid until {fmtDate(inv.expiresAt)}.</span>}
            </p>
          ) : !err && <p className="muted">Someone passed you a link to a tenant of this FeRun.</p>}
          <p className="muted small">Signed in as {(me && (me.name || me.email)) || 'you'}. Accepting gives this account the grant.</p>
          <div className="actionrow">
            <button className="primary" disabled={busy || !token} onClick={accept}><Icon name="check" size={14} /> Accept</button>
            <button className="ghost" onClick={() => navigate('/tenants')}>Not now</button>
          </div>
        </>
      )}
      <ErrBox error={err} />
    </section>
  );
}
