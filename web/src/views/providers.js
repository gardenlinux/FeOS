// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// The tenant's access providers (ADR 0054): who vouches for the visitors
// of an environment that admits visitors. A provider of kind tenant is
// the tenant's own service at an address, one of kind oidc an issuer
// that ferun-auth signs people in with, under a rule over their claims;
// ferun is every tenant's without being made, and admits the people with
// a grant on the environment. The form's fields to the provider's JSON
// and back, the words the pages use, and which provider a publish may
// name. Pure, with no React or browser imports, so that the tests load
// it as it is.

// FERUN is the built-in provider's name, which no tenant makes.
export const FERUN = 'ferun';

// KINDS are the kinds a tenant makes, as the form offers them.
export const KINDS = [
  { key: 'tenant', label: 'your own service' },
  { key: 'oidc', label: 'OpenID Connect' },
];

// kindLabel names a kind, ferun's too.
export function kindLabel(kind) {
  if (kind === 'ferun') return 'FeRun sign-in';
  const k = KINDS.find((x) => x.key === kind);
  return k ? k.label : kind || '';
}

// RULES are an oidc provider's rules over the ID token's claims.
export const RULES = [
  { key: 'anyone', label: 'everyone' },
  { key: 'emails', label: 'addresses' },
  { key: 'domains', label: 'email domains' },
  { key: 'groups', label: 'groups' },
];

// MAX_VALUES is how many addresses, domains or groups a rule holds.
export const MAX_VALUES = 100;

// nameRe is a provider's name: a DNS label.
const nameRe = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;

// nameProblem says what is wrong with a new provider's name, '' when
// nothing is.
export function nameProblem(s) {
  const n = String(s || '').trim();
  if (!nameRe.test(n)) return 'the name must be a DNS label: lowercase letters, digits and "-", at most 63';
  if (n === FERUN) return 'ferun is every tenant\'s provider and cannot be made';
  return '';
}

// providerPath is the providers' page, or one provider's, or its edit
// form.
export function providerPath(tenant, name = '', edit = false) {
  const base = `/tenants/${encodeURIComponent(tenant)}/providers`;
  if (!name) return base;
  return `${base}/${encodeURIComponent(name)}${edit ? '/edit' : ''}`;
}

// newProviderPath is the form of a new provider.
export function newProviderPath(tenant) {
  return `/tenants/${encodeURIComponent(tenant)}/new-provider`;
}

// emptyForm is a new provider's form.
export function emptyForm() {
  return {
    name: '', kind: 'tenant', url: '', issuer: '', clientId: '', clientSecret: '', rule: 'anyone', values: '',
    // secretSet: the stored provider has a client secret, which an
    // empty field keeps.
    secretSet: false,
  };
}

// ruleKey is which of RULES a rule is ('anyone' for none).
export function ruleKey(rule) {
  const r = rule || {};
  for (const k of ['emails', 'domains', 'groups']) if (Array.isArray(r[k]) && r[k].length) return k;
  return 'anyone';
}

// formOf is the form of a provider as a read answers it: the client
// secret empty and marked set.
export function formOf(p) {
  const f = emptyForm();
  const key = ruleKey(p.rule);
  return {
    ...f,
    name: p.name || '', kind: p.kind === 'oidc' ? 'oidc' : 'tenant', url: p.url || '', issuer: p.issuer || '',
    clientId: p.clientId || '', rule: key, values: key === 'anyone' ? '' : (p.rule[key] || []).join(' '),
    secretSet: !!(p.clientSecret && p.clientSecret.set),
  };
}

// valuesOf splits a rule's field at blanks and commas.
export function valuesOf(s) {
  return String(s || '').split(/[\s,]+/).filter(Boolean);
}

// httpsURL tells an absolute https URL with a host.
function httpsURL(s) {
  try {
    const u = new URL(s);
    return u.protocol === 'https:' && !!u.hostname && !u.username && !u.password && !u.hash;
  } catch {
    return false;
  }
}

