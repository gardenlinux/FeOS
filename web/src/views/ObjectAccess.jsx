// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { Fragment } from 'react';
import { CopyBtn } from '../components.jsx';
import { fmtAgo, fmtDate, fmtDateTime } from '../format.js';
import { queryOf } from '../logs.js';
import { httpSecretPath, objectPath, secretsPath } from '../routes.js';
import {
  MARKS, MARK_LABEL, accessOf, fieldsWords, identityReaders, kindShort, podSubject, soonestCert, subjectParts, usageOf, usageTone,
  usageWords, workloadOf,
} from './access.js';
import { useAddonCatalog, useBindingUsage, useEnvSecrets, useIdentities } from './accessData.js';
import { hostOf } from './httpSecret.js';
import { referencesOf } from './refs.js';
import { BindingNumbers } from './EnvSecrets.jsx';
import { EnvLink } from './EnvTabs.jsx';
import { KeyMark } from './Marks.jsx';
import '../styles/marks.css';
import '../styles/access.css';

// The Secrets section of a service's and a job's Config: who the
// workload is (its SPIFFE ID and when its pods' certificates end, and,
// unfolded, what the certificate names and who reads which part), then
// what it reaches with a key or without one, one row per target, sorted
// by where the key lives (access.js): by identity (an addon that admits
// it by its certificate), at the node (an HTTP secret its variables
// reference, ADR 0055; a binding still stated in the spec; the registry
// login the node pulls with), in the container (a variable from a value
// secret, an addon's password). An HTTP secret's and a binding's row has
// its numbers of the last 24 h. Never a key: names, versions and counts
// only. Its overview has the counts alone (ObjectKeysLine), with the
// APIs that failed.

const Link = EnvLink;

const SPEC_KEY = { service: 'services', job: 'jobs' };

// callsApis tells whether object kind/name of spec calls an API through
// the node: a variable that references a secret with a key (an HTTP
// secret's url or host), or a binding of the spec; then its numbers are
// read. A variable without a key that names an HTTP secret shows without
// them.
function callsApis(spec, kind, name) {
  const so = spec && spec[SPEC_KEY[kind]] ? spec[SPEC_KEY[kind]][name] : null;
  if (so && Array.isArray(so.bindings) && so.bindings.length > 0) return true;
  return referencesOf(spec).some((r) => r.from.key === SPEC_KEY[kind] && r.from.name === name && r.http);
}

// ObjectAccess reads what the section shows beside the spec: the
// catalog, the environment's secrets (an HTTP secret's definition), its
// workloads' identities and, when the object calls an API through the
// node, the bindings' use.
export function ObjectAccess({ tenant, st, kind, name, spec, navigate }) {
  const env = st.name;
  const catalog = useAddonCatalog();
  const secrets = useEnvSecrets(env);
  const ids = useIdentities(env);
  const usage = useBindingUsage(env, callsApis(spec, kind, name));
  return (
    <AccessCard tenant={tenant} env={env} kind={kind} name={name} spec={spec} loading={!spec && st.deployment > 0}
      catalog={catalog} secrets={secrets.data} identities={ids.data} identitiesErr={ids.err} usage={usage}
      navigate={navigate} />
  );
}

// AccessCard is the section, its data given: catalog, secrets (the
// environment's, both kinds), identities and usage null while they load.
export function AccessCard({
  tenant, env, kind, name, spec, loading = false, catalog = null, secrets = null, identities = null, identitiesErr = '',
  usage = null, now = Date.now(), navigate,
}) {
  const rows = spec ? accessOf(spec, SPEC_KEY[kind], name, { catalog, secrets: secrets || [] }) : [];
  const bindings = rows.some((r) => r.kind === 'binding' || r.kind === 'http');
  return (
    <div className="acc-sect">
      <div className="sect sg-objsect">
        Secrets
        <span className="muted small">where its keys live</span>
        <Link to={secretsPath(tenant, env)} navigate={navigate} className="small sg-all">All of the environment&apos;s</Link>
      </div>
      <IdentityLine workload={workloadOf(identities, kind, name)} identities={identities} err={identitiesErr} now={now}
        readers={identityReaders(rows, catalog)} />
      {loading && <p className="muted">Loading…</p>}
      {!loading && rows.length === 0 && (
        <p className="muted acc-none">It names no secret or addon: it holds no key and needs none.</p>)}
      {rows.length > 0 && (
        <div className="tblwrap">
          <table className="tbl acc-tbl">
            <tbody>
              {rows.map((r) => (
                <AccessRow key={`${r.kind}/${r.target.name}`} r={r} tenant={tenant} env={env} item={`${kind}/${name}`} spec={spec}
                  listed={secrets !== null} catalog={catalog} usage={usage} navigate={navigate} />))}
            </tbody>
          </table>
        </div>)}
      {bindings && usage && usage.unavailable && <p className="ov-note">No numbers of the last 24 h: {usage.unavailable}</p>}
    </div>
  );
}

