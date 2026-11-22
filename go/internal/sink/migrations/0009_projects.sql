-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- What FeRun called a tenant is a project (ADR 0061): the streams' and
-- the usage tables' column says project, and a stream's labels the key
-- ferun.project. The indexes log_streams_env, _job, _instance and _last
-- follow the column. log_streams.release stays, as before. Quickwit's
-- documents keep their field tenant (log mapping v5, trace mapping v4).

ALTER TABLE log_streams RENAME COLUMN tenant TO project;
ALTER TABLE log_usage RENAME COLUMN tenant TO project;
ALTER TABLE trace_usage RENAME COLUMN tenant TO project;
UPDATE log_streams SET labels = (labels - 'ferun.tenant') || jsonb_build_object('ferun.project', labels->'ferun.tenant')
  WHERE labels ? 'ferun.tenant';
