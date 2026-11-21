-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- Integrations (ADR 0056) and protected environments (ADR 0060). Additive:
-- the release before reads none of it.
--
-- An integration is a principal of its own, registered by an operator: the
-- issuer and subject its provider's access tokens carry. A project's admin
-- gives it grants as a person has them; grants_principal_scope stays as it
-- is (people and tokens), so that the release before keeps its ON
-- CONFLICT, and an integration's grants have a unique index of their own.
-- The route and sandbox tokens an integration mints record it, and go
-- with it. An environment is protected or it is not; a clone starts
-- unprotected.

CREATE TABLE integrations (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text NOT NULL UNIQUE CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
  title text NOT NULL DEFAULT '',
  url text NOT NULL DEFAULT '',
  issuer text NOT NULL,
  subject text NOT NULL CHECK (length(subject) BETWEEN 1 AND 255),
  disabled_at timestamptz,
  made_by text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  last_seen timestamptz,
  UNIQUE (issuer, subject));

ALTER TABLE grants ADD COLUMN integration_id uuid REFERENCES integrations(id) ON DELETE CASCADE;
-- 0027's unnamed CHECK (num_nonnulls(person_id, token_hash) = 1).
ALTER TABLE grants DROP CONSTRAINT grants_check;
ALTER TABLE grants ADD CONSTRAINT grants_one_principal
  CHECK (num_nonnulls(person_id, token_hash, integration_id) = 1);
CREATE UNIQUE INDEX grants_integration_scope
  ON grants (project_id, integration_id, coalesce(environment_id::text, ''))
  WHERE integration_id IS NOT NULL;
CREATE INDEX grants_integration ON grants (integration_id) WHERE integration_id IS NOT NULL;

ALTER TABLE env_route_tokens ADD COLUMN integration_id uuid REFERENCES integrations(id) ON DELETE CASCADE;
ALTER TABLE sandbox_tokens ADD COLUMN integration_id uuid REFERENCES integrations(id) ON DELETE CASCADE;

ALTER TABLE environments ADD COLUMN protected boolean NOT NULL DEFAULT false;
CREATE INDEX environments_protected ON environments (owner) WHERE protected;