// ObjectKeysLine is the Secrets section's count on the overview: how
// many of its targets it reaches by identity, at the node and in the
// container, and each HTTP secret or binding whose upstream refused or
// failed in the last 24 h; with the way to its Config. Nothing for one
// that names no secret or addon.
export function ObjectKeysLine({ tenant, st, kind, name, spec, navigate }) {
  const env = st.name;
  const catalog = useAddonCatalog();
  const usage = useBindingUsage(env, callsApis(spec, kind, name));
  return <KeysCount tenant={tenant} env={env} kind={kind} name={name} spec={spec} catalog={catalog} usage={usage} navigate={navigate} />;
}

// KeysCount is the line, its data given: catalog and usage null while
// they load (an addon's row counts once the catalog says its mark). The
// spec says which secret is an HTTP one (its reference's key, a pin's
// type), the list is not read for it.
export function KeysCount({ tenant, env, kind, name, spec, catalog = null, usage = null, navigate }) {
  if (!spec) return null;
  const rows = accessOf(spec, SPEC_KEY[kind], name, { catalog });
  if (rows.length === 0) return null;
  const counts = MARKS.map((m) => [m, rows.filter((r) => r.mark === m).length]).filter(([, n]) => n > 0);
  const failed = rows.filter((r) => r.kind === 'binding' || r.kind === 'http').map((r) => [r.target.name, usageOf(usage, r.target.name)])
    .filter(([, u]) => usageTone(u) === 'bad');
  const config = objectPath(tenant, env, `${kind}/${name}`, 'config');
  return (
    <p className="ov-keys obj-keys">
      Keys:{' '}
      {counts.map(([m, n], i) => <Fragment key={m}>{i > 0 && ' · '}<b>{n}</b> {MARK_LABEL[m]}</Fragment>)}
      {counts.length === 0 && <span className="muted">…</span>}
      {failed.map(([b, u]) => (
        <Fragment key={b}> · <span className="bad">{b}: {usageWords(u).split(' · ').slice(1).join(' · ')}</span></Fragment>))}
      <span className="muted"> — each with its source, its identity and certificate:</span>{' '}
      <Link to={config} navigate={navigate}>Config</Link>
    </p>
  );
}

// IdentityLine is who the workload is: its SPIFFE ID, which its pods'
// certificates carry, when the first of them ends, and the certificate
// to unfold.
function IdentityLine({ workload, identities, err, now, readers }) {
  let body;
  let cert = null;
  if (!identities) body = <span className="muted">{err ? `not known: ${err}` : '…'}</span>;
  else if (!identities.trustDomain || !workload || !workload.spiffeId) {
    body = (
      <span className="muted">
        {!identities.trustDomain ? 'none: this installation issues no workload certificates' : 'none yet'}
      </span>);
  } else {
    body = (
      <>
        <span className="mono small acc-spiffe">{workload.spiffeId}</span>
        <CopyBtn text={workload.spiffeId} mini title="copy the SPIFFE ID" />
        <CertUntil pods={workload.pods} now={now} />
      </>);
    cert = <Certificate workload={workload} identities={identities} readers={readers} now={now} />;
  }
  return (
    <div className="acc-identity">
      <div className="acc-idline">
        <span className="acc-k" title="the SPIFFE ID its pods' certificates carry; an addon by identity admits it by that">Identity</span>
        {body}
      </div>
      {cert}
    </div>
  );
}

// CertUntil is when its pods' certificate that ends first ends; every
// pod's on hover.
function CertUntil({ pods, now }) {
  const list = Array.isArray(pods) ? pods : [];
  if (list.length === 0) return <span className="muted small">no pod now</span>;
  const first = soonestCert(list);
  if (!first) return <span className="muted small">no certificate yet</span>;
  const line = (p) => `${p.pod}${p.node ? ` on ${p.node}` : ''}: ${p.notAfter ? `until ${fmtDateTime(p.notAfter)}` : 'no certificate yet'}`;
  return (
    <span className="small acc-cert" title={list.map(line).join('\n')}>
      cert until <b>{untilText(first.notAfter, now)}</b>{' '}
      <span className="muted">{fmtAgo(first.notAfter, now)}{list.length > 1 ? `, the first of ${list.length} pods` : ''}</span>
    </span>
  );
}

