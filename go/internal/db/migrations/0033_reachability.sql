-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- Reachability from declarations (ADR 0034, revised; ADR 0035, revised:
-- "One release, no mode").
--
-- A tenant's security groups are gone: what a pod reaches is derived from
-- its deployment's references, a sandbox's and a tenant's job's from
-- their reaches. This undoes what 0031 and 0032 added for groups and
-- moves and converts what they wrote:
--
--   - every deployment's spec loses securityGroups, the top-level map and
--     the list on every service, job and addon (a member that is a JSON
--     object; other shapes are left as they are). All deployments, not
--     only the latest and the deployed: an older one is reverted to or
--     promoted, and the plan refuses the field. The specs hold nothing
--     but 0031's migrated rule (default admitting default) and FeCode's
--     workspace group, both replaced by what the plan derives;
--   - sandboxes and jobs get reaches, what of their environment they
--     reach ("all" or a list of targets, [] nothing), and egress (null:
--     the internet). A sandbox or a job in FeCode's group workspace
--     reaches all, one in default nothing; an instance's are unused;
--   - every pod template gains its workload's group, wl-<id>, beside
--     what it has: an instance's the first 32 hex digits of the SHA-256
--     of "<environment>/<kind>/<object>" (env_instances; an instance
--     without its row is left as it is), a sandbox's wl-s-<id>, a
--     tenant's job's pod's wl-j-<id> (the job's ID). The sg- entries stay:
--     ferund builds them as a transition group until the pod is
--     replaced. A job's pod's template without a list gets
--     [env-<environment>, its wl]; a sandbox's own that is not of ADR
--     0035 yet (0032's moves that never ran: NULL, sbx-<id>, no
--     env-<environment>) gets the whole template a sandbox is made with;
--     a tenant's job's pod gets its job's reaches;
--   - the table security_groups, the columns security_groups,
--     groups_pending and move_restore and what goes with them are
--     dropped.
--
-- Not additive: the version before reads and writes security_groups,
-- which this drops. Stop it before. Every image service, job and addon
-- rolls once after it, its template's hash changing with its groups
-- (ferund's side).
--
-- Each statement is safe to run twice, though the migrator runs it once.

-- 1. Specs without securityGroups; only those that change are written.
UPDATE env_deployments d SET spec = n.spec
  FROM (SELECT x.env_id, x.number, (x.spec - 'securityGroups')
      || CASE WHEN jsonb_typeof(x.spec->'services') = 'object' THEN
        jsonb_build_object('services', (SELECT coalesce(jsonb_object_agg(m.key, CASE WHEN jsonb_typeof(m.value) = 'object'
            THEN m.value - 'securityGroups' ELSE m.value END), '{}'::jsonb)
          FROM jsonb_each(x.spec->'services') AS m))
        ELSE '{}'::jsonb END
      || CASE WHEN jsonb_typeof(x.spec->'jobs') = 'object' THEN
        jsonb_build_object('jobs', (SELECT coalesce(jsonb_object_agg(m.key, CASE WHEN jsonb_typeof(m.value) = 'object'
            THEN m.value - 'securityGroups' ELSE m.value END), '{}'::jsonb)
          FROM jsonb_each(x.spec->'jobs') AS m))
        ELSE '{}'::jsonb END
      || CASE WHEN jsonb_typeof(x.spec->'addons') = 'object' THEN
        jsonb_build_object('addons', (SELECT coalesce(jsonb_object_agg(m.key, CASE WHEN jsonb_typeof(m.value) = 'object'
            THEN m.value - 'securityGroups' ELSE m.value END), '{}'::jsonb)
          FROM jsonb_each(x.spec->'addons') AS m))
        ELSE '{}'::jsonb END AS spec
    FROM env_deployments x WHERE jsonb_typeof(x.spec) = 'object') n
  WHERE d.env_id = n.env_id AND d.number = n.number AND d.spec <> n.spec;

-- 2. and 3. What a sandbox and a job reach, from their groups.
ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS reaches jsonb NOT NULL DEFAULT '[]'::jsonb
  CONSTRAINT sandboxes_reaches CHECK (reaches = '"all"'::jsonb OR jsonb_typeof(reaches) = 'array');
ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS egress jsonb;    -- null: the internet
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS reaches jsonb NOT NULL DEFAULT '[]'::jsonb
  CONSTRAINT jobs_reaches CHECK (reaches = '"all"'::jsonb OR jsonb_typeof(reaches) = 'array');
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS egress jsonb;         -- null: the internet

-- The groups are read only while they exist (a second run finds them
-- dropped; PL/pgSQL plans a statement when it runs it).
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = 'sandboxes'::regclass
      AND attname = 'security_groups' AND NOT attisdropped) THEN
    UPDATE sandboxes SET reaches = '"all"'::jsonb
      WHERE NOT instance AND NOT tenant_job AND 'workspace' = ANY (security_groups) AND reaches <> '"all"'::jsonb;
  END IF;
  IF EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = 'jobs'::regclass
      AND attname = 'security_groups' AND NOT attisdropped) THEN
    UPDATE jobs SET reaches = '"all"'::jsonb
      WHERE 'workspace' = ANY (security_groups) AND reaches <> '"all"'::jsonb;
  END IF;
