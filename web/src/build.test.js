// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import { buildOf } from './build.js';

// A stand-in for a document: the attributes of the elements a selector finds.
const doc = (scripts, sheets) => ({
  querySelectorAll: (sel) =>
    (sel.startsWith('script') ? scripts : sheets).map((v) => ({ getAttribute: () => v })),
});

describe('buildOf', () => {
  it('is the entry script and stylesheets under /assets/, in order', () => {
    const page = doc(['/assets/index-B2.js'], ['/assets/index-A1.css', 'https://fonts.googleapis.com/css2?family=x']);
    expect(buildOf(page)).toBe('/assets/index-A1.css /assets/index-B2.js');
  });
  it('differs for a new deployment of the UI, not for the same files', () => {
    const old = doc(['/assets/index-B2.js'], ['/assets/index-A1.css']);
    expect(buildOf(doc(['/assets/index-B2.js'], ['/assets/index-A1.css']))).toBe(buildOf(old));
    expect(buildOf(doc(['/assets/index-B2.js'], ['/assets/index-C3.css']))).not.toBe(buildOf(old));
  });
  it('is empty on a dev server', () => {
    expect(buildOf(doc(['/@vite/client', '/src/main.jsx'], []))).toBe('');
  });
});
