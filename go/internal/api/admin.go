// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/config"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The label of the token a tenant's creation returns.
const tenantTokenLabel = "default"

// mintTenantToken returns a new tenant token and its hash: 32 random bytes
// in hex. Only the hash is stored; the token is shown once.
func mintTenantToken() (token, hash string, err error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", err
	}
	token = hex.EncodeToString(b[:])
	return token, store.HashToken(token), nil
}

// adminOfRequest is who made an admin request, for the records of who
// acted.
func adminOfRequest(r *http.Request) string {
	a, _ := auth.AdminFrom(r.Context())
	return a.User
}

// record writes an audit row of an operator's act (best effort: a failed
// write is logged, the act stands).
func (s *server) record(ctx context.Context, row store.ActivityRow) {
	row.TsNs = time.Now().UnixNano()
	row.InstanceID = s.cfg.InstanceID
	row.Source = store.SourceFerund
	if err := s.st.InsertActivity(context.WithoutCancel(ctx), row); err != nil {
		s.log.Warn("record an activity row", "kind", row.Kind, "err", err)
	}
}

// detail renders an activity row's detail object.
func detail(m map[string]any) json.RawMessage {
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}

// ---------------------------------------------------------------- tenants --

// tenantTokenInfo is a tenant token as listed: its hash, never the token.
type tenantTokenInfo struct {
	Hash       string `json:"hash"`
	HashPrefix string `json:"hashPrefix"`
	Label      string `json:"label"`
	// Role and Environment are the token's own grant (ADR 0031):
	// Environment is its scope's environment ID, "" for the tenant.
	Role        string `json:"role"`
	Environment string `json:"environment,omitempty"`
	MadeBy      string `json:"madeBy,omitempty"`
	CreatedAt   string `json:"createdAt"`
	LastUsedAt  string `json:"lastUsedAt,omitempty"`
}

func tenantTokenOf(t store.TenantToken) tenantTokenInfo {
	env := ""
	if t.EnvironmentID != uuid.Nil {
		env = t.EnvironmentID.String()
	}
	return tenantTokenInfo{Hash: t.Hash, HashPrefix: t.Hash[:8], Label: t.Label, Role: t.Role, Environment: env,
		MadeBy: t.MadeBy, CreatedAt: rfc3339(t.CreatedAt), LastUsedAt: rfc3339(t.LastUsedAt)}
}

type tenantInfo struct {
	Name         string `json:"name"`
	CreatedAt    string `json:"createdAt"`
	SandboxCount int64  `json:"sandboxCount"`
	// EnvironmentCount is its environments not being deleted, which its
	// deletion takes along with their addons' data.
	EnvironmentCount int64             `json:"environmentCount"`
	Tokens           []tenantTokenInfo `json:"tokens"`
	// Deleting is set while a deletion is under way.
	Deleting bool `json:"deleting,omitempty"`
	// MadeBy is the person who made it (ADR 0031); "": an operator.
	MadeBy string     `json:"madeBy,omitempty"`
	Limits limitsView `json:"limits"`
	// Zone is the zone of its default hosts (ADR 0031): the apps zone's
	// name or the tenant zone's, "" while the platform serves neither.
	Zone string `json:"zone"`
}

// listTenants is GET /api/v1/admin/tenants, ordered by name.
func (s *server) listTenants(w http.ResponseWriter, r *http.Request) error {
	ts, err := s.st.ListTenants(r.Context())
	if err != nil {
		return fmt.Errorf("list tenants: %w", err)
	}
	out := struct {
		Tenants []tenantInfo `json:"tenants"`
	}{Tenants: make([]tenantInfo, 0, len(ts))}
	for _, t := range ts {
		ti := tenantInfo{Name: t.ID, CreatedAt: rfc3339(t.CreatedAt), SandboxCount: t.SandboxCount,
			EnvironmentCount: t.EnvironmentCount, Tokens: make([]tenantTokenInfo, 0, len(t.Tokens)), Deleting: t.Deleting(),
			MadeBy: idOrEmpty(t.MadeBy), Limits: limitsOf(t.Limits),
			Zone: s.cfg.Zones.Of(t.Zone)}
		for _, tk := range t.Tokens {
			ti.Tokens = append(ti.Tokens, tenantTokenOf(tk))
		}
		out.Tenants = append(out.Tenants, ti)
	}
	return writeJSON(w, http.StatusOK, out)
}

