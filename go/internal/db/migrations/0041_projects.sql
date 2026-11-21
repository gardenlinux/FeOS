-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- What FeRun called a tenant is a project (ADR 0061). Not additive: an
-- instance of the release before reads tenants and tenant_tokens, so this
-- release starts once every instance of it is stopped.
--
-- The tables, columns, indexes and constraints that said tenant say
-- project; the provider kind tenant (ADR 0054) is project; the log label
-- ferun.tenant a sandbox keeps is ferun.project; the activity kinds and
-- their detail key follow. tenant_credentials, empty and refused by its
-- trigger since 0035 and read by nothing, goes. The lease of the loop
-- renamed sandbox-project-deletion goes with its old name.

ALTER TABLE tenants RENAME TO projects;
ALTER INDEX tenants_pkey RENAME TO projects_pkey;
ALTER INDEX tenants_made_by RENAME TO projects_made_by;
ALTER TABLE projects RENAME CONSTRAINT tenants_id_check TO projects_id_check;
ALTER TABLE projects RENAME CONSTRAINT tenants_made_by_fkey TO projects_made_by_fkey;
ALTER TABLE projects RENAME CONSTRAINT tenants_limit_sandboxes_check TO projects_limit_sandboxes_check;
ALTER TABLE projects RENAME CONSTRAINT tenants_limit_environments_check TO projects_limit_environments_check;
ALTER TABLE projects RENAME CONSTRAINT tenants_limit_running_jobs_check TO projects_limit_running_jobs_check;
ALTER TABLE projects RENAME CONSTRAINT tenants_limit_vcpus_check TO projects_limit_vcpus_check;
ALTER TABLE projects RENAME CONSTRAINT tenants_limit_memory_mib_check TO projects_limit_memory_mib_check;

ALTER TABLE tenant_tokens RENAME TO project_tokens;
ALTER TABLE project_tokens RENAME COLUMN tenant_id TO project_id;
ALTER INDEX tenant_tokens_pkey RENAME TO project_tokens_pkey;
ALTER INDEX tenant_tokens_tenant RENAME TO project_tokens_project;
ALTER TABLE project_tokens RENAME CONSTRAINT tenant_tokens_tenant_id_fkey TO project_tokens_project_id_fkey;
ALTER TABLE project_tokens RENAME CONSTRAINT tenant_tokens_environment_id_fkey TO project_tokens_environment_id_fkey;
ALTER TABLE project_tokens RENAME CONSTRAINT tenant_tokens_role_check TO project_tokens_role_check;

DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM tenant_credentials) THEN
    RAISE EXCEPTION 'tenant_credentials holds rows; see migration 0035';
  END IF;
END $$;
DROP TABLE tenant_credentials;
DROP FUNCTION tenant_credentials_refuse();

ALTER TABLE grants RENAME COLUMN tenant_id TO project_id;
ALTER TABLE grants RENAME CONSTRAINT grants_tenant_id_fkey TO grants_project_id_fkey;
ALTER TABLE invitations RENAME COLUMN tenant_id TO project_id;
ALTER INDEX invitations_tenant RENAME TO invitations_project;
ALTER TABLE invitations RENAME CONSTRAINT invitations_tenant_id_fkey TO invitations_project_id_fkey;

ALTER TABLE access_providers RENAME COLUMN tenant TO project;
ALTER TABLE access_providers RENAME CONSTRAINT access_providers_tenant_fkey TO access_providers_project_fkey;
ALTER TABLE access_providers DROP CONSTRAINT access_providers_kind_check,
  DROP CONSTRAINT access_providers_tenant_kind;
UPDATE access_providers SET kind = 'project' WHERE kind = 'tenant';
ALTER TABLE access_providers
  ADD CONSTRAINT access_providers_kind_check CHECK (kind IN ('project', 'oidc')),
  ADD CONSTRAINT access_providers_project_kind CHECK (kind <> 'project' OR
    (url <> '' AND issuer = '' AND client_id = '' AND client_secret_sealed IS NULL));

ALTER TABLE visits RENAME COLUMN tenant TO project;
ALTER TABLE visits DROP CONSTRAINT visits_provider_kind_check;
UPDATE visits SET provider_kind = 'project' WHERE provider_kind = 'tenant';
ALTER TABLE visits ADD CONSTRAINT visits_provider_kind_check
  CHECK (provider_kind IN ('project', 'oidc', 'ferun'));

ALTER TABLE activity RENAME COLUMN tenant_id TO project_id;          -- recurses into partitions
ALTER TABLE traffic_records RENAME COLUMN tenant_id TO project_id;   -- recurses into partitions

ALTER TABLE sandboxes RENAME COLUMN tenant_job TO project_job;
ALTER INDEX sandboxes_tenant_job RENAME TO sandboxes_project_job;
UPDATE sandboxes SET log_labels = (log_labels - 'ferun.tenant')
    || jsonb_build_object('ferun.project', log_labels->'ferun.tenant')
  WHERE log_labels ? 'ferun.tenant';

UPDATE activity SET kind = 'project' || substr(kind, 7)
  WHERE kind IN ('tenant_created', 'tenant_zone_set', 'tenant_limits');
UPDATE activity SET name = regexp_replace(name, '^(limits of )?tenant ', '\1project ')
  WHERE kind IN ('project_created', 'project_zone_set', 'project_limits');
UPDATE activity SET detail = (detail - 'tenant') || jsonb_build_object('project', detail->'tenant')
  WHERE detail ? 'tenant';

DELETE FROM leases WHERE name = 'sandbox-tenant-deletion';
