// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package env is ferund's environments service (ADR 0018): typed objects
// of a tenant (services, jobs, addons, routes, secrets) that run
// together, changed only by numbered, immutable deployments.
//
// The service answers the REST API's environment routes (internal/api
// registers them; Service has one method per operation): environments,
// plan and apply, deployments, revert, object puts and deletes, clone, tags,
// secrets, incidents, logs, restarts, job runs and route tokens; and it
// serves the route door (ServeRoute), which forwards HTTP requests to the
// environment's services by their routes. It composes the controller
// (internal/env/ctl), whose leader loops deploy the deployments, and gives
// the engine its hooks: the secrets' resolution (ResolveSecret) and the
// preparation of a node (PrepareHook).
//
// Every pod of an environment is a sandbox row with environment_id set,
// placed and run by the sandbox engine from its pod_template. The DB
// holds no secret values: env_secrets holds them sealed; templates name
// them by name and version.
//
// Subpackages: spec (the deployment spec: parse, validate, normalise, diff),
// addons (the addon blueprints), ctl (the controller), envdeps (the
// interfaces both this package and ctl reach the instance through).
package env