// createTenant is POST /api/v1/admin/tenants {name}: the tenant and its
// first token in one transaction; the token is shown once.
func (s *server) createTenant(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Name string `json:"name"`
	}
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	if !store.TenantNamePattern.MatchString(req.Name) {
		return apierr.New(http.StatusBadRequest, "tenant name must match %s", store.TenantNamePattern)
	}
	token, hash, err := mintTenantToken()
	if err != nil {
		return fmt.Errorf("create tenant: %w", err)
	}
	var t store.Tenant
	err = s.st.InTx(r.Context(), func(tx *store.Store) error {
		var created bool
		var err error
		t, created, err = tx.CreateTenant(r.Context(), req.Name)
		if err != nil {
			return err
		}
		if !created {
			if t.Deleting() {
				return apierr.New(http.StatusConflict, "tenant %s is being deleted", req.Name)
			}
			return apierr.New(http.StatusConflict, "tenant %s already exists", req.Name)
		}
		a, _ := auth.AdminFrom(r.Context())
		_, err = tx.InsertTenantToken(r.Context(), store.TenantToken{Hash: hash, TenantID: req.Name,
			Label: tenantTokenLabel, Role: auth.RoleAdmin.String(), MadeBy: a.Principal().String()})
		return err
	})
	if err != nil {
		return passOr(err, "create tenant")
	}
	by := adminOfRequest(r)
	s.log.Info("tenant created", "tenant", req.Name, "by", by)
	s.record(r.Context(), store.ActivityRow{TenantID: req.Name, Kind: "tenant_created", Category: "lifecycle",
		Name: "tenant " + req.Name + " created by " + by, Detail: detail(map[string]any{"tenant": req.Name, "by": by})})
	return writeJSON(w, http.StatusCreated, struct {
		Name         string `json:"name"`
		Token        string `json:"token"`
		CreatedAt    string `json:"createdAt"`
		SandboxCount int    `json:"sandboxCount"`
	}{req.Name, token, rfc3339(t.CreatedAt), 0})
}

// zoneMove is one environment that moving its tenant to another zone
// moved: its label before and after, and its default host now.
type zoneMove struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	HostLabel string `json:"hostLabel"`
	Before    string `json:"hostLabelBefore"`
	Host      string `json:"host"`
}

// setTenantZone is PUT /api/v1/admin/tenants/{tenant}/zone {zone}: an
// operator moves the tenant to the apps zone or the tenant zone, named by
// its name (ADR 0031), and its environments' default hosts with it, as a
// changed label moves a host (store.SetTenantZone): the old hosts answer
// 404 at once. A label that cannot move, another tenant's in the new zone
// or a plain one where the tenant owns no plain names, gives way to the
// environment's natural label or its fallback. It answers the tenant's
// zone and every environment that holds a label; 400 for a zone the
// platform does not serve, 404 for an unknown tenant, 409 while it is
// being deleted.
func (s *server) setTenantZone(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("tenant")
	var req struct {
		Zone string `json:"zone"`
	}
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	zones := s.cfg.Zones
	zone, ok := zones.Column(req.Zone)
	if !ok {
		return apierr.New(http.StatusBadRequest, "zone must be one the platform serves, %s; got %q",
			strings.Join(zones.Names(), " or "), req.Zone)
	}
	var moves []store.ZoneMove
	var before string
	err := s.st.InTx(r.Context(), func(tx *store.Store) error {
		t, err := tx.LockTenant(r.Context(), name, store.ForUpdate)
		if isNoRow(err) {
			return apierr.New(http.StatusNotFound, "unknown tenant %s", name)
		}
		if err != nil {
			return err
		}
		if t.Deleting() {
			return apierr.New(http.StatusConflict, "tenant %s is being deleted", name)
		}
		before = t.Zone
		moves, err = tx.SetTenantZone(r.Context(), name, zone, zones.PlainLabels(name, zone))
		return err
	})
	if err != nil {
		return passOr(err, "set the tenant's zone")
	}
	host := zones.Of(zone)
	out := struct {
		Tenant       string     `json:"tenant"`
		Zone         string     `json:"zone"`
		Environments []zoneMove `json:"environments"`
	}{Tenant: name, Zone: host, Environments: make([]zoneMove, 0, len(moves))}
	for _, m := range moves {
		out.Environments = append(out.Environments, zoneMove{ID: m.EnvID.String(), Name: m.Name, HostLabel: m.To,
			Before: m.From, Host: m.To + "." + host})
	}
	if before != zone {
		by := adminOfRequest(r)
		s.log.Info("tenant moved to another zone", "tenant", name, "zone", host, "from", zones.Of(before),
			"environments", len(moves), "by", by)
		s.record(r.Context(), store.ActivityRow{TenantID: name, Kind: "tenant_zone_set", Category: "lifecycle",
			Name: "tenant " + name + " moved to zone " + host + " by " + by,
			Detail: detail(map[string]any{"tenant": name, "zone": host, "from": zones.Of(before), "by": by,
				"environments": out.Environments})})
	}
	return writeJSON(w, http.StatusOK, out)
}

// deleteTenant is DELETE /api/v1/admin/tenants/{tenant}: its tokens go at
// once, then its sandboxes in batches, each as a sandbox's delete, then
// the tenant. The leader finishes a deletion this request does not.
func (s *server) deleteTenant(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("tenant")
	existed, err := s.deps.Engine.DeleteTenant(r.Context(), name)
	if err != nil {
		return internalf("delete tenant: %v", err)
	}
	if existed {
		s.log.Info("tenant deleted", "tenant", name, "by", adminOfRequest(r))
	}
	return writeJSON(w, http.StatusOK, struct {
		Deleted bool `json:"deleted"`
	}{existed})
}

// lockTenantForToken reads the tenant of a token mint FOR SHARE: unknown
// is a 404, being deleted a 409.
func lockTenantForToken(ctx context.Context, tx *store.Store, name string) error {
	t, err := tx.LockTenant(ctx, name, store.ForShare)
	if isNoRow(err) {
		return apierr.New(http.StatusNotFound, "unknown tenant %s", name)
	}
	if err != nil {
		return err
	}
	if t.Deleting() {
		return apierr.New(http.StatusConflict, "tenant %s is being deleted", name)
	}
	return nil
}

