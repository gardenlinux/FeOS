// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React from 'react'
import ReactDOM from 'react-dom/client'
import App from './App.jsx'
import { currentUser } from './auth.js'
import { applyStoredTheme } from './theme.js'
import './styles.css'
import './theme-gta.css'

applyStoredTheme()

const root = ReactDOM.createRoot(document.getElementById('root'))

// Nothing renders before the sign-in: a browser without a session goes to
// it straight away.
currentUser()
  .then((user) => {
    if (!user) return
    root.render(
      <React.StrictMode>
        <App user={user} />
      </React.StrictMode>,
    )
  })
  .catch((e) => {
    root.render(<p className="boot-error">FeChat is unreachable: {e.message}</p>)
  })
