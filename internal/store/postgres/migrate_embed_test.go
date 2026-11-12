package postgres

import (
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"testing"
)

// TestTheBinaryCarriesItsMigrations: goose scans the root of the FS it is
// given, so an embed that keeps the directory prefix silently yields "no
// migrations found" against a real database and nowhere else.
//
// @decision(DL-003)
func TestTheBinaryCarriesItsMigrations(t *testing.T) {
	fsys, err := migrationsFS()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		t.Fatalf("read the root of the migrations FS: %v", err)
	}
	var sql []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			sql = append(sql, e.Name())
		}
	}
	if len(sql) == 0 {
		t.Fatal("no .sql files at the root of the migrations FS; goose would find none")
	}
	if !strings.HasPrefix(sql[0], "00001_") {
		t.Errorf("first migration is %q, want it to start with 00001_", sql[0])
	}

	// Every migration needs both directions, or `migrate down` strands
	// the schema.
	for _, name := range sql {
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !strings.Contains(string(body), "-- +goose Up") {
			t.Errorf("%s has no +goose Up", name)
		}
		if !strings.Contains(string(body), "-- +goose Down") {
			t.Errorf("%s has no +goose Down", name)
		}
	}
}

// TestMigrationsFSIsExported keeps the accessor working, since a caller
// outside the package uses it to check the binary is complete.
//
// @decision(DL-003)
func TestMigrationsFSIsExported(t *testing.T) {
	fsys, err := MigrationsFS()
	if err != nil || fsys == nil {
		t.Fatalf("MigrationsFS() = %v, %v", fsys, err)
	}
}

// TestEveryMigrationHasItsOwnVersion: goose keys a migration by the
// number before the first underscore and records that number when it
// applies one, so two files sharing a number stop every migration, and
// a gap is a file lost in a merge. Two branches that each add the next
// number collide only here once both are merged.
//
// @decision(DL-003)
func TestEveryMigrationHasItsOwnVersion(t *testing.T) {
	fsys, err := migrationsFS()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int]string{}
	next := 1
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(prefix)
		if !ok || err != nil || len(prefix) != 5 {
			t.Errorf("%s does not start with a five-digit version", e.Name())
			continue
		}
		if other, dup := seen[v]; dup {
			t.Errorf("%s and %s share version %d: renumber the newer one", other, e.Name(), v)
			continue
		}
		seen[v] = e.Name()
		if v != next {
			t.Errorf("%s is version %d, want %s", e.Name(), v, fmt.Sprintf("%05d", next))
		}
		next = v + 1
	}
}
