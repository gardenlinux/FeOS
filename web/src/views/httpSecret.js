// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// An environment's HTTP secrets (ADR 0055, the credentials of ADR 0033
// before): an HTTP API stated once in an environment, its URL, how the
// code calls it, its authentication (its credential, ADR 0028's kinds)
// and its options, versioned as one; a variable {secret, key: url |
// host} of a service or job gets it, a sandbox names it. The form's
// fields to its JSON and back, the secret fields a write keeps ({"set":
// true}), what the code runs, and whether the environment's deployments
// pin a version behind the latest.

// KINDS are the kinds of an HTTP secret's authentication, as the form
// offers them.
export const KINDS = [
  { key: 'headers', label: 'headers' },
  { key: 'basic', label: 'basic' },
  { key: 'oauth2', label: 'OAuth2 client credentials' },
  { key: 'clientCertificate', label: 'client certificate' },
];

// kindLabel names a kind.
export function kindLabel(kind) {
  const k = KINDS.find((x) => x.key === kind);
  return k ? k.label : kind || '';
}

// SECRETS are each kind's secret fields beside the headers' values,
// which are all secret.
export const SECRETS = { headers: [], basic: ['password'], oauth2: ['clientSecret'], clientCertificate: ['keyPem'] };

// REDACTED is what an upstream's echo of a secret comes back as.
export const REDACTED = 'redacted-by-FeOS-binding';

// nameRe is an HTTP secret's name: a DNS label, since the name mode
// makes it a host name.
const nameRe = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;

// isName tells an HTTP secret's name.
export function isName(s) {
  return nameRe.test(String(s || ''));
}

// emptyForm is a new HTTP secret's form.
export function emptyForm() {
  return {
    name: '', kind: 'headers', upstream: '', mode: 'name',
    headers: [{ name: 'Authorization', value: '', set: false }],
    user: '', password: '', tokenUrl: '', clientId: '', clientSecret: '', scope: '', audience: '',
    clientAuthInBody: false, certPem: '', keyPem: '',
    pathPrefixes: '', ratePerSecond: '', rateBurst: '', caPem: '', forwardedHost: false,
    // set: the secret fields the latest version has, which an empty
    // field keeps.
    set: {},
  };
}

// formOf is the form of an HTTP secret as a read answers it: its secret
// fields empty and marked set.
export function formOf(c) {
  const f = emptyForm();
  const cr = c.credential || {};
  const set = {};
  for (const k of ['password', 'clientSecret', 'keyPem']) if (cr[k] && cr[k].set) set[k] = true;
  const headers = Object.keys(cr.headers || {}).sort().map((name) => ({ name, value: '', set: !!(cr.headers[name] || {}).set }));
  return {
    ...f,
    name: c.name || '', kind: cr.kind || 'headers', upstream: c.upstream || '', mode: c.mode || 'name',
    headers: cr.kind === 'headers' ? headers : f.headers,
    user: cr.user || '', tokenUrl: cr.tokenUrl || '', clientId: cr.clientId || '', scope: cr.scope || '',
    audience: cr.audience || '', clientAuthInBody: !!cr.clientAuthInBody, certPem: cr.certPem || '',
    pathPrefixes: (c.pathPrefixes || []).join(' '),
    ratePerSecond: c.rateLimit && c.rateLimit.perSecond ? String(c.rateLimit.perSecond) : '',
    rateBurst: c.rateLimit && c.rateLimit.burst ? String(c.rateLimit.burst) : '',
    caPem: c.caPem || '', forwardedHost: !!c.forwardedHost, set,
  };
}

// prefixesOf splits the path prefixes' field at blanks and commas.
export function prefixesOf(s) {
  return String(s || '').split(/[\s,]+/).filter(Boolean);
}

// keptOf is a form's secret fields left empty that the latest version
// has: what a write keeps, as the paths ferund names them
// ("password", "headers.Authorization").
export function keptOf(f) {
  const out = [];
  for (const k of SECRETS[f.kind] || []) if (!f[k] && f.set[k]) out.push(k);
  if (f.kind === 'headers') {
    for (const h of f.headers) if (h.name.trim() && !h.value && h.set) out.push('headers.' + h.name.trim());
  }
  return out;
}

// whole is a positive whole number of a field, 0 for none; it throws on
// anything else.
function whole(label, s) {
  const v = String(s || '').trim();
  if (!v) return 0;
  if (!/^\d+$/.test(v)) throw new Error(`${label} must be a whole number`);
  return Number(v);
}

// pem is a PEM field's text with one final newline; '' for none.
function pem(s) {
  const v = String(s || '').trim();
  return v ? v + '\n' : '';
}

