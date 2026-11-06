// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// The tenant's summary: the lists Home, the sidebar's counts and the
// search read, asked once per tenant and every SUMMARY_MS, for every
// page at once. Each list is null while unread and stays null when the
// role does not offer it or this ferund lacks the route.

import React, { createContext, useCallback, useContext, useMemo, useState } from 'react';
import { api } from './api.js';
import { usePoll } from './components.jsx';
import { attention, navCounts } from './attention.js';

const SUMMARY_MS = 15000;

// INCIDENT_ENVS bounds the environments whose open incidents are read
// one by one; the rest show by their count.
const INCIDENT_ENVS = 8;

const empty = {
  tenant: '', sandboxes: null, environments: null, incidents: {}, domains: null, invitations: null, quota: null,
  jobs: null, at: 0,
};

const SummaryContext = createContext({ ...empty, items: [], counts: {}, refresh: () => {} });

export function useSummary() {
  return useContext(SummaryContext);
}

// optional is a list's read that a missing route or role leaves null.
async function optional(fn, pick) {
  try {
    return pick(await fn());
  } catch {
    return null;
  }
}

export function SummaryProvider({ tenant, can, children }) {
  const [data, setData] = useState(empty);

  const refresh = useCallback(async () => {
    if (!tenant) {
      setData(empty);
      return;
    }
    const [sandboxes, environments, domains, invitations, quota, jobs] = await Promise.all([
      can.readTenant ? optional(api.listSandboxes, (r) => r.sandboxes || []) : null,
      can.environments ? optional(api.listEnvironments, (r) => r.environments || []) : null,
      can.members ? optional(() => api.listDomains(), (r) => r.domains || []) : null,
      can.members ? optional(api.listInvitations, (r) => r.invitations || []) : null,
      can.members ? optional(api.getQuota, (r) => r) : null,
      can.readTenant ? optional(() => api.listJobs(), (r) => r.jobs || []) : null,
    ]);
    const troubled = (environments || []).filter((e) => Number(e.openIncidents) > 0).slice(0, INCIDENT_ENVS);
    const lists = await Promise.all(troubled.map((e) => optional(
      () => api.listIncidents(e.name, { open: true }), (r) => (r.incidents || []).filter((i) => !i.closedAt))));
    const incidents = {};
    troubled.forEach((e, i) => { if (lists[i]) incidents[e.name] = lists[i]; });
    setData({ tenant, sandboxes, environments, incidents, domains, invitations, quota, jobs, at: Date.now() });
  }, [tenant, can.readTenant, can.environments, can.members]);

  usePoll(refresh, SUMMARY_MS);

  const value = useMemo(() => {
    const d = data.tenant === tenant ? data : { ...empty, tenant };
    const items = attention(d);
    const running = (d.jobs || []).filter((j) => j.phase === 'queued' || j.phase === 'running').length;
    const counts = navCounts(items, {
      sandboxes: d.sandboxes ? d.sandboxes.length : null,
      environments: d.environments ? d.environments.length : null,
      domains: d.domains ? d.domains.length : null,
      jobs: running,
    });
    return { ...d, items, counts, refresh };
  }, [data, tenant, refresh]);

  return <SummaryContext.Provider value={value}>{children}</SummaryContext.Provider>;
}
