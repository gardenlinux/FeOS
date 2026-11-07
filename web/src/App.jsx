// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  api, auth, forgetTenantTokens, onSignedOut, setActiveTenant, setBasic,
} from './api.js';
import { readItem, writeItem } from './storage.js';
import { Icon } from './Icon.jsx';
import { Avatar, BrandMark, useTheme } from './components.jsx';
import { ENV_TABS, canonicalPath, chosenEnv, envPath, parsePath, switchEnvPath } from './routes.js';
import { shortId } from './format.js';
import { SignIn } from './views/SignIn.jsx';
import { TenantGate } from './views/TenantGate.jsx';
import { InviteAccept, Members, MyTenants, RoleBadge, Tokens } from './views/People.jsx';
import { Home } from './views/Home.jsx';
import { Tenants } from './views/Tenants.jsx';
import { Sandboxes } from './views/Sandboxes.jsx';
import { Tools } from './views/Tools.jsx';
import { Environments } from './views/Environments.jsx';
import { NewEnvironment } from './views/NewEnvironment.jsx';
import { Nodes } from './views/Nodes.jsx';
import { NodeDetail } from './views/NodeDetail.jsx';
import { JoinTokens } from './views/JoinTokens.jsx';
import { Instances } from './views/Instances.jsx';
import { Jobs } from './views/Jobs.jsx';
import { Domains } from './views/Domains.jsx';
import { Credentials } from './views/Credentials.jsx';
import { TenantSettings, Usage } from './views/TenantPages.jsx';
import { Ingresses, PlatformPeople } from './views/Platform.jsx';
import { Palette, usePaletteKey } from './views/Palette.jsx';
import { SummaryProvider, useSummary } from './summary.jsx';
import { entries } from './search.js';
import { deploymentTone } from './views/envs.js';
import { ToneDot } from './views/Environments.jsx';
import {
  ENV_NAV, MeContext, NAV_GROUPS, SETTINGS_NAV, OPERATOR_VIEWS, PLATFORM_GROUPS, PLATFORM_NAV, can, isOperator, myTenants, roleLabel,
  tenantAccess, tenantHome, tenantNav, viewGate,
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
          <div className="login-brand"><BrandMark size={36} /> <span className="login-name">Fe<b>Run</b></span></div>
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

  const access = tenantAccess(me, route.tenant);
  const tenantCan = can(access);
  return (
    <MeContext.Provider value={meCtx}>
    <SummaryProvider tenant={needsGate ? '' : route.tenant || ''} can={tenantCan}>
    <Shell route={route} navigate={navigate} me={me} onSignOut={signOut}>
      {route.view === 'tenants' && (operator ? <Tenants navigate={navigate} /> : <MyTenants navigate={navigate} />)}
      {route.view === 'invite' && <InviteAccept key={route.token} token={route.token} navigate={navigate} />}
      {needsGate && <TenantGate key={route.tenant} reason={gate} tenant={route.tenant} navigate={navigate} />}
      {!needsGate && route.view === 'overview' && <Home key={route.tenant} tenant={route.tenant} navigate={navigate} />}
      {!needsGate && route.view === 'sandboxes' && (
        <Sandboxes key={route.tenant} tenant={route.tenant} sel={route.sel} tab={route.tab} navigate={navigate} />)}
      {!needsGate && route.view === 'tools' && <Tools key={route.tenant} tenant={route.tenant} navigate={navigate} />}
      {!needsGate && route.view === 'jobs' && <Jobs key={route.tenant} tenant={route.tenant} sel={route.sel} navigate={navigate} />}
      {!needsGate && route.view === 'domains' && <Domains key={route.tenant} tenant={route.tenant} sel={route.sel} navigate={navigate} />}
      {!needsGate && (route.view === 'credentials' || route.view === 'newCredential') && (
        <Credentials key={route.tenant} tenant={route.tenant} sel={route.view === 'newCredential' ? '' : route.sel}
          isNew={route.view === 'newCredential'} edit={route.tab === 'edit'} navigate={navigate} />)}
      {!needsGate && SETTINGS_VIEWS.has(route.view) && <SettingsTabs route={route} me={me} navigate={navigate} />}
      {!needsGate && route.view === 'members' && <Members key={route.tenant} tenant={route.tenant} navigate={navigate} />}
      {!needsGate && route.view === 'tokens' && <Tokens key={route.tenant} tenant={route.tenant} />}
      {!needsGate && route.view === 'usage' && <Usage key={route.tenant} tenant={route.tenant} navigate={navigate} />}
      {!needsGate && route.view === 'settings' && <TenantSettings key={route.tenant} tenant={route.tenant} navigate={navigate} />}
      {!needsGate && route.view === 'environments' && (
        <Environments key={route.tenant} tenant={route.tenant} env={route.env} tab={route.tab} item={route.item}
          replica={route.replica} search={route.search} navigate={navigate} />)}
      {!needsGate && route.view === 'newEnvironment' && <NewEnvironment key={route.tenant} tenant={route.tenant} navigate={navigate} />}
      {!needsGate && route.view === 'nodes' && <Nodes navigate={navigate} />}
      {!needsGate && route.view === 'node' && <NodeDetail key={route.node} node={route.node} tab={route.tab} navigate={navigate} />}
      {!needsGate && route.view === 'joinTokens' && <JoinTokens />}
      {!needsGate && route.view === 'instances' && <Instances />}
      {!needsGate && route.view === 'ingresses' && <Ingresses navigate={navigate} />}
      {!needsGate && route.view === 'people' && <PlatformPeople navigate={navigate} />}
    </Shell>
    </SummaryProvider>
    </MeContext.Provider>
  );
}

