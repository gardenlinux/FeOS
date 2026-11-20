// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { Fragment } from 'react';
import { CopyBtn } from '../components.jsx';
import { fmtAgo, fmtDate, fmtDateTime } from '../format.js';
import { queryOf } from '../logs.js';
import { credentialPath, objectPath, secretsPath } from '../routes.js';
import {
  accessOf, fieldsWords, identityReaders, kindShort, podSubject, soonestCert, subjectParts, usageOf, workloadOf,
} from './access.js';
import { useAddonCatalog, useBindingUsage, useEnvCredentials, useIdentities } from './accessData.js';
import { hostOf } from './credentials.js';
import { BindingNumbers } from './EnvSecrets.jsx';
import { EnvLink } from './EnvTabs.jsx';
import { KeyMark } from './Marks.jsx';
import '../styles/marks.css';
import '../styles/access.css';

// The Access section of a service's and a job's overview: who the
// workload is (its SPIFFE ID and when its pods' certificates end, and,
// unfolded, what the certificate names and who reads which part), then
// what it reaches with a key or without one, one row per target, sorted
// by where the key lives (access.js): by identity (an addon that admits
// it by its certificate), offloaded (an HTTP binding, the registry
// login the node pulls with), in the container (a variable from a
// secret, an addon's password). A binding's row has its numbers of the
// last 24 h. Never a key: names, versions and counts only.

const Link = EnvLink;

const SPEC_KEY = { service: 'services', job: 'jobs' };

// ObjectAccess reads what the section shows beside the spec: the
// catalog, the environment's credentials, its workloads' identities and,
// when the object names a binding, the bindings' use.
export function ObjectAccess({ tenant, st, kind, name, spec, navigate }) {
  const env = st.name;
  const so = spec && spec[SPEC_KEY[kind]] ? spec[SPEC_KEY[kind]][name] : null;
  const named = !!(so && Array.isArray(so.bindings) && so.bindings.length > 0);
  const catalog = useAddonCatalog();
  const creds = useEnvCredentials(env);
  const ids = useIdentities(env);
  const usage = useBindingUsage(env, named);
  return (
    <AccessCard tenant={tenant} env={env} kind={kind} name={name} spec={spec} loading={!spec && st.deployment > 0}
      catalog={catalog} credentials={creds.data} identities={ids.data} identitiesErr={ids.err} usage={usage}
      navigate={navigate} />
  );
}

// AccessCard is the section, its data given: catalog, credentials,
// identities and usage null while they load.
export function AccessCard({
  tenant, env, kind, name, spec, loading = false, catalog = null, credentials = null, identities = null, identitiesErr = '',
  usage = null, now = Date.now(), navigate,
}) {
  const rows = spec ? accessOf(spec, SPEC_KEY[kind], name, { catalog, credentials: credentials || [] }) : [];
  const bindings = rows.some((r) => r.kind === 'binding');
  return (
    <section className="card access-card">
      <div className="card-head">
        <h2>Access</h2><span className="muted">where its keys live</span>
        <Link to={secretsPath(tenant, env)} navigate={navigate} className="card-more">Secrets</Link>
      </div>
      <IdentityLine workload={workloadOf(identities, kind, name)} identities={identities} err={identitiesErr} now={now}
        readers={identityReaders(rows, catalog)} />
      {loading && <p className="muted">Loading…</p>}
      {!loading && rows.length === 0 && (
        <p className="muted acc-none">It names no secret, binding or addon: it holds no key and needs none.</p>)}
      {rows.length > 0 && (
        <div className="tblwrap">
          <table className="tbl acc-tbl">
            <tbody>
              {rows.map((r) => (
                <AccessRow key={`${r.kind}/${r.target.name}`} r={r} tenant={tenant} env={env} item={`${kind}/${name}`} spec={spec}
                  credentials={credentials} catalog={catalog} usage={usage} navigate={navigate} />))}
            </tbody>
          </table>
        </div>)}
      {bindings && usage && usage.unavailable && <p className="ov-note">No numbers of the last 24 h: {usage.unavailable}</p>}
    </section>
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
// mark of where the key lives, the key's source or the fields, and a
// binding's numbers.
function AccessRow({ r, tenant, env, item, spec, credentials, catalog, usage, navigate }) {
  const secret = (n) => (
    <Link to={secretsPath(tenant, env, 'secret', n)} navigate={navigate} className="mono" title="its row in the environment's secrets">{n}</Link>);
  let what = null;
  let to = null;
  let detail = null;
  let nums = null;
  if (r.kind === 'binding') {
    const b = r.binding;
    what = (
      <Link to={secretsPath(tenant, env, 'binding', b.name)} navigate={navigate} className="mono strong"
        title="its row in the environment's secrets, with its check">{b.name}</Link>);
    to = (
      <>
        <span className="acc-arrow">→</span><span className="mono host">{hostOf(b.upstream) || '…'}</span>{' '}
        <span className="muted small">{b.mode === 'intercept' ? 'at its own name, intercepted' : <>reached as <span className="mono">{b.reachedAs}</span></>}</span>
      </>);
    detail = <BindingKey b={b} tenant={tenant} env={env} credentials={credentials} navigate={navigate} secret={secret} />;
    nums = (
      <BindingNumbers u={usageOf(usage, b.name)} tenant={tenant} env={env} navigate={navigate}
        logs={objectPath(tenant, env, item, 'logs', null, queryOf({ source: 'egress', since: '1d' }))} />);
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

// BindingKey is a binding's credential: its kind and where it comes
// from, a credential of the environment at its pinned version or the
// environment's secrets.
function BindingKey({ b, tenant, env, credentials, navigate, secret }) {
  const kind = kindShort(b.kind);
  const c = b.credential ? (credentials || []).find((x) => x && x.name === b.credential.name) : null;
  return (
    <>
      {kind && <span>{kind}</span>}
      {b.credential && (
        <>
          {kind && ' · '}credential{' '}
          <Link to={credentialPath(tenant, env, b.credential.name)} navigate={navigate} className="mono">{b.credential.name}</Link>
          {b.credential.version > 0 && <span className="mono"> v{b.credential.version}</span>}
          {c && b.credential.version > 0 && c.version > b.credential.version && (
            <span className="warntext"> · latest v{c.version}</span>)}
        </>)}
      {!b.credential && b.secrets.length > 0 && (
        <>
          {kind && ' · '}secret{b.secrets.length === 1 ? '' : 's'}{' '}
          {b.secrets.map((n, i) => <Fragment key={n}>{i > 0 && ', '}{secret(n)}</Fragment>)}
        </>)}
    </>
  );
}
