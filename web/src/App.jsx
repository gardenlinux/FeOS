// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  auth, forgetTenantTokens, onSignedOut, setActiveTenant, setBasic,
} from './api.js';
import { Icon } from './Icon.jsx';
import { Avatar, BrandMark, ThemeToggle } from './components.jsx';
import { ENV_TABS, canonicalPath, envPath, parsePath } from './routes.js';
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
  ENV_NAV, MeContext, NAV_GROUPS, OPERATOR_VIEWS, PLATFORM_GROUPS, PLATFORM_NAV, can, isOperator, myTenants, roleLabel,
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
          <div className="login-brand"><BrandMark size={36} /> <span className="login-name">FeRun</span></div>
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

// Shell is the sidebar, the topbar and the page, and the search above
// them.
function Shell({ route, navigate, me, onSignOut, children }) {
  const [searching, setSearching] = useState(false);
  const open = useCallback(() => setSearching(true), []);
  usePaletteKey(open);
  const summary = useSummary();
  const operator = isOperator(me);
  const all = useMemo(() => entries({
    tenant: route.tenant || '',
    nav: route.tenant ? tenantNav(tenantAccess(me, route.tenant)) : [],
    platform: operator ? PLATFORM_NAV : [],
    tenants: myTenants(me),
    summary,
  }), [route.tenant, me, operator, summary]);
  return (
    <div className="app">
      <Sidebar route={route} navigate={navigate} me={me} onSearch={open} onSignOut={onSignOut} />
      <div className="main">
        <Topbar route={route} navigate={navigate} />
        <main className="content">{children}</main>
      </div>
      {searching && (
        <Palette all={all} tenant={route.tenant || ''} environments={summary.environments || []}
          onClose={() => setSearching(false)} navigate={navigate} />)}
    </div>
  );
}

