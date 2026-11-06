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

const form = (defaults, disabled = false) => renderToStaticMarkup(
  <CreateForm busy="" defaults={defaults} disabled={disabled} onSubmit={() => {}} onCancel={() => {}} />,
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

  it('explains attachable and the sealed login, and says why it is disabled', () => {
    const out = form(null, true);
    expect(out).toContain('attachable');
    expect(out).toContain('an environment&#x27;s attached service may run in it');
    expect(out).toContain('never shown again');
    expect(out).toContain('title="needs write on this tenant"');
  });
});

const SB = {
  sandboxId: 'h3k9v2m7qpx', clientState: 'paused', image: 'ghcr.io/kino/agent-base:2.4', node: 'n3', owner: 'kino',
  createdAt: '2026-11-03T10:00:00Z', updatedAt: '2026-11-06T09:00:00Z', statusDescription: '', checkpointEpoch: '0',
  vcpus: 2, memoryMib: 4096, attachable: true,
};
const head = (sb, write) => renderToStaticMarkup(
  <SandboxHead sb={sb} tenant="kino" busy="" act={() => {}} write={write} onConnect={() => {}} />,
);

describe('SandboxHead', () => {
  it('says the state with its cost, and offers resume and connect', () => {
    const out = head(SB, true);
    expect(out).toContain('<span class="kind">Sandbox</span>');
    expect(out).toContain('<h1 class="mono">h3k9v2m7qpx</h1>');
    expect(out).toContain('asleep on n3 · costs no memory · wakes on the next call');
    expect(out).toContain('Resume');
    expect(out).not.toContain('Pause<');
    expect(out).toContain('Connect an agent');
    expect(out).toContain('<b>attachable</b>');
    // The owner is the tenant: it says nothing here.
    expect(out).not.toContain('>kino<');
  });

  it('disables resume for a role without write, with the reason', () => {
    const out = head(SB, false);
    expect(out).toMatch(/<button disabled="" title="needs write on this tenant">.*Resume/);
  });

  it('shows the description and the checkpoint of a hibernated sandbox', () => {
    const out = head({ ...SB, clientState: 'hibernated', node: '', checkpointEpoch: '3', checkpointAt: '2026-11-04T10:00:00Z',
      statusDescription: 'hibernated after 1h paused' }, true);
    expect(out).toContain('in the object store · nothing on a node · restores on any node');
    expect(out).toContain('hibernated after 1h paused');
    expect(out).toContain('epoch 3');
  });
});

describe('the list and the rules', () => {
  it('counts by state with dots', () => {
    const out = renderToStaticMarkup(<StateCounts counts={{ awake: 2, asleep: 0, hibernated: 1, moving: 0, failed: 1, other: 0 }} />);
    expect(out).toContain('<b>2</b> awake');
    expect(out).toContain('sbx-count zero');
    expect(out).toContain('<b>0</b> on its way');
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