// ------------------------------------------------------------------ shell --

// platformSpace tells whether a route is in the operators' space: their
// pages, and the list of every tenant.
function platformSpace(route, operator) {
  return OPERATOR_VIEWS.has(route.view) || (operator && route.view === 'tenants');
}

// lastEnvKey stores the environment last open in a tenant: the sidebar
// keeps its pages while one is on the tenant's other pages.
const lastEnvKey = (tenant) => `ferun-env/${tenant}`;

// Shell is the sidebar, the topbar and the page, and the search above
// them.
function Shell({ route, navigate, me, onSignOut, children }) {
  const [searching, setSearching] = useState(false);
  const open = useCallback(() => setSearching(true), []);
  usePaletteKey(open);
  const summary = useSummary();
  const operator = isOperator(me);
  useEffect(() => {
    if (route.tenant && route.view === 'environments' && route.env) writeItem('localStorage', lastEnvKey(route.tenant), route.env);
  }, [route.tenant, route.view, route.env]);
  const env = route.tenant ? chosenEnv(route, summary.environments, readItem('localStorage', lastEnvKey(route.tenant))) : null;
  const all = useMemo(() => entries({
    tenant: route.tenant || '',
    nav: route.tenant ? tenantNav(tenantAccess(me, route.tenant)) : [],
    platform: operator ? PLATFORM_NAV : [],
    tenants: myTenants(me),
    summary,
  }), [route.tenant, me, operator, summary]);
  return (
    <div className="app">
      <Sidebar route={route} navigate={navigate} me={me} env={env} />
      <div className="main">
        <Topbar route={route} navigate={navigate} me={me} onSearch={open} onSignOut={onSignOut} />
        <main className="content">{children}</main>
      </div>
      {searching && (
        <Palette all={all} tenant={route.tenant || ''} environments={summary.environments || []}
          onClose={() => setSearching(false)} navigate={navigate} />)}
    </div>
  );
}

