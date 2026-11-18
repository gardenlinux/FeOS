-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- The stop op (docs/architecture/control-plane.md, "Ops"; ADR 0018): an
-- environment's instance that goes keeps its row until its pod has ended.
-- Once its traffic drained, the controller stops its pod with the op
-- stop (StopPod: SIGTERM and the pod's stop grace, an image service's
-- 660 s), during which the row stays running, booked and in its groups,
-- and deletes the row once the pod has stopped. Before, the row went
-- first and the pod's grace ran in its tombstone's DeletePod.
--
-- 0039: 0035 is left to the environments' credentials, 0038 to the disk
-- size of pods.
--
-- Additive: an instance of the release before, which a rolling deploy
-- runs beside this one, begins no stop and reads the column as text; a
-- stop it takes over as a stale op it calls nothing for, and the pod it
-- reads afterwards decides, as for any op.

ALTER TABLE sandboxes DROP CONSTRAINT sandboxes_op_check;
ALTER TABLE sandboxes ADD CONSTRAINT sandboxes_op_check
  CHECK (op IN ('','create','start','resume','restore','pause','hibernate','repair','stop'));
