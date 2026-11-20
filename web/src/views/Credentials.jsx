// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { Ago, ArmButton, CopyBtn, ErrBox, PageHead, StateDot, useArm, usePoll } from '../components.jsx';
import { fmtDateTime, shortId } from '../format.js';
import { useEnvStatus } from '../envstatus.jsx';
import { credentialPath, envPath, newCredentialPath, sandboxPath } from '../routes.js';
import {
  KINDS, REDACTED, bindingOf, curlOf, emptyForm, formOf, hostOf, isName, keptOf, kindLabel, modeWords,
  needsDeployment, pinState, pinWords, pinnedSpec, preflightRows, prefixesOf, specSnippet, usedWords,
} from './credentials.js';
import { EnvLink, NEEDS } from './EnvTabs.jsx';
import '../styles/credentials.css';

// An environment's credentials (ADR 0033): an API the environment's code
// calls by a name, whose credential FeOS adds on the node, outside the
// pod. The list is a group of the environment's Secrets view
// (EnvSecrets.jsx), whose pages these are: a credential's page with its
// check and its uses in the environment (the sandboxes that name it, the
// bindings and the version its deployments pin, moved to the latest from
// here, moveToLatest); the form of a new one or a new version, with a
// preflight and what the code sees (the concept's Fig. 5). The sandbox's
// side (SandboxCredentials, CredentialPicker) is exported for the
// sandbox pages. Reads need read on the environment, writes write.

const POLL_MS = 10000;

// saved carries a save's answer to the credential's page, which says it
// once.
let saved = null;

// Credentials are the pages of a credential of environment env (its
// name or ID, as the route has it): sel is a credential's name, "new"
// the form of a new one; edit the form of a new version. Without sel,
// nothing: the list is the Secrets view's.
export function Credentials({ tenant, env, sel, edit, ec, navigate }) {
  if (sel === 'new') return <CredentialEditor tenant={tenant} env={env} name="" ec={ec} navigate={navigate} />;
  if (sel && edit) return <CredentialEditor key={sel} tenant={tenant} env={env} name={sel} ec={ec} navigate={navigate} />;
  if (sel) return <CredentialPage key={sel} tenant={tenant} env={env} name={sel} ec={ec} navigate={navigate} />;
  return null;
}

// go is a link's click that navigates.
const go = (navigate, to) => (e) => { e.preventDefault(); e.stopPropagation(); navigate(to); };

// BackTo is the way back to the environment's credentials, in its
// Secrets view.
function BackTo({ tenant, env, navigate, children }) {
  return (
    <div className="subhead envsubhead">
      <button type="button" className="ghost mini" onClick={() => navigate(credentialPath(tenant, env))}>
        <Icon name="arrowLeft" size={13} /> secrets</button>
      {children}
    </div>
  );
}

// ------------------------------------------------------------------ page --

