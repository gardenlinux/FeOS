// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import { ApiError } from '../api.js';
import { response } from '../testing/ferun.js';
import {
  changeTone, conditionText, envCounts, exitText, findObject, fmtValue, groupObjects, hasEvidence, heavyEffect,
  incidentObject, logReplicas, logSourceText, logsProblem, objectSummary, objectTone, originText, parseLabelFilter,
  phaseTone, deploymentTone, rolloutNote, statusState,
} from './envs.js';

// The environments view's arithmetic, on ferund's fixtures: stage rolls
// deployment 7 while 6 serves and expense-service crash-loops; the preview
// runs attached services in the workspace's sandbox.

const STAGE = response('GET_environments_env.200');
const PREVIEW = response('GET_environments_env.200.preview');
const LIST = response('GET_environments.200').environments;
const obj = (st, kind, name) => st.objects.find((o) => o.kind === kind && o.name === name);

describe('tones', () => {
  it('of deployment states', () => {
    expect(deploymentTone('deployed')).toBe('running');
    expect(deploymentTone('deploying')).toBe('paused');
    expect(deploymentTone('waiting')).toBe('paused');
    expect(deploymentTone('deleting')).toBe('paused');
    expect(deploymentTone('failed')).toBe('failed');
    expect(deploymentTone('replaced')).toBe('absent');
    expect(deploymentTone('none')).toBe('absent');
  });

  it('of phases and job runs', () => {
    expect(phaseTone('ready')).toBe('running');
    expect(phaseTone('succeeded')).toBe('running');
    expect(phaseTone('starting')).toBe('paused');
    expect(phaseTone('unready')).toBe('paused');
    expect(phaseTone('running')).toBe('paused');
    expect(phaseTone('exited')).toBe('failed');
    expect(phaseTone('failed')).toBe('failed');
    expect(phaseTone('stopping')).toBe('absent');
  });

  it('of objects, per kind', () => {
    // An image service with an exited instance is red, one with all its
    // replicas ready green.
    expect(objectTone(obj(STAGE, 'service', 'expense-service'))).toBe('failed');
    expect(objectTone(obj(STAGE, 'service', 'approval-service'))).toBe('running');
    expect(objectTone({ kind: 'service', runtime: 'image', desired: 2, ready: 1, instances: [] })).toBe('paused');
    expect(objectTone({ kind: 'service', runtime: 'image', desired: 0, ready: 0, instances: [] })).toBe('absent');
    expect(objectTone(obj(STAGE, 'addon', 'postgres'))).toBe('running');
    expect(objectTone({ ...obj(STAGE, 'addon', 'postgres'), provisioned: false })).toBe('paused');
    expect(objectTone(obj(STAGE, 'job', 'migrate'))).toBe('running');
    expect(objectTone({ kind: 'job', name: 'seed', lastRun: null })).toBe('absent');
    expect(objectTone(obj(PREVIEW, 'service', 'web'))).toBe('paused');
    expect(objectTone(obj(PREVIEW, 'service', 'expense-service'))).toBe('running');
  });
});

