-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- A sandbox's working directory (docs/architecture/control-plane.md,
-- "Tools and MCP").
--
-- sandboxes.workdir is the absolute directory a sandbox's tools start in:
-- bash runs there, and the file tools take a relative path as relative to
-- it. It is fixed at the create. '' is the root, as every sandbox had it
-- before, so an existing sandbox stays as it is, and a create by the
-- release before writes nothing here.
--
-- 0035 is left to the environments' credentials, which take it when they
-- land.

ALTER TABLE sandboxes ADD COLUMN workdir text NOT NULL DEFAULT '';