function Sidebar({ route, navigate, me, onSearch, onSignOut }) {
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
        <span className="brand-name">FeRun</span>
      </a>

      <Switcher route={route} navigate={navigate} me={me} platform={platform} />

      <button className="sidesearch" onClick={onSearch}>
        <Icon name="search" size={14} /> <span>Search or jump to…</span> <kbd>⌘K</kbd>
      </button>

      {!platform && t && nav.length > 0 && NAV_GROUPS.map((g) => {
        const items = nav.filter((n) => n.group === g.key);
        if (items.length === 0) return null;
        if (g.key === 'env') {
          return <EnvSection key="env" route={route} navigate={navigate} canCreate={can(access).writeTenant} />;
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
        <p className="sidehint muted small">Choose a tenant above, or make one.</p>)}

      <div className="sidebar-foot">
        <span className="userbox" title={me.email || me.username || ''}>
          <Avatar src={me.picture} name={displayName(me)} />
          <span className="username">{displayName(me)}</span>
        </span>
        <span className="spacer" />
        <a className="iconbtn" href="/docs/" title="the architecture of FeRun and FeOS"><Icon name="book" size={14} /></a>
        <ThemeToggle />
        <button className="iconbtn" onClick={onSignOut} title="sign out"><Icon name="logout" size={14} /></button>
      </div>
    </aside>
  );
}

// Switcher is where one works: one of the person's tenants with the role
// in it, or, for operators, the platform.
function Switcher({ route, navigate, me, platform }) {
  const [open, setOpen] = useState(false);
  const box = useRef(null);
  const operator = isOperator(me);
  const tenants = myTenants(me);
  const t = route.tenant;
  useEffect(() => {
    if (!open) return undefined;
    const close = (e) => { if (box.current && !box.current.contains(e.target)) setOpen(false); };
    const esc = (e) => { if (e.key === 'Escape') setOpen(false); };
    document.addEventListener('mousedown', close);
    document.addEventListener('keydown', esc);
    return () => { document.removeEventListener('mousedown', close); document.removeEventListener('keydown', esc); };
  }, [open]);
  const go = (to) => { setOpen(false); navigate(to); };
  const current = platform ? 'Platform' : t || 'Choose a tenant';
  const role = !platform && t ? roleLabel(tenants.find((x) => x.tenant === t)) || (operator ? 'operator' : '') : '';

  return (
    <div className="switcher" ref={box}>
      <button className={'switch' + (open ? ' on' : '')} onClick={() => setOpen((o) => !o)} aria-expanded={open}>
        <Icon name={platform ? 'server' : 'building'} size={14} />
        <span className="switch-name">{current}</span>
        {role && <span className="switch-role">{role}</span>}
        <Icon name="chevronDown" size={13} />
      </button>
      {open && (
        <div className="switch-menu" role="menu">
          <div className="switch-label">{operator ? 'tenants you are a member of' : 'your tenants'}</div>
          {tenants.map((tn) => (
            <button key={tn.tenant} role="menuitem" className={'switch-item' + (tn.tenant === t && !platform ? ' on' : '')}
              onClick={() => go(tenantHome(tenantAccess(me, tn.tenant)))}>
              <span className="mono">{tn.tenant}</span>
              <span className="switch-role">{roleLabel(tn)}</span>
            </button>
          ))}
          {tenants.length === 0 && <div className="switch-empty muted small">none yet</div>}
          <button role="menuitem" className="switch-item" onClick={() => go('/tenants')}>
            <Icon name="plus" size={13} /> {operator ? 'every tenant' : 'all yours, or a new one'}
          </button>
          {operator && (
            <>
              <div className="switch-sep" />
              <button role="menuitem" className={'switch-item' + (platform ? ' on' : '')} onClick={() => go('/nodes')}>
                <Icon name="server" size={13} /> Platform <span className="switch-role">operator</span>
              </button>
            </>)}
        </div>)}
    </div>
  );
}

// ENV_SHOWN bounds the environments the section lists; the rest are a
// click away in the list.
const ENV_SHOWN = 12;

// EnvSection is the sidebar's environments: each with its state and its
// open incidents, the one the route is in unfolded into its pages; the
// list and a new one from the section's head.
function EnvSection({ route, navigate, canCreate }) {
  const { environments, counts } = useSummary();
  const t = route.tenant;
  const envs = environments || [];
  const here = route.view === 'environments' ? route.env : '';
  const open = envs.find((e) => e.name === here || e.id === here);
  const shown = envs.slice(0, ENV_SHOWN);
  if (open && !shown.includes(open)) shown.push(open);
  const tabs = ENV_NAV.filter((n) => ENV_TABS.includes(n.tab));
  const go = (to) => (e) => { e.preventDefault(); navigate(to); };
  const listOn = (route.view === 'environments' && !route.env) || route.view === 'newEnvironment';
  return (
    <nav className="snav envnav">
      <div className="snav-label snav-label-row">
        <a href={envPath(t)} onClick={go(envPath(t))} className={listOn ? 'on' : ''}>Environments</a>
        {counts.environments && counts.environments.tone && (
          <span className="snav-count bad" title="open incidents">{counts.environments.n}</span>)}
        {canCreate && (
          <a className="snav-add" href={`/tenants/${encodeURIComponent(t)}/new-environment`}
            onClick={go(`/tenants/${encodeURIComponent(t)}/new-environment`)} title="new environment">
            <Icon name="plus" size={13} /></a>)}
      </div>
      {environments === null && <div className="sidehint muted small">…</div>}
      {environments && envs.length === 0 && (
        <div className="sidehint muted small">None yet.{canCreate ? ' + makes the first.' : ''}</div>)}
      {shown.map((e) => {
        const isOpen = open === e;
        return (
          <div key={e.id} className={'envnode' + (isOpen ? ' open' : '')}>
            <a className={'snav-item envitem' + (isOpen && route.tab === 'overview' ? ' on' : '') + (isOpen ? ' here' : '')}
              href={envPath(t, e.name)} onClick={go(envPath(t, e.name))} aria-expanded={isOpen}>
              <Icon name={isOpen ? 'chevronDown' : 'chevronRight'} size={12} />
              <ToneDot tone={deploymentTone(e.state)} title={e.state} />
              <span className="envname">{e.name}</span>
              {Number(e.openIncidents) > 0 && <span className="snav-count bad" title="open incidents">{e.openIncidents}</span>}
            </a>
            {isOpen && (
              <div className="envtabs">
                {tabs.map((n) => {
                  const to = envPath(t, e.name, n.tab);
                  const on = route.tab === n.tab && n.tab !== 'overview';
                  if (n.tab === 'overview') return null;
                  return (
                    <a key={n.tab} className={'snav-item envtab' + (on ? ' on' : '')} href={to} onClick={go(to)}
                      aria-current={on ? 'page' : undefined}>
                      <Icon name={n.icon} size={13} /> <span>{n.label}</span>
                      {n.tab === 'incidents' && Number(e.openIncidents) > 0 && (
                        <span className="snav-count bad">{e.openIncidents}</span>)}
                    </a>
                  );
                })}
              </div>)}
          </div>
        );
      })}
      {envs.length > shown.length && (
        <a className="snav-item envmore" href={envPath(t)} onClick={go(envPath(t))}>
          <span>{envs.length - shown.length} more…</span></a>)}
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

// CRUMB_NAMES name the tenant's pages in the trail.
const CRUMB_NAMES = {
  sandboxes: 'sandboxes', tools: 'tools', jobs: 'jobs', domains: 'domains', members: 'people', tokens: 'tokens',
  usage: 'usage', settings: 'settings', credentials: 'credentials',
};

function Topbar({ route, navigate }) {
  const t = route.tenant;
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
      <nav className="crumbs">
        {t && crumb(t, `/tenants/${enc(t)}`, true)}
        {t && page && <>{sep}{crumb(page, `/tenants/${enc(t)}/${route.view === 'members' ? 'members' : route.view}`)}</>}
        {route.view === 'sandboxes' && route.sel === 'new' && <>{sep}<span className="crumb">new</span></>}
        {route.view === 'sandboxes' && route.sel && route.sel !== 'new' && <>{sep}<span className="crumb mono">{shortId(route.sel)}</span></>}
        {route.view === 'jobs' && route.sel && <>{sep}<span className="crumb mono">{route.sel}</span></>}
        {route.view === 'domains' && route.sel && <>{sep}<span className="crumb mono">{domainName(route.sel)}</span></>}
        {route.view === 'credentials' && route.sel && <>{sep}<span className="crumb mono">{route.sel}</span></>}
        {route.view === 'newCredential' && <>{sep}{crumb('credentials', `/tenants/${enc(t)}/credentials`)}{sep}<span className="crumb">new</span></>}
        {route.view === 'invite' && <span className="crumb">invitation</span>}
        {(route.view === 'environments' || route.view === 'newEnvironment') && <>{sep}{crumb('environments', envPath(t))}</>}
        {route.view === 'newEnvironment' && <>{sep}<span className="crumb">new</span></>}
        {route.view === 'environments' && route.env && <>{sep}{crumb(route.env, envPath(t, route.env))}</>}
        {route.view === 'environments' && route.env && route.tab !== 'overview' && (
          <>{sep}{crumb(route.tab, envPath(t, route.env, route.tab))}</>)}
        {route.view === 'environments' && route.item && (
          <>{sep}<span className="crumb mono">{route.item}{route.replica != null ? ` · replica ${route.replica}` : ''}</span></>)}
        {route.view === 'tenants' && <span className="crumb strong">tenants</span>}
        {(route.view === 'nodes' || route.view === 'node') && crumb('nodes', '/nodes', route.view === 'nodes')}
        {route.view === 'node' && <>{sep}<span className="crumb mono strong">{route.node}</span></>}
        {route.view === 'joinTokens' && <span className="crumb strong">join tokens</span>}
        {route.view === 'instances' && <span className="crumb strong">control plane</span>}
        {route.view === 'ingresses' && <span className="crumb strong">ingresses</span>}
        {route.view === 'people' && <span className="crumb strong">people</span>}
      </nav>
    </header>
  );
}
