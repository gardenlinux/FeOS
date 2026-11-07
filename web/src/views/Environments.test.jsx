// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { specBindings } from './envs.js';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { readFileSync } from 'node:fs';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { api, setActiveTenant } from '../api.js';
import { response, serveFixtures } from '../testing/ferun.js';
import {
  BindingCard, DiffView, EnvHead, EnvOverview, EnvTable, IncidentTable, IncidentView, LogsBody, DeploymentTable,
  DeploymentView, ObjectRow, TagsBox, envCan,
} from './Environments.jsx';
import {
  AccessTable, CloneBox, DeleteBox, DomainsBox, PublicBox, RouteTokenBox, SecretsTable, TrafficTable,
} from './EnvTabs.jsx';
import { logsProblem } from './envs.js';
import { LogSearch } from './LogSearch.jsx';
import { focusOf } from '../logs.js';

// The environments view, rendered with ferund's fixtures as its answers:
// the list, an environment's objects and instances per kind with its
// rollout and incidents, the deployments with a deployment's diff and spec,
// the incidents with their evidence, and an object's logs with ferund's
// refusals.

const T = 'fecode';
const nav = () => {};
const STAGE = response('GET_environments_env.200');
const PREVIEW = response('GET_environments_env.200.preview');

const html = (el) => renderToStaticMarkup(el);
// text is the markup's text, its entities decoded and its spaces
// collapsed.
const text = (el) => html(el).replace(/<[^>]+>/g, ' ').replace(/&#x27;/g, "'").replace(/&quot;/g, '"')
  .replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&amp;/g, '&').replace(/\s+/g, ' ');
const count = (s, needle) => s.split(needle).length - 1;

afterEach(() => vi.unstubAllGlobals());

describe('EnvTable', () => {
  const list = response('GET_environments.200').environments;
  const table = (envs, filtered = false) => html(<EnvTable envs={envs} filtered={filtered} tenant={T} navigate={nav} onLabel={() => {}} />);

  it('lists state, name, deployments in words, hosts, labels, open incidents and created', () => {
    const out = table(list);
    expect(out.match(/<th[ >]/g)).toHaveLength(7);
    expect(count(out, 'class="clickable"')).toBe(3);
    expect(out).toContain('href="/tenants/fecode/environments/expensa-stage"');
    expect(out).toContain('href="/tenants/fecode/environments/expensa-prod"');
    expect(out).toContain('>fecode.class=<b>stage</b></button>');
    expect(out).toContain('title="filter by fecode.class=production"');
    // Stage rolls 7 while 6 serves, in words; the others are deployed.
    expect(out).toContain('class="mono small nowrap state-label moving">7 deploying · 6 serves</td>');
    expect(out).toContain('>4 deployed</td>');
    expect(out).toContain('>3 deployed</td>');
    // A public one shows its hosts, teal; the others serve at the route door only.
    expect(out).toContain('<span class="mono host">expensa-stage-fecode.apps.fecode.dev</span>');
    expect(count(out, 'route door only')).toBe(2);
    expect(out).toContain('<span class="badge bad">1 open</span>');
    expect(count(out, '<span class="muted">0</span>')).toBe(2);
  });

  it('says why it is empty', () => {
    expect(table([])).toContain('No environments in this tenant.');
    expect(table([], true)).toContain('No environment has these labels.');
    expect(table(null)).not.toContain('No environment');
  });
});

