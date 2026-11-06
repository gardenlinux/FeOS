-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- The tenant zone (ADR 0031, "The tenants people make get their hosts under
-- a zone of their own"; docs/architecture/control-plane.md, "Public
-- environments").
--
-- A default host is a label in a zone: the apps zone (tenants.zone NULL)
-- or the tenant's zone (tenants.zone, 0027). host_labels registers a label
-- per zone, so that a label taken in the tenant zone is not taken in the
-- apps zone, and the labels of the platform's products stay out of reach
-- of whoever makes a tenant: zone is '' for the apps zone, else the zone's
-- name, as tenants.zone holds it. Every label registered so far is the
-- apps zone's. environments.host_label stops being unique on its own: two
-- environments of different zones may hold the same label, and
-- host_labels.env_id (UNIQUE) with its key (zone, label) keeps one host per
-- environment and one environment per host.
--
-- Not additive for one statement: an instance of the previous version
-- claims a label with ON CONFLICT (label), which has no constraint to
-- match from here on ("there is no unique or exclusion constraint
-- matching the ON CONFLICT specification"), so its first publishes of an
-- environment (PUT …/public true on one that never had a label) and its
-- chosen labels (PUT …/public with hostLabel) answer 500 until the rolling deploy
-- has replaced it; a retry on a new instance succeeds. Its reads are
-- unchanged. No index keeps it working: ON CONFLICT (label) infers only a
-- unique index on label alone without a predicate (a partial one WHERE
-- zone = '' it does not take), and a unique label across zones is what
-- this migration ends: a person's tenant would block the apps zone's
-- plain names, and a tenant moved to another zone could not keep its
-- labels. Keeping the old rows' label unique beside a second column for
-- the other zones' would keep it working, at the price of a schema of
-- two label columns for the window of one rollout; not worth it.

ALTER TABLE host_labels ADD COLUMN zone text NOT NULL DEFAULT '';
ALTER TABLE host_labels DROP CONSTRAINT host_labels_pkey;
ALTER TABLE host_labels ADD PRIMARY KEY (zone, label);
DROP INDEX environments_host_label;
