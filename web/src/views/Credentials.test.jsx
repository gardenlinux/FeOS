// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { CodeSees, CredentialForm, Credentials, PreflightRows } from './Credentials.jsx';

// The credential pages' pieces that the form and the page share, and the
// pages as an environment's Credentials holds them (ADR 0033).

const html = (el) => renderToStaticMarkup(el);
const nav = () => {};
const WRITE = { write: true };
const READ = { write: false };

describe('CodeSees', () => {
  it('the request without a credential, and the masked echo', () => {
    const out = renderToStaticMarkup(<CodeSees c={{ name: 'github', upstream: 'https://api.github.com', mode: 'name', pathPrefixes: ['/user'] }} />);
    expect(out).toContain('curl -s http://github/user');
    expect(out).toContain('redacted-by-FeOS-binding');
    expect(out).toContain('FeOS speaks TLS to the upstream');
  });

  it('the intercept mode calls the API\'s own host', () => {
    const out = renderToStaticMarkup(<CodeSees c={{ name: 'openai', upstream: 'https://api.openai.com', mode: 'intercept', pathPrefixes: [] }} />);
    expect(out).toContain('curl -s https://api.openai.com/');
    expect(out).toContain('interception CA');
  });
});

describe('PreflightRows', () => {
  it('a failure shows its code and message', () => {
    const out = renderToStaticMarkup(<PreflightRows result={{ ok: false, addresses: [], error: { code: 'tls_failed', message: 'x509: unknown authority' } }} />);
    expect(out).toContain('tls_failed');
    expect(out).toContain('x509: unknown authority');
    expect(out).toContain('✗');
  });
});

describe('an environment\'s credentials', () => {
  const github = {
    name: 'github', upstream: 'https://api.github.com', mode: 'name', caPem: '', pathPrefixes: ['/user'], forwardedHost: false,
    rateLimit: null, credential: { kind: 'headers', headers: { Authorization: { set: true } } }, version: 3,
    usedBy: { sandboxes: [], bindings: [], latest: 0, deployed: 0 },
  };

  it('the list: New credential needs write on the environment', () => {
    const out = html(<Credentials tenant="kino" env="prod" sel="" ec={READ} navigate={nav} />);
    expect(out).toContain('credentials <span class="count">· …</span>');
    expect(out).toMatch(/<button class="mini" disabled="" title="needs write on this environment">/);
    expect(out).toContain('A credential is this environment&#x27;s');
    expect(html(<Credentials tenant="kino" env="prod" sel="" ec={WRITE} navigate={nav} />))
      .toContain('<button class="mini" title="state an API and its credential once">');
  });

  it('a new one\'s form, back to the environment\'s credentials', () => {
    const out = html(<Credentials tenant="kino" env="prod" sel="new" ec={WRITE} navigate={nav} />);
    expect(out).toContain('New credential');
    expect(out).toContain('</svg> credentials</button>');
    expect(out).toContain('href="/tenants/kino/environments/prod/credentials">Cancel</a>');
    expect(out).toContain('This environment&#x27;s sandboxes and bindings name it');
    expect(out).toMatch(/<button type="submit" class="primary">/);
    const ro = html(<Credentials tenant="kino" env="prod" sel="new" ec={READ} navigate={nav} />);
    expect(ro).toMatch(/<button type="submit" class="primary" disabled="" title="needs write on this environment">/);
  });

  it('a new version\'s form keeps the secrets it leaves empty', () => {
    const out = html(<CredentialForm tenant="kino" env="prod" cred={github} ec={WRITE} navigate={nav} />);
    expect(out).toContain('version 3 → 4');
    expect(out).toContain('set — leave empty to keep');
    expect(out).toContain('Kept from version 3: <span class="mono">headers.Authorization</span>');
    expect(out).not.toContain('This environment&#x27;s sandboxes and bindings name it');
  });
});
