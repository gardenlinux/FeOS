// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { AddonForm, JobForm, SecurityGroupForm, ServiceForm } from './BuilderForms.jsx';
import { EnvPicker, GroupChips, GroupPicker, NoEnvironment } from './SecurityGroups.jsx';
import { PodTables } from './EnvPods.jsx';
import { InUse } from './EnvTabs.jsx';
import { CreateForm, GroupsBox } from './Sandboxes.jsx';
import { RunForm } from './Jobs.jsx';
import { builtinCatalog } from './catalog.js';
import { groupChoices } from './groups.js';
import { ApiError } from '../api.js';

// Security groups in the console (ADR 0034, ADR 0035): the builder's
// form of a group and the groups of services, jobs and addons; the
// choice of a sandbox's groups and its move; an environment's sandboxes
// and jobs, and the delete they block.

const html = (el) => renderToStaticMarkup(el);
const text = (el) => html(el).replace(/<[^>]+>/g, ' ').replace(/&#x27;/g, "'").replace(/&quot;/g, '"')
  .replace(/&amp;/g, '&').replace(/\s+/g, ' ');
const nop = () => {};

const SPEC = {
  services: {
    web: { image: 'web:1', securityGroups: ['web'] },
    api: { image: 'api:1', securityGroups: ['api'] },
    worker: { image: 'worker:1' },
    dev: { runtime: 'attached', sandbox: 'h3k9v2m7qpx', command: ['npm', 'run', 'dev'] },
  },
  jobs: { migrate: { image: 'api:1', on: 'deploy' } },
  addons: { db: { type: 'postgres', securityGroups: ['db'] } },
  securityGroups: {
    web: { in: [{ protocol: 'tcp', ports: [8080], from: { cidr: '203.0.113.0/24', except: ['203.0.113.8/29'] } }] },
    api: { in: [{ protocol: 'tcp', ports: [9000], from: { group: 'web' } }], out: [{ protocol: 'tcp', ports: [443], to: 'internet' }] },
    db: { in: [{ protocol: 'tcp', ports: [5432], from: { group: 'api' } }], out: [] },
  },
};
const ctx = (extra = {}) => ({
  spec: SPEC, catalog: builtinCatalog(), findings: [], setSecret: async () => 1, isNew: false, knownSecrets: [], pins: [],
  credentials: [], editSpec: nop, editAttached: true, tenant: 'kino', navigate: null, sandboxes: null, ...extra,
});
const form = (name, extra) => (
  <SecurityGroupForm name={name} obj={SPEC.securityGroups[name] || {}} ctx={ctx(extra)} onChange={nop} onRename={nop} onRemove={nop} />);

describe('SecurityGroupForm', () => {
  it('shows the group\'s picture, then its rules in and out, a row each: from or to, protocol, ports', () => {
    const out = html(form('api'));
    expect(out).toContain('<span class="kind">Security group</span>');
    const t = text(form('api'));
    expect(t).toContain('Allowed in Allowed out web tcp 9000 1 member api tcp 5432 db tcp 443 Internet');
    expect(t).toContain('From Protocol Ports');
    expect(out).toMatch(/<select title="protocol"><option value="tcp" selected="">tcp<\/option><option value="udp">udp<\/option><option value="icmp">icmp<\/option><option value="any">any<\/option><\/select>/);
    expect(out).toContain('value="9000"');
    expect(out).toMatch(/<select aria-label="from" title="who may connect"><optgroup label="Groups of this environment"><option value="group:default">default<\/option><option value="group:api">api<\/option><option value="group:db">db<\/option><option value="group:web" selected="">web<\/option><\/optgroup><optgroup label="Outside the environment"><option value="internet">Internet<\/option><option value="cidr">Address block…<\/option>/);
    expect(out).toContain('value="443"');
    expect(out).toMatch(/<button class="on" aria-pressed="true">Only these<\/button>/);
    expect(out).toContain('Remove security group api');
  });

  it('says that a group without out reaches the internet; out: [] reaches nothing', () => {
    const t = text(form('web'));
    expect(html(form('web'))).toMatch(/<button class="on" aria-pressed="true">The internet<\/button>/);
    expect(t).toContain('Its members reach every address on the internet. That is the default.');
    expect(html(form('web'))).toContain('value="203.0.113.0/24"');
    expect(html(form('web'))).toContain('value="203.0.113.8/29"');
    expect(t).toContain('203.0.113.0/24 except 203.0.113.8/29 tcp 8080');
    expect(html(form('db'))).toMatch(/<button class="on" aria-pressed="true">Nothing<\/button>/);
    expect(text(form('db'))).toContain('Its members reach nothing outside the environment.');
    expect(text(form('db'))).toContain('Reaches nothing');
  });

  it('keeps default: no rename, no remove, its members those that name none', () => {
    const out = html(form('default'));
    expect(out).toContain('<h2 class="mono">default</h2>');
    expect(out).not.toContain('Remove security group');
    const t = text(form('default'));
    expect(t).toContain('every pod that names no group');
    expect(t).toContain('Nothing gets in');
    expect(t).toContain('No rule: nothing gets in.');
    expect(t).toContain('2 members worker migrate');
    expect(t).not.toContain(' dev ');
  });

  it('counts the environment\'s sandboxes among the members', () => {
    const sandboxes = [{ sandboxId: 'h3k9v2m7qpx', clientState: 'running', securityGroups: ['web'] }];
    expect(text(form('web', { sandboxes }))).toContain('2 members web h3k9v2m7qpx');
  });

  it('disables the ports of icmp and any', () => {
    const o = { in: [{ protocol: 'icmp', from: 'internet' }] };
    const out = html(<SecurityGroupForm name="ping" obj={o} ctx={ctx()} onChange={nop} onRename={nop} onRemove={nop} />);
    expect(out).toContain('<input class="mono sg-ports" disabled="" placeholder="every port" aria-label="ports"');
    expect(out).toMatch(/<option value="internet" selected="">Internet<\/option>/);
  });
});

describe('the groups of services, jobs and addons', () => {
  it('ticks the object\'s groups, default for one that names none', () => {
    const web = html(<ServiceForm name="web" obj={SPEC.services.web} ctx={ctx()} onChange={nop} onRename={nop} onRemove={nop} />);
    expect(web).toContain('Security groups');
    expect(web).toMatch(/<input type="checkbox" (?:checked="" disabled=""|disabled="" checked="")\/><span class="mono">web<\/span>/);
    expect(web).toMatch(/<input type="checkbox"\/><span class="mono">default<\/span><span class="muted small">nothing in · internet out/);
    const job = html(<JobForm name="migrate" obj={SPEC.jobs.migrate} ctx={ctx()} onChange={nop} onRename={nop} onRemove={nop} />);
    expect(job).toMatch(/<input type="checkbox" (?:checked="" disabled=""|disabled="" checked="")\/><span class="mono">default<\/span>/);
    const db = html(<AddonForm name="db" obj={SPEC.addons.db} ctx={ctx()} onChange={nop} onRename={nop} onRemove={nop} />);
    expect(db).toMatch(/(?:checked="" disabled=""|disabled="" checked="")\/><span class="mono">db<\/span><span class="muted small">in from api · nothing out/);
  });

  it('marks a group the draft lacks', () => {
    const o = { image: 'x:1', securityGroups: ['web', 'gone'] };
    expect(text(<ServiceForm name="x" obj={o} ctx={ctx()} onChange={nop} onRename={nop} onRemove={nop} />)).toContain('gone not in the draft');
  });

  it('puts an attached service in its sandbox\'s groups, from the environment\'s sandboxes', () => {
    const sandboxes = [{ sandboxId: 'h3k9v2m7qpx', clientState: 'running', securityGroups: ['dev-box'] }];
    const out = html(<ServiceForm name="dev" obj={SPEC.services.dev} ctx={ctx({ sandboxes })} onChange={nop} onRename={nop} onRemove={nop} />);
    expect(out).toContain('<option value="h3k9v2m7qpx" selected="">h3k9v2m7qpx · running</option>');
    expect(text(<ServiceForm name="dev" obj={SPEC.services.dev} ctx={ctx({ sandboxes })} onChange={nop} onRename={nop} onRemove={nop} />))
      .toContain("in its sandbox's groups: dev-box");
    const own = { ...SPEC.services.dev, securityGroups: ['web'] };
    const t = text(<ServiceForm name="dev" obj={own} ctx={ctx()} onChange={nop} onRename={nop} onRemove={nop} />);
    expect(t).toContain('it names groups of its own (web), which an attached service may not');
    expect(t).toContain('Drop its own groups');
  });
});

describe('a sandbox\'s groups', () => {
  it('ticks one to five of the environment\'s choices', () => {
    const out = html(<GroupPicker choices={groupChoices(SPEC)} value={['default']} onChange={nop} />);
    expect(out).toMatch(/<input type="checkbox" (?:checked="" disabled=""|disabled="" checked="")\/><span class="mono sg-name">default<\/span><span class="muted small sg-words">nothing in · internet out/);
    expect(out).toMatch(/<input type="checkbox"\/><span class="mono sg-name">db<\/span>/);
    const odd = html(<GroupPicker choices={groupChoices({})} value={['default', 'gone']} onChange={nop} />);
    expect(odd).toContain('class="sg-choice on bad"');
    expect(odd).toContain('not a group of this environment');
    expect(html(<GroupPicker choices={null} value={['default']} onChange={nop} />)).toContain('reading the environment&#x27;s groups…');
    expect(html(<GroupChips groups={[]} />)).toContain('>default</span>');
  });

  it('says that a change is a move before it is confirmed', () => {
    const sb = { sandboxId: 'h3k9v2m7qpx', environment: { id: 'e1', name: 'kino-dev' }, securityGroups: ['default'] };
    const closed = text(<GroupsBox sb={sb} write busy="" act={nop} />);
    expect(closed).toContain('Change groups');
    expect(html(<GroupsBox sb={sb} write={false} busy="" act={nop} />)).toContain('disabled="" title="needs write on its environment"');
    const open = text(<GroupsBox sb={sb} write busy="" act={nop} open />);
    expect(open).toContain('hibernated and restored in the new groups under a new address');
    expect(open).toContain('open connections break');
    expect(open).toContain('Move into these groups');
  });
});

describe('an environment\'s sandboxes and jobs', () => {
  it('lists both, with their groups', () => {
    const sandboxes = [{ sandboxId: 'h3k9v2m7qpx', clientState: 'paused', image: 'agent:1', securityGroups: ['default', 'db-admin'],
      updatedAt: '2026-11-06T09:00:00Z' }];
    const jobs = [{ id: 'k2m7qpxh3v9aaaa', phase: 'running', image: 'report:3', securityGroups: ['default'], createdAt: '2026-11-06T09:00:00Z' }];
    const out = html(<PodTables tenant="kino" env="kino-dev" sandboxes={sandboxes} jobs={jobs} write navigate={nop} />);
    expect(out).toContain('href="/tenants/kino/sandboxes/h3k9v2m7qpx"');
    expect(out).toContain('href="/tenants/kino/sandboxes/new?environment=kino-dev"');
    expect(out).toContain('>db-admin</span>');
    expect(out).toContain('1, 1 on their way');
    expect(out).toContain('href="/tenants/kino/jobs/k2m7qpxh3v9aaaa"');
    const empty = text(<PodTables tenant="kino" env="kino-dev" sandboxes={[]} jobs={[]} navigate={nop} />);
    expect(empty).toContain('No sandbox in this environment.');
    expect(empty).not.toContain('new sandbox in');
  });

  it('says which sandboxes and jobs keep a delete from happening', () => {
    const e = new ApiError('environment kino-dev is in use', 409,
      { code: 'environment_in_use', sandboxes: ['h3k9v2m7qpx', 'q8w1z4c0dnr'], jobs: ['k2m7qpxh3v9'] });
    const out = html(<InUse error={e} tenant="kino" navigate={nop} />);
    expect(out).toContain('href="/tenants/kino/sandboxes/q8w1z4c0dnr"');
    expect(out).toContain('href="/tenants/kino/jobs/k2m7qpxh3v9"');
    expect(text(<InUse error={e} tenant="kino" navigate={nop} />)).toContain('Delete the sandboxes and cancel the jobs first');
    expect(html(<InUse error={new ApiError('x', 409, { code: 'busy' })} tenant="kino" navigate={nop} />)).toBe('');
  });

  it('tells a tenant without environments from a role that writes none', () => {
    // No environment at all: a tenant writer is sent to make one.
    const make = html(<NoEnvironment tenant="kino" navigate={nop} none make what="sandbox" />);
    expect(make).toContain('This tenant has no environment yet, and a sandbox runs in one.');
    expect(make).toContain('href="/tenants/kino/new-environment"');
    expect(make).not.toContain('Your role');
    const member = html(<NoEnvironment tenant="kino" navigate={nop} none make={false} what="job" />);
    expect(member).toContain('a job runs in one');
    expect(member).toContain('Someone with write on the tenant makes one first.');
    expect(member).not.toContain('href=');
    // Environments there are, none of them writable: the role.
    const role = html(<NoEnvironment tenant="kino" navigate={nop} none={false} make what="sandbox" />);
    expect(role).toContain('Your role does not allow this: it needs write on an environment.');
    expect(role).not.toContain('new-environment');
    // The pickers say the same.
    expect(html(<EnvPicker envs={[]} value="" onChange={nop} none />)).toContain('no environment yet');
    expect(html(<EnvPicker envs={[]} value="" onChange={nop} />)).toContain('no environment you may write');
    const sbx = html(<CreateForm busy="" defaults={null} disabled envs={[]} none onSubmit={nop} onCancel={nop} />);
    expect(sbx).toContain('no environment yet');
    expect(sbx).not.toContain('no environment you may write');
    const job = html(<RunForm envs={[]} none notice={<NoEnvironment tenant="kino" navigate={nop} none make what="job" />}
      onRun={nop} onCancel={nop} />);
    expect(job).toContain('no environment yet');
    expect(job).toContain('href="/tenants/kino/new-environment"');
  });
});
