// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { Fragment, useCallback, useEffect, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { Ago, ErrBox, usePoll } from '../components.jsx';
import { fmtDateTime, shortId } from '../format.js';
import { useEnvStatus } from '../envstatus.jsx';
import { queryOf } from '../logs.js';
import { envPath, httpSecretPath, newHttpSecretPath, objectPath, sandboxPath, secretsPath } from '../routes.js';
import {
  INVENTORY_GROUPS, fieldsWords, groupRows, httpPinNote, inventoryOf, kindShort, problemWords, secretPinNote, strayUsage,
  usageOf, usageTone, usageWords, usersWords,
} from './access.js';
import { useAddonCatalog, useBindingUsage } from './accessData.js';
import { hostOf, needsDeployment } from './httpSecret.js';
import { DeployResult, moveToLatest } from './HttpSecret.jsx';
import { DeleteSecret, EnvLink, NEEDS, NewSecret, OpNote, SecretValue, useOp } from './EnvTabs.jsx';
import { PreflightResult, useLatestSpec } from './Environments.jsx';
import { KeyMark, MarkLegend } from './Marks.jsx';
import '../styles/marks.css';
import '../styles/access.css';
import '../styles/credentials.css';

// An environment's Secrets view (…/secrets[/:kind/:name]): every key its
// workloads reach, each once, grouped by where it lives (ADR 0055,
// access.js), not by where it is stored: by identity (the addons that
// admit a workload by its certificate), at the node (the HTTP secrets,
// with their URL, how the code calls them, who uses them at which
// version, their last 24 h, Check and "Move to vN"; the value secrets
// only image pulls name; the bindings still stated in the latest
// deployment's spec, which become HTTP secrets), in the container (the
// value secrets variables reference, the addons' passwords) and unused
// (the value secrets nothing references). A legend of the three marks
// first, and Add secret, which asks the kind first. An address with
// kind/name points at a row; secrets/http/<name> is an HTTP secret's page
// (HttpSecret.jsx). Values are never shown; a writer sets, adds,
// deletes and checks, a reader sees the rest (ec, envCan).

const Link = EnvLink;

const SECRETS_MS = 10000;

// KIND_OF names an object's kind by its spec key.
const KIND_OF = { services: 'service', jobs: 'job' };

// rowId is the element ID of a row of the inventory.
const rowId = (kind, name) => `inv-${kind}-${name}`;

// EnvSecrets reads the view's data: the latest deployment's spec, the
// secrets of both kinds, the catalog and the bindings' use; and writes a
// value secret's value or a secret's delete.
export function EnvSecrets({ project, env, st, ec, item = '', changed = () => {}, navigate }) {
  const spec = useLatestSpec(st);
  const catalog = useAddonCatalog();
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
  // Until the list answered once: without it, every secret the spec
  // names would read as missing.
  const loading = !st || (st.deployment > 0 && spec === null) || secrets === null;

  // The row the address points at comes into view once it is there.
  useEffect(() => {
    if (loading || !item) return;
    const i = item.indexOf('/');
    const el = document.getElementById(rowId(item.slice(0, i), item.slice(i + 1)));
    if (el) el.scrollIntoView({ block: 'center' });
  }, [item, loading]);

  return (
    <>
      <ErrBox error={err} />
      <OpNote op={op} onDismiss={clear} />
      {loading && !err && <p className="empty">Loading…</p>}
      {!loading && (
        <SecretsInventory project={project} env={env} deployment={st.deployment} spec={spec} secrets={secrets}
          catalog={catalog} usage={usage} sandboxes={sandboxes} ec={ec} focus={item}
          editing={editing} setEditing={setEditing} onPut={put} onDelete={del} busy={!!(op && op.busy)}
          onChanged={() => { readSecrets(); changed(); }} navigate={navigate} />)}
    </>
  );
}

// GROUP_NOTES say what each group holds.
const GROUP_NOTES = {
  identity: <>No key at all: the addon admits a workload by its certificate.</>,
  offloaded: <>The key stays at the node, which adds it to each request to its API (an HTTP secret) or pulls the image
    with it: the container never holds it. A deployment pins the version it got; a sandbox gets the latest.</>,
  container: <>The value is handed to the container: a variable from a value secret, an addon&apos;s password. A key for
    an HTTP API is safer as an HTTP secret.</>,
  unused: <>Value secrets no deployment references.</>,
};

