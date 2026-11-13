// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useEffect, useState } from 'react';
import { Icon } from './Icon.jsx';
import { buildOf } from './build.js';

// The build this page runs, read when the app starts.
const mine = typeof document === 'undefined' ? '' : buildOf(document);

const EVERY = 5 * 60 * 1000; // between two checks while the page is shown
const FIRST = 5 * 1000; // after the start: the page may come from an old copy
const AGAIN = 60 * 1000; // a page shown again is checked when the last check is older

// useNewBuild answers whether ferund serves another build of the UI than
// this page runs: it asks for index.html now and then, when the page is
// shown again and when the network is back.
function useNewBuild() {
  const [found, setFound] = useState(false);
  useEffect(() => {
    if (!mine) return undefined;
    let done = false;
    let last = 0;
    const check = async () => {
      if (done || document.hidden) return;
      last = Date.now();
      try {
        const res = await fetch('/', { cache: 'no-store', credentials: 'same-origin' });
        if (!res.ok) return;
        const theirs = buildOf(new DOMParser().parseFromString(await res.text(), 'text/html'));
        if (theirs && theirs !== mine && !done) {
          done = true;
          setFound(true);
        }
      } catch {
        // Offline, or ferund between two versions: the next check.
      }
    };
    const shown = () => { if (Date.now() - last > AGAIN) check(); };
    const first = setTimeout(check, FIRST);
    const every = setInterval(check, EVERY);
    document.addEventListener('visibilitychange', shown);
    window.addEventListener('online', shown);
    return () => {
      done = true;
      clearTimeout(first);
      clearInterval(every);
      document.removeEventListener('visibilitychange', shown);
      window.removeEventListener('online', shown);
    };
  }, []);
  return found;
}

// UpdateNotice says that a new version of FeRun's UI is out, with a
// button that reloads the page. It never reloads by itself: a form being
// filled in would go. Put aside, it stays away until the page is loaded
// again.
export function UpdateNotice() {
  const found = useNewBuild();
  const [aside, setAside] = useState(false);
  if (!found || aside) return null;
  return (
    <div className="update-note" role="status" aria-live="polite">
      <span>A new version of FeRun is available.</span>
      <button className="primary" onClick={() => window.location.reload()}>
        <Icon name="refresh" size={14} /> Reload
      </button>
      <button className="iconbtn" onClick={() => setAside(true)} aria-label="Not now" title="Not now">
        <Icon name="x" size={14} />
      </button>
    </div>
  );
}
