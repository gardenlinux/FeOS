// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// The domains' words (ADR 0021): a domain's state as a dot, a word and
// a sentence with its deadline, the way it goes (claimed → verified →
// bound → routed → certificate → served) derived from the answer, what
// DNS says now against the records to set, and who may bind it. The
// states are domains.Derive's (go/internal/domains/state.go).

import { atLeast, envRole } from '../people.js';
import { inWords } from '../attention.js';
import { fmtDate } from '../format.js';

// STATES are domains.Derive's states, in its rules' order.
export const STATES = [
  'unverified', 'lapsed', 'superseded', 'unbound', 'unpublished', 'inactive',
  'dns-moved', 'waiting-for-dns', 'served', 'failed', 'issuing',
];

// reasons of domains.Derive that the words tell apart.
const REASON_ENV_DELETING = 'the environment is being deleted';

// domainDot is the class of a state's dot: served filled green, issuing
// on its way, waiting for someone amber, not in use grey, deactivated or
// failed red.
export function domainDot(state) {
  switch (state) {
    case 'served': return 'running';
    case 'issuing': return 'moving';
    case 'unverified': case 'waiting-for-dns': case 'dns-moved': return 'unknown';
    case 'unbound': case 'unpublished': return 'absent';
    case 'lapsed': case 'superseded': case 'inactive': case 'failed': return 'failed';
    default: return 'absent';
  }
}

// STATE_WORDS say a state in a few words.
const STATE_WORDS = {
  unverified: 'waiting for its TXT record',
  lapsed: 'claim lapsed: its TXT record went missing',
  superseded: "another claim's token took the name",
  unbound: 'verified, not bound to an environment',
  unpublished: 'its environment is not public',
  inactive: 'deactivated: the name resolves elsewhere',
  'dns-moved': 'resolves elsewhere; served until the grace ends',
  'waiting-for-dns': 'waiting for the name to point to the edge',
  served: '',
  failed: 'its certificate could not be ordered',
  issuing: 'ordering its certificate',
};

function ms(iso) {
  const t = Date.parse(iso || '');
  return Number.isNaN(t) ? null : t;
}

// stateWords says a domain's state in a few words.
export function stateWords(d) {
  if (d.state === 'unpublished' && d.reason === REASON_ENV_DELETING) return 'its environment is being deleted';
  return d.state in STATE_WORDS ? STATE_WORDS[d.state] : (d.state || 'unknown');
}

// graceEnd says what happens when the running grace ends: the TXT
// grace lapses the claim, the routing grace deactivates the domain; the
// earlier one's start decides (deactivatesAt is the earlier end).
export function graceEnd(w) {
  const txt = ms(w && w.recordMissingSince);
  const dns = ms(w && w.dnsMovedSince);
  if (txt === null && dns === null) return '';
  if (dns === null || (txt !== null && txt <= dns)) return 'the claim lapses';
  return 'the domain turns inactive';
}

// stateLine is the list's sentence of a domain: its state's words and
// what is wrong while it is still served, each with its deadline.
export function stateLine(d, now = Date.now()) {
  const w = d.warnings || {};
  const parts = [stateWords(d)].filter(Boolean);
  const deact = ms(w.deactivatesAt);
  if (w.recordMissingSince && d.state !== 'lapsed') parts.push(`TXT record missing since ${fmtDate(w.recordMissingSince)}`);
  if (w.dnsMovedSince && d.state !== 'inactive' && d.state !== 'dns-moved') parts.push(`resolves elsewhere since ${fmtDate(w.dnsMovedSince)}`);
  if (deact !== null && d.state !== 'lapsed' && d.state !== 'inactive') {
    parts.push(deact > now ? `${graceEnd(w).replace(/^the (claim|domain) /, '')} ${inWords(deact - now)}` : 'deactivated');
  }
  if (w.certificateFailing) parts.push('renewing its certificate fails');
  if (w.checkFailing) parts.push('its lookups fail');
  return parts.join(' · ');
}

// routing is what DNS says now of the name's routing: whether it points
// to the edge (every address an edge address, or the CNAME chain ending
// at the edge's name) and what it shows.
export function routing(d) {
  const rec = d.records || {};
  const found = d.found || {};
  const edge = new Set(rec.addresses || []);
  const chain = found.cname || [];
  const addrs = found.addresses || [];
  const toEdgeName = !!rec.cname && chain.length > 0 && chain[chain.length - 1] === rec.cname;
  const toEdgeAddrs = addrs.length > 0 && edge.size > 0 && addrs.every((a) => edge.has(a));
  const ok = toEdgeName || toEdgeAddrs;
  let shows = '';
  if (chain.length > 0) shows = 'CNAME ' + chain.join(' → ');
  else if (addrs.length > 0) shows = addrs.join(', ');
  return { ok, shows, chain, addresses: addrs };
}

