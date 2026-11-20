// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { KeyMark, MarkLegend } from './Marks.jsx';

describe('KeyMark', () => {
  it('shows a mark with its words and what it means', () => {
    const out = renderToStaticMarkup(<KeyMark mark="offloaded" />);
    expect(out).toContain('badge mark mark-offloaded');
    // The key stays offloaded in the code; the word is where it lives (ADR 0055).
    expect(out).toMatch(/<\/svg> at the node<\/span>$/);
    expect(out).toContain('The container never sees it.');
  });
  it('says no key for a relation without a mark', () => {
    expect(renderToStaticMarkup(<KeyMark mark={null} />)).toContain('no key');
  });
  it('explains all three in the legend', () => {
    const out = renderToStaticMarkup(<MarkLegend />);
    for (const w of ['</svg> by identity</span>', '</svg> at the node</span>', '</svg> in the container</span>']) expect(out).toContain(w);
  });
  it('says a mark more precisely where it is given words and a title', () => {
    const out = renderToStaticMarkup(<KeyMark mark="offloaded" label="OAuth2 at the node" title="Binding erp" />);
    expect(out).toContain('mark-offloaded');
    expect(out).toContain('title="Binding erp"');
    expect(out).toContain('OAuth2 at the node');
    expect(out).not.toContain('</svg> at the node</span>');
  });
  it('explains only the marks given to the legend, in their order', () => {
    const out = renderToStaticMarkup(<MarkLegend marks={['container', 'identity']} />);
    expect(out).not.toContain('mark-offloaded');
    expect(out.indexOf('mark-identity')).toBeLessThan(out.indexOf('mark-container'));
  });
});
