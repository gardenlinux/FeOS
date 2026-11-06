-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- People in FeRun (ADR 0031, docs/architecture/control-plane.md, "People
-- and grants").
--
-- people is everyone who signed in through the shared provider: an issuer
-- and a subject, with an ID of ferund's, the e-mail address and the name of
-- the last sign-in (for display only: nothing matches a person by them),
-- when they were first and last seen, and disabled_at: an operator's
-- switch that ends every session of the person and every grant's effect,
-- the grants kept. A person is never deleted.
--
-- grants gives a principal a role in a scope: the principal is a person
-- (person_id) or a tenant token (token_hash), exactly one of them; the
-- scope is the tenant (environment_id NULL) or one environment of it; the
-- role read, write or admin, each including the one before. A principal
-- holds at most one grant per scope. A grant on an environment goes with
-- the environment, every grant with its tenant, its person or its token.
--
-- invitations are single-use links with a role and a scope (D4), their
-- secret stored as a hash: used_by and used_at say who took one.
--
-- tenant_tokens gain the token's own grant, minted with it: role and
-- environment_id (NULL: the tenant). Every token there becomes admin on
-- its tenant, and a token an instance of the previous version mints is
-- one too (the default), so FeCode and FeChat go on unchanged.
--
-- tenants gain their zone (D6; NULL: the apps zone), the person who made
-- them (made_by; NULL: an operator's tenant) and their limits (D5; NULL:
-- none).
--
-- ui_sessions gain the person (NULL: a session of the previous version,
-- an operator's) and the provider's sid, by which a back-channel logout
-- ends them. admin stays: an operator's session.
--
-- made_by is the principal that made a row, as ferund saw it, beside the
-- free author a client sends: 'person:<people.id>', 'token:<tenant token
-- hash>' or 'basic:<user>' (an operator by --admin-users); '' when made by
-- ferund itself or by a version before this one.
--
-- Additive: the previous version reads and writes none of the new columns.

CREATE TABLE people (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  issuer text NOT NULL,
  sub text NOT NULL,
  email text NOT NULL DEFAULT '',
  name text NOT NULL DEFAULT '',
  first_seen timestamptz NOT NULL DEFAULT now(),
  last_seen timestamptz NOT NULL DEFAULT now(),
  disabled_at timestamptz,
  UNIQUE (issuer, sub));

ALTER TABLE tenant_tokens
  ADD COLUMN role text NOT NULL DEFAULT 'admin' CHECK (role IN ('read','write','admin')),
  ADD COLUMN environment_id uuid REFERENCES environments(id) ON DELETE CASCADE,
  ADD COLUMN made_by text NOT NULL DEFAULT '';

CREATE TABLE grants (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  tenant_id text NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  person_id uuid REFERENCES people(id) ON DELETE CASCADE,
  token_hash text REFERENCES tenant_tokens(hash) ON DELETE CASCADE,
  environment_id uuid REFERENCES environments(id) ON DELETE CASCADE,   -- NULL: the tenant
  role text NOT NULL CHECK (role IN ('read','write','admin')),
  made_by text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  CHECK (num_nonnulls(person_id, token_hash) = 1));
CREATE UNIQUE INDEX grants_principal_scope ON grants
  (tenant_id, coalesce(person_id::text, token_hash), coalesce(environment_id::text, ''));
CREATE INDEX grants_person ON grants(person_id) WHERE person_id IS NOT NULL;
CREATE INDEX grants_token ON grants(token_hash) WHERE token_hash IS NOT NULL;
CREATE INDEX grants_environment ON grants(environment_id) WHERE environment_id IS NOT NULL;

CREATE TABLE invitations (
  hash text PRIMARY KEY,                       -- sha256 hex of the link's secret
  tenant_id text NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  environment_id uuid REFERENCES environments(id) ON DELETE CASCADE,   -- NULL: the tenant
  role text NOT NULL CHECK (role IN ('read','write','admin')),
  made_by text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,
  used_by uuid REFERENCES people(id) ON DELETE SET NULL,
  used_at timestamptz,
  CHECK (used_by IS NULL OR used_at IS NOT NULL));
CREATE INDEX invitations_tenant ON invitations(tenant_id, created_at);

ALTER TABLE tenants
  ADD COLUMN zone text,
  ADD COLUMN made_by uuid REFERENCES people(id) ON DELETE SET NULL,
  ADD COLUMN limit_sandboxes integer CHECK (limit_sandboxes >= 0),
  ADD COLUMN limit_environments integer CHECK (limit_environments >= 0),
  ADD COLUMN limit_running_jobs integer CHECK (limit_running_jobs >= 0),
  ADD COLUMN limit_vcpus integer CHECK (limit_vcpus >= 0),
  ADD COLUMN limit_memory_mib bigint CHECK (limit_memory_mib >= 0);
CREATE INDEX tenants_made_by ON tenants(made_by) WHERE made_by IS NOT NULL;

ALTER TABLE ui_sessions
  ADD COLUMN person_id uuid REFERENCES people(id) ON DELETE CASCADE,
  ADD COLUMN sid text NOT NULL DEFAULT '';
CREATE INDEX ui_sessions_person ON ui_sessions(person_id) WHERE person_id IS NOT NULL;
CREATE INDEX ui_sessions_sid ON ui_sessions(sid) WHERE sid <> '';

ALTER TABLE env_deployments ADD COLUMN made_by text NOT NULL DEFAULT '';
ALTER TABLE env_secrets ADD COLUMN made_by text NOT NULL DEFAULT '';
ALTER TABLE env_tags ADD COLUMN made_by text NOT NULL DEFAULT '';
ALTER TABLE env_route_tokens ADD COLUMN made_by text NOT NULL DEFAULT '';
ALTER TABLE domains ADD COLUMN made_by text NOT NULL DEFAULT '';
ALTER TABLE sandboxes ADD COLUMN made_by text NOT NULL DEFAULT '';
ALTER TABLE sandbox_tokens ADD COLUMN made_by text NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN made_by text NOT NULL DEFAULT '';
