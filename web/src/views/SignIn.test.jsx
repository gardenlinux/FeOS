// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { SignIn, looksLikeEmail } from './SignIn.jsx';

// The sign-in screen offers what /auth/config says ferund offers.

const render = (config) => renderToStaticMarkup(
  <SignIn config={config} next="/tenants/acme/sandboxes" onSignedIn={() => {}} />,
);

describe('SignIn', () => {
  it('with OIDC: an address first, then /auth/login with it and the path to return to', () => {
    const out = render({ oidc: true, basic: false });
    expect(out).toContain('type="email"');
    expect(out).toContain('action="/auth/login"');
    expect(out).toContain('name="login_hint"');
    expect(out).toContain('value="/tenants/acme/sandboxes"');
    expect(out).toContain('Sign in');
    expect(out).not.toContain('type="password"');
    expect(out).not.toContain('control plane');
  });

  it('checks the address before it goes on', () => {
    expect(looksLikeEmail('ann@example.com')).toBe(true);
    expect(looksLikeEmail(' ann@example.com ')).toBe(true);
    expect(looksLikeEmail('ann')).toBe(false);
    expect(looksLikeEmail('ann@')).toBe(false);
    expect(looksLikeEmail('a b@example.com')).toBe(false);
  });

  it('with Basic only: the user and password form, no OIDC link', () => {
    const out = render({ oidc: false, basic: true });
    expect(out).toContain('type="password"');
    expect(out).not.toContain('/auth/login');
    expect(out).not.toContain('type="email"');
  });

  it('with both: the address alone (Basic stays for scripts)', () => {
    const out = render({ oidc: true, basic: true });
    expect(out).toContain('action="/auth/login"');
    expect(out).not.toContain('type="password"');
  });

  it('with neither: says so', () => {
    const out = render({ oidc: false, basic: false });
    expect(out).toContain('offers no sign-in');
    expect(out).not.toContain('<form');
  });
});
