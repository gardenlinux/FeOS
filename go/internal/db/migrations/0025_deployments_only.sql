-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- The deployments' word, the second of two steps (ADR 0018, "The
-- deployments' word"): what 0024 kept for the version before it goes,
-- and the stored specs say a deploy job's on in its new word.
--
-- Every stored spec's job on "release" becomes "deploy" (the earlier word
-- stays a synonym for good: a spec sent with it is normalised to deploy).
-- A deployment's spec is immutable; this changes its word, not what it
-- means, and no template hash takes on, so no job runs again. The view
-- env_releases, the triggers of 0024, their function, the release columns
-- of environments, env_instances, env_job_runs and env_incidents and the
-- index env_incidents_open on release go; env_incidents_open_deployment
-- takes the name env_incidents_open.
--
-- Additive for the version of 0024, which a rolling deploy runs beside
-- this one: it names env_deployments and the deployment columns only, and
-- reads a deploy job's on in either word; what it still stores as
-- release, it and this one read as deploy. Not for a version before 0024,
-- which reads env_releases and the release columns: this version rolls
-- only once 0024's rollout has ended and no instance before it is left.

UPDATE env_deployments d SET spec = jsonb_set(d.spec, '{jobs}', (
    SELECT jsonb_object_agg(j.key, CASE WHEN j.value->>'on' = 'release'
      THEN jsonb_set(j.value, '{on}', '"deploy"') ELSE j.value END)
    FROM jsonb_each(d.spec->'jobs') AS j))
  WHERE jsonb_typeof(d.spec->'jobs') = 'object'
    AND jsonb_path_exists(d.spec, '$.jobs.* ? (@.on == "release")');

DROP VIEW env_releases;

DROP TRIGGER environments_deployment_sync ON environments;
DROP TRIGGER env_instances_deployment_sync ON env_instances;
DROP TRIGGER env_job_runs_deployment_sync ON env_job_runs;
DROP TRIGGER env_incidents_deployment_sync ON env_incidents;
DROP FUNCTION deployment_sync();

ALTER TABLE environments DROP COLUMN release;
ALTER TABLE env_instances DROP COLUMN release;
ALTER TABLE env_job_runs DROP COLUMN release;
ALTER TABLE env_incidents DROP COLUMN release;   -- and env_incidents_open with it
ALTER INDEX env_incidents_open_deployment RENAME TO env_incidents_open;