// emailRe is an address as the rule takes it: something, one @, a domain.
const emailRe = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;
// domainRe is a DNS name.
const domainRe = /^(?=.{1,253}$)([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;

// ruleOf is the rule's JSON of a form: exactly one of anyone, emails,
// domains, groups. It throws on a rule no request should carry.
export function ruleOf(f) {
  if (f.rule === 'anyone') return { anyone: true };
  const values = valuesOf(f.values);
  const what = { emails: 'address', domains: 'domain', groups: 'group' }[f.rule];
  if (!what) throw new Error('choose who the issuer\'s people must be');
  if (!values.length) throw new Error(`name at least one ${what}`);
  if (values.length > MAX_VALUES) throw new Error(`a rule names at most ${MAX_VALUES} values`);
  if (f.rule === 'emails') {
    const list = values.map((v) => v.toLowerCase());
    const bad = list.find((v) => !emailRe.test(v));
    if (bad) throw new Error(`${bad} is not an email address`);
    return { emails: list };
  }
  if (f.rule === 'domains') {
    const list = values.map((v) => v.toLowerCase());
    const bad = list.find((v) => v.includes('@') || !domainRe.test(v));
    if (bad) throw new Error(`${bad} is not a domain: name it without the @, such as example.com`);
    return { domains: list };
  }
  const bad = values.find((v) => v.length > 255);
  if (bad) throw new Error('a group\'s name is at most 255 characters');
  return { groups: values };
}

// providerOf is a form as the provider's JSON (PUT /api/v1/providers/
// {name}): an empty client secret the stored provider has is {"set":
// true}, which keeps it. It throws on a form no request should carry.
export function providerOf(f) {
  if (f.kind === 'tenant') {
    const url = String(f.url || '').trim();
    if (!url) throw new Error('the address is required: https://host/path');
    if (!httpsURL(url)) throw new Error('the address must be an https URL with a host, without user or fragment');
    if (url.length > 2048) throw new Error('the address is at most 2048 bytes');
    return { kind: 'tenant', url };
  }
  if (f.kind !== 'oidc') throw new Error('choose a kind');
  const issuer = String(f.issuer || '').trim();
  if (!issuer) throw new Error('the issuer is required: https://login.example.com');
  if (!httpsURL(issuer)) throw new Error('the issuer must be an https URL');
  const clientId = String(f.clientId || '').trim();
  if (!clientId) throw new Error('the client ID is required');
  if (clientId.length > 255) throw new Error('the client ID is at most 255 characters');
  let clientSecret;
  if (f.clientSecret) clientSecret = f.clientSecret;
  else if (f.secretSet) clientSecret = { set: true };
  else throw new Error('the client secret is required');
  return { kind: 'oidc', issuer, clientId, clientSecret, rule: ruleOf(f) };
}

// list says up to three values and how many more.
function list(values) {
  const v = values || [];
  return v.length > 3 ? `${v.slice(0, 3).join(', ')} and ${v.length - 3} more` : v.join(', ');
}

// ruleWords says an oidc provider's rule.
export function ruleWords(rule) {
  const key = ruleKey(rule);
  if (key === 'emails') return `the addresses ${list(rule.emails)}, verified`;
  if (key === 'domains') return `verified addresses at ${list(rule.domains)}`;
  if (key === 'groups') return `members of ${list(rule.groups)}`;
  return 'everyone the issuer signs in';
}

// providerWords says whom a provider admits, in one line.
export function providerWords(p) {
  if (!p) return '';
  if (p.kind === 'ferun') return 'people with a grant on the environment, signed in by this installation';
  if (p.kind === 'tenant') return p.url ? `whom ${p.url} admits` : 'whom the tenant\'s service admits';
  if (p.kind === 'oidc') return `${ruleWords(p.rule)}, signed in at ${p.issuer || 'the issuer'}`;
  return '';
}

// whereOf is where a provider is: its address, its issuer, or the
// installation's sign-in.
export function whereOf(p) {
  if (!p) return '';
  if (p.kind === 'tenant') return p.url || '';
  if (p.kind === 'oidc') return p.issuer || '';
  return 'this installation';
}

// served tells whether this installation serves a provider's kind
// (kinds from the list's answer; null: not known, so it is offered and
// ferund decides).
export function served(kind, kinds) {
  if (!kinds) return true;
  return kinds.includes(kind);
}

// usedWords says which environments name a provider.
export function usedWords(p) {
  const n = ((p && p.environments) || []).length;
  if (!n) return 'no environment';
  return n === 1 ? '1 environment' : `${n} environments`;
}

// choices are the providers a publish may name, as the select offers
// them: ferun first, then the tenant's by name, each {name, kind,
// served}; the environment's current one is among them even when the
// list is not readable (null), with ferun.
export function choices(providers, kinds, current = '') {
  const out = [];
  const seen = new Set();
  const add = (name, kind) => {
    if (!name || seen.has(name)) return;
    seen.add(name);
    out.push({ name, kind: kind || '', served: kind ? served(kind, kinds) : true });
  };
  const listed = providers || [];
  const ferun = listed.find((p) => p.name === FERUN);
  add(FERUN, (ferun && ferun.kind) || 'ferun');
  for (const p of [...listed].filter((x) => x.name !== FERUN).sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0))) {
    add(p.name, p.kind);
  }
  add(current, '');
  return out;
}

// NO_VISITS says why an installation offers only anyone.
export const NO_VISITS = 'this installation guards no environments: ferund runs without --secret-key-file';

// guardsNone tells an installation known to guard no environment: its
// ferund has no providers' routes (kinds [], as the summary reads a 404
// no such route); null, not read yet, is not known.
export function guardsNone(kinds) {
  return !!kinds && kinds.length === 0;
}

// publishChoice is what the publish box starts with: a public
// environment's access as it is (anyone when the ferund says none), and
// for one not public yet visitors of ferun where the installation
// serves it, anyone where it guards nothing, else nothing: its admin
// chooses.
export function publishChoice(st, kinds) {
  if (st && st.public) {
    if (st.access === 'visitors') return { access: 'visitors', provider: st.provider || FERUN };
    return { access: 'anyone', provider: '' };
  }
  if (kinds && kinds.includes('ferun')) return { access: 'visitors', provider: FERUN };
  if (guardsNone(kinds)) return { access: 'anyone', provider: '' };
  return { access: '', provider: '' };
}

// hostLabelChoice is the access a public environment's new host label is
// published with: the one its status says, and none when the status says
// none (an older ferund answered it), so that ferund keeps the stored one
// rather than open a guarded environment to anyone.
export function hostLabelChoice(st, current) {
  if (!st || (st.access !== 'visitors' && st.access !== 'anyone')) return { access: '', provider: '' };
  return current;
}

// sameChoice tells two choices apart.
export function sameChoice(a, b) {
  return a.access === b.access && (a.access !== 'visitors' || a.provider === b.provider);
}

// inUseProblem says a delete refused because environments name the
// provider (409 provider_in_use), '' for any other error.
export function inUseProblem(e) {
  if (!e || e.status !== 409 || !e.body || e.body.code !== 'provider_in_use') return '';
  return `${e.message}. Publish them with another provider, or open them to anyone, first: an environment's provider cannot go while it guards it.`;
}