function CredentialPage({ tenant, env, name, ec, navigate }) {
  const [cred, setCred] = useState(null);
  const [err, setErr] = useState('');
  const [missing, setMissing] = useState(false);
  const [note] = useState(() => {
    const n = saved && saved.env === env && saved.name === name ? saved : null;
    saved = null;
    return n;
  });
  const [check, setCheck] = useState(null);
  const [checking, setChecking] = useState(false);
  const [probe, setProbe] = useState({ method: 'GET', path: '' });
  const [delErr, setDelErr] = useState('');
  const [armed, arm, disarm] = useArm();

  const load = useCallback(async () => {
    try {
      setCred(await api.getCredential(env, name));
      setErr('');
    } catch (e) {
      if (e.status === 404) setMissing(true);
      setErr(e.message);
    }
  }, [env, name]);
  usePoll(load, POLL_MS);

  if (missing && !cred) {
    return (
      <div className="creds">
        <BackTo tenant={tenant} env={env} navigate={navigate} />
        <p className="empty">No credential <span className="mono">{name}</span> in this environment.</p>
      </div>
    );
  }
  if (!cred) return err ? <ErrBox error={err} /> : <p className="empty">Loading…</p>;

  const runCheck = async () => {
    setChecking(true);
    try {
      const path = probe.path.trim();
      const p = path ? { method: probe.method, path } : null;
      setCheck({ result: await api.checkCredential(env, name, { probe: p }), probe: p });
    } catch (e) {
      setCheck({ result: { ok: false, addresses: [], error: { code: (e.body && e.body.code) || 'error', message: e.message } } });
    } finally {
      setChecking(false);
    }
  };
  const cr = cred.credential || {};
  const why = ec.write ? undefined : NEEDS.write;
  const toEdit = credentialPath(tenant, env, name, true);

  return (
    <div className="creds">
      <BackTo tenant={tenant} env={env} navigate={navigate} />
      <PageHead kind="Credential" title={name} mono
        state={<span className="badge acc">version {cred.version}</span>}
        actions={(
          <>
            <button disabled={!ec.write || checking} title={why || 'the preflight with the stored credential'} onClick={runCheck}>
              <Icon name="check" size={14} /> {checking ? 'Checking…' : 'Check'}</button>
            <a className={'button primary' + (ec.write ? '' : ' disabled')} href={toEdit}
              title={why || 'write a new version'}
              onClick={(e) => { e.preventDefault(); if (ec.write) navigate(toEdit); }}>
              <Icon name="settings" size={14} /> Edit</a>
          </>)}
        facts={(
          <>
            <span>upstream <b className="mono cred-up">{cred.upstream}</b></span>
            <span>reached as <b className="mono">{modeWords(cred)}</b></span>
            <span>kind <b>{kindLabel(cr.kind)}</b></span>
            <span>created by <b>{cred.createdBy || '—'}</b></span>
            <span title={fmtDateTime(cred.createdAt)}>version {cred.version} saved <b><Ago iso={cred.createdAt} /></b></span>
          </>)} />
      <ErrBox error={err} />
      {note && (
        <p className="ok small cred-note"><Icon name="check" size={12} />{' '}
          {note.created ? `Saved as version ${note.version}.` : `Unchanged: still version ${note.version}.`}
          {note.created && ((cred.usedBy && cred.usedBy.sandboxes) || []).length > 0 && ' The sandboxes that name it have it now.'}
          {note.created && needsDeployment(cred.usedBy || {}, cred.version) && ' The bindings get it with a deployment, below.'}
        </p>)}

      {check && (
        <section className="pane">
          <div className="pane-toolbar"><h2><Icon name="shield" size={14} /> Check</h2>
            <span className={'badge ' + (check.result.ok ? 'ok' : 'bad')}>{check.result.ok ? 'ok' : 'failed'}</span></div>
          <PreflightRows result={check.result} probe={check.probe} />
        </section>)}

      <div className="cred-grid">
        <section className="pane">
          <div className="pane-toolbar"><h2><Icon name="plug" size={14} /> Definition</h2></div>
          <Definition cred={cred} />
          <div className="cred-probe">
            <span className="muted small">Check probes</span>
            <select value={probe.method} onChange={(e) => setProbe({ ...probe, method: e.target.value })}>
              <option>GET</option><option>HEAD</option>
            </select>
            <input className="mono" placeholder={(cred.pathPrefixes || [])[0] || '/path (optional)'} value={probe.path}
              onChange={(e) => setProbe({ ...probe, path: e.target.value })} />
          </div>
        </section>
        <section className="pane">
          <div className="pane-toolbar"><h2><Icon name="terminal" size={14} /> What the code sees</h2></div>
          <CodeSees c={cred} />
        </section>
      </div>

      <UsedBy tenant={tenant} env={env} cred={cred} ec={ec} navigate={navigate} onChanged={load} />

      <section className="pane cred-danger">
        <div className="pane-toolbar"><h2><Icon name="trash" size={14} /> Delete</h2></div>
        <p className="muted small">Deletes every version. Refused while a sandbox of this environment names it or its
          latest or deployed deployment pins it.</p>
        <ErrBox error={delErr} />
        <ArmButton armKey="delete" armed={armed} arm={arm} disarm={disarm} disabled={!ec.write} title={why}
          icon="trash" confirmLabel={`delete ${name}?`}
          onConfirm={async () => {
            setDelErr('');
            try {
              await api.deleteCredential(env, name);
              navigate(credentialPath(tenant, env));
            } catch (e) {
              setDelErr(e.message);
            }
          }}>Delete credential</ArmButton>
      </section>
    </div>
  );
}

