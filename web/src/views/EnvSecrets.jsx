// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { Fragment, useCallback, useEffect, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { Ago, ErrBox, usePoll } from '../components.jsx';
import { fmtDateTime, shortId } from '../format.js';
import { useEnvStatus } from '../envstatus.jsx';
import { queryOf } from '../logs.js';
import { credentialPath, envPath, newCredentialPath, objectPath, sandboxPath, secretsPath } from '../routes.js';
import {
  credentialPinNote, fieldsWords, inventoryOf, kindShort, problemWords, secretPinNote, strayUsage, usageOf, usageTone, usageWords,
  usersWords,
} from './access.js';
import { useAddonCatalog, useBindingUsage, useEnvCredentials } from './accessData.js';
import { hostOf, kindLabel, modeWords, needsDeployment } from './credentials.js';
import { DeployResult, moveToLatest } from './Credentials.jsx';
import { DeleteSecret, EnvLink, NEEDS, NewSecret, OpNote, SecretValue, useOp } from './EnvTabs.jsx';
import { PreflightResult, useLatestSpec } from './Environments.jsx';
import { KeyMark, MarkLegend } from './Marks.jsx';
import '../styles/marks.css';
import '../styles/access.css';
import '../styles/credentials.css';

// An environment's Secrets view (…/secrets[/:kind/:name]): every key its
// workloads reach, sorted by where it lives (access.js), not by where it
// is stored. A legend of the three marks, then the inventory in groups:
// the secrets (each with its uses, its version against what the latest
// deployment pins, and missing when the spec names one without a value),
// the credentials (ADR 0033: what they reach, who uses them, sandboxes
// too, the version the deployments pin, "Update to vN"), the HTTP
// bindings (ADR 0028: where each key comes from, its last 24 h, Check,
// its form in New deployment) and the addons that admit by identity or
// hand out a password. An address with kind/name points at a row. Values
// are never shown; a writer sets, adds, deletes and checks, a reader
// sees the rest (ec, envCan).

const Link = EnvLink;

const SECRETS_MS = 10000;

// KIND_OF names an object's kind by its spec key.
const KIND_OF = { services: 'service', jobs: 'job' };

// rowId is the element ID of a row of the inventory.
const rowId = (kind, name) => `inv-${kind}-${name}`;

// EnvSecrets reads the view's data: the latest deployment's spec, the
// secrets, the credentials, the catalog and the bindings' use; and
// writes a secret's value or its delete.
export function EnvSecrets({ tenant, env, st, ec, item = '', changed = () => {}, navigate }) {
  const spec = useLatestSpec(st);
  const catalog = useAddonCatalog();
  const creds = useEnvCredentials(env);
  const usage = useBindingUsage(env);
  const { sandboxes } = useEnvStatus();
  const [secrets, setSecrets] = useState(null);
  const [err, setErr] = useState('');
  const [editing, setEditing] = useState('');
  const [op, run, clear] = useOp();
  const readSecrets = useCallback(async () => {
    try {
      setSecrets((await api.listSecrets(env)).secrets || []);
      setErr('');
    } catch (e) {
      setErr(e.message);
    }
  }, [env]);
  usePoll(readSecrets, SECRETS_MS);
  const put = async (name, value) => {
    const r = await run(() => api.putSecret(env, name, value),
      (res) => `${res.name}: version ${res.version} stored. Deployment ${st ? st.deployment : '—'} keeps the version it pins; the next deployment that pins ${res.name} takes it.`);
    if (r) { setEditing(''); readSecrets(); changed(); }
    return !!r;
  };
  const del = async (name) => {
    const r = await run(() => api.deleteSecret(env, name).then(() => true), `${name} deleted.`);
    if (r) { readSecrets(); changed(); }
  };
  // Until both lists answered once: without the secrets, every one the
  // spec names would read as missing.
  const loading = !st || (st.deployment > 0 && spec === null) || secrets === null || creds.data === null;

  // The row the address points at comes into view once it is there.
  useEffect(() => {
    if (loading || !item) return;
    const i = item.indexOf('/');
    const el = document.getElementById(rowId(item.slice(0, i), item.slice(i + 1)));
    if (el) el.scrollIntoView({ block: 'center' });
  }, [item, loading]);

  return (
    <>
      <ErrBox error={err || creds.err} />
      <OpNote op={op} onDismiss={clear} />
      {loading && !err && !creds.err && <p className="empty">Loading…</p>}
      {!loading && (
        <SecretsInventory tenant={tenant} env={env} deployment={st.deployment} spec={spec} secrets={secrets}
          credentials={creds.data} catalog={catalog} usage={usage} sandboxes={sandboxes} ec={ec} focus={item}
          editing={editing} setEditing={setEditing} onPut={put} onDelete={del} busy={!!(op && op.busy)}
          onChanged={() => { creds.reload(); changed(); }} navigate={navigate} />)}
    </>
  );
}