// definitionOf is a form as the HTTP secret's definition (the body of
// PUT /api/v1/environments/{env}/secrets/{name} beside its type, a
// check's draft): a secret field left empty that the latest version has
// is {"set": true} when keep, else left out. It throws on a form no
// request should carry.
export function definitionOf(f, { keep = true } = {}) {
  const upstream = String(f.upstream || '').trim().replace(/\/+$/, '');
  if (!upstream) throw new Error('the upstream is required: https://host[:port]');
  const b = { upstream, mode: f.mode === 'intercept' ? 'intercept' : 'name', pathPrefixes: prefixesOf(f.pathPrefixes) };
  if (String(f.caPem || '').trim()) b.caPem = pem(f.caPem);
  if (f.forwardedHost && b.mode === 'name') b.forwardedHost = true;
  const perSecond = whole('the rate limit', f.ratePerSecond);
  const burst = whole('the burst', f.rateBurst);
  if (burst && !perSecond) throw new Error('a burst needs a rate per second');
  if (perSecond) b.rateLimit = { perSecond, burst: burst || perSecond };
  const secret = (k) => (f[k] ? f[k] : keep && f.set[k] ? { set: true } : undefined);
  const c = { kind: f.kind };
  switch (f.kind) {
    case 'headers': {
      c.headers = {};
      for (const h of f.headers) {
        const name = h.name.trim();
        if (!name) continue;
        if (c.headers[name] !== undefined) throw new Error(`the header ${name} is named twice`);
        if (h.value) c.headers[name] = h.value;
        else if (keep && h.set) c.headers[name] = { set: true };
        else throw new Error(`the header ${name} needs a value`);
      }
      if (!Object.keys(c.headers).length) throw new Error('add a header, such as Authorization: Bearer …');
      break;
    }
    case 'basic':
      c.user = f.user.trim();
      c.password = secret('password');
      break;
    case 'oauth2':
      c.tokenUrl = f.tokenUrl.trim();
      c.clientId = f.clientId.trim();
      c.clientSecret = secret('clientSecret');
      if (f.scope.trim()) c.scope = f.scope.trim();
      if (f.audience.trim()) c.audience = f.audience.trim();
      if (f.clientAuthInBody) c.clientAuthInBody = true;
      break;
    case 'clientCertificate':
      c.certPem = pem(f.certPem);
      c.keyPem = f.keyPem.trim() ? pem(f.keyPem) : secret('keyPem');
      break;
    default:
      throw new Error(`unknown kind ${f.kind}`);
  }
  for (const k of Object.keys(c)) if (c[k] === undefined) delete c[k];
  b.credential = c;
  return b;
}

