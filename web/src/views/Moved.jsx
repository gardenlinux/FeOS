// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useEffect, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { jobPath, newSandboxPath, sandboxPath } from '../routes.js';

// Where a sandbox or a job was before it moved into its environment
// (ADR 0035): /projects/:t/sandboxes[/new | /:id[/:tab]] and
// /projects/:t/jobs[/:id]. An old link leads to its page in its
// environment, which ferund names (GET /api/v1/sandboxes/{id}, GET
// /api/v1/jobs/{id}), and replaces itself in the history; a list, or a
// new sandbox without an environment, leads to the project's Home.

// movedTarget is where a moved route leads without asking ferund: a new
// sandbox's form in the environment the address named, the project's Home
// for a list or a form without one; '' when the environment must be read.
export function movedTarget(route) {
  const t = encodeURIComponent(route.project);
  if (route.what === 'sandbox' && route.sel === 'new') return route.env ? newSandboxPath(route.project, route.env) : `/projects/${t}`;
  if (!route.sel) return `/projects/${t}`;
  return '';
}

// movedPath is the page of a sandbox or a job x, as ferund answers it,
// in its environment; tab is a sandbox's view.
export function movedPath(route, x) {
  const e = x && x.environment;
  const env = e && (e.name || e.id);
  if (!env) return '';
  return route.what === 'sandbox' ? sandboxPath(route.project, env, route.sel, route.tab) : jobPath(route.project, env, route.sel);
}

export function Moved({ route, navigate }) {
  const [missing, setMissing] = useState('');
  const { project, what, sel, tab, env } = route;
  useEffect(() => {
    const r = { project, what, sel, tab, env };
    const now = movedTarget(r);
    if (now) {
      navigate(now, { replace: true });
      return undefined;
    }
    let live = true;
    const read = what === 'sandbox' ? api.getSandbox(sel) : api.getJob(sel);
    read.then((x) => {
      if (!live) return;
      const to = movedPath(r, x);
      if (to) navigate(to, { replace: true });
      else setMissing('it names no environment');
    }, (e) => { if (live) setMissing(e.status === 404 ? 'gone' : e.message || 'gone'); });
    return () => { live = false; };
  }, [project, what, sel, tab, env, navigate]);
  const home = `/projects/${encodeURIComponent(project)}`;
  if (!missing) return <p className="empty">Looking for its environment…</p>;
  return (
    <section className="pane">
      <p className="error">No {what} <span className="mono">{sel}</span> in this project
        {missing !== 'gone' ? ` (${missing})` : ''}, or none of an environment you may read.</p>
      <a className="button ghost" href={home} onClick={(e) => { e.preventDefault(); navigate(home); }}>
        <Icon name="arrowLeft" size={14} /> Home</a>
    </section>
  );
}
