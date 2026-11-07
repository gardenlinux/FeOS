// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import {
  EMPTY_FORM, ended, fmtSpan, jobCounts, jobDurationMs, jobRequest, jobTone, jobWords, keptUntil, onItsWay,
  parseArgs, parseDuration, parsePairs, phaseWord, resultView,
} from './jobs.js';

// The tenant's jobs' arithmetic, on jobs in the shape of ferund's
// TenantJobStatus (go/internal/env/tenantjobs.go).

const job = (over) => ({
  id: 'k2m7qpxh3v9', image: 'ghcr.io/acme/report:3', imageRef: 'ghcr.io/acme/report@sha256:9f3c', phase: 'running',
  reason: '', detail: '', exitCode: null, result: '', resultPath: '', vcpus: 1, memoryMib: 512, labels: {},
  createdAt: '2026-11-06T10:00:00Z', startedAt: '2026-11-06T10:00:20Z', finishedAt: '', deadlineAt: '2026-11-06T10:10:00Z',
  ...over,
});

describe('phases', () => {
  it('tells the ended from those on their way', () => {
    for (const p of ['succeeded', 'failed', 'deadline_exceeded', 'cancelled']) expect(ended(p)).toBe(true);
    for (const p of ['queued', 'running']) { expect(ended(p)).toBe(false); expect(onItsWay(p)).toBe(true); }
  });

  it('gives each phase its dot: dashed, green, red, grey', () => {
    expect(jobTone('queued')).toBe('moving');
    expect(jobTone('running')).toBe('moving');
    expect(jobTone('succeeded')).toBe('running');
    expect(jobTone('failed')).toBe('failed');
    expect(jobTone('deadline_exceeded')).toBe('failed');
    expect(jobTone('cancelled')).toBe('absent');
    expect(phaseWord('deadline_exceeded')).toBe('deadline exceeded');
  });

  it('says why a job failed, by its reason', () => {
    expect(jobWords(job({ phase: 'failed', reason: 'exit', exitCode: 2 }))).toBe('Its container exited with code 2.');
    expect(jobWords(job({ phase: 'failed', reason: 'node_lost' }))).toMatch(/never runs twice/);
    expect(jobWords(job({ phase: 'failed', reason: 'image' }))).toMatch(/registry refused/);
    expect(jobWords(job({ phase: 'queued' }))).toMatch(/Waiting/);
  });

  it('counts the jobs', () => {
    const c = jobCounts([job(), job({ phase: 'queued' }), job({ phase: 'succeeded' }), job({ phase: 'deadline_exceeded' }),
      job({ phase: 'cancelled' })]);
    expect(c).toEqual({ onItsWay: 2, succeeded: 1, failed: 1, cancelled: 1 });
  });
});

describe('times', () => {
  it('measures from the start to the end, or to now while it runs', () => {
    const now = Date.parse('2026-11-06T10:04:32Z');
    expect(jobDurationMs(job(), now)).toBe(252000);
    expect(jobDurationMs(job({ phase: 'succeeded', finishedAt: '2026-11-06T10:01:05Z' }), now)).toBe(45000);
    expect(jobDurationMs(job({ phase: 'queued', startedAt: '' }), now)).toBe(null);
    expect(jobDurationMs(job({ phase: 'cancelled', finishedAt: '' }), now)).toBe(null);
  });

  it('renders spans', () => {
    expect(fmtSpan(45000)).toBe('45s');
    expect(fmtSpan(252000)).toBe('4m 12s');
    expect(fmtSpan(3780000)).toBe('1h 03m');
    expect(fmtSpan(null)).toBe('—');
  });

  it('keeps an ended job 7 days', () => {
    expect(keptUntil(job({ phase: 'succeeded', finishedAt: '2026-11-06T10:01:05Z' }))).toBe('2026-11-13T10:01:05.000Z');
    expect(keptUntil(job())).toBe('');
  });

  it('reads durations as Go writes them', () => {
    expect(parseDuration('90s')).toBe(90000);
    expect(parseDuration('1h30m')).toBe(5400000);
    expect(parseDuration('10m0s')).toBe(600000);
    expect(parseDuration('1.5h')).toBe(5400000);
    expect(parseDuration('10')).toBeNaN();
    expect(parseDuration('10 m')).toBeNaN();
    expect(parseDuration('')).toBeNaN();
  });
});

