// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// The UI's routes. ferund answers every path that is not a file of the
// build with index.html, so deep links work.
//
//   /, /tenants                         the tenants
//   /tenants/:t                         a tenant's overview
//   /tenants/:t/sandboxes[/:id[/:tab]]  its sandboxes; tab: overview, activity,
//                                       tokens, access, network
//   /tenants/:t/tools                   the tools of its sandboxes
//   /tenants/:t/jobs[/:id]              its jobs (runs to completion)
//   /tenants/:t/domains[/:id]           its domains
//   /tenants/:t/credentials[/:name[/edit]]
//                                       its credentials (ADR 0033)
//   /tenants/:t/new-credential          the form of a new credential
//   /tenants/:t/members                 its people: members and invitations
//   /tenants/:t/tokens                  its tokens
//   /tenants/:t/usage                   its use against its limits
//   /tenants/:t/settings                the tenant itself
//   /tenants/:t/new-environment         the builder of a new environment
//   /tenants/:t/environments[/:env[/:tab[/:item]]]
//                                       its environments; tab:
//                                       overview, builder[/:kind/:name]
//                                       (New deployment, an object's form),
//                                       activity, deployments[/:n],
//                                       incidents[/:id], logs[?search],
//                                       traces[/:trace][?search], traffic,
//                                       settings, secrets, access,
//                                       objects/:kind/:name[/:otab] (an
//                                       object's page; otab: overview,
//                                       logs[/:replica][?search],
//                                       traces[?search], traffic, config)
//   /nodes[/:node[/:tab]]               the nodes; tab: overview, certs, activity
//   /join-tokens                        join tokens
//   /instances                          the control plane's instances
//   /ingresses                          the ingresses
//   /people                             every person (operators)
//   /invite/:token                      an invitation, to accept

function decode(s) {
  try {
    return decodeURIComponent(s);
  } catch {
    return s;
  }
}

// ENV_TABS are the pages of an environment; builder is its editor,
// reached by New deployment; objects holds an object's page, reached
// from the sidebar and the overview.
export const ENV_TABS = [
  'overview', 'builder', 'activity', 'deployments', 'incidents', 'logs', 'traces', 'traffic', 'settings', 'secrets', 'access',
  'objects',
];

// ENV_LENSES are the views of an environment, its tab bar: the sidebar
// picks the thing (the environment or one of its objects), the tab bar
// how to look at it. Activity holds the deployments and the incidents,
// Settings the secrets and the domains and access (LENS_PAGES).
export const ENV_LENSES = ['overview', 'activity', 'logs', 'traces', 'traffic', 'settings'];

// LENS_PAGES are the pages of a view that has more than one, in its own
// tab bar.
export const LENS_PAGES = {
  activity: ['activity', 'deployments', 'incidents'],
  settings: ['settings', 'secrets', 'access'],
};

// lensOf is the view a tab of an environment belongs to; an object's
// page and the builder belong to none ('').
export function lensOf(tab) {
  if (ENV_LENSES.includes(tab)) return tab;
  for (const [lens, pages] of Object.entries(LENS_PAGES)) if (pages.includes(tab)) return lens;
  return '';
}

// OBJECT_TABS are the views of an object's page: the same as an
// environment's where it has them (OBJECT_LENSES), with its definition
// as config. A service has every one, an addon and a job no traces and
// no traffic.
export const OBJECT_TABS = ['overview', 'logs', 'traces', 'traffic', 'config'];

// objectTabs are the views of an object of kind.
export function objectTabs(kind) {
  return kind === 'service' ? OBJECT_TABS : OBJECT_TABS.filter((t) => t !== 'traces' && t !== 'traffic');
}

// OBJECT_LENS maps an environment's view to an object's, and back: the
// view stays when one picks another thing in the sidebar.
const TO_OBJECT = { overview: 'overview', activity: 'overview', logs: 'logs', traces: 'traces', traffic: 'traffic', settings: 'config' };
const TO_ENV = { overview: 'overview', logs: 'logs', traces: 'traces', traffic: 'traffic', config: 'settings' };

// ENV_TAB_LABELS name them.
export const ENV_TAB_LABELS = {
  overview: 'Overview',
  builder: 'New deployment',
  activity: 'Activity',
  deployments: 'Deployments',
  incidents: 'Incidents',
  logs: 'Logs',
  traces: 'Traces',
  traffic: 'Traffic',
  secrets: 'Secrets & credentials',
  access: 'Domains & access',
  settings: 'Settings',
  objects: 'Objects',
};

// PAGE_LABELS name the pages of a view in its own tab bar (LENS_PAGES).
export const PAGE_LABELS = {
  activity: 'Everything', deployments: 'Deployments', incidents: 'Incidents',
  settings: 'General', secrets: 'Secrets & credentials', access: 'Domains & access',
};