// dnsNow is the records table's "DNS says now" of the TXT and the
// routing record: {ok: true|false|null (not looked up yet), text}.
export function dnsNow(d) {
  const rec = d.records || {};
  const found = d.found || {};
  if (!d.checkedAt) return { txt: { ok: null, text: 'not checked yet' }, route: { ok: null, text: 'not checked yet' } };
  const txts = found.txt || [];
  let txt;
  if (rec.txt && txts.includes(rec.txt.value)) txt = { ok: true, text: 'found' };
  else if (txts.length > 0) txt = { ok: false, text: 'other: ' + txts.join(', ') };
  else txt = { ok: false, text: 'nothing' };
  const r = routing(d);
  const route = r.ok ? { ok: true, text: r.shows ? r.shows.replace(/^CNAME /, '') : 'the edge' }
    : { ok: false, text: r.shows || 'nothing' };
  return { txt, route };
}

// STEPS are the stations of a domain, in the order it passes them.
export const STEPS = ['claimed', 'verified', 'bound', 'routed', 'certificate', 'served'];

// certValid reports whether the domain has a certificate whose notAfter
// is ahead.
export function certValid(d, now = Date.now()) {
  const na = ms(d.certificate && d.certificate.notAfter);
  return na !== null && na > now;
}

// domainSteps derives the stations from a domain's state and answer:
// [{key, status, sub}], status done (passed), wait (now, on its way),
// warn (now, needs someone), bad (failed there) or todo. envName is its
// environment's name ('' unbound).
export function domainSteps(d, envName = '', now = Date.now()) {
  const st = d.state;
  const w = d.warnings || {};
  const s = {};
  const set = (key, status, sub) => { s[key] = { key, status, sub }; };
  const env = envName || 'its environment';
  const bound = !!d.environment;
  const r = routing(d);
  const cert = certValid(d, now);

  set('claimed', 'done', 'token issued');

  // verified
  if (st === 'unverified') set('verified', 'wait', 'waiting for the TXT record');
  else if (st === 'lapsed') set('verified', 'bad', 'TXT record missing');
  else if (st === 'superseded') set('verified', 'bad', 'another claim took the name');
  else if (w.recordMissingSince) set('verified', 'warn', 'TXT record missing');
  else set('verified', 'done', 'TXT found');

  // bound
  if (st === 'superseded') set('bound', 'todo', 'cannot be bound');
  else if (st === 'unbound') set('bound', 'warn', 'to no environment');
  else if (st === 'unpublished') {
    set('bound', 'warn', d.reason === REASON_ENV_DELETING ? `${env} is being deleted` : `${env} is not public`);
  } else if (bound) set('bound', 'done', `to ${env}`);
  else set('bound', 'todo', 'to an environment');

  // routed, certificate, served: only past the binding
  const past = ['inactive', 'dns-moved', 'waiting-for-dns', 'served', 'failed', 'issuing'].includes(st);
  if (!past) {
    set('routed', r.ok && st !== 'superseded' ? 'done' : 'todo', r.ok && st !== 'superseded' ? 'points to the edge' : 'to the edge');
    set('certificate', 'todo', 'orders once routed');
    set('served', 'todo', 'at the ingress');
  } else if (st === 'inactive') {
    set('routed', 'bad', 'resolved elsewhere past the grace');
    set('certificate', cert ? 'done' : 'todo', cert ? 'valid, not renewed' : 'orders once routed');
    set('served', 'todo', 'no longer');
  } else if (st === 'dns-moved') {
    set('routed', 'warn', r.shows ? `resolves to ${r.shows}` : 'resolves elsewhere');
    set('certificate', 'warn', 'valid, not renewed');
    set('served', 'done', 'until the grace ends');
  } else if (st === 'waiting-for-dns') {
    set('routed', 'warn', r.chain.length ? `CNAME points to ${r.chain[r.chain.length - 1]}`
      : r.addresses.length ? `resolves to ${r.addresses.join(', ')}` : 'no record yet');
    set('certificate', cert ? 'done' : 'todo', cert ? 'valid' : 'orders once routed');
    set('served', 'todo', 'at the ingress');
  } else {
    set('routed', 'done', 'points to the edge');
    if (st === 'issuing') set('certificate', 'wait', 'ordering (HTTP-01)');
    else if (st === 'failed') set('certificate', 'bad', 'order failed');
    else if (w.certificateFailing) set('certificate', 'warn', 'renewal failing');
    else set('certificate', 'done', 'valid, renewed automatically');
    set('served', st === 'served' ? 'done' : 'todo', 'at the ingress');
  }
  return STEPS.map((k) => s[k]);
}

