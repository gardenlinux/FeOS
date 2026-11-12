// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"bufio"
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gardenlinux/feos/go/internal/ingress/wg"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
)

// The files of the state directory (see the package documentation).
const (
	keyFile     = "tls.key"
	certFile    = "cert.json"
	lostPrefix  = certFile + ".lost-"
	triedFile   = "join-tried"
	removedFile = "removed"
	wgKeyFile   = "wg.key"
	meshFile    = "mesh.json"
	tableFile   = "table-v2.json"
	tmpPrefix   = ".tmp-"
	dirMode     = 0o700
	fileMode    = 0o600
)

// oldTableFile is the table cache of the releases before visits (ADR
// 0054). It is never read, and the first save deletes it: an older binary
// started on the same volume then finds no cache and serves nothing until
// the control plane gives it a table it may serve, instead of serving
// guarded environments open from a cache it does not understand.
const oldTableFile = "table.json"

// stateDir is the state directory of an instance.
type stateDir struct {
	path string
}

// openStateDir makes the directory if it is missing, sets its mode to
// 0700 and removes the temporary files a crash left behind.
func openStateDir(path string) (*stateDir, error) {
	if err := os.MkdirAll(path, dirMode); err != nil {
		return nil, fmt.Errorf("state directory: %w", err)
	}
	if err := os.Chmod(path, dirMode); err != nil {
		return nil, fmt.Errorf("state directory: %w", err)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("state directory: %w", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tmpPrefix) {
			_ = os.Remove(filepath.Join(path, e.Name()))
		}
	}
	return &stateDir{path: path}, nil
}

func (d *stateDir) file(name string) string { return filepath.Join(d.path, name) }

// write replaces the file name with data: a temporary file with mode
// 0600, synced, renamed into place, then the directory synced.
func (d *stateDir) write(name string, data []byte) error {
	tmp, err := os.CreateTemp(d.path, tmpPrefix+name+"-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(fileMode); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", name, err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := os.Rename(tmp.Name(), d.file(name)); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return d.sync()
}

// sync syncs the directory, so that a rename in it survives a crash.
func (d *stateDir) sync() error {
	f, err := os.Open(d.path)
	if err != nil {
		return fmt.Errorf("sync the state directory: %w", err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync the state directory: %w", err)
	}
	return nil
}

// read returns the content of the file name; an error wrapping
// fs.ErrNotExist when there is none.
func (d *stateDir) read(name string) ([]byte, error) {
	return os.ReadFile(d.file(name))
}

// loadKey reads tls.key. A missing file gives (nil, nil).
func (d *stateDir) loadKey() (crypto.Signer, error) {
	data, err := d.read(keyFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("%s holds no PKCS #8 PEM key", keyFile)
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", keyFile, err)
	}
	ek, ok := k.(*ecdsa.PrivateKey)
	if !ok || ek.Curve != elliptic.P256() {
		return nil, fmt.Errorf("%s holds a %T, not an ECDSA P-256 key", keyFile, k)
	}
	return ek, nil
}

// newKey makes a new certificate key and keeps it in tls.key.
func (d *stateDir) newKey() (crypto.Signer, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("make a key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, fmt.Errorf("make a key: %w", err)
	}
	if err := d.write(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})); err != nil {
		return nil, err
	}
	return k, nil
}

// loadCert reads cert.json. A missing file gives (nil, nil).
func (d *stateDir) loadCert() (*wire.Certificate, error) {
	data, err := d.read(certFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c wire.Certificate
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", certFile, err)
	}
	return &c, nil
}

func (d *stateDir) saveCert(c wire.Certificate) error {
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return d.write(certFile, data)
}

// loseCert renames cert.json to cert.json.lost-<unix seconds of now>
// and returns the new name; "" when there was no cert.json.
func (d *stateDir) loseCert(now time.Time) (string, error) {
	name := lostPrefix + strconv.FormatInt(now.Unix(), 10)
	for i := 1; ; i++ {
		if _, err := os.Lstat(d.file(name)); errors.Is(err, fs.ErrNotExist) {
			break
		}
		name = lostPrefix + strconv.FormatInt(now.Unix(), 10) + "-" + strconv.Itoa(i)
	}
	if err := os.Rename(d.file(certFile), d.file(name)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("rename %s: %w", certFile, err)
	}
	return name, d.sync()
}

