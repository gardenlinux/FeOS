-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- HTTP bindings (ADR 0028, docs/architecture/control-plane.md, "HTTP
-- bindings").
--
-- http_bindings is a sandbox's HTTP set, stated whole by PUT
-- /api/v1/sandboxes/{id}/http, beside kube_bindings and ssh_bindings: the
-- binding's upstream, mode and options in the clear, its credential sealed
-- (AES-256-GCM under --secret-key-file, bound to the table, the sandbox,
-- the name and the kind). sandboxes.http_version is the set's version: a
-- random value that changes exactly when the set does ('' for none).
--
-- pod_intercept_cas are a pod's interception CAs (ADR 0028 §5), oldest
-- first by seq: ECDSA P-256, name-constrained to hosts, the intercepted
-- host names at its issue; the key PKCS#8, sealed like a credential. A
-- pod that comes to intercept a name its newest does not permit gets a
-- new one beside the old.
--
-- sandboxes.http_pushed is the version of the clear part of the pod's HTTP
-- bindings (PodSpec.http_bindings) that the pod's node took last, by the
-- create's spec or SetPodHttpBindings, for the pod of the row's
-- incarnation; '' for none, as every pod made before had. A row whose
-- pod's part differs from it gets SetPodHttpBindings.
--
-- env_incidents gains the conditions of an environment's binding (ADR
-- 0028 §6), on the object binding/<name>: credential_refused,
-- upstream_failing, token_failing.
--
-- Additive: an instance of the previous version neither reads nor writes
-- the new columns and tables, and a sandbox's delete cascades to them; it
-- opens none of the new conditions and reads them as any other.

ALTER TABLE sandboxes ADD COLUMN http_version text NOT NULL DEFAULT '';
ALTER TABLE sandboxes ADD COLUMN http_pushed text NOT NULL DEFAULT '';

CREATE TABLE http_bindings (
  sandbox_id text COLLATE "C" NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
  name text NOT NULL CHECK (name ~ '^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$'),
  upstream text NOT NULL,                      -- https://host[:port], canonical
  mode text NOT NULL CHECK (mode IN ('name','intercept')),
  ca_pem text NOT NULL DEFAULT '',
  path_prefixes text[] NOT NULL DEFAULT '{}',
  forwarded_host boolean NOT NULL DEFAULT false,
  rate_per_second integer NOT NULL DEFAULT 0 CHECK (rate_per_second >= 0),
  rate_burst integer NOT NULL DEFAULT 0 CHECK (rate_burst >= 0),
  credential_kind text NOT NULL CHECK (credential_kind IN ('headers','basic','oauth2','clientCertificate')),
  sealed bytea NOT NULL,                       -- 0x01 || nonce(12) || AES-256-GCM(credential JSON)
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (sandbox_id, name));

CREATE TABLE pod_intercept_cas (
  sandbox_id text COLLATE "C" NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
  seq integer NOT NULL CHECK (seq >= 1),
  hosts text[] NOT NULL,                       -- sorted, lowercase: the CA's permitted DNS names
  cert_pem text NOT NULL,
  sealed_key bytea NOT NULL,                   -- 0x01 || nonce(12) || AES-256-GCM(PKCS#8 PEM)
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (sandbox_id, seq));

ALTER TABLE env_incidents DROP CONSTRAINT env_incidents_condition_check;
ALTER TABLE env_incidents ADD CONSTRAINT env_incidents_condition_check
  CHECK (condition IN ('rollout_failed','crash_loop','job_failed','no_capacity',
     'addon_unhealthy','volume_unavailable','attach_failed','names_unserved',
     'domain_record_wrong','domain_deactivated','domain_superseded','certificate_failing',
     'credential_refused','upstream_failing','token_failing'));
