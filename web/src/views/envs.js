// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// The environments view's arithmetic: pure, with no React or browser
// imports, so that the tests load it as it is. The shapes are ferund's
// (go/internal/env/wire.go): an environment's status with its objects
// and their instances, deployments, diffs and incidents.

// Tones are the classes of the state dots and labels (styles.css):
// running green, paused amber, failed red, absent grey.

// deploymentTone is the tone of a deployment's state, or of an environment's
// in the list (the latest deployment's state, "none" or "deleting").
export function deploymentTone(state) {
  switch (state) {
    case 'deployed': return 'running';
    case 'waiting': case 'deploying': case 'deleting': return 'moving';
    case 'failed': return 'failed';
    default: return 'absent';
  }
}

// phaseTone is the tone of an instance's or an object's phase, and of a
// job run's (running, succeeded, failed).
export function phaseTone(phase) {
  switch (phase) {
    case 'ready': case 'succeeded': return 'running';
    case 'starting': case 'unready': case 'running': return 'moving';
    case 'exited': case 'failed': return 'failed';
    default: return 'absent';
  }
}

// isAttached tells an attached service (a process in a sandbox) from an
// image service.
export function isAttached(o) {
  return o.kind === 'service' && o.runtime === 'attached';
}

// objectTone sums an object up: an image service is red while one of its
// instances exited or failed, green once as many are ready as desired;
// an addon and an attached service have a phase, a job its last run.
export function objectTone(o) {
  if (o.kind === 'job') return o.lastRun ? phaseTone(o.lastRun.phase) : 'absent';
  if (o.kind === 'service' && !isAttached(o)) {
    if ((o.instances || []).some((i) => i.phase === 'exited' || i.phase === 'failed')) return 'failed';
    if (!o.desired) return 'absent';
    return (o.ready || 0) >= o.desired ? 'running' : 'moving';
  }
  if (o.kind === 'addon' && o.phase === 'ready' && !o.provisioned) return 'moving';
  return phaseTone(o.phase);
}

// exitText renders an exit code; null is an exit the node reported
// without one.
export function exitText(code) {
  if (code === null || code === undefined) return 'no status';
  return String(code);
}

// idleText names an image service's instances that are ready but paused
// or hibernated for want of traffic: ferund counts only running ones as
// ready, and a request through a route wakes them.
function idleText(o) {
  const idle = (o.instances || []).filter((i) => i.phase === 'ready' && (i.state === 'paused' || i.state === 'hibernated')).length;
  return idle ? ` · ${idle} idle, woken by a request` : '';
}

// objectSummary is the line under an object's name.
export function objectSummary(o) {
  switch (o.kind) {
    case 'service':
      if (isAttached(o)) return `attached · ${o.phase || 'starting'}`;
      return `image · ${o.ready || 0} / ${o.desired || 0} ready${idleText(o)}`;
    case 'addon':
      return `${o.type} ${o.version} · plan ${o.plan} · ${o.provisioned ? 'provisioned' : 'not provisioned'}`;
    case 'job': {
      const r = o.lastRun;
      if (!r) return 'not run yet';
      let s = `deployment ${r.deployment}, attempt ${r.attempt}: ${r.phase}`;
      if (r.exitCode !== null && r.exitCode !== undefined) s += ` (exit ${r.exitCode})`;
      return s;
    }
    default:
      return '';
  }
}

export const KINDS = ['service', 'addon', 'job'];

// KIND_TITLES are the sections of the objects.
export const KIND_TITLES = { service: 'services', addon: 'addons', job: 'jobs' };

// groupObjects sorts an environment's objects into their kinds, in the
// status's order; a kind ferund adds later goes last, under its own name.
export function groupObjects(objects) {
  const out = new Map(KINDS.map((k) => [k, []]));
  for (const o of objects || []) {
    if (!out.has(o.kind)) out.set(o.kind, []);
    out.get(o.kind).push(o);
  }
  return [...out].filter(([, list]) => list.length > 0).map(([kind, list]) => ({ kind, objects: list }));
}