// OBJECT_TAB_LABELS name the views of an object's page.
export const OBJECT_TAB_LABELS = { overview: 'Overview', logs: 'Logs', traces: 'Traces', traffic: 'Traffic', config: 'Config' };

// SEARCH_TABS are the tabs whose search is in the address's query; an
// object's logs and traces have theirs too.
const SEARCH_TABS = new Set(['logs', 'traces']);

// envRoute reads the segments after /tenants/:t/environments: the
// environment, its tab (an unknown one is the overview), and the tab's
// item: a deployment's number, an incident's ID, a trace's ID, or the
// "<kind>/<name>" of the object whose logs show, with the replica (null
// unless given: every replica); and the logs' or traces' search, the
// address's query (logs.js, traces.js); on the objects tab the
// "<kind>/<name>" of the object whose page it is, on the builder's the
// object whose form is open.
function envRoute(tenant, seg, search) {
  const env = seg[0] || '';
  const tab = env && ENV_TABS.includes(seg[1]) ? seg[1] : 'overview';
  let item = '';
  let replica = null;
  if (tab === 'deployments' || tab === 'incidents' || tab === 'traces') item = seg[2] || '';
  if (tab === 'builder' && seg[2] && seg[3]) item = `${seg[2]}/${seg[3]}`;
  if (tab === 'objects' && seg[2] && seg[3]) {
    item = `${seg[2]}/${seg[3]}`;
    const otab = OBJECT_TABS.includes(seg[4]) ? seg[4] : 'overview';
    if (otab === 'logs' && /^\d+$/.test(seg[5] || '')) replica = Number(seg[5]);
    return { view: 'environments', tenant, env, tab, item, otab, replica, search: SEARCH_TABS.has(otab) ? search : '' };
  }
  return { view: 'environments', tenant, env, tab, item, replica, search: SEARCH_TABS.has(tab) ? search : '' };
}

// envPath is the path of an environment's page: the list without env,
// the tab's list without item; the search ("?…") only on a logs or
// traces path. The logs of an object (item "<kind>/<name>") are its
// page's Logs, with the replica when it is given.
export function envPath(tenant, env = '', tab = 'overview', item = '', replica = null, search = '') {
  const enc = encodeURIComponent;
  let p = `/tenants/${enc(tenant)}/environments`;
  if (!env) return p;
  if (tab === 'logs' && item) return objectPath(tenant, env, item, 'logs', replica, search);
  p += `/${enc(env)}`;
  if (!tab || tab === 'overview' || !ENV_TABS.includes(tab)) return p;
  p += `/${tab}`;
  if (item) p += '/' + String(item).split('/').map(enc).join('/');
  if (SEARCH_TABS.has(tab) && search) p += search;
  return p;
}

// objectPath is the path of an object's page (item "<kind>/<name>") in
// view otab: the replica only on its logs, the search only on its logs
// and traces.
export function objectPath(tenant, env, item, otab = 'overview', replica = null, search = '') {
  let p = envPath(tenant, env, 'objects', item);
  if (!otab || otab === 'overview' || !OBJECT_TABS.includes(otab)) return p;
  p += `/${otab}`;
  if (otab === 'logs' && replica != null && replica !== '') p += `/${Number(replica)}`;
  if (SEARCH_TABS.has(otab) && search) p += search;
  return p;
}

// pickObjectPath is where picking object o (kind, name) in the sidebar
// leads: its page in the view open now, where it has that view, else its
// overview.
export function pickObjectPath(route, tenant, env, o) {
  const now = route.view === 'environments' && route.env
    ? (route.tab === 'objects' ? route.otab : TO_OBJECT[lensOf(route.tab)])
    : '';
  const otab = now && objectTabs(o.kind).includes(now) ? now : 'overview';
  return objectPath(tenant, env, `${o.kind}/${o.name}`, otab);
}

// pickEnvPath is where picking environment env in the sidebar leads:
// from one of its objects' pages the environment in the same view, else
// its overview.
export function pickEnvPath(route, tenant, env) {
  const inEnv = route.view === 'environments' && route.env && route.tenant === tenant;
  const tab = inEnv && route.tab === 'objects' ? TO_ENV[route.otab] || 'overview' : 'overview';
  return envPath(tenant, env, tab);
}

// sandboxPath and jobPath are a tenant's sandboxes and jobs, or one of
// them; an environment's page links them.
export function sandboxPath(tenant, id = '') {
  return `/tenants/${encodeURIComponent(tenant)}/sandboxes${id ? '/' + encodeURIComponent(id) : ''}`;
}

export function jobPath(tenant, id = '') {
  return `/tenants/${encodeURIComponent(tenant)}/jobs${id ? '/' + encodeURIComponent(id) : ''}`;
}

