-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- Visitors of an environment (ADR 0054, docs/architecture/control-plane.md,
-- "Visitors").
--
-- environments.access is who the ingress admits to a public environment:
-- 'anyone', or 'visitors' of access_provider, a provider of its tenant;
-- NULL there with 'visitors' is the provider ferun, which every tenant has
-- without a row. The default 'anyone' keeps every environment as it is: one
-- public before this stays open to everyone, and a publish by the release
-- before writes neither column.
--
-- access_providers are a tenant's providers of kind tenant (its own service
-- at url) and of kind oidc (an issuer, a client and its secret, sealed with
-- --secret-key-file under the AAD 'access_providers|<tenant>|<name>', and a
-- rule over the ID token's claims). An environment that names one holds it:
-- the provider's delete is refused while one does.
--
-- visits are ferund's record that a provider admitted a subject to an
-- environment until ends_at; asked_at is when its provider (kind tenant)
-- was last asked, at the accept or at a renewal; binding is the hash of the
-- secret beside the token in the visitor's cookie, set when the code is
-- redeemed, without which no token is renewed. visit_answers are the challenges a provider
-- answered, each once, with the single redemption of an accepted one's code.
-- Neither table holds a row for a visitor nobody vouched for.
--
-- service_tokens.kind gains 'auth', ferun-auth's token.
--
-- cluster_secrets takes the sealed seed of the visits' token key: the
-- cluster secret 'visit-token' is 0x01 || nonce(12) || AES-256-GCM(32-byte
-- seed), 61 bytes, sealed with --secret-key-file; every other one stays 32
-- bytes in plaintext.
--
-- Additive: the release before neither reads nor writes the new columns and
-- tables, lists a token of kind auth as it lists any, and refuses one at the
-- sink's certificate route by its prefix; it reads no cluster secret but
-- 'mcp-session'.

ALTER TABLE service_tokens DROP CONSTRAINT service_tokens_kind_check;
ALTER TABLE service_tokens ADD CONSTRAINT service_tokens_kind_check CHECK (kind IN ('sink', 'auth'));

ALTER TABLE cluster_secrets DROP CONSTRAINT cluster_secrets_value_check;
ALTER TABLE cluster_secrets ADD CONSTRAINT cluster_secrets_value_check
  CHECK (length(value) = 32 OR (name = 'visit-token' AND length(value) = 61));

CREATE TABLE access_providers (
  tenant text NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  name text NOT NULL CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$' AND name <> 'ferun'),
  kind text NOT NULL CHECK (kind IN ('tenant', 'oidc')),
  url text NOT NULL DEFAULT '',                -- kind tenant: the https URL of the tenant's service
  issuer text NOT NULL DEFAULT '',             -- kind oidc: the issuer, compared exactly
  client_id text NOT NULL DEFAULT '',          -- kind oidc
  client_secret_sealed bytea,                  -- kind oidc: 0x01 || nonce(12) || AES-256-GCM(secret)
  rule jsonb NOT NULL DEFAULT '{}',            -- kind oidc: {"anyone":true} | {"emails":[…]} | {"domains":[…]} | {"groups":[…]}
  made_by text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant, name),
  CONSTRAINT access_providers_tenant_kind CHECK (kind <> 'tenant' OR (url <> '' AND issuer = '' AND client_id = '' AND client_secret_sealed IS NULL)),
  CONSTRAINT access_providers_oidc_kind CHECK (kind <> 'oidc' OR (url = '' AND issuer <> '' AND client_id <> '' AND client_secret_sealed IS NOT NULL)));

ALTER TABLE environments
  ADD COLUMN access text NOT NULL DEFAULT 'anyone' CHECK (access IN ('anyone', 'visitors')),
  ADD COLUMN access_provider text,             -- with visitors: access_providers.name of owner; NULL: ferun
  ADD CONSTRAINT environments_access_provider_visitors CHECK (access = 'visitors' OR access_provider IS NULL),
  ADD CONSTRAINT environments_access_provider_fkey FOREIGN KEY (owner, access_provider) REFERENCES access_providers (tenant, name);

CREATE TABLE visits (
  id uuid PRIMARY KEY,                         -- chosen by ferund at the accept; the token's vid
  env_id uuid NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
  tenant text NOT NULL,                        -- the environment's owner
  provider text NOT NULL,                      -- 'ferun' or access_providers.name
  provider_kind text NOT NULL CHECK (provider_kind IN ('tenant', 'oidc', 'ferun')),
  host text NOT NULL,                          -- the one host whose cookie holds the visit
  subject text NOT NULL CHECK (length(subject) BETWEEN 1 AND 255),
  email text NOT NULL DEFAULT '',
  name text NOT NULL DEFAULT '',
  session text NOT NULL DEFAULT '',            -- the provider's own session the visit hangs on; '' none
  person_id uuid REFERENCES people(id) ON DELETE CASCADE,   -- kind ferun: the person whose grant admits
  created_at timestamptz NOT NULL DEFAULT now(),
  ends_at timestamptz NOT NULL,                -- the provider's end, at most 12 hours after the accept
  asked_at timestamptz NOT NULL DEFAULT now(), -- the provider was last asked (the accept, a renewal)
  binding text NOT NULL DEFAULT '' CHECK (binding = '' OR binding ~ '^[A-Za-z0-9_-]{43}$'), -- '' until redeemed
  ended_at timestamptz,
  end_reason text NOT NULL DEFAULT '' CHECK (end_reason IN ('', 'expired', 'provider_no', 'signed_out', 'unpublished', 'provider_changed', 'backchannel_logout')),
  CHECK ((ended_at IS NULL) = (end_reason = '')),
  CHECK ((provider_kind = 'ferun') = (person_id IS NOT NULL)));

CREATE INDEX visits_env ON visits (env_id) WHERE ended_at IS NULL;
CREATE INDEX visits_session ON visits (provider_kind, session) WHERE ended_at IS NULL AND session <> '';
CREATE INDEX visits_ends ON visits (ends_at);

CREATE TABLE visit_answers (
  challenge_id text PRIMARY KEY CHECK (challenge_id ~ '^[A-Za-z0-9_-]{22}$'),   -- the challenge's id, from its seal
  env_id uuid NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
  host text NOT NULL,
  outcome text NOT NULL CHECK (outcome IN ('accepted', 'rejected')),
  visit_id uuid REFERENCES visits(id) ON DELETE CASCADE,
  binding text NOT NULL,                       -- the challenge's binding: base64url(sha256(cookie secret))
  code_expires_at timestamptz NOT NULL,
  redeemed_at timestamptz,                     -- the first redeem of the code; NULL: not redeemed
  answered_by text NOT NULL DEFAULT '',        -- 'token:<hash>' | 'person:<id>' | 'service:<id>'
  created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,             -- the challenge's end: swept an hour after
  CHECK ((outcome = 'accepted') = (visit_id IS NOT NULL)));

CREATE INDEX visit_answers_expires ON visit_answers (expires_at);