function Sidebar({ route, navigate, me, env }) {
  const operator = isOperator(me);
  const platform = platformSpace(route, operator);
  const t = route.tenant;
  const access = tenantAccess(me, t);
  const nav = t ? tenantNav(access) : [];
  const { counts } = useSummary();
  const enc = encodeURIComponent;
  const toOf = (n) => (n.view === 'environments' ? envPath(t) : `/tenants/${enc(t)}${n.path}`);
  const on = (n) => (n.views || [n.view]).includes(route.view);

  return (
    <aside className="sidebar">
      <a className="brand" href={t ? `/tenants/${enc(t)}` : '/tenants'}
        onClick={(e) => { e.preventDefault(); navigate(t ? `/tenants/${enc(t)}` : '/tenants'); }}>
        <BrandMark size={30} />
        <span className="brand-name">Fe<b>Run</b></span>
      </a>

      {!platform && t && nav.length > 0 && NAV_GROUPS.map((g) => {
        const items = nav.filter((n) => n.group === g.key);
        if (items.length === 0) return null;
        if (g.key === 'env') {
          return <EnvPages key="env" route={route} navigate={navigate} env={env} canCreate={can(access).writeTenant} />;
        }
        if (g.key === 'manage') {
          const item = { ...SETTINGS_NAV, views: items.map((n) => n.view) };
          return (
            <nav key="manage" className="snav">
              <NavItem item={item} active={on(item)} to={toOf(items[0])} navigate={navigate} />
            </nav>
          );
        }
        return (
          <nav key={g.key || 'top'} className="snav">
            {g.label && <div className="snav-label">{g.label}</div>}
            {items.map((n) => (
              <NavItem key={n.view} item={n} active={on(n)} count={counts[n.view]} to={toOf(n)} navigate={navigate} />
            ))}
          </nav>
        );
      })}

      {platform && PLATFORM_GROUPS.map((g) => (
        <nav key={g.key} className="snav">
          <div className="snav-label">{g.label}</div>
          {PLATFORM_NAV.filter((n) => n.group === g.key).map((n) => (
            <NavItem key={n.view} item={n} active={on(n)} to={n.path} navigate={navigate} />
          ))}
        </nav>
      ))}

      {!platform && !t && (
        <p className="sidehint muted small">Choose a tenant at the top, or make one.</p>)}

    </aside>
  );
}

// useMenu is a dropdown's open state: closed by a click outside it or by
// Escape.
function useMenu() {
  const [open, setOpen] = useState(false);
  const box = useRef(null);
  useEffect(() => {
    if (!open) return undefined;
    const close = (e) => { if (box.current && !box.current.contains(e.target)) setOpen(false); };
    // Escape closes the menu alone, not also the page below it.
    const esc = (e) => { if (e.key === 'Escape') { e.stopPropagation(); setOpen(false); } };
    document.addEventListener('mousedown', close);
    window.addEventListener('keydown', esc, true);
    return () => { document.removeEventListener('mousedown', close); window.removeEventListener('keydown', esc, true); };
  }, [open]);
  return [open, setOpen, box];
}