describe('results', () => {
  it('pretty-prints a JSON object, shows anything else as it is', () => {
    expect(resultView('{"rows":3}')).toEqual({ text: '{\n  "rows": 3\n}', json: true });
    expect(resultView('42')).toEqual({ text: '42', json: false });
    expect(resultView('done\n')).toEqual({ text: 'done\n', json: false });
    expect(resultView('')).toEqual({ text: '', json: false });
  });
});

describe('the form', () => {
  it('reads one argument per line, spaces kept', () => {
    expect(parseArgs('sh\n-c\necho a b > /tmp/r\n\n')).toEqual(['sh', '-c', 'echo a b > /tmp/r']);
  });

  it('reads key=value lines, the value as typed', () => {
    expect(parsePairs('A=1\nB = x=y\n\n=nokey\nnoeq')).toEqual({ map: { A: '1', B: ' x=y' }, bad: ['=nokey', 'noeq'] });
  });

  it('sends only what is given, the defaults left to ferund', () => {
    expect(jobRequest({ ...EMPTY_FORM, environment: 'kino-dev', image: ' alpine:3 ' }))
      .toEqual({ body: { environment: 'kino-dev', securityGroups: ['default'], image: 'alpine:3' }, errors: [] });
  });

  it('builds the whole body', () => {
    const { body, errors } = jobRequest({
      environment: 'kino-dev', securityGroups: ['default', 'reports'], image: 'ghcr.io/acme/report:3', command: 'report\n--month\n2026-10', env: 'MONTH=2026-10', secrets: 'TOKEN=s3cr3t',
      vcpus: '2', memoryMib: '1024', deadline: '15m', resultPath: '/tmp/result.json', labels: 'kind=report',
      username: 'bot', password: 'pw',
    });
    expect(errors).toEqual([]);
    expect(body).toEqual({
      environment: 'kino-dev', securityGroups: ['default', 'reports'], image: 'ghcr.io/acme/report:3', command: ['report', '--month', '2026-10'], env: { MONTH: '2026-10' },
      secrets: { TOKEN: 's3cr3t' }, vcpus: 2, memoryMib: 1024, deadline: '15m', resultPath: '/tmp/result.json',
      labels: { kind: 'report' }, imageCredentials: { username: 'bot', password: 'pw' },
    });
  });

  it('refuses what ferund would refuse', () => {
    const { errors } = jobRequest({
      ...EMPTY_FORM, env: '1BAD=x\nSAME=1', secrets: 'SAME=2', vcpus: '64', memoryMib: '100', deadline: '3h',
      resultPath: '/dev/out', username: 'bot',
    });
    expect(errors).toEqual([
      'an environment is required',
      'an image is required',
      'env: 1BAD is no environment variable\'s name',
      'SAME is in env and in secrets',
      'vCPUs must be a whole number from 1 to 32',
      'memory (MiB) must be a whole number from 256 to 65536',
      'the deadline is a duration of up to 2h (90s, 15m, 1h30m), not 3h',
      'the result path must be an absolute, clean path of a file outside /dev, not /dev/out',
      'the image\'s credentials need a username and a password',
    ]);
    for (const p of ['tmp/r', '/tmp/../r', '/tmp/', '/a//b', '/']) {
      expect(jobRequest({ ...EMPTY_FORM, environment: 'e', image: 'x', resultPath: p }).errors).toHaveLength(1);
    }
    expect(jobRequest({ ...EMPTY_FORM, environment: 'e', image: 'x', deadline: '2h' }).errors).toEqual([]);
    expect(jobRequest({ ...EMPTY_FORM, environment: 'e', image: 'x', securityGroups: [] }).errors)
      .toEqual(['name at least one security group']);
  });
});
