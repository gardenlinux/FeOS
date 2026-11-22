// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import { WIRE_BY_PACKAGE, WIRE_NAMES, answerWith, answers, ferundFixtures, fromFerund, wire } from './wire.js';

// The console's tests read ferund's fixtures of §2.16 once they exist and
// the wire contract's shapes until then. These checks keep the switch
// from missing: a fixture named slightly otherwise would leave the tests
// on the shapes for good.

const route = (name) => name.split('.')[0];

describe('wire', () => {
  it('names exactly the fixtures of §2.16', () => {
    expect([...WIRE_NAMES].sort()).toEqual([...WIRE_BY_PACKAGE.F3, ...WIRE_BY_PACKAGE.F4].sort());
  });

  it('a package\'s fixtures land together: all of them from ferund, or none', () => {
    for (const [pkg, names] of Object.entries(WIRE_BY_PACKAGE)) {
      const have = names.filter(fromFerund);
      const lacking = names.filter((n) => !fromFerund(n));
      expect(have.length === 0 || lacking.length === 0, `${pkg}: ferund wrote ${have.join(', ')} but not ${lacking.join(', ')}`).toBe(true);
    }
  });

  it('no fixture of ferund\'s is one of the contract\'s by another name', () => {
    const all = ferundFixtures();
    const lacking = WIRE_NAMES.filter((n) => !fromFerund(n));
    for (const n of lacking) {
      const near = all.filter((f) => route(f).toLowerCase() === route(n).toLowerCase() || f.toLowerCase() === n.toLowerCase());
      expect(near, `${n} is not written, but ${near.join(', ')} is`).toEqual([]);
    }
  });

  it('a case comes from ferund\'s fixture when it has it, else from the contract\'s shape', () => {
    for (const n of WIRE_NAMES) {
      const all = answers(n);
      expect(all[0]).toEqual(wire(n));
      expect(all).toHaveLength(fromFerund(n) ? 2 : 1);
      expect(answerWith(n, () => true)).toEqual(wire(n));
    }
    const [a, b] = answerWith(['GET_accounts.200', 'GET_auth_me.session.200'], (x, y) => x !== y);
    expect(a.accounts).toBeTruthy();
    expect(b.kind).toBe('session');
    expect(() => answerWith('GET_accounts.200', () => false)).toThrow('no answer of GET_accounts.200 has the case');
  });
});
