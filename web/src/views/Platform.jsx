// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { Ago, ArmButton, CopyBtn, ErrBox, LiveBadge, useArm, useNow, usePoll } from '../components.jsx';
import { fmtDateTime } from '../format.js';
import { useMe } from '../people.js';
import { JoinTokenForm } from './JoinTokens.jsx';
import { certTone, ingressRows, issuerHost, peopleRows, seenWithin } from './platformData.js';
import { Dot, DotWord, Fact, PageHead } from './platformParts.jsx';

// The platform's ingresses and people (operators).

// -------------------------------------------------------------- ingresses --

// Ingresses are the instances of ferun-ingress (ADR 0021): their link,
// version, place on the pods' mesh and certificate; one is removed with
// two clicks, and one is added with a join token of kind ingress.
export function Ingresses({ navigate }) {
  const [list, setList] = useState(null);
  const [err, setErr] = useState('');
  const [adding, setAdding] = useState(false);
  const [armed, arm, disarm] = useArm();
  useNow(5000);

  const load = useCallback(async () => {
    try {
      setList((await api.listIngresses()).ingresses || []);
      setErr('');
    } catch (e) {
      setErr(e.message);
    }
  }, []);
  const lastOk = usePoll(load, 5000);

  const rows = ingressRows(list);
  const live = rows.filter((g) => g.status !== 'removed');
  const connected = live.filter((g) => g.connected).length;
  const versions = [...new Set(live.map((g) => g.version).filter(Boolean))];
  const tokens = (e) => { e.preventDefault(); navigate('/join-tokens?kind=ingress'); };

  return (
    <div className="platform">
      <PageHead title="Ingresses"
        state={list && <LiveBadge lastOk={lastOk} />}
        actions={<button className={'primary' + (adding ? ' on' : '')} onClick={() => setAdding(!adding)}>
          <Icon name="plus" size={14} /> Add an ingress</button>}
        facts={list && <>
          <Fact n={live.length}>instance{live.length === 1 ? '' : 's'}</Fact>
          <Fact className={connected < live.length ? 'warn' : 'ok'} n={connected}>connected</Fact>
          {rows.length > live.length && <Fact n={rows.length - live.length}>removed</Fact>}
          {versions.length > 0 && (
            <span className={versions.length > 1 ? 'warn' : ''}>ferun-ingress <span className="mono">{versions.join(', ')}</span></span>)}
        </>} />

      {adding && (
        <section className="pane">
          <div className="pane-toolbar">
            <h2><Icon name="ticket" size={14} /> Add an ingress</h2>
            <span className="spacer" />
            <button className="ghost mini" onClick={() => setAdding(false)}><Icon name="x" size={13} /></button>
          </div>
          <p className="hint" style={{ marginTop: 0, marginBottom: 8 }}>
            An instance joins with a one-time join token of kind <code>ingress</code>. Pinned to a name, the token
            joins that name, a removed one again too; unpinned, the instance joins a new name.
          </p>
          <JoinTokenForm fixedKind="ingress" onMade={load} />
          <p className="small"><a href="/join-tokens?kind=ingress" onClick={tokens}>every join token →</a></p>
        </section>)}

      <section className="pane">
        <ErrBox error={err} />
        <IngressTable rows={rows} armed={armed} arm={arm} disarm={disarm}
          onRemove={async (g) => {
            try {
              await api.removeIngress(g.name);
            } catch (e) {
              setErr(e.message);
            }
            await load();
          }} />
        {list && rows.length === 0 && (
          <p className="empty">No ingress instance has joined: no public environment is reachable.{' '}
            <a href="/join-tokens?kind=ingress" onClick={(e) => { e.preventDefault(); setAdding(true); }}>Add one →</a></p>)}
        {!list && !err && <p className="empty">Loading…</p>}
        <p className="muted small footnote">
          An instance is <code>connected</code> when it was seen within 30 s (the database's clock); <code>left</code> when it
          stopped cleanly and was not seen since. It serves its last table and certificates through an outage of
          ferund. Removing one revokes its certificates and closes its connections on every instance; it leaves the
          pods' mesh and the security group <code>ferun-ingress</code>. A token pinned to its name joins it again.
        </p>
      </section>
    </div>
  );
}

// IngressTable lists the ingresses of ingressRows, a remove on each that
// is not removed.
export function IngressTable({ rows, armed, arm, disarm, onRemove }) {
  return (
    <div className="tblwrap"><table className="tbl">
      <thead>
        <tr>
          <th className="col-state" />
          <th>name</th><th>state</th><th>version</th><th>mesh address</th><th>last seen</th><th>certificate</th>
          <th className="col-when">joined</th><th className="col-quick" />
        </tr>
      </thead>
      <tbody>
        {rows.map((g) => {
          const tone = certTone(g.certNotAfter);
          return (
            <tr key={g.name} className={g.status === 'removed' ? 'dim' : ''}>
              <td className="col-state"><Dot cls={g.state.cls} title={g.state.word} /></td>
              <td className="mono strong">{g.name}</td>
              <td><DotWord st={g.state} /><div className="muted small">{g.state.say}</div></td>
              <td className="mono small">{g.version || '—'}</td>
              <td className="mono small">
                {g.meshAddress ? <span className="net">{g.meshAddress}</span> : <span className="muted">—</span>}
                {g.meshEndpoint && <div className="muted" title="its WireGuard endpoint">{g.meshEndpoint}</div>}
                {g.admittedVersion > 0 && <div className="muted" title="the version of the security group ferun-ingress at which its address entered it">group v{g.admittedVersion}</div>}
              </td>
              <td className="small">{g.lastSeen ? <Ago iso={g.lastSeen} /> : <span className="muted">never</span>}
                {g.leftAt && <div className="muted">left <Ago iso={g.leftAt} /></div>}</td>
              <td className="small nowrap">
                {g.status === 'removed'
                  ? <span className="muted">revoked</span>
                  : g.certNotAfter
                    ? <span className={tone} title={fmtDateTime(g.certNotAfter)}>{tone === 'bad' ? 'expired ' : 'until '}<Ago iso={g.certNotAfter} /></span>
                    : <span className="muted">—</span>}
              </td>
              <td className="col-when small">
                {g.status === 'removed' ? <>removed <Ago iso={g.removedAt} /></> : <Ago iso={g.joinedAt} />}
              </td>
              <td className="col-quick">
                {g.publicKey && <CopyBtn text={g.publicKey} title="copy its WireGuard key" mini />}
                {g.status !== 'removed' && (
                  <ArmButton armKey={'rm:' + g.name} armed={armed} arm={arm} disarm={disarm} icon="trash" className="mini"
                    title="revoke its certificates and close its connections; it leaves the mesh"
                    confirmLabel="remove?" onConfirm={() => onRemove(g)}>Remove</ArmButton>)}
              </td>
            </tr>
          );
        })}
      </tbody>
    </table></div>
  );
}

// ----------------------------------------------------------------- people --

const STATES = [
  { key: 'all', label: 'everyone' },
  { key: 'active', label: 'active' },
  { key: 'disabled', label: 'disabled' },
];

// PlatformPeople is everyone who signed in (ADR 0031), the last seen
// first: an operator disables or enables a person, with two clicks. A
// disabled person's sessions end at once and they sign in no more; their
// grants stay, so enabling restores what they had.
export function PlatformPeople() {
  const { me } = useMe();
  const [people, setPeople] = useState(null);
  const [err, setErr] = useState('');
  const [search, setSearch] = useState('');
  const [state, setState] = useState('all');
  const [armed, arm, disarm] = useArm();
  const now = useNow(10000);

  const load = useCallback(async () => {
    try {
      setPeople((await api.listPeople()).people || []);
      setErr('');
    } catch (e) {
      setErr(e.message);
    }
  }, []);
  const lastOk = usePoll(load, 15000);

  const all = people || [];
  const disabled = all.filter((p) => p.disabledAt).length;
  const rows = peopleRows(all, search, state);
  const toggle = async (p) => {
    try {
      const next = p.disabledAt ? await api.enablePerson(p.id) : await api.disablePerson(p.id);
      setPeople((cur) => (cur || []).map((x) => (x.id === p.id ? { ...x, ...next } : x)));
      setErr('');
    } catch (e) {
      setErr(e.message);
    }
    await load();
  };

  return (
    <div className="platform">
      <PageHead title="People" state={people && <LiveBadge lastOk={lastOk} />}
        facts={people && <>
          <Fact n={all.length}>{all.length === 1 ? 'person' : 'people'}</Fact>
          <Fact n={seenWithin(all, 7 * 24 * 3600 * 1000, now)}>seen in the last 7 days</Fact>
          <Fact className={disabled ? 'warn' : ''} n={disabled}>disabled</Fact>
        </>} />

      <section className="pane">
        <div className="pane-toolbar">
          <div className="pf-filter">
            <input placeholder="search name, e-mail, issuer" value={search} onChange={(e) => setSearch(e.target.value)} />
            <span className="seg" role="group" aria-label="state">
              {STATES.map((s) => (
                <button key={s.key} className={state === s.key ? 'on' : ''} onClick={() => setState(s.key)}>{s.label}</button>))}
            </span>
          </div>
        </div>
        <ErrBox error={err} />
        <PeopleTable rows={rows} me={me} armed={armed} arm={arm} disarm={disarm} onToggle={toggle} />
        {people && rows.length === 0 && <p className="empty">{all.length ? 'No one matches.' : 'No one has signed in yet.'}</p>}
        {!people && !err && <p className="empty">Loading…</p>}
        <p className="muted small footnote">
          A person is whoever signed in once, by their issuer and subject; e-mail and name are for display. Disabling
          a person ends every session of theirs at once, and they sign in no more (they see "Your account is
          disabled"); their grants stay, so enabling them restores what they had. A person is never deleted.
        </p>
      </section>
    </div>
  );
}

// PeopleTable lists people with a disable or an enable, both armed; the
// operator's own row cannot be disabled here, since it ends their
// session.
export function PeopleTable({ rows, me, armed, arm, disarm, onToggle }) {
  return (
    <div className="tblwrap"><table className="tbl">
      <thead>
        <tr>
          <th className="col-state" /><th>person</th><th>issuer</th><th className="col-when">first seen</th>
          <th className="col-when">last seen</th><th>state</th><th className="col-quick" />
        </tr>
      </thead>
      <tbody>
        {rows.map((p) => {
          const self = me && me.person === p.id;
          const off = !!p.disabledAt;
          return (
            <tr key={p.id} className={off ? 'disabled' : ''}>
              <td className="col-state"><Dot cls={off ? 'failed' : 'running'} title={off ? 'disabled' : 'active'} /></td>
              <td>
                <span className="strong">{p.name || p.email || p.id.slice(0, 8)}</span>
                {self && <span className="badge acc">you</span>}
                {p.email && p.name && <div className="muted small">{p.email}</div>}
              </td>
              <td className="small">
                <span className="mono" title={p.issuer}>{issuerHost(p.issuer)}</span>
                <div className="muted mono" title="the subject at the issuer">{p.sub}</div>
              </td>
              <td className="col-when small"><Ago iso={p.firstSeen} /></td>
              <td className="col-when small"><Ago iso={p.lastSeen} /></td>
              <td className="small">
                {off ? <span className="bad" title={fmtDateTime(p.disabledAt)}>disabled <Ago iso={p.disabledAt} /></span> : <span className="ok">active</span>}
              </td>
              <td className="col-quick">
                {off
                  ? (
                    <ArmButton armKey={'p:' + p.id} armed={armed} arm={arm} disarm={disarm} className="mini enable"
                      title="they can sign in again, with the grants they had" confirmLabel="enable?" onConfirm={() => onToggle(p)}>
                      Enable</ArmButton>)
                  : (
                    <ArmButton armKey={'p:' + p.id} armed={armed} arm={arm} disarm={disarm} className="mini" disabled={self}
                      title={self ? 'that is you: it would end your own session' : 'end their sessions at once; they sign in no more'}
                      confirmLabel="disable? signs them out" onConfirm={() => onToggle(p)}>Disable</ArmButton>)}
              </td>
            </tr>
          );
        })}
      </tbody>
    </table></div>
  );
}
