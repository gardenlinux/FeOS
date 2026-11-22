-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- A project has its own label, its environments <environment>--<project>
-- (ADR 0063). Every environment that holds the natural label of ADR 0045,
-- <environment>-<project>, gets the natural label of ADR 0063,
-- <environment>--<project>, in its project's zone, where model.HostLabels
-- finds it usable (the project's name is a label of its own, the whole at
-- most 63 characters and not R-LDH: an environment of two characters does
-- not get it) and nobody else holds it; else its fallback,
-- <name>-<16 hex of the SHA-256 of its ID>, the name's first 46
-- characters (a name of a natural label has no hyphen, so it is no R-LDH
-- one and needs no trimming), as at a first publish. One whose fallback
-- another project or environment holds, by a hash's collision, keeps its
-- old label. The old label stays its project's,
-- free, as a changed label's does. Chosen labels, fallbacks and the zone
-- owners' plain labels stay where they are.

-- A label of a project that is gone, held by no environment, that names
-- a project in the zone in ADR 0063's forms (<project> or
-- <choice>--<project>) is that project's: FeCode's environments ran in
-- its FeRun project fecode until FeCode became a layer (ADR 0056), and
-- the labels FeCode chose there, <environment>--<project> and <project>,
-- are the hosts of the projects they moved to. It runs first, so that an
-- environment gets its natural label where fecode held it.
UPDATE host_labels h SET owner = p.id
  FROM projects p
 WHERE h.env_id IS NULL
   AND NOT EXISTS (SELECT 1 FROM projects g WHERE g.id = h.owner)
   AND h.zone = coalesce(p.zone, '')
   AND strpos(p.id, '.') = 0 AND strpos(p.id, '--') = 0 AND p.id !~ '-[0-9a-f]{16}$'
   AND (h.label = p.id
        OR (length(h.label) > length(p.id) + 2
            AND right(h.label, length(p.id) + 2) = '--' || p.id
            AND left(h.label, length(h.label) - length(p.id) - 2) ~ '^[a-z0-9]([a-z0-9-]*[a-z0-9])?$'
            AND strpos(left(h.label, length(h.label) - length(p.id) - 2), '--') = 0));

CREATE TEMP TABLE project_labels_relabel ON COMMIT DROP AS
SELECT e.id, e.owner, coalesce(p.zone, '') AS zone,
       e.name || '--' || e.owner AS natural,
       left(e.name, 46) || '-' || left(encode(sha256(convert_to(e.id::text, 'UTF8')), 'hex'), 16) AS fallback,
       NULL::text AS label
  FROM environments e JOIN projects p ON p.id = e.owner
 WHERE e.host_label = e.name || '-' || e.owner
   AND strpos(e.name, '-') = 0
   AND strpos(e.owner, '.') = 0 AND strpos(e.owner, '--') = 0
   AND e.owner !~ '-[0-9a-f]{16}$';

UPDATE project_labels_relabel r SET label = r.natural
 WHERE length(r.natural) <= 63 AND substr(r.natural, 3, 2) <> '--'
   AND NOT EXISTS (SELECT 1 FROM host_labels h WHERE h.zone = r.zone AND h.label = r.natural
                      AND (h.owner <> r.owner OR (h.env_id IS NOT NULL AND h.env_id <> r.id)));

UPDATE project_labels_relabel r SET label = r.fallback
 WHERE r.label IS NULL
   AND NOT EXISTS (SELECT 1 FROM host_labels h WHERE h.zone = r.zone AND h.label = r.fallback
                      AND (h.owner <> r.owner OR (h.env_id IS NOT NULL AND h.env_id <> r.id)));

DELETE FROM project_labels_relabel WHERE label IS NULL;

UPDATE host_labels SET env_id = NULL WHERE env_id IN (SELECT id FROM project_labels_relabel);

INSERT INTO host_labels (zone, label, owner, env_id)
SELECT zone, label, owner, id FROM project_labels_relabel
    ON CONFLICT (zone, label) DO UPDATE SET env_id = excluded.env_id, assigned_at = now();

UPDATE environments e SET host_label = r.label, updated_at = now()
  FROM project_labels_relabel r WHERE e.id = r.id;

-- The ingress instances build their table again.
SELECT pg_notify('ferun_ingress', '*');
