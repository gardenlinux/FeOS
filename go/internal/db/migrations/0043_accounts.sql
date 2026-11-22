-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- A project is on an account, which says whom its use is charged to (ADR
-- 0059; docs/architecture/control-plane.md, "Accounts").
--
-- accounts: whom the operator charges. Its id is its name, formed as a
-- project's; reference is free text the operator's accounting
-- understands (a cost centre, an order, a customer's number), which FeRun
-- keeps and reads nothing into. A closed account (closed_at set) takes no
-- project, new or moved, and keeps the ones it has. made_by and closed_by
-- are principal strings, as every made_by.
--
-- account_members: the people who may put projects on an account, and
-- among them its admins, who also invite and remove its people. Being on
-- an account gives no role in its projects. The account default holds
-- member rows only: its admins are the operators.
--
-- project_accounts: a project's accounts over time, not one field. Each
-- row is the account the project came to at moved_at, brought by moved_by
-- (its maker or whoever moved it; '' for this migration's backfill), and
-- left at left_at; the row with left_at NULL is the account it is on now,
-- one per project. A move closes the current row and opens the next at
-- the same now(). The rows go with their project; an account a row names
-- is not deleted. The account is in nothing that names a project or what
-- is in it: no name, host, route, identity, sink key or path.
--
-- invitations gain the account as a scope: an invitation is to a project
-- (or one of its environments) with the role read, write or admin, or to
-- an account with the role member or admin.
--
-- Every installation has the account default, and every project there is
-- is on it from the moment it was made.
--
-- Additive: the release before reads none of the new tables and makes
-- projects without a row in project_accounts, which every read takes as
-- the account default. It reads invitations by hash; an account
-- invitation, whose project_id is NULL, fails its read as a server error
-- while it runs beside this release. This release starts after every
-- instance of it is stopped anyway (0041).

CREATE TABLE accounts (
  id text PRIMARY KEY CHECK (id ~ '^[a-z0-9][a-z0-9.-]{1,62}$'),
  title text NOT NULL DEFAULT '',
  reference text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  made_by text NOT NULL DEFAULT '',
  closed_at timestamptz,
  closed_by text NOT NULL DEFAULT '');

CREATE TABLE account_members (
  account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  person_id uuid NOT NULL REFERENCES people(id) ON DELETE CASCADE,
  role text NOT NULL CHECK (role IN ('member', 'admin')),
  made_by text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (account_id, person_id),
  CHECK (account_id <> 'default' OR role = 'member'));
CREATE INDEX account_members_person ON account_members (person_id);

CREATE TABLE project_accounts (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  project_id text NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  account_id text NOT NULL REFERENCES accounts(id),
  moved_at timestamptz NOT NULL DEFAULT now(),
  moved_by text NOT NULL DEFAULT '',
  left_at timestamptz,
  CHECK (left_at IS NULL OR left_at >= moved_at));
CREATE UNIQUE INDEX project_accounts_current ON project_accounts (project_id) WHERE left_at IS NULL;
CREATE INDEX project_accounts_project ON project_accounts (project_id, moved_at);
CREATE INDEX project_accounts_account ON project_accounts (account_id, moved_at);

ALTER TABLE invitations ALTER COLUMN project_id DROP NOT NULL;
ALTER TABLE invitations ADD COLUMN account_id text REFERENCES accounts(id) ON DELETE CASCADE;
ALTER TABLE invitations DROP CONSTRAINT invitations_role_check;
ALTER TABLE invitations ADD CONSTRAINT invitations_scope CHECK (
  (project_id IS NOT NULL AND account_id IS NULL AND role IN ('read', 'write', 'admin'))
  OR (account_id IS NOT NULL AND project_id IS NULL AND environment_id IS NULL
      AND role IN ('member', 'admin')));
CREATE INDEX invitations_account ON invitations (account_id, created_at) WHERE account_id IS NOT NULL;

-- default is as old as the oldest project on it.
INSERT INTO accounts (id, title, created_at)
  SELECT 'default', 'Default', coalesce(min(created_at), now()) FROM projects;
INSERT INTO project_accounts (project_id, account_id, moved_at, moved_by)
  SELECT id, 'default', created_at, '' FROM projects;
