// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useEffect, useState } from 'react';
import { api } from '../api.js';
import { envPath } from '../routes.js';
import { EnvLink } from './EnvTabs.jsx';
import { DEFAULT_GROUP, DEFAULT_WORDS, GROUP_LIMITS, groupChoices, toggleGroup } from './groups.js';
import '../styles/groups.css';

// The widgets of security groups outside the builder (ADR 0034, ADR
// 0035): the choice of an environment and of its groups for a sandbox or
// a tenant's job, and a pod's groups as chips. The groups to choose from
// are those of the environment's deployed deployment, "default" always.

// useEnvGroups reads the groups env (its name or ID) offers: {choices:
// [{name, words}], deployed, err}; choices null while unread. Without a
// deployment, or when the deployment cannot be read, only "default".
export function useEnvGroups(env) {
  const [state, setState] = useState({ env: '', choices: null, deployed: 0, err: '' });
  useEffect(() => {
    if (!env) return undefined;
    let live = true;
    (async () => {
      const st = await api.getEnvironment(env);
      const n = Number(st.deployed) || 0;
      const spec = n > 0 ? (await api.getDeployment(st.name || env, n)).spec || {} : {};
      if (live) setState({ env, choices: groupChoices(spec), deployed: n, err: '' });
    })().catch((e) => { if (live) setState({ env, choices: groupChoices({}), deployed: 0, err: e.message }); });
    return () => { live = false; };
  }, [env]);
  return state.env === env && env ? state : { env, choices: null, deployed: 0, err: '' };
}

// GroupPicker ticks one to five groups among choices ([{name, words}]):
// the last one cannot be unticked, a sixth not ticked. A name of value
// that the choices lack shows, marked, so that it can be unticked.
export function GroupPicker({ choices, value, onChange, disabled = false }) {
  const list = Array.isArray(value) ? value : [];
  const names = (choices || []).map((c) => c.name);
  const missing = list.filter((n) => !names.includes(n));
  const full = list.length >= GROUP_LIMITS.perObject;
  const row = (name, words, bad = false) => {
    const on = list.includes(name);
    const lock = disabled || (on && list.length === 1) || (!on && full);
    return (
      <label key={name} className={'sg-choice' + (on ? ' on' : '') + (bad ? ' bad' : '')}
        title={on && list.length === 1 ? 'a pod is in one group at least' : !on && full ? `at most ${GROUP_LIMITS.perObject} groups` : undefined}>
        <input type="checkbox" checked={on} disabled={lock}
          onChange={(e) => onChange(toggleGroup(list, name, e.target.checked, names))} />
        <span className="mono sg-name">{name}</span>
        <span className="muted small sg-words">{words}</span>
      </label>
    );
  };
  return (
    <div className="sg-pick" role="group" aria-label="security groups">
      {(choices || []).map((c) => row(c.name, c.words))}
      {missing.map((n) => row(n, 'not a group of this environment', true))}
      {!choices && <span className="muted small">reading the environment's groups…</span>}
    </div>
  );
}

// GroupChips are a pod's groups, "default" with what it means when it
// has no rules of its own.
export function GroupChips({ groups, words = {} }) {
  const list = Array.isArray(groups) && groups.length ? groups : [DEFAULT_GROUP];
  return (
    <span className="sg-chips">
      {list.map((g) => (
        <span key={g} className="sg-chip mono" title={words[g] || (g === DEFAULT_GROUP ? `${DEFAULT_GROUP}: ${DEFAULT_WORDS}` : undefined)}>{g}</span>))}
    </span>
  );
}

// EnvLinkName is the environment a sandbox or a job is in, as a link to
// its page.
export function EnvLinkName({ tenant, env, navigate }) {
  if (!env) return <span className="muted">—</span>;
  return <EnvLink to={envPath(tenant, env.name || env.id)} navigate={navigate} className="mono">{env.name || env.id}</EnvLink>;
}

// EnvPicker chooses one of envs (those the person may write), by name;
// none says whether the tenant has no environment at all.
export function EnvPicker({ envs, value, onChange, disabled = false, none = false }) {
  const list = envs || [];
  const empty = none ? 'no environment yet' : 'no environment you may write';
  return (
    <select value={value} onChange={(e) => onChange(e.target.value)} disabled={disabled || list.length === 0} aria-label="environment">
      <option value="">{envs === null ? 'reading the environments…' : list.length ? '(an environment)' : empty}</option>
      {list.map((e) => <option key={e.id || e.name} value={e.name}>{e.name}</option>)}
    </select>
  );
}

// NoEnvironment says why there is no environment to make a sandbox or a
// job in (what): the tenant has none yet (none), which a tenant writer
// (make) makes first, or the person writes none of those there are.
export function NoEnvironment({ tenant, navigate, none, make, what }) {
  if (!none) return <p className="error">Your role does not allow this: it needs write on an environment.</p>;
  const to = `/tenants/${encodeURIComponent(tenant)}/new-environment`;
  return (
    <p className="sg-noenv">
      This tenant has no environment yet, and a {what} runs in one.{' '}
      {make
        ? <EnvLink to={to} navigate={navigate}>Make an environment first.</EnvLink>
        : 'Someone with write on the tenant makes one first.'}
    </p>
  );
}