// logReplicas are the replicas whose logs one may ask for: an image
// service's (the desired ones and any an instance still has), replica 0
// of anything else.
export function logReplicas(o) {
  if (!o || o.kind !== 'service' || isAttached(o)) return [0];
  const set = new Set();
  for (let r = 0; r < (o.desired || 0); r++) set.add(r);
  for (const i of o.instances || []) set.add(i.replica);
  if (set.size === 0) set.add(0);
  return [...set].sort((a, b) => a - b);
}

// findObject finds "<kind>/<name>" among the status's objects.
export function findObject(objects, item) {
  const i = String(item || '').indexOf('/');
  if (i < 0) return null;
  const kind = item.slice(0, i);
  const name = item.slice(i + 1);
  return (objects || []).find((o) => o.kind === kind && o.name === name) || null;
}

// logSourceText says where an object's log comes from.
export function logSourceText(source, o, replica) {
  if (source === 'process') return `the process of the attached service in sandbox ${o && o.sandbox ? o.sandbox : '—'}`;
  if (o && o.kind === 'job') return 'the last run of the job';
  if (o && o.kind === 'addon') return 'the container of the addon';
  return `the container of replica ${replica}`;
}

// logsProblem turns a failed logs request into what the logs pane shows.
// The node that serves no container logs (501) and a paused instance
// (409) are not faults: their texts show as messages, with what to do.
export function logsProblem(e) {
  const text = (e && e.message) || String(e);
  if (e && e.status === 501) {
    return { tone: 'notice', text, hint: "The instance's node runs a feosd without container logs; update the node." };
  }
  if (e && e.status === 409) {
    return { tone: 'notice', text, hint: 'ferund never wakes a paused instance for its logs: they show once it runs again.' };
  }
  return { tone: 'error', text };
}

// parseLabelFilter reads label filters typed as "key=value", several
// separated by spaces or commas; what lacks a key and an "=" comes back
// as bad.
export function parseLabelFilter(text) {
  const labels = [];
  const bad = [];
  for (const tok of String(text || '').split(/[\s,]+/).filter(Boolean)) {
    if (tok.indexOf('=') <= 0) bad.push(tok);
    else if (!labels.includes(tok)) labels.push(tok);
  }
  return { labels, bad };
}

// labelPairs lists a label map as sorted [key, value] pairs.
export function labelPairs(labels) {
  return Object.entries(labels || {}).sort(([a], [b]) => a.localeCompare(b));
}

// originText says where a deployment came from: an apply, a revert to an
// earlier deployment, a clone of another environment's deployment, or a change
// of one object.
export function originText(r) {
  switch (r.origin) {
    case 'revert': return r.originRef ? `revert to ${r.originRef}` : 'revert';
    case 'clone': return r.originRef ? `clone of ${r.originRef}` : 'clone';
    case 'object': return r.originRef ? `object ${r.originRef}` : 'object';
    default: return r.origin || '—';
  }
}

// changeTone is the tone of a diff entry's change.
export function changeTone(change) {
  switch (change) {
    case 'add': return 'ok';
    case 'remove': return 'bad';
    default: return 'acc';
  }
}

// heavyEffect tells the effects that cost data or downtime.
export function heavyEffect(effect) {
  return effect === 'deletes data' || effect === 'recreates (downtime)';
}

// fmtValue renders a diff's value: null is absent, a string itself,
// anything else as compact JSON.
export function fmtValue(v) {
  if (v === null || v === undefined) return '—';
  if (typeof v === 'string') return v;
  return JSON.stringify(v);
}

// CONDITIONS name the incidents' conditions.
export const CONDITIONS = {
  rollout_failed: 'rollout failed',
  crash_loop: 'crash loop',
  job_failed: 'job failed',
  no_capacity: 'no capacity',
  addon_unhealthy: 'addon unhealthy',
  volume_unavailable: 'volume unavailable',
  attach_failed: 'attach failed',
};

export function conditionText(c) {
  return CONDITIONS[c] || c || '—';
}

// incidentObject splits an incident's "<kind>/<name>"; logs are read for
// the kinds that have them.
export function incidentObject(object) {
  const s = String(object || '');
  const i = s.indexOf('/');
  if (i < 0) return { kind: '', name: s, hasLogs: false };
  const kind = s.slice(0, i);
  return { kind, name: s.slice(i + 1), hasLogs: KINDS.includes(kind) };
}