// callout is the sentence of a domain's current problem, with its
// deadline: {tone: 'warn'|'bad'|'info', text, mono: the names in text
// to set in mono}, or null when nothing is wrong. envName as in
// domainSteps.
export function callout(d, envName = '', now = Date.now()) {
  const w = d.warnings || {};
  const rec = d.records || {};
  const txt = rec.txt || {};
  const env = envName || 'its environment';
  const r = routing(d);
  const deact = ms(w.deactivatesAt);
  const graceLeft = deact !== null && deact > now ? `The grace ends ${inWords(deact - now)}, then ${graceEnd(w)}.` : '';
  const next = ms(d.nextCheckAt);
  const nextCheck = next !== null ? (next > now ? `FeRun looks again ${inWords(next - now)}.` : 'FeRun looks again now.') : '';
  const out = (tone, ...parts) => {
    if (w.checkFailing) parts.push(`The last lookup failed: ${w.checkFailing}; what it found before stays.`);
    const mono = [d.name, txt.name, txt.value, rec.cname, envName, ...r.chain, ...r.addresses].filter(Boolean);
    return { tone, text: parts.filter(Boolean).join(' '), mono: [...new Set(mono)] };
  };
  switch (d.state) {
    case 'unverified':
      return out('warn', `Set the TXT record ${txt.name} to ${txt.value} at your DNS provider to prove the claim.`, nextCheck);
    case 'lapsed':
      return out('bad', `The TXT record with the claim's token was missing for longer than the grace, so the claim lapsed and ${d.name} is not served.`,
        `Put ${txt.name} back to verify it again.`);
    case 'superseded':
      return out('bad', `Another claim's token took ${d.name}: this claim is no longer checked and cannot be bound. Delete it and claim the name anew.`);
    case 'unbound':
      return out('info', `${d.name} is verified but bound to no environment. Bind it to serve an environment at it.`);
    case 'unpublished':
      if (d.reason === REASON_ENV_DELETING) return out('info', `${env} is being deleted; its domains are unbound with it.`);
      return out('info', `${env} is not public, so ${d.name} is not served. Publish the environment to serve it.`);
    case 'inactive':
      return out('bad', `${d.name} resolved elsewhere than the edge for longer than the grace; it is no longer served.`,
        `Point it to the edge again: a CNAME to ${rec.cname}.`);
    case 'dns-moved':
      return out('warn', `${d.name} resolves ${r.shows ? 'to ' + r.shows : 'elsewhere'}${w.dnsMovedSince ? ' since ' + fmtDate(w.dnsMovedSince) : ''}, not to the edge.`,
        'It is served with its certificate until the grace ends, and its certificate is not renewed meanwhile.', graceLeft);
    case 'waiting-for-dns': {
      const shows = r.shows ? (r.chain.length ? `The CNAME of ${d.name} points to ${r.chain[r.chain.length - 1]}` : `${d.name} resolves to ${r.shows}`)
        : `${d.name} does not resolve yet`;
      return out('warn', `${shows}${w.dnsMovedSince ? ' since ' + fmtDate(w.dnsMovedSince) : ''}.`,
        `FeRun can't order a certificate until it points to the edge: a CNAME to ${rec.cname}.`, graceLeft || nextCheck);
    }
    case 'issuing':
      return out('info', `${d.name} points to the edge; FeRun is ordering its certificate.`,
        `It is served once the certificate is there; until then ${env} answers at its default host.`);
    case 'failed':
      return out('bad', `Ordering the certificate failed: ${d.reason}.`, 'FeRun orders again after its backoff.');
    case 'served': {
      if (w.recordMissingSince) {
        return out('warn', `The TXT record with the claim's token is missing since ${fmtDate(w.recordMissingSince)}. Put ${txt.name} back.`, graceLeft);
      }
      if (w.certificateFailing) {
        const na = d.certificate && d.certificate.notAfter;
        return out('warn', `Renewing the certificate fails: ${w.certificateFailing}.`, na ? `The current one is valid until ${fmtDate(na)}.` : '');
      }
      if (w.checkFailing) return out('warn');
      return null;
    }
    default:
      return d.reason ? out('info', d.reason) : null;
  }
}

// pickDomain is the domain a page's segment names: by its ID, else by
// its name (several claims may name one name: the verified one, else the
// newest).
export function pickDomain(list, sel) {
  const ds = list || [];
  if (!sel) return null;
  const byId = ds.find((d) => d.id === sel);
  if (byId) return byId;
  const named = ds.filter((d) => d.name === sel);
  if (named.length === 0) return null;
  return named.find((d) => d.verification === 'verified')
    || [...named].sort((a, b) => (b.createdAt || '').localeCompare(a.createdAt || ''))[0];
}

// mayBind tells whether access may move a domain from the environment
// from (its ID, '' or null: none) to the one to ('' for unbinding):
// admin on each (the tenant's admin has it on all).
export function mayBind(access, from, to) {
  if (from && !atLeast(envRole(access, from), 'admin')) return false;
  if (to && !atLeast(envRole(access, to), 'admin')) return false;
  return true;
}

// isServed reports whether a domain is served: the state in which its
// environment's default host answers 404 (ADR 0021, "An environment on
// a domain is not served at its default host").
export function isServed(d) {
  return d.state === 'served' || d.state === 'dns-moved';
}

// monoSplit splits text into [{text, mono}] at the given names, whole
// ones only (not "prod" in "product"), the longest first, for setting
// them in mono.
export function monoSplit(text, names) {
  const ns = [...new Set((names || []).filter(Boolean))].sort((a, b) => b.length - a.length);
  if (ns.length === 0) return [{ text, mono: false }];
  const alt = ns.map((n) => n.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('|');
  const re = new RegExp(`(?<![\\w.-])(${alt})(?![\\w-]|\\.\\w)`);
  return text.split(re).filter((p) => p !== '').map((p) => ({ text: p, mono: ns.includes(p) }));
}