describe('EnvOverview', () => {
  const overview = (st) => html(<EnvOverview st={st} tenant={T} navigate={nav} />);

  it('shows the rollout: deployment 7 rolls while 6 serves, the open incident linked', () => {
    const out = overview(STAGE);
    expect(out).toContain('class="rollout moving"');
    expect(out).toContain('href="/tenants/fecode/environments/expensa-stage/deployments/7"');
    expect(text(<EnvOverview st={STAGE} tenant={T} navigate={nav} />)).toContain('deployment 6 serves meanwhile');
    expect(out).toContain('href="/tenants/fecode/environments/expensa-stage/incidents" class="bad">1 open</a>');
    expect(out).not.toContain('rollout-reason');
  });

  it("shows a failed rollout's reason, and that the deployed deployment keeps serving", () => {
    const reason = 'service expense-service: a new instance (replica 1) exited 3 times';
    const st = { ...STAGE, rollout: { ...STAGE.rollout, state: 'failed', reason } };
    const out = overview(st);
    expect(out).toContain('class="rollout failed"');
    expect(out).toContain(`<div class="rollout-reason">${reason}</div>`);
    expect(text(<EnvOverview st={st} tenant={T} navigate={nav} />)).toContain('deployment 6 keeps serving');
  });

  it("shows a deploying deployment's reason without a failure", () => {
    const st = { ...STAGE, rollout: { ...STAGE.rollout, reason: 'secrets need --secret-key-file' } };
    expect(overview(st)).toContain('<div class="rollout-reason">secrets need --secret-key-file</div>');
  });

  it('groups the objects by kind, one row each, its name a link to its page', () => {
    const out = overview(STAGE);
    expect(out).toContain('<div class="sect">services <span class="count">· 3</span></div>');
    expect(out).toContain('<div class="sect">addons <span class="count">· 2</span></div>');
    expect(out).toContain('<div class="sect">jobs <span class="count">· 1</span></div>');
    expect(count(out, '<tr class="obj-row')).toBe(6);
    expect(out).toContain('href="/tenants/fecode/environments/expensa-stage/objects/service/expense-service"');
    expect(out).toContain('href="/tenants/fecode/environments/expensa-stage/objects/addon/postgres"');
    expect(out).toContain('href="/tenants/fecode/environments/expensa-stage/objects/job/migrate"');
    // Every object links its logs; no node shows while the replicas are folded.
    expect(count(out, '> Logs</a>')).toBe(6);
    expect(out).not.toContain('href="/nodes/');
  });

  it("sums an image service up: its image short, ready, restarts, the deployments it runs; its replicas folded", () => {
    const spec = response('GET_environments_env_deployments_n.200').spec;
    const out = html(<EnvOverview st={STAGE} tenant={T} navigate={nav} spec={spec} />);
    const all = text(<EnvOverview st={STAGE} tenant={T} navigate={nav} spec={spec} />);
    expect(out).toContain('title="ghcr.io/acme/expense:1.4\npinned to ghcr.io/acme/expense@sha256:863e2ebe9c3820e6d1b20ef70fdb2bebe152f5f0fbbddb7c7e5f5144c438658a">expense:1.4</span>');
    expect(all).toContain('1/2 3 instances');
    expect(all).toContain('1/1 1 replica');
    // Three restarts, the last exit 1; replica 1 of deployment 6 still runs, muted.
    expect(out).toContain('<td class="col-num mono warntext" title="last exit 1">3<span class="bad small"> · exit 1</span></td>');
    expect(out).toContain('<span class="mono"><span class="muted" title="an older deployment still runs here">6</span> · 7</span>');
    expect(count(out, 'aria-expanded="false"')).toBe(5);
    expect(count(out, 'title="instance ')).toBe(0);
    // Without the spec, the pin the instances run.
    expect(overview(STAGE)).toContain('>web@4e1f0c2a9b7d</span>');
  });

  it("shows a service's traffic of the last hour", () => {
    const traffic = { services: [{ service: 'web', requests: 12840, errors: 64, p95Ms: 41 }] };
    const all = text(<EnvOverview st={STAGE} tenant={T} navigate={nav} traffic={traffic} />);
    expect(all).toContain('13k req · 0.5 % err · p95 41.0 ms');
  });

  it("unfolds an object's instances: replica, phase, state, address, deployment, restarts, last exit, logs, the node last", () => {
    const svc = STAGE.objects.find((o) => o.name === 'expense-service');
    const row = (ec) => html(<table><tbody><ObjectRow o={svc} st={STAGE} tenant={T} navigate={nav} ec={ec} open /></tbody></table>);
    const out = row(null);
    expect(count(out, 'title="instance ')).toBe(3);
    expect(out).toContain('title="instance 0193d5e6-0000-7000-8000-000000000004"');
    expect(out).toContain('<tr class="inst-detail"><td colSpan="10" class="small detailcell">exited with code 1; restarting in 4s</td></tr>');
    expect(out).toContain('<td class="col-num mono warntext">3</td>');
    expect(out).toContain('<td class="col-num mono bad">1</td>');
    expect(out).toContain('>exited</span>');
    expect(out).toContain('<td class="mono small muted cut-start" title="fd6b:a01:3::15">fd6b:a01:3::15</td>');
    expect(out).toContain('class="col-num mono muted" title="an older deployment&#x27;s instance">6</td>');
    expect(out).toContain('href="/tenants/fecode/environments/expensa-stage/logs/service/expense-service/1"');
    expect(out).toContain('href="/tenants/fecode/environments/expensa-stage/logs/service/expense-service/0"');
    // The node: small muted text, a link for an operator only.
    expect(out).toContain('<td class="small muted inst-node cut-start" title="n2">n2</td>');
    expect(out).not.toContain('href="/nodes/');
    expect(row({ ...ALL, operator: true })).toContain('href="/nodes/n2"');
  });

  it('shows an addon with type, version, plan and provisioning', () => {
    const all = text(<EnvOverview st={STAGE} tenant={T} navigate={nav} />);
    expect(all).toContain('postgres postgres 17 small 1/1 1 replica');
    const unprov = { ...STAGE, objects: [{ ...STAGE.objects.find((o) => o.name === 'nats'), provisioned: false }] };
    expect(text(<EnvOverview st={unprov} tenant={T} navigate={nav} />)).toContain('small · not provisioned');
    expect(html(<EnvOverview st={STAGE} tenant={T} navigate={nav} />))
      .toContain('href="/tenants/fecode/environments/expensa-stage/logs/addon/postgres"');
  });

  it("shows a job's last run", () => {
    const out = overview(STAGE);
    const all = text(<EnvOverview st={STAGE} tenant={T} navigate={nav} />);
    expect(all).toContain('migrate — with each deployment succeeded attempt 1 · exit 0');
    expect(out).toContain('href="/tenants/fecode/environments/expensa-stage/logs/job/migrate"');
    const failed = { ...STAGE, objects: [{ kind: 'job', name: 'migrate', lastRun: {
      deployment: 7, attempt: 2, phase: 'failed', exitCode: null, finishedAt: '2026-10-18T14:03:37Z',
      detail: 'the node did not report an exit status' } }] };
    expect(overview(failed)).toContain('<td colSpan="7" class="small detailcell">the node did not report an exit status</td>');
    const never = { ...STAGE, objects: [{ kind: 'job', name: 'seed', lastRun: null }] };
    expect(text(<EnvOverview st={never} tenant={T} navigate={nav} />)).toContain('not run yet');
  });

  it('shows attached services with their sandbox, phase, restarts, last exit and detail, without instances', () => {
    const out = overview(PREVIEW);
    const all = text(<EnvOverview st={PREVIEW} tenant={T} navigate={nav} />);
    expect(all).toContain('web attached · 0193b0aa');
    expect(out).toContain('href="/tenants/fecode/sandboxes/0193b0aa-1111-7222-8333-444455556601"');
    expect(out).toContain('<td colSpan="7" class="small detailcell">cwd /workspace/expensa/web does not exist yet</td>');
    expect(out).toContain('title="last exit 2">1<span class="bad small"> · exit 2</span>');
    // Only the two addons fold instances.
    expect(count(out, 'class="linkbtn obj-fold"')).toBe(2);
    expect(out).toContain('class="rollout running"');
    expect(all).toContain('every object matches it');
  });

  it('lists the routes with their ports resolved, each service a link to its page', () => {
    const out = overview(PREVIEW);
    expect(out).toContain('<td class="mono">/api/expense</td><td class="mono"><a href="/tenants/fecode/environments/expensa-login-3f9a/objects/service/expense-service">expense-service</a></td><td class="col-num mono">8081</td>');
    expect(text(<EnvOverview st={PREVIEW} tenant={T} navigate={nav} />))
      .toContain('The route door serves them all at /api/v1/environments/expensa-login-3f9a/http/… to a route token');
  });

  it('a new environment: no deployment, no objects, no routes', () => {
    const all = text(<EnvOverview st={response('POST_environments.201')} tenant={T} navigate={nav} />);
    expect(all).toContain('No deployment yet: nothing runs.');
    expect(all).toContain('No objects: the environment has no deployment yet.');
    expect(all).toContain('No routes.');
    expect(all).toContain('open incidents none');
  });

  it('an environment being deleted says so', () => {
    expect(text(<EnvOverview st={{ ...STAGE, deleting: true }} tenant={T} navigate={nav} />))
      .toContain('deleting ferund removes its instances');
  });
});

