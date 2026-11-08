// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { Ago, ErrBox, StateDot, usePoll } from '../components.jsx';
import { shortId } from '../format.js';
import { jobPath, sandboxPath } from '../routes.js';
import { EnvLink } from './EnvTabs.jsx';
import { ReachesWords } from './Reach.jsx';
import { reachesOf } from './reach.js';
import { stateWords } from './sandboxes.js';
import { jobTone, onItsWay, phaseWord } from './jobs.js';

// What an environment runs beside its deployments (ADR 0035): its
// sandboxes and its tenant's jobs, made by a call, never by a
// deployment, each reaching what it was told of the environment.

const enc = encodeURIComponent;

// EnvPods lists the sandboxes and the jobs of env, read every 5 s; write
// offers a new sandbox in it.
export function EnvPods({ tenant, env, write = false, navigate }) {
  const [sandboxes, setSandboxes] = useState(null);
  const [jobs, setJobs] = useState(null);
  const [err, setErr] = useState('');
  const refresh = useCallback(async () => {
    try {
      const [s, j] = await Promise.all([api.listSandboxes(env), api.listJobs([], env)]);
      setSandboxes(s.sandboxes || []);
      setJobs(j.jobs || []);
      setErr('');
    } catch (e) {
      setErr(e.message);
    }
  }, [env]);
  usePoll(refresh, 5000);
  return <PodTables tenant={tenant} env={env} sandboxes={sandboxes} jobs={jobs} err={err} write={write} navigate={navigate} />;
}

// PodTables are the two lists, as EnvPods reads them.
export function PodTables({ tenant, env, sandboxes, jobs, err = '', write = false, navigate }) {
  const newSandbox = `${sandboxPath(tenant, 'new')}?environment=${enc(env)}`;
  const running = (jobs || []).filter((j) => onItsWay(j.phase)).length;
  return (
    <>
      <div className="envsect">
        <div className="sect">sandboxes <span className="count">· {sandboxes ? sandboxes.length : '…'}</span>
          {write && <> · <EnvLink to={newSandbox} navigate={navigate}><Icon name="plus" size={11} /> new sandbox in {env}</EnvLink></>}
        </div>
        <ErrBox error={err} />
        {sandboxes && sandboxes.length === 0 && (
          <p className="empty small">No sandbox in this environment. One made in it reaches what its create names of it,
            and runs its attached services.</p>)}
        {sandboxes && sandboxes.length > 0 && (
          <div className="tblwrap">
            <table className="tbl">
              <thead><tr><th>sandbox</th><th>state</th><th>reaches</th><th>image</th><th className="col-when">last activity</th></tr></thead>
              <tbody>
                {sandboxes.map((sb) => (
                  <tr key={sb.sandboxId}>
                    <td className="mono"><EnvLink to={sandboxPath(tenant, sb.sandboxId)} navigate={navigate}>{sb.sandboxId}</EnvLink></td>
                    <td className="nowrap"><StateDot state={sb.clientState} /> {stateWords(sb).short}</td>
                    <td className="small"><ReachesWords reaches={reachesOf(sb)} /></td>
                    <td className="mono small muted trunc" title={sb.image}>{sb.image || 'the default'}</td>
                    <td className="col-when"><Ago iso={sb.updatedAt} /></td>
                  </tr>))}
              </tbody>
            </table>
          </div>)}
      </div>
      <div className="envsect">
        <div className="sect">jobs of the tenant <span className="count">· {jobs ? jobs.length : '…'}{running ? `, ${running} on their way` : ''}</span>
          {' · '}<EnvLink to={jobPath(tenant)} navigate={navigate}>all jobs</EnvLink>
        </div>
        {jobs && jobs.length === 0 && <p className="empty small">No job run in this environment is kept.</p>}
        {jobs && jobs.length > 0 && (
          <div className="tblwrap">
            <table className="tbl">
              <thead><tr><th>phase</th><th>job</th><th>reaches</th><th>image</th><th className="col-when">created</th></tr></thead>
              <tbody>
                {jobs.map((j) => (
                  <tr key={j.id}>
                    <td className="nowrap"><span className={'state-label ' + jobTone(j.phase)}>
                      <span className={'sdot ' + jobTone(j.phase)} title={j.phase} />{phaseWord(j.phase)}</span></td>
                    <td className="mono"><EnvLink to={jobPath(tenant, j.id)} navigate={navigate}>{shortId(j.id)}</EnvLink></td>
                    <td className="small"><ReachesWords reaches={reachesOf(j)} /></td>
                    <td className="mono small trunc" title={j.imageRef || j.image}>{j.image}</td>
                    <td className="col-when"><Ago iso={j.createdAt} /></td>
                  </tr>))}
              </tbody>
            </table>
          </div>)}
      </div>
    </>
  );
}