// hasEvidence tells whether an incident's evidence holds anything.
export function hasEvidence(ev) {
  if (!ev) return false;
  return (ev.exits || []).length > 0 || (ev.logTail || []).length > 0 || (ev.diff || []).length > 0 || !!ev.placement
    || (ev.pending || []).length > 0;
}

// envCounts counts the list's environments by the state of their latest
// deployment, and their open incidents.
export function envCounts(envs) {
  const c = { deployed: 0, deploying: 0, failed: 0, incidents: 0 };
  for (const e of envs || []) {
    if (e.state === 'deployed') c.deployed++;
    else if (e.state === 'waiting' || e.state === 'deploying') c.deploying++;
    else if (e.state === 'failed') c.failed++;
    c.incidents += e.openIncidents || 0;
  }
  return c;
}

// statusState is an environment's state as the list names it, from its
// status: deleting, none without a deployment, else the latest deployment's.
export function statusState(st) {
  if (!st) return '';
  if (st.deleting) return 'deleting';
  return st.rollout ? st.rollout.state : 'none';
}

// rolloutNote says what serves while the latest deployment is not deployed:
// the deployed deployment keeps serving a failed one and serves during a
// rollout.
export function rolloutNote(st) {
  const r = st && st.rollout;
  if (!r) return '';
  if (r.state === 'deployed') return 'every object matches it';
  if (st.deployed > 0 && st.deployed !== r.deployment) {
    return r.state === 'failed' ? `deployment ${st.deployed} keeps serving` : `deployment ${st.deployed} serves meanwhile`;
  }
  if (r.state === 'failed') return 'no deployment was deployed before it';
  return '';
}

// ------------------------------------------------------------ operations --

// revertOffer is the deployment the page head offers to revert to: the
// deployed one, while the latest is another and failed or is still
// waiting or deploying; else 0.
export function revertOffer(st) {
  const r = st && st.rollout;
  if (!r || st.deleting || !(st.deployed > 0) || st.deployed === st.deployment) return 0;
  return ['failed', 'waiting', 'deploying'].includes(r.state) ? st.deployed : 0;
}

// mayRevertTo tells the deployments one can revert to: any but the
// latest, whose revert would change nothing.
export function mayRevertTo(n, latest) {
  return Number(n) > 0 && Number(n) !== Number(latest);
}

// tagPlan says what putting tag name on deployment n does: nothing for
// an empty name, a new tag, or moving one from the deployment it names
// ('here' when it names n already).
export function tagPlan(tags, name, n) {
  const t = String(name || '').trim();
  if (!t) return { kind: 'none', tag: '' };
  const cur = (tags || []).find((x) => x.tag === t);
  if (!cur) return { kind: 'new', tag: t };
  if (Number(cur.deployment) === Number(n)) return { kind: 'here', tag: t };
  return { kind: 'move', tag: t, from: cur.deployment };
}

// specBindings lists a spec's HTTP bindings (ADR 0028) with the objects
// that name them.
export function specBindings(spec) {
  const s = spec || {};
  const users = (name) => ['services', 'jobs'].flatMap((key) => Object.entries(s[key] || {})
    .filter(([, o]) => Array.isArray(o && o.bindings) && o.bindings.includes(name))
    .map(([n]) => `${key === 'services' ? 'service' : 'job'}/${n}`));
  return Object.entries(s.bindings || {}).sort(([a], [b]) => a.localeCompare(b)).map(([name, b]) => ({
    name,
    upstream: (b && b.upstream) || '',
    mode: (b && b.mode) || 'name',
    credential: (b && b.credential && b.credential.kind) || '',
    users: users(name),
    // Where the credential comes from: a credential of the tenant at the
    // version the deployment pinned (ADR 0033), or the environment's
    // secrets its fields name.
    tenantCredential: (b && b.tenantCredential) || null,
    secrets: credentialSecrets(b && b.credential),
  }));
}

// credentialSecrets are the environment's secrets a binding's credential
// names, sorted.
function credentialSecrets(c) {
  if (!c) return [];
  const vals = [...Object.values(c.headers || {}),
    ...['user', 'password', 'tokenUrl', 'clientId', 'clientSecret', 'scope', 'audience', 'certPem', 'keyPem'].map((k) => c[k])];
  return [...new Set(vals.filter((v) => v && typeof v === 'object' && v.secret).map((v) => v.secret))].sort();
}