// loadTried reads join-tried: the hashes of the tokens sent. Lines that
// are not a hex SHA-256 are skipped.
func (d *stateDir) loadTried() (map[string]bool, error) {
	tried := map[string]bool{}
	data, err := d.read(triedFile)
	if errors.Is(err, fs.ErrNotExist) {
		return tried, nil
	}
	if err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.ToLower(strings.TrimSpace(sc.Text()))
		if b, err := hex.DecodeString(line); err == nil && len(b) == 32 {
			tried[line] = true
		}
	}
	return tried, nil
}

// addTried adds hash to join-tried, rewriting the file.
func (d *stateDir) addTried(hash string) error {
	tried, err := d.loadTried()
	if err != nil {
		return err
	}
	if tried[hash] {
		return nil
	}
	data, err := d.read(triedFile)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	data = append(data, hash+"\n"...)
	return d.write(triedFile, data)
}

// loadRemoved reads removed: the refusal that said the ingress was
// removed. A missing file gives ok false.
func (d *stateDir) loadRemoved() (text string, ok bool, err error) {
	data, err := d.read(removedFile)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return string(data), true, nil
}

func (d *stateDir) saveRemoved(text string) error {
	return d.write(removedFile, []byte(text))
}

// clearRemoved removes the file removed, if there is one.
func (d *stateDir) clearRemoved() error {
	if err := os.Remove(d.file(removedFile)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("remove %s: %w", removedFile, err)
	}
	return d.sync()
}

// loadWGKey reads wg.key. A missing file gives ok false.
func (d *stateDir) loadWGKey() (k wg.Key, ok bool, err error) {
	data, err := d.read(wgKeyFile)
	if errors.Is(err, fs.ErrNotExist) {
		return wg.Key{}, false, nil
	}
	if err != nil {
		return wg.Key{}, false, err
	}
	if len(data) != wg.KeyLen {
		return wg.Key{}, false, fmt.Errorf("%s holds %d bytes, not %d", wgKeyFile, len(data), wg.KeyLen)
	}
	copy(k[:], data)
	return k, true, nil
}

// newWGKey makes a new WireGuard key and keeps it in wg.key.
func (d *stateDir) newWGKey() (wg.Key, error) {
	k, err := wg.GenerateKey()
	if err != nil {
		return wg.Key{}, err
	}
	if err := d.write(wgKeyFile, k[:]); err != nil {
		return wg.Key{}, err
	}
	return k, nil
}

// meshPlace is the content of mesh.json: the instance's place on the
// pods' mesh, as the last Hello answered it.
type meshPlace struct {
	PodNetwork string `json:"podNetwork"`
	Prefix     string `json:"prefix"`
	Address    string `json:"address"`
}

// loadMesh reads mesh.json. A missing file gives (nil, nil).
func (d *stateDir) loadMesh() (*meshPlace, error) {
	data, err := d.read(meshFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m meshPlace
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", meshFile, err)
	}
	return &m, nil
}

func (d *stateDir) saveMesh(m meshPlace) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return d.write(meshFile, data)
}

// tableCache is the content of table-v2.json.
type tableCache struct {
	SavedAt time.Time   `json:"savedAt"`
	State   *wire.State `json:"state"`
}

// loadTable reads table-v2.json. A missing file gives (nil, zero, nil).
func (d *stateDir) loadTable() (*wire.State, time.Time, error) {
	data, err := d.read(tableFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, time.Time{}, nil
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	var c tableCache
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, time.Time{}, fmt.Errorf("%s: %w", tableFile, err)
	}
	if c.State == nil {
		return nil, time.Time{}, fmt.Errorf("%s holds no state", tableFile)
	}
	if c.State.Envs == nil {
		c.State.Envs = map[string]wire.Env{}
	}
	return c.State, c.SavedAt, nil
}

func (d *stateDir) saveTable(s *wire.State, savedAt time.Time) error {
	data, err := json.Marshal(tableCache{SavedAt: savedAt, State: s})
	if err != nil {
		return err
	}
	if err := d.write(tableFile, data); err != nil {
		return err
	}
	if err := os.Remove(d.file(oldTableFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", oldTableFile, err)
	}
	return nil
}