// SecretsInventory is the view, its data given: the spec of the latest
// deployment (null without one), the secrets and credentials as listed,
// the catalog (null while it loads), the bindings' use (null without).
export function SecretsInventory({
  tenant, env, deployment = 0, spec, secrets, credentials, catalog, usage, sandboxes = null, ec, focus = '', editing = '',
  setEditing = () => {}, onPut, onDelete, busy = false, onChanged = () => {}, navigate,
}) {
  const rows = inventoryOf(spec, { catalog, credentials: credentials || [], secrets: secrets || [] });
  const of = (kind) => rows.filter((r) => r.kind === kind);
  const stray = strayUsage(usage, spec);
  const missing = of('secret').filter((r) => r.state === 'missing').length;
  const troubled = of('binding').filter((r) => usageTone(usageOf(usage, r.name)) === 'bad').length;
  const ctx = { tenant, env, deployment, spec, secrets: secrets || [], credentials: credentials || [], usage, sandboxes, ec, focus, navigate };
  return (
    <div className="inv">
      <p className="panel-note inv-note">
        Every key this environment&apos;s services, jobs and sandboxes reach, by where it lives: no key at all, at the
        node, or in the container. Values are never shown, here or anywhere.
      </p>
      <MarkLegend />
      <div className="facts inv-facts">
        <span><b>{of('secret').length}</b> secret{of('secret').length === 1 ? '' : 's'}</span>
        {missing > 0 && <span className="bad" title="named by the spec, without a value">▲ <b className="bad">{missing}</b> missing</span>}
        <span><b>{of('credential').length}</b> credential{of('credential').length === 1 ? '' : 's'}</span>
        <span><b>{of('binding').length}</b> binding{of('binding').length === 1 ? '' : 's'}{deployment ? ` of deployment ${deployment}` : ''}</span>
        {troubled > 0 && <span className="bad" title="refused, failing or without a token in the last 24 h">▲ <b className="bad">{troubled}</b> with problems</span>}
      </div>
      {usage && usage.unavailable && <p className="muted small inv-unavailable">No numbers of the last 24 h: {usage.unavailable}</p>}
      <SecretsGroup rows={of('secret')} ctx={ctx} editing={editing} setEditing={setEditing} onPut={onPut} onDelete={onDelete} busy={busy} />
      <CredentialsGroup rows={of('credential')} ctx={ctx} onChanged={onChanged} />
      <BindingsGroup rows={of('binding')} stray={stray} ctx={ctx} />
      <AddonsGroup rows={of('addon')} loading={!catalog} ctx={ctx} />
    </div>
  );
}

// Group is a group of the inventory: its title and count, an action, a
// line of what it holds, its rows.
function Group({ id, title, count, action = null, note, children }) {
  return (
    <section className="envsect inv-group" id={id}>
      <div className="sect sect-row">
        {title} <span className="count">· {count}</span>
        <span className="spacer" />
        {action}
      </div>
      {note && <p className="panel-note envsecrets-note">{note}</p>}
      {children}
    </section>
  );
}

// Marks are a row's marks; none for a key no one uses.
function Marks({ marks }) {
  if (!marks || marks.length === 0) return <span className="muted">—</span>;
  return <span className="inv-marks">{marks.map((m) => <KeyMark key={m} mark={m} />)}</span>;
}