// GROUP_EMPTY say a group without rows.
const GROUP_EMPTY = {
  identity: 'No addon admits a workload by its certificate.',
  offloaded: 'No key at the node.',
  container: 'No key in the container.',
  unused: 'Nothing unused.',
};

// SecretsInventory is the view, its data given: the spec of the latest
// deployment (null without one), the secrets as listed (both kinds), the
// catalog (null while it loads), the bindings' use (null without).
export function SecretsInventory({
  project, env, deployment = 0, spec, secrets, catalog, usage, sandboxes = null, ec, focus = '', editing = '',
  setEditing = () => {}, onPut, onDelete, busy = false, onChanged = () => {}, navigate, adding = '',
}) {
  const rows = inventoryOf(spec, { catalog, secrets: secrets || [] });
  const stray = strayUsage(usage, spec, secrets || []);
  const keyed = rows.filter((r) => !r.folded);
  const missing = rows.filter((r) => (r.kind === 'value' || r.kind === 'http') && r.state === 'missing').length;
  const troubled = rows.filter((r) => (r.kind === 'http' || r.kind === 'binding') && usageTone(usageOf(usage, r.name)) === 'bad').length;
  const count = (place) => groupRows(rows, place).length + (place === 'offloaded' ? stray.length : 0);
  const ctx = { project, env, deployment, spec, secrets: secrets || [], usage, sandboxes, ec, focus, navigate, editing, setEditing,
    onPut, onDelete, busy, onChanged };
  return (
    <div className="inv">
      <p className="panel-note inv-note">
        Every key this environment&apos;s services, jobs and sandboxes reach, each once, by where it lives: no key at
        all, at the node, or in the container. Values are never shown, here or anywhere.
      </p>
      <MarkLegend />
      <div className="facts inv-facts">
        {INVENTORY_GROUPS.map((g) => (
          <span key={g.place}><b>{count(g.place)}</b> {g.title.toLowerCase()}</span>))}
        {missing > 0 && <span className="bad" title="named by the spec, without a value">▲ <b className="bad">{missing}</b> missing</span>}
        {troubled > 0 && <span className="bad" title="refused, failing or without a token in the last 24 h">▲ <b className="bad">{troubled}</b> with problems</span>}
        <span className="spacer" />
        <AddSecret project={project} env={env} ec={ec} onPut={onPut} navigate={navigate} initial={adding} />
      </div>
      {usage && usage.unavailable && <p className="muted small inv-unavailable">No numbers of the last 24 h: {usage.unavailable}</p>}
      {INVENTORY_GROUPS.map((g) => (
        <PlaceGroup key={g.place} group={g} rows={groupRows(keyed, g.place)} stray={g.place === 'offloaded' ? stray : []}
          loading={!catalog && (g.place === 'identity' || g.place === 'container')} ctx={ctx} />))}
    </div>
  );
}

// AddSecret is "Add secret": it asks the kind first (ADR 0055) — a value
// the code reads, whose form is a name and a value here, or an HTTP API
// the platform calls with the key, whose form is its page's. initial
// opens it at a step ('ask', 'value').
export function AddSecret({ project, env, ec, onPut, navigate, initial = '' }) {
  const [step, setStep] = useState(initial);
  const toHttp = newHttpSecretPath(project, env);
  if (!step) {
    return (
      <button className="primary mini" disabled={!ec.write} title={ec.write ? 'a value, or an HTTP API and its key' : NEEDS.write}
        onClick={() => setStep('ask')}><Icon name="plus" size={12} /> Add secret</button>);
  }
  return (
    <div className="inv-add">
      <div className="inv-add-head">
        <b>{step === 'ask' ? 'What is it?' : 'A value your code reads'}</b>
        <span className="spacer" />
        <button className="ghost mini" onClick={() => setStep(step === 'ask' ? '' : 'ask')}>{step === 'ask' ? 'Cancel' : 'Back'}</button>
      </div>
      {step === 'ask' && (
        <div className="inv-add-choices">
          <button type="button" className="inv-add-choice rec" onClick={() => navigate(toHttp)}>
            <span className="inv-add-title"><Icon name="lock" size={13} /> An HTTP API the platform calls with the key</span>
            <span className="badge acc">recommended for an API</span>
            <span className="muted small">Its URL, how the code calls it and its authentication. The code calls the API
              through FeOS, which adds the key at the node: the container never holds it.</span>
          </button>
          <button type="button" className="inv-add-choice" onClick={() => setStep('value')}>
            <span className="inv-add-title"><Icon name="key" size={13} /> A value your code reads</span>
            <span className="muted small">A setting, a signing key, a password for something that is no HTTP API: handed to
              the containers whose variables reference it.</span>
          </button>
        </div>)}
      {step === 'value' && <NewSecret disabled={!ec.write} onPut={onPut} />}
    </div>
  );
}

