// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useState } from 'react';
import { auth, loginURL } from '../api.js';
import { Icon } from '../Icon.jsx';
import { BrandMark } from '../components.jsx';

// The sign-in screen. With OIDC, the person types their address and goes
// to the provider through /auth/login with it as login_hint: the provider
// asks for the password, or sends them to their company's sign-in. The
// browser remembers the address for next time. With Basic (--admin-users),
// a user and password, kept for this tab only. ferund's /auth/config says
// which of them it offers.

const EMAIL_KEY = 'ferun-email';

function storedEmail() {
  try { return localStorage.getItem(EMAIL_KEY) || ''; } catch { return ''; }
}

function rememberEmail(v) {
  try { localStorage.setItem(EMAIL_KEY, v); } catch { /* private window: forget it */ }
}

// looksLikeEmail is the screen's own check; ferund and the provider check again.
export function looksLikeEmail(v) {
  return /^[^\s@]+@[^\s@]+$/.test((v || '').trim());
}

export function SignIn({ config, next, notice, onSignedIn }) {
  const [user, setUser] = useState('');
  const [pass, setPass] = useState('');
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);
  const [email, setEmail] = useState(storedEmail);
  const oidc = !!(config && config.oidc);
  // Basic is the screen's way in only where there is no provider (a
  // machine of one's own); with a provider, Basic stays for scripts.
  const basic = !oidc && !!(config && config.basic);

  return (
    <div className="login">
      <div className="login-card">
        <div className="login-brand">
          <BrandMark size={48} />
          <div>
            <div className="login-name">FeRun</div>
            <div className="login-tag">Containers and agent sandboxes on microVMs</div>
          </div>
        </div>
        {notice && <p className="notice small">{notice}</p>}
        {oidc && (
          <form action="/auth/login" method="get" onSubmit={(e) => {
            e.preventDefault();
            if (!looksLikeEmail(email)) { setErr('Enter your e-mail address.'); return; }
            rememberEmail(email.trim());
            window.location.assign(loginURL(next, email));
          }}>
            <label htmlFor="signin-email">E-mail address</label>
            <input id="signin-email" type="email" name="login_hint" value={email} autoFocus required
              autoComplete="username email" inputMode="email" placeholder="you@company.com"
              onChange={(e) => { setEmail(e.target.value); setErr(''); }} />
            <input type="hidden" name="next" value={next || '/'} />
            <button type="submit" className="primary">Sign in</button>
          </form>
        )}
        {basic && (
          <form onSubmit={async (e) => {
            e.preventDefault();
            setBusy(true);
            setErr('');
            try {
              const me = await auth.signInBasic(user, pass);
              onSignedIn(me);
            } catch (e2) {
              setErr(e2.message);
            } finally {
              setBusy(false);
            }
          }}>
            <input value={user} autoFocus placeholder="user" autoComplete="username"
              onChange={(e) => setUser(e.target.value)} />
            <input type="password" value={pass} placeholder="password" autoComplete="current-password"
              onChange={(e) => setPass(e.target.value)} />
            <button type="submit" className="primary" disabled={busy || !user}>
              {busy ? 'checking…' : 'Sign in'}
            </button>
          </form>
        )}
        {!oidc && !basic && (
          <p className="error">
            This instance offers no sign-in: ferund runs without --oidc-issuer and without --admin-users.
          </p>
        )}
        {err && <p className="error">{err}</p>}
      </div>
    </div>
  );
}
