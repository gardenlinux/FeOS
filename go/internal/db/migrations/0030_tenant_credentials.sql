-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- Credentials of a tenant (ADR 0033, docs/architecture/control-plane.md,
-- "Credentials of a tenant").
--
-- tenant_credentials holds every version of a tenant's credentials: an
-- HTTP binding's definition in the clear, as http_bindings has it, and its
-- credential sealed (AES-256-GCM under --secret-key-file, bound to the
-- table, the tenant, the name and the version). A write is a new version,
-- one more than the latest; a version never changes; a delete removes
-- every version of the name.
--
-- sandbox_credentials are the credentials a sandbox names: its pod gets
-- each at its latest version beside its own HTTP set. A sandbox's delete
-- cascades. An environment's binding names a credential in its spec, at
-- a pinned version (env_deployments), which takes no row here.
--
-- Additive: an instance of the previous version neither reads nor writes
-- the new tables.

CREATE TABLE tenant_credentials (
  tenant text NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  name text NOT NULL CHECK (name ~ '^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$'),
  version integer NOT NULL CHECK (version >= 1),
  upstream text NOT NULL,                      -- https://host[:port], canonical
  mode text NOT NULL CHECK (mode IN ('name','intercept')),
  ca_pem text NOT NULL DEFAULT '',
  path_prefixes text[] NOT NULL DEFAULT '{}',
  forwarded_host boolean NOT NULL DEFAULT false,
  rate_per_second integer NOT NULL DEFAULT 0 CHECK (rate_per_second >= 0),
  rate_burst integer NOT NULL DEFAULT 0 CHECK (rate_burst >= 0),
  credential_kind text NOT NULL CHECK (credential_kind IN ('headers','basic','oauth2','clientCertificate')),
  sealed bytea NOT NULL,                       -- 0x01 || nonce(12) || AES-256-GCM(credential JSON)
  created_by text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant, name, version));

CREATE TABLE sandbox_credentials (
  sandbox_id text COLLATE "C" NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
  name text NOT NULL,
  PRIMARY KEY (sandbox_id, name));
