// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React from 'react';
import { Icon } from '../Icon.jsx';

// TenantGate stands in for a page the sign-in may not open (people.js,
// viewGate): an operator's page, a tenant it is no member of, a page its
// role does not offer, or a tenant's page under a Basic sign-in, which
// the tenant routes do not take. The UI calls nothing for it.

const TEXTS = {
  operator: {
    icon: 'shield', title: 'For operators',
    text: 'This page is the operators\' of this FeRun: nodes, join tokens and instances.',
  },
  member: {
    icon: 'lock', title: 'No such tenant',
    text: 'There is no tenant of this name that you are a member of. An admin of a tenant can invite you.',
  },
  role: {
    icon: 'lock', title: 'Not with your role',
    text: 'Your role in this tenant does not offer this page.',
  },
  basic: {
    icon: 'key', title: 'Sign in with the provider to work in a tenant',
    text: 'A password sign-in is an operator\'s for the operators\' pages; a tenant\'s pages act with a '
      + 'person\'s session. Sign out and sign in with the provider.',
  },
};

export function TenantGate({ reason, tenant, navigate }) {
  const t = TEXTS[reason] || TEXTS.member;
  return (
    <section className="pane gate">
      <h2><Icon name={t.icon} size={15} /> {t.title}{reason !== 'operator' && tenant ? <> · <span className="mono">{tenant}</span></> : null}</h2>
      <p className="muted">{t.text}</p>
      <div className="actionrow">
        <button className="ghost" onClick={() => navigate('/tenants')}><Icon name="arrowLeft" size={14} /> Your tenants</button>
      </div>
    </section>
  );
}
