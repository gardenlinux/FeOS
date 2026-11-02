// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// The environments view's arithmetic: pure, with no React or browser
// imports, so that the tests load it as it is. The shapes are ferund's
// (go/internal/env/wire.go): an environment's status with its objects
// and their instances, releases, diffs and incidents.

// Tones are the classes of the state dots and labels (styles.css):
// running green, paused amber, failed red, absent grey.

// releaseTone is the tone of a release's state, or of an environment's
// in the list (the latest release's state, "none" or "deleting").
export function releaseTone(state) {
  switch (state) {
    case 'deployed': return 'running';
    case 'waiting': case 'deploying': case 'deleting': return 'paused';
    case 'failed': return 'failed';
    default: return 'absent';
  }
}

// phaseTone is the tone of an instance's or an object's phase, and of a
// job run's (running, succeeded, failed).
export function phaseTone(phase) {
  switch (phase) {
    case 'ready': case 'succeeded': return 'running';
    case 'starting': case 'unready': case 'running': return 'paused';
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
    return (o.ready || 0) >= o.desired ? 'running' : 'paused';
  }
  if (o.kind === 'addon' && o.phase === 'ready' && !o.provisioned) return 'paused';
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
      let s = `release ${r.release}, attempt ${r.attempt}: ${r.phase}`;
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

// originText says where a release came from: an apply, a revert to an
// earlier release, a clone of another environment's release, or a change
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
// release, and their open incidents.
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
// status: deleting, none without a release, else the latest release's.
export function statusState(st) {
  if (!st) return '';
  if (st.deleting) return 'deleting';
  return st.rollout ? st.rollout.state : 'none';
}

// rolloutNote says what serves while the latest release is not deployed:
// the deployed release keeps serving a failed one and serves during a
// rollout.
export function rolloutNote(st) {
  const r = st && st.rollout;
  if (!r) return '';
  if (r.state === 'deployed') return 'every object matches it';
  if (st.deployed > 0 && st.deployed !== r.release) {
    return r.state === 'failed' ? `release ${st.deployed} keeps serving` : `release ${st.deployed} serves meanwhile`;
  }
  if (r.state === 'failed') return 'no release was deployed before it';
  return '';
}
