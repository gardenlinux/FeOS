// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { Ago, ArmButton, ErrBox, LiveBadge, Secret, useArm, useNow, usePoll } from '../components.jsx';
import { fmtDate, fmtDateTime } from '../format.js';
import { joinTokenState } from './fleet.js';
import { JOIN_KINDS, joinKindOf, joinTokenBody, joinTokenTitle } from './platformData.js';
import { Fact, PageHead } from './platformParts.jsx';

// Join tokens: how a node or an ingress instance joins. A token is used
// once; pinned to a name, it lets that name join again (a re-join
// replaces the identity), unpinned it joins a new name only. It is shown
// once, when it is made; the list holds no secrets. ?kind=ingress picks
// the kind of the form.

export const TTLS = [
  { label: 'default', seconds: 0 },
  { label: '10 min', seconds: 600 },
  { label: '1 h', seconds: 3600 },
  { label: '1 day', seconds: 86400 },
  { label: '7 days', seconds: 7 * 86400 },
];

// JoinHow says what to do with a fresh token of a kind.
export function JoinHow({ kind }) {
  if (kind === 'ingress') {
    return (
      <p className="pf-how">
        On Kubernetes, put it into the Secret <code>ferun-ingress-join</code> under the key of the instance's pod
        name; the pod copies its own at its start, so a running pod needs a restart. Elsewhere, write it
        to <code>ferun-ingress serve</code>'s <code>--join-token-file</code>. It carries the pin of the platform's
        root certificate, so that the instance can check whom it joins.
      </p>
    );
  }
  return (
    <p className="pf-how">
      Give it to the node's feosd for its join; on Kubernetes, <code>feos-k8s</code> makes the nodes' tokens itself.
      It carries the pin of the platform's root certificate, so that the node can check whom it joins.
    </p>
  );
}

// JoinTokenForm makes a join token of a kind, pinned to a name or not,
// and shows it once. fixedKind hides the kind's choice (the ingresses'
// page makes ingress tokens only).
export function JoinTokenForm({ initialKind = 'node', fixedKind = '', onMade }) {
  const [kind, setKind] = useState(fixedKind || initialKind);
  const [name, setName] = useState('');
  const [ttl, setTtl] = useState(0);
  const [fresh, setFresh] = useState(null);
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);

  return (
    <>
      <form className="pf-form" onSubmit={async (e) => {
        e.preventDefault();
        setBusy(true);
        try {
          const res = await api.createJoinToken(joinTokenBody(kind, name, ttl));
          setFresh({ kind, ...res });
          setName('');
          setErr('');
          if (onMade) await onMade();
        } catch (e2) {
          setErr(e2.message);
        } finally {
          setBusy(false);
        }
      }}>
        {!fixedKind && (
          <span className="seg" role="group" aria-label="kind">
            {JOIN_KINDS.map((k) => (
              <button key={k} type="button" className={kind === k ? 'on' : ''} onClick={() => setKind(k)}>
                <Icon name={k === 'node' ? 'server' : 'route'} size={13} /> {k}</button>))}
          </span>)}
        <input className="mono" value={name} onChange={(e) => setName(e.target.value)}
          placeholder={kind === 'ingress' ? 'ingress name (empty: any new name)' : 'node name (empty: any new name)'}
          title="pinned to a name, the token joins that name, again too; unpinned, a new name only" />
        <select value={ttl} onChange={(e) => setTtl(Number(e.target.value))} title="how long the token is valid">
          {TTLS.map((t) => <option key={t.seconds} value={t.seconds}>valid {t.label}</option>)}
        </select>
        <button type="submit" className="primary" disabled={busy}><Icon name="plus" size={14} /> Make a join token</button>
      </form>
      <ErrBox error={err} />
      {fresh && (
        <Secret value={fresh.token} title={joinTokenTitle(fresh)} onDismiss={() => setFresh(null)}>
          <p className="muted small">Valid until {fmtDateTime(fresh.expiresAt)}, for one join.</p>
          <JoinHow kind={fresh.kind} />
        </Secret>
      )}
    </>
  );
}

