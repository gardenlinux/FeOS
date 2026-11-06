// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { CodeSees, PreflightRows } from './Credentials.jsx';

// The credential pages' pieces that the form and the page share.

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
