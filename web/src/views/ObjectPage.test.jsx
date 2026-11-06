// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { response } from '../testing/ferun.js';
import { ObjectView } from './ObjectPage.jsx';

// An object's page, rendered with ferund's fixtures: stage's status and
// deployment 7's spec; the incidents and the changes it reads itself
// (not in a static render).

const T = 'fecode';
const nav = () => {};
const STAGE = response('GET_environments_env.200');
const PREVIEW = response('GET_environments_env.200.preview');
const SPEC = response('GET_environments_env_deployments_n.200').spec;
const W = { write: true, admin: false, del: false, clone: false, operator: false };

const html = (el) => renderToStaticMarkup(el);
const text = (el) => html(el).replace(/<[^>]+>/g, ' ').replace(/&#x27;/g, "'").replace(/&quot;/g, '"')
  .replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&amp;/g, '&').replace(/\s+/g, ' ');
const page = (kind, name, { st = STAGE, spec = SPEC, ec = W } = {}) => (
  <ObjectView tenant={T} st={st} kind={kind} name={name} spec={spec} ec={ec} navigate={nav} />);

describe("a service's page", () => {
  it('heads with its kind, name, state, Logs, Traces of it, Restart and Edit, and its facts', () => {
    const out = html(page('service', 'expense-service'));
    const all = text(page('service', 'expense-service'));
    expect(out).toContain('<span class="kind">Service</span><h1 class="mono">expense-service</h1>');
    expect(all).toContain('1 of 2 ready');
    expect(out).toContain('href="/tenants/fecode/environments/expensa-stage/logs/service/expense-service"');
    expect(out).toContain('href="/tenants/fecode/environments/expensa-stage/traces?service=expense-service"');
    expect(out).toContain('href="/tenants/fecode/environments/expensa-stage/builder"');
    expect(all).toContain('Restart');
    expect(all).toContain('image expense:1.4');
    expect(all).toContain('deployment 6 · 7 (rolling)');
    expect(html(page('service', 'expense-service', { ec: { ...W, write: false } }))).toContain('disabled="" title="needs write on this environment"');
  });

  it('shows its definition, the environment with secrets at their versions and references as links', () => {
    const out = html(page('service', 'expense-service'));
    const all = text(page('service', 'expense-service'));
    expect(out).toContain('href="/tenants/fecode/environments/expensa-stage/deployments/7" class="mono">deployment 7</a>');
    expect(all).toContain('image ghcr.io/acme/expense:1.4 pinned to sha256:863e2ebe9c38');
    expect(all).toContain('readiness HTTP GET :http/readyz · every 2s, unready after 3 failures');
    expect(all).toContain('EXPENSA_INTERNAL_TOKEN secret internal-token v3');
    expect(out).toContain('href="/tenants/fecode/environments/expensa-stage/secrets"');
    expect(all).toContain('EXPENSA_DATABASE_URL url of addon postgres (database expensa, role expense_svc)');
    expect(out).toContain('href="/tenants/fecode/environments/expensa-stage/objects/addon/postgres"');
    expect(out).toContain('href="/tenants/fecode/environments/expensa-stage/objects/service/approval-service"');
    expect(all).toContain('EXPENSA_HTTP_ADDR [::]:8080');
  });

  it('lists the routes to it, its instances with the node as text, and says where the changes come from', () => {
    const out = html(page('service', 'expense-service'));
    const all = text(page('service', 'expense-service'));
    expect(all).toContain('routes to it · 1');
    expect(all).toContain('api-expense /api/expense http 8080 expensa-stage-fecode.apps.fecode.dev/api/expense');
    expect(all).toContain('instances · 3 of 2 replicas');
    expect(out.split('title="instance ').length - 1).toBe(3);
    expect(out).not.toContain('href="/nodes/');
    expect(html(page('service', 'expense-service', { ec: { ...W, operator: true } }))).toContain('href="/nodes/n2"');
    expect(all).toContain('Reading the diffs of the last 10 deployments, one request each: 0 so far');
  });

  it('an attached service shows its process; a service the latest deployment lacks says so', () => {
    const all = text(page('service', 'web', { st: PREVIEW, spec: null }));
    expect(all).toContain('attached in sandbox 0193b0aa');
    expect(all).toContain('cwd /workspace/expensa/web does not exist yet');
    expect(all).toContain('definition · deployment');
    const gone = text(page('service', 'old-api'));
    expect(gone).toContain('not in the latest deployment');
    expect(gone).toContain('The latest deployment (7) has no service old-api.');
    expect(gone).toContain('Not in this deployment.');
  });
});

describe("an addon's and a job's page", () => {
  it('an addon: type, version, plan, its options, output keys, who uses it', () => {
    const out = html(page('addon', 'postgres'));
    const all = text(page('addon', 'postgres'));
    expect(out).toContain('<span class="kind">Addon</span>');
    expect(all).toContain('postgres 17 · plan small · provisioned');
    expect(all).toContain('option roles expense_svc, approval_svc');
    expect(out).toContain('<span class="labelchip mono on" title="named by an object">url</span>');
    expect(out).toContain('<span class="labelchip mono" title="not named">password</span>');
    expect(all).toContain('used by · 3');
    expect(out).toContain('href="/tenants/fecode/environments/expensa-stage/objects/job/migrate"');
    expect(all).not.toContain('Traces');
  });

  it('a job: when it runs, its last run, Run now', () => {
    const all = text(page('job', 'migrate'));
    expect(all).toContain('Job migrate last run succeeded');
    expect(all).toContain('runs with each deployment that changes it');
    expect(all).toContain('timeout 10m0s');
    expect(all).toContain('Run now');
    expect(all).toContain('deployment 7 attempt 1 phase succeeded exit 0');
    const manual = { ...SPEC, jobs: { migrate: { ...SPEC.jobs.migrate, on: 'manual' } } };
    expect(html(page('job', 'migrate', { spec: manual }))).toContain('<button class="primary"');
  });
});
