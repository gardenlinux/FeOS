// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// The derivations of the node, sandbox and join-token views: pure, with no
// browser or API imports, so that the tests load them as they are.

export function fmtBytes(n) {
  const v = Number(n || 0);
  // **, not <<: shifts in JavaScript are 32-bit (1 << 40 === 256).
  if (v >= 2 ** 40) return (v / 2 ** 40).toFixed(1) + ' TiB';
  if (v >= 2 ** 30) return (v / 2 ** 30).toFixed(1) + ' GiB';
  if (v >= 2 ** 20) return (v / 2 ** 20).toFixed(0) + ' MiB';
  if (v >= 1024) return (v / 1024).toFixed(0) + ' KiB';
  return v + ' B';
}

// The node statuses: joined (a certificate, no telemetry yet), ready, down
// (telemetry stopped), removed (by an operator; kept for its history).
export const NODE_STATUSES = ['joined', 'ready', 'down', 'removed'];

// nodeRows derives the node table's rows from GET /api/v1/admin/nodes.
// Removed nodes go last; the others keep the API's order (by name).
export function nodeRows(nodes) {
  const rows = (nodes || []).map((n) => {
    const status = n.status || 'unknown';
    const memTotal = Number(n.memoryTotalbytes || 0);
    const memAvail = Number(n.memAvailBytes || 0);
    // In use: the total less what the host reports available, what the
    // memory bar fills by and labels.
    const memUsed = Math.max(0, memTotal - memAvail);
    return {
      key: n.node,
      node: n.node,
      cls: status === 'down' ? 'down' : status === 'removed' ? 'removed' : '',
      status,
      description: n.statusDescription || '',
      draining: !!n.draining,
      hostname: n.hostname || '',
      // Connected to the instance that answered, with how many node
      // connections (the current one and those still draining).
      connected: !!n.connected,
      connections: Number(n.connections || 0),
      lastSeenNs: Number(n.lastSeenAtNs || 0),
      memAvail,
      memTotal,
      memUsed,
      memPct: memTotal > 0 ? Math.round((100 * memUsed) / memTotal) : 0,
      load1: Number(n.load1 || 0),
      diskAvail: Number(n.diskAvailBytes || 0),
      running: Number(n.sandboxesRunning || 0),
      paused: Number(n.sandboxesPaused || 0),
      slots: Number(n.slots || 0),
      inventoryHash: n.inventoryHash || '',
      cpus: Number(n.cpuNumcores || 0),
      // What placement booked against what it may book: vCPUs against
      // floor(cores × cpuOvercommit), where 0 means CPU is not gated, and
      // memory against the total less the headroom.
      booked: Number(n.sandboxesBooked || 0),
      vcpusBooked: Number(n.vcpusBooked || 0),
      vcpusCapacity: Number(n.vcpusCapacity || 0),
      // ferund keeps the overcommit as a float32 and renders it widened
      // (1.1 comes back as 1.100000023841858); six significant digits give
      // back the value the operator entered.
      cpuOvercommit: Number(Number(n.cpuOvercommit || 0).toPrecision(6)),
      memBooked: Number(n.memoryBookedBytes || 0),
      memCapacity: Number(n.memoryCapacityBytes || 0),
      memHeadroom: Number(n.memHeadroomBytes || 0),
      certSerial: n.certSerial || '',
      certNotAfter: n.certNotAfter || '',
      objectStore: !!n.objectStore,
      objectStoreId: n.objectStoreId || '',
      identityForked: !!n.identityForked,
      stoppingUntil: n.stoppingUntil || '',
      drainStartedAt: n.drainStartedAt || '',
      removedAt: n.removedAt || '',
    };
  });
  const order = (r) => (r.status === 'removed' ? 1 : 0);
  return rows.map((r, i) => [r, i]).sort((a, b) => order(a[0]) - order(b[0]) || a[1] - b[1]).map(([r]) => r);
}

// fmtBytesOf renders a part of a whole, "12.0 / 63.0 GiB", with the unit
// once when both share it.
export function fmtBytesOf(part, whole) {
  const p = fmtBytes(part);
  const w = fmtBytes(whole);
  const unit = (s) => s.slice(s.lastIndexOf(' '));
  return unit(p) === unit(w) ? `${p.slice(0, p.lastIndexOf(' '))} / ${w}` : `${p} / ${w}`;
}