describe('DeploymentTable', () => {
  const deployments = response('GET_environments_env_deployments.200').deployments;
  const table = (list, latest = 7) => html(<DeploymentTable deployments={list} latest={latest} tenant={T} env="expensa-stage" navigate={nav} />);

  it('lists number, state, origin, author, message, annotations and tags, the latest marked', () => {
    const out = table(deployments);
    expect(count(out, 'class="clickable"')).toBe(3);
    expect(out).toContain('href="/tenants/fecode/environments/expensa-stage/deployments/7"');
    expect(count(out, '<span class="badge acc">latest</span>')).toBe(1);
    expect(out).toContain('title="tag"><svg');
    expect(out).toMatch(/<\/svg> v1\.3<\/span>/);
    // Without the actions no revert is offered.
    expect(out).not.toContain('Revert to this');
    expect(out).toContain('>replaced</span>');
    expect(out).toContain('title="fecode:0193a000-aaaa-7bbb-8ccc-000000000001"');
    expect(out).toContain('stage follows main at revision 12');
    expect(out).toContain('>fecode.dev/kind=<b>stage</b></span>');
    expect(out).toContain('title="fecode.dev/revision=0193c111-2222-7333-8444-000000000012"');
    expect(text(<DeploymentTable deployments={deployments} latest={7} tenant={T} env="expensa-stage" navigate={nav} />))
      .toContain('apply');
  });

  it("shows a failed deployment's reason and a revert's origin", () => {
    const failed = { ...deployments[0], number: 8, state: 'failed', origin: 'revert', originRef: '6',
      reason: 'service expense-service did not roll out within 10m0s' };
    const out = table([failed, ...deployments], 8);
    expect(out).toContain('<div class="bad">service expense-service did not roll out within 10m0s</div>');
    expect(out).toContain('revert to 6');
  });

  it('says when there are none', () => {
    expect(table([])).toContain('No deployments yet.');
  });
});

describe('DeploymentView', () => {
  const rel = response('GET_environments_env_deployments_n.200');

  it('shows the deployment with its annotations', () => {
    const all = text(<DeploymentView rel={rel} />);
    expect(all).toContain('deployment 7 deploying');
    expect(all).toContain('origin apply base deployment 6');
    expect(all).toContain('message stage follows main at revision 12');
    expect(all).toContain('fecode.dev/revision 0193c111-2222-7333-8444-000000000012');
    expect(all).toContain('finished —');
  });

  it('shows its diff against the deployment before, with effects and fields from → to', () => {
    const out = html(<DeploymentView rel={rel} />);
    expect(text(<DeploymentView rel={rel} />)).toContain('changes against deployment 6');
    expect(count(out, 'class="diff-entry"')).toBe(3);
    expect(out).toContain('<span class="badge acc">update</span><span class="mono">service/expense-service</span>');
    expect(out).toContain('<span class="small muted">replaces 2 pods</span>');
    expect(out).toContain('<span class="small muted">restarts</span>');
    expect(out).toContain('<td class="mono small diff-path">image</td>');
    expect(out).toContain('<td class="mono small diff-from">ghcr.io/acme/expense:1.3 (sha256:2780e52e5ffc077629f9b26f1469ccc2b0bc37d1778d4038d4a9fc587ff89ab5)</td>');
    expect(out).toContain('<td class="mono small diff-to">ghcr.io/acme/expense:1.4 (sha256:863e2ebe9c3820e6d1b20ef70fdb2bebe152f5f0fbbddb7c7e5f5144c438658a)</td>');
  });

  it('shows its normalised spec', () => {
    const out = html(<DeploymentView rel={rel} />);
    expect(out).toContain('<details class="specbox">');
    expect(out).toContain('&quot;imageAsWritten&quot;: &quot;ghcr.io/acme/expense:1.4&quot;');
    expect(out).toContain('&quot;internal-token&quot;: {\n      &quot;version&quot;: 3\n    }');
  });

  it('the first deployment changes against nothing; a failed one shows its reason', () => {
    const first = { ...rel, number: 1, base: 0, diff: [], state: 'failed', reason: 'job migrate failed its last attempt (2 of 2) with code 3' };
    const out = html(<DeploymentView rel={first} />);
    expect(text(<DeploymentView rel={first} />)).toContain('changes (the first deployment) No changes.');
    expect(out).toContain('<div class="error">job migrate failed its last attempt (2 of 2) with code 3</div>');
    expect(text(<DeploymentView rel={first} />)).toContain('base none');
  });
});

