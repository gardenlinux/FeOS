// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// The status of the environment one works in (GET /api/v1/environments/
// {env}), read once for the sidebar, which lists its objects with their
// state, and the environment's pages: every ENV_OPEN_MS while one of its
// pages is open, every ENV_SIDE_MS while only the sidebar shows it.

import React, { createContext, useCallback, useContext, useEffect, useMemo, useState } from 'react';
import { api } from './api.js';
import { usePoll } from './components.jsx';

const ENV_OPEN_MS = 3000;
const ENV_SIDE_MS = 10000;

const none = { env: '', st: null, err: '', gone: false, lastOk: 0, refresh: () => {} };

const EnvStatusContext = createContext(none);

// useEnvStatus is the status of the environment one works in: {env, st
// (null until read), err, gone (404), lastOk, refresh}.
export function useEnvStatus() {
  return useContext(EnvStatusContext);
}

export function EnvStatusProvider({ env, open, children }) {
  const [data, setData] = useState({ env: '', st: null, err: '', gone: false });
  useEffect(() => { setData({ env, st: null, err: '', gone: false }); }, [env]);
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
  const value = useMemo(() => (data.env === env ? { ...data, lastOk, refresh } : { ...none, env, refresh }),
    [data, env, lastOk, refresh]);
  return <EnvStatusContext.Provider value={value}>{children}</EnvStatusContext.Provider>;
}