describe('objects', () => {
  it('sum up per kind', () => {
    expect(objectSummary(obj(STAGE, 'service', 'expense-service'))).toBe('image · 1 / 2 ready');
    const idle = { kind: 'service', runtime: 'image', desired: 2, ready: 0,
      instances: [{ phase: 'ready', state: 'paused' }, { phase: 'ready', state: 'hibernated' }] };
    expect(objectSummary(idle)).toBe('image · 0 / 2 ready · 2 idle, woken by a request');
    expect(objectSummary(obj(PREVIEW, 'service', 'web'))).toBe('attached · starting');
    expect(objectSummary(obj(STAGE, 'addon', 'postgres'))).toBe('postgres 17 · plan small · provisioned');
    expect(objectSummary({ ...obj(STAGE, 'addon', 'nats'), provisioned: false })).toBe('nats 2 · plan small · not provisioned');
    expect(objectSummary(obj(STAGE, 'job', 'migrate'))).toBe('deployment 7, attempt 1: succeeded (exit 0)');
    expect(objectSummary({ kind: 'job', lastRun: { deployment: 3, attempt: 2, phase: 'failed', exitCode: null } }))
      .toBe('deployment 3, attempt 2: failed');
    expect(objectSummary({ kind: 'job', lastRun: null })).toBe('not run yet');
  });

  it('group by kind in the status order, an unknown kind last', () => {
    const groups = groupObjects(STAGE.objects);
    expect(groups.map((g) => g.kind)).toEqual(['service', 'addon', 'job']);
    expect(groups[0].objects.map((o) => o.name)).toEqual(['approval-service', 'expense-service', 'web']);
    expect(groupObjects(PREVIEW.objects).map((g) => g.kind)).toEqual(['service', 'addon']);
    expect(groupObjects([{ kind: 'volume', name: 'v' }, { kind: 'job', name: 'j' }]).map((g) => g.kind))
      .toEqual(['job', 'volume']);
    expect(groupObjects([])).toEqual([]);
  });

  it("offer an image service's replicas for its logs, replica 0 of anything else", () => {
    expect(logReplicas(obj(STAGE, 'service', 'expense-service'))).toEqual([0, 1]);
    expect(logReplicas({ kind: 'service', runtime: 'image', desired: 1, instances: [{ replica: 0 }, { replica: 3 }] }))
      .toEqual([0, 3]);
    expect(logReplicas(obj(STAGE, 'addon', 'postgres'))).toEqual([0]);
    expect(logReplicas(obj(PREVIEW, 'service', 'web'))).toEqual([0]);
    expect(logReplicas(obj(STAGE, 'job', 'migrate'))).toEqual([0]);
    expect(logReplicas(null)).toEqual([0]);
  });

  it('are found by kind/name', () => {
    expect(findObject(STAGE.objects, 'addon/postgres').type).toBe('postgres');
    expect(findObject(STAGE.objects, 'service/postgres')).toBeNull();
    expect(findObject(STAGE.objects, 'postgres')).toBeNull();
    expect(findObject(null, 'job/migrate')).toBeNull();
  });

  it('name the source of their logs', () => {
    expect(logSourceText('container', obj(STAGE, 'service', 'expense-service'), 1)).toBe('the container of replica 1');
    expect(logSourceText('container', obj(STAGE, 'addon', 'postgres'), 0)).toBe('the container of the addon');
    expect(logSourceText('container', obj(STAGE, 'job', 'migrate'), 0)).toBe('the last run of the job');
    expect(logSourceText('process', obj(PREVIEW, 'service', 'expense-service'), 0))
      .toBe('the process of the attached service in sandbox 0193b0aa-1111-7222-8333-444455556601');
  });

  it('render exit codes, null as an exit without a status', () => {
    expect(exitText(1)).toBe('1');
    expect(exitText(0)).toBe('0');
    expect(exitText(null)).toBe('no status');
  });
});

describe('the logs', () => {
  it("show the 501 and 409 texts as messages, everything else as an error", () => {
    const n501 = logsProblem(new ApiError(response('GET_environments_env_objects_kind_name_logs.501').error, 501));
    expect(n501.tone).toBe('notice');
    expect(n501.text).toBe('this node does not serve container logs yet');
    expect(n501.hint).toMatch(/feosd without container logs/);
    const n409 = logsProblem(new ApiError(response('GET_environments_env_objects_kind_name_logs.409').error, 409));
    expect(n409).toMatchObject({ tone: 'notice', text: 'paused' });
    expect(n409.hint).toMatch(/never wakes a paused instance/);
    expect(logsProblem(new ApiError('no such object: service/nope', 404)))
      .toEqual({ tone: 'error', text: 'no such object: service/nope' });
    expect(logsProblem(new Error('offline'))).toEqual({ tone: 'error', text: 'offline' });
  });
});

