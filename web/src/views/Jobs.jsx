// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useMemo, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { Ago, ArmButton, CopyBtn, ErrBox, KV, LiveBadge, useArm, useNow, usePoll } from '../components.jsx';
import { fmtDateTime, shortId } from '../format.js';
import { envPath, jobPath, objectPath } from '../routes.js';
import { useEnvStatus } from '../envstatus.jsx';
import { useSummary } from '../summary.jsx';
import { parseLabelFilter } from './envs.js';
import { EnvLink } from './EnvTabs.jsx';
import { Labels } from './Environments.jsx';
import {
  EMPTY_FORM, JOB_LIMITS, ended, fmtSpan, jobCounts, jobDurationMs, jobRequest, jobTone, jobWords, keptUntil,
  onItsWay, phaseWord, resultView,
} from './jobs.js';
import { reachesOf } from './reach.js';
import { EgressEditor, ReachesPicker, ReachesSummary, ReachesWords, useDeployed } from './Reach.jsx';
import '../styles/tenant.css';

// An environment's jobs made by a call (POST /api/v1/jobs, ADR 0041):
// runs to completion, one pod each, never retried, in the environment
// (ADR 0035), reaching what their create says of it. They are its
// Activity's Jobs, beside the deployments and the incidents: the list,
// newest first, with a label filter and the form of a new one; a job with
// its log's tail, its result, a cancel while it has not ended and the
// delete of its record. The jobs of the deployment are objects, on their
// own pages. /tenants/:t/environments/:env/jobs[/:id]. The list is the
// environment's (envstatus.jsx), which the sidebar reads too. A change
// needs write on the environment (write).

const NEEDS_WRITE = 'needs write on this environment';

export function Jobs({ tenant, env, sel, write = false, navigate }) {
  if (sel) return <JobDetail key={sel} tenant={tenant} env={env} id={sel} write={write} navigate={navigate} />;
  return <JobList tenant={tenant} env={env} write={write} navigate={navigate} />;
}

// PhaseLabel is a phase as a dot and its word.
export function PhaseLabel({ phase }) {
  const tone = jobTone(phase);
  return <span className={'state-label ' + tone}><span className={'sdot ' + tone} title={phase} />{phaseWord(phase)}</span>;
}

// withLabels keeps the jobs that carry every label of labels ("k=v").
export function withLabels(jobs, labels) {
  if (!jobs || !labels.length) return jobs;
  return jobs.filter((j) => labels.every((l) => {
    const i = l.indexOf('=');
    return ((j.labels || {})[l.slice(0, i)] ?? null) === l.slice(i + 1);
  }));
}

// ------------------------------------------------------------------- list --

