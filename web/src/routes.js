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
//   /tenants/:t/environments[/:env[/:tab[/:item]]]
//                                       its environments, read-only; tab:
//                                       overview, deployments[/:n],
//                                       incidents[/:id],
//                                       logs[/:kind/:name[/:replica]][?search],
//                                       traces[/:trace][?search]
//   /nodes[/:node[/:tab]]               the nodes; tab: overview, certs, activity
//   /join-tokens                        join tokens
//   /instances                          the control plane's instances

function decode(s) {
  try {
    return decodeURIComponent(s);
  } catch {
    return s;
  }
}

// ENV_TABS are the tabs of an environment.
export const ENV_TABS = ['overview', 'deployments', 'incidents', 'logs', 'traces'];

// SEARCH_TABS are the tabs whose search is in the address's query.
const SEARCH_TABS = new Set(['logs', 'traces']);

// envRoute reads the segments after /tenants/:t/environments: the
// environment, its tab (an unknown one is the overview), and the tab's
// item: a deployment's number, an incident's ID, a trace's ID, or the
// "<kind>/<name>" of the object whose logs show, with the replica (null
// unless given: every replica); and the logs' or traces' search, the
// address's query (logs.js, traces.js).
function envRoute(tenant, seg, search) {
  const env = seg[0] || '';
  const tab = env && ENV_TABS.includes(seg[1]) ? seg[1] : 'overview';
  let item = '';
  let replica = null;
  if (tab === 'deployments' || tab === 'incidents' || tab === 'traces') item = seg[2] || '';
  if (tab === 'logs' && seg[2] && seg[3]) {
    item = `${seg[2]}/${seg[3]}`;
    if (/^\d+$/.test(seg[4] || '')) replica = Number(seg[4]);
  }
  return { view: 'environments', tenant, env, tab, item, replica, search: SEARCH_TABS.has(tab) ? search : '' };
}

// envPath is the path of an environment's page: the list without env,
// the tab's list without item; the replica only on a logs path and only
// when it is given, the search ("?…") only on a logs or traces path.
export function envPath(tenant, env = '', tab = 'overview', item = '', replica = null, search = '') {
  const enc = encodeURIComponent;
  let p = `/tenants/${enc(tenant)}/environments`;
  if (!env) return p;
  p += `/${enc(env)}`;
  if (!tab || tab === 'overview' || !ENV_TABS.includes(tab)) return p;
  p += `/${tab}`;
  if (item) p += '/' + String(item).split('/').map(enc).join('/');
  if (tab === 'logs' && item && replica != null && replica !== '') p += `/${Number(replica)}`;
  if (SEARCH_TABS.has(tab) && search) p += search;
  return p;
}

// EARLIER_TABS maps an environment tab's earlier name to its name: the
// deployments were the releases (ADR 0018, "The deployments' word").
const EARLIER_TABS = { releases: 'deployments' };

// canonicalPath is pathname with an environment tab's earlier name
// replaced by its name, so that an old link redirects; else pathname.
export function canonicalPath(pathname) {
  const seg = String(pathname || '/').split('/');
  if (seg[1] === 'tenants' && seg[3] === 'environments' && seg[4] && EARLIER_TABS[seg[5]]) {
    seg[5] = EARLIER_TABS[seg[5]];
    return seg.join('/');
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
    case 'tenants':
      break;
    default:
      return { view: 'tenants' };
  }
  if (seg.length === 1) return { view: 'tenants' };
  const tenant = seg[1];
  if (seg[2] === 'tools') return { view: 'tools', tenant };
  if (seg[2] === 'environments') return envRoute(tenant, seg.slice(3), search);
  if (seg[2] === 'sandboxes') return { view: 'sandboxes', tenant, sel: seg[3] || '', tab: seg[4] || 'overview' };
  return { view: 'overview', tenant };
}

// TENANT_VIEWS are the views that act as a tenant: they need its token.
export const TENANT_VIEWS = new Set(['overview', 'sandboxes', 'tools', 'environments']);
