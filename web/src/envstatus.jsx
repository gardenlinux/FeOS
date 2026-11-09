// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// The status of the environment one works in (GET /api/v1/environments/
// {env}), read once for the sidebar, which lists its objects with their
// state, and the environment's pages: every ENV_OPEN_MS while one of its
// pages is open, every ENV_SIDE_MS while only the sidebar shows it.
// While one of its pages is open, also what runs in it beside its
// deployments (ADR 0035): its sandboxes and the jobs made by a call,
// every PODS_MS, for the sidebar, the overview and their pages alike.

import React, { createContext, useCallback, useContext, useEffect, useMemo, useState } from 'react';
import { api } from './api.js';
import { usePoll } from './components.jsx';

const ENV_OPEN_MS = 3000;
const ENV_SIDE_MS = 10000;
const PODS_MS = 4000;

const none = {
  env: '', st: null, err: '', gone: false, lastOk: 0, refresh: () => {},
  sandboxes: null, jobs: null, podsErr: '', podsOk: 0, refreshPods: async () => {},
};

const EnvStatusContext = createContext(none);

// useEnvStatus is the status of the environment one works in: {env, st
// (null until read), err, gone (404), lastOk, refresh}, and its pods:
// {sandboxes, jobs (each null until read), podsErr, podsOk, refreshPods}.
export function useEnvStatus() {
  return useContext(EnvStatusContext);
}

export function EnvStatusProvider({ env, open, children }) {
  const [data, setData] = useState({ env: '', st: null, err: '', gone: false });
  const [pods, setPods] = useState({ env: '', sandboxes: null, jobs: null, err: '' });
  useEffect(() => {
    setData({ env, st: null, err: '', gone: false });
    setPods({ env, sandboxes: null, jobs: null, err: '' });
  }, [env]);
  const refresh = useCallback(async () => {
    if (!env) return;
    try {
      const st = await api.getEnvironment(env);
      setData({ env, st, err: '', gone: false });
    } catch (e) {
      setData((cur) => ({ env, st: cur.env === env ? cur.st : null, err: e.message, gone: e.status === 404 }));
    }
  }, [env]);
  const lastOk = usePoll(refresh, open ? ENV_OPEN_MS : ENV_SIDE_MS);
  const refreshPods = useCallback(async () => {
    if (!env || !open) return;
    try {
      const [s, j] = await Promise.all([api.listSandboxes(env), api.listJobs([], env)]);
      setPods({ env, sandboxes: s.sandboxes || [], jobs: j.jobs || [], err: '' });
    } catch (e) {
      setPods((cur) => (cur.env === env ? { ...cur, err: e.message } : { env, sandboxes: null, jobs: null, err: e.message }));
    }
  }, [env, open]);
  // Without a page of the environment open the sidebar does not list
  // them, and refreshPods asks nothing.
  const podsOk = usePoll(refreshPods, PODS_MS);
  const value = useMemo(() => {
    const p = pods.env === env ? pods : { sandboxes: null, jobs: null, err: '' };
    const base = data.env === env ? { ...data, lastOk, refresh } : { ...none, env, refresh };
    return { ...base, sandboxes: p.sandboxes, jobs: p.jobs, podsErr: p.err, podsOk, refreshPods };
  }, [data, pods, env, lastOk, refresh, podsOk, refreshPods]);
  return <EnvStatusContext.Provider value={value}>{children}</EnvStatusContext.Provider>;
}
