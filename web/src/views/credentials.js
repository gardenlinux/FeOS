// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// The tenant's credentials (ADR 0033): an HTTP binding's definition
// (ADR 0028) stated once, versioned, used by name by sandboxes and by
// environments' bindings. The form's fields to the binding's JSON and
// back, the secret fields a write keeps ({"set": true}), what the code
// runs, and the environments whose pin is behind the latest.

// KINDS are a credential's kinds, as the form offers them.
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

// nameRe is a credential's name: a DNS label.
const nameRe = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;

// isName tells a credential's name.
export function isName(s) {
  return nameRe.test(String(s || ''));
}

// emptyForm is a new credential's form.
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

// formOf is the form of a credential as a read answers it: its secret
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

// bindingOf is a form as the binding's JSON (PUT /api/v1/credentials/
// {name}, the preflight's binding): a secret field left empty that the
// latest version has is {"set": true} when keep, else left out. It throws
// on a form no request should carry.
export function bindingOf(f, { keep = true } = {}) {
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

// sameDefinition tells whether a form states a credential's definition
// with every secret kept: then the stored credential's check is the
// form's preflight.
export function sameDefinition(f, c) {
  if (!c) return false;
  let a;
  let b;
  try {
    a = bindingOf(f);
    b = bindingOf(formOf(c));
  } catch {
    return false;
  }
  return JSON.stringify(a) === JSON.stringify(b);
}

// preflightOf decides how a form is checked before a save: the form's
// binding with every value in it (form), the stored credential's check
// when only kept secrets are missing and nothing else changed (stored),
// or none: the kept fields' values are needed (needs, their paths).
export function preflightOf(f, c) {
  const kept = keptOf(f);
  if (!kept.length) return { how: 'form', binding: bindingOf(f, { keep: false }) };
  if (sameDefinition(f, c)) return { how: 'stored' };
  return { how: 'needs', fields: kept };
}

// hostOf is an upstream's host and port ("api.github.com",
// "api.example.com:8443"); '' for no URL.
export function hostOf(upstream) {
  const m = /^[a-z]+:\/\/([^/?#]+)/i.exec(String(upstream || '').trim());
  return m ? m[1].toLowerCase().replace(/:443$/, '') : '';
}

// modeWords is how the code reaches a credential: "http://github/…" or
// "intercepts api.github.com".
export function modeWords(c) {
  if (c.mode === 'intercept') return `intercepts ${hostOf(c.upstream) || 'the upstream\'s host'}`;
  return `http://${c.name || '<name>'}/…`;
}

// curlOf is the request the code runs: by the name, or at the upstream's
// own host, at the first path prefix.
export function curlOf(c) {
  const prefixes = Array.isArray(c.pathPrefixes) ? c.pathPrefixes : prefixesOf(c.pathPrefixes);
  const path = prefixes[0] || '/';
  if (c.mode === 'intercept') return `curl -s https://${hostOf(c.upstream) || '<host>'}${path}`;
  return `curl -s http://${c.name || '<name>'}${path}`;
}

// behind tells whether an environment's use pins a version before the
// latest, in its latest or its deployed deployment (0: that one pins
// none).
export function behind(use, version) {
  return (Number(use.latest) > 0 && Number(use.latest) < version) || (Number(use.deployed) > 0 && Number(use.deployed) < version);
}

// needsDeployment tells whether an environment's latest deployment pins
// a version before the latest: a new deployment would move it. One whose
// latest deployment pins the latest but whose deployed one does not is
// on its way.
export function needsDeployment(use, version) {
  return Number(use.latest) > 0 && Number(use.latest) < version;
}

// pinState is an environment's pins against the latest version, in a
// word: behind, rolling out (its latest deployment has it, the deployed
// one not yet) or current.
export function pinState(use, version) {
  if (needsDeployment(use, version)) return 'behind';
  if (behind(use, version)) return 'rolling out';
  return 'current';
}

// pinWords is what an environment's deployments pin, in words.
export function pinWords(use) {
  const l = Number(use.latest) || 0;
  const d = Number(use.deployed) || 0;
  if (l && d && l === d) return `v${l}`;
  const parts = [];
  if (l) parts.push(`latest deployment v${l}`);
  if (d) parts.push(`deployed v${d}`);
  return parts.join(' · ') || 'no version';
}

// oldestPin is the oldest version an environment pins, for the list's
// "on vN".
export function oldestPin(use) {
  const vs = [Number(use.latest) || 0, Number(use.deployed) || 0].filter((v) => v > 0);
  return vs.length ? Math.min(...vs) : 0;
}

// usedWords is a credential's uses in words: "2 sandboxes · 1
// environment", "unused".
export function usedWords(usedBy) {
  const s = ((usedBy && usedBy.sandboxes) || []).length;
  const e = ((usedBy && usedBy.environments) || []).length;
  const parts = [];
  if (s) parts.push(`${s} sandbox${s === 1 ? '' : 'es'}`);
  if (e) parts.push(`${e} environment${e === 1 ? '' : 's'}`);
  return parts.join(' · ') || 'unused';
}

// pinnedSpec is a deployment's spec with every binding that names
// credential name moved to version: {spec, bindings (their names)}; the
// spec is a copy. A binding keeps nothing else of its own: the
// credential's version brings its upstream and mode.
export function pinnedSpec(spec, name, version) {
  const out = JSON.parse(JSON.stringify(spec || {}));
  const names = [];
  for (const [k, b] of Object.entries(out.bindings || {})) {
    if (b && b.tenantCredential && b.tenantCredential.name === name) {
      out.bindings[k] = { tenantCredential: { name, version } };
      names.push(k);
    }
  }
  return { spec: out, bindings: names.sort() };
}

// specSnippet is the binding an environment's spec names a credential
// with, and a service that gets it: the spec's two places, one line each.
export function specSnippet(name, service = 'api') {
  const j = (v) => JSON.stringify(v).replace(/([{:,])(?=[^\s])/g, '$1 ').replace(/(?<=[^\s])}/g, ' }');
  return `"bindings": ${j({ [name]: { tenantCredential: { name } } })}\n` +
    `"services": ${j({ [service]: { bindings: [name] } })}`;
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

// credentialPath is the credentials' page, or one credential's, or its
// edit form.
export function credentialPath(tenant, name = '', edit = false) {
  const base = `/tenants/${encodeURIComponent(tenant)}/credentials`;
  if (!name) return base;
  return `${base}/${encodeURIComponent(name)}${edit ? '/edit' : ''}`;
}

// newCredentialPath is the form of a new credential.
export function newCredentialPath(tenant) {
  return `/tenants/${encodeURIComponent(tenant)}/new-credential`;
}
