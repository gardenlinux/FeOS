-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- Everything runs in an environment (ADR 0035, docs/architecture/
-- control-plane.md, "Sandboxes and jobs in an environment").
--
-- Every row of sandboxes belongs to an environment: environment_id is
-- not null. What it is says the row's kind, fixed at its insert:
--
--   instance    an environment's instance (env_instances), its pod made
--               from its deployment's template;
--   tenant_job  a tenant's job's pod, under the job's ID;
--   neither     a sandbox (POST /api/v1/sandboxes), the tenant's own.
--
-- Until now environment_id marked an instance; instance takes that over.
-- security_groups are the names of the groups of its environment (ADR
-- 0034) a sandbox's or a job's pod is in, 1 to 5; an instance's are its
-- template's and the column is empty. groups_pending marks a sandbox
-- whose pod runs under other groups than security_groups: the controller
-- moves it (hibernate, restore with the row's groups, ADR 0035) and
-- clears it, as any restore or create that makes its pod does. A job
-- names its groups in jobs.security_groups, copied to its pod's row; a
-- job's groups are fixed.
--
-- The data moves once, with no second mode:
--   - a sandbox goes into the environment whose latest deployment has an
--     attached service in it, the most recently created of several
--     (not one being deleted);
--   - every other sandbox and every job of a tenant (its record and its
--     pod) goes into the tenant's environment "sandboxes", made here,
--     empty, with its group default; a tenant that has one already keeps
--     it and gets its sandboxes and jobs there;
--   - all are in [default]; sandboxes with a pod are marked pending, so
--     that they are moved once; a hibernated one takes its groups at its
--     restore; a running job ends under the groups it has;
--   - log_labels carry the environment: a sandbox gets those of a
--     sandbox (tenant, instance, environment, kind sandbox, object its
--     ID), a job's pod its environment beside its own; a pod that runs
--     keeps the labels of its create until its next restore.
--
-- Not additive: the version before inserts sandboxes and jobs without an
-- environment, which the not-null columns refuse. Stop it before.

ALTER TABLE sandboxes ADD COLUMN instance boolean NOT NULL DEFAULT false;    -- an environment's instance; fixed
UPDATE sandboxes SET instance = true WHERE environment_id IS NOT NULL;
ALTER TABLE sandboxes ADD CONSTRAINT sandboxes_kind CHECK (NOT (instance AND tenant_job));
ALTER TABLE sandboxes ADD COLUMN security_groups text[] NOT NULL DEFAULT '{default}';
ALTER TABLE sandboxes ADD COLUMN groups_pending boolean NOT NULL DEFAULT false;
UPDATE sandboxes SET security_groups = '{}' WHERE instance;
ALTER TABLE sandboxes ADD CONSTRAINT sandboxes_security_groups CHECK (
  (instance AND cardinality(security_groups) = 0 AND NOT groups_pending)
  OR (NOT instance AND cardinality(security_groups) BETWEEN 1 AND 5));

ALTER TABLE jobs ADD COLUMN environment_id uuid REFERENCES environments(id) ON DELETE CASCADE;
ALTER TABLE jobs ADD COLUMN security_groups text[] NOT NULL DEFAULT '{default}'
  CHECK (cardinality(security_groups) BETWEEN 1 AND 5);

-- A sandbox that an environment's attached service names.
UPDATE sandboxes s SET environment_id = (SELECT e.id FROM environments e
    JOIN env_deployments d ON d.env_id = e.id AND d.number = e.deployment
    WHERE e.owner = s.owner AND NOT e.deleting AND EXISTS (
      SELECT 1 FROM jsonb_each(CASE WHEN jsonb_typeof(d.spec->'services') = 'object'
        THEN d.spec->'services' ELSE '{}'::jsonb END) svc
      WHERE svc.value->>'runtime' = 'attached' AND lower(svc.value->>'sandbox') = lower(s.id))
    ORDER BY e.created_at DESC, e.id LIMIT 1)
  WHERE NOT s.instance AND NOT s.tenant_job;

-- The tenants' environments "sandboxes", for the rest.
INSERT INTO environments (id, owner, name)
  SELECT gen_random_uuid(), t.owner, 'sandboxes' FROM (
    SELECT owner FROM sandboxes WHERE environment_id IS NULL
    UNION SELECT owner FROM jobs) t
  ON CONFLICT (owner, name) DO NOTHING;
INSERT INTO security_groups (env_id, name)
  SELECT id, 'default' FROM environments WHERE name = 'sandboxes'
  ON CONFLICT (env_id, name) DO NOTHING;
UPDATE sandboxes s SET environment_id = e.id FROM environments e
  WHERE s.environment_id IS NULL AND e.owner = s.owner AND e.name = 'sandboxes';
UPDATE jobs j SET environment_id = e.id FROM environments e
  WHERE e.owner = j.owner AND e.name = 'sandboxes';

ALTER TABLE sandboxes ALTER COLUMN environment_id SET NOT NULL;
ALTER TABLE jobs ALTER COLUMN environment_id SET NOT NULL;

UPDATE sandboxes SET log_labels = jsonb_build_object('ferun.tenant', owner, 'ferun.instance', id,
    'ferun.environment', environment_id::text, 'ferun.kind', 'sandbox', 'ferun.object', id)
  WHERE NOT instance AND NOT tenant_job;
UPDATE sandboxes SET log_labels = log_labels || jsonb_build_object('ferun.environment', environment_id::text)
  WHERE tenant_job;
UPDATE sandboxes SET groups_pending = true
  WHERE NOT instance AND NOT tenant_job AND client_state NOT IN ('pending', 'hibernating', 'hibernated');

DROP INDEX sandboxes_environment;
CREATE INDEX sandboxes_environment ON sandboxes(environment_id);
CREATE INDEX sandboxes_groups_pending ON sandboxes(id) WHERE groups_pending;
CREATE INDEX jobs_environment ON jobs(environment_id, created_at);
