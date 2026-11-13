// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import { ApiError } from '../api.js';
import {
  FERUN, KINDS, choices, emptyForm, formOf, inUseProblem, kindLabel, nameProblem, newProviderPath, providerOf, providerPath,
  guardsNone, hostLabelChoice, providerWords, publishChoice, ruleKey, ruleOf, ruleWords, sameChoice, served, usedWords, valuesOf, whereOf,
} from './providers.js';

// The access providers' arithmetic (ADR 0054), on providers as ferund
// answers them (control-plane.md, "Visitors").

const FERUN_P = { name: 'ferun', kind: 'ferun', builtIn: true, environments: [{ id: 'e1', name: 'stage' }] };
const FECODE = {
  name: 'fecode', kind: 'tenant', builtIn: false, url: 'https://fecode.dev/api/v1/visit',
  environments: [{ id: 'e2', name: 'kino--stage' }, { id: 'e3', name: 'kino--prod' }], createdAt: '2026-11-12T20:00:00Z',
};
const SSO = {
  name: 'sso', kind: 'oidc', builtIn: false, issuer: 'https://login.example.com/', clientId: 'abc', clientSecret: { set: true },
  rule: { domains: ['example.com'] }, backchannelUri: 'https://auth.fecore.dev/backchannel/acme/sso', environments: [],
};

describe('a provider', () => {
  it('has a kind of its tenant, or is ferun, built in', () => {
    expect(KINDS.map((k) => k.key)).toEqual(['tenant', 'oidc']);
    expect(kindLabel('tenant')).toBe('your own service');
    expect(kindLabel('oidc')).toBe('OpenID Connect');
    expect(kindLabel('ferun')).toBe('FeRun sign-in');
    expect(kindLabel('other')).toBe('other');
    expect(FERUN).toBe('ferun');
  });

  it('is named by a DNS label, never ferun', () => {
    expect(nameProblem('members')).toBe('');
    expect(nameProblem(' sso-2 ')).toBe('');
    expect(nameProblem('ferun')).toBe('ferun is every tenant\'s provider and cannot be made');
    expect(nameProblem('Sso')).toContain('DNS label');
    expect(nameProblem('-a')).toContain('DNS label');
    expect(nameProblem('a'.repeat(64))).toContain('DNS label');
    expect(nameProblem('')).toContain('DNS label');
  });

  it('has its pages', () => {
    expect(providerPath('acme')).toBe('/tenants/acme/providers');
    expect(providerPath('acme', 'sso')).toBe('/tenants/acme/providers/sso');
    expect(providerPath('a b', 'sso', true)).toBe('/tenants/a%20b/providers/sso/edit');
    expect(newProviderPath('acme')).toBe('/tenants/acme/new-provider');
  });

  it('says whom it admits, where it is, and who names it', () => {
    expect(providerWords(FERUN_P)).toBe('people with a grant on the environment, signed in by this installation');
    expect(providerWords(FECODE)).toBe('whom https://fecode.dev/api/v1/visit admits');
    expect(providerWords(SSO)).toBe('verified addresses at example.com, signed in at https://login.example.com/');
    expect(providerWords(null)).toBe('');
    expect(whereOf(FECODE)).toBe('https://fecode.dev/api/v1/visit');
    expect(whereOf(SSO)).toBe('https://login.example.com/');
    expect(whereOf(FERUN_P)).toBe('this installation');
    expect(usedWords(FECODE)).toBe('2 environments');
    expect(usedWords(FERUN_P)).toBe('1 environment');
    expect(usedWords(SSO)).toBe('no environment');
  });

  it('says a rule in words', () => {
    expect(ruleWords({ anyone: true })).toBe('everyone the issuer signs in');
    expect(ruleWords({ emails: ['a@example.com'] })).toBe('the addresses a@example.com, verified');
    expect(ruleWords({ groups: ['a', 'b', 'c', 'd', 'e'] })).toBe('members of a, b, c and 2 more');
    expect(ruleWords(undefined)).toBe('everyone the issuer signs in');
    expect(ruleKey({ domains: ['x.io'] })).toBe('domains');
    expect(ruleKey({})).toBe('anyone');
  });
});