// jobTrigger is when a job of the spec runs: on each deploy that changes
// it (the default), or only when asked.
export function jobTrigger(spec, name) {
  const j = spec && spec.jobs && spec.jobs[name];
  return j && j.on === 'manual' ? 'manual' : 'deploy';
}

// actionProblem says a refused object action in the page's words: a job
// that runs already (409 busy) waits for its run, ferund's text kept.
export function actionProblem(e) {
  const text = (e && e.message) || String(e);
  const code = e && e.body && e.body.code;
  if (e && e.status === 409 && (code === 'busy' || /runs already/.test(text))) {
    return `${text} — a new run starts once it ends; try again then.`;
  }
  if (e && e.status === 409 && code === 'not_ready') return `${text} — the job's deployment is not ready yet.`;
  return text;
}

// preflightRows renders a binding check's result (the preflight's,
// control-plane.md "HTTP bindings") as rows: what was reached, in
// order; the fields not reached are left out.
export function preflightRows(res) {
  if (!res) return [];
  const rows = [];
  rows.push({ k: 'result', v: res.ok ? 'ok' : 'not ok', tone: res.ok ? 'ok' : 'bad' });
  if (res.error) {
    const e = res.error;
    const v = typeof e === 'string' ? e : [e.code, e.message].filter(Boolean).join(': ');
    rows.push({ k: 'error', v, tone: 'bad' });
  }
  if ((res.addresses || []).length > 0) rows.push({ k: 'addresses', v: res.addresses.join(', ') });
  if (res.tls) {
    rows.push({ k: 'tls subject', v: res.tls.subject || '—' });
    rows.push({ k: 'tls issuer', v: res.tls.issuer || '—' });
    if (res.tls.notAfter) rows.push({ k: 'valid until', v: res.tls.notAfter, time: true });
  }
  if (res.token) rows.push({ k: 'token', v: `fetched, expires in ${res.token.expiresIn} s` });
  if (res.probe) {
    rows.push({ k: 'probe', v: `HTTP ${res.probe.status} in ${res.probe.latencyMs} ms`, tone: res.probe.status < 400 ? 'ok' : 'warn' });
  }
  return rows;
}

// ---------------------------------------------------------------- traffic --

// sparkPoints are an SVG polyline's points of series[key] in a box w×h,
// the oldest left, scaled to max (the series' own by default, so that
// errors can be drawn to the scale of the requests); a bucket without a
// value (null) sits on the floor.
export function sparkPoints(series, key, w = 120, h = 24, top = 0) {
  const list = series || [];
  if (list.length === 0) return '';
  const vals = list.map((p) => Number(p[key]) || 0);
  const max = top > 0 ? top : Math.max(...vals);
  const step = list.length > 1 ? w / (list.length - 1) : 0;
  const r = (v) => Math.round(v * 10) / 10;
  return vals.map((v, i) => `${r(i * step)},${r(max > 0 ? h - (v / max) * (h - 1) - 0.5 : h - 0.5)}`).join(' ');
}

// fmtLatency renders a percentile in ms; null is a window without
// requests.
export function fmtLatency(ms) {
  if (ms === null || ms === undefined) return '—';
  const v = Number(ms);
  if (v >= 1000) return `${(v / 1000).toFixed(v >= 10000 ? 0 : 1)} s`;
  return `${v >= 100 ? Math.round(v) : v.toFixed(1)} ms`;
}

// errorShare is the share of a service's requests that failed, "0 %"
// without errors, "—" without requests.
export function errorShare(s) {
  if (!s || !s.requests) return '—';
  if (!s.errors) return '0 %';
  const p = (s.errors / s.requests) * 100;
  return `${p >= 10 ? Math.round(p) : p.toFixed(1)} %`;
}

// statusTone is the tone of an access log record's status: 5xx (and none
// written) bad, 4xx warn.
export function statusTone(status) {
  const s = Number(status) || 0;
  if (s === 0 || s >= 500) return 'bad';
  if (s >= 400) return 'warn';
  return 'ok';
}