// hostOf is an upstream's host and port ("api.github.com",
// "api.example.com:8443"); '' for no URL.
export function hostOf(upstream) {
  const m = /^[a-z]+:\/\/([^/?#]+)/i.exec(String(upstream || '').trim());
  return m ? m[1].toLowerCase().replace(/:443$/, '') : '';
}

// modeWords is how the code reaches an HTTP secret: "http://github/…"
// or "intercepts api.github.com".
export function modeWords(c) {
  if (c.mode === 'intercept') return `intercepts ${hostOf(c.upstream) || 'the upstream\'s host'}`;
  return `http://${c.name || '<name>'}/…`;
}

// refValue is what a variable {secret, key} of HTTP secret c gets (ADR
// 0055): key url the base URL the code calls, http://<name> in the name
// mode and https://<the upstream's host> in the intercept mode; key host
// the host alone.
export function refValue(c, key = 'url') {
  const intercept = c && c.mode === 'intercept';
  const host = intercept ? hostOf(c.upstream) || '<host>' : (c && c.name) || '<name>';
  if (key === 'host') return host;
  return `${intercept ? 'https' : 'http'}://${host}`;
}

// curlOf is the request the code runs: by the name, or at the upstream's
// own host, at the first path prefix.
export function curlOf(c) {
  const prefixes = Array.isArray(c.pathPrefixes) ? c.pathPrefixes : prefixesOf(c.pathPrefixes);
  const path = prefixes[0] || '/';
  if (c.mode === 'intercept') return `curl -s https://${hostOf(c.upstream) || '<host>'}${path}`;
  return `curl -s http://${c.name || '<name>'}${path}`;
}

// behind tells whether the environment's deployments pin a version
// before the latest, in its latest or its deployed deployment (use: the
// HTTP secret's usedBy, latest and deployed 0 where that one pins none).
export function behind(use, version) {
  return (Number(use.latest) > 0 && Number(use.latest) < version) || (Number(use.deployed) > 0 && Number(use.deployed) < version);
}

// needsDeployment tells whether the environment's latest deployment pins
// a version before the latest: a new deployment would move it. When its
// latest deployment pins the latest but its deployed one does not, it is
// on its way.
export function needsDeployment(use, version) {
  return Number(use.latest) > 0 && Number(use.latest) < version;
}

// pinState is the environment's pins against the latest version, in a
// word: behind, rolling out (its latest deployment has it, the deployed
// one not yet) or current.
export function pinState(use, version) {
  if (needsDeployment(use, version)) return 'behind';
  if (behind(use, version)) return 'rolling out';
  return 'current';
}

// pinWords is what the environment's deployments pin, in words.
export function pinWords(use) {
  const l = Number(use.latest) || 0;
  const d = Number(use.deployed) || 0;
  if (l && d && l === d) return `v${l}`;
  const parts = [];
  if (l) parts.push(`latest deployment v${l}`);
  if (d) parts.push(`deployed v${d}`);
  return parts.join(' · ') || 'no version';
}

// oldestPin is the oldest version the environment's deployments pin.
export function oldestPin(use) {
  const vs = [Number(use.latest) || 0, Number(use.deployed) || 0].filter((v) => v > 0);
  return vs.length ? Math.min(...vs) : 0;
}

// usedWords is an HTTP secret's uses in its environment, in words: "1
// service · 2 sandboxes", "1 binding in the spec", "unused". usedBy's
// objects are "kind/name"; its bindings those of the spec that name it
// (credentialRef, from before ADR 0055).
export function usedWords(usedBy) {
  const objs = ((usedBy && usedBy.objects) || []).map((o) => String(o).split('/')[0]);
  const n = (k) => objs.filter((x) => x === k).length;
  const s = ((usedBy && usedBy.sandboxes) || []).length;
  const b = ((usedBy && usedBy.bindings) || []).length;
  const parts = [];
  if (n('service')) parts.push(`${n('service')} service${n('service') === 1 ? '' : 's'}`);
  if (n('job')) parts.push(`${n('job')} job${n('job') === 1 ? '' : 's'}`);
  if (s) parts.push(`${s} sandbox${s === 1 ? '' : 'es'}`);
  if (b) parts.push(`${b} binding${b === 1 ? '' : 's'} in the spec`);
  return parts.join(' · ') || 'unused';
}

// pinnedSpec is a deployment's spec with HTTP secret name moved to
// version: its pin (the spec's secrets, for the variables that reference
// it) and every binding of the spec that names it (credentialRef, from
// before ADR 0055): {spec, moved (whether anything moved), bindings
// (their names)}; the spec is a copy. The pin is the version alone: the
// apply puts that version's upstream and mode beside it; a binding keeps
// nothing else of its own either.
export function pinnedSpec(spec, name, version) {
  const out = JSON.parse(JSON.stringify(spec || {}));
  const names = [];
  for (const [k, b] of Object.entries(out.bindings || {})) {
    if (b && b.credentialRef && b.credentialRef.name === name) {
      out.bindings[k] = { credentialRef: { name, version } };
      names.push(k);
    }
  }
  let pinned = false;
  if (out.secrets && Object.prototype.hasOwnProperty.call(out.secrets, name)) {
    out.secrets[name] = { version };
    pinned = true;
  }
  return { spec: out, moved: pinned || names.length > 0, bindings: names.sort() };
}

// specSnippet is the variable a service gets HTTP secret name by: its
// URL, which the code calls.
export function specSnippet(name, service = 'api') {
  const j = (v) => JSON.stringify(v).replace(/([{:,])(?=[^\s])/g, '$1 ').replace(/(?<=[^\s])}/g, ' }');
  const variable = `${name.toUpperCase().replace(/[^A-Z0-9]+/g, '_')}_URL`;
  return `"services": ${j({ [service]: { env: { [variable]: { secret: name, key: 'url' } } } })}`;
}

// preflightRows are a preflight's answer as rows: what was checked, ok
// or not, in words; probe the request it sent, if any.
export function preflightRows(r, probe = null) {
  if (!r) return [];
  const rows = [];
  const addrs = r.addresses || [];
  if (addrs.length) rows.push({ key: 'addresses', ok: true, text: addrs.join(' · ') });
  if (r.tls) {
    const until = r.tls.notAfter ? String(r.tls.notAfter).slice(0, 10) : '';
    rows.push({ key: 'TLS', ok: true, text: [r.tls.subject, r.tls.issuer, until && `until ${until}`].filter(Boolean).join(' · ') });
  }
  if (r.token) {
    rows.push({ key: 'token', ok: true, text: r.token.expiresIn ? `fetched · expires in ${r.token.expiresIn}s` : 'fetched · no lifetime given' });
  }
  if (r.probe) {
    const good = r.probe.status >= 200 && r.probe.status < 400;
    const req = probe && probe.path ? `${probe.method || 'GET'} ${probe.path} ` : '';
    rows.push({ key: 'probe', ok: good, text: `${req}→ ${r.probe.status} in ${r.probe.latencyMs} ms` });
  }
  if (r.error) rows.push({ key: r.error.code, ok: false, text: r.error.message });
  return rows;
}
