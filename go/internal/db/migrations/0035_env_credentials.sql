-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- Credentials of an environment (ADR 0033, corrected on 2026-11-10;
-- docs/architecture/control-plane.md, "Credentials of an environment").
--
-- A credential belongs to an environment, not to its tenant: one stated
-- for production is not a name a pod of another environment can use.
-- env_credentials holds every version of an environment's credentials:
-- an HTTP binding's definition in the clear, as http_bindings has it, and
-- its credential sealed (AES-256-GCM under --secret-key-file, bound to
-- the table, the environment's ID, the name and the version). A write is
-- a new version, one more than any the name had (env_credential_names);
-- a version never changes; a delete removes every version of the name;
-- the environment's delete takes them all.
--
-- sandbox_credentials stays as it is: the names a sandbox names, which
-- now resolve in the sandbox's environment (sandboxes.environment_id), at
-- their latest versions. An environment's binding names one in its spec
-- (credentialRef), at a pinned version (env_deployments), which takes no
-- row here.
--
-- tenant_credentials is read and written no more. It held no rows when
-- this was written (2026-11-10), and it stays here so that the migration
-- is additive: ferund rolls one instance at a time, and the version
-- before reads tenant_credentials and sandbox_credentials for every
-- sandbox's bindings. A later migration drops it.
--
-- A row in it would be a credential no pod gets any more and no page
-- shows: its secret is sealed to its tenant, so SQL cannot move it into
-- an environment. The migration refuses while there is one, and with it
-- ferund's start; the instances of the version before run on. Once it
-- has run, a trigger refuses every row the version before inserts or
-- changes there, so that its PUT of a tenant's credential fails during
-- the roll instead of storing what nobody reads; its delete still works.
--
-- 0035 was left free for this when 0036 and later were numbered, so on
-- a database of those it runs after them; it needs nothing of theirs.

DO $$
DECLARE
  n bigint;
  tenants text;
BEGIN
  SELECT count(*), string_agg(DISTINCT tenant, ', ' ORDER BY tenant) INTO n, tenants FROM tenant_credentials;
  IF n > 0 THEN
    RAISE EXCEPTION '%', format('tenant_credentials holds %s rows (tenants: %s): credentials of a tenant, which '
      || 'this release no longer reads (ADR 0033). With the release before still running, note each one''s '
      || 'definition, take it from the sandboxes and environments that use it and delete it (DELETE '
      || '/api/v1/credentials/{name}); once tenant_credentials is empty, start this release and make each again '
      || 'under its environment (PUT /api/v1/environments/{env}/credentials/{name}).', n, tenants);
  END IF;
END
$$;

CREATE TABLE env_credentials (
  env_id uuid NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
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
  PRIMARY KEY (env_id, name, version));

-- env_credential_names holds the highest version each credential name of
-- an environment ever had, which a delete of the credential keeps: the
-- name made again goes on from there. So a version names one definition
-- and one secret for as long as the environment lives, and a deployment
-- that pinned version 1 of a credential deleted since finds no version 1
-- when it is reverted to, rather than another secret under that number.
-- An insert into env_credentials moves the mark first and is refused when
-- its version is not above it.
CREATE TABLE env_credential_names (
  env_id uuid NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
  name text NOT NULL,
  last_version integer NOT NULL CHECK (last_version >= 1),
  PRIMARY KEY (env_id, name));

CREATE FUNCTION tenant_credentials_refuse() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION '%', 'tenant_credentials is written by a ferund older than migration 0035: credentials are an '
    || 'environment''s now (ADR 0033); make it under its environment (PUT '
    || '/api/v1/environments/{env}/credentials/{name}) once every instance runs the new release';
END $$;

CREATE TRIGGER tenant_credentials_refuse BEFORE INSERT OR UPDATE ON tenant_credentials
  FOR EACH ROW EXECUTE FUNCTION tenant_credentials_refuse();
