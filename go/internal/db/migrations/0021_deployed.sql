-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- The states' words (ADR 0018, "The states' words"): an environment's
-- realised release is its deployed release, environments.deployed, which
-- this version reads and writes in place of environments.realised.
--
-- Additive: an instance of the previous version, which a rolling deploy
-- runs beside this one, reads and writes realised. Until the next
-- version, a trigger keeps the two columns equal: a write of deployed
-- sets realised, a write of realised alone sets deployed. The next
-- version drops the trigger, its function and realised, which this one
-- never names.
--
-- The release states and the stopping phase keep their stored words
-- (pending, rolling, realised, superseded; retiring) in this version: the
-- previous version, beside it, would take a release in a state it does
-- not know for one that is not rolling out. This version reads both words
-- and writes the earlier ones (internal/store/env_words.go); the next
-- writes the new ones, lets the CHECKs of env_releases.state and
-- env_instances.phase admit both and rewrites the rows; the one after
-- lets them admit the new words only.

ALTER TABLE environments
  ADD COLUMN deployed integer NOT NULL DEFAULT 0 CHECK (deployed >= 0);   -- the latest deployed release; 0: none
UPDATE environments SET deployed = realised;

CREATE FUNCTION environments_deployed_sync() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    NEW.deployed := greatest(NEW.deployed, NEW.realised);
  ELSIF NEW.deployed IS NOT DISTINCT FROM OLD.deployed THEN
    NEW.deployed := NEW.realised;
  END IF;
  NEW.realised := NEW.deployed;
  RETURN NEW;
END $$;

CREATE TRIGGER environments_deployed_sync BEFORE INSERT OR UPDATE ON environments
  FOR EACH ROW EXECUTE FUNCTION environments_deployed_sync();