// Switcher is where one works, at the head of the topbar's trail: a
// tenant and one of its environments, in two columns (choosing a tenant
// on the left lists its environments on the right), or, for operators,
// the platform.
function Switcher({ route, navigate, me, platform }) {
  const [open, setOpen, box] = useMenu();
  const [picked, setPicked] = useState('');
  const [others, setOthers] = useState({});
  const { environments } = useSummary();
  const operator = isOperator(me);
  const tenants = myTenants(me);
  const t = route.tenant;
  const envName = route.view === 'environments' ? route.env : '';

  const toggle = () => {
    if (!open) {
      setPicked(platform ? '' : t || (tenants[0] || {}).tenant || '');
      setOthers({});
    }
    setOpen(!open);
  };
  // Another tenant's environments are read when it is chosen.
  useEffect(() => {
    if (!open || !picked || picked === t || others[picked] !== undefined) return;
    setOthers((o) => ({ ...o, [picked]: null }));
    api.listEnvironmentsOf(picked).then(
      (r) => setOthers((o) => ({ ...o, [picked]: r.environments || [] })),
      () => setOthers((o) => ({ ...o, [picked]: 'error' })));
  }, [open, picked, t, others]);

  const go = (to) => { setOpen(false); navigate(to); };
  const envs = picked === t ? environments : others[picked];
  const pickedAccess = tenantAccess(me, picked);
  const role = !platform && t ? roleLabel(tenants.find((x) => x.tenant === t)) || (operator ? 'operator' : '') : '';
  const enc = encodeURIComponent;

  return (
    <div className="switcher" ref={box}>
      <button className={'switch' + (open ? ' on' : '')} onClick={toggle} aria-expanded={open}>
        <Icon name={platform ? 'server' : 'building'} size={14} />
        <span className="switch-name">{platform ? 'Platform' : t || 'Choose a tenant'}</span>
        {!platform && t && envName && <><span className="switch-slash">/</span><span className="switch-name">{envName}</span></>}
        {role && <span className="switch-role">{role}</span>}
        <Icon name="chevronDown" size={13} />
      </button>
      {open && (
        <div className="switch-menu" role="menu">
          <div className="switch-col">
            <div className="switch-label">{operator ? 'tenants you are a member of' : 'tenants'}</div>
            {tenants.map((tn) => (
              <button key={tn.tenant} role="menuitem" className={'switch-item' + (tn.tenant === picked ? ' on' : '')}
                onClick={() => setPicked(tn.tenant)} aria-haspopup="true">
                <span className="mono">{tn.tenant}</span>
                <span className="switch-role">{roleLabel(tn)}</span>
                <Icon name="chevronRight" size={12} />
              </button>
            ))}
            {tenants.length === 0 && <div className="switch-empty muted small">none yet</div>}
            <div className="switch-sep" />
            <button role="menuitem" className="switch-item" onClick={() => go('/tenants')}>
              <Icon name="plus" size={13} /> {operator ? 'every tenant' : 'all yours, or a new one'}
            </button>
            {operator && (
              <button role="menuitem" className={'switch-item' + (platform ? ' on' : '')} onClick={() => go('/nodes')}>
                <Icon name="server" size={13} /> Platform <span className="switch-role">operator</span>
              </button>)}
          </div>
          {picked && (
            <div className="switch-col">
              <div className="switch-label">environments of <span className="mono">{picked}</span></div>
              {(envs === null || envs === undefined) && <div className="switch-empty muted small">…</div>}
              {envs === 'error' && <div className="switch-empty muted small">could not be read</div>}
              {Array.isArray(envs) && envs.length === 0 && <div className="switch-empty muted small">none yet</div>}
              {Array.isArray(envs) && envs.map((e) => (
                <button key={e.id} role="menuitem"
                  className={'switch-item' + (picked === t && (envName === e.name || envName === e.id) ? ' on' : '')}
                  onClick={() => go(switchEnvPath(route, picked, e.name))}>
                  <ToneDot tone={deploymentTone(e.state)} title={e.state} />
                  <span className="mono">{e.name}</span>
                  {Number(e.openIncidents) > 0 && <span className="snav-count bad" title="open incidents">{e.openIncidents}</span>}
                </button>
              ))}
              <div className="switch-sep" />
              <button role="menuitem" className="switch-item" onClick={() => go(tenantHome(pickedAccess))}>
                <Icon name="home" size={13} /> Tenant home
              </button>
              <button role="menuitem" className="switch-item" onClick={() => go(envPath(picked))}>
                <Icon name="layers" size={13} /> All environments
              </button>
              {can(pickedAccess).writeTenant && (
                <button role="menuitem" className="switch-item" onClick={() => go(`/tenants/${enc(picked)}/new-environment`)}>
                  <Icon name="plus" size={13} /> New environment
                </button>)}
            </div>)}
        </div>)}
    </div>
  );
}

