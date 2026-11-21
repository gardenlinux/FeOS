// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  api, auth, forgetOldTokens, onSignedOut, setActiveProject, setBasic,
} from './api.js';
import { readItem, writeItem } from './storage.js';
import { Icon } from './Icon.jsx';
import { Avatar, BrandMark, EnvName, StateDot, useTheme } from './components.jsx';
import {
  ENV_TAB_LABELS, OBJECT_TAB_LABELS, canonicalPath, chosenEnv, envPath, jobPath, newEnvPath, newSandboxPath, parsePath,
  pickEnvPath, pickObjectPath, sandboxPath, switchEnvPath,
} from './routes.js';
import { shortId } from './format.js';
import { SignIn } from './views/SignIn.jsx';
import { ProjectGate } from './views/ProjectGate.jsx';
import { InviteAccept, Members, MyProjects, RoleBadge, Tokens } from './views/People.jsx';
import { Home } from './views/Home.jsx';
import { Projects } from './views/Projects.jsx';
import { Tools } from './views/Tools.jsx';
import { Environments } from './views/Environments.jsx';
import { NewEnvironment } from './views/NewEnvironment.jsx';
import { Nodes } from './views/Nodes.jsx';
import { NodeDetail } from './views/NodeDetail.jsx';
import { JoinTokens } from './views/JoinTokens.jsx';
import { Instances } from './views/Instances.jsx';
import { Moved } from './views/Moved.jsx';
import { Domains } from './views/Domains.jsx';
import { Providers } from './views/Providers.jsx';
import { ProjectSettings, Usage } from './views/ProjectPages.jsx';
import { Ingresses, PlatformPeople } from './views/Platform.jsx';
import { Palette, usePaletteKey } from './views/Palette.jsx';
import { SummaryProvider, useSummary } from './summary.jsx';
import { entries } from './search.js';
import {
  KIND_TITLES, deploymentText, deploymentTone, groupObjects, objectStateText, objectTone, sideMeta, statusState,
} from './views/envs.js';
import { sandboxMeta, sideSandboxes } from './views/sandboxes.js';
import { jobTone, onItsWay } from './views/jobs.js';
import { EnvStatusProvider, useEnvStatus } from './envstatus.jsx';
import './styles/shell.css';
import { ToneDot } from './views/Environments.jsx';
import {
  MeContext, SETTINGS_NAV, OPERATOR_VIEWS, PLATFORM_GROUPS, PLATFORM_NAV, atLeast, can, envRole, higher, isOperator, myProjects,
  projectAccess, projectHome, projectNav, viewGate,
} from './people.js';

// The management UI of FeRun, the projects' console (ADR 0031). Before
// anything else it asks ferund who is signed in (/auth/me: the person,
// whether an operator, their projects with their roles) and how one signs
// in (/auth/config); signed out, or once a call is answered 401, it shows
// the sign-in screen. /auth/me decides what the UI offers (people.js):
// operators get the operators' pages, everyone their projects.

// ME_POLL_MS is how often the UI asks /auth/me again, so that a grant
// given or taken elsewhere shows.
const ME_POLL_MS = 60000;