// olderCursor is the beforeTsNs of the page before records (ascending):
// its first record's; '' for none.
export function olderCursor(records) {
  const list = records || [];
  return list.length > 0 ? String(list[0].tsNs) : '';
}

// newestFirst merges pages of access log records (each ascending) into
// one list, newest first, each record once.
export function newestFirst(...pages) {
  const seen = new Set();
  const all = [];
  for (const r of pages.flat()) {
    const k = `${r.tsNs}/${r.spanId || ''}/${r.instance || ''}`;
    if (seen.has(k)) continue;
    seen.add(k);
    all.push(r);
  }
  const ns = (r) => (/^\d+$/.test(String(r.tsNs)) ? BigInt(r.tsNs) : 0n);
  return all.sort((a, b) => (ns(b) > ns(a) ? 1 : ns(b) < ns(a) ? -1 : 0));
}

// ------------------------------------------------------ domains & access --

// SERVED_STATES are the domain states in which the ingress serves it, so
// that the environment's default host answers 404 (ADR 0021).
const SERVED_STATES = new Set(['served', 'dns-moved']);

// envDomains are the tenant's domains bound to an environment.
export function envDomains(domains, st) {
  if (!st) return [];
  return (domains || []).filter((d) => d.environment && (d.environment.id === st.id || d.environment.name === st.name));
}

// defaultHost is an environment's default host: {host, label, serving};
// while one of its domains is served the hosts are those and the default
// host (its label kept) answers 404, so host is '' unless a host of the
// status is it. null: it has none (not public yet, no apps zone).
export function defaultHost(st, domains) {
  if (!st) return null;
  const served = new Set(envDomains(domains, st).filter((d) => SERVED_STATES.has(d.state)).map((d) => d.name));
  const label = st.hostLabel || '';
  const own = (st.hosts || []).find((h) => !served.has(h) && (!label || h.startsWith(label + '.')));
  if (own) return { host: own, label: label || own.split('.')[0], serving: true };
  if (label) return { host: '', label, serving: false };
  return null;
}

// routeCurl is a request through the route door with a route token.
export function routeCurl(origin, env, token, path = '/') {
  const p = String(path || '/').replace(/^\/+/, '');
  return `curl -H 'X-FeRun-Route-Token: ${token}' ${origin}/api/v1/environments/${encodeURIComponent(env)}/http/${p}`;
}

// publishNote says what a publish or unpublish did ({public, hosts,
// rolling}).
export function publishNote(res) {
  if (!res) return '';
  if (!res.public) return 'Unpublished: the ingress stops serving it within a moment; the route door still serves it with a route token.';
  const at = (res.hosts || []).length > 0 ? ` at ${res.hosts.join(', ')}` : '';
  return res.rolling
    ? `Public${at}. This first publish rolls the image services once, so that their pods join the ingress's group.`
    : `Public${at}.`;
}

// ----------------------------------------------------------------- clone --

// cloneBody is a clone's request: the new name, the deployment whose
// spec it takes (0: the latest), and each secret copied or left out.
export function cloneBody({ name, deployment, copy }) {
  const body = { name: String(name || '').trim() };
  if (Number(deployment) > 0) body.deployment = Number(deployment);
  const secrets = {};
  for (const [k, v] of Object.entries(copy || {})) if (v) secrets[k] = { copy: true };
  if (Object.keys(secrets).length > 0) body.secrets = secrets;
  return body;
}

// deploymentText says which deployment an environment wants and which
// serves: "13 failed · 12 serves", "9 deploying", "3 deployed".
export function deploymentText(e) {
  if (!e.deployment) return e.state === 'deleting' ? 'deleting' : 'no deployment';
  if (e.state === 'deployed') return `${e.deployment} deployed`;
  const serves = e.deployed && e.deployed !== e.deployment ? ` · ${e.deployed} serves` : '';
  return `${e.deployment} ${e.state}${serves}`;
}

// ---------------------------------------------------------------- objects --

// KIND_NAMES name an object's kind in its page's head.
export const KIND_NAMES = { service: 'Service', addon: 'Addon', job: 'Job' };

