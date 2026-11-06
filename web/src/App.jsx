// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useEffect, useMemo, useState } from 'react';
import {
  auth, forgetTenantTokens, onSignedOut, setActiveTenant, setBasic,
} from './api.js';
import { Icon } from './Icon.jsx';
import { Avatar, ThemeToggle, usePoll } from './components.jsx';
import { canonicalPath, envPath, parsePath } from './routes.js';
import { shortId } from './format.js';
import { SignIn } from './views/SignIn.jsx';
import { TenantGate } from './views/TenantGate.jsx';
import { InviteAccept, Members, MyTenants, RoleBadge, Tokens } from './views/People.jsx';
import { Overview } from './views/Overview.jsx';
import { Tenants } from './views/Tenants.jsx';
import { Sandboxes } from './views/Sandboxes.jsx';
import { Tools } from './views/Tools.jsx';
import { Environments } from './views/Environments.jsx';
import { NewEnvironment } from './views/NewEnvironment.jsx';
import { Nodes } from './views/Nodes.jsx';
import { NodeDetail } from './views/NodeDetail.jsx';
import { JoinTokens } from './views/JoinTokens.jsx';
import { Instances } from './views/Instances.jsx';
import {
  MeContext, isOperator, myTenants, roleLabel, tenantAccess, tenantHome, tenantNav, viewGate,
} from './people.js';

// The management UI of FeRun, the tenants' console (ADR 0031). Before
// anything else it asks ferund who is signed in (/auth/me: the person,
// whether an operator, their tenants with their roles) and how one signs
// in (/auth/config); signed out, or once a call is answered 401, it shows
// the sign-in screen. /auth/me decides what the UI offers (people.js):
// operators get the operators' pages, everyone their tenants.

// ME_POLL_MS is how often the UI asks /auth/me again, so that a grant
// given or taken elsewhere shows.
const ME_POLL_MS = 60000;

// useRoute follows the location, its path and query; a path under an
// earlier name is replaced by its canonical one (canonicalPath). A new
// path scrolls to the top, a new query alone (a search) does not.
function useRoute() {
  const [path, setPath] = useState(() => canonical());
  useEffect(() => {
    const onPop = () => setPath(canonical());
    window.addEventListener('popstate', onPop);
    return () => window.removeEventListener('popstate', onPop);
  }, []);
  const navigate = useCallback((to) => {
    if (to !== location.pathname + location.search) {
      const samePath = to.split('?')[0] === location.pathname;
      history.pushState(null, '', to);
      setPath(to);
      if (!samePath) window.scrollTo(0, 0);
    }
  }, []);
  return [parsePath(path), navigate];
}

// canonical is the location's path and query, the path replaced in the
// history by its canonical one when it differs.
function canonical() {
  const canon = canonicalPath(location.pathname);
  if (canon !== location.pathname) history.replaceState(null, '', canon + location.search + location.hash);
  return canon + location.search;
}

// displayName is how the header names the signed-in person.
export function displayName(me) {
  if (!me) return '';
  return me.name || me.username || me.email || 'admin';
}