// listTenantTokens is GET /api/v1/admin/tenants/{tenant}/tokens.
func (s *server) listTenantTokens(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("tenant")
	if _, err := s.st.GetTenant(r.Context(), name); isNoRow(err) {
		return apierr.New(http.StatusNotFound, "unknown tenant %s", name)
	} else if err != nil {
		return fmt.Errorf("list tokens: %w", err)
	}
	toks, err := s.st.ListTenantTokens(r.Context(), name)
	if err != nil {
		return fmt.Errorf("list tokens: %w", err)
	}
	out := struct {
		Tokens []tenantTokenInfo `json:"tokens"`
	}{Tokens: make([]tenantTokenInfo, 0, len(toks))}
	for _, t := range toks {
		out.Tokens = append(out.Tokens, tenantTokenOf(t))
	}
	return writeJSON(w, http.StatusOK, out)
}

// createTenantToken is POST /api/v1/admin/tenants/{tenant}/tokens
// {label?, role?, environment?}: another token of the tenant, shown once,
// with its own grant (ADR 0031): role read, write or admin (the default),
// on the environment of that ID or name, or on the tenant.
func (s *server) createTenantToken(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("tenant")
	var req struct {
		Label       string `json:"label"`
		Role        string `json:"role"`
		Environment string `json:"environment"`
	}
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	if req.Role == "" {
		req.Role = auth.RoleAdmin.String()
	}
	if _, ok := auth.ParseRole(req.Role); !ok {
		return apierr.New(http.StatusBadRequest, "role must be read, write or admin, got %q", req.Role)
	}
	token, hash, err := mintTenantToken()
	if err != nil {
		return fmt.Errorf("create token: %w", err)
	}
	var tok store.TenantToken
	err = s.st.InTx(r.Context(), func(tx *store.Store) error {
		if err := lockTenantForToken(r.Context(), tx, name); err != nil {
			return err
		}
		var env uuid.UUID
		if req.Environment != "" {
			id, err := tx.EnvironmentRef(r.Context(), name, req.Environment)
			if isNoRow(err) {
				return apierr.New(http.StatusNotFound, "tenant %s has no environment %s", name, req.Environment)
			}
			if err != nil {
				return err
			}
			env = id
		}
		a, _ := auth.AdminFrom(r.Context())
		var err error
		tok, err = tx.InsertTenantToken(r.Context(), store.TenantToken{Hash: hash, TenantID: name, Label: req.Label,
			Role: req.Role, EnvironmentID: env, MadeBy: a.Principal().String()})
		if isNoRow(err) {
			return apierr.New(http.StatusNotFound, "unknown tenant %s", name)
		}
		return err
	})
	if err != nil {
		return passOr(err, "create token")
	}
	return writeJSON(w, http.StatusCreated, mintedToken{Token: token, HashPrefix: hash[:8], CreatedAt: rfc3339(tok.CreatedAt)})
}

// revokeTenantToken is DELETE /api/v1/admin/tenants/{tenant}/tokens/{hash}.
func (s *server) revokeTenantToken(w http.ResponseWriter, r *http.Request) error {
	ok, err := s.st.DeleteTenantToken(r.Context(), r.PathValue("tenant"), r.PathValue("hash"))
	if err != nil {
		return fmt.Errorf("revoke token: %w", err)
	}
	return writeJSON(w, http.StatusOK, struct {
		Revoked bool `json:"revoked"`
	}{ok})
}

// ------------------------------------------------------------------ nodes --