// EnvPages are the pages of one environment in the sidebar: the open
// one, else the one last open, else the tenant's first (chosenEnv); the
// topbar's switcher chooses another.
function EnvPages({ route, navigate, env, canCreate }) {
  const { environments } = useSummary();
  const t = route.tenant;
  const go = (to) => (e) => { e.preventDefault(); navigate(to); };
  const known = (environments || []).find((e) => env && e.id === env.id) || env;
  const here = env && route.view === 'environments' && (route.env === env.name || route.env === env.id);
  const newTo = `/tenants/${encodeURIComponent(t)}/new-environment`;
  if (!env) {
    return (
      <nav className="snav envnav">
        <div className="snav-label">Environment</div>
        {environments === null
          ? <div className="sidehint muted small">…</div>
          : <div className="sidehint muted small">None yet.</div>}
        {environments && canCreate && (
          <a className={'snav-item' + (route.view === 'newEnvironment' ? ' on' : '')} href={newTo} onClick={go(newTo)}>
            <Icon name="plus" size={15} /> <span>New environment</span></a>)}
      </nav>
    );
  }
  const incidents = Number(known.openIncidents) || 0;
  return (
    <nav className="snav envnav">
      <div className="snav-label snav-label-row">
        <span>Environment</span>
        <span className="envlabel">
          {known.state && <ToneDot tone={deploymentTone(known.state)} title={known.state} />}
          <span className="envname">{known.name}</span>
        </span>
      </div>
      {ENV_NAV.filter((n) => ENV_TABS.includes(n.tab)).map((n) => {
        const to = envPath(t, known.name, n.tab);
        const on = here && (route.tab === n.tab || (n.tab === 'overview' && route.tab === 'objects'));
        return (
          <a key={n.tab} className={'snav-item' + (on ? ' on' : '')} href={to} onClick={go(to)}
            aria-current={on ? 'page' : undefined}>
            <Icon name={n.icon} size={15} /> <span>{n.label}</span>
            {n.tab === 'incidents' && incidents > 0 && <span className="snav-count bad">{incidents}</span>}
          </a>
        );
      })}
    </nav>
  );
}

function NavItem({ item, active, count, to, navigate }) {
  return (
    <a className={'snav-item' + (active ? ' on' : '')} href={to}
      onClick={(e) => { e.preventDefault(); navigate(to); }} aria-current={active ? 'page' : undefined}>
      <Icon name={item.icon} size={15} /> <span>{item.label}</span>
      {count && <span className={'snav-count' + (count.tone ? ' ' + count.tone : '')}>{count.n}</span>}
    </a>
  );
}

// SettingsTabs are the tenant's settings, one page each: the tabs over
// them (the sidebar's "Tenant settings").
const SETTINGS_VIEWS = new Set(['members', 'tokens', 'usage', 'settings']);
function SettingsTabs({ route, me, navigate }) {
  const t = encodeURIComponent(route.tenant);
  const pages = tenantNav(tenantAccess(me, route.tenant)).filter((n) => n.group === 'manage');
  return (
    <div className="tabbar settings-tabs">
      {pages.map((n) => (
        <a key={n.view} href={`/tenants/${t}${n.path}`} className={'tab' + (route.view === n.view ? ' on' : '')}
          onClick={(e) => { e.preventDefault(); navigate(`/tenants/${t}${n.path}`); }}>
          <Icon name={n.icon} size={13} /> {n.label}
        </a>
      ))}
    </div>
  );
}

// UserMenu is the signed-in person at the topbar's right end: who, the
// architecture page, the theme and signing out.
function UserMenu({ me, onSignOut }) {
  const [open, setOpen, box] = useMenu();
  const [theme, toggleTheme] = useTheme();
  const name = displayName(me);
  return (
    <div className="usermenu" ref={box}>
      <button className={'userbtn' + (open ? ' on' : '')} onClick={() => setOpen(!open)} aria-expanded={open}
        title={me.email || me.username || ''}>
        <Avatar src={me.picture} name={name} />
        <span className="username">{name}</span>
        <Icon name="chevronDown" size={13} />
      </button>
      {open && (
        <div className="user-menu" role="menu">
          <div className="user-who">
            <strong>{name}</strong>
            {me.email && me.email !== name && <span className="muted small">{me.email}</span>}
          </div>
          <div className="switch-sep" />
          <a role="menuitem" className="switch-item" href="/docs/"><Icon name="book" size={13} /> Architecture of FeRun</a>
          <button role="menuitem" className="switch-item" onClick={toggleTheme}>
            <Icon name={theme === 'dark' ? 'sun' : 'moon'} size={13} /> {theme === 'dark' ? 'Light mode' : 'Dark mode'}
          </button>
          <div className="switch-sep" />
          <button role="menuitem" className="switch-item" onClick={onSignOut}><Icon name="logout" size={13} /> Sign out</button>
        </div>)}
    </div>
  );
}

