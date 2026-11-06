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
-- match from here on, so its first publishes and chosen labels fail until
-- it is replaced in the rolling deploy. Its reads are unchanged.

ALTER TABLE host_labels ADD COLUMN zone text NOT NULL DEFAULT '';
ALTER TABLE host_labels DROP CONSTRAINT host_labels_pkey;
ALTER TABLE host_labels ADD PRIMARY KEY (zone, label);
DROP INDEX environments_host_label;