describe('the form', () => {
  it('starts empty, as a service of the tenant', () => {
    expect(emptyForm()).toMatchObject({ name: '', kind: 'tenant', url: '', rule: 'anyone', secretSet: false });
  });

  it('reads a provider back, its secret empty and marked set', () => {
    expect(formOf(FECODE)).toMatchObject({ name: 'fecode', kind: 'tenant', url: 'https://fecode.dev/api/v1/visit' });
    const f = formOf(SSO);
    expect(f).toMatchObject({ kind: 'oidc', issuer: 'https://login.example.com/', clientId: 'abc', clientSecret: '',
      secretSet: true, rule: 'domains', values: 'example.com' });
  });

  it('writes a service of the tenant: an https URL with a host', () => {
    expect(providerOf({ ...emptyForm(), url: ' https://fecode.dev/api/v1/visit ' }))
      .toEqual({ kind: 'tenant', url: 'https://fecode.dev/api/v1/visit' });
    expect(() => providerOf(emptyForm())).toThrow('the address is required');
    expect(() => providerOf({ ...emptyForm(), url: 'http://fecode.dev/x' })).toThrow('https URL');
    expect(() => providerOf({ ...emptyForm(), url: 'https://u:p@fecode.dev/x' })).toThrow('https URL');
    expect(() => providerOf({ ...emptyForm(), url: 'https://fecode.dev/x#frag' })).toThrow('https URL');
    expect(() => providerOf({ ...emptyForm(), url: 'fecode.dev' })).toThrow('https URL');
    expect(() => providerOf({ ...emptyForm(), url: `https://a.example/${'x'.repeat(2048)}` })).toThrow('2048');
  });

  it('writes an issuer with a client, keeps a stored secret with {"set": true} and needs one when there is none', () => {
    const f = { ...formOf(SSO) };
    expect(providerOf(f)).toEqual({
      kind: 'oidc', issuer: 'https://login.example.com/', clientId: 'abc', clientSecret: { set: true }, rule: { domains: ['example.com'] },
    });
    expect(providerOf({ ...f, clientSecret: 's3cret' }).clientSecret).toBe('s3cret');
    expect(() => providerOf({ ...f, secretSet: false })).toThrow('the client secret is required');
    expect(() => providerOf({ ...f, issuer: '' })).toThrow('the issuer is required');
    expect(() => providerOf({ ...f, issuer: 'http://login.example.com' })).toThrow('https URL');
    expect(() => providerOf({ ...f, clientId: ' ' })).toThrow('the client ID is required');
    expect(() => providerOf({ ...f, kind: 'ferun' })).toThrow('choose a kind');
  });

  it('writes exactly one rule, addresses and domains in lower case', () => {
    expect(ruleOf({ rule: 'anyone', values: 'ignored' })).toEqual({ anyone: true });
    expect(ruleOf({ rule: 'emails', values: 'Ada@Example.com, grace@example.com' })).toEqual({ emails: ['ada@example.com', 'grace@example.com'] });
    expect(ruleOf({ rule: 'domains', values: 'Example.COM\nexample.org' })).toEqual({ domains: ['example.com', 'example.org'] });
    expect(ruleOf({ rule: 'groups', values: 'Staff ops' })).toEqual({ groups: ['Staff', 'ops'] });
    expect(() => ruleOf({ rule: 'emails', values: '' })).toThrow('name at least one address');
    expect(() => ruleOf({ rule: 'emails', values: 'nobody' })).toThrow('nobody is not an email address');
    expect(() => ruleOf({ rule: 'domains', values: '@example.com' })).toThrow('without the @');
    expect(() => ruleOf({ rule: 'groups', values: Array(101).fill('g').join(' ') })).toThrow('at most 100');
    expect(() => ruleOf({ rule: 'groups', values: 'x'.repeat(256) })).toThrow('255');
    expect(() => ruleOf({ rule: 'other', values: 'x' })).toThrow('choose who');
    expect(valuesOf(' a, b\nc  ')).toEqual(['a', 'b', 'c']);
  });
});

