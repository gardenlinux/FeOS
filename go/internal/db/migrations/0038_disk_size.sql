-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- A disk size per pod (docs/architecture/control-plane.md, "Environments"
-- and "Placement"): an environment's image service or job may ask for a
-- disk larger than the guest's 8 GiB (resources.diskGib), which the pod's
-- spec carries (feos.pod.v1.Resources.disk_gib). The disk is thin and
-- scratch: it does not outlive its pod.
--
-- sandboxes.disk_gib is the row's disk, fixed at the insert; 0 is the
-- guest's, as every row had it before. bookings.disk_gib is what a pod
-- books of its node's disk: its disk_gib, 0 for the guest's disk, which
-- the node's slots and --disk-budget cover as before.
--
-- 0038: 0035 is left to the environments' credentials.
--
-- Additive: an instance of the release before, which a rolling deploy
-- runs beside this one, inserts rows and bookings of 0 and reads neither
-- column; a row with a disk that it creates a pod of gets the guest's.

ALTER TABLE sandboxes ADD COLUMN disk_gib integer NOT NULL DEFAULT 0
  CHECK (disk_gib >= 0 AND disk_gib <= 1024);  -- 0: the guest's disk; the spec allows 8..200

ALTER TABLE bookings ADD COLUMN disk_gib integer NOT NULL DEFAULT 0
  CHECK (disk_gib >= 0);                       -- the pod's own disk on the node, 0 for the guest's
