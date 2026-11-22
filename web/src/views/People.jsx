// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useEffect, useRef, useState } from 'react';
import { api, notAvailable } from '../api.js';
import { Icon } from '../Icon.jsx';
import { Ago, ArmButton, ErrBox, LiveBadge, PageHead, Secret, useArm, useNow, usePoll } from '../components.jsx';
import { fmtDate } from '../format.js';
import {
  ROLES, accountAccess, accountLabel, accountsToMakeOn, envCan, grantableRoles, integrationRoleText, invitationState, inviteLink,
  madeByText, myAccounts, myProjects, personLabel, quotaRows, fmtQuota, roleLabel, scopeLabel, projectAccess, projectHome,
  useAccess, useMe,
} from '../people.js';
import { issuerHost } from './platformData.js';

// The people's pages (ADR 0031, "The UI is the projects' console"): a
// person's projects and a new one, a project's members and invitations, its
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

// ------------------------------------------------------------ my projects --

// MyProjects is a person's projects, from /auth/me, with their role and
// account, the form for a new one on an account they may put projects on
// (ADR 0059), and their accounts.
export function MyProjects({ navigate }) {
  const { me, reload } = useMe();
  const projects = myProjects(me);
  const accounts = myAccounts(me);
  const makeOn = accountsToMakeOn(me);
  // /projects?new opens the form (the switcher's New project, FeCode's
  // way here for a person without a project).
  const [showCreate, setShowCreate] = useState(projects.length === 0 || new URLSearchParams(location.search).has('new'));
  const [name, setName] = useState('');
  const [account, setAccount] = useState('');
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);
  const chosen = account || (makeOn[0] && makeOn[0].name) || '';
  const chosenAccount = makeOn.find((a) => a.name === chosen) || null;

  return (
    <div className="projects single">
      <PageHead kind="Projects" title="Your projects"
        facts={<span>A project holds sandboxes, environments, domains and the people who may use them; its account is charged for its use.</span>} />
      <section className="pane">
        <div className="pane-toolbar">
          <h2><Icon name="folder" size={15} /> Your projects <span className="count">{projects.length}</span></h2>
          <span className="spacer" />
          <button className={'ghost mini' + (showCreate ? ' on' : '')} onClick={() => setShowCreate(!showCreate)}>
            <Icon name="plus" size={13} /> New project
          </button>
        </div>

        {showCreate && (
          <form className="createpanel" onSubmit={async (e) => {
            e.preventDefault();
            const n = name.trim();
            setBusy(true);
            setErr('');
            try {
              await api.createOwnProject(n, chosen === 'default' ? '' : chosen);
              await reload();
              setName('');
              setShowCreate(false);
              navigate(`/projects/${encodeURIComponent(n)}`);
            } catch (e2) {
              setErr(notAvailable(e2) ? 'Making projects is not available on this ferund yet.' : e2.message);
            } finally {
              setBusy(false);
            }
          }}>
            <input placeholder="name: lowercase letters, digits and '-'" value={name} autoFocus
              aria-label="the new project's name" onChange={(e) => setName(e.target.value)} />
            {makeOn.length > 1 && (
              <>
                <span className="muted small">on account</span>
                <select value={chosen} aria-label="the account" onChange={(e) => setAccount(e.target.value)}>
                  {makeOn.map((a) => <option key={a.name} value={a.name}>{accountLabel(a)}</option>)}
                </select>
              </>)}
            {makeOn.length === 1 && <span className="muted small">on account <b>{accountLabel(makeOn[0])}</b></span>}
            <button type="submit" className="primary" disabled={busy || !name.trim() || !chosenAccount}>
              <Icon name="plus" size={14} /> Create</button>
          </form>
        )}
        {showCreate && makeOn.length > 0 && (
          <p className="hint">
            The name is part of the project's hosts. You become its admin, and can invite others. Its use is charged
            to the account; an admin of the project moves it to another account later.
          </p>
        )}
        {showCreate && makeOn.length === 0 && (
          <p className="notice small">
            You are on no account you may put projects on: ask an operator or an account&apos;s admin for an invitation.
          </p>
        )}
        <ErrBox error={err} />

        <div className="tblwrap"><table className="tbl">
          <thead>
            <tr><th>project</th><th>your role</th><th>environments</th><th>account</th><th className="col-quick" /></tr>
          </thead>
          <tbody>
            {projects.map((t) => (
              <tr key={t.project} className="clickable" onClick={() => navigate(projectHome(projectAccess(me, t.project)))}>
                <td className="strong">{t.project}</td>
                <td><RoleBadge role={roleLabel(t)} /></td>
                <td className="muted small">
                  {(t.environments || []).length === 0 ? '—'
                    : t.environments.map((e) => `${e.name} (${e.role || 'none'}${e.protected ? ', protected' : ''})`).join(', ')}
                </td>
                <td className="small">{t.account ? accountLabel(t.account) : <span className="muted">—</span>}</td>
                <td className="col-quick"><Icon name="chevronRight" size={13} /></td>
              </tr>
            ))}
          </tbody>
        </table></div>
        {projects.length === 0 && (
          <p className="empty">
            You are a member of no project yet. Make one, or ask a project's admin for an invitation link.
          </p>
        )}
      </section>
      {accounts.length > 0 && <AccountList accounts={accounts} navigate={navigate} />}
    </div>
  );
}