// ObjLink is an object of the spec ({key, name}), a link to its page.
function ObjLink({ from, ctx }) {
  const kind = KIND_OF[from.key];
  if (!kind) return <span className="mono">{from.name}</span>;
  return (
    <span className="inv-obj">
      <span className="muted">{kind}</span>{' '}
      <Link to={objectPath(ctx.tenant, ctx.env, `${kind}/${from.name}`)} navigate={ctx.navigate} className="mono">{from.name}</Link>
    </span>
  );
}

// SandboxLink is a sandbox of the environment, by its ID.
function SandboxLink({ id, ctx }) {
  const sb = (ctx.sandboxes || []).find((s) => s.sandboxId === id);
  return (
    <span className="inv-obj">
      <span className="muted">sandbox</span>{' '}
      <Link to={sandboxPath(ctx.tenant, ctx.env, id)} navigate={ctx.navigate} className="mono"
        title={sb && sb.owner ? `${id} · ${sb.owner}` : id}>{shortId(id)}</Link>
    </span>
  );
}

// rowClass marks the row the address points at.
function rowClass(ctx, kind, name, extra = '') {
  return [ctx.focus === `${kind}/${name}` ? 'sel' : '', extra].filter(Boolean).join(' ') || undefined;
}

// --------------------------------------------------------------- secrets --

// SecretsGroup are the secrets: the stored ones and those the spec names.
function SecretsGroup({ rows, ctx, editing, setEditing, onPut, onDelete, busy }) {
  const { ec } = ctx;
  const stored = (name) => ctx.secrets.find((s) => s.name === name) || null;
  return (
    <Group id="inv-secrets" title="Secrets" count={rows.length}
      note={<>A value a deployment hands to a container (a variable, in the container), a binding adds at the node or the
        node pulls an image with (offloaded). Each deployment pins the version it names; a new value is a new version,
        which the pods get with the next deployment.</>}>
      {rows.length > 0 && (
        <div className="tblwrap">
          <table className="tbl inv-tbl">
            <thead><tr><th>name</th><th>where</th><th>used by</th><th>state</th><th className="col-quick" /></tr></thead>
            <tbody>
              {rows.map((r) => {
                const s = stored(r.name);
                return (
                  <Fragment key={r.name}>
                    <tr id={rowId('secret', r.name)} className={rowClass(ctx, 'secret', r.name, r.state === 'missing' ? 'inv-missing' : '')}>
                      <td className="mono strong">{r.name}</td>
                      <td><Marks marks={r.marks} /></td>
                      <td className="small"><SecretUses uses={r.uses} ctx={ctx} /></td>
                      <td className="small"><SecretState r={r} s={s} /></td>
                      <td className="col-quick nowrap">
                        <button className={'mini' + (r.state === 'missing' ? ' primary' : '')} disabled={!ec.write || busy}
                          title={ec.write ? (r.state === 'missing' ? 'its first version' : 'a new version') : NEEDS.write}
                          onClick={() => setEditing(editing === r.name ? '' : r.name)}><Icon name="key" size={12} /> Set value</button>
                        {s && <>{' '}<DeleteSecret name={r.name} disabled={!ec.write || busy} onDelete={onDelete} /></>}
                      </td>
                    </tr>
                    {editing === r.name && (
                      <tr className="secret-edit"><td colSpan={5}>
                        <SecretValue name={r.name} onPut={onPut} onCancel={() => setEditing('')} />
                      </td></tr>)}
                  </Fragment>
                );
              })}
            </tbody>
          </table>
        </div>)}
      {rows.length === 0 && <p className="empty">No secrets.</p>}
      <NewSecret disabled={!ec.write} onPut={onPut} />
    </Group>
  );
}

