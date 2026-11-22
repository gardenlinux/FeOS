// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import { expectationOf, expectationWords, newSecretQuery, offeredSecret, pinSecret, secretPins } from './pins.js';

// A spec states what its code expects of a secret (ADR 0058): a moved pin
// keeps it, and the builder shows it and offers it for a missing secret.
const SPEC = {
  services: { web: { image: 'a', env: { S4: { secret: 's4', key: 'url' }, T: { secret: 'token' } } } },
  secrets: {
    s4: { version: 1, type: 'http', upstream: 'https://api.s4.example', mode: 'name', forwardedHost: true },
    token: { version: 2, type: 'value' },
    old: { version: 3, upstream: 'https://old.example' },
  },
};

describe('a pin that moves', () => {
  it('keeps type, mode and forwardedHost and drops the upstream', () => {
    expect(pinSecret(SPEC, 's4', 2).secrets.s4).toEqual({ version: 2, type: 'http', mode: 'name', forwardedHost: true });
    expect(pinSecret(SPEC, 's4', 0).secrets.s4).toEqual({ type: 'http', mode: 'name', forwardedHost: true });
    expect(pinSecret(SPEC, 'token', 3).secrets.token).toEqual({ version: 3, type: 'value' });
    expect(pinSecret(SPEC, 'old', 4).secrets.old).toEqual({ version: 4 });
    expect(pinSecret(SPEC, 'fresh', 1).secrets.fresh).toEqual({ version: 1 });
    // The spec it moved in stays as it was.
    expect(SPEC.secrets.s4.version).toBe(1);
  });
  it('keeps a forwardedHost of false', () => {
    const s = { secrets: { s4: { version: 1, mode: 'name', forwardedHost: false } } };
    expect(pinSecret(s, 's4', 2).secrets.s4).toEqual({ version: 2, mode: 'name', forwardedHost: false });
  });
});

describe('the expectation', () => {
  it('is what the pin writes, and nothing else', () => {
    expect(expectationOf(SPEC.secrets.s4)).toEqual({ type: 'http', mode: 'name', forwardedHost: true });
    expect(expectationOf({ type: 'file', mode: 'env', forwardedHost: 'yes' })).toEqual({});
    expect(expectationOf(null)).toEqual({});
  });
  it('is the row of each secret, and a written type is its kind before the list knows it', () => {
    const pins = secretPins({ ...SPEC, secrets: { ...SPEC.secrets, gh: { type: 'http', mode: 'intercept' } } },
      ['s4', 'token', 'gh'], [{ name: 's4', type: 'http', version: 1 }]);
    expect(pins.map((p) => [p.name, p.type, p.expect])).toEqual([
      ['s4', 'http', { type: 'http', mode: 'name', forwardedHost: true }],
      ['token', 'value', { type: 'value' }],
      ['gh', 'http', { type: 'http', mode: 'intercept' }],
    ]);
  });
  it('reads as words', () => {
    expect(expectationWords({})).toBe('');
    expect(expectationWords({ type: 'value' })).toBe('a value');
    expect(expectationWords({ type: 'http' })).toBe('an HTTP secret');
    expect(expectationWords({ mode: 'intercept' })).toBe('an HTTP secret in the intercept mode');
    expect(expectationWords({ type: 'http', mode: 'name', forwardedHost: true })).toBe('an HTTP secret in the name mode, the host forwarded');
    expect(expectationWords({ forwardedHost: false })).toBe('an HTTP secret, the host not forwarded');
  });
});

describe('a missing secret made as expected', () => {
  it('carries name, mode and forwarded host to the new secret, and back', () => {
    const q = newSecretQuery('s4', { type: 'http', mode: 'name', forwardedHost: true });
    expect(q).toBe('?name=s4&mode=name&forwardedHost=1');
    expect(offeredSecret(q)).toEqual({ name: 's4', mode: 'name', forwardedHost: true });
    expect(offeredSecret(newSecretQuery('gh', { mode: 'intercept' }))).toEqual({ name: 'gh', mode: 'intercept' });
    expect(offeredSecret(newSecretQuery('x', {}))).toEqual({ name: 'x' });
  });
  it('takes nothing that is not well formed', () => {
    expect(offeredSecret('?name=Bad_Name&mode=env&forwardedHost=yes')).toEqual({});
    expect(offeredSecret('?mode=intercept&forwardedHost=1')).toEqual({ mode: 'intercept' });
    expect(offeredSecret('')).toEqual({});
  });
});
