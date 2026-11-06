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
  }));
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
