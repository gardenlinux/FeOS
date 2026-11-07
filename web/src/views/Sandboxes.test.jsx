// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { CreateForm, IdleRules, SandboxHead, StateCounts } from './Sandboxes.jsx';
import { ToolList } from './Tools.jsx';
import { response } from '../testing/ferun.js';

// The create form names the platform's default image and size and the
// bounds ferund accepts, from GET /api/v1/admin/nodes's defaults; a
// sandbox's head says its state in words, with what the role may do.

const DEFAULTS = {
  telemetryIntervalSeconds: 5, nodeTtlSeconds: 15, lostGraceSeconds: 600, drainTimeoutSeconds: 3600,
  defaultVcpus: 4, defaultMemoryMib: 2048, defaultImage: 'ghcr.io/example/image:1',
  minVcpus: 1, maxVcpus: 16, minMemoryMib: 512, maxMemoryMib: 32768,
};

const ENVS = [{ id: '0199f0b7-1c2d-7e3f-8a00-0000000000d1', name: 'kino-dev' }, { id: '0199f0b7-1c2d-7e3f-8a00-0000000000d2', name: 'kino-stage' }];
const form = (defaults, disabled = false, envs = ENVS, env = '') => renderToStaticMarkup(
  <CreateForm busy="" defaults={defaults} disabled={disabled} envs={envs} env={env} onSubmit={() => {}} onCancel={() => {}} />,
);

describe('CreateForm', () => {
  it("names the instance's defaults and bounds", () => {
    const out = form(DEFAULTS);
    expect(out).toContain('placeholder="image (empty: ghcr.io/example/image:1)"');
    expect(out).toContain('placeholder="vCPUs (4)"');
    expect(out).toContain('title="vCPUs, 1–16 (empty: 4)"');
    expect(out).toContain('min="1" max="16"');
    expect(out).toContain('placeholder="MiB (2048)"');
    expect(out).toContain('title="memory in MiB, 512–32768 (empty: 2048)"');
    expect(out).toContain('min="512" max="32768"');
  });

  it('without defaults: "the default", and no bounds of its own', () => {
    const out = form(null);
    expect(out).toContain('placeholder="image (empty: the default)"');
    expect(out).toContain('placeholder="vCPUs"');
    expect(out).toContain('placeholder="MiB"');
    expect(out).not.toContain('min="');
  });

  it('asks for the environment and its groups, and has no attachable any more', () => {
    const out = form(null);
    expect(out).toContain('<option value="" selected="">(an environment)</option>');
    expect(out).toContain('<option value="kino-dev">kino-dev</option>');
    expect(out).toContain('Choose the environment first.');
    expect(out).toContain('<b>default</b>: nothing in, internet out');
    expect(out).not.toContain('attachable');
    // Without an environment there is nothing to create.
    expect(out).toMatch(/<button type="submit" class="primary" disabled="" title="choose its environment">/);
  });

  it('takes the one environment there is, or the one the address names, with default ticked', () => {
    for (const out of [form(null, false, [ENVS[0]]), form(null, false, ENVS, 'kino-dev')]) {
      expect(out).toContain('<option value="kino-dev" selected="">kino-dev</option>');
      expect(out).toContain('reading the environment&#x27;s groups…');
      expect(out).not.toContain('Choose the environment first.');
    }
    // One the person may not write is no choice.
    expect(form(null, false, ENVS, 'prod')).toContain('Choose the environment first.');
  });

  it('explains the sealed login, and says why it is disabled', () => {
    const out = form(null, true, []);
    expect(out).toContain('never shown again');
    expect(out).toContain('no environment you may write');
    expect(out).toContain('title="needs write on an environment"');
  });
});

const SB = {
  sandboxId: 'h3k9v2m7qpx', clientState: 'paused', image: 'ghcr.io/kino/agent-base:2.4', node: 'n3', owner: 'kino',
  createdAt: '2026-11-03T10:00:00Z', updatedAt: '2026-11-06T09:00:00Z', statusDescription: '', checkpointEpoch: '0',
  vcpus: 2, memoryMib: 4096, environment: { id: '0199f0b7-1c2d-7e3f-8a00-0000000000d1', name: 'kino-dev' },
  securityGroups: ['default', 'db-admin'],
};
const head = (sb, write) => renderToStaticMarkup(
  <SandboxHead sb={sb} tenant="kino" busy="" act={() => {}} write={write} onConnect={() => {}} />,
);

describe('SandboxHead', () => {
  it('says the state with its cost, and offers resume and connect', () => {
    const out = head(SB, true);
    expect(out).toContain('<span class="kind">Sandbox</span>');
    expect(out).toContain('<h1 class="mono">h3k9v2m7qpx</h1>');
    expect(out).toContain('paused on n3 · its memory on the node&#x27;s disk, no RAM · resumes on the next call');
    expect(out).toContain('Resume');
    expect(out).not.toContain('Pause<');
    expect(out).toContain('Connect an agent');
    expect(out).toContain('href="/tenants/kino/environments/kino-dev"');
    expect(out).toContain('<span class="sg-chip mono" title="default: nothing in, internet out">default</span>');
    expect(out).toContain('<span class="sg-chip mono">db-admin</span>');
    expect(out).not.toContain('attachable');
    // The owner is the tenant: it says nothing here.
    expect(out).not.toContain('>kino<');
  });

  it('disables resume for a role without write, with the reason', () => {
    const out = head(SB, false);
    expect(out).toMatch(/<button disabled="" title="needs write on its environment">.*Resume/);
  });

  it('shows the description and the checkpoint of a hibernated sandbox', () => {
    const out = head({ ...SB, clientState: 'hibernated', node: '', checkpointEpoch: '3', checkpointAt: '2026-11-04T10:00:00Z',
      statusDescription: 'hibernated after 1h paused' }, true);
    expect(out).toContain('hibernated in S3 · nothing on a node · resumes on any node');
    expect(out).toContain('hibernated after 1h paused');
    expect(out).toContain('epoch 3');
  });
});

describe('the list and the rules', () => {
  it('counts by state with dots', () => {
    const out = renderToStaticMarkup(<StateCounts counts={{ running: 2, paused: 0, hibernated: 1, changing: 0, failed: 1, other: 0 }} />);
    expect(out).toContain('<b>2</b> running');
    expect(out).toContain('sbx-count zero');
    expect(out).toContain('<b>0</b> changing');
    expect(out).not.toContain('stopped or unknown');
  });

  it('names the idle flags', () => {
    const out = renderToStaticMarkup(<IdleRules />);
    expect(out).toContain('--idle-pause');
    expect(out).toContain('--hibernate-after');
    expect(out).toContain('process_start');
  });
});

describe('ToolList', () => {
  it("folds each tool to its name, first sentence and parameters", () => {
    const tools = [{ name: 'bash', description: 'Run a command. More text.', inputSchema: { properties: { command: {}, timeout: {} }, required: ['command'] } }];
    const out = renderToStaticMarkup(<ToolList tools={tools} list={tools} waitMs={30000} />);
    expect(out).toContain('<details class="sbx-tool">');
    expect(out).toContain('Run a command.</span>');
    expect(out).toContain('command*, timeout');
    expect(out).toContain('&quot;required&quot;');
  });

  it('reads GET /api/v1/tools as ferund answers it', () => {
    let res;
    try {
      res = response('GET_tools.200');
    } catch {
      return; // no fixture of the route
    }
    const out = renderToStaticMarkup(<ToolList tools={res.tools} list={res.tools} />);
    expect(out).toContain(res.tools[0].name);
  });
});
