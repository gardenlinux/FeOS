// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// The marks of where a key lives (access.js), as badges: by identity (a
// shield), offloaded (a lock, at the node), in the container (a key,
// handed over); a relation that hands over no key has none.

import React from 'react';
import { Icon } from '../Icon.jsx';
import { MARKS, MARK_LABEL, MARK_TITLE } from './access.js';
import '../styles/marks.css';

const MARK_ICON = { identity: 'shield', offloaded: 'lock', container: 'key' };

// KeyMark is a relation's mark; label may say it more precisely ("OAuth2
// at the node"), title replace what it means on hover.
export function KeyMark({ mark, label = '', title = '' }) {
  if (!MARKS.includes(mark)) {
    return <span className="badge mark mark-none" title="Hands over no key.">no key</span>;
  }
  return (
    <span className={`badge mark mark-${mark}`} title={title || MARK_TITLE[mark]}>
      <Icon name={MARK_ICON[mark]} size={11} /> {label || MARK_LABEL[mark]}
    </span>
  );
}

// MarkLegend says once what the three marks mean, where they first show;
// marks, when given, are the ones shown there.
export function MarkLegend({ marks = MARKS }) {
  return (
    <div className="mark-legend">
      {MARKS.filter((m) => marks.includes(m)).map((m) => (
        <span key={m}><KeyMark mark={m} /> {MARK_TITLE[m]}</span>
      ))}
    </div>
  );
}
