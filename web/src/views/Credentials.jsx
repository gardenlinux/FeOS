// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { Ago, ArmButton, CopyBtn, ErrBox, PageHead, StateDot, useArm, usePoll } from '../components.jsx';
import { fmtDateTime, shortId } from '../format.js';
import { atLeast, envRole, useAccess } from '../people.js';
import { envPath, sandboxPath } from '../routes.js';
import { useSummary } from '../summary.jsx';
import {
  KINDS, REDACTED, behind, bindingOf, credentialPath, curlOf, emptyForm, formOf, hostOf, isName, keptOf, kindLabel,
  modeWords, needsDeployment, newCredentialPath, oldestPin, pinState, pinWords, pinnedSpec, preflightOf, preflightRows, prefixesOf, specSnippet,
  usedWords,
} from './credentials.js';
import '../styles/credentials.css';

// The tenant's credentials (ADR 0033): an API the code calls by a name,
// whose credential FeOS adds on the node, outside the pod. The list; a
// credential's page with its check and its uses (the sandboxes that name
// it, the environments and the version each pins, moved to the latest
// from here); the form of a new one or a new version, with a preflight
// and what the code sees (the concept's Fig. 5). The sandbox's side
// (SandboxCredentials, CredentialPicker) is exported for the sandbox
// pages. Reads need read on the tenant, writes write.

const POLL_MS = 10000;

const WRITE_REASON = 'needs write on this tenant';

// WHAT is what a credential is, in one sentence.
const WHAT = 'An API your code calls by name; FeOS adds the credential on the node, outside the pod — the code never sees it.';

// saved carries a save's answer to the credential's page, which says it
// once.
let saved = null;

export function Credentials({ tenant, sel, isNew, edit, navigate }) {
  if (isNew) return <CredentialEditor tenant={tenant} name="" navigate={navigate} />;
  if (sel && edit) return <CredentialEditor key={sel} tenant={tenant} name={sel} navigate={navigate} />;
  if (sel) return <CredentialPage key={sel} tenant={tenant} name={sel} navigate={navigate} />;
  return <CredentialList tenant={tenant} navigate={navigate} />;
}

// go is a link's click that navigates.
const go = (navigate, to) => (e) => { e.preventDefault(); e.stopPropagation(); navigate(to); };

// ------------------------------------------------------------------ list --

function CredentialList({ tenant, navigate }) {
  const { can: c } = useAccess(tenant);
  const [list, setList] = useState(null);
  const [err, setErr] = useState('');
  const load = useCallback(async () => {
    try {
      const r = await api.listCredentials();
      setList(r.credentials || []);
      setErr('');
    } catch (e) {
      setErr(e.message);
    }
  }, []);
  usePoll(load, POLL_MS);
  const creds = list || [];
  const toNew = newCredentialPath(tenant);
  return (
    <div className="creds">
      <PageHead kind="Connect" title="Credentials"
        actions={(
          <a className={'button primary' + (c.writeTenant ? '' : ' disabled')} href={toNew}
            title={c.writeTenant ? 'state an API and its credential once' : WRITE_REASON}
            onClick={(e) => { e.preventDefault(); if (c.writeTenant) navigate(toNew); }}>
            <Icon name="plus" size={14} /> New credential</a>)}
        facts={<span>{WHAT}</span>} />
      <ErrBox error={err} />
      <section className="pane">
        {!list && !err && <p className="empty">Loading…</p>}
        {list && creds.length === 0 && (
          <div className="cred-empty">
            <p>No credential yet. State one for each API your agents and services call — GitHub, a model provider, a
              payment API — and give it to sandboxes and environments by its name.</p>
            <p className="muted small">Rotating it is one write: every sandbox that names it gets the new version at
              once, and each environment with its next deployment.</p>
          </div>)}
        {creds.length > 0 && (
          <div className="tblwrap"><table className="tbl cred-list">
            <thead><tr><th>name</th><th>upstream</th><th>reached as</th><th>kind</th><th>version</th><th>used by</th><th>created</th></tr></thead>
            <tbody>
              {creds.map((cr) => {
                const envs = (cr.usedBy && cr.usedBy.environments) || [];
                const lagging = envs.filter((u) => behind(u, cr.version));
                return (
                  <tr key={cr.name} className="clickable" onClick={() => navigate(credentialPath(tenant, cr.name))}>
                    <td><a className="mono" href={credentialPath(tenant, cr.name)} onClick={go(navigate, credentialPath(tenant, cr.name))}>{cr.name}</a></td>
                    <td className="mono cred-up">{cr.upstream}</td>
                    <td className="mono small">{modeWords(cr)}</td>
                    <td className="small">{kindLabel(cr.credential && cr.credential.kind)}</td>
                    <td className="mono">v{cr.version}</td>
                    <td className="small">
                      {usedWords(cr.usedBy)}
                      {lagging.map((u) => (
                        <div key={u.id} className="warntext" title={pinWords(u)}>
                          <span className="mono">{u.name}</span> on v{oldestPin(u)}</div>))}
                    </td>
                    <td className="small" title={fmtDateTime(cr.createdAt)}><Ago iso={cr.createdAt} /></td>
                  </tr>
                );
              })}
            </tbody>
          </table></div>)}
      </section>
    </div>
  );
}

