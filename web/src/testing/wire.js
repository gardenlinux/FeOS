// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { existsSync, readdirSync } from 'node:fs';
import { response } from './ferun.js';

// The answers of ferund's people routes (FeOS ADRs 0056, 0059, 0060): the
// fixtures ferund's handler tests write (go/internal/api/testdata/ferun,
// named here), and until a ferund writes one, the shape the release's
// wire contract gives it, with the same names and fields. A test reads
// wire(name) and asserts what holds of every answer of that shape, so
// that it holds of ferund's fixture once that exists.

const dir = new URL('../../../go/internal/api/testdata/ferun/', import.meta.url);

const P = '0199a1b2-0000-7000-8000-00000000aaaa';
const ADA = 'c1a7e1f0-ada0-4000-8000-000000000001';
const PROD = '0199a1b2-0000-7000-8000-000000000001';
const STAGE = '0199a1b2-0000-7000-8000-000000000002';
const DEFAULT = { name: 'default', title: 'Default' };
const TEAM = { name: 'cc-4711', title: 'Team X' };
const FECODE = { name: 'fecode', title: 'FeCode', url: 'https://fecode.dev', disabled: false };
const env = (id, name, role, prot, plan, del, extra = {}) => ({ id, name, role, ...extra, protected: prot, plan, delete: del });

