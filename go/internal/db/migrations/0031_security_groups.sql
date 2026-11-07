-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- Security groups of a tenant's own (ADR 0034, its core).
--
-- security_groups names an environment's groups: a row per name that a
-- deployment of the environment introduced, its UUID the group's ID on the
-- nodes ("sg-<uuid>"), so that a name may be long and a copy of an
-- environment gets groups of its own. An insert of a deployment adds the
-- rows of the groups its spec has (store.InsertDeployment), an insert of
-- an environment the row of its group default, which every environment
-- has. A row stays while the environment does: deleting the rows that no
-- deployment names any more is left to a cleanup of later.
--
-- Existing environments keep what they had, every pod reaching every
-- other: their latest and their deployed deployments' specs get the
-- group default with the rule of today (in from default, any protocol;
-- out to the internet, as every group without out rules) and every image
-- service, job and addon the groups [default], which is the normalised
-- form the specs of this version have (spec.Normalise). A deployment's
-- spec is immutable; this writes what it meant, and the older
-- deployments, which nothing runs, keep theirs: a revert to one is
-- normalised again, closed. Environments made after this start closed.
--
-- Additive for the version before this one, which a rolling deploy runs
-- beside it: it reads stored specs with encoding/json, which passes over
-- the new fields, and never names security_groups (a deployment it
-- inserts gets its rows from this version's first reader of them,
-- store.EnsureSecurityGroup).

CREATE TABLE security_groups (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  env_id uuid NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
  name text NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9-]{0,31}$'),
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (env_id, name)
);

INSERT INTO security_groups (env_id, name) SELECT id, 'default' FROM environments;

UPDATE env_deployments d SET spec = d.spec
    || jsonb_build_object('securityGroups', jsonb_build_object('default', jsonb_build_object(
      'in', jsonb_build_array(jsonb_build_object('protocol', 'any', 'from', jsonb_build_object('group', 'default'))),
      'out', jsonb_build_array(jsonb_build_object('protocol', 'any', 'to', 'internet')))))
    || CASE WHEN jsonb_typeof(d.spec->'services') = 'object' AND d.spec->'services' <> '{}'::jsonb THEN
      jsonb_build_object('services', (SELECT jsonb_object_agg(s.key, CASE WHEN s.value->>'runtime' = 'attached'
          THEN s.value ELSE s.value || '{"securityGroups": ["default"]}'::jsonb END)
        FROM jsonb_each(d.spec->'services') AS s))
      ELSE '{}'::jsonb END
    || CASE WHEN jsonb_typeof(d.spec->'jobs') = 'object' AND d.spec->'jobs' <> '{}'::jsonb THEN
      jsonb_build_object('jobs', (SELECT jsonb_object_agg(j.key, j.value || '{"securityGroups": ["default"]}'::jsonb)
        FROM jsonb_each(d.spec->'jobs') AS j))
      ELSE '{}'::jsonb END
    || CASE WHEN jsonb_typeof(d.spec->'addons') = 'object' AND d.spec->'addons' <> '{}'::jsonb THEN
      jsonb_build_object('addons', (SELECT jsonb_object_agg(a.key, a.value || '{"securityGroups": ["default"]}'::jsonb)
        FROM jsonb_each(d.spec->'addons') AS a))
      ELSE '{}'::jsonb END
  FROM environments e
  WHERE d.env_id = e.id AND d.number IN (e.deployment, e.deployed) AND NOT d.spec ? 'securityGroups';