function JobList({ tenant, env, write, navigate }) {
  const sum = useSummary();
  const { st, jobs: all, podsErr, podsOk, refreshPods } = useEnvStatus();
  const [labels, setLabels] = useState([]);
  const [draft, setDraft] = useState('');
  const [bad, setBad] = useState([]);
  const [open, setOpen] = useState(false);
  const now = useNow(1000);

  const jobs = useMemo(() => withLabels(all, labels), [all, labels]);
  const addLabels = (list) => setLabels((cur) => [...cur, ...list.filter((l) => !cur.includes(l))]);
  const counts = jobCounts(jobs);
  const declared = ((st && st.objects) || []).filter((o) => o.kind === 'job');
  const pick = (j) => navigate(jobPath(tenant, env, j.id));

  return (
    <>
      <div className="subhead jobs-head">
        <span className="facts">
          <span><b>{jobs ? jobs.length : '…'}</b> kept{labels.length ? ' with these labels' : ''}</span>
          <span><b>{counts.onItsWay}</b> on their way</span>
          <span><b>{counts.succeeded}</b> succeeded</span>
          {counts.failed > 0 && <span className="bad"><b className="bad">{counts.failed}</b> failed</span>}
          {counts.cancelled > 0 && <span><b>{counts.cancelled}</b> cancelled</span>}
        </span>
        <span className="spacer" />
        {podsOk > 0 && <LiveBadge lastOk={podsOk} />}
        <button className="primary" disabled={!write} title={write ? 'run a container to its end' : NEEDS_WRITE}
          onClick={() => setOpen((o) => !o)}>
          <Icon name={open ? 'x' : 'play'} size={14} /> {open ? 'Close' : 'Run a job'}</button>
      </div>

      {open && write && (
        <RunForm env={env} onCancel={() => setOpen(false)} onRun={async (job) => {
          await refreshPods();
          sum.refresh();
          navigate(jobPath(tenant, env, job.id));
        }} />)}

      <ErrBox error={podsErr} />
      {(all || []).length > 0 && (
        <form className="filterbar slim" onSubmit={(e) => {
          e.preventDefault();
          const p = parseLabelFilter(draft);
          setBad(p.bad);
          addLabels(p.labels);
          setDraft(p.bad.join(' '));
        }}>
          <span className="searchwrap grow">
            <Icon name="search" size={13} />
            <input className="searchbare" placeholder="filter by label: key=value, then Enter" value={draft}
              onChange={(e) => { setDraft(e.target.value); setBad([]); }} />
          </span>
          {labels.map((l) => (
            <button key={l} type="button" className="chipbtn on mono" title="remove this filter"
              onClick={() => setLabels((cur) => cur.filter((x) => x !== l))}>
              {l} <Icon name="x" size={11} />
            </button>
          ))}
        </form>)}
      {bad.length > 0 && <p className="notice small">A label filter is key=value: {bad.join(', ')}</p>}

      {jobs && jobs.length > 0 && (
        <div className="tblwrap">
          <table className="tbl jobs-tbl">
            <thead>
              <tr>
                <th>phase</th>
                <th>job</th>
                <th>reaches</th>
                <th>image</th>
                <th>labels</th>
                <th className="col-when">created</th>
                <th className="col-num">duration</th>
                <th className="col-num">exit</th>
              </tr>
            </thead>
            <tbody>
              {jobs.map((j) => (
                <tr key={j.id} className="clickable" onClick={() => pick(j)}>
                  <td className="nowrap"><PhaseLabel phase={j.phase} /></td>
                  <td className="mono nowrap">
                    <EnvLink to={jobPath(tenant, env, j.id)} navigate={navigate}>{shortId(j.id)}</EnvLink>
                  </td>
                  <td className="small"><ReachesWords reaches={reachesOf(j)} /></td>
                  <td className="mono small trunc" title={j.imageRef || j.image}>{j.image}</td>
                  <td><Labels labels={j.labels} onLabel={(k, v) => addLabels([`${k}=${v}`])} /></td>
                  <td className="col-when"><Ago iso={j.createdAt} /></td>
                  <td className="col-num mono small">{fmtSpan(jobDurationMs(j, now))}</td>
                  <td className={'col-num mono' + (j.exitCode ? ' bad' : '')}>{j.exitCode ?? <span className="muted">—</span>}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>)}
      {jobs && jobs.length === 0 && (
        <p className="empty">
          {labels.length ? 'No job matches these labels.' : `No job made by a call is kept in ${env}.`}
          {!labels.length && write && <> <button className="linkbtn" onClick={() => setOpen(true)}>Run one →</button></>}
        </p>)}
      {!jobs && !podsErr && <p className="empty">Loading…</p>}
      <p className="footnote muted small">
        A job made by a call is one container run to its end exactly once, with a deadline; it is never retried. Its
        record, with the exit code, the result and the log&apos;s tail, is kept {JOB_LIMITS.keptDays} days after it ends,
        then it goes, and with the environment. It reaches what its create says of the environment. Jobs queued or
        running count towards the tenant&apos;s limits.
        {declared.length > 0 && (
          <> The deployment&apos;s jobs run with their deployment or when asked, on their pages:{' '}
            {declared.map((o, i) => (
              <React.Fragment key={o.name}>{i > 0 && ', '}
                <EnvLink to={objectPath(tenant, env, `job/${o.name}`)} navigate={navigate} className="mono">{o.name}</EnvLink>
              </React.Fragment>))}.</>)}
      </p>
    </>
  );
}

// ------------------------------------------------------------------- form --

// Field is a labelled control of the form, with a hint below.
function Field({ label, hint, wide, children }) {
  return (
    <label className={'jf-field' + (wide ? ' wide' : '')}>
      <span className="jf-label">{label}</span>
      {children}
      {hint && <span className="jf-hint muted">{hint}</span>}
    </label>
  );
}

// RunForm is a new job in env, reaching nothing, all, or entries of its
// deployed deployment, with an egress.
export function RunForm({ env, onRun, onCancel }) {
  const [form, setForm] = useState(() => ({ ...EMPTY_FORM, environment: env }));
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);
  const set = (k) => (e) => setForm((f) => ({ ...f, [k]: e.target.value }));
  const dep = useDeployed(env);
  const { body, errors } = jobRequest({ ...form, environment: env });
  const L = JOB_LIMITS;

  const submit = async (e) => {
    e.preventDefault();
    if (errors.length || busy) return;
    setBusy(true);
    setErr('');
    try {
      const job = await api.createJob(body);
      // The secrets and the password are never shown again.
      setForm({ ...EMPTY_FORM, environment: env });
      await onRun(job);
    } catch (x) {
      setErr(x.message);
      setBusy(false);
    }
  };

  return (
    <section className="pane">
      <div className="pane-toolbar"><h2><Icon name="play" size={14} /> Run a job in <span className="mono">{env}</span></h2></div>
      <form className="jobform" onSubmit={submit}>
        <div className="jf-field">
          <span className="jf-label">reaches</span>
          <ReachesPicker spec={dep.spec} mode={form.reachMode} list={form.reaches} err={dep.err}
            onChange={(reachMode, reaches) => setForm((f) => ({ ...f, reachMode, reaches }))} />
          <span className="jf-hint muted">What of the environment its pod may reach, fixed for the run.</span>
        </div>
        <div className="jf-field">
          <span className="jf-label">egress</span>
          <EgressEditor value={form.egress} who="the job" onChange={(egress) => setForm((f) => ({ ...f, egress }))} />
        </div>
        <Field label="image" wide hint="Pinned to its digest when the job is made; a registry that refuses it refuses the job.">
          <input className="mono" value={form.image} onChange={set('image')} placeholder="ghcr.io/acme/report:3" />
        </Field>
        <Field label="command" wide hint="One argument per line, as typed (a space belongs to its argument). Replaces the image's entrypoint and command; empty: they run.">
          <textarea className="mono" rows={3} value={form.command} onChange={set('command')} placeholder={'sh\n-c\necho done > /tmp/result.json'} />
        </Field>
        <Field label="env" hint="KEY=value, one per line.">
          <textarea className="mono" rows={3} value={form.env} onChange={set('env')} placeholder="MONTH=2026-10" />
        </Field>
        <Field label="secrets" hint="KEY=value, one per line. Sealed, never shown again, redacted from the log, dropped when the job ends.">
          <textarea className="mono secretarea" rows={3} value={form.secrets} onChange={set('secrets')} placeholder="API_TOKEN=…"
            autoComplete="off" spellCheck={false} />
        </Field>
        <div className="jf-row wide">
          <Field label="vCPUs" hint={`${L.minVcpus}–${L.maxVcpus}; empty: ${L.defaultVcpus}`}>
            <input className="num mono" inputMode="numeric" value={form.vcpus} onChange={set('vcpus')} placeholder={String(L.defaultVcpus)} />
          </Field>
          <Field label="memory (MiB)" hint={`${L.minMemoryMib}–${L.maxMemoryMib}; empty: ${L.defaultMemoryMib}`}>
            <input className="num mono" inputMode="numeric" value={form.memoryMib} onChange={set('memoryMib')} placeholder={String(L.defaultMemoryMib)} />
          </Field>
          <Field label="deadline" hint="From now, the queue included; empty: 10m, at most 2h.">
            <input className="short mono" value={form.deadline} onChange={set('deadline')} placeholder={L.defaultDeadline} />
          </Field>
          <Field label="result path" hint="A file read once the process exited; its first 4 KiB are the result. Not under /dev.">
            <input className="mono" value={form.resultPath} onChange={set('resultPath')} placeholder="/tmp/result.json" />
          </Field>
        </div>
        <Field label="labels" hint="key=value, one per line, to find the job by.">
          <textarea className="mono" rows={2} value={form.labels} onChange={set('labels')} placeholder="kind=report" />
        </Field>
        <Field label="private image" hint="The registry's login, if the image is not public. Sealed with the secrets.">
          <div className="jf-creds">
            <input className="mono" value={form.username} onChange={set('username')} placeholder="username" autoComplete="off" />
            <input type="password" value={form.password} onChange={set('password')} placeholder="password or token" autoComplete="new-password" />
          </div>
        </Field>
        {form.image.trim() && errors.length > 0 && (
          <ul className="jf-errors">{errors.map((x) => <li key={x}>{x}</li>)}</ul>)}
        <ErrBox error={err} />
        <div className="jf-actions wide">
          <button type="submit" className="primary" disabled={busy || errors.length > 0}>
            <Icon name="play" size={14} /> {busy ? 'Starting…' : 'Run'}</button>
          <button type="button" className="ghost" onClick={onCancel}>Cancel</button>
          <span className="muted small">It runs once and is never retried. A new job past the tenant's limits is refused.</span>
        </div>
      </form>
    </section>
  );
}

// ----------------------------------------------------------------- detail --

// JobDetail is a job's page in its environment's Activity: back to the
// jobs, its head with Cancel, why it is in its phase, the job, its
// result, its log and the delete of its record.
function JobDetail({ tenant, env, id, write, navigate }) {
  const sum = useSummary();
  const { refreshPods } = useEnvStatus();
  const [job, setJob] = useState(null);
  const [err, setErr] = useState('');
  const [gone, setGone] = useState(false);
  const [actErr, setActErr] = useState('');
  const [armed, arm, disarm] = useArm();
  const now = useNow(1000);
  const live = !job || onItsWay(job.phase);
  const list = jobPath(tenant, env);

  const refresh = useCallback(async () => {
    try {
      setJob(await api.getJob(id));
      setErr('');
    } catch (e) {
      if (e.status === 404) setGone(true);
      else setErr(e.message);
    }
  }, [id]);
  const lastOk = usePoll(refresh, live ? 2000 : 30000);

  const act = async (fn) => {
    setActErr('');
    try {
      await fn();
      refreshPods();
      sum.refresh();
    } catch (e) {
      setActErr(e.message);
    }
  };

  const back = (
    <div className="subhead">
      <button className="ghost mini" onClick={() => navigate(list)}><Icon name="arrowLeft" size={13} /> jobs</button>
    </div>);

  // A job of another environment is not this one's.
  const elsewhere = job && job.environment && ![job.environment.name, job.environment.id].includes(env) ? job.environment : null;
  if (gone || elsewhere) {
    return (
      <>
        {back}
        <p className="empty">
          {elsewhere
            ? <>Job <span className="mono">{id}</span> runs in{' '}
              <EnvLink to={jobPath(tenant, elsewhere.name || elsewhere.id, id)} navigate={navigate} className="mono">
                {elsewhere.name || elsewhere.id}</EnvLink>.</>
            : <>No job <span className="mono">{id}</span> in {env}. A job&apos;s record goes {JOB_LIMITS.keptDays} days after it
              ended, when someone deleted it, or with its environment.</>}
        </p>
      </>
    );
  }

  const done = job && ended(job.phase);
  const kept = job && keptUntil(job);
  return (
    <>
      {back}
      <div className="deployment-head job-head">
        <span className="mono strong">job {id}</span>
        {job && <PhaseLabel phase={job.phase} />}
        <span className="spacer" />
        <LiveBadge lastOk={lastOk} />
        {job && !done && (
          <ArmButton armKey="cancel" armed={armed} arm={arm} disarm={disarm} icon="x" disabled={!write}
            title={write ? 'stop it: it ends cancelled, its pod is deleted' : NEEDS_WRITE} confirmLabel="cancel the job?"
            onConfirm={() => act(async () => { setJob(await api.cancelJob(id)); })}>Cancel</ArmButton>)}
      </div>
      {job && (
        <div className="facts">
          <span>reaches <b><ReachesWords reaches={reachesOf(job)} /></b></span>
          <span className="mono" title={job.imageRef}>{job.image}</span>
          <span>created <b><Ago iso={job.createdAt} /></b></span>
          <span>ran <b>{fmtSpan(jobDurationMs(job, now))}</b></span>
          <span><b>{job.vcpus}</b> vCPU{job.vcpus === 1 ? '' : 's'} · <b>{job.memoryMib}</b> MiB</span>
          {job.exitCode !== null && job.exitCode !== undefined && (
            <span>exit code <b className={job.exitCode ? 'bad' : 'ok'}>{job.exitCode}</b></span>)}
        </div>)}
      <ErrBox error={err} />
      <ErrBox error={actErr} />
      {!job && !err && <p className="empty">Loading…</p>}

      {job && (
        <>
          <div className={'jobwhy ' + jobTone(job.phase)}>
            <span className={'sdot ' + jobTone(job.phase)} />
            <span>
              <span className="mono small jobwhy-word">{job.phase}{job.reason ? ` · ${job.reason}` : ''}</span>
              {' '}{jobWords(job)}
              {job.phase === 'queued' && job.deadlineAt && <> Its deadline counts already: <Ago iso={job.deadlineAt} />.</>}
              {job.detail && job.detail !== job.phase && <span className="jobwhy-detail mono">{job.detail}</span>}
            </span>
          </div>

          <div className="job-grid">
            <section className="pane">
              <div className="pane-toolbar"><h2><Icon name="jobs" size={14} /> The job</h2></div>
              <div className="kv-grid">
                <KV k="environment"><EnvLink to={envPath(tenant, env)} navigate={navigate} className="mono">{env}</EnvLink></KV>
                <KV k="reaches"><ReachesSummary pod={{ reaches: reachesOf(job), egress: job.egress }} /> <span className="muted small">fixed for its run</span></KV>
                <KV k="image" className="mono">{job.image}</KV>
                <KV k="pinned" className="mono small">
                  {job.imageRef || <span className="muted">—</span>}
                  {job.imageRef && <> <CopyBtn text={job.imageRef} mini title="copy the pinned reference" /></>}
                </KV>
                <KV k="size">{job.vcpus} vCPU{job.vcpus === 1 ? '' : 's'}, {job.memoryMib} MiB</KV>
                <KV k="created">{fmtDateTime(job.createdAt)}</KV>
                <KV k="started">{job.startedAt ? fmtDateTime(job.startedAt) : <span className="muted">not yet</span>}</KV>
                <KV k="ended">{job.finishedAt ? fmtDateTime(job.finishedAt) : <span className="muted">not yet</span>}</KV>
                <KV k="deadline">
                  {fmtDateTime(job.deadlineAt)}
                  {!done && job.deadlineAt && <span className="muted"> · <Ago iso={job.deadlineAt} /></span>}
                </KV>
                <KV k="labels">{Object.keys(job.labels || {}).length ? <Labels labels={job.labels} /> : <span className="muted">—</span>}</KV>
                <KV k="kept">
                  {kept ? <>until {fmtDateTime(kept)} <span className="muted">· {JOB_LIMITS.keptDays} days after its end</span></>
                    : <span className="muted">{JOB_LIMITS.keptDays} days after it ends</span>}
                </KV>
              </div>
            </section>
            <ResultPane job={job} />
          </div>

          <JobLog id={id} phase={job.phase} />

          {done && (
            <section className="pane danger-zone">
              <div className="pane-toolbar"><h2><Icon name="trash" size={14} /> Delete</h2></div>
              <p className="small muted">
                The record, with its result and log's tail, goes now instead of {JOB_LIMITS.keptDays} days after its end.
                The log store keeps its log lines by its own retention.
              </p>
              <ArmButton armKey="delete" armed={armed} arm={arm} disarm={disarm} icon="trash" disabled={!write}
                title={write ? 'delete the job\'s record' : NEEDS_WRITE}
                confirmLabel="delete the record?"
                onConfirm={() => act(async () => {
                  await api.deleteJob(id);
                  navigate(list);
                })}>Delete the job</ArmButton>
            </section>)}
        </>)}
    </>
  );
}

function ResultPane({ job }) {
  const view = resultView(job.result);
  const done = ended(job.phase);
  return (
    <section className="pane">
      <div className="pane-toolbar">
        <h2><Icon name="file" size={14} /> Result</h2>
        {view.json && <span className="badge">JSON</span>}
        <span className="spacer" />
        {view.text && <CopyBtn text={job.result} title="copy the result as it is" />}
      </div>
      {!job.resultPath && <p className="empty small">This job names no result file (resultPath).</p>}
      {job.resultPath && (
        <>
          <p className="small muted">The first {JOB_LIMITS.resultBytes / 1024} KiB of <code className="mono">{job.resultPath}</code>,
            read once the process has exited.</p>
          {view.text && <pre className="logpre jobresult">{view.text}</pre>}
          {!view.text && <p className="empty small">{done ? 'The file was empty, or not there.' : 'Not yet: the job has not ended.'}</p>}
        </>)}
    </section>
  );
}

// JobLog is the log's tail: from the pod while the job runs (read every
// 3 s), the kept tail once it ended (read once).
function JobLog({ id, phase }) {
  const [lines, setLines] = useState(null);
  const [err, setErr] = useState(null);
  const live = onItsWay(phase);
  const refresh = useCallback(async () => {
    try {
      const res = await api.jobLogs(id);
      setLines(res.lines || []);
      setErr(null);
    } catch (e) {
      setErr(e);
    }
    // phase: a job that ends is read once more, for its kept tail.
  }, [id, phase]);
  const lastOk = usePoll(refresh, live ? 3000 : 3600000);

  let note = '';
  if (phase === 'queued' && (err || !(lines && lines.length))) note = 'No log yet: the job\'s pod has not started.';
  else if (lines && lines.length === 0) note = live ? 'Nothing written yet.' : 'The job wrote nothing.';

  return (
    <section className="pane">
      <div className="pane-toolbar">
        <h2><Icon name="file" size={14} /> Log <span className="count">{live ? 'the pod\'s, live' : 'the kept tail'}</span></h2>
        <span className="spacer" />
        {live && <LiveBadge lastOk={lastOk} />}
        {lines && lines.length > 0 && <CopyBtn text={lines.join('\n')} title="copy the log" />}
      </div>
      {note && <p className="empty small">{note}</p>}
      {!note && err && <ErrBox error={err.message} />}
      {lines && lines.length > 0 && <pre className="logpre joblog">{lines.join('\n')}</pre>}
      {!lines && !err && <p className="empty small">Loading…</p>}
      <p className="small muted">
        The last 200 lines; once it ended ferund keeps at most 1000 within 64 KiB, the secrets' values redacted.
        {live ? ' Read every 3 seconds while the job has not ended.' : ''}
      </p>
    </section>
  );
}
