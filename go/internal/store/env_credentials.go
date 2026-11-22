// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// EnvCredential is a version of an environment's credential (ADR 0033):
// an HTTP binding's definition, its credential sealed at rest.
type EnvCredential struct {
	EnvID         uuid.UUID
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
// LockEnvCredential takes ("FECR").
const credentialLock int32 = 0x46454352

var credentialColumns = []string{"env_id", "name", "version", "upstream", "mode", "ca_pem", "path_prefixes",
	"forwarded_host", "rate_per_second", "rate_burst", "credential_kind", "sealed", "created_by", "created_at"}

var credentialCols = prefixed("", credentialColumns)

type rowScanner interface {
	Scan(dest ...any) error
}

func scanCredential(r rowScanner) (EnvCredential, error) {
	var c EnvCredential
	var prefixes []string
	err := r.Scan(&c.EnvID, &c.Name, &c.Version, &c.Upstream, &c.Mode, &c.CAPEM, &prefixes, &c.ForwardedHost,
		&c.RatePerSecond, &c.RateBurst, &c.Kind, &c.Sealed, &c.CreatedBy, &c.CreatedAt)
	c.PathPrefixes = nonNil(prefixes)
	return c, err
}

// LockEnvCredential serialises the writes of one credential of an
// environment until the caller's transaction ends: a new version's number
// and a delete's check of its uses.
func (s *Store) LockEnvCredential(ctx context.Context, env uuid.UUID, name string) error {
	_, err := s.q.Exec(ctx, "SELECT pg_advisory_xact_lock($1, hashtext($2::text))", credentialLock,
		env.String()+"/"+name)
	return err
}

// EnvCredentials are the latest versions of an environment's credentials,
// by name.
func (s *Store) EnvCredentials(ctx context.Context, env uuid.UUID) ([]EnvCredential, error) {
	rows, err := s.q.Query(ctx, `SELECT DISTINCT ON (name) `+credentialCols+`
		FROM env_credentials WHERE env_id = $1 ORDER BY name, version DESC`, env)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EnvCredential{}
	for rows.Next() {
		c, err := scanCredential(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// EnvCredential is a version of an environment's credential, 0 its
// latest; model.ErrNotFound when there is none.
func (s *Store) EnvCredential(ctx context.Context, env uuid.UUID, name string, version int) (EnvCredential, error) {
	row := s.q.QueryRow(ctx, `SELECT `+credentialCols+` FROM env_credentials
		WHERE env_id = $1 AND name = $2 AND ($3 = 0 OR version = $3) ORDER BY version DESC LIMIT 1`,
		env, name, version)
	c, err := scanCredential(row)
	if err != nil {
		return EnvCredential{}, noRow(err, model.ErrNotFound)
	}
	return c, nil
}

// CredentialVersion is what a version of a credential is to an
// environment's spec: its number, what makes a pod another, and whether
// the node forwards the host (what a spec may expect, ADR 0058).
type CredentialVersion struct {
	Version       int
	Upstream      string
	Mode          string
	ForwardedHost bool
}

// EnvCredentialVersions are the versions of each of an environment's
// credentials, oldest first.
func (s *Store) EnvCredentialVersions(ctx context.Context, env uuid.UUID) (map[string][]CredentialVersion, error) {
	rows, err := s.q.Query(ctx, `SELECT name, version, upstream, mode, forwarded_host FROM env_credentials
		WHERE env_id = $1 ORDER BY name, version`, env)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]CredentialVersion{}
	for rows.Next() {
		var name string
		var v CredentialVersion
		if err := rows.Scan(&name, &v.Version, &v.Upstream, &v.Mode, &v.ForwardedHost); err != nil {
			return nil, err
		}
		out[name] = append(out[name], v)
	}
	return out, rows.Err()
}

// NextEnvCredentialVersion is the version a new version of the
// environment's credential name gets: one more than any it ever had,
// those of a deleted credential of the name too, so that a number is
// never used for two definitions. Run it under LockEnvCredential.
func (s *Store) NextEnvCredentialVersion(ctx context.Context, env uuid.UUID, name string) (int, error) {
	var v int
	err := s.q.QueryRow(ctx, `SELECT COALESCE((SELECT last_version FROM env_credential_names
		WHERE env_id = $1 AND name = $2), 0) + 1`, env, name).Scan(&v)
	return v, err
}

// InsertEnvCredential writes c, a new version of its credential, and
// moves the name's mark to it (NextEnvCredentialVersion). A version that
// is not above every version the name had is refused. Run it under
// LockEnvCredential (or in the transaction that makes the environment).
func (s *Store) InsertEnvCredential(ctx context.Context, c EnvCredential) error {
	tag, err := s.q.Exec(ctx, `INSERT INTO env_credential_names (env_id, name, last_version) VALUES ($1, $2, $3)
		ON CONFLICT (env_id, name) DO UPDATE SET last_version = EXCLUDED.last_version
		WHERE env_credential_names.last_version < EXCLUDED.last_version`, c.EnvID, c.Name, c.Version)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("credential %s: version %d is not above every version the name had", c.Name, c.Version)
	}
	_, err = s.q.Exec(ctx, `INSERT INTO env_credentials (env_id, name, version, upstream, mode, ca_pem,
			path_prefixes, forwarded_host, rate_per_second, rate_burst, credential_kind, sealed, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		c.EnvID, c.Name, c.Version, c.Upstream, c.Mode, c.CAPEM, nonNil(c.PathPrefixes), c.ForwardedHost,
		c.RatePerSecond, c.RateBurst, c.Kind, c.Sealed, c.CreatedBy)
	return err
}

// DeleteEnvCredential removes every version of a credential, but not
// its name's mark (NextEnvCredentialVersion); false when it had none.
func (s *Store) DeleteEnvCredential(ctx context.Context, env uuid.UUID, name string) (bool, error) {
	tag, err := s.q.Exec(ctx, `DELETE FROM env_credentials WHERE env_id = $1 AND name = $2`, env, name)
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

// SandboxEnvCredentials are the latest versions of the credentials a
// sandbox names, of its environment, by name; a name its environment has
// no credential of is left out.
func (s *Store) SandboxEnvCredentials(ctx context.Context, id podid.ID) ([]EnvCredential, error) {
	rows, err := s.q.Query(ctx, `SELECT DISTINCT ON (c.name) `+prefixed("c", credentialColumns)+`
		FROM sandbox_credentials sc JOIN sandboxes s ON s.id = sc.sandbox_id
		JOIN env_credentials c ON c.env_id = s.environment_id AND c.name = sc.name
		WHERE sc.sandbox_id = $1 ORDER BY c.name, c.version DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EnvCredential{}
	for rows.Next() {
		c, err := scanCredential(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CredentialSandboxes are the sandboxes of an environment that name each
// of its credentials, by name.
func (s *Store) CredentialSandboxes(ctx context.Context, env uuid.UUID) (map[string][]podid.ID, error) {
	rows, err := s.q.Query(ctx, `SELECT sc.name, sc.sandbox_id FROM sandbox_credentials sc
		JOIN sandboxes s ON s.id = sc.sandbox_id WHERE s.environment_id = $1 ORDER BY sc.name, sc.sandbox_id`, env)
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

// EnvCredentialExists reports whether the environment has a version of
// the credential, its HTTP secret (ADR 0055).
func (s *Store) EnvCredentialExists(ctx context.Context, env uuid.UUID, name string) (bool, error) {
	var ok bool
	err := s.q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM env_credentials WHERE env_id = $1 AND name = $2)`,
		env, name).Scan(&ok)
	return ok, err
}
