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
    expect(out).toContain('offloaded');
    expect(out).toContain('The container never sees it.');
  });
  it('says no key for a relation without a mark', () => {
    expect(renderToStaticMarkup(<KeyMark mark={null} />)).toContain('no key');
  });
  it('explains all three in the legend', () => {
    const out = renderToStaticMarkup(<MarkLegend />);
    for (const w of ['by identity', 'offloaded', 'in the container']) expect(out).toContain(w);
  });
});