// nodeInfo is a node as the admin view shows it: the old fields where they
// still exist, and the fleet's own. Node byte counts and the *Ns times are
// int64 strings; the other times are RFC 3339 ("" when unset).
type nodeInfo struct {
	Node              string `json:"node"`
	InstanceID        string `json:"instanceId"` // feosd's current start
	Status            string `json:"status"`     // joined | ready | down | removed
	StatusDescription string `json:"statusDescription"`
	Draining          bool   `json:"draining"`

	Hostname         string `json:"hostname"`
	Arch             string `json:"arch"`
	CPUModelname     string `json:"cpuModelname"`
	CPUNumcores      int    `json:"cpuNumcores"`
	MemoryTotalbytes i64    `json:"memoryTotalbytes"`
	DiskTotalbytes   i64    `json:"diskTotalbytes"`
	Slots            int    `json:"slots"`
	MemHeadroomBytes i64    `json:"memHeadroomBytes"`
	// VcpusCapacity is floor(cores × cpuOvercommit), placement's CPU gate;
	// 0: CPU is not gated.
	VcpusCapacity       int64   `json:"vcpusCapacity"`
	MemoryCapacityBytes i64     `json:"memoryCapacityBytes"`
	CPUOvercommit       float64 `json:"cpuOvercommit"`

	MemAvailBytes    i64     `json:"memAvailBytes"`
	DiskAvailBytes   i64     `json:"diskAvailBytes"`
	Load1            float64 `json:"load1"`
	SandboxesRunning int     `json:"sandboxesRunning"`
	SandboxesPaused  int     `json:"sandboxesPaused"`
	InventoryHash    string  `json:"inventoryHash"`

	RegisteredAtNs i64 `json:"registeredAtNs"`
	LastSeenAtNs   i64 `json:"lastSeenAtNs"`
	DownSinceNs    i64 `json:"downSinceNs"`

	// What is booked on the node (the bookings table only).
	SandboxesBooked   int64 `json:"sandboxesBooked"`
	VcpusBooked       int64 `json:"vcpusBooked"`
	MemoryBookedBytes i64   `json:"memoryBookedBytes"`

	StartID       string          `json:"startId"`
	Versions      json.RawMessage `json:"versions"`
	ObjectStore   bool            `json:"objectStore"`
	ObjectStoreID string          `json:"objectStoreId"`
	PodPrefix     string          `json:"podPrefix"`
	// MeshPrefix and MeshEndpoint: the node on the pods' mesh (ADR 0017),
	// its /96 of the cluster's ULA and its WireGuard device's address;
	// empty off the mesh.
	MeshPrefix   string `json:"meshPrefix"`
	MeshEndpoint string `json:"meshEndpoint"`
	// Connected: to the instance that answers; Connections: its node
	// connections there, current and draining.
	Connected      bool   `json:"connected"`
	Connections    int    `json:"connections"`
	CertSerial     string `json:"certSerial"`
	CertNotAfter   string `json:"certNotAfter"`
	StoppingUntil  string `json:"stoppingUntil"`
	DrainStartedAt string `json:"drainStartedAt"`
	IdentityForked bool   `json:"identityForked"`
	RemovedAt      string `json:"removedAt"`
}

// nodeView is what one node's info is made of.
type nodeView struct {
	node      store.Node
	telemetry store.Telemetry
	booked    store.BookingSums
	certAfter time.Time
}

func (s *server) nodeOf(v nodeView) nodeInfo {
	n, t := v.node, v.telemetry
	startID := ""
	if n.StartID != uuid.Nil {
		startID = n.StartID.String()
	}
	versions := n.Versions
	if len(versions) == 0 {
		versions = json.RawMessage("{}")
	}
	info := nodeInfo{
		Node: n.Name, InstanceID: startID, Status: n.Status, StatusDescription: n.StatusDescription,
		Draining: n.Draining,

		Hostname: n.Hostname, Arch: n.Arch, CPUModelname: n.CPUModel, CPUNumcores: n.CPUCores,
		MemoryTotalbytes: i64(n.MemoryTotalBytes), DiskTotalbytes: i64(n.DiskTotalBytes),
		Slots: n.Slots, MemHeadroomBytes: i64(n.MemHeadroomBytes),
		VcpusCapacity: n.VCPUCapacity(), MemoryCapacityBytes: i64(max(n.MemoryTotalBytes-n.MemHeadroomBytes, 0)),
		CPUOvercommit: n.CPUOvercommit,

		MemAvailBytes: i64(t.MemoryAvailableBytes), DiskAvailBytes: i64(t.DiskAvailableBytes), Load1: t.Load1,
		SandboxesRunning: t.PodsRunning, SandboxesPaused: t.PodsSuspended, InventoryHash: t.InventoryHash,

		RegisteredAtNs: unixNs(n.JoinedAt), LastSeenAtNs: unixNs(t.LastSeen), DownSinceNs: unixNs(n.DownSince),

		SandboxesBooked: v.booked.Count, VcpusBooked: v.booked.VCPUs, MemoryBookedBytes: i64(v.booked.MemoryMiB << 20),

		StartID: startID, Versions: versions, ObjectStore: n.ObjectStore, ObjectStoreID: n.ObjectStoreID,
		PodPrefix: n.PodPrefix, MeshPrefix: n.MeshPrefix, MeshEndpoint: n.MeshEndpoint,
		CertSerial: n.CurrentSerial, CertNotAfter: rfc3339(v.certAfter),
		StoppingUntil: rfc3339(n.StoppingUntil), DrainStartedAt: rfc3339(n.DrainStartedAt),
		IdentityForked: n.Forked(), RemovedAt: rfc3339(n.RemovedAt),
	}
	if c := s.deps.Conns; c != nil {
		info.Connected, info.Connections = c.Connected(n.Name), c.Connections(n.Name)
	}
	return info
}

// listNodes is GET /api/v1/admin/nodes.
func (s *server) listNodes(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	nodes, err := s.st.ListNodes(ctx)
	if err != nil {
		return fmt.Errorf("list nodes: %w", err)
	}
	tel, err := s.st.ListTelemetry(ctx)
	if err != nil {
		return fmt.Errorf("list nodes: %w", err)
	}
	booked, err := s.st.SumAllBookings(ctx, "")
	if err != nil {
		return fmt.Errorf("list nodes: %w", err)
	}
	var serials []string
	for _, n := range nodes {
		if n.CurrentSerial != "" {
			serials = append(serials, n.CurrentSerial)
		}
	}
	expiries, err := s.st.NodeCertExpiries(ctx, serials)
	if err != nil {
		return fmt.Errorf("list nodes: %w", err)
	}
	out := struct {
		Nodes    []nodeInfo   `json:"nodes"`
		Defaults nodeDefaults `json:"defaults"`
	}{Nodes: make([]nodeInfo, 0, len(nodes)), Defaults: s.nodeDefaults()}
	for _, n := range nodes {
		out.Nodes = append(out.Nodes, s.nodeOf(nodeView{node: n, telemetry: tel[n.Name], booked: booked[n.Name],
			certAfter: expiries[n.CurrentSerial]}))
	}
	return writeJSON(w, http.StatusOK, out)
}