// CRUMB_NAMES name the tenant's pages in the trail.
const CRUMB_NAMES = {
  sandboxes: 'sandboxes', tools: 'tools', jobs: 'jobs', domains: 'domains', members: 'people', tokens: 'tokens',
  usage: 'usage', settings: 'general', credentials: 'credentials',
};

function Topbar({ route, navigate, me, onSearch, onSignOut }) {
  const t = route.tenant;
  const platform = platformSpace(route, isOperator(me));
  const enc = encodeURIComponent;
  const crumb = (label, to, strong) => (
    <button className={'crumb' + (strong ? ' strong' : '')} onClick={() => navigate(to)}>{label}</button>
  );
  const sep = <span className="crumb-sep">/</span>;
  const page = CRUMB_NAMES[route.view];
  const { domains } = useSummary();
  // A domain's page is addressed by its ID; the trail names it.
  const domainName = (sel) => ((domains || []).find((d) => d.id === sel) || {}).name || sel;
  return (
    <header className="topbar">
      <Switcher route={route} navigate={navigate} me={me} platform={platform} />
      <nav className="crumbs">
        {t && SETTINGS_VIEWS.has(route.view) && <>{sep}<span className="crumb">tenant settings</span></>}
        {t && page && <>{sep}{crumb(page, `/tenants/${enc(t)}/${route.view === 'members' ? 'members' : route.view}`)}</>}
        {route.view === 'sandboxes' && route.sel === 'new' && <>{sep}<span className="crumb">new</span></>}
        {route.view === 'sandboxes' && route.sel && route.sel !== 'new' && <>{sep}<span className="crumb mono">{shortId(route.sel)}</span></>}
        {route.view === 'jobs' && route.sel && <>{sep}<span className="crumb mono">{route.sel}</span></>}
        {route.view === 'domains' && route.sel && <>{sep}<span className="crumb mono">{domainName(route.sel)}</span></>}
        {route.view === 'credentials' && route.sel && <>{sep}<span className="crumb mono">{route.sel}</span></>}
        {route.view === 'newCredential' && <>{sep}{crumb('credentials', `/tenants/${enc(t)}/credentials`)}{sep}<span className="crumb">new</span></>}
        {route.view === 'invite' && <>{sep}<span className="crumb">invitation</span></>}
        {((route.view === 'environments' && !route.env) || route.view === 'newEnvironment') && <>{sep}{crumb('environments', envPath(t))}</>}
        {route.view === 'newEnvironment' && <>{sep}<span className="crumb">new</span></>}
        {route.view === 'environments' && route.env && route.tab !== 'overview' && (
          <>{sep}{crumb(route.tab, envPath(t, route.env, route.tab))}</>)}
        {route.view === 'environments' && route.item && (
          <>{sep}<span className="crumb mono">{route.item}{route.replica != null ? ` · replica ${route.replica}` : ''}</span></>)}
        {route.view === 'tenants' && <>{sep}<span className="crumb strong">tenants</span></>}
        {(route.view === 'nodes' || route.view === 'node') && <>{sep}{crumb('nodes', '/nodes', route.view === 'nodes')}</>}
        {route.view === 'node' && <>{sep}<span className="crumb mono strong">{route.node}</span></>}
        {route.view === 'joinTokens' && <>{sep}<span className="crumb strong">join tokens</span></>}
        {route.view === 'instances' && <>{sep}<span className="crumb strong">control plane</span></>}
        {route.view === 'ingresses' && <>{sep}<span className="crumb strong">ingresses</span></>}
        {route.view === 'people' && <>{sep}<span className="crumb strong">people</span></>}
      </nav>
      <button className="topsearch" onClick={onSearch} title="search (⌘K)">
        <Icon name="search" size={14} /> <span>Search or jump to…</span> <kbd>⌘K</kbd>
      </button>
      <UserMenu me={me} onSignOut={onSignOut} />
    </header>
  );
}
