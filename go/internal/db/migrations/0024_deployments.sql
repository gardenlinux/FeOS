-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- The deployments' word (ADR 0018, "The deployments' word"), the first of
-- two steps: an environment's numbered change of its spec is a deployment.
-- This version reads and writes env_deployments, in place of
-- env_releases, and the column deployment, in place of release, of
-- environments, env_instances, env_job_runs and env_incidents.
--
-- Additive: an instance of the previous version, which a rolling deploy
-- runs beside this one, reads and writes env_releases and the release
-- columns. Until the next version:
--
-- - env_releases is a view of env_deployments, every column as it is.
--   A view this simple is updatable as its table: the previous version's
--   INSERT … RETURNING, SELECT … FOR UPDATE, UPDATE … RETURNING and the
--   foreign key of env_tags (the table's, renamed with it) work on it as
--   they did; a column it does not name takes the table's default.
-- - Each release column stays beside its deployment column, the two kept
--   equal by a trigger, as 0021 kept realised and deployed: a write of
--   deployment sets release, a write of release alone sets deployment, and
--   an insert of either sets both.
-- - env_incidents_open stays for the previous version's ON CONFLICT on
--   release; env_incidents_open_deployment is this version's, on
--   deployment. The two columns are equal, so the two indexes refuse the
--   same rows.
--
-- The next version rewrites the stored specs' deploy jobs to their new
-- word and drops the view, the triggers, their function, the release
-- columns and env_incidents_open; this one never names them.

ALTER TABLE env_releases RENAME TO env_deployments;
ALTER TABLE env_deployments RENAME CONSTRAINT env_releases_pkey TO env_deployments_pkey;
ALTER TABLE env_deployments RENAME CONSTRAINT env_releases_env_id_fkey TO env_deployments_env_id_fkey;
ALTER TABLE env_deployments RENAME CONSTRAINT env_releases_number_check TO env_deployments_number_check;
ALTER TABLE env_deployments RENAME CONSTRAINT env_releases_base_check TO env_deployments_base_check;
ALTER TABLE env_deployments RENAME CONSTRAINT env_releases_origin_check TO env_deployments_origin_check;
ALTER TABLE env_deployments RENAME CONSTRAINT env_releases_state_check TO env_deployments_state_check;

CREATE VIEW env_releases AS SELECT env_id, number, spec, base, author, message, annotations, origin, origin_ref,
  state, reason, started_at, finished_at, created_at, updated_at FROM env_deployments;

ALTER TABLE environments
  ADD COLUMN deployment integer NOT NULL DEFAULT 0 CHECK (deployment >= 0);   -- the latest desired deployment; 0: none
UPDATE environments SET deployment = release;

ALTER TABLE env_instances ADD COLUMN deployment integer;   -- the deployment whose target it was last kept for
UPDATE env_instances SET deployment = release;
ALTER TABLE env_instances ALTER COLUMN deployment SET NOT NULL;

ALTER TABLE env_job_runs ADD COLUMN deployment integer;
UPDATE env_job_runs SET deployment = release;
ALTER TABLE env_job_runs ALTER COLUMN deployment SET NOT NULL;

ALTER TABLE env_incidents ADD COLUMN deployment integer;
UPDATE env_incidents SET deployment = release;
ALTER TABLE env_incidents ALTER COLUMN deployment SET NOT NULL;
CREATE UNIQUE INDEX env_incidents_open_deployment ON env_incidents(env_id, object, deployment, condition)
  WHERE closed_at IS NULL;

-- The previous version's columns have no default but environments.release
-- (0): an insert of this version names deployment alone, one of the
-- previous names release alone, and NOT NULL is checked after the
-- trigger. greatest() passes over NULL.
CREATE FUNCTION deployment_sync() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    NEW.deployment := greatest(NEW.deployment, NEW.release);
  ELSIF NEW.deployment IS NOT DISTINCT FROM OLD.deployment THEN
    NEW.deployment := NEW.release;
  END IF;
  NEW.release := NEW.deployment;
  RETURN NEW;
END $$;

CREATE TRIGGER environments_deployment_sync BEFORE INSERT OR UPDATE ON environments
  FOR EACH ROW EXECUTE FUNCTION deployment_sync();
CREATE TRIGGER env_instances_deployment_sync BEFORE INSERT OR UPDATE ON env_instances
  FOR EACH ROW EXECUTE FUNCTION deployment_sync();
CREATE TRIGGER env_job_runs_deployment_sync BEFORE INSERT OR UPDATE ON env_job_runs
  FOR EACH ROW EXECUTE FUNCTION deployment_sync();
CREATE TRIGGER env_incidents_deployment_sync BEFORE INSERT OR UPDATE ON env_incidents
  FOR EACH ROW EXECUTE FUNCTION deployment_sync();