// SHAPES are the answers by fixture name.
const SHAPES = {
  'GET_auth_me.session.200': {
    kind: 'session', sub: ADA, person: P, email: 'ada@example.com', name: 'Ada', username: 'ada', picture: '', admin: false, method: 'oidc',
    projects: [
      { project: 'kino', role: 'write', account: DEFAULT, delete: false, environments: [env(PROD, 'prod', 'read', true, true, false), env(STAGE, 'stage', 'write', false, true, true)] },
      { project: 'shop', role: '', account: TEAM, delete: false, environments: [env('0199a1b2-0000-7000-8000-000000000003', 'shop-prod', 'admin', true, true, true)] },
    ],
    accounts: [
      { name: 'cc-4711', title: 'Team X', role: 'admin', explicit: true, closed: false },
      { name: 'default', title: 'Default', role: 'member', explicit: false, closed: false },
      { name: 'old', title: 'Old', role: 'member', explicit: true, closed: true },
    ],
  },
  'GET_auth_me.operator.200': {
    kind: 'session', sub: 'op', person: '0199a1b2-0000-7000-8000-00000000bbbb', email: 'op@example.com', name: 'Op', admin: true, method: 'oidc',
    projects: [
      { project: 'kino', role: 'admin', account: DEFAULT, delete: true, environments: [env(PROD, 'prod', 'admin', true, true, true), env(STAGE, 'stage', 'admin', false, true, true)] },
    ],
    accounts: [
      { name: 'cc-4711', title: 'Team X', role: 'admin', explicit: true, closed: false },
      { name: 'default', title: 'Default', role: 'admin', explicit: true, closed: false },
    ],
  },
  'GET_auth_me.token.200': {
    kind: 'token', token: { label: 'landscape release', role: 'write', environment: PROD },
    projects: [{ project: 'fecode-platform', role: '', delete: false, environments: [env(PROD, 'prod', 'write', false, true, false)] }],
    accounts: [],
  },
  'GET_auth_me.integration.200': {
    kind: 'integration', integration: FECODE,
    projects: [{
      project: 'kino', role: 'write', integrationRole: 'write', delete: false, environments: [
        env(PROD, 'prod', 'read', true, true, false, { integrationRole: 'write' }),
        env(STAGE, 'stage', 'write', false, true, true, { integrationRole: 'write' }),
      ],
    }],
    accounts: [],
  },
  'GET_auth_me.integration_person.200': {
    kind: 'integration', integration: FECODE,
    named: { sub: ADA, known: true, person: P, email: 'ada@example.com', name: 'Ada' },
    projects: [{
      project: 'kino', role: 'write', integrationRole: 'admin', personRole: 'write', delete: false, environments: [
        env(PROD, 'prod', 'read', true, true, false, { integrationRole: 'admin', personRole: 'read' }),
      ],
    }],
    accounts: [],
  },
  'GET_project_members.200': {
    members: [
      {
        person: P, name: 'Ada', email: 'ada@example.com', issuer: 'https://id.fecore.dev/', subject: ADA, role: 'admin',
        madeBy: `person:${P}`, madeByName: 'Ada',
        environments: [{ id: PROD, name: 'prod', role: 'admin', madeBy: `person:${P}`, madeByName: 'Ada' }],
      },
      {
        person: '0199a1b2-0000-7000-8000-00000000cccc', name: 'Bo', email: 'bo@example.com', issuer: 'https://id.fecore.dev/',
        subject: 'b0b', disabled: true, role: 'write', madeBy: `person:${P}`, madeByName: 'Ada', environments: [],
      },
    ],
    integrations: [
      { integration: 'fecode', title: 'FeCode', url: 'https://fecode.dev', disabled: false, role: 'write', madeBy: `person:${P}`, madeByName: 'Ada',
        environments: [{ id: PROD, name: 'prod', role: 'write', madeBy: `person:${P}`, madeByName: 'Ada' }] },
    ],
  },
  'GET_integrations.200': { integrations: [{ name: 'fecode', title: 'FeCode', url: 'https://fecode.dev' }, { name: 'fecode-dev', title: 'FeCode dev', url: '' }] },
  'GET_admin_integrations.200': {
    integrations: [
      { name: 'fecode', title: 'FeCode', url: 'https://fecode.dev', issuer: 'https://id.fecore.dev/', subject: 'dd324626-d373-44f1-8490-cf0d0857bbdf',
        disabled: false, createdAt: '2026-11-21T10:00:00Z', updatedAt: '2026-11-21T10:00:00Z', lastSeen: '2026-11-21T11:00:00Z', madeBy: 'basic:feos-k8s', projects: 3 },
      { name: 'old', title: 'Old', url: '', issuer: 'https://id.fecore.dev/', subject: 'x', disabled: true, disabledAt: '2026-11-21T10:30:00Z',
        createdAt: '2026-11-20T10:00:00Z', updatedAt: '2026-11-21T10:30:00Z', madeBy: 'basic:feos-k8s', projects: 0 },
    ],
  },
  'PUT_environments_env_protected.200': { id: PROD, name: 'prod', protected: true, changed: true },
  'GET_environments_env_principals.200': {
    principals: [
      { kind: 'person', id: P, label: 'Ada <ada@example.com>', projectRole: 'write', environmentRole: '', losesWrite: true },
      { kind: 'person', id: '0199a1b2-0000-7000-8000-00000000dddd', label: 'Cy <cy@example.com>', projectRole: 'admin', environmentRole: '', losesWrite: false },
      { kind: 'token', id: '3f9a1c2b7d4e00000000000000000000000000000000000000000000000000ff', label: 'landscape release', projectRole: 'write', environmentRole: '', losesWrite: true },
      { kind: 'integration', id: 'fecode', label: 'FeCode', projectRole: 'write', environmentRole: 'write', losesWrite: false },
      { kind: 'person', id: '0199a1b2-0000-7000-8000-00000000eeee', label: 'Eve <eve@example.com>', projectRole: 'read', environmentRole: '', losesWrite: false },
    ],
  },
  'GET_accounts.200': {
    accounts: [
      { name: 'cc-4711', title: 'Team X', role: 'admin', explicit: true, closed: false },
      { name: 'default', title: 'Default', role: 'member', explicit: false, closed: false },
    ],
  },
  'GET_accounts_account.200': {
    name: 'cc-4711', title: 'Team X', reference: 'CC 4711', closed: false, role: 'member',
    members: [{ person: P, name: 'Ada', email: 'ada@example.com', role: 'admin', disabled: false }],
    projects: [{
      name: 'kino', madeBy: { person: P, name: 'Ada', email: 'ada@example.com' }, admins: [{ person: P, name: 'Ada', email: 'ada@example.com' }],
      since: '2026-11-21T10:00:00Z', deleting: false, limits: { sandboxes: 10 }, use: { sandboxes: 2, environments: 3, runningJobs: 0, vcpus: 4, memoryMib: 4096 },
    }],
    moves: [
      { project: 'kino', from: DEFAULT, to: TEAM, at: '2026-11-21T10:00:00Z', by: { principal: `person:${P}`, name: 'Ada' } },
      { project: 'shop', from: null, to: TEAM, at: '2026-11-20T10:00:00Z', by: { principal: '', name: 'FeRun' } },
    ],
  },
  'GET_project_account.200': {
    account: TEAM, since: '2026-11-21T10:00:00Z',
    moves: [{ project: 'kino', from: DEFAULT, to: TEAM, at: '2026-11-21T10:00:00Z', by: { principal: `person:${P}`, name: 'Ada' } }],
  },
  'POST_project_move.200': { project: 'kino', account: TEAM, from: DEFAULT, movedAt: '2026-11-21T10:00:00Z', moved: true },
  'GET_admin_accounts.200': {
    accounts: [
      { name: 'cc-4711', title: 'Team X', reference: 'CC 4711', createdAt: '2026-11-20T10:00:00Z', madeBy: 'basic:feos-k8s', closed: false, projects: 1, members: 1 },
      { name: 'default', title: 'Default', reference: '', createdAt: '2026-11-01T10:00:00Z', madeBy: '', closed: false, projects: 4, members: 0 },
      { name: 'old', title: 'Old', reference: '', createdAt: '2026-11-01T10:00:00Z', madeBy: 'basic:feos-k8s', closed: true, closedAt: '2026-11-10T10:00:00Z', projects: 0, members: 0 },
    ],
  },
  'GET_invitations_token.account.200': { account: TEAM, role: 'member', expiresAt: '2026-11-28T10:00:00Z' },
};