// SPEC_KEYS are the spec's maps of the kinds.
const SPEC_KEYS = { service: 'services', addon: 'addons', job: 'jobs' };

// splitItem splits "<kind>/<name>"; null without a kind.
export function splitItem(item) {
  const s = String(item || '');
  const i = s.indexOf('/');
  if (i <= 0 || i === s.length - 1) return null;
  return { kind: s.slice(0, i), name: s.slice(i + 1) };
}

// specObject is the object kind/name of a spec, null when it has none.
export function specObject(spec, kind, name) {
  const map = spec && SPEC_KEYS[kind] ? spec[SPEC_KEYS[kind]] : null;
  const o = map && Object.prototype.hasOwnProperty.call(map, name) ? map[name] : null;
  return o && typeof o === 'object' ? o : null;
}

// shortImage is an image reference without its registry and path: the
// repository's last segment with its tag ("expense:1.4"), or with the
// first 12 hex digits of its digest.
export function shortImage(ref) {
  const s = String(ref || '');
  if (!s) return '';
  const at = s.indexOf('@');
  const base = at >= 0 ? s.slice(0, at) : s;
  const last = base.slice(base.lastIndexOf('/') + 1);
  if (at < 0) return last;
  const digest = s.slice(at + 1).replace(/^sha256:/, '');
  return `${last}@${digest.slice(0, 12)}`;
}

// objectImage is what an image service or a job runs: the image as the
// spec wrote it, the digest reference it is pinned to, and a short form
// of the one as written; without the spec, the instances' image (the
// pin alone).
export function objectImage(o, spec) {
  const so = o ? specObject(spec, o.kind, o.name) : null;
  let pinned = '';
  let written = '';
  if (so && so.image) {
    pinned = String(so.image).includes('@sha256:') ? so.image : '';
    written = so.imageAsWritten || so.image;
  }
  if (!written && o) {
    const i = (o.instances || []).find((x) => x.image);
    if (i) { pinned = i.image; written = i.image; }
  }
  const m = /@(sha256:[0-9a-f]+)$/.exec(pinned);
  return { written, pinned, digest: m ? m[1] : '', short: shortImage(written) };
}

// instanceDeployments are the deployments an object's instances run,
// ascending.
export function instanceDeployments(o) {
  return [...new Set((o && o.instances || []).map((i) => i.deployment).filter((n) => n > 0))].sort((a, b) => a - b);
}

// restartsOf counts an object's restarts: its instances' together, an
// attached service's own.
export function restartsOf(o) {
  if (!o) return 0;
  if (o.kind === 'service' && isAttached(o)) return o.restarts || 0;
  return (o.instances || []).reduce((n, i) => n + (i.restarts || 0), 0);
}

// readyText is an object's ready count: "1/2" for an image service and
// an addon (the addon's ready instances of its instances), '' else.
export function readyText(o) {
  if (!o) return '';
  if (o.kind === 'service' && !isAttached(o)) return `${o.ready || 0}/${o.desired || 0}`;
  if (o.kind === 'addon') {
    const list = o.instances || [];
    return `${list.filter((i) => i.phase === 'ready').length}/${list.length}`;
  }
  return '';
}

// objectStateText is an object's state in a few words, for its page's
// head: an image service's ready replicas, an attached service's and an
// addon's phase, a job's last run.
export function objectStateText(o) {
  if (!o) return 'not in the latest deployment';
  switch (o.kind) {
    case 'service':
      if (isAttached(o)) return o.phase || 'starting';
      return `${o.ready || 0} of ${o.desired || 0} ready${idleText(o)}`;
    case 'addon':
      return o.phase === 'ready' && !o.provisioned ? 'ready, not provisioned yet' : (o.phase || 'starting');
    case 'job':
      return o.lastRun ? `last run ${o.lastRun.phase}` : 'not run yet';
    default:
      return o.phase || '';
  }
}

// fmtCount renders a count short: 980, 1.2k, 34k, 1.2M.
export function fmtCount(n) {
  const v = Number(n) || 0;
  if (v < 1000) return String(v);
  if (v < 1e6) return `${v < 1e4 ? (v / 1000).toFixed(1) : Math.round(v / 1000)}k`;
  return `${(v / 1e6).toFixed(1)}M`;
}