// READS says what an addon makes of the part of the certificate it
// reads (the catalog's identityBy).
const READS = {
  ou: (r) => <>maps the <b>OU</b> to {r.role ? <>role <span className="mono">{r.role}</span></> : 'the role its reference names'}</>,
  uri: () => <>maps the <b>URI</b> to its user</>,
};

// Certificate is what its pods' certificates name, unfolded: the
// subject (CN the pod, OU the workload), the URI name (the SPIFFE ID),
// the issuer, the key, which part each addon by identity reads, and each
// pod's newest certificate. A ferund of before the subject answers the
// SPIFFE ID and the ends alone; what it does not answer is left out.
function Certificate({ workload, identities, readers, now }) {
  const pods = Array.isArray(workload.pods) ? workload.pods : [];
  const subject = podSubject(identities, workload, pods.find((p) => p.subject) || null);
  const parts = subjectParts(subject);
  const what = { CN: pods.length > 1 ? 'the pod: each its own' : 'the pod', OU: 'the workload: every pod of it the same' };
  return (
    <details className="acc-certd">
      <summary>Certificate</summary>
      <dl className="acc-certf">
        {parts.length > 0 && (
          <>
            <dt>Subject</dt>
            <dd>
              <span className="mono small">{subject}</span>
              <div className="acc-dn">
                {parts.map(([k, v]) => (
                  <div key={k + v}>
                    <span className="mono small"><b>{k}</b> {v}</span>{what[k] && <span className="muted small"> · {what[k]}</span>}
                  </div>))}
              </div>
            </dd>
          </>)}
        <dt>URI</dt>
        <dd><span className="mono small">{workload.spiffeId}</span><span className="muted small"> · the SPIFFE ID, its only name</span></dd>
        {identities.issuer && (<><dt>Issuer</dt><dd><span className="mono small">{identities.issuer}</span></dd></>)}
        <dt>Key</dt>
        <dd className="small">Ed25519, made in the pod, never leaves it · client and server authentication · renewed at three quarters of its lifetime</dd>
        {readers.length > 0 && (
          <>
            <dt>Read by</dt>
            <dd className="small">
              {readers.map((r) => (
                <div key={r.addon}>
                  <span className="mono">{r.addon}</span> <span className="muted">({r.type})</span>{' '}
                  {READS[r.by] ? READS[r.by](r) : 'admits it by it'}
                </div>))}
            </dd>
          </>)}
      </dl>
      {pods.length > 0 && (
        <div className="tblwrap">
          <table className="tbl acc-certpods">
            <thead><tr><th>Pod</th><th>Serial</th><th>Issued</th><th>Valid until</th><th>Node</th></tr></thead>
            <tbody>
              {pods.map((p) => (
                <tr key={p.pod}>
                  <td className="mono small">{p.pod}</td>
                  <td className="mono small acc-serial">{p.serial || <span className="muted">{p.notAfter ? '…' : 'none yet'}</span>}</td>
                  <td className="small">{p.issuedAt ? fmtDateTime(p.issuedAt) : ''}</td>
                  <td className="small">{p.notAfter ? <>{fmtDateTime(p.notAfter)} <span className="muted">{fmtAgo(p.notAfter, now)}</span></> : ''}</td>
                  <td className="mono small">{p.node}</td>
                </tr>))}
            </tbody>
          </table>
        </div>)}
    </details>
  );
}

// untilText is a time as the hour of today, else with its date.
function untilText(iso, now) {
  const d = new Date(iso);
  const day = (x) => new Date(x).toDateString();
  return day(d) === day(now) ? fmtDate(iso).slice(11) : fmtDate(iso);
}

