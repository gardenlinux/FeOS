// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// The derivations of the sandbox pages: the state in words with what it
// costs, the counts by state, the list's filter, the tabs, the create
// form's request and the snippets that connect an agent. Pure, with no
// browser or API imports, so that the tests load them as they are.

// fmtMem renders a sandbox's memory: whole GiB as "4 GiB", the rest in
// MiB ("1536 MiB").
export function fmtMem(mib) {
  const m = Number(mib || 0);
  if (!m) return '';
  return m % 1024 === 0 ? `${m / 1024} GiB` : `${m} MiB`;
}

// sizeWords renders a sandbox's size, "2 vCPU, 4 GiB"; '' without one.
export function sizeWords(sb) {
  const v = Number((sb && sb.vcpus) || 0);
  const mem = fmtMem(sb && sb.memoryMib);
  if (!v && !mem) return '';
  return [v ? `${v} vCPU` : '', mem].filter(Boolean).join(', ');
}

// STATE_BUCKETS are the list's counts, in order: each a set of
// clientStates, the dot's state, and its words.
export const STATE_BUCKETS = [
  { key: 'awake', label: 'awake', dot: 'running', states: ['running'] },
  { key: 'asleep', label: 'asleep', dot: 'paused', states: ['paused'] },
  { key: 'hibernated', label: 'hibernated', dot: 'hibernated', states: ['hibernated'] },
  { key: 'moving', label: 'on its way', dot: 'resuming', states: ['pending', 'creating', 'pausing', 'resuming', 'hibernating'] },
  { key: 'failed', label: 'failed', dot: 'failed', states: ['failed', 'lost'] },
  { key: 'other', label: 'stopped or unknown', dot: 'unknown', states: ['stopped', 'unknown'] },
];

// bucketOf is a clientState's bucket key; anything unnamed is other.
export function bucketOf(state) {
  const b = STATE_BUCKETS.find((x) => x.states.includes(state));
  return b ? b.key : 'other';
}

// stateCounts counts the sandboxes per bucket.
export function stateCounts(sandboxes) {
  const c = Object.fromEntries(STATE_BUCKETS.map((b) => [b.key, 0]));
  for (const s of sandboxes || []) c[bucketOf(s.clientState)]++;
  return c;
}

// stateWords says where a sandbox is and what it costs, in the concept's
// sentences (§6): {short, long}. short is the list's; long the head's,
// with the node and the booking, beside the API's word (so a failure's
// long sentence says what now, not the word again).
export function stateWords(sb) {
  const st = (sb && sb.clientState) || 'unknown';
  const node = (sb && sb.node) || '';
  const on = node ? ` on ${node}` : '';
  const size = sizeWords(sb);
  const desc = (sb && sb.statusDescription) || '';
  switch (st) {
    case 'running':
      return { short: 'awake', long: `awake${size ? ` · ${size} booked${on}` : on}` };
    case 'paused':
      return { short: 'asleep on its node', long: `asleep${on} · costs no memory · wakes on the next call` };
    case 'hibernated':
      return { short: 'in the object store', long: 'in the object store · nothing on a node · restores on any node' };
    case 'pending':
      return { short: 'not created yet', long: 'no pod yet · the next call creates it' };
    case 'creating':
      return { short: 'starting', long: `starting${on}` };
    case 'pausing':
      return { short: 'falling asleep', long: `falling asleep${on}` };
    case 'resuming':
      return { short: 'waking', long: `waking${on}` };
    case 'hibernating':
      return { short: 'hibernating', long: `going to the object store${node ? ` from ${node}` : ''}` };
    case 'stopped':
      return { short: 'stopped', long: 'the next call starts it again' };
    case 'failed':
      return { short: 'failed', long: 'the next call tries to start it again, or delete it' };
    case 'lost':
      return { short: 'lost', long: 'no checkpoint · delete it or create anew' };
    case 'unknown':
      return { short: 'unknown', long: `not known now${node ? ` · node ${node} not reachable` : ''} · a sync resolves it` };
    default:
      return { short: st, long: desc || st };
  }
}

// canPause and canResume: what the head's main action is.
export function canPause(sb) {
  return !!sb && sb.clientState === 'running';
}
export function canResume(sb) {
  return !!sb && (sb.clientState === 'paused' || sb.clientState === 'hibernated');
}

// filterSandboxes keeps the sandboxes whose ID, owner, image, node or
// state (the API's word or the list's) has q in it.
export function filterSandboxes(sandboxes, q) {
  const n = (q || '').trim().toLowerCase();
  const list = sandboxes || [];
  if (!n) return list;
  return list.filter((s) => [s.sandboxId, s.owner, s.image, s.node, s.clientState, stateWords(s).short]
    .some((v) => (v || '').toLowerCase().includes(n)));
}

