-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- A secret is a value or an HTTP API (ADR 0055;
-- docs/architecture/control-plane.md, "Secrets").
--
-- Every key of an environment is a secret, and its kind says where it
-- lives: a value secret is a row of env_secrets, as before, and an HTTP
-- secret is what ADR 0033 called a credential, its versions in
-- env_credentials and its high-water mark in env_credential_names, as
-- before. Both kinds share one name space per environment: a name is a
-- value or an HTTP API, never both, and a secret's kind does not change.
--
-- The migration refuses while an environment has a name in both, naming
-- each: the console listed such a name twice, and a variable that
-- references it could not say which it meant. Either is deleted with the
-- release before (DELETE /api/v1/environments/{env}/secrets/{name} or
-- …/credentials/{name}) and, if it is still needed, made again under
-- another name.
--
-- From then on a trigger refuses the insert of either kind while the
-- other holds the name in the environment: a value secret holds it while
-- it has a version that is not deleted (a system secret, "addon.…", is no
-- DNS label and never an HTTP secret's name), an HTTP secret while it has
-- a version. It takes the advisory lock a credential's writes take
-- (store.LockEnvCredential: the key "FECR" and the environment's ID and
-- the name), so that two writes of the two kinds to one name serialise
-- and the second sees the first. ferund checks the same before it writes
-- and answers 409 secret_kind; the trigger is the rule for every writer,
-- the release before among them during a rolling update, whose write it
-- refuses as a server error.
--
-- Additive otherwise: no table or column changes.

DO $$
DECLARE
  n bigint;
  names text;
BEGIN
  SELECT count(*), string_agg(format('%s in environment %s', c.name, c.env_id), ', ' ORDER BY c.env_id, c.name)
    INTO n, names
    FROM (SELECT DISTINCT env_id, name FROM env_credentials) c
    WHERE EXISTS (SELECT 1 FROM env_secrets s WHERE s.env_id = c.env_id AND s.name = c.name AND NOT s.system
      AND s.deleted_at IS NULL);
  IF n > 0 THEN
    RAISE EXCEPTION '%', format('%s names are both a secret and a credential of their environment (%s): a secret '
      || 'is a value or an HTTP API, never both (ADR 0055). With the release before still running, delete one of '
      || 'each (DELETE /api/v1/environments/{env}/secrets/{name} or /api/v1/environments/{env}/credentials/{name}), '
      || 'then start this release; make it again under another name if it is still needed.', n, names);
  END IF;
END
$$;

CREATE FUNCTION env_secrets_kind() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.system OR NEW.deleted_at IS NOT NULL THEN
    RETURN NEW;
  END IF;
  PERFORM pg_advisory_xact_lock(1178944338, hashtext(NEW.env_id::text || '/' || NEW.name));
  IF EXISTS (SELECT 1 FROM env_credentials c WHERE c.env_id = NEW.env_id AND c.name = NEW.name) THEN
    RAISE EXCEPTION USING ERRCODE = 'check_violation', CONSTRAINT = 'secret_kind',
      MESSAGE = format('secret %s is an HTTP secret of its environment; a secret''s kind does not change: delete '
        || 'it and make a new one', NEW.name);
  END IF;
  RETURN NEW;
END $$;

CREATE FUNCTION env_credentials_kind() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM pg_advisory_xact_lock(1178944338, hashtext(NEW.env_id::text || '/' || NEW.name));
  IF EXISTS (SELECT 1 FROM env_secrets s WHERE s.env_id = NEW.env_id AND s.name = NEW.name AND NOT s.system
      AND s.deleted_at IS NULL) THEN
    RAISE EXCEPTION USING ERRCODE = 'check_violation', CONSTRAINT = 'secret_kind',
      MESSAGE = format('secret %s is a value of its environment; a secret''s kind does not change: delete it and '
        || 'make a new one', NEW.name);
  END IF;
  RETURN NEW;
END $$;

CREATE TRIGGER env_secrets_kind BEFORE INSERT ON env_secrets
  FOR EACH ROW EXECUTE FUNCTION env_secrets_kind();
CREATE TRIGGER env_credentials_kind BEFORE INSERT ON env_credentials
  FOR EACH ROW EXECUTE FUNCTION env_credentials_kind();