// memBar is the memory bar's label and title: the memory in use against
// the total, as the bar fills, and what is available in the title.
export function memBar(row) {
  return {
    label: fmtBytesOf(row.memUsed, row.memTotal),
    title: `${fmtBytes(row.memUsed)} in use of ${fmtBytes(row.memTotal)}, ${fmtBytes(row.memAvail)} available`,
  };
}

// vcpuCapacityText renders a vCPU capacity: 0 is "∞" (not gated).
export function vcpuCapacityText(n) {
  const v = Number(n || 0);
  return v > 0 ? String(v) : '∞';
}

// canDrain reports whether a node can be drained or undrained: not when
// it is down or removed (ferund answers 409 to a drain then).
export function canDrain(row) {
  return row.status !== 'down' && row.status !== 'removed';
}

// stopping reports whether the node's feosd announced a restart that is
// still under way at now (ms): ferund clears stoppingUntil only when the
// node starts again, so a node that stopped for good keeps a past time.
export function stopping(row, now = Date.now()) {
  const until = Date.parse(row.stoppingUntil || '');
  return !Number.isNaN(until) && until > now;
}

// certState is a certificate's state at now (ms): revoked, expired,
// expiring (within a day), or valid.
export function certState(cert, now = Date.now()) {
  if (cert.revokedAt) return 'revoked';
  const after = Date.parse(cert.notAfter || '');
  if (!Number.isNaN(after)) {
    if (after <= now) return 'expired';
    if (after - now < 24 * 3600 * 1000) return 'expiring';
  }
  return 'valid';
}

// settingsForm is the node settings form's values for a row: slots, the
// memory headroom in whole MiB and the CPU overcommit, as the inputs'
// strings.
export function settingsForm(row) {
  return {
    slots: String(row.slots),
    memHeadroomMiB: String(Math.round(row.memHeadroom / 2 ** 20)),
    cpuOvercommit: String(row.cpuOvercommit),
  };
}

// settingsPatch is the PATCH body of the node settings form: only the
// fields the operator changed, each checked (≥ 0). A field counts as
// changed when its value differs from base, the settingsForm the form
// started from, not from the node's exact values: the form shows the
// headroom rounded to MiB, and a value changed elsewhere while the
// operator edited another field is not the form's to overwrite. The headroom is entered in MiB and sent in
// bytes, as an int64 string. It throws on a bad value.
export function settingsPatch(base, form) {
  const out = {};
  const field = (label, key, re) => {
    const s = String(form[key]).trim();
    if (s === '') return null;
    const v = re.test(s) ? Number(s) : NaN;
    if (!Number.isFinite(v) || v < 0) throw new Error(`${label} must be a number ≥ 0`);
    return v === Number(String(base[key]).trim()) ? null : v;
  };
  const slots = field('slots', 'slots', /^\d+$/);
  if (slots !== null) out.slots = slots;
  const mib = field('memory headroom', 'memHeadroomMiB', /^\d+$/);
  if (mib !== null) out.memHeadroomBytes = String(mib * 2 ** 20);
  const oc = field('CPU overcommit', 'cpuOvercommit', /^\d+(\.\d+)?$/);
  if (oc !== null) out.cpuOvercommit = oc;
  return out;
}

// fmtSize renders a sandbox's size.
export function fmtSize(sb) {
  const v = Number(sb.vcpus || 0);
  const m = Number(sb.memoryMib || 0);
  if (!v && !m) return '—';
  return `${v} vCPU · ${fmtBytes(m * 2 ** 20)}`;
}

// sandboxCounts buckets the sandboxes by clientState: hibernated is its
// own bucket (its checkpoint is in the object store, nothing on a node),
// and failed and lost are the errors.
export function sandboxCounts(sandboxes) {
  const c = { running: 0, paused: 0, hibernated: 0, error: 0, other: 0 };
  for (const s of sandboxes || []) {
    if (s.clientState === 'running') c.running++;
    else if (s.clientState === 'paused') c.paused++;
    else if (s.clientState === 'hibernated') c.hibernated++;
    else if (s.clientState === 'failed' || s.clientState === 'lost') c.error++;
    else c.other++;
  }
  return c;
}

