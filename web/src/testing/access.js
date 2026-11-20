// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// A spec and the secrets of its environment (ADR 0055: value and HTTP
// secrets in one list, as GET /api/v1/environments/{env}/secrets answers
// them) whose names are those of ferund's fixtures of the bindings' use
// and the identities (GET_environments_env_bindings_usage.200,
// …_identities.200): service api with HTTP secret erp by a variable
// (pinned behind its latest, a sandbox names it too), a binding mail
// stated in the spec (from before ADR 0055, its key a secret without a
// value), its addons db (postgres, by identity) and bus (nats, a
// password), a secret pinned behind its latest and a registry login; job
// migrate. HTTP secret models is a sandbox's only, spare-api no one's,
// leftover a value no one references. For the views of where a key
// lives.

export const ACCESS_SPEC = {
  services: {
    api: {
      image: 'ghcr.io/acme/api:1',
      env: {
        DATABASE_URL: { addon: 'db', key: 'url', role: 'api_svc', database: 'app' },
        BUS_URL: { addon: 'bus', key: 'url' },
        SESSION_KEY: { secret: 'session-key' },
        ERP_URL: { secret: 'erp', key: 'url' },
        PLAIN: 'x',
      },
      imageCredentials: { username: 'bot', password: { secret: 'ghcr-pull' } },
      bindings: ['mail'],
    },
    web: { image: 'ghcr.io/acme/web:1' },
  },
  jobs: { migrate: { image: 'ghcr.io/acme/api:1', env: { DATABASE_URL: { addon: 'db', key: 'url', role: 'owner' } } } },
  addons: { db: { type: 'postgres' }, bus: { type: 'nats' } },
  bindings: {
    mail: { upstream: 'https://mail.example', credential: { kind: 'basic', user: 'app', password: { secret: 'mail-pass' } } },
  },
  secrets: {
    'session-key': { version: 2 },
    'ghcr-pull': { version: 1 },
    erp: { version: 3, type: 'http', upstream: 'https://api.vendor.example', mode: 'intercept' },
  },
};

// ACCESS_SECRETS are the environment's secrets, sorted by name.
export const ACCESS_SECRETS = [
  { name: 'erp', type: 'http', version: 4, createdBy: 'malte', createdAt: '2026-11-18T10:00:00Z',
    upstream: 'https://api.vendor.example', mode: 'intercept', pathPrefixes: ['/odata/'], forwardedHost: false,
    rateLimit: { perSecond: 5, burst: 10 },
    credential: { kind: 'oauth2', tokenUrl: 'https://id.vendor.example/token', clientId: 'radar', clientSecret: { set: true } },
    usedBy: { sandboxes: ['c3kv9mwq2hd'], objects: ['service/api'], latest: 3, deployed: 3 } },
  { name: 'ghcr-pull', type: 'value', version: 1, createdBy: 'fecode:x', createdAt: '2026-11-01T08:00:00Z' },
  { name: 'leftover', type: 'value', version: 1, createdBy: 'fecode:x', createdAt: '2026-10-01T08:00:00Z' },
  { name: 'models', type: 'http', version: 1, createdBy: 'malte', createdAt: '2026-11-17T10:00:00Z',
    upstream: 'https://models.example', mode: 'name', pathPrefixes: [], forwardedHost: false,
    credential: { kind: 'headers', headers: { Authorization: { set: true } } },
    usedBy: { sandboxes: ['c3kv9mwq2hd'], objects: [], latest: 0, deployed: 0 } },
  { name: 'session-key', type: 'value', version: 3, createdBy: 'fecode:0193a000-aaaa-7bbb-8ccc-000000000001',
    createdAt: '2026-11-19T08:00:00Z' },
  { name: 'spare-api', type: 'http', version: 1, createdBy: 'malte', createdAt: '2026-11-10T10:00:00Z',
    upstream: 'https://spare.example', mode: 'name', pathPrefixes: [], forwardedHost: false,
    credential: { kind: 'basic', user: 'x', password: { set: true } },
    usedBy: { sandboxes: [], objects: [], latest: 0, deployed: 0 } },
];

// ACCESS_HTTP are its HTTP secrets.
export const ACCESS_HTTP = ACCESS_SECRETS.filter((s) => s.type === 'http');
