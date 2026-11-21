// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import { parsePath } from '../routes.js';
import { movedPath, movedTarget } from './Moved.jsx';

// A sandbox's and a job's old links lead to their page in their
// environment (ADR 0035).

describe('an old link of a sandbox or a job', () => {
  it('leads a list, or a new sandbox without an environment, to Home', () => {
    expect(movedTarget(parsePath('/projects/kino/sandboxes'))).toBe('/projects/kino');
    expect(movedTarget(parsePath('/projects/kino/jobs'))).toBe('/projects/kino');
    expect(movedTarget(parsePath('/projects/kino/sandboxes/new'))).toBe('/projects/kino');
  });

  it('leads a new sandbox to the form of the environment the address names', () => {
    expect(movedTarget(parsePath('/projects/kino/sandboxes/new?environment=kino--dev')))
      .toBe('/projects/kino/environments/kino--dev/sandboxes/new');
  });

  it('asks ferund where a sandbox or a job is, and keeps the sandbox\'s view', () => {
    const sbx = parsePath('/projects/kino/sandboxes/h3k9v2m7qpx/connect');
    expect(movedTarget(sbx)).toBe('');
    expect(movedPath(sbx, { sandboxId: 'h3k9v2m7qpx', environment: { id: 'e1', name: 'kino--tools' } }))
      .toBe('/projects/kino/environments/kino--tools/sandboxes/h3k9v2m7qpx/connect');
    const job = parsePath('/projects/kino/jobs/a3ze6kkjvfx');
    expect(movedTarget(job)).toBe('');
    expect(movedPath(job, { id: 'a3ze6kkjvfx', environment: { id: 'e1', name: 'kino--tools' } }))
      .toBe('/projects/kino/environments/kino--tools/jobs/a3ze6kkjvfx');
    expect(movedPath(job, { id: 'a3ze6kkjvfx' })).toBe('');
  });
});