// joinTokenState is a join token's state at now (ms): used, expired or
// open.
export function joinTokenState(t, now = Date.now()) {
  if (t.usedAt) return 'used';
  const exp = Date.parse(t.expiresAt || '');
  if (!Number.isNaN(exp) && exp <= now) return 'expired';
  return 'open';
}

// tokenRows orders the tokens of a tenant's list newest first.
export function tokenRows(tokens) {
  return [...(tokens || [])]
    .sort((a, b) => (b.createdAt || '').localeCompare(a.createdAt || ''));
}

// tenantDeleteText is the delete button's confirmation and title for a
// tenant of the admin list: a tenant's deletion takes its sandboxes and
// its environments, with their addons' data, along.
export function tenantDeleteText(t) {
  const n = (v) => Number(v) || 0;
  const of = (k, one, many) => `${k} ${k === 1 ? one : many}`;
  const sandboxes = n(t && t.sandboxCount);
  const envs = n(t && t.environmentCount);
  let confirm = 'delete?';
  if (sandboxes && envs) {
    confirm = `delete with ${of(sandboxes, 'sandbox', 'sandboxes')} and ${of(envs, 'environment', 'environments')} `
      + 'and their data?';
  } else if (envs) {
    confirm = `delete with ${of(envs, 'environment', 'environments')} and their data?`;
  } else if (sandboxes) {
    confirm = `delete with ${of(sandboxes, 'sandbox', 'sandboxes')}?`;
  }
  return {
    confirm,
    title: 'delete the tenant, its tokens, every sandbox and every environment of it, with the environments\' addon data',
  };
}

// fmtSeconds renders a duration in seconds as the flags' values read:
// "5 s", "1.5 s", "90 s", "10 min", "1 h", "1 h 30 min"; whole minutes
// as minutes, anything else in seconds; '' when unset.
export function fmtSeconds(sec) {
  const v = Number(sec);
  if (!Number.isFinite(v) || v <= 0) return '';
  if (v % 60 !== 0) return `${Number(v.toFixed(3))} s`;
  const min = v / 60;
  if (min < 60) return `${min} min`;
  const h = Math.floor(min / 60);
  const rest = min % 60;
  return rest ? `${h} h ${rest} min` : `${h} h`;
}

// fleetDefaults reads the defaults of GET /api/v1/admin/nodes: the
// instance's timings (rendered) and the size and image of a sandbox
// created without one, with the bounds a create accepts. A field the
// answer lacks (an older ferund) is '' or 0, and the views then name the
// flag without a value.
export function fleetDefaults(d) {
  const n = (k) => Number((d && d[k]) || 0);
  return {
    telemetryInterval: fmtSeconds(n('telemetryIntervalSeconds')),
    nodeTtl: fmtSeconds(n('nodeTtlSeconds')),
    lostGrace: fmtSeconds(n('lostGraceSeconds')),
    drainTimeout: fmtSeconds(n('drainTimeoutSeconds')),
    vcpus: n('defaultVcpus'),
    memoryMib: n('defaultMemoryMib'),
    image: (d && d.defaultImage) || '',
    minVcpus: n('minVcpus'),
    maxVcpus: n('maxVcpus'),
    minMemoryMib: n('minMemoryMib'),
    maxMemoryMib: n('maxMemoryMib'),
  };
}

// createHints are the create form's placeholders, titles and bounds from
// fleetDefaults: an empty field takes the default, which the form names.
export function createHints(fd) {
  const range = (min, max) => (min && max ? `${min}–${max}` : '');
  const empty = (v) => `empty: ${v || 'the default'}`;
  const vr = range(fd.minVcpus, fd.maxVcpus);
  const mr = range(fd.minMemoryMib, fd.maxMemoryMib);
  return {
    image: `image (${empty(fd.image)})`,
    vcpus: {
      placeholder: fd.vcpus ? `vCPUs (${fd.vcpus})` : 'vCPUs',
      title: `vCPUs${vr ? ', ' + vr : ''} (${empty(fd.vcpus)})`,
      min: fd.minVcpus || undefined,
      max: fd.maxVcpus || undefined,
    },
    memory: {
      placeholder: fd.memoryMib ? `MiB (${fd.memoryMib})` : 'MiB',
      title: `memory in MiB${mr ? ', ' + mr : ''} (${empty(fd.memoryMib)})`,
      min: fd.minMemoryMib || undefined,
      max: fd.maxMemoryMib || undefined,
    },
  };
}