export function JoinTokens() {
  const [tokens, setTokens] = useState(null);
  const [err, setErr] = useState('');
  const [armed, arm, disarm] = useArm();
  const [initialKind] = useState(() => joinKindOf(typeof location !== 'undefined' ? location.search : ''));
  useNow(5000);

  const refresh = useCallback(async () => {
    try {
      setTokens((await api.listJoinTokens()).joinTokens || []);
      setErr('');
    } catch (e) {
      setErr(e.message);
    }
  }, []);
  const lastOk = usePoll(refresh, 10000);
  const open = (tokens || []).filter((t) => joinTokenState(t) === 'open');

  return (
    <div className="platform">
      <PageHead title="Join tokens" state={tokens && <LiveBadge lastOk={lastOk} />}
        facts={tokens && <>
          <Fact n={open.length}>open</Fact>
          <Fact n={open.filter((t) => (t.kind || 'node') === 'node').length}>for nodes</Fact>
          <Fact n={open.filter((t) => t.kind === 'ingress').length}>for ingresses</Fact>
          <Fact n={tokens.length - open.length}>used or expired</Fact>
        </>} />

      <section className="pane">
        <div className="pane-toolbar"><h2><Icon name="plus" size={14} /> Make one</h2></div>
        <JoinTokenForm initialKind={initialKind} onMade={refresh} />
      </section>

      <section className="pane">
        <div className="pane-toolbar"><h2><Icon name="ticket" size={14} /> Tokens <span className="count">{tokens ? tokens.length : '…'}</span></h2></div>
        <ErrBox error={err} />
        <JoinTokensTable tokens={tokens || []} armed={armed} arm={arm} disarm={disarm}
          onRevoke={async (t) => {
            try {
              await api.revokeJoinToken(t.id);
              await refresh();
            } catch (e) {
              setErr(e.message);
            }
          }} />
        {tokens && tokens.length === 0 && <p className="empty">No join tokens.</p>}
        {!tokens && !err && <p className="empty">Loading…</p>}
      </section>
    </div>
  );
}

// JoinTokensTable lists the join tokens, the open ones with a revoke.
export function JoinTokensTable({ tokens, armed, arm, disarm, onRevoke }) {
  return (
    <div className="tblwrap"><table className="tbl">
      <thead>
        <tr>
          <th>ID</th><th>kind</th><th>name</th><th>made by</th><th className="col-when">made</th><th>expires</th><th>use</th><th className="col-quick" />
        </tr>
      </thead>
      <tbody>
        {tokens.map((t) => {
          const st = joinTokenState(t);
          const kind = t.kind || 'node';
          return (
            <tr key={t.id} className={st !== 'open' ? 'dim' : ''}>
              <td className="mono small" title={t.id}>{t.id.slice(0, 8)}</td>
              <td><span className={'kindtag ' + kind}>{kind}</span></td>
              <td className="mono">{t.node || <span className="muted">any new name</span>}</td>
              <td className="small">{t.createdBy || '—'}</td>
              <td className="col-when muted">{fmtDate(t.createdAt)}</td>
              <td className="small">{st === 'expired' ? <span className="muted">expired</span> : <Ago iso={t.expiresAt} />}</td>
              <td className="small">
                {t.usedAt
                  ? <>used by <span className="mono">{t.usedName || '?'}</span> <Ago iso={t.usedAt} />
                    {t.usedPeer && <div className="muted mono">{t.usedPeer}</div>}</>
                  : <span className="muted">unused</span>}
              </td>
              <td className="col-quick">
                {st === 'open' && (
                  <ArmButton armKey={t.id} armed={armed} arm={arm} disarm={disarm} icon="trash"
                    className="mini" confirmLabel="revoke?" onConfirm={() => onRevoke(t)}>Revoke</ArmButton>
                )}
              </td>
            </tr>
          );
        })}
      </tbody>
    </table></div>
  );
}