// PlaceGroup is a group of the inventory: its mark and title, its count,
// a line of what it holds, its rows.
function PlaceGroup({ group, rows, stray, loading, ctx }) {
  const n = rows.length + stray.length;
  return (
    <section className="envsect inv-group" id={`inv-${group.place}`}>
      <div className="sect sect-row">
        {group.mark ? <KeyMark mark={group.mark} label={group.title} /> : <span className="badge mark mark-none">{group.title}</span>}
        <span className="count">· {loading ? '…' : n}</span>
      </div>
      <p className="panel-note envsecrets-note">{GROUP_NOTES[group.place]}</p>
      {loading && rows.length === 0 && <p className="empty">Loading…</p>}
      {n > 0 && (
        <div className="tblwrap">
          <table className="tbl inv-tbl">
            <thead><tr><th>name</th><th>what</th><th>used by</th><th>state</th><th className="col-quick" /></tr></thead>
            <tbody>
              {rows.map((r) => <InventoryRow key={`${r.kind}/${r.name}`} r={r} ctx={ctx} />)}
              {stray.map((u) => <StrayBindingRow key={`stray/${u.name}`} u={u} ctx={ctx} />)}
            </tbody>
          </table>
        </div>)}
      {!loading && n === 0 && <p className="empty">{GROUP_EMPTY[group.place]}</p>}
    </section>
  );
}

function InventoryRow({ r, ctx }) {
  if (r.kind === 'http') return <HttpRow r={r} ctx={ctx} />;
  if (r.kind === 'binding') return <BindingRow r={r} ctx={ctx} />;
  if (r.kind === 'value') return <ValueRow r={r} ctx={ctx} />;
  return <AddonRow r={r} ctx={ctx} />;
}

// ObjLink is an object of the spec ({key, name}), a link to its page.
function ObjLink({ from, ctx }) {
  const kind = KIND_OF[from.key];
  if (!kind) return <span className="mono">{from.name}</span>;
  return (
    <span className="inv-obj">
      <span className="muted">{kind}</span>{' '}
      <Link to={objectPath(ctx.project, ctx.env, `${kind}/${from.name}`)} navigate={ctx.navigate} className="mono">{from.name}</Link>
    </span>
  );
}

// SandboxLink is a sandbox of the environment, by its ID.
function SandboxLink({ id, ctx }) {
  const sb = (ctx.sandboxes || []).find((s) => s.sandboxId === id);
  return (
    <span className="inv-obj">
      <span className="muted">sandbox</span>{' '}
      <Link to={sandboxPath(ctx.project, ctx.env, id)} navigate={ctx.navigate} className="mono"
        title={sb && sb.owner ? `${id} · ${sb.owner}` : id}>{shortId(id)}</Link>
    </span>
  );
}

// rowClass marks the row the address points at.
function rowClass(ctx, kind, name, extra = '') {
  return [ctx.focus === `${kind}/${name}` ? 'sel' : '', extra].filter(Boolean).join(' ') || undefined;
}

// logsOf are the environment's outbound requests of the last day.
const logsOf = (ctx) => envPath(ctx.project, ctx.env, 'logs', '', null, queryOf({ source: 'egress', since: '1d' }));

// Reaches is an API's host and how the code calls it, and its
// authentication's kind.
function Reaches({ b }) {
  const kind = kindShort(b.kind);
  return (
    <>
      <span className="mono host">{hostOf(b.upstream) || '—'}</span>{' '}
      <span className="muted">{b.mode === 'intercept' ? 'intercepted' : <>as <span className="mono">{b.reachedAs}</span></>}</span>
      {kind && <div>{kind}</div>}
    </>
  );
}

// ------------------------------------------------------------ HTTP secrets --

