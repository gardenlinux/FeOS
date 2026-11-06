// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"time"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// TenantCredential is a version of a tenant's credential (ADR 0033): an
// HTTP binding's definition, its credential sealed at rest.
type TenantCredential struct {
	Tenant        string
	Name          string
	Version       int
	Upstream      string // https://host[:port], canonical
	Mode          string // name, intercept
	CAPEM         string
	PathPrefixes  []string
	ForwardedHost bool
	RatePerSecond int
	RateBurst     int
	Kind          string // headers, basic, oauth2, clientCertificate
	Sealed        []byte // 0x01 || nonce(12) || AES-256-GCM(credential JSON)
	CreatedBy     string
	CreatedAt     time.Time
}

// credentialLock is the first key of the advisory locks that
// LockTenantCredential takes ("FECR").
const credentialLock int32 = 0x46454352

var credentialColumns = []string{"tenant", "name", "version", "upstream", "mode", "ca_pem", "path_prefixes",
	"forwarded_host", "rate_per_second", "rate_burst", "credential_kind", "sealed", "created_by", "created_at"}

var credentialCols = prefixed("", credentialColumns)

type rowScanner interface {
	Scan(dest ...any) error
}

func scanCredential(r rowScanner) (TenantCredential, error) {
	var c TenantCredential
	var prefixes []string
	err := r.Scan(&c.Tenant, &c.Name, &c.Version, &c.Upstream, &c.Mode, &c.CAPEM, &prefixes, &c.ForwardedHost,
		&c.RatePerSecond, &c.RateBurst, &c.Kind, &c.Sealed, &c.CreatedBy, &c.CreatedAt)
	c.PathPrefixes = nonNil(prefixes)
	return c, err
}

// LockTenantCredential serialises the writes of one credential of a
// tenant until the caller's transaction ends: a new version's number and
// a delete's check of its uses.
func (s *Store) LockTenantCredential(ctx context.Context, tenant, name string) error {
	_, err := s.q.Exec(ctx, "SELECT pg_advisory_xact_lock($1, hashtext($2::text))", credentialLock, tenant+"/"+name)
	return err
}