export default function App() {
  const [session, setSession] = useState({ phase: 'loading' });
  const [route, navigate] = useRoute();

  const check = useCallback(async () => {
    try {
      const [config, me] = await Promise.all([auth.config(), auth.me()]);
      if (!me) setBasic('');
      forgetTenantTokens();
      setSession({ phase: me ? 'in' : 'out', config, me });
    } catch (e) {
      setSession({ phase: 'error', error: e.message });
    }
  }, []);
  useEffect(() => { check(); }, [check]);
  useEffect(() => onSignedOut(() => setSession((s) => ({
    ...s, phase: 'out', me: null, notice: 'Your sign-in has ended. Sign in again.',
  }))), []);
  // reload asks /auth/me again and returns it (null: signed out).
  const reload = useCallback(async () => {
    const me = await auth.me();
    setSession((s) => (me ? { ...s, phase: 'in', me } : { ...s, phase: 'out', me: null }));
    return me;
  }, []);
  const signedIn = session.phase === 'in';
  useEffect(() => {
    if (!signedIn) return undefined;
    const id = setInterval(() => { reload().catch(() => {}); }, ME_POLL_MS);
    return () => clearInterval(id);
  }, [signedIn, reload]);
  const meCtx = useMemo(() => ({ me: session.me || null, reload }), [session.me, reload]);

  // The tenant the route acts as, set during render: the views' effects
  // run before this component's, and call the API at once.
  setActiveTenant(route.tenant || '');

  if (session.phase === 'loading') return <div className="login"><p className="muted">Loading…</p></div>;
  if (session.phase === 'error') {
    return (
      <div className="login">
        <div className="login-card">
          <div className="login-brand"><Icon name="box" size={22} /> FeRun</div>
          <p className="error">ferund cannot be reached: {session.error}</p>
          <button className="primary" onClick={check}><Icon name="refresh" size={14} /> Try again</button>
        </div>
      </div>
    );
  }
  if (session.phase === 'out') {
    return (
      <SignIn config={session.config} notice={session.notice}
        next={location.pathname + location.search}
        onSignedIn={(me) => setSession((s) => ({ ...s, phase: 'in', me, notice: '' }))} />
    );
  }

  const me = session.me;
  const operator = isOperator(me);
  const gate = viewGate(me, route);
  const needsGate = !!gate;
  const signOut = async () => {
    let to = '/';
    try {
      to = await auth.signOut();
    } catch {
      // The session ends with the cookie's life anyway; go home.
    }
    location.assign(to);
  };

  return (
    <MeContext.Provider value={meCtx}>
    <div className="app">
      <Sidebar route={route} navigate={navigate} me={me} />
      <div className="main">
        <Topbar route={route} navigate={navigate} me={me} onSignOut={signOut} />
        <main className="content">
          {route.view === 'tenants' && (operator ? <Tenants navigate={navigate} /> : <MyTenants navigate={navigate} />)}
          {route.view === 'invite' && <InviteAccept key={route.token} token={route.token} navigate={navigate} />}
          {needsGate && <TenantGate key={route.tenant} reason={gate} tenant={route.tenant} navigate={navigate} />}
          {!needsGate && route.view === 'overview' && <Overview key={route.tenant} tenant={route.tenant} navigate={navigate} />}
          {!needsGate && route.view === 'sandboxes' && (
            <Sandboxes key={route.tenant} tenant={route.tenant} sel={route.sel} tab={route.tab} navigate={navigate} />)}
          {!needsGate && route.view === 'tools' && <Tools key={route.tenant} />}
          {!needsGate && route.view === 'members' && <Members key={route.tenant} tenant={route.tenant} navigate={navigate} />}
          {!needsGate && route.view === 'tokens' && <Tokens key={route.tenant} tenant={route.tenant} />}
          {!needsGate && route.view === 'environments' && (
            <Environments key={route.tenant} tenant={route.tenant} env={route.env} tab={route.tab} item={route.item}
              replica={route.replica} search={route.search} navigate={navigate} />)}
          {!needsGate && route.view === 'newEnvironment' && <NewEnvironment key={route.tenant} tenant={route.tenant} navigate={navigate} />}
          {!needsGate && route.view === 'nodes' && <Nodes navigate={navigate} />}
          {!needsGate && route.view === 'node' && <NodeDetail key={route.node} node={route.node} tab={route.tab} navigate={navigate} />}
          {!needsGate && route.view === 'joinTokens' && <JoinTokens />}
          {!needsGate && route.view === 'instances' && <Instances />}
        </main>
      </div>
    </div>
    </MeContext.Provider>
  );
}

// ------------------------------------------------------------------ shell --

