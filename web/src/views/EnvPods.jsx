// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React from 'react';
import { Icon } from '../Icon.jsx';
import { Ago, ErrBox, StateDot } from '../components.jsx';
import { shortId } from '../format.js';
import { jobPath, newSandboxPath, sandboxPath } from '../routes.js';
import { useEnvStatus } from '../envstatus.jsx';
import { EnvLink } from './EnvTabs.jsx';
import { ReachesWords } from './Reach.jsx';
import { reachesOf } from './reach.js';
import { stateWords } from './sandboxes.js';
import { jobTone, onItsWay, phaseWord } from './jobs.js';

// What an environment runs beside its deployments (ADR 0035): its
// sandboxes and the jobs made by a call, never by a deployment, each
// reaching what it was told of the environment. The overview shows the
// sandboxes and the newest jobs; their pages are the environment's
// (Sandboxes.jsx, Jobs.jsx).

// JOBS_SHOWN is how many of the newest jobs the overview shows; the rest
// are in Activity's Jobs.
export const JOBS_SHOWN = 5;

// EnvPods lists the sandboxes and the newest jobs of env, as the
// environment's status reads them (envstatus.jsx); write offers a new
// sandbox in it.
export function EnvPods({ tenant, env, write = false, navigate }) {
  const { sandboxes, jobs, podsErr } = useEnvStatus();
  return <PodTables tenant={tenant} env={env} sandboxes={sandboxes} jobs={jobs} err={podsErr} write={write} navigate={navigate} />;
}

// PodTables are the two lists, as EnvPods reads them.
export function PodTables({ tenant, env, sandboxes, jobs, err = '', write = false, navigate }) {
  const running = (jobs || []).filter((j) => onItsWay(j.phase)).length;
  const newest = [...(jobs || [])].sort((a, b) => (b.createdAt || '').localeCompare(a.createdAt || '')).slice(0, JOBS_SHOWN);
  return (
    <>
      <div className="envsect">
        <div className="sect">sandboxes <span className="count">· {sandboxes ? sandboxes.length : '…'}</span>
          {sandboxes && sandboxes.length > 0 && <> · <EnvLink to={sandboxPath(tenant, env)} navigate={navigate}>all sandboxes</EnvLink></>}
          {write && <> · <EnvLink to={newSandboxPath(tenant, env)} navigate={navigate}><Icon name="plus" size={11} /> new sandbox</EnvLink></>}
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
                    <td className="mono"><EnvLink to={sandboxPath(tenant, env, sb.sandboxId)} navigate={navigate}>{sb.sandboxId}</EnvLink></td>
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
        <div className="sect">jobs made by a call <span className="count">· {jobs ? jobs.length : '…'}{running ? `, ${running} on their way` : ''}</span>
          {jobs && jobs.length > 0 && <> · <EnvLink to={jobPath(tenant, env)} navigate={navigate}>all jobs</EnvLink></>}
        </div>
        {jobs && jobs.length === 0 && <p className="empty small">No job made by a call in this environment is kept.</p>}
        {newest.length > 0 && (
          <div className="tblwrap">
            <table className="tbl">
              <thead><tr><th>phase</th><th>job</th><th>reaches</th><th>image</th><th className="col-when">created</th></tr></thead>
              <tbody>
                {newest.map((j) => (
                  <tr key={j.id}>
                    <td className="nowrap"><span className={'state-label ' + jobTone(j.phase)}>
                      <span className={'sdot ' + jobTone(j.phase)} title={j.phase} />{phaseWord(j.phase)}</span></td>
                    <td className="mono"><EnvLink to={jobPath(tenant, env, j.id)} navigate={navigate}>{shortId(j.id)}</EnvLink></td>
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
