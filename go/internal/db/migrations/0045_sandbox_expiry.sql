-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- A sandbox expires after its idle time (ADR 0065). expires_after_idle_s
-- is a caller's sandbox's idle time in seconds, NULL for never; a create
-- writes the caller's value or the installation's default. Every row that
-- exists now gets NULL: nothing made before expiry existed expires unless
-- its creator sets a time. An expired sandbox keeps its row, has no pod
-- and no checkpoint, and expired_at says when it expired.
ALTER TABLE sandboxes ADD COLUMN expires_after_idle_s bigint
  CHECK (expires_after_idle_s IS NULL OR expires_after_idle_s >= 3600);
ALTER TABLE sandboxes ADD COLUMN expired_at timestamptz;

ALTER TABLE sandboxes DROP CONSTRAINT sandboxes_client_state_check;
ALTER TABLE sandboxes ADD CONSTRAINT sandboxes_client_state_check CHECK (client_state IN ('pending','creating',
  'running','pausing','paused','resuming','hibernating','hibernated','stopped','failed','unknown','lost','expired'));
ALTER TABLE sandboxes ADD CHECK ((client_state = 'expired') = (expired_at IS NOT NULL));

-- The expiry loop's candidates: a caller's sandbox with an idle time that
-- does not run.
CREATE INDEX sandboxes_expiry ON sandboxes(last_active_at)
  WHERE expires_after_idle_s IS NOT NULL AND op = ''
    AND client_state IN ('pending','paused','hibernated','stopped','failed','lost');
