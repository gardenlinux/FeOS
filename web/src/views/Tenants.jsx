// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useEffect, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { Ago, ArmButton, ErrBox, LiveBadge, Secret, useArm, useNow, usePoll } from '../components.jsx';
import { fmtDate } from '../format.js';
import { useMe } from '../people.js';
import { tenantDeleteText, tokenRows } from './fleet.js';

// The operators' tenants: every tenant, create, delete, and their tokens.
// ferund stores a tenant's tokens as hashes only, so a token is shown
// once, when it is made, for a client such as FeCode; the browser keeps
// none. An operator works in a tenant with the session, as its admin.

export function Tenants({ navigate }) {
  const { reload } = useMe();
  const [tenants, setTenants] = useState(null);
  const [err, setErr] = useState('');
  const [name, setName] = useState('');
  const [showCreate, setShowCreate] = useState(false);
  const [created, setCreated] = useState(null); // {name, token}
  const [open, setOpen] = useState('');
  const [armed, arm, disarm] = useArm();
  useNow(5000);

  const refresh = useCallback(async () => {
    try {
      setTenants((await api.listTenants()).tenants || []);
      setErr('');
    } catch (e) {
      setErr(e.message);
    }
  }, []);
  const lastOk = usePoll(refresh, 5000);

  const go = (t) => navigate(`/tenants/${encodeURIComponent(t)}`);

  return (
    <div className="tenants single">
      <section className="pane">
        <div className="pane-toolbar">
          <h2><Icon name="users" size={15} /> Tenants <span className="count">{tenants ? tenants.length : '…'}</span></h2>
          <span className="spacer" />
          <LiveBadge lastOk={lastOk} />
          <button className={'ghost mini' + (showCreate ? ' on' : '')} onClick={() => setShowCreate(!showCreate)}>
            <Icon name="plus" size={13} /> new
          </button>
        </div>

        {showCreate && (
          <form className="createpanel" onSubmit={async (e) => {
            e.preventDefault();
            try {
              const n = name.trim();
              const res = await api.createTenant(n);
              setCreated({ name: n, token: res.token });
              reload().catch(() => {});
              setName('');
              setShowCreate(false);
              await refresh();
            } catch (e2) {
              setErr(e2.message);
            }
          }}>
            <input placeholder="tenant name: lowercase letters, digits, '.' and '-'" value={name} autoFocus
              onChange={(e) => setName(e.target.value)} />
            <button type="submit" className="primary" disabled={!name.trim()}><Icon name="plus" size={14} /> Create</button>
            <button type="button" className="ghost" onClick={() => setShowCreate(false)}><Icon name="x" size={14} /></button>
          </form>
        )}
        {created && (
          <Secret value={created.token} title={`Token of tenant ${created.name}`} onDismiss={() => setCreated(null)}>
            <p className="muted small">For a client such as FeCode; this browser keeps it not.</p>
          </Secret>
        )}
        <ErrBox error={err} />

        <div className="tblwrap"><table className="tbl">
          <thead>
            <tr>
              <th>name</th>
              <th className="col-num">sandboxes</th>
              <th className="col-num">tokens</th>
              <th className="col-when">created</th>
              <th className="col-quick" />
            </tr>
          </thead>
          <tbody>
            {(tenants || []).map((t) => {
              const del = tenantDeleteText(t);
              return (
                <React.Fragment key={t.name}>
                  <tr className={'clickable' + (open === t.name ? ' sel' : '')} onClick={() => go(t.name)}
                    title="work in this tenant">
                    <td className="strong">
                      {t.name}
                      {t.deleting && <span className="badge warn">being deleted</span>}
                    </td>
                    <td className="col-num">{t.sandboxCount}</td>
                    <td className="col-num">{(t.tokens || []).length}</td>
                    <td className="col-when muted">{fmtDate(t.createdAt)}</td>
                    <td className="col-quick" onClick={(e) => e.stopPropagation()}>
                      <button className={'ghost mini' + (open === t.name ? ' on' : '')} title="the tenant's tokens"
                        onClick={() => setOpen(open === t.name ? '' : t.name)}>
                        <Icon name="key" size={13} /> tokens
                      </button>
                      <button className="iconbtn mini" title="work in this tenant" onClick={() => go(t.name)}>
                        <Icon name="chevronRight" size={13} /></button>
                      <ArmButton armKey={t.name} armed={armed} arm={arm} disarm={disarm} icon="trash"
                        className="mini" confirmLabel={del.confirm} title={del.title}
                        onConfirm={async () => {
                          try {
                            await api.deleteTenant(t.name);
                            await refresh();
                            reload().catch(() => {});
                          } catch (e) {
                            setErr(e.message);
                          }
                        }}>Delete</ArmButton>
                    </td>
                  </tr>
                  {open === t.name && (
                    <tr className="subrow">
                      <td colSpan={5}><TenantTokens tenant={t.name} onChange={refresh} /></td>
                    </tr>
                  )}
                </React.Fragment>
              );
            })}
          </tbody>
        </table></div>
        {tenants && tenants.length === 0 && <p className="empty">No tenants yet.</p>}
        {!tenants && !err && <p className="empty">Loading…</p>}

        <p className="hint">
          A tenant's token acts in the tenant as its admin, for a client such as FeCode's backend; people work in it
          with their sign-in and the grants its admins give. Deleting a tenant revokes its tokens at once and deletes
          all its sandboxes and environments.
        </p>
      </section>
    </div>
  );
}

