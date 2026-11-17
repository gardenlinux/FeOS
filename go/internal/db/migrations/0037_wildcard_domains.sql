-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- Wildcard domains (ADR 0045, "A wildcard is a domain of its own"): a
-- claim may name *.<name>, the * the whole first label. Its TXT record
-- stands at <--domain-challenge-label>.<name>, its certificate comes over
-- DNS-01, which its owner delegates with a CNAME of
-- _acme-challenge.<name> to <token>.<--acme-zone>; that delegation is
-- part of its routing. domains.delegated is what the last answer showed
-- of it (false for a name that is not a wildcard), so that the reasons
-- name the record that is wrong; domains.routed stays the one fact the
-- states and the orders read, for a wildcard both records right.
--
-- 0037: 0035 is left to the environments' credentials.
--
-- Additive: an instance of the release before, which a rolling deploy
-- runs beside this one, never reads the new column, and while both run a
-- wildcard claimed meanwhile is only checked and ordered wrong by the old
-- one, which verifies and serves nothing of it.

ALTER TABLE domains DROP CONSTRAINT domains_name_check;
ALTER TABLE domains ADD CONSTRAINT domains_name_check
  CHECK (name ~ '^(\*\.)?([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$'
         AND length(name) <= 253);             -- lowercase A-labels, no trailing dot, a wildcard's * first

ALTER TABLE domains ADD COLUMN delegated boolean NOT NULL DEFAULT false;   -- a wildcard's _acme-challenge CNAME named its target at the last answer
