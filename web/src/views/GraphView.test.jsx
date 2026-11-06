// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { fixture, response } from '../testing/ferun.js';
import { GraphView } from './GraphView.jsx';
import { builtinCatalog } from './catalog.js';
import { groupFindings } from './findings.js';

// The graph rendered with ferund's fixtures: nodes with their findings
// and the draft's marks, the folded edges' labels, the secrets as markers
// and a list, the entry's hosts or door.

const STORED = response('GET_environments_env_deployments_n.200').spec;
const WRITTEN = fixture('POST_environments_env_deployments.422').request.spec;
const INVALID = response('POST_environments_env_deployments.422');
const STATUS = response('GET_environments_env.200');
const none = { global: [], objects: new Map() };
const nop = () => {};

const render = (props) => renderToStaticMarkup(
  <GraphView spec={STORED} groups={none} diff={null} open="" onOpen={nop} catalog={builtinCatalog()} knownSecrets={[]}
    pins={[]} edit={nop} focus marks={new Map()} removed={[]} entry={{ public: STATUS.public, hosts: STATUS.hosts, env: STATUS.name }} {...props} />);

describe('GraphView', () => {
  it('draws the objects, one edge per pair, the routes from the entry, and the secrets beside', () => {
    const out = render({});
    expect(out.match(/class="g-node /g)).toHaveLength(7);
    expect(out).toContain('expensa-stage-fecode.apps.fecode.dev');
    expect(out).toContain('>/api/expense<');
    expect(out).toContain('>EXPENSA_DATABASE_URL<');
    expect(out.match(/class="g-edge /g)).toHaveLength(8);
    // The secret is a marker on its users and a line of the list, no node.
    expect(out.match(/1 secret</g)).toHaveLength(2);
    expect(out).toContain('class="g-secret"');
    expect(out).toContain('>internal-token<');
    // The job no route reaches is in the lane.
    expect(out).toContain('no route reaches these');
    // Handles on the entry, the image services and the job.
    expect(out.match(/class="g-handle"/g)).toHaveLength(5);
    // A read-only builder (no edit) draws no handles and adds no secret.
    expect(render({ edit: null })).not.toContain('class="g-handle"');
  });

  it('folds references between two objects into one edge labelled with the first and a count', () => {
    const spec = { ...STORED, services: { ...STORED.services, 'expense-service': { ...STORED.services['expense-service'],
      env: { ...STORED.services['expense-service'].env, APPROVAL_ADMIN: { service: 'approval-service', port: 'http', key: 'host' } } } } };
    const out = render({ spec });
    expect(out).toContain('>EXPENSA_APPROVAL_URL</span><span class="g-more">+1</span>');
    expect(out).toContain('title="EXPENSA_APPROVAL_URL → approval-service:http · url\nAPPROVAL_ADMIN → approval-service:http · host"');
  });

  it('shows the route door of an environment that is not public', () => {
    const out = render({ entry: { public: false, hosts: [], env: 'stage' } });
    expect(out).toContain('not public');
    expect(out).toContain('/api/v1/environments/stage/http/…');
  });

  it('marks findings and the draft changes on their nodes, and dims what is not next to the open one', () => {
    const groups = groupFindings(INVALID.findings, WRITTEN);
    const marks = new Map([['services/expense-service', 'update']]);
    const out = render({ spec: WRITTEN, groups, marks, removed: [{ key: 'addons', name: 'gone' }], open: 'services/expense-service' });
    expect(out).toContain('class="g-node k-services error m-update on"');
    expect(out).toContain('title="changed by this draft">~</span>');
    // mysql is referenced, not declared; gone the draft removes.
    expect(out).toContain('class="g-node k-addons missing"');
    expect(out).toContain('class="g-node k-addons m-remove dim removed"');
    expect(out).toContain('removed by this draft');
    expect(out).toContain('class="g-node k-entry dim"');
    expect(out).toContain('>2</span>');
  });
});
