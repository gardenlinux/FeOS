// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useEffect, useReducer, useState } from 'react';
import {
  api, auth, getTenantToken, onSignedOut, onTenantTokens, setActiveTenant, setBasic,
} from './api.js';
import { Icon } from './Icon.jsx';
import { Avatar, ThemeToggle, usePoll } from './components.jsx';
import { TENANT_VIEWS, canonicalPath, envPath, parsePath } from './routes.js';
import { shortId } from './format.js';
import { SignIn } from './views/SignIn.jsx';
import { TenantGate } from './views/TenantGate.jsx';
import { Overview } from './views/Overview.jsx';
import { Tenants } from './views/Tenants.jsx';
import { Sandboxes } from './views/Sandboxes.jsx';
import { Tools } from './views/Tools.jsx';
import { Environments } from './views/Environments.jsx';
import { Nodes } from './views/Nodes.jsx';
import { NodeDetail } from './views/NodeDetail.jsx';
import { JoinTokens } from './views/JoinTokens.jsx';
import { Instances } from './views/Instances.jsx';

// The management UI of FeRun. Before anything else it asks ferund who is
// signed in (/auth/me) and how one signs in (/auth/config); signed out,
// or once an admin call is answered 401, it shows the sign-in screen.

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
  const [, bump] = useReducer((x) => x + 1, 0);

  const check = useCallback(async () => {
    try {
      const [config, me] = await Promise.all([auth.config(), auth.me()]);
      if (!me) setBasic('');
      setSession({ phase: me ? 'in' : 'out', config, me });
    } catch (e) {
      setSession({ phase: 'error', error: e.message });
    }
  }, []);
  useEffect(() => { check(); }, [check]);
  useEffect(() => onSignedOut(() => setSession((s) => ({
    ...s, phase: 'out', me: null, notice: 'Your sign-in has ended. Sign in again.',
  }))), []);
  useEffect(() => onTenantTokens(bump), []);

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
  const user = me.username || me.email || '';
  const needsGate = TENANT_VIEWS.has(route.view) && route.tenant && !getTenantToken(route.tenant);
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
    <div className="app">
      <Sidebar route={route} navigate={navigate} />
      <div className="main">
        <Topbar route={route} navigate={navigate} me={me} onSignOut={signOut} />
        <main className="content">
          {route.view === 'tenants' && <Tenants navigate={navigate} user={user} />}
          {needsGate && <TenantGate key={route.tenant} tenant={route.tenant} user={user} />}
          {!needsGate && route.view === 'overview' && <Overview key={route.tenant} tenant={route.tenant} navigate={navigate} />}
          {!needsGate && route.view === 'sandboxes' && (
            <Sandboxes key={route.tenant} tenant={route.tenant} sel={route.sel} tab={route.tab} navigate={navigate} />)}
          {!needsGate && route.view === 'tools' && <Tools key={route.tenant} />}
          {!needsGate && route.view === 'environments' && (
            <Environments key={route.tenant} tenant={route.tenant} env={route.env} tab={route.tab} item={route.item}
              replica={route.replica} search={route.search} navigate={navigate} />)}
          {route.view === 'nodes' && <Nodes navigate={navigate} />}
          {route.view === 'node' && <NodeDetail key={route.node} node={route.node} tab={route.tab} navigate={navigate} />}
          {route.view === 'joinTokens' && <JoinTokens />}
          {route.view === 'instances' && <Instances />}
        </main>
      </div>
    </div>
  );
}

// ------------------------------------------------------------------ shell --

function Sidebar({ route, navigate }) {
  const [tenants, setTenants] = useState([]);
  const refresh = useCallback(async () => {
    try {
      setTenants((await api.listTenants()).tenants || []);
    } catch {
      // The sidebar's list is a convenience; the views show the errors.
    }
  }, []);
  usePoll(refresh, 8000);

  const t = route.tenant;
  const enc = encodeURIComponent;
  const link = (to) => (e) => { e.preventDefault(); navigate(to); };

  return (
    <aside className="sidebar">
      <a className="brand" href="/tenants" onClick={link('/tenants')}>
        <span className="brand-mark"><Icon name="box" size={17} /></span>
        <span className="brand-name">FeRun</span>
      </a>

      {t && (
        <nav className="snav">
          <div className="snav-label">tenant · <span className="mono">{t}</span></div>
          <NavItem icon="dashboard" label="Overview" active={route.view === 'overview'} onClick={() => navigate(`/tenants/${enc(t)}`)} />
          <NavItem icon="box" label="Sandboxes" active={route.view === 'sandboxes'} onClick={() => navigate(`/tenants/${enc(t)}/sandboxes`)} />
          <NavItem icon="blocks" label="Environments" active={route.view === 'environments'} onClick={() => navigate(envPath(t))} />
          <NavItem icon="terminal" label="Tools" active={route.view === 'tools'} onClick={() => navigate(`/tenants/${enc(t)}/tools`)} />
        </nav>
      )}

      <nav className="snav">
        <div className="snav-label">platform</div>
        <NavItem icon="users" label="Tenants" active={route.view === 'tenants'} onClick={() => navigate('/tenants')} />
        <NavItem icon="server" label="Nodes" active={route.view === 'nodes' || route.view === 'node'} onClick={() => navigate('/nodes')} />
        <NavItem icon="ticket" label="Join tokens" active={route.view === 'joinTokens'} onClick={() => navigate('/join-tokens')} />
        <NavItem icon="layers" label="Instances" active={route.view === 'instances'} onClick={() => navigate('/instances')} />
      </nav>

      <nav className="snav">
        <div className="snav-label">{t ? 'switch tenant' : 'work as a tenant'}</div>
        <div className="tenantlist">
          {tenants.map((tn) => (
            <button key={tn.name} className={'tenantchip' + (t === tn.name ? ' on' : '')}
              title={getTenantToken(tn.name) ? 'this browser holds a token of the tenant' : 'this browser holds no token of the tenant'}
              onClick={() => navigate(`/tenants/${enc(tn.name)}`)}>
              <span className="tenantchip-name">
                {tn.name}{!getTenantToken(tn.name) && <Icon name="lock" size={11} className="dim" />}
              </span>
              <span className="tenantchip-count mono">{tn.sandboxCount}</span>
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
        {route.view === 'environments' && <>{sep}{crumb('environments', envPath(t))}</>}
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
      {t && getTenantToken(t) && (
        <span className="muted mono small tokenhint" title="the tenant token this browser works with">token …{getTenantToken(t).slice(-8)}</span>
      )}
      <span className="userbox" title={me.email || me.username || ''}>
        <Avatar src={me.picture} name={displayName(me)} />
        <span className="username">{displayName(me)}</span>
        {me.method === 'basic' && <span className="muted small">(password)</span>}
      </span>
      <button className="ghost mini" onClick={onSignOut}><Icon name="logout" size={13} /> Sign out</button>
    </header>
  );
}