function Sidebar({ route, navigate, me }) {
  const tenants = myTenants(me);
  const operator = isOperator(me);
  const t = route.tenant;
  const access = tenantAccess(me, t);
  const nav = t ? tenantNav(access) : [];
  const enc = encodeURIComponent;
  const link = (to) => (e) => { e.preventDefault(); navigate(to); };

  return (
    <aside className="sidebar">
      <a className="brand" href="/tenants" onClick={link('/tenants')}>
        <span className="brand-mark"><Icon name="box" size={17} /></span>
        <span className="brand-name">FeRun</span>
      </a>

      {t && nav.length > 0 && (
        <nav className="snav">
          <div className="snav-label">tenant · <span className="mono">{t}</span></div>
          {nav.map((n) => (
            <NavItem key={n.view} icon={n.icon} label={n.label} active={(n.views || [n.view]).includes(route.view)}
              onClick={() => navigate(n.view === 'environments' ? envPath(t) : `/tenants/${enc(t)}${n.path}`)} />
          ))}
        </nav>
      )}

      {operator && (
        <nav className="snav">
          <div className="snav-label">platform</div>
          <NavItem icon="users" label="Tenants" active={route.view === 'tenants'} onClick={() => navigate('/tenants')} />
          <NavItem icon="server" label="Nodes" active={route.view === 'nodes' || route.view === 'node'} onClick={() => navigate('/nodes')} />
          <NavItem icon="ticket" label="Join tokens" active={route.view === 'joinTokens'} onClick={() => navigate('/join-tokens')} />
          <NavItem icon="layers" label="Instances" active={route.view === 'instances'} onClick={() => navigate('/instances')} />
        </nav>
      )}

      <nav className="snav">
        <div className="snav-label">{operator ? 'tenants' : 'your tenants'}</div>
        {!operator && (
          <NavItem icon="users" label="All yours" active={route.view === 'tenants'} onClick={() => navigate('/tenants')} />
        )}
        <div className="tenantlist">
          {tenants.map((tn) => (
            <button key={tn.tenant} className={'tenantchip' + (t === tn.tenant ? ' on' : '')}
              title={`your role: ${roleLabel(tn) || 'none'}`}
              onClick={() => navigate(tenantHome(tenantAccess(me, tn.tenant)))}>
              <span className="tenantchip-name">{tn.tenant}</span>
              {!operator && <RoleBadge role={roleLabel(tn)} />}
            </button>
          ))}
          {tenants.length === 0 && <span className="muted small">—</span>}
        </div>
      </nav>

      <div className="sidebar-foot">
        <span className="muted small">FeOS control plane</span>
        <ThemeToggle />
      </div>
    </aside>
  );
}

function NavItem({ icon, label, active, onClick }) {
  return (
    <button className={'snav-item' + (active ? ' on' : '')} onClick={onClick}>
      <Icon name={icon} size={15} /> <span>{label}</span>
    </button>
  );
}

function Topbar({ route, navigate, me, onSignOut }) {
  const t = route.tenant;
  const enc = encodeURIComponent;
  const crumb = (label, to, strong) => (
    <button className={'crumb' + (strong ? ' strong' : '')} onClick={() => navigate(to)}>{label}</button>
  );
  const sep = <span className="crumb-sep">/</span>;
  return (
    <header className="topbar">
      <nav className="crumbs">
        {(route.view === 'tenants' || t) && crumb('tenants', '/tenants')}
        {t && <>{sep}{crumb(t, `/tenants/${enc(t)}`, true)}</>}
        {route.view === 'sandboxes' && <>{sep}{crumb('sandboxes', `/tenants/${enc(t)}/sandboxes`)}</>}
        {route.view === 'sandboxes' && route.sel && <>{sep}<span className="crumb mono">{shortId(route.sel)}</span></>}
        {route.view === 'tools' && <>{sep}<span className="crumb">tools</span></>}
        {route.view === 'members' && <>{sep}<span className="crumb">members</span></>}
        {route.view === 'tokens' && <>{sep}<span className="crumb">tokens</span></>}
        {route.view === 'invite' && <span className="crumb">invitation</span>}
        {(route.view === 'environments' || route.view === 'newEnvironment') && <>{sep}{crumb('environments', envPath(t))}</>}
        {route.view === 'newEnvironment' && <>{sep}<span className="crumb">new</span></>}
        {route.view === 'environments' && route.env && <>{sep}{crumb(route.env, envPath(t, route.env))}</>}
        {route.view === 'environments' && route.env && route.tab !== 'overview' && (
          <>{sep}{crumb(route.tab, envPath(t, route.env, route.tab))}</>)}
        {route.view === 'environments' && route.item && (
          <>{sep}<span className="crumb mono">{route.item}{route.replica != null ? ` · replica ${route.replica}` : ''}</span></>)}
        {(route.view === 'nodes' || route.view === 'node') && crumb('nodes', '/nodes')}
        {route.view === 'node' && <>{sep}<span className="crumb mono strong">{route.node}</span></>}
        {route.view === 'joinTokens' && <span className="crumb">join tokens</span>}
        {route.view === 'instances' && <span className="crumb">instances</span>}
      </nav>
      <span className="spacer" />
      <span className="userbox" title={me.email || me.username || ''}>
        <Avatar src={me.picture} name={displayName(me)} />
        <span className="username">{displayName(me)}</span>
        {me.method === 'basic' && <span className="muted small">(password)</span>}
        {isOperator(me) && <span className="badge acc" title="an operator of this FeRun">operator</span>}
      </span>
      <button className="ghost mini" onClick={onSignOut}><Icon name="logout" size={13} /> Sign out</button>
    </header>
  );
}