// nodeDefaults are the instance's settings that the UI explains the fleet
// with, so that it shows this deployment's values and not a copy of the
// built-in ones: the timings of the node statuses and of a drain (in
// seconds), and the size and image of a sandbox created without one, with
// the bounds a create accepts.
type nodeDefaults struct {
	TelemetryIntervalSeconds float64 `json:"telemetryIntervalSeconds"`
	NodeTTLSeconds           float64 `json:"nodeTtlSeconds"`
	LostGraceSeconds         float64 `json:"lostGraceSeconds"`
	DrainTimeoutSeconds      float64 `json:"drainTimeoutSeconds"`
	DefaultVCPUs             int     `json:"defaultVcpus"`
	DefaultMemoryMiB         int     `json:"defaultMemoryMib"`
	DefaultImage             string  `json:"defaultImage"`
	MinVCPUs                 int     `json:"minVcpus"`
	MaxVCPUs                 int     `json:"maxVcpus"`
	MinMemoryMiB             int     `json:"minMemoryMib"`
	MaxMemoryMiB             int     `json:"maxMemoryMib"`
}

func (s *server) nodeDefaults() nodeDefaults {
	return nodeDefaults{
		TelemetryIntervalSeconds: s.cfg.TelemetryInterval.Seconds(),
		NodeTTLSeconds:           s.cfg.NodeTTL.Seconds(),
		LostGraceSeconds:         s.cfg.LostGrace.Seconds(),
		DrainTimeoutSeconds:      s.cfg.DrainTimeout.Seconds(),
		DefaultVCPUs:             s.cfg.DefaultVCPUs,
		DefaultMemoryMiB:         s.cfg.DefaultMemoryMiB,
		DefaultImage:             s.cfg.DefaultImage,
		MinVCPUs:                 config.MinVCPUs,
		MaxVCPUs:                 config.MaxVCPUs,
		MinMemoryMiB:             config.MinMemoryMiB,
		MaxMemoryMiB:             config.MaxMemoryMiB,
	}
}

// nodeAnswer reads one node's info after a change.
func (s *server) nodeAnswer(ctx context.Context, w http.ResponseWriter, n store.Node) error {
	t, err := s.st.GetTelemetry(ctx, n.Name)
	if err != nil && !isNoRow(err) {
		return err
	}
	b, err := s.st.SumBookings(ctx, n.Name, "")
	if err != nil {
		return err
	}
	var after time.Time
	if n.CurrentSerial != "" {
		ex, err := s.st.NodeCertExpiries(ctx, []string{n.CurrentSerial})
		if err != nil {
			return err
		}
		after = ex[n.CurrentSerial]
	}
	return writeJSON(w, http.StatusOK, s.nodeOf(nodeView{node: n, telemetry: t, booked: b, certAfter: after}))
}

func errUnknownNode(name string) error {
	return apierr.New(http.StatusNotFound, "unknown node %s", name)
}

// drainNode is POST /api/v1/admin/nodes/{node}/drain: placement keeps off
// the node, and the leader moves its sandboxes into the object store.
func (s *server) drainNode(w http.ResponseWriter, r *http.Request) error {
	return s.setDraining(w, r, true)
}

// undrainNode is POST /api/v1/admin/nodes/{node}/undrain.
func (s *server) undrainNode(w http.ResponseWriter, r *http.Request) error {
	return s.setDraining(w, r, false)
}

// isDrainDescription reports whether a node's status description is one
// the engine's drain loop wrote ("draining: <n> remain", "drained",
// "cannot drain: …"); the other writers' texts (a down node's, the
// fleet's) stay on an undrain.
func isDrainDescription(d string) bool {
	return strings.HasPrefix(d, "draining") || d == "drained" || strings.HasPrefix(d, "cannot drain")
}

func (s *server) setDraining(w http.ResponseWriter, r *http.Request, draining bool) error {
	name := r.PathValue("node")
	var n store.Node
	err := s.st.InTx(r.Context(), func(tx *store.Store) error {
		cur, err := tx.LockNode(r.Context(), name, store.ForUpdate)
		if isNoRow(err) {
			return errUnknownNode(name)
		}
		if err != nil {
			return err
		}
		if draining && (cur.Status == model.NodeDown || cur.Status == model.NodeRemoved) {
			return apierr.New(http.StatusConflict, "node %s is %s", name, cur.Status)
		}
		if n, err = tx.SetNodeDraining(r.Context(), name, draining); err != nil {
			return err
		}
		// The drain loop's progress text ends with the drain. The loop
		// writes it only while the node drains (in the same statement
		// that checks it), so a pass that read the node before this
		// commit does not write it back.
		if !draining && isDrainDescription(n.StatusDescription) {
			if err := tx.SetNodeDescription(r.Context(), name, ""); err != nil {
				return err
			}
			n.StatusDescription = ""
		}
		return tx.Notify(r.Context(), db.ChanLeader, "drain")
	})
	if err != nil {
		op := "undrain node"
		if draining {
			op = "drain node"
		}
		return passOr(err, op)
	}
	s.log.Info("node drain changed", "node", name, "draining", draining, "by", adminOfRequest(r))
	return s.nodeAnswer(r.Context(), w, n)
}