// SecretUses say who uses a secret and how: a variable, a binding's
// credential, the registry login.
function SecretUses({ uses, ctx }) {
  if (!uses || uses.length === 0) return <span className="muted">no one</span>;
  return (
    <span className="inv-uses">
      {uses.map((u) => {
        const how = u.how === 'variable' ? <span className="mono">{u.variable}</span>
          : u.how === 'image' ? 'image pull'
            : <>binding <Link to={secretsPath(ctx.tenant, ctx.env, 'binding', u.binding)} navigate={ctx.navigate} className="mono">{u.binding}</Link></>;
        const k = `${u.from.key}/${u.from.name}/${u.how}/${u.variable || u.binding || ''}`;
        if (u.from.key === 'bindings') return <span key={k}>{how} <span className="muted">(no one names it)</span></span>;
        return <span key={k}><ObjLink from={u.from} ctx={ctx} /> <span className="muted">·</span> {how}</span>;
      })}
    </span>
  );
}

// SecretState is a secret's version, when and by whom it was set, and
// what stands out: missing (the spec names it, it has no value), an
// older version pinned, no user.
function SecretState({ r, s }) {
  if (r.state === 'missing') {
    return (
      <span className="inv-state">
        <span className="badge bad">missing</span> <span className="bad">named by the spec, no value</span>
      </span>);
  }
  const pin = secretPinNote(r.pinned, r.version);
  return (
    <span className="inv-state" title={s && s.createdBy ? `version ${s.version} set by ${s.createdBy}, ${fmtDateTime(s.createdAt)}` : undefined}>
      <span className="mono">v{r.version}</span>
      {s && s.createdAt && <> <Ago iso={s.createdAt} /></>}
      {r.state === 'unused' && <span className="muted"> · unused</span>}
      {pin && <div className="warntext">{pin}</div>}
    </span>
  );
}

// ----------------------------------------------------------- credentials --

// CredentialsGroup are the environment's credentials (ADR 0033); a
// credential's page has its check, its new version and its delete.
function CredentialsGroup({ rows, ctx, onChanged }) {
  const { ec, tenant, env, navigate } = ctx;
  const toNew = newCredentialPath(tenant, env);
  return (
    <Group id="inv-credentials" title="Credentials" count={rows.length}
      action={(
        <button className="mini" disabled={!ec.write} title={ec.write ? 'state an API and its credential once' : NEEDS.write}
          onClick={() => navigate(toNew)}><Icon name="plus" size={12} /> New credential</button>)}
      note={<>An API this environment&apos;s code calls by name; FeOS adds the credential on the node, outside the pod.
        A credential is this environment&apos;s: its sandboxes name it and get the latest version at once; a binding of
        its deployments names it, and each deployment pins the version it got.</>}>
      {rows.length > 0 && (
        <div className="tblwrap">
          <table className="tbl inv-tbl">
            <thead><tr><th>name</th><th>where</th><th>reaches</th><th>used by</th><th>state</th><th className="col-quick" /></tr></thead>
            <tbody>
              {rows.map((r) => <CredentialRow key={r.name} r={r} ctx={ctx} onChanged={onChanged} />)}
            </tbody>
          </table>
        </div>)}
      {rows.length === 0 && (
        <div className="cred-empty">
          <p>No credential in this environment yet. State one for each API this environment&apos;s services and
            sandboxes call — GitHub, a model provider, a payment API — and name it in a binding or a sandbox.</p>
          <p className="muted small">Rotating it is one write: every sandbox here that names it gets the new version at
            once, the services with the next deployment.</p>
        </div>)}
    </Group>
  );
}

