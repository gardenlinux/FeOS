// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/env/ctl"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/routing"
	"github.com/gardenlinux/feos/go/internal/store"
)

// status assembles an environment's status (§4.9) from the tables: its
// latest deployment's rollout; the objects of its target deployment (the
// latest, or the deployed one while the latest failed) with their
// instances (env_instances with their sandbox rows: state, node,
// address), attached services (env_attached) and jobs' latest runs
// (env_job_runs); the routes of its deployed deployment (the latest when
// none is deployed), with the doors that serve them; and its open
// incidents. It reads only.
func (s *Service) status(ctx context.Context, e store.Environment) (EnvStatus, error) {
	hosts, err := s.hosts(ctx, e)
	if err != nil {
		return EnvStatus{}, fmt.Errorf("hosts: %w", err)
	}
	access, provider := accessOf(e)
	t, err := s.st.GetProject(ctx, e.Owner)
	if err != nil {
		return EnvStatus{}, fmt.Errorf("the zone of project %s: %w", e.Owner, err)
	}
	st := EnvStatus{ID: e.ID.String(), Name: e.Name, Labels: e.Labels, Deployment: e.Deployment, Deployed: e.Deployed,
		Deleting: e.Deleting, Public: e.Public, Hosts: hosts, HostLabel: e.HostLabel, ProjectAddress: holdsAddress(e),
		ProjectHost: projectHost(e.Owner, s.zones().Of(t.Zone)), Access: access, Provider: provider,
		Protected: e.Protected, Objects: []ObjectStatus{}, Routes: []RouteStatus{}, CreatedAt: rfc3339(e.CreatedAt)}
	if st.Labels == nil {
		st.Labels = map[string]string{}
	}
	open, err := s.st.CountOpenIncidents(ctx, e.ID)
	if err != nil {
		return EnvStatus{}, fmt.Errorf("incidents: %w", err)
	}
	st.OpenIncidents = int(open)
	if e.Deployment == 0 {
		return st, nil
	}
	latest, err := s.st.GetDeployment(ctx, e.ID, e.Deployment)
	if err != nil {
		return EnvStatus{}, fmt.Errorf("deployment %d: %w", e.Deployment, err)
	}
	st.Rollout = &RolloutStatus{Deployment: latest.Number, State: latest.State, StartedAt: rfc3339(latest.StartedAt),
		Reason: latest.Reason}
	target := e.Deployment
	if latest.State == model.DeploymentFailed && e.Deployed > 0 {
		target = e.Deployed
	}
	targetSpec, err := s.deploymentSpec(ctx, e, target)
	if err != nil {
		return EnvStatus{}, err
	}
	routed := e.Deployment
	if e.Deployed > 0 {
		routed = e.Deployed
	}
	routeSpec, err := s.deploymentSpec(ctx, e, routed)
	if err != nil {
		return EnvStatus{}, err
	}
	instances, err := s.st.ListEnvInstanceRows(ctx, e.ID)
	if err != nil {
		return EnvStatus{}, fmt.Errorf("instances: %w", err)
	}
	attached, err := s.st.ListAttached(ctx, e.ID)
	if err != nil {
		return EnvStatus{}, fmt.Errorf("attached services: %w", err)
	}
	runs, err := s.st.LatestJobRuns(ctx, e.ID)
	if err != nil {
		return EnvStatus{}, fmt.Errorf("job runs: %w", err)
	}
	st.Objects = objectsOf(targetSpec, instances, attached, runs)
	st.Routes = routesOf(routeSpec, hosts)
	return st, nil
}