// trafficOf is a service's line of a traffic answer, null without one.
export function trafficOf(traffic, name) {
  return ((traffic && traffic.services) || []).find((s) => s.service === name) || null;
}

// envValueRef is what an environment variable's value is: a plain
// string (also {value}), or a reference to a secret (with the version the
// spec pins, 0: none), an addon's output or a service's address; target
// is the name it names, ref its "<kind>/<name>".
export function envValueRef(v, spec) {
  if (typeof v === 'string') return { form: 'plain', text: v, ref: '' };
  if (!v || typeof v !== 'object') return { form: 'plain', text: v == null ? '' : String(v), ref: '' };
  if ('value' in v) return { form: 'plain', text: String(v.value), ref: '' };
  if (v.secret) {
    const pin = spec && spec.secrets && spec.secrets[v.secret];
    const version = (pin && pin.version) || 0;
    return { form: 'secret', target: v.secret, version, text: `secret ${v.secret}${version ? ` v${version}` : ''}`, ref: `secret/${v.secret}` };
  }
  if (v.addon) {
    const fields = Object.entries(v).filter(([k]) => k !== 'addon' && k !== 'key').sort(([a], [b]) => a.localeCompare(b));
    const extra = fields.length ? ` (${fields.map(([k, x]) => `${k} ${x}`).join(', ')})` : '';
    return { form: 'addon', target: v.addon, key: v.key || '', detail: extra, text: `${v.key || 'output'} of addon ${v.addon}${extra}`, ref: `addon/${v.addon}` };
  }
  if (v.service) {
    const port = v.port !== undefined && v.port !== '' ? ` port ${v.port}` : '';
    return { form: 'service', target: v.service, key: v.key || 'url', detail: port, text: `${v.key || 'url'} of service ${v.service}${port}`, ref: `service/${v.service}` };
  }
  return { form: 'plain', text: JSON.stringify(v), ref: '' };
}

// envRows are an object's environment variables, sorted, each with
// envValueRef's reading.
export function envRows(so, spec) {
  return Object.entries((so && so.env) || {}).sort(([a], [b]) => a.localeCompare(b))
    .map(([name, v]) => ({ name, ...envValueRef(v, spec) }));
}

// readinessText says a readiness check: "GET :http/readyz", "TCP :5432",
// "exec pg_isready", with its timing.
export function readinessText(r) {
  if (!r || typeof r !== 'object') return '';
  let s = '';
  if (r.http) s = `HTTP GET :${r.http.port}${r.http.path || '/'}`;
  else if (r.tcp) s = `TCP :${r.tcp.port}`;
  else if (r.exec) s = `exec ${(r.exec.command || []).join(' ')}`;
  else return '';
  const t = [];
  if (r.period) t.push(`every ${r.period}`);
  if (r.initialDelay && r.initialDelay !== '0s') t.push(`after ${r.initialDelay}`);
  if (r.failureThreshold) t.push(`unready after ${r.failureThreshold} failure${r.failureThreshold === 1 ? '' : 's'}`);
  return t.length ? `${s} · ${t.join(', ')}` : s;
}

// resourcesText says a pod's size: "1 vCPU · 512 MiB".
export function resourcesText(r) {
  if (!r || typeof r !== 'object') return '';
  const parts = [];
  if (r.vcpus) parts.push(`${r.vcpus} vCPU${r.vcpus === 1 ? '' : 's'}`);
  if (r.memoryMib) parts.push(r.memoryMib >= 1024 && r.memoryMib % 1024 === 0 ? `${r.memoryMib / 1024} GiB` : `${r.memoryMib} MiB`);
  return parts.join(' · ');
}