// CredentialRow is a credential: what it reaches, who uses it, its
// version against what the deployments pin, and the deployment that
// moves them to the latest.
function CredentialRow({ r, ctx, onChanged }) {
  const { tenant, env, ec, navigate } = ctx;
  const c = ctx.credentials.find((x) => x.name === r.name) || {};
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState(null);
  const pin = credentialPinNote(r.pins, r.version);
  const update = async () => {
    setBusy(true);
    setResult(null);
    try {
      setResult(await moveToLatest(env, r.name, r.version));
      onChanged();
    } catch (e) {
      setResult({ error: e.message, findings: (e.body && e.body.findings) || [] });
    } finally {
      setBusy(false);
    }
  };
  const to = credentialPath(tenant, env, r.name);
  const kind = (c.credential && c.credential.kind) || c.kind || '';
  return (
    <>
      <tr id={rowId('credential', r.name)} className={rowClass(ctx, 'credential', r.name)}>
        <td><Link to={to} navigate={navigate} className="mono strong" title="its page: check, new version, uses, delete">{r.name}</Link></td>
        <td><Marks marks={r.marks} /></td>
        <td className="small">
          <span className="mono cred-up">{hostOf(c.upstream) || c.upstream || '—'}</span>
          <div className="muted">{modeWords(c)}{kind ? ` · ${kindLabel(kind)}` : ''}</div>
        </td>
        <td className="small">
          <span className="inv-uses">
            {r.users.map((u) => <ObjLink key={`${u.key}/${u.name}`} from={u} ctx={ctx} />)}
            {r.sandboxes.map((id) => <SandboxLink key={id} id={id} ctx={ctx} />)}
            {r.users.length === 0 && r.sandboxes.length === 0 && <span className="muted">no one</span>}
          </span>
          {r.bindings.length > 0 && (
            <div className="muted">through binding{r.bindings.length === 1 ? '' : 's'}{' '}
              {r.bindings.map((b, i) => (
                <Fragment key={b}>{i > 0 && ', '}
                  <Link to={secretsPath(tenant, env, 'binding', b)} navigate={navigate} className="mono">{b}</Link></Fragment>))}
            </div>)}
        </td>
        <td className="small inv-state">
          <span className="mono">v{r.version}</span>
          {c.createdAt && <> <Ago iso={c.createdAt} /></>}
          {r.state === 'unused' && <span className="muted"> · unused</span>}
          {pin && <div className="warntext">{pin}</div>}
        </td>
        <td className="col-quick nowrap">
          {needsDeployment(r.pins, r.version) && (
            <button className="primary mini" disabled={!ec.write || busy}
              title={ec.write ? `a new deployment with ${r.name} at version ${r.version}` : NEEDS.write} onClick={update}>
              {busy ? 'Deploying…' : `Update to v${r.version}`}</button>)}
        </td>
      </tr>
      {result && (
        <tr className="subrow"><td colSpan={6}><DeployResult tenant={tenant} env={env} res={result} navigate={navigate} /></td></tr>)}
    </>
  );
}

// -------------------------------------------------------------- bindings --

// BindingsGroup are the latest deployment's HTTP bindings (ADR 0028),
// then the bindings its sandboxes named themselves, as their requests
// show them. A binding is part of a deployment: added and changed in New
// deployment.
function BindingsGroup({ rows, stray, ctx }) {
  const { ec, tenant, env, deployment, navigate } = ctx;
  const add = envPath(tenant, env, 'builder', 'new/binding');
  return (
    <Group id="inv-bindings" title="HTTP bindings" count={rows.length}
      action={ec.write && <button className="mini" onClick={() => navigate(add)}><Icon name="plus" size={12} /> Add binding</button>}
      note={<>A binding sends a service&apos;s calls to an upstream through the platform, which adds the credential to
        each request: the key never enters the pod. Its credential is one of the credentials above or made of this
        environment&apos;s secrets. A binding is part of the deployment: adding or changing one makes the next
        deployment.</>}>
      {(rows.length > 0 || stray.length > 0) && (
        <div className="tblwrap">
          <table className="tbl inv-tbl">
            <thead><tr><th>name</th><th>where</th><th>reaches</th><th>used by</th><th>last 24 h</th><th className="col-quick" /></tr></thead>
            <tbody>
              {rows.map((r) => <BindingRow key={r.name} r={r} ctx={ctx} />)}
              {stray.map((u) => <StrayBindingRow key={u.name} u={u} ctx={ctx} />)}
            </tbody>
          </table>
        </div>)}
      {rows.length === 0 && stray.length === 0 && (
        <p className="empty">No bindings{deployment ? ` in deployment ${deployment}` : ''}: no service&apos;s calls carry a credential of the platform.</p>)}
    </Group>
  );
}