// ------------------------------------------------------------------ page --

function CredentialPage({ tenant, name, navigate }) {
  const access = useAccess(tenant);
  const c = access.can;
  const sum = useSummary();
  const [cred, setCred] = useState(null);
  const [err, setErr] = useState('');
  const [missing, setMissing] = useState(false);
  const [note] = useState(() => {
    const n = saved && saved.name === name ? saved : null;
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
      setCred(await api.getCredential(name));
      setErr('');
    } catch (e) {
      if (e.status === 404) setMissing(true);
      setErr(e.message);
    }
  }, [name]);
  usePoll(load, POLL_MS);

  const reload = async () => {
    await load();
    sum.refresh();
  };

  if (missing && !cred) {
    return (
      <div className="creds">
        <PageHead kind="Credential" title={name} mono />
        <p className="empty">No credential <span className="mono">{name}</span> in this tenant. <a href={credentialPath(tenant)}
          onClick={go(navigate, credentialPath(tenant))}>All credentials</a></p>
      </div>
    );
  }
  if (!cred) return err ? <ErrBox error={err} /> : <p className="empty">Loading…</p>;

  const runCheck = async () => {
    setChecking(true);
    try {
      const path = probe.path.trim();
      const p = path ? { method: probe.method, path } : null;
      setCheck({ result: await api.checkCredential(name, p || undefined), probe: p });
    } catch (e) {
      setCheck({ result: { ok: false, addresses: [], error: { code: (e.body && e.body.code) || 'error', message: e.message } } });
    } finally {
      setChecking(false);
    }
  };
  const cr = cred.credential || {};
  const why = c.writeTenant ? undefined : WRITE_REASON;

  return (
    <div className="creds">
      <PageHead kind="Credential" title={name} mono
        state={<span className="badge acc">version {cred.version}</span>}
        actions={(
          <>
            <button disabled={!c.writeTenant || checking} title={why || 'the preflight with the stored credential'} onClick={runCheck}>
              <Icon name="check" size={14} /> {checking ? 'Checking…' : 'Check'}</button>
            <a className={'button primary' + (c.writeTenant ? '' : ' disabled')} href={credentialPath(tenant, name, true)}
              title={why || 'write a new version'}
              onClick={(e) => { e.preventDefault(); if (c.writeTenant) navigate(credentialPath(tenant, name, true)); }}>
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

      <UsedBy tenant={tenant} cred={cred} access={access} navigate={navigate} onChanged={reload} />

      <section className="pane cred-danger">
        <div className="pane-toolbar"><h2><Icon name="trash" size={14} /> Delete</h2></div>
        <p className="muted small">Deletes every version. Refused while a sandbox names it or an environment's latest or
          deployed deployment pins it.</p>
        <ErrBox error={delErr} />
        <ArmButton armKey="delete" armed={armed} arm={arm} disarm={disarm} disabled={!c.writeTenant} title={why}
          icon="trash" confirmLabel={`delete ${name}?`}
          onConfirm={async () => {
            setDelErr('');
            try {
              await api.deleteCredential(name);
              sum.refresh();
              navigate(credentialPath(tenant));
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

function UsedBy({ tenant, cred, access, navigate, onChanged }) {
  const c = access.can;
  const sum = useSummary();
  const name = cred.name;
  const uses = cred.usedBy || { sandboxes: [], environments: [] };
  const sbs = uses.sandboxes || [];
  const envs = uses.environments || [];
  const sandboxes = sum.sandboxes || [];
  const sbOf = (id) => sandboxes.find((s) => s.sandboxId === id);
  const [pick, setPick] = useState('');
  const [busy, setBusy] = useState('');
  const [err, setErr] = useState('');
  const [results, setResults] = useState({});
  const [envPick, setEnvPick] = useState('');
  const why = c.writeTenant ? undefined : WRITE_REASON;

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
    } catch (e) {
      setErr(e.message);
    } finally {
      setBusy('');
    }
  };

  const updateEnv = async (u) => {
    setBusy('env:' + u.id);
    setResults((r) => ({ ...r, [u.id]: null }));
    try {
      const res = await moveToLatest(u.name, name, cred.version);
      setResults((r) => ({ ...r, [u.id]: res }));
      await onChanged();
    } catch (e) {
      setResults((r) => ({ ...r, [u.id]: { error: e.message, findings: (e.body && e.body.findings) || [] } }));
    } finally {
      setBusy('');
    }
  };

  const free = sandboxes.filter((s) => !sbs.includes(s.sandboxId));
  const otherEnvs = (sum.environments || []).filter((e) => !envs.some((u) => u.id === e.id));

  return (
    <section className="pane">
      <div className="pane-toolbar"><h2><Icon name="link" size={14} /> Used by <span className="count">{usedWords(uses)}</span></h2></div>
      <ErrBox error={err} />

      <h3 className="sect"><Icon name="box" size={13} /> Sandboxes</h3>
      <p className="muted small">A sandbox gets the latest version, at once and without a restart: a new version
        reaches its running pod with the save.</p>
      {sbs.length === 0 && <p className="empty">No sandbox names it.</p>}
      {sbs.length > 0 && (
        <div className="tblwrap"><table className="tbl">
          <thead><tr><th>sandbox</th><th>owner</th><th>state</th><th></th></tr></thead>
          <tbody>
            {sbs.map((id) => {
              const sb = sbOf(id);
              // A sandbox the summary does not know yet is found by its
              // ID (the moved view leads to its environment).
              const to = sb && sb.environment
                ? sandboxPath(tenant, sb.environment.name || sb.environment.id, id, 'credentials')
                : `/tenants/${encodeURIComponent(tenant)}/sandboxes/${encodeURIComponent(id)}/credentials`;
              return (
                <tr key={id}>
                  <td><a className="mono" href={to} onClick={go(navigate, to)}>{id}</a></td>
                  <td className="small">{(sb && sb.owner) || <span className="muted">—</span>}</td>
                  <td className="small">{sb ? <><StateDot state={sb.clientState} /> {sb.clientState}</> : <span className="muted">—</span>}</td>
                  <td className="col-actions">
                    <button className="ghost small" disabled={!c.writeTenant || !!busy} title={why || 'take it from this sandbox'}
                      onClick={() => setSandbox(id, false)}>{busy === 'sb:' + id ? 'Removing…' : 'Remove'}</button>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table></div>)}
      <div className="createpanel cred-use">
        <select value={pick} onChange={(e) => setPick(e.target.value)} disabled={!c.writeTenant || !free.length} title={why}>
          <option value="">{free.length ? 'Use in a sandbox…' : 'every sandbox names it'}</option>
          {free.map((s) => <option key={s.sandboxId} value={s.sandboxId}>{shortId(s.sandboxId)} · {s.owner || 'no owner'} · {s.clientState}</option>)}
        </select>
        <button disabled={!c.writeTenant || !pick || !!busy} title={why} onClick={() => setSandbox(pick, true)}>
          <Icon name="plus" size={14} /> {busy === 'sb:' + pick ? 'Adding…' : 'Add'}</button>
      </div>

      <h3 className="sect"><Icon name="layers" size={13} /> Environments</h3>
      <p className="muted small">An environment's deployment pins the version it got; a new version reaches it with its
        next deployment.</p>
      {envs.length === 0 && <p className="empty">No environment's deployment names it.</p>}
      {envs.length > 0 && (
        <div className="tblwrap"><table className="tbl cred-envs">
          <thead><tr><th>environment</th><th>bindings</th><th>pins</th><th></th></tr></thead>
          <tbody>
            {envs.map((u) => {
              const state = pinState(u, cred.version);
              const lag = needsDeployment(u, cred.version);
              const mayWrite = atLeast(envRole(access, u.id) || envRole(access, u.name), 'write');
              const res = results[u.id];
              return (
                <React.Fragment key={u.id}>
                  <tr>
                    <td><a className="mono" href={envPath(tenant, u.name)} onClick={go(navigate, envPath(tenant, u.name))}>{u.name}</a></td>
                    <td className="mono small">{(u.bindings || []).join(', ')}</td>
                    <td className="small">
                      <span className={state === 'current' ? '' : 'warntext'}>{pinWords(u)}</span>
                      <span className="muted">
                        {state === 'behind' && ` · behind v${cred.version}`}
                        {state === 'rolling out' && ` · v${cred.version} rolling out`}
                        {state === 'current' && ' · the latest'}
                      </span>
                    </td>
                    <td className="col-actions">
                      {lag && (
                        <button className="primary small" disabled={!mayWrite || !!busy}
                          title={mayWrite ? `a new deployment with ${name} at version ${cred.version}` : 'needs write on this environment'}
                          onClick={() => updateEnv(u)}>
                          {busy === 'env:' + u.id ? 'Deploying…' : `Update to v${cred.version}`}</button>)}
                    </td>
                  </tr>
                  {res && (
                    <tr className="subrow"><td colSpan={4}><EnvResult tenant={tenant} env={u.name} res={res} navigate={navigate} /></td></tr>)}
                </React.Fragment>
              );
            })}
          </tbody>
        </table></div>)}

      <details className="cred-fold">
        <summary>Use in an environment</summary>
        <p className="small">An environment's spec names the credential in a binding, and each service (or job) that
          calls it names the binding. Its next deployment pins the latest version:</p>
        <div className="snippet"><pre className="cred-snip">{specSnippet(name)}</pre><CopyBtn text={specSnippet(name)} title="copy" /></div>
        <p className="muted small">The binding takes nothing else: the upstream, paths, rate and credential are the
          credential's.</p>
        <p className="muted small">The code then calls <span className="mono">{curlOf(cred).replace(/^curl -s /, '')}</span>.
          Write it in the environment's builder, as a new deployment.</p>
        <div className="createpanel">
          <select value={envPick} onChange={(e) => setEnvPick(e.target.value)} disabled={!otherEnvs.length}>
            <option value="">{otherEnvs.length ? 'environment…' : 'no other environment'}</option>
            {otherEnvs.map((e) => <option key={e.id} value={e.name}>{e.name}</option>)}
          </select>
          <a className={'button' + (envPick ? '' : ' disabled')} href={envPick ? envPath(tenant, envPick, 'builder') : undefined}
            onClick={(e) => { e.preventDefault(); if (envPick) navigate(envPath(tenant, envPick, 'builder')); }}>
            <Icon name="arrowRight" size={14} /> New deployment</a>
        </div>
      </details>
    </section>
  );
}

// moveToLatest makes a deployment of env from its latest one with every
// binding that names credential name at version: {deployment} or, when
// the plan finds it invalid, {findings}.
async function moveToLatest(env, name, version) {
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

function EnvResult({ tenant, env, res, navigate }) {
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

function CredentialEditor({ tenant, name, navigate }) {
  const [cred, setCred] = useState(null);
  const [err, setErr] = useState('');
  useEffect(() => {
    if (!name) return undefined;
    let live = true;
    api.getCredential(name).then((c) => { if (live) setCred(c); }, (e) => { if (live) setErr(e.message); });
    return () => { live = false; };
  }, [name]);
  if (name && !cred) return err ? <ErrBox error={err} /> : <p className="empty">Loading…</p>;
  return <CredentialForm tenant={tenant} cred={cred} navigate={navigate} />;
}

// CredentialForm writes a new credential (cred null) or a new version of
// cred, whose secret fields an empty field keeps.
export function CredentialForm({ tenant, cred, navigate }) {
  const { can: c } = useAccess(tenant);
  const sum = useSummary();
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
  const back = editing ? credentialPath(tenant, cred.name) : credentialPath(tenant);
  const why = c.writeTenant ? undefined : WRITE_REASON;

  const runPreflight = async () => {
    setBad('');
    setPreBusy(true);
    try {
      const how = preflightOf(f, cred);
      const path = probe.path.trim();
      const p = path ? { method: probe.method, path } : undefined;
      if (how.how === 'needs') {
        setPre({ needs: how.fields });
      } else if (how.how === 'stored') {
        setPre({ result: await api.checkCredential(cred.name, p), probe: p, stored: true });
      } else {
        setPre({ result: await api.httpPreflight({ name: name || 'preflight', ...how.binding }, p), probe: p });
      }
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
    let body;
    try {
      body = bindingOf(f);
    } catch (e2) {
      setBad(e2.message);
      return;
    }
    setSaving(true);
    try {
      const out = await api.putCredential(name, body);
      saved = { name, version: out.version, created: !editing || out.version !== cred.version };
      sum.refresh();
      navigate(credentialPath(tenant, name));
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
      <PageHead kind={editing ? 'Edit credential' : 'New credential'} title={editing ? cred.name : name || 'New credential'}
        mono={editing || !!name}
        state={editing ? <span className="badge">version {cred.version} → {cred.version + 1}</span> : null}
        actions={(
          <>
            <a className="button ghost" href={back} onClick={go(navigate, back)}>Cancel</a>
            <button type="submit" className="primary" disabled={!c.writeTenant || saving} title={why}>
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
              <span className="cred-hint">A DNS label. Sandboxes and environments name it; in the name mode the code calls{' '}
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
            <button type="button" disabled={!c.writeTenant || preBusy} title={why} onClick={runPreflight}>
              <Icon name="refresh" size={14} /> {preBusy ? 'Running…' : pre ? 'Run again' : 'Preflight'}</button>
          </div>
          {pre && pre.needs && (
            <p className="warntext small">The preflight needs the values of <span className="mono">{pre.needs.join(', ')}</span>{' '}
              with this change: the stored ones never leave ferund. Type them, or save and use Check.</p>)}
          {pre && pre.result && <PreflightRows result={pre.result} probe={pre.probe} />}
          {pre && pre.stored && <p className="muted small">Checked with the stored version {cred.version}: nothing else changed.</p>}

          <h3 className="sect">What the code sees</h3>
          <CodeSees c={view} />
        </section>
      </div>
    </form>
  );
}

// ------------------------------------------------------------- sandboxes --

// SandboxCredentials are the credentials a sandbox names, with their
// upstream and version, and the way to add and remove one (write on the
// tenant): the whole list is written, and the running pod gets it at
// once.
export function SandboxCredentials({ tenant, sandboxId, write, navigate }) {
  const [names, setNames] = useState(null);
  const [all, setAll] = useState(null);
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState('');
  const [pick, setPick] = useState('');
  const [done, setDone] = useState('');
  const load = useCallback(async () => {
    try {
      const [mine, list] = await Promise.all([api.getSandboxCredentials(sandboxId), api.listCredentials()]);
      setNames(mine.credentials || []);
      setAll(list.credentials || []);
      setErr('');
    } catch (e) {
      setErr(e.message);
    }
  }, [sandboxId]);
  usePoll(load, POLL_MS);
  const why = write ? undefined : WRITE_REASON;

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
  return (
    <div className="cred-sbx">
      <p className="muted small">The tenant's credentials this sandbox's code calls by name; FeOS adds each on the node,
        outside the pod. A change reaches the running pod at once, without a restart.</p>
      <ErrBox error={err} />
      {done && <p className="ok small"><Icon name="check" size={12} /> {done}</p>}
      {names.length === 0 && <p className="empty">It names no credential.</p>}
      {names.length > 0 && (
        <div className="tblwrap"><table className="tbl">
          <thead><tr><th>credential</th><th>upstream</th><th>the code calls</th><th>version</th><th></th></tr></thead>
          <tbody>
            {names.map((n) => {
              const cr = of(n);
              const to = credentialPath(tenant, n);
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
          <option value="">{free.length ? 'add a credential…' : (all || []).length ? 'it names every credential' : 'the tenant has no credential yet'}</option>
          {free.map((c) => <option key={c.name} value={c.name}>{c.name} · {c.upstream}</option>)}
        </select>
        <button type="submit" disabled={!write || !pick || !!busy} title={why}><Icon name="plus" size={14} /> {busy === 'add' ? 'Adding…' : 'Add'}</button>
        <a className="small" href={credentialPath(tenant)} onClick={go(navigate, credentialPath(tenant))}>all credentials →</a>
      </form>
      <p className="muted small">The sandbox's own HTTP set, which its token writes (<code className="mono">PUT /api/v1/sandboxes/&lt;id&gt;/http</code>),
        doesn't show here.</p>
    </div>
  );
}

// CredentialPicker picks credentials for a new sandbox, from the
// summary's list.
export function CredentialPicker({ value, onChange, disabled }) {
  const { credentials } = useSummary();
  const list = credentials || [];
  if (!credentials) return null;
  const toggle = (n) => onChange(value.includes(n) ? value.filter((x) => x !== n) : [...value, n]);
  return (
    <fieldset className="sbx-fieldset">
      <legend>credentials <span className="muted">· optional</span></legend>
      {list.length === 0 && <span className="sbx-hint">The tenant has no credential yet.</span>}
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