describe('DiffView', () => {
  it('tones adds, removes and the effects that cost data', () => {
    const out = html(<DiffView diff={[
      { kind: 'addon', name: 'valkey', change: 'add', fields: [], effect: 'in place' },
      { kind: 'addon', name: 'nats', change: 'remove', fields: [], effect: 'deletes data' },
      { kind: 'service', name: 'web', change: 'update', effect: 'replaces 1 pod',
        fields: [{ path: 'replicas', from: 1, to: 2 }, { path: 'env.TOKEN', from: null, to: { secret: 'internal-token', version: 3 } }] },
    ]} />);
    expect(out).toContain('<span class="badge ok">add</span>');
    expect(out).toContain('<span class="badge bad">remove</span>');
    expect(out).toContain('<span class="small bad">deletes data</span>');
    expect(out).toContain('<td class="mono small diff-from">1</td><td class="muted diff-arrow">→</td><td class="mono small diff-to">2</td>');
    expect(out).toContain('<td class="mono small diff-from">—</td>');
    expect(out).toContain('{&quot;secret&quot;:&quot;internal-token&quot;,&quot;version&quot;:3}');
    // Adds and removes have no fields to list.
    expect(count(out, 'class="tbl diff-tbl"')).toBe(1);
  });

  it('says when nothing changes', () => {
    expect(html(<DiffView diff={[]} />)).toContain('No changes.');
    expect(html(<DiffView diff={null} empty="Nothing." />)).toContain('Nothing.');
  });
});

describe('IncidentTable', () => {
  const incidents = response('GET_environments_env_incidents.200').incidents;

  it('lists condition, object, deployment, summary, opened and closed; a closed one dimmed', () => {
    const out = html(<IncidentTable incidents={incidents} tenant={T} env="expensa-stage" navigate={nav} />);
    expect(out).toContain('class="clickable"');
    expect(out).toContain('class="clickable dim"');
    expect(out).toContain('href="/tenants/fecode/environments/expensa-stage/incidents/12"');
    expect(out).toContain('<td class="nowrap">crash loop</td>');
    expect(out).toContain('<td class="nowrap">job failed</td>');
    expect(out).toContain('<td class="mono small nowrap"><a href="/tenants/fecode/environments/expensa-stage/objects/service/expense-service">service/expense-service</a></td>');
    expect(out).toContain('expense-service (deployment 7) exited 3 times within 10 minutes, last with code 1');
    expect(out).toContain('<span class="bad small">open</span>');
    expect(out).toContain('class="sdot failed" title="open"');
    expect(out).toContain('class="sdot absent" title="closed"');
  });

  it('says when there are none', () => {
    expect(html(<IncidentTable incidents={[]} tenant={T} env="e" navigate={nav} />)).toContain('No incidents.');
    expect(html(<IncidentTable incidents={[]} openOnly tenant={T} env="e" navigate={nav} />)).toContain('No open incidents.');
  });
});

