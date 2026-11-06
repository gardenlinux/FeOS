// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { BindingCard, CertificateCard, DomainCallout, DomainStateLine, DomainStepper, DomainsEmpty, RecordsTable, domainPath, envOf } from './Domains.jsx';

// The domain page's pieces, as the environment's tab reuses them.

const TOKEN = 'k3m9q2x7v4b8n1c6p0z5w2r8t4y7u3e1';
const ENV = '0193c4d0-5e6f-7a01-8b2c-3d4e5f6a7b01';

const d = {
  id: '0193d1a0-0000-7000-8000-000000000002', name: 'shop.example.com', labels: {}, environment: ENV,
  verification: 'verified', state: 'waiting-for-dns',
  reason: 'waiting for the name to resolve to the edge: a CNAME to the edge\'s name, or A and AAAA records of its addresses',
  records: { txt: { name: '_ferun-challenge.shop.example.com', value: TOKEN }, cname: 'edge.run.example.com', addresses: ['203.0.113.47', '2001:db8::47'], apex: null },
  found: { txt: [TOKEN], cname: ['old-host.example.net'], addresses: ['198.51.100.9'] },
  warnings: { recordMissingSince: null, dnsMovedSince: null, deactivatesAt: null, checkFailing: '', certificateFailing: '' },
  certificate: null, createdAt: '2026-11-04T12:00:00Z', verifiedAt: '2026-11-05T12:00:00Z', boundAt: '2026-11-05T12:00:00Z',
  checkedAt: '2026-11-06T11:50:00Z', nextCheckAt: '2026-11-06T12:12:00Z',
};

describe('RecordsTable', () => {
  it('lists the TXT and the CNAME with what DNS says now, and the apex alternative', () => {
    const out = renderToStaticMarkup(<RecordsTable d={d} />);
    expect(out).toContain('_ferun-challenge.shop.example.com');
    expect(out).toContain(TOKEN);
    expect(out).toContain('edge.run.example.com');
    expect(out).toContain('✓ <span class="mono">found</span>');
    expect(out).toContain('✗ <span class="mono">CNAME old-host.example.net</span>');
    expect(out).toContain('ALIAS or ANAME');
    expect(out).toContain('2001:db8::47');
  });
});

describe('DomainStepper', () => {
  it('shows the six stations with their status', () => {
    const out = renderToStaticMarkup(<DomainStepper d={d} envName="prod" />);
    expect(out.match(/class="dom-step /g)).toHaveLength(6);
    expect(out).toContain('<li class="dom-step warn"><b>routed</b><span>CNAME points to old-host.example.net</span></li>');
  });
});

describe('DomainCallout', () => {
  it('sets the names in mono', () => {
    const out = renderToStaticMarkup(<DomainCallout d={d} envName="prod" />);
    expect(out).toContain('dom-callout t-warn');
    expect(out).toContain('<span class="mono">old-host.example.net</span>');
  });
});

describe('DomainStateLine', () => {
  it('is the dot, the state and the sentence', () => {
    const out = renderToStaticMarkup(<DomainStateLine d={d} />);
    expect(out).toContain('sdot unknown');
    expect(out).toContain('waiting-for-dns');
    expect(out).toContain('waiting for the name to point to the edge');
  });
});

describe('CertificateCard', () => {
  it('none without a certificate; else its times and serial, renewed automatically', () => {
    expect(renderToStaticMarkup(<CertificateCard d={d} />)).toBe('');
    const out = renderToStaticMarkup(<CertificateCard d={{
      ...d, state: 'served',
      certificate: { notBefore: '2026-11-05T06:00:00Z', notAfter: '2099-11-11T06:00:00Z', issuedAt: '2026-11-05T06:00:00Z', renewAfter: '2099-11-08T14:00:00Z', serial: '04a1f3' },
    }} />);
    expect(out).toContain('04a1f3');
    expect(out).toContain('short-lived');
    expect(out).toContain('renews it');
    expect(out).toContain('badge ok');
  });
});

describe('BindingCard', () => {
  const card = (access, dom = d) => renderToStaticMarkup(
    <BindingCard tenant="kino" d={dom} env={null} envName="prod" access={access} busy="" act={() => {}} navigate={() => {}} />,
  );
  it('says the default host answers 404 while the domain is served', () => {
    expect(card({ role: 'admin', environments: [] })).toContain('default host answers 404');
  });
  it('without admin on its environment the select is disabled', () => {
    expect(card({ role: 'read', environments: [] })).toMatch(/<select[^>]*disabled/);
    expect(card({ role: 'admin', environments: [] })).not.toMatch(/<select[^>]*disabled/);
  });
  it('a superseded claim cannot be bound', () => {
    expect(card({ role: 'admin', environments: [] }, { ...d, state: 'superseded', environment: null })).toContain('cannot be bound');
  });
});

describe('DomainsEmpty', () => {
  it('explains a domain, and offers the claim to an admin only', () => {
    expect(renderToStaticMarkup(<DomainsEmpty admin onClaim={() => {}} />)).toContain('Claim domain');
    const out = renderToStaticMarkup(<DomainsEmpty admin={false} onClaim={() => {}} />);
    expect(out).toContain('instead of its default host');
    expect(out).toContain('needs admin on this tenant');
  });
});

describe('domainPath and envOf', () => {
  it('link by the ID and find the environment by its ID', () => {
    expect(domainPath('kino', d)).toBe('/tenants/kino/domains/0193d1a0-0000-7000-8000-000000000002');
    expect(domainPath('kino')).toBe('/tenants/kino/domains');
    expect(envOf(d, [{ id: ENV, name: 'prod' }])).toEqual({ id: ENV, name: 'prod' });
    expect(envOf({ ...d, environment: null }, [{ id: ENV, name: 'prod' }])).toBeNull();
  });
});