// definitionRows are an object's definition as the spec has it, as rows
// {k, v, mono}: the fields of its kind that are set, in the order one
// reads them; an image as written with its pin beside it (pinned).
export function definitionRows(so, kind) {
  if (!so) return [];
  const rows = [];
  const add = (k, v, extra = {}) => { if (v !== '' && v !== undefined && v !== null) rows.push({ k, v: String(v), ...extra }); };
  if (kind === 'addon') {
    add('type', so.type, { mono: true });
    add('version', so.version, { mono: true });
    add('plan', so.plan || 'the default plan');
    for (const [k, v] of Object.entries(so.options || {}).sort(([a], [b]) => a.localeCompare(b))) {
      add(`option ${k}`, Array.isArray(v) ? v.join(', ') : typeof v === 'object' ? JSON.stringify(v) : v, { mono: true });
    }
    return rows;
  }
  if (kind === 'service') add('runtime', so.runtime || 'image');
  if (so.sandbox) add('sandbox', so.sandbox, { mono: true, sandbox: true });
  if (so.image) {
    const pinned = String(so.image).includes('@sha256:') ? so.image : '';
    add('image', so.imageAsWritten || so.image, { mono: true, pinned });
  }
  if (so.imageCredentials) {
    const u = envValueRef(so.imageCredentials.username);
    const p = envValueRef(so.imageCredentials.password);
    add('registry login', `${u.form === 'secret' ? `secret ${u.target}` : u.text} · ${p.form === 'secret' ? `secret ${p.target}` : 'a literal password'}`
      + ' (the latest versions, at each pod)');
  }
  if ((so.command || []).length) add('command', so.command.join(' '), { mono: true });
  add('working directory', so.cwd, { mono: true });
  if ((so.ports || []).length) add('ports', so.ports.map((p) => `${p.name} ${p.port}`).join(', '), { mono: true });
  if (so.replicas !== undefined) add('replicas', so.replicas, { mono: true });
  add('resources', resourcesText(so.resources));
  add('readiness', readinessText(so.readiness), { mono: true });
  if (kind === 'service') add('idle', so.idle ? (so.idle === '0s' || so.idle === 'never' ? 'never paused' : `paused after ${so.idle} without a request`) : '');
  if (so.rollout && so.rollout.timeout) add('rollout timeout', so.rollout.timeout, { mono: true });
  if (kind === 'job') {
    add('runs', so.on === 'manual' ? 'when asked (on: manual)' : 'with each deployment that changes it (on: deploy)');
    add('timeout', so.timeout, { mono: true });
    if (so.retries !== undefined) add('retries', so.retries, { mono: true });
  }
  if (so.mounts) add('mounts', JSON.stringify(so.mounts), { mono: true });
  return rows;
}

// routesTo are the routes of the status that send to service name, with
// the spec's port as written (a name) beside the resolved number.
export function routesTo(st, spec, name) {
  const written = (spec && spec.routes) || {};
  return ((st && st.routes) || []).filter((r) => r.service === name).map((r) => {
    const w = written[r.name];
    const as = w && w.port !== undefined && String(w.port) !== String(r.port) ? String(w.port) : '';
    return { ...r, portAsWritten: as };
  });
}

// usedBy are the objects of the spec whose environment names kind/name
// (an addon's output, a service's address): [{from, var, text}].
export function usedBy(spec, kind, name) {
  const out = [];
  for (const k of ['service', 'job']) {
    for (const [n, o] of Object.entries((spec && spec[SPEC_KEYS[k]]) || {}).sort(([a], [b]) => a.localeCompare(b))) {
      for (const row of envRows(o, spec)) {
        if (row.ref === `${kind}/${name}`) out.push({ from: `${k}/${n}`, var: row.name, text: row.key || '' });
      }
    }
  }
  return out;
}

// CHANGES_READ is how many of the newest deployments an object's page
// reads for the changes to it: each is a request of its own.
export const CHANGES_READ = 10;

// changesTo are the changes to kind/name in deployments (each with its
// diff, newest first): [{number, state, author, message, createdAt,
// change, effect, fields}].
export function changesTo(deployments, kind, name) {
  const out = [];
  for (const d of deployments || []) {
    const e = (d.diff || []).find((x) => x.kind === kind && x.name === name);
    if (!e) continue;
    out.push({
      number: d.number, state: d.state, author: d.author || '', message: d.message || '', createdAt: d.createdAt || '',
      change: e.change, effect: e.effect || '', fields: e.fields || [],
    });
  }
  return out;
}

// objectIncidents are the incidents of kind/name.
export function objectIncidents(incidents, kind, name) {
  return (incidents || []).filter((i) => i.object === `${kind}/${name}`);
}