// logsOf are the environment's outbound requests of the last day.
const logsOf = (ctx) => envPath(ctx.tenant, ctx.env, 'logs', '', null, queryOf({ source: 'egress', since: '1d' }));

// BindingRow is a binding of the latest deployment: what it reaches and
// as what, where its key comes from, who names it, its last 24 h, Check
// (the preflight with the secrets' latest versions) and its form.
function BindingRow({ r, ctx }) {
  const { tenant, env, ec, navigate } = ctx;
  const b = r.binding;
  const [op, run] = useOp();
  const res = (op && op.result) || null;
  const busy = !!(op && op.busy);
  const kind = kindShort(b.kind);
  const latest = b.credential ? ctx.credentials.find((c) => c.name === b.credential.name) : null;
  return (
    <>
      <tr id={rowId('binding', r.name)} className={rowClass(ctx, 'binding', r.name)}>
        <td className="mono strong">{r.name}</td>
        <td><Marks marks={r.marks} /></td>
        <td className="small">
          <span className="mono host">{hostOf(b.upstream) || '—'}</span>{' '}
          <span className="muted">{b.mode === 'intercept' ? 'intercepted' : <>as <span className="mono">{b.reachedAs}</span></>}</span>
          <div>
            {kind && <span>{kind}</span>}
            {b.credential && (
              <>{kind && ' · '}credential{' '}
                <Link to={credentialPath(tenant, env, b.credential.name)} navigate={navigate} className="mono">{b.credential.name}</Link>
                {b.credential.version > 0 && <span className="mono"> v{b.credential.version}</span>}
                {latest && b.credential.version > 0 && latest.version > b.credential.version && (
                  <span className="warntext"> · latest v{latest.version}</span>)}
              </>)}
            {!b.credential && b.secrets.length > 0 && (
              <>{kind && ' · '}secret{b.secrets.length === 1 ? '' : 's'}{' '}
                {b.secrets.map((n, i) => (
                  <Fragment key={n}>{i > 0 && ', '}
                    <Link to={secretsPath(tenant, env, 'secret', n)} navigate={navigate} className="mono">{n}</Link></Fragment>))}
              </>)}
          </div>
        </td>
        <td className="small">
          <span className="inv-uses">
            {r.users.map((u) => <ObjLink key={`${u.key}/${u.name}`} from={u} ctx={ctx} />)}
            {r.users.length === 0 && <span className="muted">no one: unused</span>}
          </span>
        </td>
        <td><BindingNumbers u={usageOf(ctx.usage, r.name)} tenant={tenant} env={env} logs={logsOf(ctx)} navigate={navigate} /></td>
        <td className="col-quick nowrap">
          <button className="mini" disabled={!ec.write || busy} title={ec.write ? 'resolve, dial, check TLS and the credential, from an instance' : NEEDS.write}
            onClick={() => run(() => api.checkBinding(env, r.name), '')}>
            <Icon name="check" size={12} /> {busy ? 'Checking…' : 'Check'}</button>
          {ec.write && (
            <>{' '}<Link to={envPath(tenant, env, 'builder', `binding/${r.name}`)} navigate={navigate} className="small"
              title="its form; a change makes the next deployment">Edit in New deployment</Link></>)}
        </td>
      </tr>
      {op && (op.tone === 'bad' || res) && (
        <tr className="subrow"><td colSpan={6}>
          {op.tone === 'bad' && <div className="envop-note bad">{op.text}</div>}
          {res && <PreflightResult res={res} />}
        </td></tr>)}
    </>
  );
}