// patchNode is PATCH /api/v1/admin/nodes/{node} {slots?, memHeadroomBytes?,
// cpuOvercommit?}: the node's capacity settings, each ≥ 0 (cpuOvercommit
// 0: CPU not gated).
func (s *server) patchNode(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("node")
	var req struct {
		Slots *int `json:"slots"`
		// memHeadroomBytes is an int64: a JSON string as the view renders
		// it, or a number.
		MemHeadroomBytes *flexInt64 `json:"memHeadroomBytes"`
		CPUOvercommit    *float64   `json:"cpuOvercommit"`
	}
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	if (req.Slots != nil && *req.Slots < 0) || (req.MemHeadroomBytes != nil && int64(*req.MemHeadroomBytes) < 0) ||
		(req.CPUOvercommit != nil && *req.CPUOvercommit < 0) {
		return apierr.New(http.StatusBadRequest, "slots, memHeadroomBytes and cpuOvercommit must not be negative")
	}
	settings := store.NodeSettings{Slots: req.Slots, CPUOvercommit: req.CPUOvercommit}
	if req.MemHeadroomBytes != nil {
		v := int64(*req.MemHeadroomBytes)
		settings.MemHeadroomBytes = &v
	}
	n, err := s.st.UpdateNodeSettings(r.Context(), name, settings)
	if isNoRow(err) {
		return errUnknownNode(name)
	}
	if err != nil {
		return fmt.Errorf("update node: %w", err)
	}
	return s.nodeAnswer(r.Context(), w, n)
}

// removeNode is DELETE /api/v1/admin/nodes/{node}[?force=true]: the node
// is removed (never deleted), its certificates revoked, its connections
// closed on every instance; its sandboxes then go through the down sweep
// and lost grace at once. With sandboxes still on it, only by force.
func (s *server) removeNode(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("node")
	force, err := queryBool(r, "force")
	if err != nil {
		return err
	}
	var n store.Node
	err = s.st.InTx(r.Context(), func(tx *store.Store) error {
		if _, err := tx.LockNode(r.Context(), name, store.ForUpdate); isNoRow(err) {
			return errUnknownNode(name)
		} else if err != nil {
			return err
		}
		if !force {
			k, err := tx.CountSandboxesOnNode(r.Context(), name)
			if err != nil {
				return err
			}
			if k > 0 {
				return apierr.New(http.StatusConflict, "node %s has %d sandboxes; move them or pass ?force=true", name, k)
			}
		}
		if n, err = tx.RemoveNode(r.Context(), name); err != nil {
			return err
		}
		return tx.Notify(r.Context(), db.ChanLeader, "*")
	})
	if err != nil {
		return passOr(err, "remove node")
	}
	by := adminOfRequest(r)
	s.log.Info("node removed", "node", name, "force", force, "by", by)
	s.record(r.Context(), store.ActivityRow{Node: name, Kind: "node_removed", Category: "lifecycle",
		Name: "node " + name + " removed by " + by, Detail: detail(map[string]any{"by": by, "force": force})})
	return s.nodeAnswer(r.Context(), w, n)
}

// listNodeCerts is GET /api/v1/admin/nodes/{node}/certs: the node's
// certificates, newest first, its renewal history.
func (s *server) listNodeCerts(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("node")
	if _, err := s.st.GetNode(r.Context(), name); isNoRow(err) {
		return errUnknownNode(name)
	} else if err != nil {
		return fmt.Errorf("list certs: %w", err)
	}
	certs, err := s.st.ListNodeCerts(r.Context(), name)
	if err != nil {
		return fmt.Errorf("list certs: %w", err)
	}
	type certInfo struct {
		Serial         string `json:"serial"`
		Kind           string `json:"kind"`
		KeyHash        string `json:"keyHash"`
		PreviousSerial string `json:"previousSerial"`
		Peer           string `json:"peer"`
		IssuedAt       string `json:"issuedAt"`
		NotBefore      string `json:"notBefore"`
		NotAfter       string `json:"notAfter"`
		FirstSeenAt    string `json:"firstSeenAt"`
		RevokedAt      string `json:"revokedAt"`
		RevokedReason  string `json:"revokedReason"`
	}
	out := struct {
		Certs []certInfo `json:"certs"`
	}{Certs: make([]certInfo, 0, len(certs))}
	for _, c := range certs {
		out.Certs = append(out.Certs, certInfo{
			Serial: c.Serial, Kind: c.Kind, KeyHash: c.KeyHash, PreviousSerial: c.PreviousSerial, Peer: c.Peer,
			IssuedAt: rfc3339(c.IssuedAt), NotBefore: rfc3339(c.NotBefore), NotAfter: rfc3339(c.NotAfter),
			FirstSeenAt: rfc3339(c.FirstSeenAt), RevokedAt: rfc3339(c.RevokedAt), RevokedReason: c.RevokedReason,
		})
	}
	return writeJSON(w, http.StatusOK, out)
}