// useRoute follows the location, its path and query; a path under an
// earlier name is replaced by its canonical one (canonicalPath). A new
// path scrolls to the top, a new query alone (a search) does not.
// navigate(to, {replace: true}) replaces the history's entry instead of
// adding one, as a redirect does.
function useRoute() {
  const [path, setPath] = useState(() => canonical());
  useEffect(() => {
    const onPop = () => setPath(canonical());
    window.addEventListener('popstate', onPop);
    return () => window.removeEventListener('popstate', onPop);
  }, []);
  const navigate = useCallback((to, { replace = false } = {}) => {
    if (to !== location.pathname + location.search) {
      const samePath = to.split('?')[0] === location.pathname;
      if (replace) history.replaceState(null, '', to);
      else history.pushState(null, '', to);
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
      forgetOldTokens();
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

  // The project the route acts as, set during render: the views' effects
  // run before this component's, and call the API at once.
  setActiveProject(route.project || '');

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

  const access = projectAccess(me, route.project);
  const projectCan = can(access);
  return (
    <MeContext.Provider value={meCtx}>
    <SummaryProvider project={needsGate ? '' : route.project || ''} can={projectCan}>
    <Shell route={route} navigate={navigate} me={me} onSignOut={signOut}>
      {route.view === 'projects' && (operator
        ? <Projects key={location.search} navigate={navigate} />
        : <MyProjects key={location.search} navigate={navigate} />)}
      {route.view === 'invite' && <InviteAccept key={route.token} token={route.token} navigate={navigate} />}
      {needsGate && <ProjectGate key={route.project} reason={gate} project={route.project} navigate={navigate} />}
      {!needsGate && route.view === 'overview' && <Home key={route.project} project={route.project} navigate={navigate} />}
      {!needsGate && route.view === 'moved' && <Moved route={route} navigate={navigate} />}
      {!needsGate && route.view === 'tools' && <Tools key={route.project} />}
      {!needsGate && route.view === 'domains' && <Domains key={route.project} project={route.project} sel={route.sel} navigate={navigate} />}
      {!needsGate && (route.view === 'providers' || route.view === 'newProvider') && (
        <Providers key={route.project} project={route.project} sel={route.view === 'newProvider' ? '' : route.sel}
          isNew={route.view === 'newProvider'} edit={route.tab === 'edit'} navigate={navigate} />)}
      {!needsGate && SETTINGS_VIEWS.has(route.view) && <SettingsTabs route={route} me={me} navigate={navigate} />}
      {!needsGate && route.view === 'members' && <Members key={route.project} project={route.project} navigate={navigate} />}
      {!needsGate && route.view === 'tokens' && <Tokens key={route.project} project={route.project} />}
      {!needsGate && route.view === 'usage' && <Usage key={route.project} project={route.project} navigate={navigate} />}
      {!needsGate && route.view === 'settings' && <ProjectSettings key={route.project} project={route.project} navigate={navigate} />}
      {!needsGate && route.view === 'environments' && (
        <Environments key={route.project} project={route.project} env={route.env} tab={route.tab} item={route.item}
          otab={route.otab} replica={route.replica} search={route.search} navigate={navigate} />)}
      {!needsGate && route.view === 'newEnvironment' && <NewEnvironment key={route.project} project={route.project} navigate={navigate} />}
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
// pages, and the list of every project.
function platformSpace(route, operator) {
  return OPERATOR_VIEWS.has(route.view) || (operator && route.view === 'projects');
}

// lastEnvKey stores the environment last open in a project: the sidebar
// keeps its pages while one is on the project's other pages.
const lastEnvKey = (project) => `ferun-env/${project}`;

// Shell is the sidebar, the topbar and the page, and the search above
// them.
function Shell({ route, navigate, me, onSignOut, children }) {
  const [searching, setSearching] = useState(false);
  const open = useCallback(() => setSearching(true), []);
  usePaletteKey(open);
  const summary = useSummary();
  const operator = isOperator(me);
  useEffect(() => {
    if (route.project && route.view === 'environments' && route.env) writeItem('localStorage', lastEnvKey(route.project), route.env);
  }, [route.project, route.view, route.env]);
  const env = route.project ? chosenEnv(route, summary.environments, readItem('localStorage', lastEnvKey(route.project))) : null;
  const all = useMemo(() => entries({
    project: route.project || '',
    nav: route.project ? projectNav(projectAccess(me, route.project)) : [],
    platform: operator ? PLATFORM_NAV : [],
    projects: myProjects(me),
    summary,
  }), [route.project, me, operator, summary]);
  // The status the sidebar and the environment's pages read: of the
  // route's environment as the route names it, else of the chosen one.
  const inEnv = route.view === 'environments' && !!route.env;
  const statusOf = inEnv ? route.env : env ? env.name : '';
  return (
    <EnvStatusProvider env={statusOf} open={inEnv}>
    <div className="app">
      <Sidebar route={route} navigate={navigate} me={me} env={env} />
      <div className="main">
        <Topbar route={route} navigate={navigate} me={me} onSearch={open} onSignOut={onSignOut} />
        <main className="content">{children}</main>
      </div>
      {searching && (
        <Palette all={all} project={route.project || ''} environments={summary.environments || []}
          onClose={() => setSearching(false)} navigate={navigate} />)}
    </div>
    </EnvStatusProvider>
  );
}

function Sidebar({ route, navigate, me, env }) {
  const operator = isOperator(me);
  const platform = platformSpace(route, operator);
  const t = route.project;
  const access = projectAccess(me, t);
  const nav = t ? projectNav(access) : [];
  const inEnv = route.view === 'environments' && !!route.env;
  const home = t ? projectHome(access) : '/projects';

  return (
    <aside className="sidebar">
      <a className="brand" href={home} onClick={(e) => { e.preventDefault(); navigate(home); }}>
        <BrandMark />
        <span className="brand-name">Fe<b>Run</b></span>
      </a>

      {!platform && t && nav.some((n) => n.group === 'env') && (
        <EnvSide route={route} navigate={navigate} env={env} access={access} objects={inEnv} />)}

      {!platform && t && !inEnv && <ProjectPages route={route} navigate={navigate} nav={nav} project={t} />}

      {platform && PLATFORM_GROUPS.map((g) => (
        <nav key={g.key} className="snav">
          <div className="snav-label">{g.label}</div>
          {PLATFORM_NAV.filter((n) => n.group === g.key).map((n) => (
            <NavItem key={n.view} item={n} active={(n.views || [n.view]).includes(route.view)} to={n.path} navigate={navigate} />
          ))}
        </nav>
      ))}

      {!platform && !t && (
        <p className="sidehint muted small">Choose a project at the top, or make one.</p>)}

      <div className="side-foot">
        {!platform && t && inEnv && <ProjectRow project={t} nav={nav} to={home} navigate={navigate} />}
        {operator && (
          <nav className="snav snav-admin">
            <NavItem item={{ view: 'administration', label: 'Administration', icon: 'server' }} active={platform}
              to="/nodes" navigate={navigate} />
          </nav>)}
      </div>
    </aside>
  );
}

// plainClick tells a click the router takes from one that opens a new
// tab or window.
const plainClick = (e) => e.button === 0 && !e.metaKey && !e.ctrlKey && !e.shiftKey;

// EnvSide is the environment one works in, in the sidebar: a card with
// its name, its state and its host, which leads to it (in the same view
// from one of its objects' pages); while one of its pages is open
// (objects), below it what runs in it: its objects by kind, each with
// its state, which lead to their pages (in the view open now), its jobs
// made by a call that are on their way, and its sandboxes (ADR 0035).
// The topbar's switcher chooses another environment.
function EnvSide({ route, navigate, env, access, objects }) {
  const canCreate = can(access).writeProject;
  const { environments } = useSummary();
  const { st, env: statusEnv } = useEnvStatus();
  const t = route.project;
  const go = (to) => (e) => { if (plainClick(e)) { e.preventDefault(); navigate(to); } };
  if (!env) {
    const newTo = newEnvPath(t);
    return (
      <div className="side-env">
        <div className="snav-label">Environment</div>
        {environments === null
          ? <div className="sidehint muted small">…</div>
          : <div className="sidehint muted small">None yet.</div>}
        {environments && canCreate && (
          <a className={'snav-item' + (route.view === 'newEnvironment' ? ' on' : '')} href={newTo} onClick={go(newTo)}>
            <Icon name="plus" size={16} /> <span>New environment</span></a>)}
      </div>
    );
  }
  const known = (environments || []).find((e) => e.id === env.id || e.name === env.name) || env;
  // The status is of this environment when it was read by its name or ID.
  const status = st && [known.name, known.id].includes(statusEnv) ? st : null;
  const here = route.view === 'environments' && (route.env === known.name || route.env === known.id);
  const cardOn = here && route.tab !== 'objects';
  const state = status ? statusState(status) : known.state || '';
  const incidents = Number(status ? status.openIncidents : known.openIncidents) || 0;
  const tone = incidents > 0 ? 'failed' : deploymentTone(state);
  const facts = status || known;
  const host = status && status.public ? (status.hosts || [])[0] : '';
  // A host that admits visitors (ADR 0054) has a lock before it.
  const guarded = !!(status && status.public && status.access === 'visitors');
  const to = pickEnvPath(route, t, known.name);
  return (
    <div className="side-env">
      <a className={'envcard' + (cardOn ? ' on' : '')} href={to} onClick={go(to)} aria-current={cardOn ? 'page' : undefined}
        title="the environment: its overview, or the view open now">
        <EnvName name={known.name} />
        {state && (
          <span className={'envcard-state ' + tone}>
            <ToneDot tone={tone} title={state} />
            <span>
              {incidents > 0 && `${incidents} open incident${incidents === 1 ? '' : 's'} · `}
              {facts.deleting || state === 'deleting' ? 'deleting'
                : `deployment ${deploymentText({ deployment: facts.deployment, deployed: facts.deployed, state })}`}
            </span>
          </span>)}
        {host && (
          <span className="envcard-host" title={guarded ? `admits visitors of ${status.provider || 'ferun'}` : undefined}>
            {guarded && <span className="admits-lock"><Icon name="lock" size={11} /></span>}{host}</span>)}
      </a>
      {objects && (status
        ? <ObjectList route={route} navigate={navigate} st={status} project={t}
          write={atLeast(higher(envRole(access, status.id), envRole(access, status.name)), 'write')} />
        : <div className="sidehint muted small">…</div>)}
    </div>
  );
}

// ObjectList are what runs in an environment, in the sidebar: its
// objects by kind, each with its state's dot, its name, and what is wrong
// while something is; under its jobs those made by a call that are on
// their way, and the link to all of them (Activity's Jobs); then its
// sandboxes, the hibernated folded when there are many (sideSandboxes),
// with New sandbox where write allows.
function ObjectList({ route, navigate, st, project, write }) {
  const { sandboxes, jobs } = useEnvStatus();
  const [openAll, setOpenAll] = useState(false);
  const env = st.name;
  const inEnv = route.view === 'environments';
  const sel = inEnv && route.tab === 'objects' ? route.item : '';
  const selSandbox = inEnv && route.tab === 'sandboxes' ? route.item : '';
  const selJob = inEnv && route.tab === 'jobs' ? route.item : '';
  // Another sandbox opens in the view open now.
  const sbView = selSandbox && selSandbox !== 'new' ? route.otab : '';
  const groups = groupObjects(st.objects);
  const live = [...(jobs || [])].filter((j) => onItsWay(j.phase)).sort((a, b) => (b.createdAt || '').localeCompare(a.createdAt || ''));
  const kept = (jobs || []).length;
  if (kept > 0 && !groups.some((g) => g.kind === 'job')) groups.push({ kind: 'job', objects: [] });
  const side = sideSandboxes(sandboxes, { open: openAll, selected: selSandbox });
  const go = (to) => (e) => { if (plainClick(e)) { e.preventDefault(); navigate(to); } };
  const empty = groups.length === 0 && sandboxes !== null && jobs !== null && sandboxes.length === 0;

  return (
    <>
      {empty && <p className="sidehint muted small">Nothing runs here: no deployment, no sandbox yet.</p>}
      {groups.map(({ kind, objects }) => (
        <nav key={kind} className="snav objnav" aria-label={KIND_TITLES[kind]}>
          <div className="snav-label">{KIND_TITLES[kind]} <span className="snav-labelcount">{objects.length + (kind === 'job' ? kept : 0)}</span></div>
          {objects.map((o) => {
            const id = `${o.kind}/${o.name}`;
            const tone = objectTone(o);
            const to = pickObjectPath(route, project, env, o);
            const on = sel === id;
            const meta = sideMeta(o);
            return (
              <a key={id} className={'objitem' + (on ? ' on' : '')} href={to} aria-current={on ? 'page' : undefined} onClick={go(to)}>
                <ToneDot tone={tone} title={objectStateText(o)} />
                <span className="objitem-name">{o.name}</span>
                {meta && <span className={'objitem-meta ' + tone}>{meta}</span>}
              </a>
            );
          })}
          {kind === 'job' && live.map((j) => {
            const to = jobPath(project, env, j.id);
            const on = selJob === j.id;
            const tone = jobTone(j.phase);
            return (
              <a key={j.id} className={'objitem' + (on ? ' on' : '')} href={to} aria-current={on ? 'page' : undefined} onClick={go(to)}
                title={`made by a call · ${j.image}`}>
                <ToneDot tone={tone} title={j.phase} />
                <span className="objitem-name mono">{shortId(j.id)}</span>
                <span className={'objitem-meta ' + tone}>{j.phase}</span>
              </a>
            );
          })}
          {kind === 'job' && kept > 0 && (
            <a className="objitem objitem-more" href={jobPath(project, env)} onClick={go(jobPath(project, env))}
              aria-current={inEnv && route.tab === 'jobs' && !selJob ? 'page' : undefined}>
              <span className="objitem-name">{kept} made by a call</span>
            </a>)}
        </nav>
      ))}
      {sandboxes && sandboxes.length > 0 && (
        <nav className="snav objnav" aria-label="sandboxes">
          <div className="snav-label">sandboxes <span className="snav-labelcount">{sandboxes.length}</span>
            {write && (
              <a className="snav-labeladd" href={newSandboxPath(project, env)} onClick={go(newSandboxPath(project, env))}
                title={`new sandbox in ${env}`} aria-label="new sandbox"><Icon name="plus" size={13} /></a>)}
          </div>
          {side.shown.map((sb) => {
            const to = sandboxPath(project, env, sb.sandboxId, sbView);
            const on = selSandbox === sb.sandboxId;
            const meta = sandboxMeta(sb);
            return (
              <a key={sb.sandboxId} className={'objitem' + (on ? ' on' : '')} href={to} aria-current={on ? 'page' : undefined}
                onClick={go(to)}>
                <StateDot state={sb.clientState} />
                <span className="objitem-name mono">{sb.sandboxId}</span>
                {meta && <span className="objitem-meta failed">{meta}</span>}
              </a>
            );
          })}
          {side.folded > 0 && (
            <button type="button" className="objitem objitem-more" onClick={() => setOpenAll(true)}>
              <StateDot state="hibernated" />
              <span className="objitem-name">{side.folded} hibernated</span>
            </button>)}
        </nav>)}
    </>
  );
}

// ProjectRow is the project at the sidebar's foot while an environment is
// open: one entry for its home, which lists its pages.
function ProjectRow({ project, nav, to, navigate }) {
  const pages = nav.filter((n) => n.group === 'project').map((n) => n.label.toLowerCase());
  return (
    <a className="projectrow" href={to} onClick={(e) => { if (plainClick(e)) { e.preventDefault(); navigate(to); } }}
      title="the project's home and its pages">
      <Icon name="building" size={16} />
      <span className="projectrow-text">
        <span className="projectrow-name">{project}</span>
        <span className="projectrow-pages">Project{pages.length > 0 ? `: ${pages.join(', ')}` : ''}</span>
      </span>
      <Icon name="chevronRight" size={14} />
    </a>
  );
}

// ProjectPages are the project's pages while none of an environment is
// open: its home, the environments, the project's own pages, and one entry
// for its settings.
function ProjectPages({ route, navigate, nav, project }) {
  const { counts } = useSummary();
  const enc = encodeURIComponent;
  const toOf = (n) => (n.view === 'environments' ? envPath(project) : `/projects/${enc(project)}${n.path}`);
  const on = (n) => (n.views || [n.view]).includes(route.view);
  const manage = nav.filter((n) => n.group === 'manage');
  return (
    <nav className="snav projectnav">
      <div className="snav-label">Project <span className="snav-labelcount">{project}</span></div>
      {nav.filter((n) => n.group !== 'manage').map((n) => (
        <NavItem key={n.view} item={n} active={on(n)} count={counts[n.view]} to={toOf(n)} navigate={navigate} />
      ))}
      {manage.length > 0 && (
        <NavItem item={{ ...SETTINGS_NAV, views: manage.map((n) => n.view) }}
          active={manage.some((n) => n.view === route.view)} to={toOf(manage[0])} navigate={navigate} />)}
    </nav>
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

// PROJECT_FILTER is the number of projects from which the switcher offers
// a filter; NEW_PROJECT is where a project is made.
const PROJECT_FILTER = 8;
const NEW_PROJECT = '/projects?new';

// Switcher is where one works, at the head of the topbar's trail: a
// project and one of its environments, in two columns (choosing a project
// on the left lists its environments on the right). The platform's
// administration is the sidebar's last entry, for operators.
function Switcher({ route, navigate, me, platform }) {
  const [open, setOpen, box] = useMenu();
  const [picked, setPicked] = useState('');
  const [others, setOthers] = useState({});
  const [filter, setFilter] = useState('');
  const { environments } = useSummary();
  const operator = isOperator(me);
  const projects = myProjects(me);
  const t = route.project;
  const envName = route.view === 'environments' ? route.env : '';

  const toggle = () => {
    if (!open) {
      setPicked(platform ? '' : t || (projects[0] || {}).project || '');
      setOthers({});
      setFilter('');
    }
    setOpen(!open);
  };
  // Another project's environments are read when it is chosen.
  useEffect(() => {
    if (!open || !picked || picked === t || others[picked] !== undefined) return;
    setOthers((o) => ({ ...o, [picked]: null }));
    api.listEnvironmentsOf(picked).then(
      (r) => setOthers((o) => ({ ...o, [picked]: r.environments || [] })),
      () => setOthers((o) => ({ ...o, [picked]: 'error' })));
  }, [open, picked, t, others]);

  const go = (to) => { setOpen(false); navigate(to); };
  const envs = picked === t ? environments : others[picked];
  const f = filter.trim().toLowerCase();
  const shownProjects = f ? projects.filter((tn) => tn.project.toLowerCase().includes(f)) : projects;
  const pickedAccess = projectAccess(me, picked);
  const enc = encodeURIComponent;

  return (
    <div className="switcher" ref={box}>
      <button className={'switch' + (open ? ' on' : '')} onClick={toggle} aria-expanded={open}>
        <Icon name={platform ? 'server' : 'building'} size={14} />
        <span className="switch-name">{platform ? 'Administration' : t || 'Choose a project'}</span>
        {!platform && t && envName && <><span className="switch-slash">/</span><span className="switch-name">{envName}</span></>}
        <Icon name="chevronDown" size={13} />
      </button>
      {open && (
        <div className="switch-menu" role="menu">
          <div className="switch-col">
            <div className="switch-label">{operator ? 'projects you are a member of' : 'projects'}</div>
            {projects.length > PROJECT_FILTER && (
              <input className="switch-filter" placeholder="filter" value={filter} autoFocus
                aria-label="filter the projects" onChange={(e) => setFilter(e.target.value)} />)}
            <div className="switch-list">
            {shownProjects.map((tn) => (
              <button key={tn.project} role="menuitem" className={'switch-item' + (tn.project === picked ? ' on' : '')}
                onClick={() => setPicked(tn.project)} aria-haspopup="true">
                <span className="mono">{tn.project}</span>
                <Icon name="chevronRight" size={12} />
              </button>
            ))}
            {projects.length === 0 && <div className="switch-empty muted small">none yet</div>}
            </div>
            <div className="switch-foot">
              <button role="menuitem" className="switch-item" onClick={() => go(NEW_PROJECT)}>
                <Icon name="plus" size={13} /> New project
              </button>
            </div>
          </div>
          {picked && (
            <div className="switch-col">
              <div className="switch-label">environments of <span className="mono">{picked}</span></div>
              <div className="switch-list">
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
              </div>
              <div className="switch-foot">
              <button role="menuitem" className="switch-item" onClick={() => go(projectHome(pickedAccess))}>
                <Icon name="home" size={13} /> Project home
              </button>
              <button role="menuitem" className="switch-item" onClick={() => go(envPath(picked))}>
                <Icon name="layers" size={13} /> All environments
              </button>
              {can(pickedAccess).writeProject && (
                <button role="menuitem" className="switch-item" onClick={() => go(`/projects/${enc(picked)}/new-environment`)}>
                  <Icon name="plus" size={13} /> New environment
                </button>)}
              </div>
            </div>)}
        </div>)}
    </div>
  );
}

function NavItem({ item, active, count, to, navigate }) {
  return (
    <a className={'snav-item' + (active ? ' on' : '')} href={to}
      onClick={(e) => { e.preventDefault(); navigate(to); }} aria-current={active ? 'page' : undefined}>
      <Icon name={item.icon} size={16} /> <span>{item.label}</span>
      {count && <span className={'snav-count' + (count.tone ? ' ' + count.tone : '')}>{count.n}</span>}
    </a>
  );
}

// SettingsTabs are the project's settings, one page each: the tabs over
// them (the sidebar's "Project settings").
const SETTINGS_VIEWS = new Set(['members', 'tokens', 'usage', 'settings']);
function SettingsTabs({ route, me, navigate }) {
  const t = encodeURIComponent(route.project);
  const pages = projectNav(projectAccess(me, route.project)).filter((n) => n.group === 'manage');
  return (
    <div className="tabbar settings-tabs">
      {pages.map((n) => (
        <a key={n.view} href={`/projects/${t}${n.path}`} className={'tab' + (route.view === n.view ? ' on' : '')}
          onClick={(e) => { e.preventDefault(); navigate(`/projects/${t}${n.path}`); }}>
          <Icon name={n.icon} size={13} /> {n.label}
        </a>
      ))}
    </div>
  );
}

// The theme switch says where it leads.
const THEME_LABELS = { light: 'Light mode', dark: 'Dark mode', gta: 'Vice mode' };
const THEME_ICONS = { light: 'sun', dark: 'moon', gta: 'star' };

// UserMenu is the signed-in person at the topbar's right end: who, the
// architecture page, the theme and signing out.
function UserMenu({ me, onSignOut }) {
  const [open, setOpen, box] = useMenu();
  const [, toggleTheme, nextTheme] = useTheme();
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
            <Icon name={THEME_ICONS[nextTheme]} size={13} /> {THEME_LABELS[nextTheme]}
          </button>
          <div className="switch-sep" />
          <button role="menuitem" className="switch-item" onClick={onSignOut}><Icon name="logout" size={13} /> Sign out</button>
        </div>)}
    </div>
  );
}

// CRUMB_NAMES name the project's pages in the trail.
const CRUMB_NAMES = {
  tools: 'tools', domains: 'domains', members: 'people', tokens: 'tokens', usage: 'usage', settings: 'general',
  providers: 'access providers',
};

// crumbItem is an environment's item in the trail: "new service" for
// new/service, "HTTP secret s4" for an HTTP secret's page (ADR 0055),
// "new HTTP secret" for its form, else the item.
function crumbItem(item) {
  const s = String(item);
  if (s === 'http/new') return 'new HTTP secret';
  if (s.startsWith('http/')) return `HTTP secret ${s.slice(5)}`;
  return s.replace(/^new\//, 'new ');
}

function Topbar({ route, navigate, me, onSearch, onSignOut }) {
  const t = route.project;
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
        {t && SETTINGS_VIEWS.has(route.view) && <>{sep}<span className="crumb">project settings</span></>}
        {t && page && <>{sep}{crumb(page, `/projects/${enc(t)}/${route.view === 'members' ? 'members' : route.view}`)}</>}
        {route.view === 'domains' && route.sel && <>{sep}<span className="crumb mono">{domainName(route.sel)}</span></>}
        {route.view === 'providers' && route.sel && <>{sep}<span className="crumb mono">{route.sel}</span></>}
        {route.view === 'newProvider' && <>{sep}{crumb('access providers', `/projects/${enc(t)}/providers`)}{sep}<span className="crumb">new</span></>}
        {route.view === 'invite' && <>{sep}<span className="crumb">invitation</span></>}
        {((route.view === 'environments' && !route.env) || route.view === 'newEnvironment') && <>{sep}{crumb('environments', envPath(t))}</>}
        {route.view === 'newEnvironment' && <>{sep}<span className="crumb">new</span></>}
        {route.view === 'environments' && route.env && route.tab !== 'overview' && route.tab !== 'objects' && (
          <>{sep}{crumb((ENV_TAB_LABELS[route.tab] || route.tab).toLowerCase(), envPath(t, route.env, route.tab))}</>)}
        {route.view === 'environments' && route.item && (
          <>{sep}<span className="crumb mono">{crumbItem(route.item)}</span></>)}
        {route.view === 'environments' && route.tab === 'objects' && route.otab && route.otab !== 'overview' && (
          <>{sep}<span className="crumb">{(OBJECT_TAB_LABELS[route.otab] || route.otab).toLowerCase()}
            {route.replica != null ? ` · replica ${route.replica}` : ''}</span></>)}
        {route.view === 'environments' && route.tab === 'sandboxes' && route.item && route.item !== 'new' && route.otab
          && route.otab !== 'overview' && <>{sep}<span className="crumb">{route.otab}</span></>}
        {route.view === 'environments' && route.tab === 'secrets' && route.otab === 'edit' && (
          <>{sep}<span className="crumb">new version</span></>)}
        {route.view === 'projects' && <>{sep}<span className="crumb strong">projects</span></>}
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