// objectsOf are the objects of a target spec: services, addons and jobs,
// each by name. An image service's ready count is of its live instances,
// as the controller counts them (ctl.Live).
func objectsOf(sp *spec.Spec, instances []store.EnvInstanceRow, attached []store.EnvAttached,
	runs []store.EnvJobRun) []ObjectStatus {
	byObject := map[string][]store.EnvInstanceRow{}
	for _, row := range instances {
		k := row.Instance.Kind + "/" + row.Instance.Object
		byObject[k] = append(byObject[k], row)
	}
	attachedOf := map[string]store.EnvAttached{}
	for _, a := range attached {
		attachedOf[a.Service] = a
	}
	runOf := map[string]store.EnvJobRun{}
	for _, r := range runs {
		runOf[r.Job] = r
	}
	out := []ObjectStatus{}
	for _, name := range slices.Sorted(maps.Keys(sp.Services)) {
		svc := sp.Services[name]
		if svc.Runtime == spec.RuntimeAttached {
			o := ObjectStatus{Kind: spec.KindService, Name: name, Runtime: spec.RuntimeAttached, Sandbox: svc.Sandbox,
				Phase: model.PhaseStarting}
			if a, ok := attachedOf[name]; ok {
				o.Sandbox, o.Phase, o.Restarts, o.LastExit, o.Detail = a.SandboxID.String(), a.Phase, a.Restarts,
					a.LastExit, a.Detail
			}
			out = append(out, o)
			continue
		}
		o := ObjectStatus{Kind: spec.KindService, Name: name, Runtime: spec.RuntimeImage, Desired: spec.DefaultReplicas}
		if svc.Replicas != nil {
			o.Desired = *svc.Replicas
		}
		for _, row := range byObject[spec.KindService+"/"+name] {
			if ctl.Live(row, svc) {
				o.Ready++
			}
			o.Instances = append(o.Instances, instanceStatus(row))
		}
		out = append(out, o)
	}
	for _, name := range slices.Sorted(maps.Keys(sp.Addons)) {
		a := sp.Addons[name]
		o := ObjectStatus{Kind: spec.KindAddon, Name: name, Type: a.Type, Version: a.Version, Plan: a.Plan,
			Phase: model.PhaseStarting}
		var current *store.EnvInstance
		for _, row := range byObject[spec.KindAddon+"/"+name] {
			o.Instances = append(o.Instances, instanceStatus(row))
			if row.Instance.Phase != model.PhaseStopping {
				i := row.Instance
				current = &i
			}
		}
		if current != nil {
			o.Phase, o.Provisioned = current.Phase, current.ProvisionedHash != ""
		}
		out = append(out, o)
	}
	for _, name := range slices.Sorted(maps.Keys(sp.Jobs)) {
		o := ObjectStatus{Kind: spec.KindJob, Name: name}
		if r, ok := runOf[name]; ok {
			o.LastRun = &JobRunStatus{Deployment: r.Deployment, Attempt: r.Attempt, Phase: r.Phase, ExitCode: r.ExitCode,
				FinishedAt: rfc3339(r.FinishedAt), Detail: r.Detail}
		}
		out = append(out, o)
	}
	return out
}

// instanceStatus is an instance with its sandbox row.
func instanceStatus(row store.EnvInstanceRow) InstanceStatus {
	i, sb := row.Instance, row.Sandbox
	detail := i.Detail
	// A stop's row stays running while its containers drain, and says so.
	if detail == "" && (sb.ClientState != model.StateRunning || sb.Op != "") {
		detail = sb.StatusDescription
	}
	return InstanceStatus{ID: sb.ID.String(), Replica: i.Replica, Phase: i.Phase, State: sb.ClientState, Node: sb.Node,
		Address: sb.Address, Deployment: i.Deployment, Restarts: i.Restarts, LastExit: i.LastExit,
		ReadySince: rfc3339(i.ReadySince), Detail: detail, Image: sb.ImageRef}
}

// routesOf are a spec's routes by name, their ports resolved, each with
// its host and its door: the ingress serves a route to an image service
// while the environment has a host that is an address, not a wildcard,
// among hosts (its hosts, hostsOf), the route door alone every other.
// A route of the ingress has a URL where it answers its own path (ADR
// 0045): one with a host at that host while the environment is served
// there, one without at the first such address where no route of that
// host takes its path; a route whose host is a wildcard has none, as a
// wildcard is no address.
func routesOf(sp *spec.Spec, hosts []string) []RouteStatus {
	out := []RouteStatus{}
	addrs := slices.DeleteFunc(slices.Clone(hosts), routing.IsWildcard)
	names := slices.Sorted(maps.Keys(sp.Routes))
	prefixes, routeHosts := make([]string, len(names)), make([]string, len(names))
	for i, name := range names {
		prefixes[i], routeHosts[i] = spec.RoutePrefix(sp.Routes[name].Path), spec.RouteHost(sp.Routes[name].Host)
	}
	for i, name := range names {
		r := sp.Routes[name]
		target, ok := sp.Services[r.Service]
		port, _ := spec.PortNumber(target.Ports, r.Port)
		rs := RouteStatus{Name: name, Path: r.Path, Host: routeHosts[i], Service: r.Service, Port: port, Door: DoorRoute}
		if len(addrs) > 0 && ok && !spec.IsAttached(target) {
			rs.Door = DoorIngress
			at := addrs
			if h := routeHosts[i]; h != "" {
				at = nil
				if !routing.IsWildcard(h) && spec.CoveredBy(hosts, h) {
					at = []string{h}
				}
			}
			for _, h := range at {
				if j, _, ok := routing.MatchHost(prefixes, routeHosts, h, prefixes[i]); ok && j == i {
					rs.URL = "https://" + h + prefixes[i]
					break
				}
			}
		}
		out = append(out, rs)
	}
	return out
}