// ------------------------------------------------------------ join tokens --

// createJoinToken is POST /api/v1/admin/join-tokens {ttlSeconds?, kind?,
// name?, node?}: a one-time token, shown once, of kind node (the default)
// or ingress, pinned to name when given. node is name's older alias; both
// given and different is a 400. The answer names the kind, so that an
// operator sees what an instance that knows no kinds issued (a node
// token).
func (s *server) createJoinToken(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		TTLSeconds int64  `json:"ttlSeconds"`
		Kind       string `json:"kind"`
		Name       string `json:"name"`
		Node       string `json:"node"`
	}
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	if req.TTLSeconds < 0 || req.TTLSeconds > int64((100*365*24*time.Hour)/time.Second) {
		return apierr.New(http.StatusBadRequest, "ttlSeconds must be positive (0 = default %d)", int64(s.cfg.JoinTokenTTL/time.Second))
	}
	kind := req.Kind
	if kind == "" {
		kind = model.JoinKindNode
	}
	if kind != model.JoinKindNode && kind != model.JoinKindIngress {
		return apierr.New(http.StatusBadRequest, "kind %q: %s or %s", req.Kind, model.JoinKindNode, model.JoinKindIngress)
	}
	if req.Name != "" && req.Node != "" && req.Name != req.Node {
		return apierr.New(http.StatusBadRequest, "name %q and node %q differ: node is an alias of name; send one", req.Name, req.Node)
	}
	name := req.Name
	if name == "" {
		name = req.Node
	}
	var issue func(ctx context.Context, createdBy, name string, ttl time.Duration) (uuid.UUID, string, time.Time, error)
	switch {
	case kind == model.JoinKindNode && s.deps.Fleet != nil:
		issue = s.deps.Fleet.IssueJoinToken
	case kind == model.JoinKindIngress && s.deps.Ingress != nil:
		issue = s.deps.Ingress.IssueJoinToken
	default:
		return errUnavailable()
	}
	ttl := time.Duration(req.TTLSeconds) * time.Second
	if ttl == 0 {
		ttl = s.cfg.JoinTokenTTL
	}
	id, token, expires, err := issue(r.Context(), adminOfRequest(r), name, ttl)
	if err != nil {
		return passOr(err, "create join token")
	}
	return writeJSON(w, http.StatusCreated, struct {
		ID        string `json:"id"`
		Token     string `json:"token"`
		ExpiresAt string `json:"expiresAt"`
		Kind      string `json:"kind"`
		Name      string `json:"name"`
		Node      string `json:"node"`
	}{id.String(), token, rfc3339(expires), kind, name, name})
}

// listJoinTokens is GET /api/v1/admin/join-tokens, newest first, without
// secrets.
func (s *server) listJoinTokens(w http.ResponseWriter, r *http.Request) error {
	ts, err := s.st.ListJoinTokens(r.Context())
	if err != nil {
		return fmt.Errorf("list join tokens: %w", err)
	}
	type joinTokenInfo struct {
		ID        string `json:"id"`
		Kind      string `json:"kind"`
		Node      string `json:"node"`
		CreatedBy string `json:"createdBy"`
		CreatedAt string `json:"createdAt"`
		ExpiresAt string `json:"expiresAt"`
		UsedAt    string `json:"usedAt"`
		UsedName  string `json:"usedName"`
		UsedPeer  string `json:"usedPeer"`
	}
	out := struct {
		JoinTokens []joinTokenInfo `json:"joinTokens"`
	}{JoinTokens: make([]joinTokenInfo, 0, len(ts))}
	for _, t := range ts {
		out.JoinTokens = append(out.JoinTokens, joinTokenInfo{
			ID: t.ID.String(), Kind: t.Kind, Node: t.NodeName, CreatedBy: t.CreatedBy, CreatedAt: rfc3339(t.CreatedAt),
			ExpiresAt: rfc3339(t.ExpiresAt), UsedAt: rfc3339(t.UsedAt), UsedName: t.UsedName, UsedPeer: t.UsedPeer,
		})
	}
	return writeJSON(w, http.StatusOK, out)
}

// revokeJoinToken is DELETE /api/v1/admin/join-tokens/{id}.
func (s *server) revokeJoinToken(w http.ResponseWriter, r *http.Request) error {
	revoked := false
	if id, err := uuid.Parse(r.PathValue("id")); err == nil {
		if revoked, err = s.st.DeleteJoinToken(r.Context(), id); err != nil {
			return fmt.Errorf("revoke join token: %w", err)
		}
	}
	return writeJSON(w, http.StatusOK, struct {
		Revoked bool `json:"revoked"`
	}{revoked})
}

// -------------------------------------------------------------- ingresses --