// HttpRow is an HTTP secret: its URL and how the code calls it, its
// authentication, who uses it (the services and jobs of the latest
// deployment, by their variables or a binding of the spec; the
// sandboxes, at the latest version), its version against what the
// deployments pin, its last 24 h, Check and the deployment that moves
// them to the latest.
function HttpRow({ r, ctx }) {
  const { project, env, ec, navigate } = ctx;
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState(null);
  const [op, run] = useOp();
  const checking = !!(op && op.busy);
  const res = (op && op.result) || null;
  const pins = r.item ? r.pins : { latest: r.pinned, deployed: 0 };
  const pin = r.item ? httpPinNote(pins, r.version) : '';
  const move = async () => {
    setBusy(true);
    setResult(null);
    try {
      setResult(await moveToLatest(env, r.name, r.version));
      ctx.onChanged();
    } catch (e) {
      setResult({ error: e.message, findings: (e.body && e.body.findings) || [] });
    } finally {
      setBusy(false);
    }
  };
  const to = httpSecretPath(project, env, r.name);
  return (
    <>
      <tr id={rowId('http', r.name)} className={rowClass(ctx, 'http', r.name, r.state === 'missing' ? 'inv-missing' : '')}>
        <td><Link to={to} navigate={navigate} className="mono strong" title="its page: definition, check, new version, uses, delete">{r.name}</Link></td>
        <td className="small">
          <span className="inv-kind">HTTP secret</span>
          {r.item ? <div><Reaches b={r.http} /></div> : <div className="bad">no HTTP secret {r.name} in this environment</div>}
        </td>
        <td className="small">
          <span className="inv-uses">
            {r.users.map((u) => (
              <span key={`${u.key}/${u.name}`}>
                <ObjLink from={u} ctx={ctx} />
                {u.variables.length > 0 && <> <span className="muted">·</span> <span className="mono">{u.variables.join(', ')}</span></>}
                {u.bindings.length > 0 && <span className="muted"> · binding {u.bindings.join(', ')} in the spec</span>}
              </span>))}
            {r.sandboxes.map((id) => (
              <span key={id}><SandboxLink id={id} ctx={ctx} /> <span className="muted">· the latest</span></span>))}
            {r.users.length === 0 && r.sandboxes.length === 0 && <span className="muted">no one</span>}
          </span>
        </td>
        <td className="small inv-state">
          {r.item ? (
            <>
              <span className="mono">v{r.version}</span>
              {r.item.createdAt && <> <Ago iso={r.item.createdAt} /></>}
              {r.state === 'unused' && <span className="muted"> · unused</span>}
              {pins.latest > 0 && !pin && <span className="muted"> · pinned</span>}
              {pin && <div className="warntext">{pin}</div>}
            </>)
            : <><span className="badge bad">missing</span> <span className="bad">named by the spec</span></>}
          <div><BindingNumbers u={usageOf(ctx.usage, r.name)} project={project} env={env} logs={logsOf(ctx)} navigate={navigate} /></div>
        </td>
        <td className="col-quick nowrap">
          {r.item && (
            <button className="mini" disabled={!ec.write || checking} title={ec.write ? 'resolve, dial, check TLS and the key, from an instance' : NEEDS.write}
              onClick={() => run(() => api.checkSecret(env, r.name), '')}>
              <Icon name="check" size={12} /> {checking ? 'Checking…' : 'Check'}</button>)}
          {r.item && needsDeployment(pins, r.version) && (
            <>{' '}<button className="primary mini" disabled={!ec.write || busy}
              title={ec.write ? `a new deployment with ${r.name} at version ${r.version}` : NEEDS.write} onClick={move}>
              {busy ? 'Deploying…' : `Move to v${r.version}`}</button></>)}
          {!r.item && ec.write && (
            <Link to={newHttpSecretPath(project, env)} navigate={navigate} className="small">Add it</Link>)}
        </td>
      </tr>
      {(result || (op && (op.tone === 'bad' || res))) && (
        <tr className="subrow"><td colSpan={5}>
          {result && <DeployResult project={project} env={env} name={r.name} res={result} navigate={navigate} />}
          {op && op.tone === 'bad' && <div className="envop-note bad">{op.text}</div>}
          {res && <PreflightResult res={res} />}
        </td></tr>)}
    </>
  );
}

// ------------------------------------------------- bindings of the spec --

