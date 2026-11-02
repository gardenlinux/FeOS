-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- The states' words, the second of three steps (ADR 0018, "The states'
-- words"): the release states and the stopping phase are stored in their
-- new words, and environments.realised goes.
--
-- The CHECKs of env_releases.state and env_instances.phase admit both
-- words, the earlier ones for the version of 0021; the rows are rewritten
-- to the new ones, updated_at kept (the state is the same, its word is
-- new), without a notification: what a state or a phase means to the
-- controller and the ingress has not changed. A new release defaults to
-- waiting. The trigger of 0021 goes with its function and realised.
--
-- Additive for the version of 0021, which a rolling deploy runs beside
-- this one: it reads both words and writes the earlier ones, which the
-- CHECKs admit; it reads and writes deployed and never names realised, so
-- that it needs neither the column nor the trigger. A version before 0021,
-- which reads realised, does not run on this schema. The next version
-- rewrites what the version of 0021 wrote meanwhile and lets the CHECKs
-- admit the new words only; it needs this version's rollout to have ended.

ALTER TABLE env_releases DROP CONSTRAINT env_releases_state_check;
ALTER TABLE env_releases ADD CONSTRAINT env_releases_state_check
  CHECK (state IN ('waiting','deploying','deployed','failed','replaced',
     'pending','rolling','realised','superseded'));
ALTER TABLE env_releases ALTER COLUMN state SET DEFAULT 'waiting';
UPDATE env_releases SET state = CASE state
    WHEN 'pending' THEN 'waiting' WHEN 'rolling' THEN 'deploying' WHEN 'realised' THEN 'deployed'
    ELSE 'replaced' END
  WHERE state IN ('pending','rolling','realised','superseded');

ALTER TABLE env_instances DROP CONSTRAINT env_instances_phase_check;
ALTER TABLE env_instances ADD CONSTRAINT env_instances_phase_check
  CHECK (phase IN ('starting','ready','unready','exited','succeeded','failed','stopping','retiring'));
UPDATE env_instances SET phase = 'stopping' WHERE phase = 'retiring';

DROP TRIGGER environments_deployed_sync ON environments;
DROP FUNCTION environments_deployed_sync();
ALTER TABLE environments DROP COLUMN realised;
