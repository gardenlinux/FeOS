-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- The states' words, the last of three steps (ADR 0018, "The states'
-- words"): the CHECKs of env_releases.state and env_instances.phase admit
-- the new words only. What the version of 0021 wrote in the earlier words
-- while 0022's rollout ran is rewritten first, as 0022 rewrote the rest.
--
-- Additive for the version of 0022, which a rolling deploy runs beside
-- this one: it writes the new words only. Not for the version of 0021,
-- whose writes of the earlier words the CHECKs refuse from here on: this
-- version rolls only once 0022's rollout has ended and no instance of
-- 0021 is left, also none that is still stopping.

UPDATE env_releases SET state = CASE state
    WHEN 'pending' THEN 'waiting' WHEN 'rolling' THEN 'deploying' WHEN 'realised' THEN 'deployed'
    ELSE 'replaced' END
  WHERE state IN ('pending','rolling','realised','superseded');
ALTER TABLE env_releases DROP CONSTRAINT env_releases_state_check;
ALTER TABLE env_releases ADD CONSTRAINT env_releases_state_check
  CHECK (state IN ('waiting','deploying','deployed','failed','replaced'));

UPDATE env_instances SET phase = 'stopping' WHERE phase = 'retiring';
ALTER TABLE env_instances DROP CONSTRAINT env_instances_phase_check;
ALTER TABLE env_instances ADD CONSTRAINT env_instances_phase_check
  CHECK (phase IN ('starting','ready','unready','exited','succeeded','failed','stopping'));