describe('the list', () => {
  it('reads label filters, several by spaces or commas, and names the bad ones', () => {
    expect(parseLabelFilter('fecode.class=stage')).toEqual({ labels: ['fecode.class=stage'], bad: [] });
    expect(parseLabelFilter(' a=b, c=d  a=b x =y')).toEqual({ labels: ['a=b', 'c=d'], bad: ['x', '=y'] });
    expect(parseLabelFilter('k=')).toEqual({ labels: ['k='], bad: [] });
    expect(parseLabelFilter('')).toEqual({ labels: [], bad: [] });
  });

  it('counts the environments by state and their open incidents', () => {
    expect(envCounts(LIST)).toEqual({ deployed: 2, deploying: 1, failed: 0, incidents: 1 });
    expect(envCounts([{ state: 'failed', openIncidents: 2 }, { state: 'none', openIncidents: 0 }]))
      .toEqual({ deployed: 0, deploying: 0, failed: 1, incidents: 2 });
    expect(envCounts(null)).toEqual({ deployed: 0, deploying: 0, failed: 0, incidents: 0 });
  });
});

describe('the rollout', () => {
  it("names an environment's state as the list does", () => {
    expect(statusState(STAGE)).toBe('deploying');
    expect(statusState(PREVIEW)).toBe('deployed');
    expect(statusState(response('POST_environments.201'))).toBe('none');
    expect(statusState({ ...STAGE, deleting: true })).toBe('deleting');
    expect(statusState(null)).toBe('');
  });

  it('says what serves meanwhile', () => {
    expect(rolloutNote(STAGE)).toBe('deployment 6 serves meanwhile');
    expect(rolloutNote({ ...STAGE, rollout: { ...STAGE.rollout, state: 'failed' } })).toBe('deployment 6 keeps serving');
    expect(rolloutNote({ ...STAGE, deployed: 0, rollout: { ...STAGE.rollout, state: 'failed' } }))
      .toBe('no deployment was deployed before it');
    expect(rolloutNote(PREVIEW)).toBe('every object matches it');
    expect(rolloutNote(response('POST_environments.201'))).toBe('');
  });
});

describe('deployments and diffs', () => {
  it('name their origin', () => {
    const rel = response('GET_environments_env_deployments.200').deployments[0];
    expect(originText(rel)).toBe('apply');
    expect(originText({ origin: 'revert', originRef: '5' })).toBe('revert to 5');
    expect(originText({ origin: 'clone', originRef: '0193c4d0-5e6f-7a01-8b2c-3d4e5f6a7b01/7' }))
      .toBe('clone of 0193c4d0-5e6f-7a01-8b2c-3d4e5f6a7b01/7');
    expect(originText({ origin: 'object', originRef: 'service/web' })).toBe('object service/web');
  });

  it('tone changes and mark the effects that cost data or downtime', () => {
    expect(changeTone('add')).toBe('ok');
    expect(changeTone('remove')).toBe('bad');
    expect(changeTone('update')).toBe('acc');
    expect(heavyEffect('deletes data')).toBe(true);
    expect(heavyEffect('recreates (downtime)')).toBe(true);
    expect(heavyEffect('replaces 2 pods')).toBe(false);
  });

  it('render values: absent, a string, JSON', () => {
    expect(fmtValue(null)).toBe('—');
    expect(fmtValue('ghcr.io/acme/expense:1.4')).toBe('ghcr.io/acme/expense:1.4');
    expect(fmtValue(2)).toBe('2');
    expect(fmtValue({ secret: 'internal-token', version: 3 })).toBe('{"secret":"internal-token","version":3}');
  });
});

describe('incidents', () => {
  it('name their conditions and objects', () => {
    expect(conditionText('crash_loop')).toBe('crash loop');
    expect(conditionText('rollout_failed')).toBe('rollout failed');
    expect(conditionText('something_new')).toBe('something_new');
    expect(incidentObject('service/expense-service')).toEqual({ kind: 'service', name: 'expense-service', hasLogs: true });
    expect(incidentObject('job/migrate').hasLogs).toBe(true);
    expect(incidentObject('route/root').hasLogs).toBe(false);
    expect(incidentObject('odd').hasLogs).toBe(false);
  });

  it('tell evidence from none', () => {
    expect(hasEvidence(response('GET_environments_env_incidents_id.200').evidence)).toBe(true);
    expect(hasEvidence({ placement: 'no capacity (retryable): no feasible node' })).toBe(true);
    expect(hasEvidence({ pending: ['addon postgres is not ready'] })).toBe(true);
    expect(hasEvidence({})).toBe(false);
    expect(hasEvidence(null)).toBe(false);
  });
});
