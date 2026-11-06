// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React from 'react';
import { Icon } from '../Icon.jsx';
import { envPath } from '../routes.js';
import { Builder } from './Builder.jsx';

// NewEnvironment is the builder of an environment that does not exist
// yet (ADR 0032): a name and a draft, empty or imported, planned without
// an environment; its first apply creates the environment, writes the
// secrets whose values the page holds and applies deployment 1, in that
// order.
export function NewEnvironment({ tenant, navigate }) {
  return (
    <section className="pane pane-detail">
      <div className="detail-head">
        <button className="iconbtn" title="back to the environments" onClick={() => navigate(envPath(tenant))}>
          <Icon name="arrowLeft" size={15} /></button>
        <div className="detail-title">
          <span className="strong">New environment</span>
          <span className="muted small">its draft stays in this browser until it is applied</span>
        </div>
      </div>
      <div className="tabbody">
        <Builder tenant={tenant} env="" st={null} navigate={navigate} />
      </div>
    </section>
  );
}