// hostsOf are the hosts the ingress serves environment e at while it is
// public: the names of its served domains in the order they were bound,
// else its default host under zone (none without a zone), and after them
// its served wildcard domains, which are no address and retire no
// default host (ADR 0045); none while it is not public or is being
// deleted, which the ingress no longer serves. An environment on a domain
// of its own is not served at its default host (ADR 0021).
func hostsOf(e store.Environment, zone string, domains []string) []string {
	if !e.Public || e.Deleting {
		return []string{}
	}
	names := slices.DeleteFunc(slices.Clone(domains), routing.IsWildcard)
	wildcards := slices.DeleteFunc(slices.Clone(domains), func(d string) bool { return !routing.IsWildcard(d) })
	if len(names) == 0 && zone != "" && e.HostLabel != "" {
		names = []string{e.HostLabel + "." + zone}
	}
	return append(names, wildcards...)
}

// hosts are e's hosts (Hosts).
func (s *Service) hosts(ctx context.Context, e store.Environment) ([]string, error) {
	return Hosts(ctx, s.st, s.zones(), e)
}

// Hosts are the hosts the ingress serves environment e at, as
// EnvStatus.Hosts has them: its served domains in the order they were
// bound, else its default host under its project's zone of zones; none
// while it is not public or is being deleted. It reads the domains and
// the project on q. The visits service checks a challenge's host with it
// (ADR 0054).
func Hosts(ctx context.Context, q *store.Store, zones model.Zones, e store.Environment) ([]string, error) {
	if !e.Public || e.Deleting {
		return []string{}, nil
	}
	ds, err := q.ServedDomainNames(ctx, []uuid.UUID{e.ID})
	if err != nil {
		return nil, err
	}
	t, err := q.GetProject(ctx, e.Owner)
	if err != nil {
		return nil, fmt.Errorf("the zone of project %s: %w", e.Owner, err)
	}
	return hostsOf(e, zones.Of(t.Zone), ds[e.ID]), nil
}

// accessOf is an environment's stored access as the answers show it: the
// access, and with visitors its provider's name (model.ProviderFerun for
// the builtin); "" with anyone.
func accessOf(e store.Environment) (access, provider string) {
	switch {
	case e.Access != model.AccessVisitors:
		return model.AccessAnyone, ""
	case e.AccessProvider == "":
		return model.AccessVisitors, model.ProviderFerun
	}
	return model.AccessVisitors, e.AccessProvider
}

// summaryOf is an environment as its project's listing shows it, with its
// hosts under zone.
func summaryOf(r store.EnvSummaryRow, zone string, domains []string) EnvSummary {
	state := r.DeploymentState
	switch {
	case r.Deleting:
		state = model.EnvDeleting
	case state == "":
		state = model.EnvNone
	}
	labels := r.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	access, provider := accessOf(r.Environment)
	return EnvSummary{ID: r.ID.String(), Name: r.Name, Labels: labels, Deployment: r.Deployment, Deployed: r.Deployed,
		State: state, Public: r.Public, Hosts: hostsOf(r.Environment, zone, domains), HostLabel: r.HostLabel,
		ProjectAddress: holdsAddress(r.Environment), ProjectHost: projectHost(r.Owner, zone), Access: access,
		Provider: provider, Protected: r.Protected, OpenIncidents: int(r.OpenIncidents), CreatedAt: rfc3339(r.CreatedAt)}
}

// holdsAddress reports whether e holds its project's address, the
// project's name as its label (ADR 0063).
func holdsAddress(e store.Environment) bool { return e.HostLabel != "" && e.HostLabel == e.Owner }

// projectHost is the project's address under zone, <project>.<zone>; ""
// without a zone, and for a project whose name is no label of its own
// (model.ProjectLabel).
func projectHost(project, zone string) string {
	if zone == "" || !model.ProjectLabel(project) {
		return ""
	}
	return project + "." + zone
}

// incidentOf is an incident as the answers show it.
func incidentOf(i store.EnvIncident) Incident {
	var ev IncidentEvidence
	if len(i.Evidence) > 0 {
		_ = json.Unmarshal(i.Evidence, &ev)
	}
	return Incident{ID: fmt.Sprint(i.ID), Object: i.Object, Deployment: i.Deployment, Condition: i.Condition,
		OpenedAt: rfc3339(i.OpenedAt), ClosedAt: rfc3339(i.ClosedAt), Summary: i.Summary, Evidence: ev}
}