describe('a publish', () => {
  const kinds = ['tenant', 'ferun', 'oidc'];

  it('names ferun first, then the tenant\'s by name, each served or not', () => {
    expect(choices([SSO, FERUN_P, FECODE], kinds)).toEqual([
      { name: 'ferun', kind: 'ferun', served: true }, { name: 'fecode', kind: 'tenant', served: true }, { name: 'sso', kind: 'oidc', served: true },
    ]);
    // Without ferun-auth only the tenant's own services are served.
    expect(choices([FERUN_P, FECODE, SSO], ['tenant']).map((c) => [c.name, c.served]))
      .toEqual([['ferun', false], ['fecode', true], ['sso', false]]);
    // A list that is not readable still offers ferun and the current one.
    expect(choices(null, null, 'fecode')).toEqual([{ name: 'ferun', kind: 'ferun', served: true }, { name: 'fecode', kind: '', served: true }]);
    expect(choices([FECODE], kinds, 'fecode').map((c) => c.name)).toEqual(['ferun', 'fecode']);
    expect(served('oidc', null)).toBe(true);
    expect(served('oidc', ['tenant'])).toBe(false);
  });

  it('publishes a new host label with the access the status says, and none when it says none', () => {
    const cur = { access: 'visitors', provider: 'fecode' };
    expect(hostLabelChoice({ public: true, access: 'visitors', provider: 'fecode' }, cur)).toEqual(cur);
    const anyone = { access: 'anyone', provider: '' };
    expect(hostLabelChoice({ public: true, access: 'anyone', provider: '' }, anyone)).toEqual(anyone);
    // An older ferund answered the status: ferund keeps the stored access.
    expect(hostLabelChoice({ public: true }, anyone)).toEqual({ access: '', provider: '' });
  });

  it('knows an installation that guards nothing only by its empty kinds', () => {
    expect(guardsNone([])).toBe(true);
    expect(guardsNone(null)).toBe(false);
    expect(guardsNone(['tenant'])).toBe(false);
    expect(publishChoice({ public: false }, [])).toEqual({ access: 'anyone', provider: '' });
    expect(publishChoice({ public: false }, null)).toEqual({ access: '', provider: '' });
  });

  it('starts from the access a public environment has, and from visitors of ferun for a first one where it is served', () => {
    expect(publishChoice({ public: true, access: 'visitors', provider: 'fecode' }, kinds)).toEqual({ access: 'visitors', provider: 'fecode' });
    expect(publishChoice({ public: true, access: 'visitors' }, kinds)).toEqual({ access: 'visitors', provider: 'ferun' });
    expect(publishChoice({ public: true, access: 'anyone', provider: '' }, kinds)).toEqual({ access: 'anyone', provider: '' });
    // An older ferund's public environment admits anyone.
    expect(publishChoice({ public: true }, kinds)).toEqual({ access: 'anyone', provider: '' });
    expect(publishChoice({ public: false, access: 'anyone' }, kinds)).toEqual({ access: 'visitors', provider: 'ferun' });
    // Without ferun-auth, or not known yet, the admin chooses.
    expect(publishChoice({ public: false }, ['tenant'])).toEqual({ access: '', provider: '' });
    expect(publishChoice({ public: false }, null)).toEqual({ access: '', provider: '' });
  });

  it('tells a change of access or of provider', () => {
    expect(sameChoice({ access: 'anyone', provider: '' }, { access: 'anyone', provider: 'x' })).toBe(true);
    expect(sameChoice({ access: 'visitors', provider: 'a' }, { access: 'visitors', provider: 'b' })).toBe(false);
    expect(sameChoice({ access: 'visitors', provider: 'a' }, { access: 'anyone', provider: '' })).toBe(false);
  });
});

describe('a delete', () => {
  it('names the environments that keep it', () => {
    const e = new ApiError('provider fecode is named by 2 environments', 409, { code: 'provider_in_use' });
    expect(inUseProblem(e)).toBe('provider fecode is named by 2 environments. Publish them with another provider, or open them to '
      + 'anyone, first: an environment\'s provider cannot go while it guards it.');
    expect(inUseProblem(new ApiError('no such provider', 404, {}))).toBe('');
    expect(inUseProblem(null)).toBe('');
  });
});