// EARLIER_TABS maps an environment tab's earlier or other name to its
// name: the deployments were the releases (ADR 0018, "The deployments'
// word"); the builder is what Edit opens.
const EARLIER_TABS = { releases: 'deployments', edit: 'builder' };

// canonicalPath is pathname with an environment tab's earlier name
// replaced by its name, and an object's logs under the environment's
// Logs (logs/:kind/:name[/:replica]) moved to its page's, so that an old
// link redirects; else pathname.
export function canonicalPath(pathname) {
  const seg = String(pathname || '/').split('/');
  if (seg[1] === 'tenants' && seg[3] === 'environments' && seg[4] && EARLIER_TABS[seg[5]]) {
    seg[5] = EARLIER_TABS[seg[5]];
    return seg.join('/');
  }
  if (seg[1] === 'tenants' && seg[3] === 'environments' && seg[4] && seg[5] === 'logs' && seg[6] && seg[7]) {
    const rest = /^\d+$/.test(seg[8] || '') ? `/${seg[8]}` : '';
    return [...seg.slice(0, 5), 'objects', seg[6], seg[7], 'logs'].join('/') + rest;
  }
  return pathname;
}

// parsePath maps a path to {view, tenant?, sel?, tab?, node?}, and for
// the environments to {view, tenant, env, tab, item, replica}.
export function parsePath(pathname) {
  const [path, query] = String(pathname || '/').split('?');
  const search = query ? '?' + query : '';
  const seg = String(canonicalPath(path) || '/').split('/').filter(Boolean).map(decode);
  switch (seg[0]) {
    case 'nodes':
      return seg[1] ? { view: 'node', node: seg[1], tab: seg[2] || 'overview' } : { view: 'nodes' };
    case 'join-tokens':
      return { view: 'joinTokens' };
    case 'instances':
      return { view: 'instances' };
    case 'ingresses':
      return { view: 'ingresses' };
    case 'people':
      return { view: 'people' };
    case 'invite':
      return { view: 'invite', token: seg[1] || '' };
    case 'tenants':
      break;
    default:
      return { view: 'tenants' };
  }
  if (seg.length === 1) return { view: 'tenants' };
  const tenant = seg[1];
  if (seg[2] === 'tools') return { view: 'tools', tenant };
  if (seg[2] === 'members') return { view: 'members', tenant };
  if (seg[2] === 'tokens') return { view: 'tokens', tenant };
  if (seg[2] === 'jobs') return { view: 'jobs', tenant, sel: seg[3] || '' };
  if (seg[2] === 'domains') return { view: 'domains', tenant, sel: seg[3] || '' };
  if (seg[2] === 'credentials') {
    return { view: 'credentials', tenant, sel: seg[3] || '', tab: seg[3] && seg[4] === 'edit' ? 'edit' : '' };
  }
  if (seg[2] === 'new-credential') return { view: 'newCredential', tenant };
  if (seg[2] === 'usage') return { view: 'usage', tenant };
  if (seg[2] === 'settings') return { view: 'settings', tenant };
  if (seg[2] === 'new-environment') return { view: 'newEnvironment', tenant };
  if (seg[2] === 'environments') return envRoute(tenant, seg.slice(3), search);
  if (seg[2] === 'sandboxes') return { view: 'sandboxes', tenant, sel: seg[3] || '', tab: seg[4] || 'overview' };
  return { view: 'overview', tenant };
}

// TENANT_VIEWS are the views that act in a tenant, with the session.
export const TENANT_VIEWS = new Set([
  'overview',
  'sandboxes',
  'tools',
  'environments',
  'newEnvironment',
  'members',
  'tokens',
]);

// newEnvPath is the builder of a new environment.
export function newEnvPath(tenant) {
  return `/tenants/${encodeURIComponent(tenant)}/new-environment`;
}

// chosenEnv is the environment the sidebar's pages are of: the route's,
// else the one last open in this tenant (last), else the tenant's first;
// null when the tenant has none.
export function chosenEnv(route, envs, last = '') {
  const list = envs || [];
  const by = (n) => (n ? list.find((e) => e.name === n || e.id === n) : undefined);
  if (route.view === 'environments' && route.env) return by(route.env) || { id: route.env, name: route.env };
  return by(last) || list[0] || null;
}

// switchEnvPath is where choosing env of tenant leads: the same page of
// the other environment while one is open (a deployment of one is not in
// the other, so its list), from an object's page the environment in the
// same view (another environment may not have the object), else its
// overview.
export function switchEnvPath(route, tenant, env) {
  const inEnv = route.view === 'environments' && route.env && route.tenant === tenant;
  if (inEnv && route.tab === 'objects') return pickEnvPath(route, tenant, env);
  const tab = inEnv ? route.tab : 'overview';
  return envPath(tenant, env, tab);
}
