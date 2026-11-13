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
  BindingCard, DiffView, EnvHead, EnvTable, IncidentTable, IncidentView, InstanceNote, InstanceTable, LogsBody, LogsTab, DeploymentTable,
  DeploymentView, TagsBox, envCan,
} from './Environments.jsx';
import { AboutBox, EnvOverview } from './EnvOverview.jsx';
import { activityItems } from './envs.js';
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
    // and whom it admits (ADR 0054); the others admit nobody at a host.
    expect(count(out, '<div class="muted small">visitors · fecode</div>')).toBe(1);
    expect(out).not.toContain('>anyone<');
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
  const OPEN = response('GET_environments_env_incidents.200.open').incidents;
  const overview = (st, extra = {}) => html(<EnvOverview st={st} tenant={T} navigate={nav} {...extra} />);
  const words = (st, extra = {}) => text(<EnvOverview st={st} tenant={T} navigate={nav} {...extra} />);

  it('says first what needs someone: the open incident with the end of its log and the way to its object', () => {
    const out = overview(STAGE, { incidents: OPEN });
    const all = words(STAGE, { incidents: OPEN });
    expect(out).toContain('<section class="statecard failed">');
    expect(all).toContain('expense-service crash loop');
    expect(all).toContain('expense-service (deployment 7) exited 3 times within 10 minutes, last with code 1');
    expect(out).toContain('<pre class="statecard-log">2026/10/18 14:07:11 expense-service 1.4 starting on [::]:8080\n'
      + '2026/10/18 14:07:11 connect to nats: nats: authorization violation\n2026/10/18 14:07:12 exit status 1</pre>');
    expect(out).toContain('href="/tenants/fecode/environments/expensa-stage/objects/service/expense-service" class="button primary"');
    expect(out).toContain('href="/tenants/fecode/environments/expensa-stage/incidents/12"');
    expect(out).toContain('href="/tenants/fecode/environments/expensa-stage/deployments/7"');
  });

  it('shows the rollout: deployment 7 rolls while 6 serves', () => {
    const out = overview(STAGE);
    expect(out).toContain('<section class="statecard moving">');
    expect(words(STAGE)).toContain('Deployment 7 deploying');
    expect(words(STAGE)).toContain('Deployment 6 serves meanwhile.');
    expect(out).not.toContain('ov-reason');
    // No all-clear while the incidents are unread.
    expect(out).not.toContain('statecard running');
  });

  it("shows a failed rollout's reason, and that the deployed deployment keeps serving", () => {
    const reason = 'service expense-service: a new instance (replica 1) exited 3 times';
    const st = { ...STAGE, rollout: { ...STAGE.rollout, state: 'failed', reason } };
    expect(overview(st)).toContain('<section class="statecard failed">');
    expect(overview(st)).toContain(`<span class="ov-reason">${reason}</span>`);
    expect(words(st)).toContain('Deployment 6 keeps serving.');
  });

  it("shows a deploying deployment's reason without a failure", () => {
    const st = { ...STAGE, rollout: { ...STAGE.rollout, reason: 'secrets need --secret-key-file' } };
    expect(overview(st)).toContain('<span class="ov-reason">secrets need --secret-key-file</span>');
    expect(overview(st)).not.toContain('statecard failed');
  });

  it('says once that every object matches the deployment, when no incident is open', () => {
    const all = words(PREVIEW, { incidents: [] });
    expect(all).toContain('Every object matches deployment 3');
    expect(all).toContain('3 services and 2 addons run as deployed. No open incidents.');
    expect(overview(PREVIEW, { incidents: [] })).toContain('<section class="statecard running">');
  });

  it('shows the objects by kind as cards, each a link to its page', () => {
    const out = overview(STAGE);
    expect(out).toContain('<h2 class="ov-h">Services <span class="count">3</span></h2>');
    expect(out).toContain('<h2 class="ov-h">Addons <span class="count">2</span></h2>');
    expect(out).toContain('<h2 class="ov-h">Jobs <span class="count">1</span></h2>');
    expect(count(out, '<div class="ocard ')).toBe(6);
    expect(out).toContain('href="/tenants/fecode/environments/expensa-stage/objects/service/expense-service" class="ocard-name"');
    expect(out).toContain('href="/tenants/fecode/environments/expensa-stage/objects/addon/postgres" class="ocard-name"');
    expect(out).toContain('href="/tenants/fecode/environments/expensa-stage/objects/job/migrate" class="ocard-name"');
    // The instances and the nodes are on the objects' pages.
    expect(count(out, 'title="instance ')).toBe(0);
    expect(out).not.toContain('href="/nodes/');
  });

  it('sums an image service up: its image short, ready, and what is wrong: its restarts and last exit', () => {
    const spec = response('GET_environments_env_deployments_n.200').spec;
    const out = overview(STAGE, { spec });
    expect(out).toContain('<span class="mono" title="ghcr.io/acme/expense:1.4">expense:1.4</span>');
    expect(out).toContain('<div class="ocard failed">');
    expect(out).toContain('<span class="ocard-ready failed">1/2</span>');
    expect(words(STAGE, { spec })).toContain('3 restarts · last exit 1');
    // Without the spec, the pin the instances run.
    expect(overview(STAGE)).toContain('>web@4e1f0c2a9b7d</span>');
  });

  it("shows the traffic of the last hour: the sum, and a service's on its card", () => {
    const traffic = { services: [{ service: 'web', requests: 12840, errors: 64, p95Ms: 41, series: [{ requests: 1 }, { requests: 3 }] }] };
    const all = words(STAGE, { traffic });
    expect(all).toContain('13k requests · 0.5 % errors · p95 41.0 ms');
    expect(all).toContain('13k requests last hour · p95 41.0 ms');
    expect(count(overview(STAGE, { traffic }), 'class="wspark"')).toBe(2);
    expect(words(STAGE)).toContain('No numbers: the log store does not answer.');
    // Without numbers a card says nothing of requests.
    expect(words(STAGE)).not.toContain('no requests in the last hour');
    expect(words(STAGE)).toContain('reached by a route');
    expect(words(STAGE, { traffic: { services: [] } })).toContain('No requests in the last hour.');
    expect(words(STAGE, { traffic: { services: [] } })).toContain('no traced requests in the last hour');
    // A service counted at the ingress, since it sends no spans, says so.
    const viaIngress = { services: [{ ...traffic.services[0], source: 'ingress' }] };
    expect(words(STAGE, { traffic: viaIngress })).toContain('13k req · p95 41.0 ms · ingress');
    expect(words(STAGE, { traffic: viaIngress })).toContain('13k requests last hour · p95 41.0 ms · ingress');
  });

  it('shows an addon with type, version, plan and provisioning', () => {
    expect(words(STAGE)).toContain('postgres 1/1 postgres 17 · small provisioned');
    const unprov = { ...STAGE, objects: [{ ...STAGE.objects.find((o) => o.name === 'nats'), provisioned: false }] };
    expect(words(unprov)).toContain('ready, not provisioned yet');
  });

  it("shows a job's last run, and runs it with write on the environment", () => {
    expect(words(STAGE)).toContain('last run succeeded');
    expect(overview(STAGE, { ec: ALL })).toContain('Run now');
    expect(overview(STAGE, { ec: NONE })).toContain('disabled="" title="needs write on this environment"');
    expect(overview(STAGE)).not.toContain('Run now');
    const manual = overview(STAGE, { ec: ALL, spec: { jobs: { migrate: { on: 'manual' } } } });
    expect(manual).toContain('class="mini primary"');
    expect(text(<EnvOverview st={STAGE} tenant={T} navigate={nav} spec={{ jobs: { migrate: { on: 'manual' } } }} />))
      .toContain('runs when asked');
    const never = { ...STAGE, objects: [{ kind: 'job', name: 'seed', lastRun: null }] };
    expect(words(never)).toContain('not run yet');
  });

  it('shows attached services with their sandbox and phase', () => {
    const all = words(PREVIEW);
    expect(all).toContain('approval-service attached in sandbox 0193b0aa ready');
    expect(all).toContain('expense-service attached in sandbox 0193b0aa ready · 1 restart');
    expect(all).toContain('web attached in sandbox 0193b0aa starting');
  });

  it('shows where it is reached: the public hosts, else the route door, and the routes to its services', () => {
    const pub = overview(STAGE);
    expect(words(STAGE)).toContain('Reachable at Domains & access expensa-stage-fecode.apps.fecode.dev');
    expect(pub).toContain('<span class="mono">/api/expense</span>');
    expect(pub).toContain('href="/tenants/fecode/environments/expensa-stage/objects/service/expense-service" class="strong">expense-service</a>');
    const door = words(PREVIEW);
    expect(door).toContain('Not public');
    expect(door).toContain('The route door serves its routes to a route token.');
    expect(door).toContain('/api/expense expense-service port 8081');
  });

  it('shows the recent activity, deployments and incidents together, newest first', () => {
    const deps = response('GET_environments_env_deployments.200').deployments;
    const activity = activityItems(deps, response('GET_environments_env_incidents.200').incidents);
    const all = words(STAGE, { activity });
    expect(all).toContain('Recent activity');
    expect(count(overview(STAGE, { activity }), 'class="act-title"')).toBe(4);
    expect(all.indexOf('Deployment 7 deploying')).toBeLessThan(all.indexOf('Deployment 6 deployed'));
    expect(overview(STAGE, { activity })).toContain('href="/tenants/fecode/environments/expensa-stage/activity"');
  });

  it('a new environment: no deployment, no objects, no routes', () => {
    const all = words(response('POST_environments.201'));
    expect(all).toContain('No deployment yet');
    expect(all).not.toContain('Services');
    expect(all).not.toContain('Reachable at');
  });

  it('an environment being deleted says so', () => {
    expect(words({ ...STAGE, deleting: true })).toContain('Deleting ferund removes its instances');
  });
});