describe('IncidentView', () => {
  const inc = response('GET_environments_env_incidents_id.200');
  const view = (i) => html(<IncidentView inc={i} tenant={T} env="expensa-stage" navigate={nav} />);
  const words = (i) => text(<IncidentView inc={i} tenant={T} env="expensa-stage" navigate={nav} />);

  it('shows a crash loop with its exits and the end of the log', () => {
    const out = view(inc);
    expect(words(inc)).toContain('incident 12 open crash loop');
    expect(words(inc)).toContain('still open');
    expect(out).toContain('href="/tenants/fecode/environments/expensa-stage/logs/service/expense-service"');
    expect(out).toContain('href="/tenants/fecode/environments/expensa-stage/deployments/7"');
    expect(count(out, '<td class="col-num mono bad">1</td>')).toBe(3);
    expect(out).toContain('<pre class="logpre">2026/10/18 14:07:11 expense-service 1.4 starting on [::]:8080\n'
      + '2026/10/18 14:07:11 connect to nats: nats: authorization violation\n2026/10/18 14:07:12 exit status 1</pre>');
    expect(out).not.toContain('the diff against the deployed deployment');
    expect(out).not.toContain('placement');
  });

  it("shows a failed rollout's diff against the deployed deployment, and an exit without a status", () => {
    const failed = {
      ...inc, id: '13', condition: 'rollout_failed',
      summary: 'service expense-service: a new instance (replica 1) exited 3 times',
      evidence: { exits: [{ at: '2026-10-18T14:05:01Z', code: null }], logTail: [],
        diff: response('GET_environments_env_deployments_n.200').diff },
    };
    const out = view(failed);
    expect(words(failed)).toContain('incident 13 open rollout failed');
    expect(out).toContain('<td class="col-num mono bad">no status</td>');
    expect(words(failed)).toContain('the diff against the deployed deployment');
    expect(count(out, 'class="diff-entry"')).toBe(3);
    expect(out).not.toContain('the end of the log');
  });

  it('shows what a deployment past its deadline waited for', () => {
    const late = response('GET_environments_env_incidents_id.200.rollout');
    const out = view(late);
    expect(words(late)).toContain('incident 13 closed rollout failed');
    expect(words(late)).toContain('what the deployment waited for');
    expect(out).toContain('<pre class="logpre">addon postgres is not ready\nservice expense-service waits for the addons\n'
      + 'addon/postgres replica 0: provisioning step provision exited 1');
    expect(words(late)).toContain('the diff against the deployed deployment');
    expect(out).not.toContain('No evidence recorded.');
  });

  it('shows why an instance found no place', () => {
    const starved = {
      ...inc, id: '14', object: 'addon/postgres', condition: 'no_capacity', closedAt: '2026-10-18T15:00:00Z',
      summary: 'addon/postgres replica 0 found no capacity for 2m0s',
      evidence: { placement: 'no capacity (retryable): no feasible node; n1: memory' },
    };
    const out = view(starved);
    expect(words(starved)).toContain('incident 14 closed no capacity');
    expect(out).toContain('<pre class="logpre">no capacity (retryable): no feasible node; n1: memory</pre>');
    expect(out).toContain('href="/tenants/fecode/environments/expensa-stage/logs/addon/postgres"');
    expect(out).not.toContain('still open');
  });

  it('says when there is no evidence', () => {
    expect(words({ ...inc, evidence: {} })).toContain('No evidence recorded.');
  });
});

describe('the logs', () => {
  it('show the lines of an image pod and of an attached process', () => {
    const lines = response('GET_environments_env_objects_kind_name_logs.200').lines;
    expect(html(<LogsBody result={{ lines, source: 'container' }} />)).toContain(
      '<pre class="logpre">2026/10/18 14:07:11 expense-service 1.4 starting on [::]:8080\n');
    const proc = response('GET_environments_env_objects_kind_name_logs.200.process').lines;
    expect(html(<LogsBody result={{ lines: proc, source: 'process' }} />))
      .toContain('GET /api/expense/reports 200 3ms</pre>');
    expect(html(<LogsBody result={{ lines: [], source: 'container' }} />)).toContain('No lines yet.');
    expect(html(<LogsBody result={null} />)).toContain('Loading…');
  });

  // The refusals come from ferund's fixtures through the client, as the
  // view reads them.
  async function refusal(name, call) {
    setActiveTenant(T);
    serveFixtures(name);
    const e = await call().then(() => null, (x) => x);
    expect(e).not.toBeNull();
    return html(<LogsBody result={{ problem: logsProblem(e) }} />);
  }

  it("show a node without container logs (501) as a message", async () => {
    const out = await refusal('GET_environments_env_objects_kind_name_logs.501',
      () => api.objectLogs('expensa-stage', 'addon', 'postgres'));
    expect(out).toContain('class="message"');
    expect(out).toContain('<strong>this node does not serve container logs yet</strong>');
    expect(out).not.toContain('class="error"');
  });

  it('show a paused instance (409) as a message', async () => {
    const out = await refusal('GET_environments_env_objects_kind_name_logs.409',
      () => api.objectLogs('expensa-login-3f9a', 'addon', 'nats'));
    expect(out).toContain('class="message"');
    expect(out).toContain('<strong>paused</strong>');
    expect(out).toContain('ferund never wakes a paused instance for its logs');
    expect(out).not.toContain('class="error"');
  });

  it('show any other refusal as an error', async () => {
    const out = await refusal('GET_environments_env_objects_kind_name_logs.404',
      () => api.objectLogs('expensa-stage', 'service', 'nope'));
    expect(out).toContain('class="error"');
    expect(out).toContain('no such object: service/nope');
  });

  it("offer the status's objects, an image service's replicas and sources, the words, levels and time", () => {
    const search = (item, replica = null, query = '') => html(<LogSearch tenant={T} env="expensa-stage" st={STAGE}
      focus={focusOf(item, replica, query)} navigate={nav} tail={null} />);
    const out = search('service/expense-service', 1, '?q=timeout&level=error,warn');
    expect(out).toContain('<button type="button" class="chipbtn mono objchip on"><span class="muted">service/</span>expense-service</button>');
    expect(out).toContain('<button type="button" class="chipbtn mono on" aria-pressed="true">1</button>');
    expect(out).toContain('aria-pressed="false">every</button>');
    // A replica is a container's: its output, or what it sent out.
    expect(out).not.toContain('>requests</button>');
    expect(out).toContain('aria-pressed="true">output</button>');
    expect(search('service/expense-service')).toContain('aria-pressed="false">requests</button>');
    expect(out).toContain('value="timeout"');
    expect(out).toContain('class="chipbtn mono lg-lvlchip lvl-error on"');
    expect(out).toContain('class="chipbtn mono lg-lvlchip lvl-warn on"');
    expect(out).toContain('class="chipbtn mono lg-lvlchip lvl-info"');
    expect(out).toContain('class="chipbtn on" aria-pressed="true">1 h</button>');
    // An addon has output only, no replicas to pick; every object has
    // no replica row either.
    expect(search('addon/postgres')).not.toContain('>replica<');
    expect(search('addon/postgres')).not.toContain('>requests</button>');
    expect(search('').match(/class="chipbtn on">every object/)).toBeTruthy();
    // A trace's search reads all the store keeps, and can be left.
    const tr = search('', null, '?trace=4bf92f3577b34da6a3ce929d0e0e4736');
    expect(tr).toContain('<code>4bf92f3577b34da6a3ce929d0e0e4736</code>');
    expect(tr).toContain('aria-pressed="true">all kept</button>');
    expect(tr).toContain('leave the trace');
  });

  it("show an attached service's process output alone, from its node", () => {
    const out = html(<LogSearch tenant={T} env="expensa-preview" st={PREVIEW} focus={focusOf('service/web', null, '')}
      navigate={nav} tail={<p className="tail">the tail</p>} tailOnly />);
    expect(out).toContain('<p class="tail">the tail</p>');
    expect(out).not.toContain('aria-label="search the logs"');
  });
});