// Definition is a stored credential's fields, every secret one shown as
// set.
function Definition({ cred }) {
  const cr = cred.credential || {};
  const set = <span className="cred-set">set</span>;
  const rows = [
    ['upstream', <span className="mono cred-up">{cred.upstream}</span>],
    ['mode', cred.mode === 'intercept'
      ? <span>intercept <span className="mono">{hostOf(cred.upstream)}:443</span></span>
      : <span>by name <span className="mono">http://{cred.name}/…</span></span>],
    ['paths', (cred.pathPrefixes || []).length ? <span className="mono">{cred.pathPrefixes.join('  ')}</span> : <span className="muted">all</span>],
    ['rate limit', cred.rateLimit ? `${cred.rateLimit.perSecond} per second, burst ${cred.rateLimit.burst}` : <span className="muted">none</span>],
  ];
  if (cred.forwardedHost) rows.push(['forwarded host', 'X-Forwarded-Host and X-Forwarded-Proto sent']);
  rows.push(['CA', cred.caPem ? 'its own CA' : <span className="muted">the WebPKI</span>]);
  rows.push(['credential', kindLabel(cr.kind)]);
  if (cr.kind === 'headers') {
    for (const h of Object.keys(cr.headers || {}).sort()) rows.push([h, set]);
  }
  if (cr.kind === 'basic') {
    rows.push(['user', <span className="mono">{cr.user}</span>]);
    rows.push(['password', cr.password ? set : '—']);
  }
  if (cr.kind === 'oauth2') {
    rows.push(['token URL', <span className="mono">{cr.tokenUrl}</span>]);
    rows.push(['client ID', <span className="mono">{cr.clientId}</span>]);
    rows.push(['client secret', cr.clientSecret ? set : '—']);
    if (cr.scope) rows.push(['scope', <span className="mono">{cr.scope}</span>]);
    if (cr.audience) rows.push(['audience', <span className="mono">{cr.audience}</span>]);
    if (cr.clientAuthInBody) rows.push(['client auth', 'in the body']);
  }
  if (cr.kind === 'clientCertificate') {
    rows.push(['certificate', cr.certPem ? 'PEM' : '—']);
    rows.push(['key', cr.keyPem ? set : '—']);
  }
  return (
    <div className="kv-grid">
      {rows.map(([k, v]) => (
        <React.Fragment key={k}><span className="k">{k}</span><span className="v">{v}</span></React.Fragment>))}
    </div>
  );
}

// CodeSees is the request the code runs, without a credential, and what
// comes back of an echoed secret.
export function CodeSees({ c }) {
  return (
    <div className="cred-sees">
      <pre className="cred-snip"><span className="c"># in the pod, no credential anywhere</span>{'\n'}$ {curlOf(c)}{'\n'}{'{ …, "token": "'}<span className="red">{REDACTED}</span>{'" }'}</pre>
      <p className="muted small">
        {c.mode === 'intercept'
          ? <>The code calls the API under its own name; the pod trusts FeOS's interception CA for <span className="mono">{hostOf(c.upstream) || 'the host'}</span>.</>
          : <>The code calls plain HTTP to the name; FeOS speaks TLS to the upstream.</>}
        {' '}FeOS adds the credential on the node, and a secret an answer echoes comes back masked as{' '}
        <span className="mono">{REDACTED}</span>.
      </p>
    </div>
  );
}

// PreflightRows are a preflight's answer, row by row.
export function PreflightRows({ result, probe = null }) {
  const rows = preflightRows(result, probe);
  if (!rows.length) return <p className="muted small">{result && result.ok ? 'ok' : 'nothing checked'}</p>;
  return (
    <div className="cred-checks">
      {rows.map((r) => (
        <div key={r.key} className={r.ok ? 'ok' : 'bad'}>
          <span className="cred-mark">{r.ok ? '✓' : '✗'}</span>
          <span className={'cred-k' + (r.ok ? '' : ' mono')}>{r.key}</span>
          <span className={r.ok ? 'mono' : ''}>{r.text}</span>
        </div>))}
    </div>
  );
}

// ---------------------------------------------------------------- used by --

// UsedBy is a credential's uses in its environment: the sandboxes that
// name it (added and taken out here) and the bindings of its deployments
// that name it, with the version each pins and one deployment that moves
// them to the latest.
function UsedBy({ tenant, env, cred, ec, navigate, onChanged }) {
  const { sandboxes, refreshPods } = useEnvStatus();
  const name = cred.name;
  const uses = cred.usedBy || {};
  const sbs = uses.sandboxes || [];
  const bindings = uses.bindings || [];
  const mine = sandboxes || [];
  const sbOf = (id) => mine.find((s) => s.sandboxId === id);
  const [pick, setPick] = useState('');
  const [busy, setBusy] = useState('');
  const [err, setErr] = useState('');
  const [result, setResult] = useState(null);
  const why = ec.write ? undefined : NEEDS.write;

  // setSandbox adds the credential to a sandbox's list, or takes it out.
  const setSandbox = async (id, on) => {
    setBusy('sb:' + id);
    setErr('');
    try {
      const cur = (await api.getSandboxCredentials(id)).credentials || [];
      const next = on ? [...new Set([...cur, name])] : cur.filter((n) => n !== name);
      await api.putSandboxCredentials(id, next);
      setPick('');
      await onChanged();
      if (refreshPods) refreshPods();
    } catch (e) {
      setErr(e.message);
    } finally {
      setBusy('');
    }
  };

  const update = async () => {
    setBusy('deploy');
    setResult(null);
    try {
      setResult(await moveToLatest(env, name, cred.version));
      await onChanged();
    } catch (e) {
      setResult({ error: e.message, findings: (e.body && e.body.findings) || [] });
    } finally {
      setBusy('');
    }
  };

  const free = mine.filter((s) => !sbs.includes(s.sandboxId));
  const state = pinState(uses, cred.version);
  const builder = envPath(tenant, env, 'builder');

  return (
    <section className="pane">
      <div className="pane-toolbar"><h2><Icon name="link" size={14} /> Used by <span className="count">{usedWords(uses)}</span></h2></div>
      <ErrBox error={err} />

      <h3 className="sect"><Icon name="box" size={13} /> Sandboxes</h3>
      <p className="muted small">A sandbox of this environment that names it gets the latest version, at once and
        without a restart: a new version reaches its running pod with the save.</p>
      {sbs.length === 0 && <p className="empty">No sandbox names it.</p>}
      {sbs.length > 0 && (
        <div className="tblwrap"><table className="tbl">
          <thead><tr><th>sandbox</th><th>owner</th><th>state</th><th></th></tr></thead>
          <tbody>
            {sbs.map((id) => {
              const sb = sbOf(id);
              const to = sandboxPath(tenant, env, id, 'credentials');
              return (
                <tr key={id}>
                  <td><a className="mono" href={to} onClick={go(navigate, to)}>{id}</a></td>
                  <td className="small">{(sb && sb.owner) || <span className="muted">—</span>}</td>
                  <td className="small">{sb ? <><StateDot state={sb.clientState} /> {sb.clientState}</> : <span className="muted">—</span>}</td>
                  <td className="col-actions">
                    <button className="ghost small" disabled={!ec.write || !!busy} title={why || 'take it from this sandbox'}
                      onClick={() => setSandbox(id, false)}>{busy === 'sb:' + id ? 'Removing…' : 'Remove'}</button>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table></div>)}
      <div className="createpanel cred-use">
        <select value={pick} onChange={(e) => setPick(e.target.value)} disabled={!ec.write || !free.length} title={why}>
          <option value="">{free.length ? 'Use in a sandbox…' : mine.length ? 'every sandbox here names it' : 'no sandbox in this environment'}</option>
          {free.map((s) => <option key={s.sandboxId} value={s.sandboxId}>{shortId(s.sandboxId)} · {s.owner || 'no owner'} · {s.clientState}</option>)}
        </select>
        <button disabled={!ec.write || !pick || !!busy} title={why} onClick={() => setSandbox(pick, true)}>
          <Icon name="plus" size={14} /> {busy === 'sb:' + pick ? 'Adding…' : 'Add'}</button>
      </div>

      <h3 className="sect"><Icon name="layers" size={13} /> Bindings</h3>
      <p className="muted small">A binding of this environment&apos;s spec names the credential; each deployment pins the
        version it got, and a new version reaches the services with the next deployment.</p>
      {bindings.length === 0 && <p className="empty">No binding of a deployment names it.</p>}
      {bindings.length > 0 && (
        <div className="tblwrap"><table className="tbl cred-envs">
          <thead><tr><th>bindings</th><th>pins</th><th></th></tr></thead>
          <tbody>
            <tr>
              <td className="mono small">{bindings.join(', ')}</td>
              <td className="small">
                <span className={state === 'current' ? '' : 'warntext'}>{pinWords(uses)}</span>
                <span className="muted">
                  {state === 'behind' && ` · behind v${cred.version}`}
                  {state === 'rolling out' && ` · v${cred.version} rolling out`}
                  {state === 'current' && ' · the latest'}
                </span>
              </td>
              <td className="col-actions">
                {needsDeployment(uses, cred.version) && (
                  <button className="primary small" disabled={!ec.write || !!busy}
                    title={why || `a new deployment with ${name} at version ${cred.version}`} onClick={update}>
                    {busy === 'deploy' ? 'Deploying…' : `Update to v${cred.version}`}</button>)}
              </td>
            </tr>
            {result && (
              <tr className="subrow"><td colSpan={3}><DeployResult tenant={tenant} env={env} res={result} navigate={navigate} /></td></tr>)}
          </tbody>
        </table></div>)}

      <details className="cred-fold">
        <summary>Use in a service</summary>
        <p className="small">The environment&apos;s spec names the credential in a binding, and each service (or job)
          that calls it names the binding. The next deployment pins the latest version:</p>
        <div className="snippet"><pre className="cred-snip">{specSnippet(name)}</pre><CopyBtn text={specSnippet(name)} title="copy" /></div>
        <p className="muted small">The binding takes nothing else: the upstream, paths, rate and credential are the
          credential&apos;s.</p>
        <p className="muted small">The code then calls <span className="mono">{curlOf(cred).replace(/^curl -s /, '')}</span>.
          Write it in{' '}
          <EnvLink to={builder} navigate={navigate}>New deployment</EnvLink>: a service&apos;s form offers this
          environment&apos;s credentials.</p>
      </details>
    </section>
  );
}

// moveToLatest makes a deployment of env from its latest one with every
// binding that names credential name at version: {deployment} or, when
// the plan finds it invalid, {findings}.
export async function moveToLatest(env, name, version) {
  const e = await api.getEnvironment(env);
  const n = e.deployment;
  const d = await api.getDeployment(env, n);
  const { spec, bindings } = pinnedSpec(d.spec, name, version);
  if (!bindings.length) return { error: `deployment #${n} names no binding of ${name}` };
  const plan = await api.planEnvironment(env, spec, n);
  if (!plan.valid) return { findings: plan.findings || [], error: 'the plan is invalid' };
  const dep = await api.applyDeployment(env, { spec, base: n, message: `credential ${name} to version ${version}` });
  return { deployment: dep, bindings };
}

// DeployResult is what came of moveToLatest: the deployment it made, or
// why it made none.
export function DeployResult({ tenant, env, res, navigate }) {
  if (res.deployment) {
    const to = envPath(tenant, env, 'deployments', String(res.deployment.number));
    return (
      <span className="ok small">
        <Icon name="check" size={12} /> Deployment <a className="mono" href={to} onClick={go(navigate, to)}>#{res.deployment.number}</a>{' '}
        <span className="muted">{res.deployment.state}</span> · bindings <span className="mono">{res.bindings.join(', ')}</span>
      </span>
    );
  }
  return (
    <div className="small">
      <span className="bad">{res.error}</span>
      {(res.findings || []).map((f, i) => (
        <div key={i} className={f.severity === 'error' ? 'bad' : 'warntext'}>
          <span className="mono">{f.path}</span>: {f.problem}{f.remedy ? <span className="muted"> — {f.remedy}</span> : null}
        </div>))}
    </div>
  );
}

// ------------------------------------------------------------------ form --

function CredentialEditor({ tenant, env, name, ec, navigate }) {
  const [cred, setCred] = useState(null);
  const [err, setErr] = useState('');
  useEffect(() => {
    if (!name) return undefined;
    let live = true;
    api.getCredential(env, name).then((c) => { if (live) setCred(c); }, (e) => { if (live) setErr(e.message); });
    return () => { live = false; };
  }, [env, name]);
  if (name && !cred) return err ? <ErrBox error={err} /> : <p className="empty">Loading…</p>;
  return <CredentialForm tenant={tenant} env={env} cred={cred} ec={ec} navigate={navigate} />;
}

// CredentialForm writes a new credential of env (cred null) or a new
// version of cred, whose secret fields an empty field keeps. Its
// preflight checks the draft as it stands: a kept field is the latest
// version's value, which never leaves ferund.
export function CredentialForm({ tenant, env, cred, ec, navigate }) {
  const [f, setF] = useState(() => (cred ? formOf(cred) : emptyForm()));
  const [bad, setBad] = useState('');
  const [saving, setSaving] = useState(false);
  const [pre, setPre] = useState(null);
  const [preBusy, setPreBusy] = useState(false);
  const [probe, setProbe] = useState({ method: 'GET', path: '' });
  const set = (k) => (e) => setF({ ...f, [k]: e.target.type === 'checkbox' ? e.target.checked : e.target.value });
  const editing = !!cred;
  const name = editing ? cred.name : f.name.trim();
  const view = useMemo(() => ({ ...f, name: name || '<name>', pathPrefixes: prefixesOf(f.pathPrefixes) }), [f, name]);
  const kept = keptOf(f);
  const host = hostOf(f.upstream);
  const back = editing ? credentialPath(tenant, env, cred.name) : credentialPath(tenant, env);
  const why = ec.write ? undefined : NEEDS.write;

  const runPreflight = async () => {
    setBad('');
    setPreBusy(true);
    try {
      const binding = bindingOf(f);
      const path = probe.path.trim();
      const p = path ? { method: probe.method, path } : null;
      // A new credential without a valid name yet is checked under a
      // stand-in: the name decides nothing of the check.
      const as = isName(name) ? name : 'preflight';
      setPre({ result: await api.checkCredential(env, as, { binding, probe: p }), probe: p, kept });
    } catch (e) {
      if (e.status) setPre({ result: { ok: false, addresses: [], error: { code: (e.body && e.body.code) || 'error', message: e.message } } });
      else setBad(e.message);
    } finally {
      setPreBusy(false);
    }
  };

  const save = async (e) => {
    e.preventDefault();
    setBad('');
    if (!editing && !isName(name)) {
      setBad('the name must be a DNS label: lowercase letters, digits and "-", at most 63');
      return;
    }
    if (!editing && name === 'new') {
      setBad('"new" is the form\'s own address; choose another name');
      return;
    }
    let body;
    try {
      body = bindingOf(f);
    } catch (e2) {
      setBad(e2.message);
      return;
    }
    setSaving(true);
    try {
      const out = await api.putCredential(env, name, body);
      saved = { env, name, version: out.version, created: !editing || out.version !== cred.version };
      navigate(credentialPath(tenant, env, name));
    } catch (e2) {
      setBad(e2.body && e2.body.code ? `${e2.body.code}: ${e2.message}` : e2.message);
    } finally {
      setSaving(false);
    }
  };

  const secretInput = (k, label, multiline = false) => {
    const isSet = !!f.set[k];
    const props = {
      value: f[k], onChange: set(k), autoComplete: 'new-password',
      placeholder: isSet ? 'set — leave empty to keep' : '',
    };
    return (
      <label className="cred-field">
        <span className="cred-label">{label} <span className="muted">· secret{isSet ? ', set' : ''}</span></span>
        {multiline ? <textarea className="mono" rows={4} {...props} /> : <input type="password" {...props} />}
      </label>
    );
  };
  const textInput = (k, label, placeholder = '', hint = '') => (
    <label className="cred-field">
      <span className="cred-label">{label}</span>
      <input className="mono" value={f[k]} onChange={set(k)} placeholder={placeholder} />
      {hint && <span className="cred-hint">{hint}</span>}
    </label>
  );
  const setHeader = (i, patch) => setF({ ...f, headers: f.headers.map((h, j) => (j === i ? { ...h, ...patch } : h)) });

  return (
    <form className="creds" onSubmit={save}>
      <BackTo tenant={tenant} env={env} navigate={navigate} />
      <PageHead kind={editing ? 'Edit credential' : 'New credential'} title={editing ? cred.name : name || 'New credential'}
        mono={editing || !!name}
        state={editing ? <span className="badge">version {cred.version} → {cred.version + 1}</span> : null}
        actions={(
          <>
            <a className="button ghost" href={back} onClick={go(navigate, back)}>Cancel</a>
            <button type="submit" className="primary" disabled={!ec.write || saving} title={why}>
              <Icon name="check" size={14} /> {saving ? 'Saving…' : editing ? 'Save a new version' : 'Save'}</button>
          </>)}
        facts={<span>Code calls the upstream through FeOS. FeOS adds the credential on the node, outside the pod.</span>} />
      {bad && <ErrBox error={bad} />}
      <div className="cred-grid">
        <section className="pane cred-form">
          <div className="pane-toolbar"><h2><Icon name="plug" size={14} /> How it is applied</h2></div>
          {!editing && (
            <label className="cred-field">
              <span className="cred-label">name</span>
              <input className="mono" value={f.name} onChange={set('name')} placeholder="github" autoFocus />
              <span className="cred-hint">A DNS label. This environment&apos;s sandboxes and bindings name it; in the name mode the code calls{' '}
                <span className="mono">http://{f.name.trim() || '<name>'}/…</span>.</span>
            </label>)}
          <div className="cred-field">
            <span className="cred-label">kind</span>
            <span className="cred-seg" role="group" aria-label="kind">
              {KINDS.map((k) => (
                <button key={k.key} type="button" className={f.kind === k.key ? 'on' : ''} onClick={() => setF({ ...f, kind: k.key })}>{k.label}</button>))}
            </span>
          </div>
          {textInput('upstream', 'upstream', 'https://api.github.com', 'An https origin, no path; restrict paths below.')}
          <div className="cred-field">
            <span className="cred-label">how the code reaches it</span>
            <span className="cred-seg" role="group" aria-label="mode">
              <button type="button" className={f.mode !== 'intercept' ? 'on' : ''} onClick={() => setF({ ...f, mode: 'name' })}>
                by name <span className="mono">http://{f.name.trim() || (editing ? cred.name : '<name>')}/…</span></button>
              <button type="button" className={f.mode === 'intercept' ? 'on' : ''} onClick={() => setF({ ...f, mode: 'intercept' })}>
                intercept <span className="mono">{host || '<host>'}:443</span></button>
            </span>
            <span className="cred-hint">
              {f.mode === 'intercept'
                ? 'The code calls the API under its own name, unchanged; the pod trusts an interception CA for that host.'
                : 'The code calls plain HTTP to the name; FeOS speaks TLS to the upstream.'}
            </span>
          </div>

          <fieldset className="cred-fieldset">
            <legend>credential · {kindLabel(f.kind)}</legend>
            {f.kind === 'headers' && (
              <>
                {f.headers.map((h, i) => (
                  <div key={i} className="cred-header">
                    <input className="mono" value={h.name} placeholder="Authorization" aria-label="header name"
                      onChange={(e) => setHeader(i, { name: e.target.value })} />
                    <input type="password" value={h.value} aria-label="header value" autoComplete="new-password"
                      placeholder={h.set ? 'set — leave empty to keep' : 'Bearer …'} onChange={(e) => setHeader(i, { value: e.target.value })} />
                    <button type="button" className="iconbtn" title="remove the header"
                      onClick={() => setF({ ...f, headers: f.headers.filter((_, j) => j !== i) })}><Icon name="x" size={13} /></button>
                  </div>))}
                <div><button type="button" className="ghost small"
                  onClick={() => setF({ ...f, headers: [...f.headers, { name: '', value: '', set: false }] })}>
                  <Icon name="plus" size={12} /> header</button></div>
                <span className="cred-hint">Every value is secret: FeOS sets the header on each request.</span>
              </>)}
            {f.kind === 'basic' && (
              <div className="cred-row">{textInput('user', 'user')}{secretInput('password', 'password')}</div>)}
            {f.kind === 'oauth2' && (
              <>
                {textInput('tokenUrl', 'token URL', 'https://auth.example.com/oauth/token')}
                <div className="cred-row">{textInput('clientId', 'client ID')}{secretInput('clientSecret', 'client secret')}</div>
                <div className="cred-row">{textInput('scope', 'scope', 'optional')}{textInput('audience', 'audience', 'optional')}</div>
                <label className="cred-check"><input type="checkbox" checked={f.clientAuthInBody} onChange={set('clientAuthInBody')} />
                  <span>send the client's ID and secret in the body, not as Basic auth</span></label>
                <span className="cred-hint">FeOS fetches and renews the token; the code never sees it.</span>
              </>)}
            {f.kind === 'clientCertificate' && (
              <>
                <label className="cred-field">
                  <span className="cred-label">certificate (PEM)</span>
                  <textarea className="mono" rows={4} value={f.certPem} onChange={set('certPem')} placeholder="-----BEGIN CERTIFICATE-----" />
                </label>
                {secretInput('keyPem', 'key (PEM)', true)}
              </>)}
            {editing && kept.length > 0 && (
              <span className="cred-hint">Kept from version {cred.version}: <span className="mono">{kept.join(', ')}</span>.</span>)}
          </fieldset>

          {textInput('pathPrefixes', 'only these paths', '/repos/kino/ /user', 'Path prefixes, separated by blanks; empty: every path.')}
          <div className="cred-row">
            {textInput('ratePerSecond', 'rate limit, per second', 'none')}
            {textInput('rateBurst', 'burst', f.ratePerSecond ? f.ratePerSecond : '')}
          </div>
          <span className="cred-hint">Per pod; the burst defaults to the rate.</span>
          {f.mode !== 'intercept' && (
            <label className="cred-check"><input type="checkbox" checked={f.forwardedHost} onChange={set('forwardedHost')} />
              <span><b>forwarded host</b> <span className="muted">— send X-Forwarded-Host and X-Forwarded-Proto</span></span></label>)}
          <details className="cred-fold" open={!!f.caPem}>
            <summary>CA of the upstream <span className="muted">· optional</span></summary>
            <textarea className="mono" rows={5} value={f.caPem} onChange={set('caPem')}
              placeholder="-----BEGIN CERTIFICATE----- (empty: the WebPKI)" />
          </details>
        </section>

        <section className="pane">
          <div className="pane-toolbar"><h2><Icon name="shield" size={14} /> Preflight</h2>
            {pre && pre.result && <span className={'badge ' + (pre.result.ok ? 'ok' : 'bad')}>{pre.result.ok ? 'ok' : 'failed'}</span>}</div>
          <p className="muted small">Resolves the upstream, checks its certificate, fetches a token for OAuth2, and with a
            path sends one request with the credential, before anything is stored.</p>
          <div className="cred-probe">
            <select value={probe.method} onChange={(e) => setProbe({ ...probe, method: e.target.value })}>
              <option>GET</option><option>HEAD</option>
            </select>
            <input className="mono" placeholder={prefixesOf(f.pathPrefixes)[0] || '/path (optional)'} value={probe.path}
              onChange={(e) => setProbe({ ...probe, path: e.target.value })} />
            <button type="button" disabled={!ec.write || preBusy} title={why} onClick={runPreflight}>
              <Icon name="refresh" size={14} /> {preBusy ? 'Running…' : pre ? 'Run again' : 'Preflight'}</button>
          </div>
          {pre && pre.result && <PreflightRows result={pre.result} probe={pre.probe} />}
          {pre && pre.result && editing && (pre.kept || []).length > 0 && (
            <p className="muted small">Checked with the stored <span className="mono">{pre.kept.join(', ')}</span> of version{' '}
              {cred.version}.</p>)}

          <h3 className="sect">What the code sees</h3>
          <CodeSees c={view} />
        </section>
      </div>
    </form>
  );
}

// ------------------------------------------------------------- sandboxes --

// SandboxCredentials are the credentials a sandbox names, of its
// environment env, with their upstream and version, and the way to add
// and remove one (write on the environment): the whole list is written,
// and the running pod gets it at once.
export function SandboxCredentials({ tenant, env, sandboxId, write, navigate }) {
  const [names, setNames] = useState(null);
  const [all, setAll] = useState(null);
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState('');
  const [pick, setPick] = useState('');
  const [done, setDone] = useState('');
  const load = useCallback(async () => {
    try {
      const [mine, list] = await Promise.all([api.getSandboxCredentials(sandboxId), api.listCredentials(env)]);
      setNames(mine.credentials || []);
      setAll(list.credentials || []);
      setErr('');
    } catch (e) {
      setErr(e.message);
    }
  }, [sandboxId, env]);
  usePoll(load, POLL_MS);
  const why = write ? undefined : NEEDS.write;

  const put = async (label, next, ok) => {
    setBusy(label);
    setErr('');
    setDone('');
    try {
      const r = await api.putSandboxCredentials(sandboxId, next);
      setNames(r.credentials || next);
      setPick('');
      setDone(ok);
    } catch (e) {
      setErr(e.message);
    } finally {
      setBusy('');
    }
  };

  if (!names) return err ? <ErrBox error={err} /> : <p className="empty">Loading…</p>;
  const of = (n) => (all || []).find((c) => c.name === n);
  const free = (all || []).filter((c) => !names.includes(c.name));
  const list = credentialPath(tenant, env);
  return (
    <div className="cred-sbx">
      <p className="muted small">The credentials of its environment this sandbox&apos;s code calls by name; FeOS adds
        each on the node, outside the pod. A change reaches the running pod at once, without a restart.</p>
      <ErrBox error={err} />
      {done && <p className="ok small"><Icon name="check" size={12} /> {done}</p>}
      {names.length === 0 && <p className="empty">It names no credential.</p>}
      {names.length > 0 && (
        <div className="tblwrap"><table className="tbl">
          <thead><tr><th>credential</th><th>upstream</th><th>the code calls</th><th>version</th><th></th></tr></thead>
          <tbody>
            {names.map((n) => {
              const cr = of(n);
              const to = credentialPath(tenant, env, n);
              return (
                <tr key={n}>
                  <td><a className="mono" href={to} onClick={go(navigate, to)}>{n}</a></td>
                  <td className="mono small cred-up">{cr ? cr.upstream : <span className="muted">—</span>}</td>
                  <td className="mono small">{cr ? curlOf(cr).replace(/^curl -s /, '') : '—'}</td>
                  <td className="mono small">{cr ? `v${cr.version}` : '—'} <span className="muted">· the latest</span></td>
                  <td className="col-actions">
                    <button className="ghost small" disabled={!write || !!busy} title={why}
                      onClick={() => put('rm:' + n, names.filter((x) => x !== n), `${n} taken from the sandbox.`)}>
                      {busy === 'rm:' + n ? 'Removing…' : 'Remove'}</button>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table></div>)}
      <form className="createpanel" onSubmit={(e) => {
        e.preventDefault();
        if (pick) put('add', [...names, pick], `${pick} added: the pod reaches it now.`);
      }}>
        <select value={pick} onChange={(e) => setPick(e.target.value)} disabled={!write || !free.length} title={why}>
          <option value="">{free.length ? 'add a credential…' : (all || []).length ? 'it names every credential' : 'the environment has no credential yet'}</option>
          {free.map((c) => <option key={c.name} value={c.name}>{c.name} · {c.upstream}</option>)}
        </select>
        <button type="submit" disabled={!write || !pick || !!busy} title={why}><Icon name="plus" size={14} /> {busy === 'add' ? 'Adding…' : 'Add'}</button>
        <a className="small" href={list} onClick={go(navigate, list)}>the environment&apos;s credentials →</a>
      </form>
      <p className="muted small">The sandbox&apos;s own HTTP set, which its token writes (<code className="mono">PUT /api/v1/sandboxes/&lt;id&gt;/http</code>),
        doesn&apos;t show here.</p>
    </div>
  );
}

// CredentialPicker picks credentials of env for a new sandbox there.
export function CredentialPicker({ tenant, env, value, onChange, disabled, navigate }) {
  const [list, setList] = useState(null);
  useEffect(() => {
    let live = true;
    setList(null);
    if (!env) return undefined;
    api.listCredentials(env).then((r) => { if (live) setList(r.credentials || []); }, () => { if (live) setList([]); });
    return () => { live = false; };
  }, [env]);
  if (!env || !list) return null;
  const toggle = (n) => onChange(value.includes(n) ? value.filter((x) => x !== n) : [...value, n]);
  const toNew = newCredentialPath(tenant, env);
  return (
    <fieldset className="sbx-fieldset">
      <legend>credentials <span className="muted">· optional</span></legend>
      {list.length === 0 && (
        <span className="sbx-hint">The environment has no credential yet.{navigate && (
          <> <a href={toNew} onClick={go(navigate, toNew)}>Make one</a> in its Secrets.</>)}</span>)}
      <div className="cred-pick">
        {list.map((c) => (
          <label key={c.name} className="sbx-check">
            <input type="checkbox" checked={value.includes(c.name)} disabled={disabled} onChange={() => toggle(c.name)} />
            <span><b className="mono">{c.name}</b> <span className="muted mono small">{c.upstream}</span></span>
          </label>))}
      </div>
      {list.length > 0 && <span className="sbx-hint">The code calls each by its name; FeOS adds the credential on the node.</span>}
    </fieldset>
  );
}