describe('InstanceNote', () => {
  it("shows a readiness check's label, request and reason", () => {
    const check = html(<InstanceNote note={{ tone: 'moving', icon: 'activity', label: 'Readiness check', request: 'GET /readyz', text: 'nothing answered on port 8080 in this sandbox' }} title="t" />);
    expect(check).toContain('<span class="inst-note-label">Readiness check</span><span class="inst-note-req mono">GET /readyz</span><span class="inst-note-text">nothing answered on port 8080 in this sandbox</span>');
  });
});

describe('InstanceTable', () => {
  it("lists an object's instances: replica, phase, state, address, deployment, restarts, last exit, logs, the node last", () => {
    const svc = STAGE.objects.find((o) => o.name === 'expense-service');
    const logsTo = (r) => `/tenants/fecode/environments/expensa-stage/objects/service/expense-service/logs/${r}`;
    const table = (operator) => html(<InstanceTable instances={svc.instances} latest={7} navigate={nav} logsTo={logsTo} operator={operator} />);
    const out = table(false);
    expect(count(out, 'title="instance ')).toBe(3);
    expect(out).toContain('<tr class="inst-detail"><td colSpan="10"><div class="inst-note failed" title="exited with code 1; restarting in 4s">');
    expect(out).toContain('<span class="inst-note-text">exited with code 1; restarting in 4s</span></div></td></tr>');
    expect(out).toContain('<td class="col-num mono warntext">3</td>');
    expect(out).toContain('<td class="col-num mono bad">1</td>');
    expect(out).toContain('class="col-num mono muted" title="an older deployment&#x27;s instance">6</td>');
    expect(out).toContain('href="/tenants/fecode/environments/expensa-stage/objects/service/expense-service/logs/1"');
    expect(out).toContain('<td class="small muted inst-node cut-start" title="n2">n2</td>');
    expect(out).not.toContain('href="/nodes/');
    expect(table(true)).toContain('href="/nodes/n2"');
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
    expect(out).toContain('href="/tenants/fecode/environments/expensa-stage/objects/service/expense-service/logs"');
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
    expect(out).toContain('href="/tenants/fecode/environments/expensa-stage/objects/addon/postgres/logs"');
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

  it("on an object's page offer no other object, only its replicas and sources", () => {
    const out = html(<LogSearch tenant={T} env="expensa-stage" st={STAGE} focus={focusOf('service/expense-service', null, '')}
      navigate={nav} tail={null} scoped />);
    expect(out).not.toContain('every object');
    expect(out).not.toContain('objchip');
    expect(out).toContain('aria-pressed="true">every</button>');
    expect(out).toContain('aria-pressed="false">requests</button>');
    // The environment's Logs reads the object picked from the query.
    const env = html(<LogsTab tenant={T} env="expensa-stage" st={STAGE} item="" replica={null}
      search="?object=service%2Fexpense-service" navigate={nav} />);
    expect(env).toContain('class="chipbtn mono objchip on"><span class="muted">service/</span>expense-service</button>');
  });

  it("show an attached service's process output alone, from its node", () => {
    const out = html(<LogSearch tenant={T} env="expensa-preview" st={PREVIEW} focus={focusOf('service/web', null, '')}
      navigate={nav} tail={<p className="tail">the tail</p>} tailOnly />);
    expect(out).toContain('<p class="tail">the tail</p>');
    expect(out).not.toContain('aria-label="search the logs"');
  });
});

// The view calls the environments' routes of the client and no other:
// its reads (with what runs in it beside its deployments, ADR 0035), and
// its operations (ADR 0018), each behind its role.
describe('the routes it calls', () => {
  it('are the reads and the operations of an environment', () => {
    const src = ['./Environments.jsx', './EnvOverview.jsx', './ObjectPage.jsx', './EnvTabs.jsx', './LogSearch.jsx', './Traces.jsx',
      '../envstatus.jsx']
      .map((f) => readFileSync(new URL(f, import.meta.url), 'utf8')).join('\n');
    const used = [...new Set([...src.matchAll(/\bapi\.(\w+)\(/g)].map((m) => m[1]))].sort();
    expect(used).toEqual([
      'accessLogs', 'addonCatalog', 'checkBinding', 'cloneEnvironment', 'deleteEnvironment', 'deleteSecret', 'deleteTag',
      'followLogs', 'getDeployment', 'getEnvironment', 'getIncident', 'getTrace', 'listDeployments', 'listEnvironments',
      'listIncidents', 'listJobs', 'listSandboxes', 'listSecrets', 'listTags', 'mintRouteToken', 'objectLogs', 'putSecret', 'putTag', 'renameEnvironment', 'restartService',
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
  const words = (st, ec = ALL) => text(<EnvHead st={st} env={st.name} tenant={T} ec={ec} domains={[]} editing={false}
    lastOk={0} changed={nav} navigate={nav} />);

  it('names the environment, its state, what runs where, Revert to the one that serves, and New deployment', () => {
    const out = head(STAGE);
    expect(out).toContain('<h1><span class="">expensa-stage</span></h1>');
    // An open incident is its state; the line says what runs.
    expect(out).toContain('<span class="statepill failed">');
    expect(words(STAGE)).toContain('1 open incident');
    expect(words(STAGE)).toContain('Deployment 7 deploying, started');
    expect(words(STAGE)).toContain('deployment 6 serves meanwhile · public at expensa-stage-fecode.apps.fecode.dev · visitors of fecode');
    expect(out).toContain('Revert to 6');
    expect(out).toContain('href="/tenants/fecode/environments/expensa-stage/builder"');
    // public at its host, teal and a link
    expect(out).toContain('<a class="mono host" href="https://expensa-stage-fecode.apps.fecode.dev"');
    // Its labels are on Settings.
    expect(out).not.toContain('fecode.class=');
  });

  it('shows the name whole, whatever its form', () => {
    expect(head({ ...STAGE, name: 'kino-prod-9101dfcb' })).toContain('<h1><span class="">kino-prod-9101dfcb</span></h1>');
    expect(head({ ...STAGE, name: 'kino--ws--login-fix' })).toContain('<h1><span class="">kino--ws--login-fix</span></h1>');
  });

  it('offers no revert while the latest serves, New deployment the one primary action, and disables it without write', () => {
    const deployed = { ...STAGE, deployed: 7, openIncidents: 0, rollout: { ...STAGE.rollout, state: 'deployed' } };
    expect(head(deployed)).not.toContain('Revert to');
    expect(head(deployed)).toContain('<span class="statepill running">');
    expect(words(deployed)).toContain('Deployment 7 deployed');
    expect(head(deployed)).toContain('class="button primary"');
    // While an incident is open, the way to it is the primary action, not a new deployment.
    expect(head(STAGE)).not.toContain('class="button primary"');
    expect(head(STAGE, NONE)).toContain('disabled="" title="needs write on this environment"');
  });

  it('shows Done while the builder is open, and the default host that answers 404 while a domain is served', () => {
    const st = { ...STAGE, hosts: ['kino.example.com'], hostLabel: 'stage--kino' };
    const domains = [{ id: 'd', name: 'kino.example.com', state: 'served', environment: st.id }];
    const out = html(<EnvHead st={st} env={st.name} tenant={T} ec={ALL} editing lastOk={0} changed={nav} navigate={nav} domains={domains} />);
    expect(out).toContain('Done');
    expect(out).not.toContain('New deployment');
    expect(text(<EnvHead st={st} env={st.name} tenant={T} ec={ALL} editing={false} lastOk={0} changed={nav} navigate={nav} domains={domains} />))
      .toContain('default host stage--kino answers 404 while a domain is served');
  });

  it('says not public for one the route door alone serves', () => {
    expect(words({ ...PREVIEW, public: false, hosts: [] })).toContain('not public · route door only');
  });

  it('says who a public one admits: anyone, or anyone from an older ferund that answers no access', () => {
    const open = { ...PREVIEW, public: true, hosts: ['p.apps.fecode.dev'] };
    expect(words(open)).toContain('public at p.apps.fecode.dev · open to anyone');
    const { access, provider, ...old } = STAGE;
    expect(access).toBe('visitors');
    expect(provider).toBe('fecode');
    expect(words(old)).toContain('public at expensa-stage-fecode.apps.fecode.dev · open to anyone');
  });
});

describe('AboutBox', () => {
  it("shows the environment's name, ID, creation and labels", () => {
    const all = text(<AboutBox st={STAGE} />);
    expect(all).toContain('name expensa-stage');
    expect(all).toContain(`ID ${STAGE.id}`);
    expect(html(<AboutBox st={STAGE} />)).toContain('fecode.class=<b>stage</b>');
  });
});

describe('the bindings', () => {
  it("lists the latest deployment's bindings in the overview, each with Check", () => {
    const spec = { services: { 'expense-service': { bindings: ['erp'] } }, bindings: { erp: { upstream: 'https://erp.example', credential: { kind: 'basic' } } } };
    const out = html(<EnvOverview st={STAGE} tenant={T} navigate={nav} spec={spec} ec={ALL} />);
    expect(out).toContain('Bindings <span class="count">1</span>');
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
  it('lists each service: requests, errors, p50, p95, a sparkline and where it is counted', () => {
    const t = response('GET_environments_env_traffic.200');
    const out = html(<TrafficTable traffic={t} />);
    expect(out).toContain('<td class="mono strong">api</td>');
    expect(out).toContain('3.1 ms');
    expect(out).toContain('3.4 ms');
    expect(out).toContain('<svg class="spark"');
    expect(out).toContain('<polyline class="spark-req"');
    // api from its own spans, web (which sends none) from the ingress.
    expect(text(<TrafficTable traffic={t} />)).toMatch(/api 3 .* its spans web .* the ingress/);
    expect(html(<TrafficTable traffic={{ services: [] }} />)).toContain('No requests in this window.');
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
    // Unpublish sits in the host label's form: a plain button, whose
    // arming click submits nothing.
    expect(on).toMatch(/<form class="inlineform">.*<button type="button" class="mini danger confirm"[^>]*>Unpublish<\/button><\/form>/);
    expect(on).toContain('<button class="primary mini" type="submit"');
    expect(on).toContain('<a class="mono host" href="https://expensa-stage-fecode.apps.fecode.dev"');
    expect(html(<PublicBox env="e" st={off} ec={{ ...ALL, admin: false }} domains={[]} changed={nav} />))
      .toContain('title="needs admin on this environment"');
  });

  // The providers as GET /api/v1/providers answers them, and the kinds an
  // installation with ferun-auth serves.
  const PROVIDERS = [
    { name: 'ferun', kind: 'ferun', builtIn: true, environments: [] },
    { name: 'sso', kind: 'oidc', builtIn: false, issuer: 'https://login.example.com/', rule: { domains: ['example.com'] }, environments: [] },
    { name: 'fecode', kind: 'tenant', builtIn: false, url: 'https://fecode.dev/api/v1/visit', environments: [{ id: STAGE.id, name: STAGE.name }] },
  ];
  const KINDS = ['tenant', 'ferun', 'oidc'];
  const box = (st, extra = {}) => <PublicBox env="e" st={st} ec={ALL} domains={[]} changed={nav} providers={PROVIDERS} kinds={KINDS} {...extra} />;

  it("admits visitors of ferun by default on a first publish, where the installation serves it (ADR 0054)", () => {
    const off = { ...STAGE, public: false, hosts: [], access: 'anyone', provider: '' };
    const out = html(box(off));
    expect(out).toContain('<button type="button" class="on" title="">Visitors</button>');
    expect(out).toContain('<button type="button" class="" title="">Anyone</button>');
    // ferun first, then the tenant's by name, each with its kind.
    expect(out).toMatch(/<option value="ferun" selected="">ferun · FeRun sign-in<\/option><option value="fecode">fecode · your own service<\/option><option value="sso">sso · OpenID Connect<\/option>/);
    expect(out).toContain('<button class="primary mini" type="submit" title="">Publish</button>');
    const all = text(box(off));
    expect(all).toContain('is sent to ferun to sign in');
    expect(all).toContain('gets 401');
    expect(all).toContain('stays open to anyone');
    expect(all).toContain('Changing the provider or unpublishing ends its visits');
    expect(all).toContain('for up to 16 minutes');
    // Who it admits is said once it is public, not before.
    expect(all).not.toContain('admits visitors');
  });

  it('has no default without ferun-auth: the admin chooses, and the kinds not served are disabled', () => {
    const off = { ...STAGE, public: false, hosts: [], access: 'anyone', provider: '' };
    const out = html(box(off, { kinds: ['tenant'] }));
    expect(out).toContain('<button type="button" class="" title="">Visitors</button>');
    expect(out).toContain('<button type="button" class="" title="">Anyone</button>');
    expect(out).not.toContain('<select');
    expect(text(box(off, { kinds: ['tenant'] }))).toContain('Choose who it admits');
    expect(out).toContain('disabled="" title="choose who it admits first">Publish</button>');
  });

  it('offers only anyone, saying why, where the installation guards no environment', () => {
    const off = { ...STAGE, public: false, hosts: [], access: 'anyone', provider: '' };
    const out = html(box(off, { kinds: [], providers: [] }));
    expect(out).toContain('disabled="" title="this installation guards no environments: ferund runs without --secret-key-file">Visitors</button>');
    expect(out).toContain('<button type="button" class="on" title="">Anyone</button>');
    expect(out).toContain('<button class="primary mini" type="submit" title="">Publish</button>');
    expect(text(box(off, { kinds: [], providers: [] }))).toContain('Only anyone: this installation guards no environments');
  });

  it("shows a public one's access, with its provider in words, and keeps it on a new host label", () => {
    const out = html(box(STAGE));
    expect(out).toContain('visitors · fecode');
    expect(text(box(STAGE))).toContain('admits visitors · fecode whom https://fecode.dev/api/v1/visit admits');
    expect(out).toContain('<option value="fecode" selected="">fecode · your own service</option>');
    // Nothing to change yet: no Open to anyone, no Admit.
    expect(out).not.toContain('Open to anyone');
    expect(out).not.toContain('Admit visitors of');
    expect(out).toContain('Change host label');
    expect(out).toContain('Unpublish');
    const open = text(box({ ...STAGE, access: 'anyone', provider: '' }));
    expect(open).toContain('admits anyone every request its routes let through');
    expect(open).toContain('Anyone reaches the routes to its image services');
  });

  it('says that an older ferund guards nothing, and offers the current provider when the list is not readable', () => {
    const { access, provider, ...old } = STAGE;
    expect(access).toBe('visitors');
    expect(text(box({ ...old, provider }))).toContain('admits anyone this ferund does not guard environments yet: open to anyone');
    const unread = html(<PublicBox env="e" st={STAGE} ec={ALL} domains={[]} changed={nav} providers={null} kinds={null} />);
    expect(unread).toContain('<option value="ferun">ferun · FeRun sign-in</option><option value="fecode" selected="">fecode</option>');
    const noFerun = html(box({ ...STAGE, provider: 'sso' }, { kinds: ['tenant'] }));
    expect(noFerun).toContain('<option value="ferun" disabled="">ferun · FeRun sign-in — not served by this installation</option>');
    expect(noFerun).toContain('<option value="sso" disabled="" selected="">sso · OpenID Connect — not served by this installation</option>');
    expect(text(box({ ...STAGE, provider: 'sso' }, { kinds: ['tenant'] }))).toContain('nobody can sign in');
  });

  it('lists the domains bound to it, linked to their pages', () => {
    const domains = [{ id: 'd-1', name: 'kino.example.com', state: 'served', verification: 'verified', environment: STAGE.id },
      { id: 'd-2', name: 'x.example.org', state: 'served', environment: 'o' }];
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
