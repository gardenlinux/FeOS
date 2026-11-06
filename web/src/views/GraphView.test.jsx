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
// and the plan's marks, the edges' labels, the entry's hosts or door.

const STORED = response('GET_environments_env_deployments_n.200').spec;
const WRITTEN = fixture('POST_environments_env_deployments.422').request.spec;
const INVALID = response('POST_environments_env_deployments.422');
const STATUS = response('GET_environments_env.200');
const none = { global: [], objects: new Map() };
const nop = () => {};

const render = (props) => renderToStaticMarkup(
  <GraphView spec={STORED} groups={none} diff={null} open="" onOpen={nop} catalog={builtinCatalog()} knownSecrets={[]}
    pins={[]} edit={nop} focus entry={{ public: STATUS.public, hosts: STATUS.hosts, env: STATUS.name }} {...props} />);

describe('GraphView', () => {
  it('draws the objects, the references and the routes from the entry', () => {
    const out = render({});
    expect(out.match(/class="g-node /g)).toHaveLength(8);
    expect(out).toContain('expensa-stage-fecode.apps.fecode.dev');
    expect(out).toContain('>/api/expense<');
    expect(out).toContain('>EXPENSA_DATABASE_URL · url<');
    expect(out.match(/class="g-edge /g)).toHaveLength(10);
    // Handles on the entry, the image services and the job.
    expect(out.match(/class="g-handle"/g)).toHaveLength(5);
    expect(out).toContain('environment <span class="mono">expensa-stage</span>');
  });

  it('shows the route door of an environment that is not public', () => {
    const out = render({ entry: { public: false, hosts: [], env: 'stage' } });
    expect(out).toContain('not public');
    expect(out).toContain('/api/v1/environments/stage/http/…');
  });

  it('marks findings and the plan changes on their nodes, and dims what is not next to the open one', () => {
    const groups = groupFindings(INVALID.findings, WRITTEN);
    const out = render({ spec: WRITTEN, groups, diff: [{ kind: 'service', name: 'expense-service', change: 'update' },
      { kind: 'addon', name: 'gone', change: 'remove' }], open: 'services/expense-service' });
    expect(out).toContain('class="g-node k-services error m-update on"');
    // mysql is referenced, not declared; gone the plan removes.
    expect(out).toContain('class="g-node k-addons missing"');
    expect(out).toContain('class="g-node k-addons m-remove dim removed"');
    expect(out).toContain('the plan removes it');
    expect(out).toContain('class="g-node k-entry dim"');
    expect(out).toContain('class="g-label mono error full"');
    expect(out).toContain('>2</span>');
  });
});