// WIRE_NAMES are the fixture names of the release's people answers.
export const WIRE_NAMES = Object.keys(SHAPES);

// WIRE_BY_PACKAGE are the fixtures of §2.16 by the package whose handler
// tests write them: a package lands all of its own at once, so that a
// part of them is one named otherwise (wire.test.js).
export const WIRE_BY_PACKAGE = {
  F3: ['GET_auth_me.session.200', 'GET_auth_me.operator.200', 'GET_auth_me.token.200', 'GET_auth_me.integration.200',
    'GET_auth_me.integration_person.200', 'GET_project_members.200', 'GET_integrations.200', 'GET_admin_integrations.200',
    'PUT_environments_env_protected.200', 'GET_environments_env_principals.200'],
  F4: ['GET_accounts.200', 'GET_accounts_account.200', 'GET_project_account.200', 'POST_project_move.200',
    'GET_admin_accounts.200', 'GET_invitations_token.account.200'],
};

// ferundFixtures are the names of every fixture ferund's tests wrote.
export function ferundFixtures() {
  if (!existsSync(dir)) return [];
  return readdirSync(dir).filter((f) => f.endsWith('.json')).map((f) => f.slice(0, -'.json'.length));
}

// fromFerund tells whether ferund's fixture of name exists.
export function fromFerund(name) {
  return existsSync(new URL(`${name}.json`, dir));
}

// wire is the answer of name: ferund's fixture's when it exists, else
// the shape of the wire contract.
export function wire(name) {
  if (fromFerund(name)) return response(name);
  if (!(name in SHAPES)) throw new Error(`no answer named ${name}`);
  return structuredClone(SHAPES[name]);
}

// answers are every answer of name the tests know: ferund's fixture
// first when it exists, then the wire contract's shape.
export function answers(name) {
  if (!(name in SHAPES)) throw new Error(`no answer named ${name}`);
  const shape = structuredClone(SHAPES[name]);
  return fromFerund(name) ? [response(name), shape] : [shape];
}

// answerWith is the first answer of names (a name, or a list of names
// taken together, ferund's first) of which has(...answers) holds: the
// case a test is about, from ferund's fixture when that has it, else
// from the contract's shape, which has every case the console shows.
// It throws when no answer has the case.
export function answerWith(names, has) {
  const list = Array.isArray(names) ? names : [names];
  const tuples = list.reduce((acc, n) => acc.flatMap((t) => answers(n).map((a) => [...t, a])), [[]]);
  const hit = tuples.find((t) => has(...t));
  if (!hit) throw new Error(`no answer of ${list.join(' + ')} has the case`);
  return Array.isArray(names) ? hit : hit[0];
}
