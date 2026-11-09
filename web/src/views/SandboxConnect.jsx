// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useEffect, useState } from 'react';
import { api, mcpURL } from '../api.js';
import { Icon } from '../Icon.jsx';
import { Ago, ArmButton, CopyBtn, ErrBox, Secret, useArm } from '../components.jsx';
import { fmtDate } from '../format.js';
import { connectSnippets, TOKEN_VAR } from './sandboxes.js';
import { ToolReference } from './Tools.jsx';

// A sandbox's Connect tab: what an agent needs to work in it, on one
// page. A token of the sandbox (shown once, ferund keeps its hash), the
// MCP address, ready snippets with the token in them, the tokens minted
// so far, and the tools' reference. fresh is a token the create answered,
// shown once here; onFreshSeen forgets it.

export function Connect({ sandboxId, write, fresh = '', onFreshSeen = () => {} }) {
  const [tokens, setTokens] = useState(null);
  const [label, setLabel] = useState('');
  const [minted, setMinted] = useState(fresh ? { token: fresh, label: 'the first token, from the create' } : null);
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);
  const [armed, arm, disarm] = useArm();
  const why = write ? undefined : 'needs write on its environment';

  const refresh = useCallback(async () => {
    try {
      setTokens((await api.listTokens(sandboxId)).tokens || []);
      setErr('');
    } catch (e) {
      setErr(e.message);
    }
  }, [sandboxId]);
  useEffect(() => { refresh(); }, [refresh]);
  // The create's token is the page's now; the parent forgets it.
  useEffect(() => { if (fresh) onFreshSeen(); }, [fresh, onFreshSeen]);

  const mint = async (e) => {
    e.preventDefault();
    setBusy(true);
    try {
      const res = await api.createToken(sandboxId, label.trim());
      setMinted({ token: res.token, label: label.trim() });
      setLabel('');
      await refresh();
    } catch (e2) {
      setErr(e2.message);
    } finally {
      setBusy(false);
    }
  };

  const mcp = mcpURL();
  const origin = globalThis.location ? globalThis.location.origin : '';
  const snip = connectSnippets({ id: sandboxId, mcp, origin, token: minted ? minted.token : '' });

  return (
    <div className="sbx-connect">
      <ErrBox error={err} />

      <section className="sbx-step">
        <h3><span className="sbx-stepno">1</span> A token for the agent</h3>
        <p className="muted small">
          A sandbox token reaches this sandbox only: its tools over MCP and REST, its ports. It is shown once; ferund keeps
          its hash. Mint one per agent or machine, so that each can be revoked alone.
        </p>
        {minted && (
          <Secret value={minted.token} title={`Token for sandbox ${sandboxId}${minted.label ? ` · ${minted.label}` : ''}`}
            onDismiss={() => setMinted(null)}>
            <p className="muted small sbx-once-note">The snippets below carry it until you close this.</p>
          </Secret>)}
        <form className="createpanel" onSubmit={mint}>
          <input placeholder="label (e.g. laptop, claude code, review-bot)" value={label} disabled={!write} title={why}
            onChange={(e) => setLabel(e.target.value)} />
          <button type="submit" className="primary" disabled={!write || busy} title={why}>
            <Icon name="key" size={14} /> Mint a token</button>
        </form>
      </section>

      <section className="sbx-step">
        <h3><span className="sbx-stepno">2</span> Point the agent at it</h3>
        <p className="muted small">
          One MCP address for every sandbox: the token says which.
          {!minted && <> The snippets read the token from <code className="mono">{TOKEN_VAR}</code>.</>}
        </p>
        <div className="snippet"><pre>{mcp}</pre><CopyBtn text={mcp} title="copy the MCP address" /></div>
        <h4 className="sbx-snip-h">Claude Code</h4>
        <div className="snippet"><pre>{snip.claude}</pre><CopyBtn text={snip.claude} /></div>
        <h4 className="sbx-snip-h">An MCP client's JSON (<code className="mono">.mcp.json</code>, editors)</h4>
        <div className="snippet"><pre>{snip.mcpJson}</pre><CopyBtn text={snip.mcpJson} /></div>
        <h4 className="sbx-snip-h">REST: run a command</h4>
        <div className="snippet"><pre>{snip.curl}</pre><CopyBtn text={snip.curl} /></div>
        <p className="muted small">
          A call resumes the sandbox when it is paused or hibernated and keeps it running while it runs. <code className="mono">timeout</code> is
          in seconds (120 by default).
        </p>
      </section>

      <section className="sbx-step">
        <h3><span className="sbx-stepno">3</span> Tokens of this sandbox <span className="count">{tokens ? tokens.length : '…'}</span></h3>
        <div className="tblwrap"><table className="tbl">
          <thead>
            <tr><th>hash</th><th>label</th><th className="col-when">created</th><th className="col-when">last used</th><th className="col-quick" /></tr>
          </thead>
          <tbody>
            {(tokens || []).map((t) => (
              <tr key={t.hash}>
                <td className="mono muted" title={t.hash}>{t.hashPrefix}…</td>
                <td>{t.label || <span className="muted">(no label)</span>}</td>
                <td className="col-when muted">{fmtDate(t.createdAt)}</td>
                <td className="col-when">{t.lastUsedAt ? <Ago iso={t.lastUsedAt} /> : <span className="muted">never</span>}</td>
                <td className="col-quick">
                  <ArmButton armKey={t.hash} armed={armed} arm={arm} disarm={disarm} icon="trash"
                    className="mini" confirmLabel="revoke?" disabled={!write} title={why || 'revoke: the agent with it is cut off'}
                    onConfirm={() => api.revokeToken(sandboxId, t.hash).then(refresh, (e) => setErr(e.message))}>Revoke</ArmButton>
                </td>
              </tr>
            ))}
          </tbody>
        </table></div>
        {tokens && tokens.length === 0 && <p className="empty">No tokens. Mint one above.</p>}
      </section>

      <section className="sbx-step">
        <ToolReference />
      </section>
    </div>
  );
}
