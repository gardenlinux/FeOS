// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// The arithmetic of the jobs made by a call (ADR 0041): pure, so
// that the tests load it as it is. A job's shape is ferund's
// TenantJobStatus (go/internal/env/tenantjobs.go): id, image, imageRef,
// phase, reason, detail, exitCode, result, resultPath, vcpus, memoryMib,
// labels, createdAt, startedAt, finishedAt, deadlineAt, and (ADR 0035)
// environment {id, name}, reaches and egress, fixed at its create.

import { egressProblems, reachesValue } from './reach.js';

// The bounds of a create, as ferund checks them (go/internal/env/spec).
export const JOB_LIMITS = {
  minVcpus: 1, maxVcpus: 32, defaultVcpus: 1,
  minMemoryMib: 256, maxMemoryMib: 65536, defaultMemoryMib: 512,
  defaultDeadline: '10m', maxDeadlineMs: 2 * 3600 * 1000,
  resultBytes: 4096, keptDays: 7,
};

// ended tells a phase a job does not leave: succeeded, failed,
// deadline_exceeded, cancelled.
export function ended(phase) {
  return phase === 'succeeded' || phase === 'failed' || phase === 'deadline_exceeded' || phase === 'cancelled';
}

// onItsWay tells a job that has not ended: queued or running.
export function onItsWay(phase) {
  return phase === 'queued' || phase === 'running';
}

// jobTone is the class of a phase's dot (styles.css .sdot): on its way
// dashed, succeeded green, failed and past the deadline red, cancelled
// grey.
export function jobTone(phase) {
  switch (phase) {
    case 'queued': case 'running': return 'moving';
    case 'succeeded': return 'running';
    case 'failed': case 'deadline_exceeded': return 'failed';
    default: return 'absent';
  }
}

// phaseWord is a phase as the list shows it.
export function phaseWord(phase) {
  return phase === 'deadline_exceeded' ? 'deadline exceeded' : phase || 'unknown';
}

// jobWords says in a sentence where a job is, or why it ended.
export function jobWords(job) {
  if (!job) return '';
  switch (job.phase) {
    case 'queued': return 'Waiting for its pod: room on a node, the image\'s pull.';
    case 'running': return 'Its container runs.';
    case 'succeeded': return 'Its container exited with code 0.';
    case 'deadline_exceeded': return 'It passed its deadline and was stopped.';
    case 'cancelled': return 'It was cancelled.';
    case 'failed':
      switch (job.reason) {
        case 'exit': return `Its container exited with code ${job.exitCode ?? '?'}.`;
        case 'image': return 'The registry refused the image when its pod was made.';
        case 'node_lost': return 'Its node was lost. A job never runs twice: run a new one if that is safe.';
        case 'no_exit_status': return 'Its container ended without an exit status.';
        default: return 'It failed.';
      }
    default: return '';
  }
}

// jobDurationMs is how long a job ran: from its start to its end, or to
// now while it runs; null before it started.
export function jobDurationMs(job, now = Date.now()) {
  const start = Date.parse((job && job.startedAt) || '');
  if (Number.isNaN(start)) return null;
  const end = job.finishedAt ? Date.parse(job.finishedAt) : (ended(job.phase) ? NaN : now);
  if (Number.isNaN(end)) return null;
  return Math.max(0, end - start);
}

