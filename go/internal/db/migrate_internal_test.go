// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"strings"
	"testing"
	"testing/fstest"
)

// Two migrations of one number are refused: the second would be skipped
// on every database that has the first.
func TestReadMigrationsRefusesOneNumberTwice(t *testing.T) {
	fsys := fstest.MapFS{
		"0001_a.sql": {Data: []byte("select 1;")},
		"0002_b.sql": {Data: []byte("select 2;")},
		"0002_c.sql": {Data: []byte("select 3;")},
	}
	_, err := readMigrations(fsys)
	if err == nil || !strings.Contains(err.Error(), "0002_b.sql and 0002_c.sql") {
		t.Fatalf("readMigrations = %v, want a refusal naming both files", err)
	}
	delete(fsys, "0002_c.sql")
	ms, err := readMigrations(fsys)
	if err != nil || len(ms) != 2 || ms[1].version != 2 {
		t.Fatalf("readMigrations = %v, %v", ms, err)
	}
}

// The embedded migrations have a number each.
func TestEmbeddedMigrationsDistinct(t *testing.T) {
	if _, err := migrations(); err != nil {
		t.Fatal(err)
	}
}