// TenantTokens lists, mints and revokes a tenant's tokens. A minted token
// is shown once.
function TenantTokens({ tenant, onChange }) {
  const [tokens, setTokens] = useState(null);
  const [label, setLabel] = useState('');
  const [fresh, setFresh] = useState('');
  const [err, setErr] = useState('');
  const [armed, arm, disarm] = useArm();

  const refresh = useCallback(async () => {
    try {
      setTokens((await api.listTenantTokens(tenant)).tokens || []);
      setErr('');
    } catch (e) {
      setErr(e.message);
    }
  }, [tenant]);
  useEffect(() => { refresh(); }, [refresh]);

  const rows = tokenRows(tokens);

  return (
    <div className="subpanel">
      <ErrBox error={err} />
      {fresh && (
        <Secret value={fresh} title={`New token of tenant ${tenant}`} onDismiss={() => setFresh('')} />
      )}
      <div className="tblwrap"><table className="tbl">
        <thead>
          <tr><th>hash</th><th>label</th><th>role</th><th className="col-when">created</th><th className="col-when">last used</th><th className="col-quick" /></tr>
        </thead>
        <tbody>
          {rows.map((t) => (
            <tr key={t.hash}>
              <td className="mono muted" title={t.hash}>{t.hashPrefix}…</td>
              <td>{t.label || <span className="muted">(no label)</span>}</td>
              <td className="col-when muted">{fmtDate(t.createdAt)}</td>
              <td className="col-when">{t.lastUsedAt ? <Ago iso={t.lastUsedAt} /> : <span className="muted">never</span>}</td>
              <td className="small">{t.role || 'admin'}{t.environmentName ? ` on ${t.environmentName}` : ''}</td>
              <td className="col-quick">
                <ArmButton armKey={t.hash} armed={armed} arm={arm} disarm={disarm} icon="trash"
                  className="mini" confirmLabel="revoke?"
                  onConfirm={async () => {
                    try {
                      await api.revokeTenantToken(tenant, t.hash);
                      await refresh();
                      onChange();
                    } catch (e) {
                      setErr(e.message);
                    }
                  }}>Revoke</ArmButton>
              </td>
            </tr>
          ))}
        </tbody>
      </table></div>
      {tokens && tokens.length === 0 && <p className="empty">No tokens: nothing can act as this tenant.</p>}
      <form className="createpanel" onSubmit={async (e) => {
        e.preventDefault();
        try {
          const res = await api.createTenantToken(tenant, label.trim());
          setFresh(res.token);
          setLabel('');
          await refresh();
          onChange();
        } catch (e2) {
          setErr(e2.message);
        }
      }}>
        <input placeholder="label (e.g. fecode)" value={label}
          onChange={(e) => setLabel(e.target.value)} />
        <button type="submit" className="primary"><Icon name="key" size={14} /> Mint a token</button>
      </form>
    </div>
  );
}