// listIngresses is GET /api/v1/admin/ingresses: the ingress's instances
// (ADR 0021), removed ones included, by name. connected says that one was
// seen within 30 s, on the database clock; admittedVersion is the version
// of the security group ferun-ingress at which its address entered it;
// publicKey is its WireGuard key in base64.
func (s *server) listIngresses(w http.ResponseWriter, r *http.Request) error {
	if s.deps.Ingress == nil {
		return errUnavailable()
	}
	list, err := s.deps.Ingress.ListIngresses(r.Context())
	if err != nil {
		return passOr(err, "list ingresses")
	}
	type ingressInfo struct {
		Name            string `json:"name"`
		Status          string `json:"status"`
		JoinedAt        string `json:"joinedAt"`
		RemovedAt       string `json:"removedAt"`
		MeshPrefix      string `json:"meshPrefix"`
		MeshAddress     string `json:"meshAddress"`
		MeshEndpoint    string `json:"meshEndpoint"`
		PublicKey       string `json:"publicKey"`
		Version         string `json:"version"`
		LastSeen        string `json:"lastSeen"`
		LeftAt          string `json:"leftAt"`
		Connected       bool   `json:"connected"`
		AdmittedVersion int64  `json:"admittedVersion"`
		CertNotAfter    string `json:"certNotAfter"`
	}
	out := struct {
		Ingresses []ingressInfo `json:"ingresses"`
	}{Ingresses: make([]ingressInfo, 0, len(list))}
	for _, g := range list {
		key := ""
		if g.PublicKey != nil {
			key = base64.StdEncoding.EncodeToString(g.PublicKey)
		}
		out.Ingresses = append(out.Ingresses, ingressInfo{
			Name: g.Name, Status: g.Status, JoinedAt: rfc3339(g.JoinedAt), RemovedAt: rfc3339(g.RemovedAt),
			MeshPrefix: g.MeshPrefix, MeshAddress: g.MeshAddress, MeshEndpoint: g.MeshEndpoint, PublicKey: key,
			Version: g.Version, LastSeen: rfc3339(g.LastSeen), LeftAt: rfc3339(g.LeftAt), Connected: g.Connected,
			AdmittedVersion: g.AdmittedVersion, CertNotAfter: rfc3339(g.CertNotAfter),
		})
	}
	return writeJSON(w, http.StatusOK, out)
}

// removeIngress is DELETE /api/v1/admin/ingresses/{name}: the ingress
// instance is removed (never deleted), its certificates revoked, its
// connections closed on every instance; it leaves the pods' mesh and the
// security group ferun-ingress. A token pinned to its name joins it
// again. 204, or 404 for an unknown name.
func (s *server) removeIngress(w http.ResponseWriter, r *http.Request) error {
	if s.deps.Ingress == nil {
		return errUnavailable()
	}
	name := r.PathValue("name")
	found, err := s.deps.Ingress.RemoveIngress(r.Context(), name)
	if err != nil {
		return passOr(err, "remove ingress")
	}
	if !found {
		return apierr.New(http.StatusNotFound, "unknown ingress %s", name)
	}
	by := adminOfRequest(r)
	s.log.Info("ingress removed", "ingress", name, "by", by)
	s.record(r.Context(), store.ActivityRow{Kind: "ingress_removed", Category: "lifecycle",
		Name: "ingress " + name + " removed by " + by, Detail: detail(map[string]any{"by": by, "ingress": name})})
	return noContent(w, http.StatusNoContent)
}

// -------------------------------------------------------------- instances --

// listInstances is GET /api/v1/admin/instances: the instances and who
// leads. fresh says that the instance wrote its row within the TTL,
// stopping or not (a stopping one still serves); live that it is fresh and
// not stopping. Both are decided on the database clock.
func (s *server) listInstances(w http.ResponseWriter, r *http.Request) error {
	ins, err := s.st.ListInstances(r.Context())
	if err != nil {
		return fmt.Errorf("list instances: %w", err)
	}
	type leaderInfo struct {
		ID    string `json:"id"`
		Epoch int64  `json:"epoch"`
		Since string `json:"since"`
	}
	type instanceInfo struct {
		ID          string `json:"id"`
		Address     string `json:"address"`
		NodeAddress string `json:"nodeAddress"`
		Version     string `json:"version"`
		StartedAt   string `json:"startedAt"`
		LastSeen    string `json:"lastSeen"`
		Stopping    string `json:"stopping"`
		Fresh       bool   `json:"fresh"`
		Live        bool   `json:"live"`
	}
	out := struct {
		Leader    *leaderInfo    `json:"leader"`
		Instances []instanceInfo `json:"instances"`
	}{Instances: make([]instanceInfo, 0, len(ins))}
	if s.deps.Elector != nil {
		holder, epoch, since, err := s.deps.Elector.Leader(r.Context())
		if err != nil {
			return fmt.Errorf("read the leader: %w", err)
		}
		if holder != "" {
			out.Leader = &leaderInfo{ID: holder, Epoch: epoch, Since: rfc3339(since)}
		}
	}
	for _, in := range ins {
		out.Instances = append(out.Instances, instanceInfo{
			ID: in.ID, Address: in.Address, NodeAddress: in.NodeAddress, Version: in.Version,
			StartedAt: rfc3339(in.StartedAt), LastSeen: rfc3339(in.LastSeen), Stopping: rfc3339(in.Stopping),
			Fresh: in.Fresh, Live: in.Live,
		})
	}
	return writeJSON(w, http.StatusOK, out)
}

// flexInt64 is an int64 written as a JSON number or a JSON string.
type flexInt64 int64

func (n *flexInt64) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("%s is not an integer", b)
	}
	*n = flexInt64(v)
	return nil
}