// BindingRow is a binding stated in the latest deployment's spec (ADR
// 0028, from before ADR 0055): what it reaches and as what, its key (the
// value secrets its credential names, whose rows are its own), who names
// it, its last 24 h and Check (the preflight with the secrets' latest
// versions). It becomes an HTTP secret: no form offers a new one.
function BindingRow({ r, ctx }) {
  const { project, env, ec, navigate, editing } = ctx;
  const b = r.binding;
  const [op, run] = useOp();
  const res = (op && op.result) || null;
  const busy = !!(op && op.busy);
  const missing = r.secrets.some((s) => s.state === 'missing');
  const mine = r.secrets.find((s) => s.name === editing);
  return (
    <>
      <tr id={rowId('binding', r.name)} className={rowClass(ctx, 'binding', r.name, missing ? 'inv-missing' : '')}>
        <td className="mono strong">{r.name}</td>
        <td className="small">
          <span className="inv-kind">binding in the spec</span>
          <div><Reaches b={b} /></div>
          {r.secrets.length > 0 && (
            <div>key from{' '}
              {r.secrets.map((s, i) => (
                <Fragment key={s.name}>{i > 0 && ', '}secret <span className="mono">{s.name}</span>
                  {s.state === 'missing' ? <span className="bad"> (no value)</span> : <span className="muted mono"> v{s.version}</span>}
                </Fragment>))}
            </div>)}
          <div className="warntext inv-hint">
            {r.taken
              ? <>in the spec — HTTP secret <Link to={httpSecretPath(project, env, r.name)} navigate={navigate} className="mono">{r.name}</Link> takes
                its place once a deployment references it</>
              : 'in the spec — becomes an HTTP secret'}
          </div>
        </td>
        <td className="small">
          <span className="inv-uses">
            {r.users.map((u) => <ObjLink key={`${u.key}/${u.name}`} from={u} ctx={ctx} />)}
            {r.users.length === 0 && <span className="muted">no one: unused</span>}
          </span>
        </td>
        <td className="small inv-state">
          {missing && <div><span className="badge bad">missing</span> <span className="bad">its key has no value</span></div>}
          <BindingNumbers u={usageOf(ctx.usage, r.name)} project={project} env={env} logs={logsOf(ctx)} navigate={navigate} />
        </td>
        <td className="col-quick nowrap">
          <button className="mini" disabled={!ec.write || busy} title={ec.write ? 'resolve, dial, check TLS and the key, from an instance' : NEEDS.write}
            onClick={() => run(() => api.checkBinding(env, r.name), '')}>
            <Icon name="check" size={12} /> {busy ? 'Checking…' : 'Check'}</button>
          {r.secrets.map((s) => (
            <Fragment key={s.name}>{' '}
              <button className={'mini' + (s.state === 'missing' ? ' primary' : '')} disabled={!ec.write || ctx.busy}
                title={ec.write ? `${s.state === 'missing' ? 'the first version' : 'a new version'} of secret ${s.name}` : NEEDS.write}
                onClick={() => ctx.setEditing(editing === s.name ? '' : s.name)}><Icon name="key" size={12} /> Set {s.name}</button>
            </Fragment>))}
        </td>
      </tr>
      {mine && (
        <tr className="secret-edit"><td colSpan={5}>
          <SecretValue name={mine.name} onPut={ctx.onPut} onCancel={() => ctx.setEditing('')} />
        </td></tr>)}
      {op && (op.tone === 'bad' || res) && (
        <tr className="subrow"><td colSpan={5}>
          {op.tone === 'bad' && <div className="envop-note bad">{op.text}</div>}
          {res && <PreflightResult res={res} />}
        </td></tr>)}
    </>
  );
}

// StrayBindingRow is a binding a sandbox stated itself (its own set,
// which ADR 0055 ends), from its requests of the last 24 h.
function StrayBindingRow({ u, ctx }) {
  const { project, env, navigate } = ctx;
  const users = Array.isArray(u.users) ? u.users : [];
  return (
    <tr id={rowId('binding', u.name)} className={rowClass(ctx, 'binding', u.name, 'inv-stray')}>
      <td className="mono strong">{u.name}</td>
      <td className="small"><span className="inv-kind">a sandbox&apos;s own binding</span>
        <div className="muted">not shown: its sandbox&apos;s token wrote it</div></td>
      <td className="small">
        <span className="inv-uses">
          {users.map((x) => (x.kind === 'sandbox'
            ? <SandboxLink key={`s/${x.name}`} id={x.name} ctx={ctx} />
            : <ObjLink key={`${x.kind}/${x.name}`} from={{ key: `${x.kind}s`, name: x.name }} ctx={ctx} />))}
        </span>
      </td>
      <td><BindingNumbers u={u} project={project} env={env} logs={logsOf(ctx)} navigate={navigate} /></td>
      <td className="col-quick" />
    </tr>
  );
}

