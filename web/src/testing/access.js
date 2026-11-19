// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// A spec and the credentials of its environment whose names are those of
// ferund's fixtures of the bindings' use and the identities
// (GET_environments_env_bindings_usage.200, …_identities.200): service
// api with its bindings erp (a credential of the environment, pinned
// behind its latest) and mail (secrets, one without a value), its addons
// db (postgres, by identity) and bus (nats, a password), a secret pinned
// behind its latest and a registry login; job migrate. Credential models
// is a sandbox's only. For the views of where a key lives.

export const ACCESS_SPEC = {
  services: {
    api: {
      image: 'ghcr.io/acme/api:1',
      env: {
        DATABASE_URL: { addon: 'db', key: 'url', role: 'api_svc', database: 'app' },
        BUS_URL: { addon: 'bus', key: 'url' },
        SESSION_KEY: { secret: 'session-key' },
        PLAIN: 'x',
      },
      imageCredentials: { username: 'bot', password: { secret: 'ghcr-pull' } },
      bindings: ['erp', 'mail'],
    },
    web: { image: 'ghcr.io/acme/web:1' },
  },
  jobs: { migrate: { image: 'ghcr.io/acme/api:1', env: { DATABASE_URL: { addon: 'db', key: 'url', role: 'owner' } } } },
  addons: { db: { type: 'postgres' }, bus: { type: 'nats' } },
  bindings: {
    erp: { credentialRef: { name: 'erp', version: 3 } },
    mail: { upstream: 'https://mail.example', credential: { kind: 'basic', user: 'app', password: { secret: 'mail-pass' } } },
  },
  secrets: { 'session-key': { version: 2 }, 'ghcr-pull': { version: 1 } },
};

export const ACCESS_CREDENTIALS = [
  { name: 'erp', version: 4, upstream: 'https://api.vendor.example', mode: 'intercept', credential: { kind: 'oauth2' },
    createdAt: '2026-11-18T10:00:00Z', usedBy: { sandboxes: [], bindings: ['erp'], latest: 3, deployed: 3 } },
  { name: 'models', version: 1, upstream: 'https://models.example', mode: 'name', credential: { kind: 'headers' },
    createdAt: '2026-11-17T10:00:00Z', usedBy: { sandboxes: ['c3kv9mwq2hd'], bindings: [], latest: 0, deployed: 0 } },
];