// ownerShown is the owner a sandbox page names beside the ID: ferund's
// owner is the tenant, which says nothing on the tenant's own page, so
// only an owner other than the tenant shows.
export function ownerShown(sb, tenant) {
  const o = (sb && sb.owner) || '';
  return o && o !== tenant ? o : '';
}

// SANDBOX_TABS are a sandbox's tabs; operator: an operator's only (they
// read admin routes).
export const SANDBOX_TABS = [
  { key: 'overview', label: 'Overview', icon: 'gauge' },
  { key: 'connect', label: 'Connect', icon: 'plug' },
  { key: 'credentials', label: 'Credentials', icon: 'lock' },
  { key: 'network', label: 'Network', icon: 'globe', operator: true },
  { key: 'activity', label: 'Activity', icon: 'activity', operator: true },
  { key: 'settings', label: 'Settings', icon: 'settings' },
];

// EARLIER_TABS map a tab's earlier name to its name.
const EARLIER_TABS = { tokens: 'connect', access: 'credentials' };

// sandboxTabs are the tabs can offers.
export function sandboxTabs(can) {
  return SANDBOX_TABS.filter((t) => !t.operator || (can && can.operator));
}

// activeTab is the tab a route's tab names among those can offers: an
// earlier name is its new one, anything else the overview.
export function activeTab(tab, can) {
  const t = EARLIER_TABS[tab] || tab;
  return sandboxTabs(can).some((x) => x.key === t) ? t : 'overview';
}

// createRequest turns the create form's fields into createSandbox's
// arguments: [image, vcpus, memoryMib, {attachable, imageCredentials,
// credentials?}]; credentials are the tenant's its pod gets (ADR 0033).
// An empty image or size is the platform's default (0); the registry
// login takes a username and a password, or neither. It throws on a bad
// value.
export function createRequest(form) {
  const num = (label, s) => {
    const v = String(s || '').trim();
    if (!v) return 0;
    if (!/^\d+$/.test(v) || Number(v) <= 0) throw new Error(`${label} must be a whole number above 0, or empty for the default`);
    return Number(v);
  };
  const image = String(form.image || '').trim();
  const vcpus = num('vCPUs', form.vcpus);
  const memoryMib = num('memory', form.memoryMib);
  const creds = credentialsOf(form.username, form.password, true);
  const opts = { attachable: !!form.attachable, imageCredentials: creds };
  if (form.credentials && form.credentials.length) opts.credentials = [...form.credentials].sort();
  return [image, vcpus, memoryMib, opts];
}

// credentialsOf is {username, password}, null when both are empty
// (optional) and an error when only one is given.
export function credentialsOf(username, password, optional = false) {
  const u = String(username || '').trim();
  const p = String(password || '');
  if (!u && !p) {
    if (optional) return null;
    throw new Error('the registry login needs a username and a password');
  }
  if (!u || !p) throw new Error('the registry login needs a username and a password, or neither');
  return { username: u, password: p };
}

// TOKEN_VAR is the shell variable the snippets read the token from when
// they show no token of their own.
export const TOKEN_VAR = '$FERUN_TOKEN';

// connectSnippets are the ready snippets to connect an agent to sandbox
// id: Claude Code's command, an MCP client's JSON, and REST exec with
// curl. With token they carry it; without, the shell snippets read
// $FERUN_TOKEN and the JSON says <token>.
export function connectSnippets({ id, mcp, origin, token }) {
  const shellTok = token || TOKEN_VAR;
  const name = `ferun-${id}`;
  const claude = `claude mcp add --transport http ${name} ${mcp} --header "Authorization: Bearer ${shellTok}"`;
  const mcpJson = JSON.stringify({ mcpServers: { [name]: { type: 'http', url: mcp, headers: { Authorization: `Bearer ${token || '<token>'}` } } } }, null, 2);
  const curl = [
    `curl -sS -X POST ${origin}/api/v1/sandboxes/${id}/exec \\`,
    `  -H "Authorization: Bearer ${shellTok}" \\`,
    '  -H "Content-Type: application/json" \\',
    '  -d \'{"command": "uname -a", "timeout": 60}\'',
  ].join('\n');
  return { name, claude, mcpJson, curl };
}

// toolSummary is a tool's input schema as a line of its parameters,
// "command*, timeout" (* required).
export function toolSummary(schema) {
  const props = (schema && schema.properties) || {};
  const req = new Set((schema && schema.required) || []);
  return Object.keys(props).map((k) => (req.has(k) ? `${k}*` : k)).join(', ');
}