// AccountList is a person's accounts (ADR 0059): those they may put
// projects on or read; an account's page opens for its explicit members.
export function AccountList({ accounts, navigate }) {
  return (
    <section className="pane">
      <div className="pane-toolbar">
        <h2><Icon name="building" size={15} /> Your accounts <span className="count">{accounts.length}</span></h2>
      </div>
      <div className="tblwrap"><table className="tbl">
        <thead><tr><th>account</th><th>your role</th><th>state</th><th className="col-quick" /></tr></thead>
        <tbody>
          {accounts.map((a) => (
            <tr key={a.name} className={a.explicit ? 'clickable' : ''}
              onClick={a.explicit ? () => navigate(`/accounts/${encodeURIComponent(a.name)}`) : undefined}>
              <td><span className="strong">{a.title || a.name}</span> <span className="mono muted small">{a.name}</span></td>
              <td>{a.explicit ? <RoleBadge role={a.role} />
                : <span className="muted small" title="open to every person here: you put projects on it; its page is the operators'">open to you</span>}</td>
              <td className="small">{a.closed ? <span className="warn">closed: takes no new project</span> : <span className="ok">open</span>}</td>
              <td className="col-quick">{a.explicit && <Icon name="chevronRight" size={13} />}</td>
            </tr>
          ))}
        </tbody>
      </table></div>
      <p className="hint">
        A project&apos;s account is who is charged for its use. An account&apos;s people see its projects, their makers and
        admins and their use; nothing inside them.
      </p>
    </section>
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

// Members is the one view of who has access to a project (ADR 0056):
// its members with their grants (and who gave them, the issuer and
// subject they sign in with), the integrations it grants, for its admin
// its tokens (read-only; minted on Tokens) and the invitations. An admin
// changes the grants; every member may leave. ?integration=<name> opens
// the grant of that integration (FeCode links here).
export function Members({ project, navigate }) {
  const { me, reload } = useMe();
  const access = useAccess(project);
  const c = access.can;
  const [members, setMembers] = useState(null);
  const [integrations, setIntegrations] = useState(null);
  const [err, setErr] = useState('');
  const [missing, setMissing] = useState(false);
  const [armed, arm, disarm] = useArm();
  const envs = useEnvironments(true);
  // An admin of the project grants on it and on every environment; an
  // admin of an environment on that one (ADR 0031, admin by scope).
  const adminEnvs = envs.filter((e) => c.adminProject || envCan(access, e.id).admin);
  useNow(10000);

  const refresh = useCallback(async () => {
    try {
      const r = await api.listMembers();
      setMembers(r.members || []);
      setIntegrations(r.integrations || []);
      setErr('');
    } catch (e) {
      if (notAvailable(e)) setMissing(true);
      else setErr(e.message);
    }
  }, []);
  const lastOk = usePoll(refresh, 10000);

  // Every grant change reads /auth/me again: the roles it answers are
  // what the pages offer.
  const act = async (fn) => {
    setErr('');
    try {
      await fn();
      await refresh();
      reload().catch(() => {});
    } catch (e) {
      setErr(notAvailable(e) ? 'This change is not available on this ferund yet.' : e.message);
    }
  };

  return (
    <div className="single">
      <PageHead kind="Project" title="People"
        facts={<span>Members and integrations with a role on the project or on single environments; invitations bring new people in.</span>} />
      <section className="pane">
        <div className="pane-toolbar">
          <h2><Icon name="users" size={15} /> Members {!missing && <span className="count">{members ? members.length : '…'}</span>}</h2>
          <span className="muted small">your role: <RoleBadge role={roleLabel(access)} /></span>
          <span className="spacer" />
          {!missing && <LiveBadge lastOk={lastOk} />}
        </div>
        <ErrBox error={err} />
        {missing && <NotAvailable what="A project's members" />}
        {!missing && (
          <div className="tblwrap"><table className="tbl members">
            <thead>
              <tr><th>person</th><th>on the project</th><th>on environments</th><th className="col-quick" /></tr>
            </thead>
            <tbody>
              {(members || []).map((m) => {
                const self = !!me && !!me.person && m.person === me.person;
                return (
                  <tr key={m.person} className={m.disabled ? 'dim' : ''}>
                    <td>
                      <span className="strong">{personLabel(m)}</span>
                      {self && <span className="badge">you</span>}
                      {m.disabled && <span className="badge warn">disabled</span>}
                      {m.email && m.name && <div className="muted small">{m.email}</div>}
                      {(m.issuer || m.subject) && (
                        <div className="who-sub mono" title={m.issuer}>{issuerHost(m.issuer)}{m.subject ? ` · ${m.subject}` : ''}</div>)}
                    </td>
                    <td title={m.role && m.madeBy ? `given by ${madeByText(m.madeBy, m.madeByName)}` : undefined}>
                      {c.adminProject && !self ? (
                        <select value={m.role || ''} aria-label={`${personLabel(m)}'s role on the project`}
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
                        <span key={e.id} className="grantchip" title={e.madeBy ? `given by ${madeByText(e.madeBy, e.madeByName)}` : undefined}>
                          {e.name} · {e.role}
                          {c.adminProject && !self && (
                            <button className="iconbtn mini" title={`remove the grant on ${e.name}`}
                              onClick={() => act(() => api.deleteGrant(m.person, e.id))}><Icon name="x" size={11} /></button>)}
                        </span>
                      ))}
                      {(m.environments || []).length === 0 && <span className="muted">—</span>}
                      {c.adminProject && !self && envs.length > 0 && (
                        <EnvGrantForm envs={envs} onGrant={(environment, role) => act(() => api.putGrant(m.person, { role, environment }))} />
                      )}
                    </td>
                    <td className="col-quick">
                      {self && c.leave && (
                        <ArmButton armKey={'leave'} armed={armed} arm={arm} disarm={disarm} icon="logout" className="mini"
                          confirmLabel="leave the project?" title="give up every grant you hold in this project"
                          onConfirm={() => act(async () => {
                            await api.leaveProject();
                            await reload();
                            navigate('/projects');
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
          Members see each other&apos;s name and e-mail address in this project only. A role on the project reaches every
          environment but a protected one, which a project role of read or write reads; a role on an environment that one
          alone. A project always keeps an admin.
        </p>
      </section>
      {!missing && (
        <IntegrationsPane list={integrations} access={access} envs={envs} adminEnvs={adminEnvs} act={act}
          preselect={new URLSearchParams(location.search).get('integration') || ''} />)}
      {c.adminProject && !missing && <TokensPane project={project} envs={envs} navigate={navigate} />}
      {c.adminProject && (
        <Invitations envs={envs} role={access.role}
          list={api.listInvitations} create={api.createInvitation} revoke={api.revokeInvitation} />)}
    </div>
  );
}

// IntegrationsPane is the integrations a project grants (ADR 0056): each
// with its role on the project and its grants on environments, who gave
// them; an admin of the scope changes them and adds one. Every member
// reads it.
export function IntegrationsPane({ list, access, envs, adminEnvs, act, preselect }) {
  const c = access.can;
  // Who may grant is /auth/me's, known at once; which environments the
  // form offers comes with their list.
  const mayGrant = c.adminProject || (access.environments || []).some((e) => e.role === 'admin');
  const [adding, setAdding] = useState(!!preselect && mayGrant);
  // ?integration=<name> opens the form once the sign-in may grant, should
  // /auth/me be read after the first render.
  const opened = useRef(!!preselect && mayGrant);
  useEffect(() => {
    if (!preselect || !mayGrant || opened.current) return;
    opened.current = true;
    setAdding(true);
  }, [preselect, mayGrant]);
  const envAdmin = (id) => c.adminProject || adminEnvs.some((e) => e.id === id);
  return (
    <section className="pane">
      <div className="pane-toolbar">
        <h2><Icon name="plug" size={15} /> Integrations <span className="count">{list ? list.length : '…'}</span></h2>
        <span className="spacer" />
        {mayGrant && (
          <button className={'ghost mini' + (adding ? ' on' : '')} onClick={() => setAdding(!adding)}>
            <Icon name="plus" size={13} /> Add an integration</button>)}
      </div>
      {adding && (
        <IntegrationGrantForm granted={list || []} envs={adminEnvs} projectRole={access.role} adminProject={c.adminProject}
          preselect={preselect} onCancel={() => setAdding(false)}
          onGrant={(name, role, environment) => act(async () => {
            await api.putIntegrationGrant(name, { role, environment });
            setAdding(false);
          })} />)}
      {list && list.length > 0 && (
        <div className="tblwrap"><table className="tbl members">
          <thead><tr><th>integration</th><th>on the project</th><th>on environments</th><th>given by</th></tr></thead>
          <tbody>
            {list.map((g) => (
              <tr key={g.integration} className={g.disabled ? 'dim' : ''}>
                <td>
                  <span className="strong">{g.title || g.integration}</span> <span className="mono muted small">{g.integration}</span>
                  {g.disabled && <span className="badge warn" title="its grants act again once an operator enables it">disabled by an operator</span>}
                  {g.url && <div className="small"><a href={g.url} target="_blank" rel="noreferrer">{g.url}</a></div>}
                </td>
                <td>
                  {c.adminProject ? (
                    <select value={g.role || ''} aria-label={`${g.title || g.integration}'s role on the project`}
                      onChange={(e) => act(() => (e.target.value
                        ? api.putIntegrationGrant(g.integration, { role: e.target.value })
                        : api.deleteIntegrationGrant(g.integration)))}>
                      <option value="">none</option>
                      {ROLES.map((r) => <option key={r} value={r}>{r}</option>)}
                    </select>
                  ) : <RoleBadge role={g.role} />}
                </td>
                <td className="small">
                  {(g.environments || []).map((e) => (
                    <span key={e.id} className="grantchip" title={e.madeBy ? `given by ${madeByText(e.madeBy, e.madeByName)}` : undefined}>
                      {e.name} · {e.role}
                      {envAdmin(e.id) && (
                        <button className="iconbtn mini" title={`remove the grant on ${e.name}`}
                          onClick={() => act(() => api.deleteIntegrationGrant(g.integration, e.id))}><Icon name="x" size={11} /></button>)}
                    </span>
                  ))}
                  {(g.environments || []).length === 0 && <span className="muted">—</span>}
                  {adminEnvs.length > 0 && (
                    <EnvGrantForm envs={adminEnvs}
                      onGrant={(environment, role) => act(() => api.putIntegrationGrant(g.integration, { role, environment }))} />)}
                </td>
                <td className="small muted">{madeByText(g.madeBy, g.madeByName) || '—'}</td>
              </tr>
            ))}
          </tbody>
        </table></div>)}
      {list && list.length === 0 && <p className="empty">No integration acts in this project.</p>}
      <p className="hint">
        An integration is a service such as FeCode that acts in the project by the grants its admins give it, alone or for
        a person it names; then it does what both of you may. It never changes members, invitations, grants, tokens or the
        project&apos;s account.
      </p>
    </section>
  );
}

// grantScope is the scope a grant form offers: the one chosen, '' the
// project; one who is no admin of the project grants on an environment
// they are admin of, never on the project, and the environments come
// after the first render (FeCode's link opens the form at once), so a
// choice made before them falls to the first. '' while there is none.
export function grantScope(adminProject, chosen, envs) {
  if (adminProject || envs.some((e) => e.id === chosen)) return chosen;
  return (envs[0] && envs[0].id) || '';
}

// IntegrationGrantForm grants a registered integration a role on the
// project or on an environment the granter is admin of, and says what
// the role lets it do (integrationRoleText).
export function IntegrationGrantForm({ granted, envs, projectRole, adminProject, preselect = '', onGrant, onCancel }) {
  const [registered, setRegistered] = useState(null);
  const [err, setErr] = useState('');
  const [name, setName] = useState(preselect);
  const [chosen, setEnv] = useState(adminProject ? '' : ((envs[0] && envs[0].id) || ''));
  const env = grantScope(adminProject, chosen, envs);
  const [role, setRole] = useState('write');
  useEffect(() => {
    let live = true;
    api.listIntegrations().then((r) => { if (live) setRegistered(r.integrations || []); },
      (e) => { if (live) setErr(notAvailable(e) ? 'Integrations are not available on this ferund yet.' : e.message); });
    return () => { live = false; };
  }, []);
  const has = (n) => granted.some((g) => g.integration === n && (env
    ? (g.environments || []).some((e) => e.id === env) : !!g.role));
  const offered = (registered || []).filter((i) => !has(i.name));
  const pick = offered.find((i) => i.name === name) || null;
  const max = env ? 'admin' : projectRole;
  const roles = grantableRoles(max);
  const shown = roles.includes(role) ? role : roles[roles.length - 1] || '';
  const ready = !!pick && !!shown && (adminProject || !!env);
  const scopeName = env ? ((envs.find((e) => e.id === env) || {}).name || env) : '';
  return (
    <form className="createpanel column" onSubmit={(e) => { e.preventDefault(); if (ready) onGrant(pick.name, shown, env); }}>
      <div className="inlineform">
        <select value={pick ? pick.name : ''} aria-label="the integration" onChange={(e) => setName(e.target.value)}>
          <option value="">integration…</option>
          {offered.map((i) => <option key={i.name} value={i.name}>{i.title || i.name} · {i.name}</option>)}
        </select>
        <select value={shown} aria-label="role" onChange={(e) => setRole(e.target.value)}>
          {roles.map((r) => <option key={r} value={r}>{r}</option>)}
        </select>
        <span className="muted small">on</span>
        <select value={env} aria-label="scope" onChange={(e) => setEnv(e.target.value)}>
          {adminProject && <option value="">the project</option>}
          {envs.map((x) => <option key={x.id} value={x.id}>environment {x.name}</option>)}
        </select>
        <button type="submit" className="primary mini" disabled={!ready}>Grant</button>
        <button type="button" className="ghost mini" onClick={onCancel}><Icon name="x" size={11} /></button>
      </div>
      {pick && pick.url && <div className="small"><a href={pick.url} target="_blank" rel="noreferrer">{pick.url}</a></div>}
      {preselect && registered && !pick && !has(preselect) && (
        <p className="notice small">No integration named <span className="mono">{preselect}</span> is registered here.</p>)}
      {preselect && has(preselect) && <p className="small ok">{preselect} holds a grant on {env ? `environment ${scopeName}` : 'the project'} already.</p>}
      <ul className="role-lines">
        {integrationRoleText(shown, env).map((l) => <li key={l}>{l}</li>)}
      </ul>
      <ErrBox error={err} />
    </form>
  );
}

// TokensPane is the project's tokens, read-only, for its admin: who else
// acts in it (ADR 0056: integrations beside people and tokens). Minting
// and revoking are on Tokens.
function TokensPane({ project, envs, navigate }) {
  const [tokens, setTokens] = useState(null);
  const [gone, setGone] = useState(false);
  useEffect(() => {
    let live = true;
    api.listTokensOfProject().then((r) => { if (live) setTokens(r.tokens || []); }, () => { if (live) setGone(true); });
    return () => { live = false; };
  }, []);
  if (gone) return null;
  const to = `/projects/${encodeURIComponent(project)}/tokens`;
  return (
    <section className="pane">
      <div className="pane-toolbar">
        <h2><Icon name="key" size={15} /> Tokens <span className="count">{tokens ? tokens.length : '…'}</span></h2>
        <span className="spacer" />
        <a className="small" href={to} onClick={(e) => { e.preventDefault(); navigate(to); }}>Mint and revoke on Tokens <Icon name="arrowRight" size={12} /></a>
      </div>
      {tokens && tokens.length > 0 && <TokenRows tokens={tokens} envs={envs} />}
      {tokens && tokens.length === 0 && <p className="empty">No tokens.</p>}
    </section>
  );
}

// TokenScope is a token's "on" cell: the scope of its own grant, and its
// grants beside it (the protect dialog gives them, ADR 0060), each
// removable with onRemove.
export function TokenScope({ token: t, envName, onRemove = null }) {
  return (
    <>
      {t.environment ? `environment ${t.environmentName || envName(t.environment)}` : 'the project'}
      {(t.grants || []).map((g) => (
        <span key={g.environment || 'project'} className="grantchip">
          {g.environment ? envName(g.environment) : 'the project'} · {g.role}
          {onRemove && (
            <button className="iconbtn mini" title={`remove the grant on ${g.environment ? envName(g.environment) : 'the project'}`}
              onClick={() => onRemove(t, g)}><Icon name="x" size={11} /></button>)}
        </span>))}
    </>
  );
}

// TokenRows is a project's tokens as a table: hash, label, role, scope,
// made, last used; with actions per row when given.
export function TokenRows({ tokens, envs = [], actions = null }) {
  const envName = (id) => (envs.find((x) => x.id === id) || {}).name || id;
  return (
    <div className="tblwrap"><table className="tbl">
      <thead>
        <tr><th>hash</th><th>label</th><th>role</th><th>on</th><th className="col-when">created</th><th className="col-when">last used</th>
          {actions && <th className="col-quick" />}</tr>
      </thead>
      <tbody>
        {[...(tokens || [])].sort((a, b) => (b.createdAt || '').localeCompare(a.createdAt || '')).map((t) => (
          <tr key={t.hash}>
            <td className="mono muted" title={t.hash}>{(t.hashPrefix || t.hash || '').slice(0, 8)}…</td>
            <td>{t.label || <span className="muted">(no label)</span>}</td>
            <td><RoleBadge role={t.role || 'admin'} /></td>
            <td className="small"><TokenScope token={t} envName={envName} /></td>
            <td className="col-when muted">{fmtDate(t.createdAt)}</td>
            <td className="col-when">{t.lastUsedAt ? <Ago iso={t.lastUsedAt} /> : <span className="muted">never</span>}</td>
            {actions && <td className="col-quick">{actions(t)}</td>}
          </tr>
        ))}
      </tbody>
    </table></div>
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
        <option value="">the project</option>
        {envs.map((x) => <option key={x.id} value={x.id}>environment {x.name}</option>)}
      </select>
    </>
  );
}

// Invitations are single-use links of a project or an account (ADR
// 0059): made with a role (and for a project a scope, envs), shown once,
// valid for 7 days. FeRun sends no mail: the admin passes the link on.
// list, create and revoke are the calls of the scope; roles the roles it
// offers (a project's never above role, the maker's).
export function Invitations({ envs = null, role: myRole = 'admin', roles = null, list: listCall, create, revoke, what = 'project' }) {
  const offered = roles || grantableRoles(myRole);
  const [list, setList] = useState(null);
  const [err, setErr] = useState('');
  const [missing, setMissing] = useState(false);
  const [role, setRole] = useState(offered[0] || 'read');
  const [env, setEnv] = useState('');
  const [fresh, setFresh] = useState(null);
  const [armed, arm, disarm] = useArm();
  useNow(30000);

  const refresh = useCallback(async () => {
    try {
      setList((await listCall()).invitations || []);
      setErr('');
    } catch (e) {
      if (notAvailable(e)) setMissing(true);
      else setErr(e.message);
    }
  }, [listCall]);
  usePoll(refresh, 15000);

  const scopeOf = (inv) => (envs ? (inv.environmentName ? `environment ${inv.environmentName}` : scopeLabel(inv.environment, envs)) : `the ${what}`);
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
              const inv = await create(envs ? { role, environment: env } : { role });
              setFresh(inv);
              await refresh();
            } catch (e2) {
              setErr(e2.message);
            }
          }}>
            <span className="muted small">Invite as</span>
            {envs
              ? <ScopeRoleFields role={role} setRole={setRole} env={env} setEnv={setEnv} envs={envs} max={myRole} />
              : (
                <select value={role} aria-label="role" onChange={(e) => setRole(e.target.value)}>
                  {offered.map((r) => <option key={r} value={r}>{r}</option>)}
                </select>)}
            <button type="submit" className="primary"><Icon name="plus" size={14} /> Make a link</button>
          </form>
          {fresh && (
            <Secret value={inviteLink(origin, fresh)}
              title={`Invitation as ${fresh.role || role} on ${envs ? scopeLabel(fresh.environment, envs) : `the ${what}`}`}
              onDismiss={() => setFresh(null)}>
              <p className="muted small">
                Pass the link on. The first person who opens it while signed in gets the {envs ? 'grant' : 'role'}; it is valid for 7 days.
              </p>
            </Secret>
          )}
          <ErrBox error={err} />
          <div className="tblwrap"><table className="tbl">
            <thead>
              <tr><th>role</th>{envs && <th>on</th>}<th>state</th><th className="col-when">made</th><th className="col-when">expires</th><th className="col-quick" /></tr>
            </thead>
            <tbody>
              {(list || []).map((inv) => {
                const st = invitationState(inv);
                return (
                  <tr key={inv.id} className={st === 'open' ? '' : 'dim'}>
                    <td><RoleBadge role={inv.role} /></td>
                    {envs && <td className="small">{scopeOf(inv)}</td>}
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
                              await revoke(inv.id);
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

// Tokens are a project's tokens: for clients (FeCode, an agent, a CI) to
// act in the project with a role and a scope, never above the minter's.
// A token is shown once; the browser keeps none.
export function Tokens({ project }) {
  const access = useAccess(project);
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
      setTokens((await api.listTokensOfProject()).tokens || []);
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
      <PageHead kind="Project" title="Tokens"
        facts={<span>For CI and automation: a role on the project or on one environment, never above yours. A token shows once.</span>} />
      <section className="pane">
        <div className="pane-toolbar">
          <h2><Icon name="key" size={15} /> Tokens {!missing && <span className="count">{tokens ? tokens.length : '…'}</span>}</h2>
          <span className="spacer" />
          {!missing && <LiveBadge lastOk={lastOk} />}
        </div>
        {missing && <NotAvailable what="A project's tokens" />}
        {!missing && (
          <>
            <form className="createpanel" onSubmit={async (e) => {
              e.preventDefault();
              setErr('');
              try {
                const res = await api.mintTokenOfProject({ label: label.trim(), role, environment: env });
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
                    <td className="small">
                      <TokenScope token={t} envName={envName} onRemove={async (tk, g) => {
                        try {
                          await api.deleteTokenGrant(tk.hash, g.environment);
                          await refresh();
                        } catch (e) {
                          setErr(e.message);
                        }
                      }} />
                    </td>
                    <td className="col-when muted">{fmtDate(t.createdAt)}</td>
                    <td className="col-when">{t.lastUsedAt ? <Ago iso={t.lastUsedAt} /> : <span className="muted">never</span>}</td>
                    <td className="col-quick">
                      <ArmButton armKey={t.hash} armed={armed} arm={arm} disarm={disarm} icon="trash" className="mini"
                        confirmLabel="revoke?" onConfirm={async () => {
                          try {
                            await api.revokeTokenOfProject(t.hash);
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
            {tokens && tokens.length === 0 && <p className="empty">No tokens: only members act in this project.</p>}
          </>
        )}
        <p className="hint">
          A program such as FeChat's backend acts with a token; an integration such as FeCode acts by its grants on
          People, with no token. A token with write on the project reads a protected environment unless it holds a grant
          on it. Sandbox tokens are a sandbox's own, under Sandboxes.
        </p>
      </section>
    </div>
  );
}

// ----------------------------------------------------------------- quota --

// QuotaCard is the project's use against its limits; nothing when this
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
        <span className="muted small">what the project asks for, against its limits</span>
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

// acceptedPath is where an accepted invitation leads: an account's
// (ADR 0059) to its page when me, read after the acceptance, may open it,
// else to the accounts (default's page is the operators', though an
// operator invites people to it); a project's to the project as me now
// sees it, else the projects. res is the acceptance's answer, inv the
// preview.
export function acceptedPath(res, inv, me) {
  const account = (res && res.account) || (inv && inv.account) || '';
  const accountName = typeof account === 'string' ? account : account.name || '';
  if (accountName) return accountAccess(me, accountName).member ? `/accounts/${encodeURIComponent(accountName)}` : '/accounts';
  const name = (res && res.project) || (inv && inv.project) || '';
  return name ? projectHome(projectAccess(me, name)) : '/projects';
}

// InviteAccept is /invite/<token>: what the invitation grants, and its
// acceptance by the signed-in person. Signed out, the sign-in brings the
// browser back here.
// initial is the invitation's preview when it is read already.
export function InviteAccept({ token, navigate, initial = null }) {
  const { me, reload } = useMe();
  const [inv, setInv] = useState(initial);
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
      navigate(acceptedPath(res, inv, fresh || me));
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
          {inv && inv.account ? (
            <p>
              You are invited to account <span className="strong">{inv.account.title || inv.account.name}</span>{' '}
              (<span className="mono">{inv.account.name}</span>) as <RoleBadge role={inv.role} />.
              {inv.expiresAt && <span className="muted small"> Valid until {fmtDate(inv.expiresAt)}.</span>}
            </p>
          ) : inv ? (
            <p>
              You are invited to project <span className="mono strong">{inv.project}</span> as{' '}
              <RoleBadge role={inv.role} /> on {inv.environmentName ? `environment ${inv.environmentName}` : scopeLabel(inv.environment)}.
              {inv.expiresAt && <span className="muted small"> Valid until {fmtDate(inv.expiresAt)}.</span>}
            </p>
          ) : !err && <p className="muted">Someone passed you a link to a project or an account of this FeRun.</p>}
          <p className="muted small">Signed in as {(me && (me.name || me.email)) || 'you'}. Accepting gives you the role.</p>
          <div className="actionrow">
            <button className="primary" disabled={busy || !token} onClick={accept}><Icon name="check" size={14} /> Accept</button>
            <button className="ghost" onClick={() => navigate('/projects')}>Not now</button>
          </div>
        </>
      )}
      <ErrBox error={err} />
    </section>
  );
}
