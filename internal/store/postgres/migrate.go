package postgres

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var embedded embed.FS

// migrationsFS is the migrations directory itself: goose scans the root
// of the FS it is given, and the embed keeps the directory prefix.
//
// @decision(DL-003)
func migrationsFS() (fs.FS, error) {
	sub, err := fs.Sub(embedded, "migrations")
	if err != nil {
		return nil, fmt.Errorf("store: embedded migrations: %w", err)
	}
	return sub, nil
}

// MigrationsFS exposes the embedded migrations so a test can check that
// the binary carries them.
func MigrationsFS() (fs.FS, error) { return migrationsFS() }

// MigrationStatus is one migration and whether it has been applied.
type MigrationStatus struct {
	Version int64
	Source  string
	Applied bool
}

// provider opens a throwaway database/sql handle, which is what goose
// needs, and builds a provider over the embedded migrations. The session
// locker makes two replicas migrating at once safe.
//
// @decision(DL-003)
func provider(url string) (*goose.Provider, *sql.DB, error) {
	cfg, err := stdlibConfig(url)
	if err != nil {
		return nil, nil, err
	}
	fsys, err := migrationsFS()
	if err != nil {
		return nil, nil, err
	}
	db := stdlib.OpenDB(*cfg)
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("store: migration locker: %w", err)
	}
	p, err := goose.NewProvider(database.DialectPostgres, db, fsys,
		goose.WithSessionLocker(locker))
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("store: migration provider: %w", err)
	}
	return p, db, nil
}

// MigrateUp applies every pending migration and reports what it applied.
//
// @decision(DL-003)
func MigrateUp(ctx context.Context, url string) ([]int64, error) {
	p, db, err := provider(url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	results, err := p.Up(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: migrate up: %w", err)
	}
	applied := make([]int64, 0, len(results))
	for _, r := range results {
		applied = append(applied, r.Source.Version)
	}
	return applied, nil
}

// MigrateDown rolls back the most recent migration.
//
// @decision(DL-003)
func MigrateDown(ctx context.Context, url string) (int64, error) {
	p, db, err := provider(url)
	if err != nil {
		return 0, err
	}
	defer func() { _ = db.Close() }()
	r, err := p.Down(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: migrate down: %w", err)
	}
	return r.Source.Version, nil
}

// MigrationsStatus lists every migration and whether it is applied.
//
// @decision(DL-003)
func MigrationsStatus(ctx context.Context, url string) ([]MigrationStatus, error) {
	p, db, err := provider(url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	stats, err := p.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: migration status: %w", err)
	}
	out := make([]MigrationStatus, 0, len(stats))
	for _, s := range stats {
		out = append(out, MigrationStatus{
			Version: s.Source.Version,
			Source:  s.Source.Path,
			Applied: !s.AppliedAt.IsZero(),
		})
	}
	return out, nil
}

// AtHead reports whether every embedded migration has been applied. It is
// what /readyz asks before reporting ready.
//
// @decision(DL-003)
func AtHead(ctx context.Context, url string) (bool, error) {
	stats, err := MigrationsStatus(ctx, url)
	if err != nil {
		return false, err
	}
	for _, s := range stats {
		if !s.Applied {
			return false, nil
		}
	}
	return len(stats) > 0, nil
}

// stdlibConfig parses url into a pgx config for the database/sql bridge.
func stdlibConfig(url string) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("store: parse database url: %w", err)
	}
	return cfg, nil
}