// The view calls the environments' routes of the client and no other:
// its reads, and its operations (ADR 0018), each behind its role.
describe('the routes it calls', () => {
  it('are the reads and the operations of an environment', () => {
    const src = ['./Environments.jsx', './EnvTabs.jsx', './LogSearch.jsx', './Traces.jsx']
      .map((f) => readFileSync(new URL(f, import.meta.url), 'utf8')).join('\n');
    const used = [...new Set([...src.matchAll(/\bapi\.(\w+)\(/g)].map((m) => m[1]))].sort();
    expect(used).toEqual([
      'accessLogs', 'checkBinding', 'cloneEnvironment', 'deleteEnvironment', 'deleteSecret', 'deleteTag', 'findTraces',
      'followLogs', 'getDeployment', 'getEnvironment', 'getIncident', 'getTrace', 'listDeployments', 'listEnvironments',
      'listIncidents', 'listSecrets', 'listTags', 'mintRouteToken', 'objectLogs', 'putSecret', 'putTag', 'restartService',
      'revertDeployment', 'revokeRouteTokens', 'runJob', 'searchLogs', 'setPublic', 'traffic',
    ]);
  });
});

// The environment's head and its operations (ADR 0018), each with the
// role it needs.
const ALL = { write: true, admin: true, del: true, clone: true, operator: false };
const NONE = { write: false, admin: false, del: false, clone: false, operator: false };

describe('envCan', () => {
  it('reads the roles: the higher of the tenant grant and the environment grant', () => {
    expect(envCan({ role: 'read', environments: [{ id: 'e1', name: 'stage', role: 'write' }] }, 'stage'))
      .toEqual({ write: true, admin: false, del: false, clone: false, operator: false });
    expect(envCan({ role: 'write', environments: [] }, 'stage')).toEqual({ write: true, admin: false, del: true, clone: true, operator: false });
    expect(envCan({ role: '', environments: [{ id: 'e1', name: 'stage', role: 'admin' }] }, 'e1'))
      .toEqual({ write: true, admin: true, del: true, clone: false, operator: false });
    expect(envCan({ role: 'read', environments: [] }, 'stage')).toEqual(NONE);
    expect(envCan({ role: 'admin', environments: [], operator: true }, 'stage').operator).toBe(true);
  });
});

describe('EnvHead', () => {
  const head = (st, ec = ALL, extra = {}) => html(<EnvHead st={st} env={st.name} tenant={T} ec={ec} domains={[]} editing={false}
    lastOk={0} changed={nav} navigate={nav} {...extra} />);

  it('names the environment, its state in words, Revert to the one that serves, and Edit', () => {
    const out = head(STAGE);
    expect(out).toContain('<span class="kind">Environment</span><h1 class="mono">expensa-stage</h1>');
    expect(out).toContain('7 deploying · 6 serves');
    expect(out).toContain('Revert to 6');
    expect(out).toContain('href="/tenants/fecode/environments/expensa-stage/builder"');
    expect(out).toContain('class="button primary"');
    // public at its host, teal and a link
    expect(out).toContain('<a class="mono host" href="https://expensa-stage-fecode.apps.fecode.dev"');
    expect(out).toContain('fecode.class=<b>stage</b>');
  });

  it('offers no revert while the latest serves, and disables it without write', () => {
    const deployed = { ...STAGE, deployed: 7, rollout: { ...STAGE.rollout, state: 'deployed' } };
    expect(head(deployed)).not.toContain('Revert to');
    expect(head(deployed)).toContain('7 deployed');
    expect(head(STAGE, NONE)).toContain('disabled="" title="needs write on this environment"');
  });

  it('shows Done while the builder is open, and the default host that answers 404 while a domain is served', () => {
    const st = { ...STAGE, hosts: ['kino.example.com'], hostLabel: 'stage--kino' };
    const out = html(<EnvHead st={st} env={st.name} tenant={T} ec={ALL} editing lastOk={0} changed={nav} navigate={nav}
      domains={[{ id: 'd', name: 'kino.example.com', state: 'served', environment: { id: st.id, name: st.name } }]} />);
    expect(out).toContain('Done');
    expect(out).not.toContain('class="button primary"');
    expect(text(<EnvHead st={st} env={st.name} tenant={T} ec={ALL} editing={false} lastOk={0} changed={nav} navigate={nav}
      domains={[{ id: 'd', name: 'kino.example.com', state: 'served', environment: { id: st.id, name: st.name } }]} />))
      .toContain('default host stage--kino (404 while a domain is served)');
  });

  it('says not public for one the route door alone serves', () => {
    expect(text(<EnvHead st={{ ...PREVIEW, public: false, hosts: [] }} env="p" tenant={T} ec={ALL} domains={[]} lastOk={0}
      changed={nav} navigate={nav} />)).toContain('not public · route door only');
  });
});

describe('the actions on objects', () => {
  const card = (o, ec, spec = null) => html(<table><tbody><ObjectRow o={o} st={STAGE} tenant={T} navigate={nav}
    spec={spec} ec={ec} open /></tbody></table>);
  const svc = STAGE.objects.find((o) => o.name === 'expense-service');
  const job = STAGE.objects.find((o) => o.kind === 'job');

  it('restart a service and run a job, with write on the environment', () => {
    expect(card(svc, ALL)).toContain('Restart');
    expect(card(svc, ALL)).toContain('<table class="tbl inst-tbl">');
    expect(card(job, ALL)).toContain('Run now');
    expect(card(job, ALL)).not.toContain('>manual</span>');
    expect(card(svc, NONE)).toContain('disabled="" title="needs write on this environment"');
    // Without ec (a read-only render) no action shows.
    expect(card(svc, null)).not.toContain('Restart');
  });

  it("marks a job on: manual, its Run now primary", () => {
    const out = card(job, ALL, { jobs: { migrate: { on: 'manual' } } });
    expect(out).toContain('>manual</span>');
    expect(out).toContain('class="mini primary"');
  });

  it("lists the latest deployment's bindings in the overview, each with Check", () => {
    const spec = { services: { 'expense-service': { bindings: ['erp'] } }, bindings: { erp: { upstream: 'https://erp.example', credential: { kind: 'basic' } } } };
    const out = html(<EnvOverview st={STAGE} tenant={T} navigate={nav} spec={spec} ec={ALL} />);
    expect(out).toContain('bindings <span class="count">· 1</span>');
    expect(out).toContain('https://erp.example');
    expect(text(<EnvOverview st={STAGE} tenant={T} navigate={nav} spec={spec} ec={ALL} />)).toContain('name · basic · used by service/expense-service');
    expect(out).toContain('Check');
  });

  it('lists the bindings on Secrets & credentials, each with where its credential comes from', () => {
    const spec = {
      services: { api: { bindings: ['erp', 'gh'] } },
      bindings: {
        erp: { upstream: 'https://erp.example', credential: { kind: 'oauth2' }, tenantCredential: { name: 'erp', version: 3 } },
        gh: { upstream: 'https://api.github.com', credential: { kind: 'headers', headers: { Authorization: { secret: 'gh-token' } } } },
      },
    };
    const cards = specBindings(spec).map((b) => text(<BindingCard key={b.name} b={b} env="stage" ec={ALL} tenant={T} navigate={nav} />));
    expect(cards[0]).toContain('credential erp · version 3 of the tenant');
    expect(cards[0]).toContain('Edit in New deployment');
    expect(cards[1]).toContain('secret gh-token of this environment');
  });

  it("shows a binding check's result", () => {
    const b = { name: 'erp', upstream: 'https://erp.example', mode: 'name', credential: 'oauth2', users: [] };
    const res = { ok: false, addresses: ['203.0.113.40'], tls: null, token: null, probe: { status: 401, latencyMs: 80 }, error: 'unauthorized' };
    const all = text(<BindingCard b={b} env="e" ec={ALL} result={res} />);
    expect(all).toContain('result not ok');
    expect(all).toContain('error unauthorized');
    expect(all).toContain('addresses 203.0.113.40');
    expect(all).toContain('probe HTTP 401 in 80 ms');
  });
});

describe('reverts and tags', () => {
  const deployments = response('GET_environments_env_deployments.200').deployments;
  const tags = response('GET_environments_env_tags.200').tags;

  it('offers a revert on each deployment but the latest', () => {
    const out = html(<DeploymentTable deployments={deployments} latest={7} tenant={T} env="expensa-stage" navigate={nav}
      ec={ALL} onRevert={async () => ({})} />);
    expect(count(out, 'Revert to this')).toBe(2);
    expect(out).toContain("title=\"a new deployment with deployment 6&#x27;s spec\"");
  });

  it('lists the tags with move and delete, disabled without write', () => {
    const out = html(<TagsBox env="expensa-stage" tags={tags} ec={ALL} tenant={T} navigate={nav} onChange={nav} />);
    expect(out).toContain('v1.3');
    expect(out).toContain('href="/tenants/fecode/environments/expensa-stage/deployments/6"');
    expect(out).toContain('>Move</button>');
    expect(out).toContain('Delete');
    expect(html(<TagsBox env="e" tags={tags} ec={NONE} tenant={T} navigate={nav} onChange={nav} />))
      .toContain('title="needs write on this environment"');
    expect(text(<TagsBox env="e" tags={[]} ec={ALL} tenant={T} navigate={nav} onChange={nav} />)).toContain('No tags.');
  });
});

describe('the traffic tab', () => {
  it('lists each service: requests, errors, p50, p95 and a sparkline', () => {
    const t = response('GET_environments_env_traffic.200');
    const out = html(<TrafficTable traffic={t} />);
    expect(out).toContain('<td class="mono strong">api</td>');
    expect(out).toContain('3.1 ms');
    expect(out).toContain('3.4 ms');
    expect(out).toContain('<svg class="spark"');
    expect(out).toContain('<polyline class="spark-req"');
    expect(html(<TrafficTable traffic={{ services: [] }} />)).toContain('No requests through the ingress in this window.');
  });

  it("shows the ingress's access log, a trace linked to the Traces tab", () => {
    const recs = response('GET_environments_env_access-logs.200').records;
    const out = html(<AccessTable records={[...recs].reverse()} tenant={T} env="expensa-stage" navigate={nav} />);
    expect(out).toContain('href="/tenants/fecode/environments/expensa-stage/traces/0af7651916cd43dd8448eb211c80319c"');
    expect(out).toContain('class="col-num mono bad" title="5xx">503</td>');
    expect(out).toContain('held 2.4 s while the service woke');
    expect(out).toContain('/api/login');
    expect(out.indexOf('0af76519')).toBeLessThan(out.indexOf('4bf92f35'));
  });
});

describe('the secrets tab', () => {
  const secrets = response('GET_environments_env_secrets.200').secrets;
  it('lists names, versions and who set them, never a value', () => {
    const out = html(<SecretsTable secrets={secrets} ec={ALL} editing="" setEditing={nav} onPut={nav} onDelete={nav} />);
    expect(out).toContain('<td class="mono strong">internal-token</td><td class="col-num mono">v3</td>');
    expect(out).toContain('fecode:0193a000-aaaa-7bbb-8ccc-000000000001');
    expect(out).toContain('Set value');
    expect(out).not.toContain('type="password"');
    const editing = html(<SecretsTable secrets={secrets} ec={ALL} editing="internal-token" setEditing={nav} onPut={nav} onDelete={nav} />);
    expect(editing).toContain('type="password"');
    expect(editing).toContain('shown never again once stored');
    expect(html(<SecretsTable secrets={secrets} ec={NONE} editing="" setEditing={nav} onPut={nav} onDelete={nav} />))
      .toContain('disabled="" title="needs write on this environment"');
  });
});

describe('the domains & access tab', () => {
  it('publishes with a host label, admin on the environment, and says the first publish rolls', () => {
    const off = { ...STAGE, public: false, hosts: [] };
    const all = text(<PublicBox env="e" st={off} ec={ALL} domains={[]} changed={nav} />);
    expect(all).toContain('not public');
    expect(all).toContain('Publish');
    expect(all).toContain('The first publish rolls the image services once');
    const on = html(<PublicBox env="e" st={STAGE} ec={ALL} domains={[]} changed={nav} />);
    expect(on).toContain('Unpublish');
    expect(on).toContain('<a class="mono host" href="https://expensa-stage-fecode.apps.fecode.dev"');
    expect(html(<PublicBox env="e" st={off} ec={{ ...ALL, admin: false }} domains={[]} changed={nav} />))
      .toContain('title="needs admin on this environment"');
  });

  it('lists the domains bound to it, linked to their pages', () => {
    const domains = [{ id: 'd-1', name: 'kino.example.com', state: 'served', verification: 'verified', environment: { id: STAGE.id, name: STAGE.name } },
      { id: 'd-2', name: 'x.example.org', state: 'served', environment: { id: 'o', name: 'o' } }];
    const out = html(<DomainsBox tenant={T} st={STAGE} domains={domains} navigate={nav} />);
    expect(out).toContain('href="/tenants/fecode/domains/d-1"');
    expect(out).not.toContain('x.example.org');
    expect(text(<DomainsBox tenant={T} st={STAGE} domains={[]} navigate={nav} />)).toContain('No domain is bound to this environment.');
  });

  it('mints route tokens and revokes them all, admin on the environment', () => {
    const all = text(<RouteTokenBox env="expensa-stage" ec={ALL} />);
    expect(all).toContain('/api/v1/environments/expensa-stage/http/…');
    expect(all).toContain('X-FeRun-Route-Token');
    expect(all).toContain('Mint a route token');
    expect(all).toContain('Revoke all');
    expect(count(html(<RouteTokenBox env="e" ec={NONE} />), 'title="needs admin on this environment"')).toBe(2);
  });
});

describe('the settings tab', () => {
  it('clones, saying data is not copied and attached services are dropped, write on the tenant', () => {
    const all = text(<CloneBox tenant={T} env="e" st={STAGE} ec={ALL} changed={nav} navigate={nav} />);
    expect(all).toContain('Its data is not copied');
    expect(all).toContain('attached services are dropped');
    expect(all).toContain('Its sandboxes and jobs stay here');
    expect(html(<CloneBox tenant={T} env="e" st={STAGE} ec={NONE} changed={nav} navigate={nav} />))
      .toContain('title="needs write on the tenant"');
  });

  it('deletes, saying what goes, admin on the environment or write on the tenant', () => {
    const all = text(<DeleteBox tenant={T} env="e" st={STAGE} ec={ALL} changed={nav} navigate={nav} />);
    expect(all).toContain('removes its instances and their volumes, with its addons\' data');
    expect(all).toContain('Delete environment');
    expect(html(<DeleteBox tenant={T} env="e" st={STAGE} ec={NONE} changed={nav} navigate={nav} />))
      .toContain('title="needs admin on this environment or write on the tenant"');
  });
});