// AccessRow is one target: what the code names, what it reaches, the
// mark of where the key lives, the key's source or the fields, and an
// API's numbers. listed says the environment's secrets answered: until
// then an HTTP secret's definition is its pin's.
function AccessRow({ r, tenant, env, item, spec, listed, catalog, usage, navigate }) {
  const secret = (n) => (
    <Link to={secretsPath(tenant, env, 'secret', n)} navigate={navigate} className="mono" title="its row in the environment's secrets">{n}</Link>);
  const logs = objectPath(tenant, env, item, 'logs', null, queryOf({ source: 'egress', since: '1d' }));
  let what = null;
  let to = null;
  let detail = null;
  let nums = null;
  if (r.kind === 'http') {
    const h = r.http;
    what = (
      <>
        {r.variables.length > 0 && <span className="mono strong">{r.variables.join(', ')}</span>}
        {r.variables.length === 0 && r.via.length > 0 && <span className="mono strong">binding {r.via.join(', ')}</span>}
      </>);
    to = (
      <>
        <span className="acc-arrow">→</span><span className="mono host">{hostOf(h.upstream) || h.name}</span>{' '}
        <span className="muted small">{h.mode === 'intercept' ? 'at its own name, intercepted' : <>reached as <span className="mono">{h.reachedAs}</span></>}</span>
      </>);
    detail = <HttpKey r={r} tenant={tenant} env={env} listed={listed} navigate={navigate} />;
    nums = <BindingNumbers u={usageOf(usage, h.name)} tenant={tenant} env={env} navigate={navigate} logs={logs} />;
  } else if (r.kind === 'binding') {
    const b = r.binding;
    what = (
      <Link to={secretsPath(tenant, env, 'binding', b.name)} navigate={navigate} className="mono strong"
        title="its row in the environment's secrets, with its check">{b.name}</Link>);
    to = (
      <>
        <span className="acc-arrow">→</span><span className="mono host">{hostOf(b.upstream) || '…'}</span>{' '}
        <span className="muted small">{b.mode === 'intercept' ? 'at its own name, intercepted' : <>reached as <span className="mono">{b.reachedAs}</span></>}</span>
      </>);
    detail = (
      <>
        <BindingKey b={b} secret={secret} />
        <div className="warntext small">binding in the spec — becomes an HTTP secret</div>
      </>);
    nums = <BindingNumbers u={usageOf(usage, b.name)} tenant={tenant} env={env} navigate={navigate} logs={logs} />;
  } else if (r.kind === 'addon') {
    what = <Link to={objectPath(tenant, env, `addon/${r.target.name}`)} navigate={navigate} className="mono strong">{r.target.name}</Link>;
    to = <><span className="acc-arrow">→</span><span>{r.addon.type || 'addon'}</span></>;
    const f = fieldsWords(r.fields);
    detail = (
      <>
        {f && <span className="mono">{f}</span>}
        {r.variables.length > 0 && <span className="muted mono">{f ? ' · ' : ''}{r.variables.join(', ')}</span>}
        {r.variables.length === 0 && <span className="muted">reaches it without a variable</span>}
      </>);
  } else if (r.kind === 'secret') {
    what = <span className="mono strong">{r.variables.join(', ')}</span>;
    to = <><span className="acc-arrow">←</span>{secret(r.target.name)}</>;
    const pin = spec && spec.secrets && spec.secrets[r.target.name] ? Number(spec.secrets[r.target.name].version) || 0 : 0;
    detail = <span className="muted">{pin ? `secret v${pin}` : 'secret, its latest version'}</span>;
  } else {
    what = <span className="strong">image pull</span>;
    to = <><span className="acc-arrow">←</span>{secret(r.target.name)}</>;
    detail = <span className="muted">the node pulls with it; the container never gets it</span>;
  }
  const pending = r.kind === 'addon' && !catalog;
  return (
    <tr className={'acc-row acc-' + r.kind}>
      <td className="acc-what">{what}</td>
      <td className="acc-to">{to}</td>
      <td className="acc-mark">{pending ? <span className="muted">…</span> : <KeyMark mark={r.mark} />}</td>
      <td className="small acc-detail">{detail}</td>
      <td className="acc-nums-cell">{nums}</td>
    </tr>
  );
}

// HttpKey is an HTTP secret's key: its authentication's kind, the secret
// at the version the latest deployment pins, the latest beside it when
// it is later; the keys of its variables (url, host); the binding of the
// spec it comes through, when one names it.
function HttpKey({ r, tenant, env, listed, navigate }) {
  const h = r.http;
  const kind = kindShort(h.kind);
  return (
    <>
      {kind && <span>{kind} · </span>}
      HTTP secret{' '}
      <Link to={httpSecretPath(tenant, env, h.name)} navigate={navigate} className="mono" title="its page: definition, check, uses">{h.name}</Link>
      {h.pinned > 0 && <span className="mono"> v{h.pinned}</span>}
      {h.pinned > 0 && h.version > h.pinned && <span className="warntext"> · latest v{h.version}</span>}
      {listed && !h.known && <span className="bad"> · not in this environment</span>}
      {r.keys.length > 0 && <span className="muted"> · its {r.keys.join(' and ')}</span>}
      {r.via.length > 0 && <div className="warntext small">through binding {r.via.join(', ')} in the spec — becomes a variable</div>}
    </>
  );
}

// BindingKey is a binding's key: its kind and the environment's secrets
// its credential names.
function BindingKey({ b, secret }) {
  const kind = kindShort(b.kind);
  return (
    <>
      {kind && <span>{kind}</span>}
      {b.secrets.length > 0 && (
        <>
          {kind && ' · '}secret{b.secrets.length === 1 ? '' : 's'}{' '}
          {b.secrets.map((n, i) => <Fragment key={n}>{i > 0 && ', '}{secret(n)}</Fragment>)}
        </>)}
    </>
  );
}