END
$$;

-- A tenant's job's pod reaches what its job does.
UPDATE sandboxes s SET reaches = j.reaches FROM jobs j
  WHERE s.id = j.id AND s.tenant_job AND s.reaches <> j.reaches;

-- 4. Every pod template in its workload's group.
--
-- A sandbox of its own whose template is not one of ADR 0035 yet (the
-- moves of 0032 that never ran, groups_pending): NULL, or the attachable
-- one's {"securityGroups": ["sbx-<id>"], "nameTables": ["sbx-<id>"]},
-- or any other without env-<env> or with an sbx- group. Nothing makes it
-- again after this migration (the moves are gone), so it gets the
-- template a sandbox is made with (model.SandboxTemplate): sleep
-- infinity, env-<env> and wl-s-<id>, the table env-<env>. Its pod, where
-- it runs, keeps what its node gave it until its next restore.
UPDATE sandboxes SET pod_template = CASE WHEN jsonb_typeof(pod_template) = 'object' THEN pod_template
      ELSE '{}'::jsonb END
    || jsonb_build_object('command', jsonb_build_array('sleep', 'infinity'),
      'securityGroups', jsonb_build_array('env-' || environment_id::text, 'wl-s-' || id),
      'nameTables', jsonb_build_array('env-' || environment_id::text))
  WHERE NOT instance AND NOT tenant_job AND (
    jsonb_typeof(pod_template) IS DISTINCT FROM 'object'
    OR jsonb_typeof(pod_template->'securityGroups') IS DISTINCT FROM 'array'
    OR NOT (pod_template->'securityGroups') ? ('env-' || environment_id::text)
    OR EXISTS (SELECT 1 FROM jsonb_array_elements_text(pod_template->'securityGroups') g WHERE g LIKE 'sbx-%')
    OR jsonb_typeof(pod_template->'nameTables') IS DISTINCT FROM 'array'
    OR EXISTS (SELECT 1 FROM jsonb_array_elements_text(pod_template->'nameTables') t WHERE t LIKE 'sbx-%'));
UPDATE sandboxes s SET pod_template = jsonb_set(s.pod_template, '{securityGroups}',
    (s.pod_template->'securityGroups') || jsonb_build_array(w.wl))
  FROM (SELECT ei.sandbox_id, 'wl-' || left(encode(sha256(convert_to(
      ei.env_id::text || '/' || ei.kind || '/' || ei.object, 'UTF8')), 'hex'), 32) AS wl
    FROM env_instances ei) w
  WHERE w.sandbox_id = s.id AND s.instance
    AND jsonb_typeof(s.pod_template) = 'object' AND jsonb_typeof(s.pod_template->'securityGroups') = 'array'
    AND NOT (s.pod_template->'securityGroups') ? w.wl;
UPDATE sandboxes SET pod_template = jsonb_set(pod_template, '{securityGroups}',
    (pod_template->'securityGroups')
      || jsonb_build_array(CASE WHEN tenant_job THEN 'wl-j-' ELSE 'wl-s-' END || id))
  WHERE NOT instance
    AND jsonb_typeof(pod_template) = 'object' AND jsonb_typeof(pod_template->'securityGroups') = 'array'
    AND NOT (pod_template->'securityGroups') ? (CASE WHEN tenant_job THEN 'wl-j-' ELSE 'wl-s-' END || id);
UPDATE sandboxes SET pod_template = pod_template || jsonb_build_object('securityGroups', jsonb_build_array(
    'env-' || environment_id::text, CASE WHEN tenant_job THEN 'wl-j-' ELSE 'wl-s-' END || id))
  WHERE NOT instance
    AND jsonb_typeof(pod_template) = 'object' AND jsonb_typeof(pod_template->'securityGroups') IS DISTINCT FROM 'array';

-- 5. What the first version added for groups and moves.
DROP INDEX IF EXISTS sandboxes_groups_pending;
ALTER TABLE sandboxes DROP CONSTRAINT IF EXISTS sandboxes_security_groups;
ALTER TABLE sandboxes DROP COLUMN IF EXISTS security_groups;
ALTER TABLE sandboxes DROP COLUMN IF EXISTS groups_pending;
ALTER TABLE sandboxes DROP COLUMN IF EXISTS move_restore;
ALTER TABLE jobs DROP COLUMN IF EXISTS security_groups;
DROP TABLE IF EXISTS security_groups;