// StrayBindingRow is a binding a sandbox named itself (one of the
// environment's credentials), from its requests of the last 24 h.
function StrayBindingRow({ u, ctx }) {
  const { tenant, env, navigate } = ctx;
  const cred = ctx.credentials.find((c) => c.name === u.name);
  const users = Array.isArray(u.users) ? u.users : [];
  return (
    <tr id={rowId('binding', u.name)} className={rowClass(ctx, 'binding', u.name, 'inv-stray')}>
      <td className="mono strong">{u.name}</td>
      <td><Marks marks={['offloaded']} /></td>
      <td className="small">
        <span className="muted">a sandbox&apos;s own</span>
        {cred && <div>credential <Link to={credentialPath(tenant, env, cred.name)} navigate={navigate} className="mono">{cred.name}</Link></div>}
      </td>
      <td className="small">
        <span className="inv-uses">
          {users.map((x) => (x.kind === 'sandbox'
            ? <SandboxLink key={`s/${x.name}`} id={x.name} ctx={ctx} />
            : <ObjLink key={`${x.kind}/${x.name}`} from={{ key: `${x.kind}s`, name: x.name }} ctx={ctx} />))}
        </span>
      </td>
      <td><BindingNumbers u={u} tenant={tenant} env={env} logs={logsOf(ctx)} navigate={navigate} /></td>
      <td className="col-quick" />
    </tr>
  );
}

// BindingNumbers are a binding's last 24 h: its requests, then its
// problems in their tone, the last problem and who sent them on hover,
// the outbound requests in the logs (logs, a path) and the last
// problem's trace.
export function BindingNumbers({ u, tenant, env, logs = '', navigate }) {
  if (!u) return null;
  const [head, ...problems] = usageWords(u).split(' · ');
  const p = u.lastProblem;
  const users = Array.isArray(u.users) ? u.users : [];
  const title = [
    p && `last problem ${fmtDateTime(p.at)}: ${problemWords(p)}`,
    users.length > 0 && `sent by ${usersWords(users)}`,
    u.lastAt && `last request ${fmtDateTime(u.lastAt)}`,
  ].filter(Boolean).join('\n');
  return (
    <span className="small acc-nums" title={title || undefined}>
      <span className="muted">{head}</span>
      {problems.length > 0 && <span className={usageTone(u)}> · {problems.join(' · ')}</span>}
      {logs && Number(u.requests) > 0 && (
        <> · <Link to={logs} navigate={navigate} title="the outbound requests of the last day, in the logs">outbound</Link></>)}
      {p && p.traceId && (
        <> · <Link to={envPath(tenant, env, 'traces', p.traceId)} navigate={navigate} title={problemWords(p)}>last problem</Link></>)}
    </span>
  );
}

// ---------------------------------------------------------------- addons --

// AddonsGroup are the addons a workload reaches by its identity (its
// certificate admits it) or with the password the addon hands out.
function AddonsGroup({ rows, loading, ctx }) {
  return (
    <Group id="inv-addons" title="Addons" count={loading ? '…' : rows.length}
      note={<>An addon that admits a workload by its certificate needs no key at all; one that hands out a password puts it
        in the container.</>}>
      {loading && <p className="empty">Loading…</p>}
      {!loading && rows.length > 0 && (
        <div className="tblwrap">
          <table className="tbl inv-tbl">
            <thead><tr><th>name</th><th>where</th><th>type</th><th>used by</th></tr></thead>
            <tbody>
              {rows.map((r) => (
                <tr key={r.name} id={rowId('addon', r.name)} className={rowClass(ctx, 'addon', r.name)}>
                  <td>
                    <Link to={objectPath(ctx.tenant, ctx.env, `addon/${r.name}`)} navigate={ctx.navigate} className="mono strong">{r.name}</Link>
                  </td>
                  <td><Marks marks={r.marks} /></td>
                  <td className="small">{r.addon.type}</td>
                  <td className="small">
                    <span className="inv-uses">
                      {r.uses.map((u) => {
                        const f = fieldsWords(u.fields);
                        return (
                          <span key={`${u.from.key}/${u.from.name}`}>
                            <ObjLink from={u.from} ctx={ctx} />{f && <span className="muted mono"> · {f}</span>}
                          </span>);
                      })}
                      {r.uses.length === 0 && <span className="muted">no one: unused</span>}
                    </span>
                  </td>
                </tr>))}
            </tbody>
          </table>
        </div>)}
      {!loading && rows.length === 0 && <p className="empty">No addon that admits by identity or hands out a password.</p>}
    </Group>
  );
}