// fmtSpan renders a duration as "45s", "4m 12s", "1h 03m".
export function fmtSpan(ms) {
  if (ms === null || ms === undefined || !Number.isFinite(ms)) return '—';
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${String(s % 60).padStart(2, '0')}s`;
  return `${Math.floor(m / 60)}h ${String(m % 60).padStart(2, '0')}m`;
}

// jobCounts counts the jobs on their way, succeeded and failed (failed
// and past the deadline).
export function jobCounts(jobs) {
  const out = { onItsWay: 0, succeeded: 0, failed: 0, cancelled: 0 };
  for (const j of jobs || []) {
    if (onItsWay(j.phase)) out.onItsWay++;
    else if (j.phase === 'succeeded') out.succeeded++;
    else if (j.phase === 'cancelled') out.cancelled++;
    else out.failed++;
  }
  return out;
}

// keptUntil is when ferund sweeps an ended job's record: 7 days after
// its end (env-delete); '' while it has not ended.
export function keptUntil(job) {
  const end = Date.parse((job && job.finishedAt) || '');
  if (Number.isNaN(end)) return '';
  return new Date(end + JOB_LIMITS.keptDays * 86400000).toISOString();
}

// resultView is a job's result to show: pretty-printed when it is JSON.
export function resultView(result) {
  const text = result || '';
  if (!text.trim()) return { text: '', json: false };
  try {
    const v = JSON.parse(text);
    if (v !== null && typeof v === 'object') return { text: JSON.stringify(v, null, 2), json: true };
  } catch {
    // Not JSON: as it is.
  }
  return { text, json: false };
}

// ------------------------------------------------------------- the form --

// parseArgs reads the command: one argument per line, as typed (a
// space is part of its argument); empty lines are left out.
export function parseArgs(text) {
  return String(text || '').split('\n').map((l) => l.replace(/\r$/, '')).filter((l) => l !== '');
}

// parsePairs reads key=value lines (the value may hold '=' and is taken
// as typed); empty lines are left out. bad are the lines without a key.
export function parsePairs(text) {
  const map = {};
  const bad = [];
  for (const raw of String(text || '').split('\n')) {
    const line = raw.replace(/\r$/, '');
    if (!line.trim()) continue;
    const i = line.indexOf('=');
    const k = i < 0 ? '' : line.slice(0, i).trim();
    if (!k) { bad.push(line.trim()); continue; }
    map[k] = line.slice(i + 1);
  }
  return { map, bad };
}

const UNITS = { ns: 1e-6, us: 1e-3, 'µs': 1e-3, ms: 1, s: 1000, m: 60000, h: 3600000 };

// parseDuration reads a duration as Go writes one ("90s", "15m",
// "1h30m") into milliseconds; NaN for what is none.
export function parseDuration(text) {
  const s = String(text || '').trim();
  if (!s) return NaN;
  const re = /(\d+(?:\.\d*)?|\.\d+)(ns|us|µs|ms|s|m|h)/gy;
  let total = 0;
  let pos = 0;
  let m;
  while ((m = re.exec(s)) !== null) {
    total += Number(m[1]) * UNITS[m[2]];
    pos = re.lastIndex;
  }
  return pos === s.length ? total : NaN;
}

export const ENV_NAME = /^[A-Za-z_][A-Za-z0-9_]*$/;

// EMPTY_FORM is the form of a new job: no environment yet, reaching
// nothing of it, out to the internet.
export const EMPTY_FORM = {
  environment: '', reachMode: 'none', reaches: [], egress: undefined,
  image: '', command: '', env: '', secrets: '', vcpus: '', memoryMib: '', deadline: '', resultPath: '',
  labels: '', username: '', password: '',
};

function intIn(text, lo, hi, what, errors) {
  const s = String(text || '').trim();
  if (!s) return 0;
  const n = Number(s);
  if (!Number.isInteger(n) || n < lo || n > hi) {
    errors.push(`${what} must be a whole number from ${lo} to ${hi}`);
    return 0;
  }
  return n;
}

// jobRequest turns the form into POST /api/v1/jobs's body, with what
// ferund would refuse as errors in words (it checks again).
export function jobRequest(form) {
  const f = { ...EMPTY_FORM, ...form };
  const errors = [];
  const body = {};
  const environment = String(f.environment || '').trim();
  if (!environment) errors.push('an environment is required');
  else body.environment = environment;
  const reaches = reachesValue(f.reachMode, f.reaches);
  if (reaches !== undefined) body.reaches = reaches;
  errors.push(...egressProblems(f.egress));
  if (f.egress !== undefined && f.egress !== 'internet') body.egress = f.egress;
  const image = f.image.trim();
  if (!image) errors.push('an image is required');
  body.image = image;

  const command = parseArgs(f.command);
  if (command.length) body.command = command;

  const env = parsePairs(f.env);
  const secrets = parsePairs(f.secrets);
  for (const [what, p] of [['env', env], ['secrets', secrets]]) {
    if (p.bad.length) errors.push(`${what}: a line is key=value, not ${p.bad.join(', ')}`);
    const wrong = Object.keys(p.map).filter((k) => !ENV_NAME.test(k));
    if (wrong.length) errors.push(`${what}: ${wrong.join(', ')} is no environment variable's name`);
  }
  const both = Object.keys(secrets.map).filter((k) => k in env.map);
  if (both.length) errors.push(`${both.join(', ')} is in env and in secrets`);
  if (Object.keys(env.map).length) body.env = env.map;
  if (Object.keys(secrets.map).length) body.secrets = secrets.map;

  const L = JOB_LIMITS;
  const vcpus = intIn(f.vcpus, L.minVcpus, L.maxVcpus, 'vCPUs', errors);
  const mem = intIn(f.memoryMib, L.minMemoryMib, L.maxMemoryMib, 'memory (MiB)', errors);
  if (vcpus) body.vcpus = vcpus;
  if (mem) body.memoryMib = mem;

  const deadline = f.deadline.trim();
  if (deadline) {
    const ms = parseDuration(deadline);
    if (!(ms > 0) || ms > L.maxDeadlineMs) errors.push(`the deadline is a duration of up to 2h (90s, 15m, 1h30m), not ${deadline}`);
    else body.deadline = deadline;
  }

  const rp = f.resultPath.trim();
  if (rp) {
    const clean = rp.startsWith('/') && !/\/\/|\/\.\.?(\/|$)/.test(rp) && !(rp.length > 1 && rp.endsWith('/'));
    if (!clean || rp === '/' || rp === '/dev' || rp.startsWith('/dev/')) {
      errors.push(`the result path must be an absolute, clean path of a file outside /dev, not ${rp}`);
    } else body.resultPath = rp;
  }

  const labels = parsePairs(f.labels);
  if (labels.bad.length) errors.push(`labels: a line is key=value, not ${labels.bad.join(', ')}`);
  if (Object.keys(labels.map).length) body.labels = labels.map;

  const user = f.username.trim();
  if (user || f.password) {
    if (!user || !f.password) errors.push('the image\'s credentials need a username and a password');
    else body.imageCredentials = { username: user, password: f.password };
  }
  return { body, errors };
}
