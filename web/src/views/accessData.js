// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// What the views of where a key lives read beside the spec (access.js):
// the addon catalog (which types admit a workload by its certificate),
// the environment's secrets (both kinds, ADR 0055), its bindings' use
// (the sink's counts of the last 24 h, per binding: an HTTP secret's
// under its name) and its workloads' identities. Read with the session;
// none of it carries a key.

import { useCallback, useEffect, useState } from 'react';
import { api } from '../api.js';
import { usePoll } from '../components.jsx';
import { builtinCatalog, readCatalog } from './catalog.js';

const SECRETS_MS = 10000;
const USAGE_MS = 60000;
const IDENTITIES_MS = 60000;

// useAddonCatalog is ferund's addon catalog, read once; null until it
// answers, the built-in list from a ferund that has none or fails.
export function useAddonCatalog() {
  const [catalog, setCatalog] = useState(null);
  useEffect(() => {
    let live = true;
    api.addonCatalog().then(
      (a) => { if (live) setCatalog(readCatalog(a) || builtinCatalog()); },
      () => { if (live) setCatalog(builtinCatalog()); },
    );
    return () => { live = false; };
  }, []);
  return catalog;
}

// useRead is what fn answers, read now and every ms: {data (null until
// the first answer; the last one kept on an error), err, reload}.
function useRead(fn, ms) {
  const [state, setState] = useState({ data: null, err: '' });
  const read = useCallback(async () => {
    try {
      const data = await fn();
      setState({ data, err: '' });
    } catch (e) {
      setState((cur) => ({ data: cur.data, err: e.message }));
    }
  }, [fn]);
  usePoll(read, ms);
  return { ...state, reload: read };
}

// useEnvSecrets are environment env's secrets, value and HTTP (ADR
// 0055), an HTTP secret with its definition and usedBy.
export function useEnvSecrets(env) {
  const fn = useCallback(async () => (await api.listSecrets(env)).secrets || [], [env]);
  return useRead(fn, SECRETS_MS);
}

// useBindingUsage is what env's bindings carried in the last 24 h
// (GET …/bindings/usage): a failed read answers as the log store's
// unavailable does, with no numbers and why. Not read (null) unless
// wanted: the sink searches its trace index for it.
export function useBindingUsage(env, wanted = true) {
  const fn = useCallback(async () => {
    if (!wanted) return null;
    try {
      return await api.bindingUsage(env, '24h');
    } catch (e) {
      return { window: '24h', unavailable: e.message, truncated: false, bindings: [] };
    }
  }, [env, wanted]);
  return useRead(fn, USAGE_MS).data;
}

// useIdentities are env's workloads with their SPIFFE IDs and their pods'
// certificates' ends (GET …/identities).
export function useIdentities(env) {
  const fn = useCallback(() => api.identities(env), [env]);
  return useRead(fn, IDENTITIES_MS);
}