// BindingNumbers are an API's last 24 h (an HTTP secret's, a binding's):
// its requests, then its problems in their tone, the last problem and
// who sent them on hover, the outbound requests in the logs (logs, a
// path) and the last problem's trace.
export function BindingNumbers({ u, project, env, logs = '', navigate }) {
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
        <> · <Link to={envPath(project, env, 'traces', p.traceId)} navigate={navigate} title={problemWords(p)}>last problem</Link></>)}
    </span>
  );
}

// ---------------------------------------------------------- value secrets --

// ValueRow is a value secret: who uses it and how (a variable, the
// registry login), its version against what the latest deployment pins,
// missing when the spec names it without a value; Set value and Delete.
function ValueRow({ r, ctx }) {
  const { ec, editing, setEditing } = ctx;
  const s = ctx.secrets.find((x) => x.name === r.name) || null;
  return (
    <>
      <tr id={rowId('secret', r.name)} className={rowClass(ctx, 'secret', r.name, r.state === 'missing' ? 'inv-missing' : '')}>
        <td className="mono strong">{r.name}</td>
        <td className="small"><span className="inv-kind">value</span></td>
        <td className="small"><SecretUses uses={r.uses} ctx={ctx} /></td>
        <td className="small"><SecretState r={r} s={s} /></td>
        <td className="col-quick nowrap">
          <button className={'mini' + (r.state === 'missing' ? ' primary' : '')} disabled={!ec.write || ctx.busy}
            title={ec.write ? (r.state === 'missing' ? 'its first version' : 'a new version') : NEEDS.write}
            onClick={() => setEditing(editing === r.name ? '' : r.name)}><Icon name="key" size={12} /> Set value</button>
          {s && <>{' '}<DeleteSecret name={r.name} disabled={!ec.write || ctx.busy} onDelete={ctx.onDelete} /></>}
        </td>
      </tr>
      {editing === r.name && (
        <tr className="secret-edit"><td colSpan={5}>
          <SecretValue name={r.name} onPut={ctx.onPut} onCancel={() => setEditing('')} />
        </td></tr>)}
    </>
  );
}

// SecretUses say who uses a value secret and how: a variable, the
// registry login, a binding's credential.
function SecretUses({ uses, ctx }) {
  if (!uses || uses.length === 0) return <span className="muted">no one</span>;
  return (
    <span className="inv-uses">
      {uses.map((u) => {
        const how = u.how === 'variable' ? <span className="mono">{u.variable}</span>
          : u.how === 'image' ? 'image pull'
            : <>binding <Link to={secretsPath(ctx.project, ctx.env, 'binding', u.binding)} navigate={ctx.navigate} className="mono">{u.binding}</Link></>;
        const k = `${u.from.key}/${u.from.name}/${u.how}/${u.variable || u.binding || ''}`;
        if (u.from.key === 'bindings') return <span key={k}>{how} <span className="muted">(no one names it)</span></span>;
        return <span key={k}><ObjLink from={u.from} ctx={ctx} /> <span className="muted">·</span> {how}</span>;
      })}
    </span>
  );
}

// SecretState is a value secret's version, when and by whom it was set,
// and what stands out: missing (the spec names it, it has no value), an
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

// ---------------------------------------------------------------- addons --

// AddonRow is an addon a workload reaches by its identity (its
// certificate admits it) or with the password the addon hands out.
function AddonRow({ r, ctx }) {
  return (
    <tr id={rowId('addon', r.name)} className={rowClass(ctx, 'addon', r.name)}>
      <td><Link to={objectPath(ctx.project, ctx.env, `addon/${r.name}`)} navigate={ctx.navigate} className="mono strong">{r.name}</Link></td>
      <td className="small"><span className="inv-kind">addon</span> <span className="muted">{r.addon.type}</span></td>
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
      <td className="small">{r.place === 'container' ? <span className="muted">its password</span> : <span className="muted">its certificate</span>}</td>
      <td className="col-quick" />
    </tr>
  );
}