// TenantCredentials are the latest versions of a tenant's credentials, by
// name.
func (s *Store) TenantCredentials(ctx context.Context, tenant string) ([]TenantCredential, error) {
	rows, err := s.q.Query(ctx, `SELECT DISTINCT ON (name) `+credentialCols+`
		FROM tenant_credentials WHERE tenant = $1 ORDER BY name, version DESC`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TenantCredential{}
	for rows.Next() {
		c, err := scanCredential(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// TenantCredential is a version of a tenant's credential, 0 its latest;
// model.ErrNotFound when there is none.
func (s *Store) TenantCredential(ctx context.Context, tenant, name string, version int) (TenantCredential, error) {
	row := s.q.QueryRow(ctx, `SELECT `+credentialCols+` FROM tenant_credentials
		WHERE tenant = $1 AND name = $2 AND ($3 = 0 OR version = $3) ORDER BY version DESC LIMIT 1`,
		tenant, name, version)
	c, err := scanCredential(row)
	if err != nil {
		return TenantCredential{}, noRow(err, model.ErrNotFound)
	}
	return c, nil
}

// CredentialVersion is what a version of a credential is to an
// environment's spec: its number and what makes a pod another.
type CredentialVersion struct {
	Version  int
	Upstream string
	Mode     string
}

// TenantCredentialVersions are the versions of each of a tenant's
// credentials, oldest first.
func (s *Store) TenantCredentialVersions(ctx context.Context, tenant string) (map[string][]CredentialVersion, error) {
	rows, err := s.q.Query(ctx, `SELECT name, version, upstream, mode FROM tenant_credentials
		WHERE tenant = $1 ORDER BY name, version`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]CredentialVersion{}
	for rows.Next() {
		var name string
		var v CredentialVersion
		if err := rows.Scan(&name, &v.Version, &v.Upstream, &v.Mode); err != nil {
			return nil, err
		}
		out[name] = append(out[name], v)
	}
	return out, rows.Err()
}

// InsertTenantCredential writes c, a new version of its credential.
// Run it under LockTenantCredential, with the version one more than the
// latest.
func (s *Store) InsertTenantCredential(ctx context.Context, c TenantCredential) error {
	_, err := s.q.Exec(ctx, `INSERT INTO tenant_credentials (tenant, name, version, upstream, mode, ca_pem,
			path_prefixes, forwarded_host, rate_per_second, rate_burst, credential_kind, sealed, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		c.Tenant, c.Name, c.Version, c.Upstream, c.Mode, c.CAPEM, nonNil(c.PathPrefixes), c.ForwardedHost,
		c.RatePerSecond, c.RateBurst, c.Kind, c.Sealed, c.CreatedBy)
	return err
}

// DeleteTenantCredential removes every version of a credential; false
// when it had none.
func (s *Store) DeleteTenantCredential(ctx context.Context, tenant, name string) (bool, error) {
	tag, err := s.q.Exec(ctx, `DELETE FROM tenant_credentials WHERE tenant = $1 AND name = $2`, tenant, name)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// SandboxCredentialNames are the credentials a sandbox names, sorted;
// model.ErrNotFound when the sandbox does not exist.
func (s *Store) SandboxCredentialNames(ctx context.Context, id podid.ID) ([]string, error) {
	rows, err := s.q.Query(ctx, `SELECT c.name FROM sandboxes s
		LEFT JOIN sandbox_credentials c ON c.sandbox_id = s.id WHERE s.id = $1 ORDER BY c.name`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	found := false
	out := []string{}
	for rows.Next() {
		found = true
		var name *string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if name != nil {
			out = append(out, *name)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !found {
		return nil, model.ErrNotFound
	}
	return out, nil
}

// SetSandboxCredentialNames replaces the credentials a sandbox names.
func (s *Store) SetSandboxCredentialNames(ctx context.Context, id podid.ID, names []string) error {
	if _, err := s.q.Exec(ctx, `DELETE FROM sandbox_credentials WHERE sandbox_id = $1`, id); err != nil {
		return err
	}
	if len(names) == 0 {
		return nil
	}
	_, err := s.q.Exec(ctx, `INSERT INTO sandbox_credentials (sandbox_id, name) SELECT $1, unnest($2::text[])`,
		id, names)
	return err
}

// SandboxTenantCredentials are the latest versions of the credentials a
// sandbox names, by name; a name its tenant has no credential of is left
// out.
func (s *Store) SandboxTenantCredentials(ctx context.Context, id podid.ID) ([]TenantCredential, error) {
	rows, err := s.q.Query(ctx, `SELECT DISTINCT ON (t.name) `+prefixed("t", credentialColumns)+`
		FROM sandbox_credentials sc JOIN sandboxes s ON s.id = sc.sandbox_id
		JOIN tenant_credentials t ON t.tenant = s.owner AND t.name = sc.name
		WHERE sc.sandbox_id = $1 ORDER BY t.name, t.version DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TenantCredential{}
	for rows.Next() {
		c, err := scanCredential(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CredentialSandboxes are the sandboxes of a tenant that name each of its
// credentials, by name.
func (s *Store) CredentialSandboxes(ctx context.Context, tenant string) (map[string][]podid.ID, error) {
	rows, err := s.q.Query(ctx, `SELECT sc.name, sc.sandbox_id FROM sandbox_credentials sc
		JOIN sandboxes s ON s.id = sc.sandbox_id WHERE s.owner = $1 ORDER BY sc.name, sc.sandbox_id`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]podid.ID{}
	for rows.Next() {
		var name string
		var id podid.ID
		if err := rows.Scan(&name, &id); err != nil {
			return nil, err
		}
		out[name] = append(out[name], id)
	}
	return out, rows.Err()
}
