// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React from 'react';
import { Icon } from '../Icon.jsx';

// ProjectGate stands in for a page the sign-in may not open (people.js,
// viewGate): an operator's page, a project it is no member of, a page its
// role does not offer, or a project's page under a Basic sign-in, which
// the project routes do not take. The UI calls nothing for it.

const TEXTS = {
  operator: {
    icon: 'shield', title: 'For operators',
    text: 'This page is the operators\' of this FeRun: nodes, join tokens, instances, people and integrations.',
  },
  account: {
    icon: 'building', title: 'No such account',
    text: 'There is no account of this name whose page you may read: its members\' and the operators\'. '
      + 'An account\'s admin can invite you.',
  },
  member: {
    icon: 'lock', title: 'No such project',
    text: 'There is no project of this name that you are a member of. An admin of a project can invite you.',
  },
  role: {
    icon: 'lock', title: 'Not with your role',
    text: 'Your role in this project does not offer this page.',
  },
  basic: {
    icon: 'key', title: 'Sign in with the provider to work in a project',
    text: 'A password sign-in is an operator\'s for the operators\' pages; a project\'s pages act with a '
      + 'person\'s session. Sign out and sign in with the provider.',
  },
};

export function ProjectGate({ reason, project, account = '', navigate }) {
  const t = TEXTS[reason] || TEXTS.member;
  return (
    <section className="pane gate">
      <h2><Icon name={t.icon} size={15} /> {t.title}{reason !== 'operator' && (project || account)
        ? <> · <span className="mono">{project || account}</span></> : null}</h2>
      <p className="muted">{t.text}</p>
      <div className="actionrow">
        {reason === 'account'
          ? <button className="ghost" onClick={() => navigate('/accounts')}><Icon name="arrowLeft" size={14} /> Your accounts</button>
          : <button className="ghost" onClick={() => navigate('/projects')}><Icon name="arrowLeft" size={14} /> Your projects</button>}
      </div>
    </section>
  );
}
