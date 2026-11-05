package postgres

import "context"

// MigrateUpTo applies migrations up to and including version, so a test
// can write rows in an older schema and migrate them.
func MigrateUpTo(ctx context.Context, url string, version int64) error {
	p, db, err := provider(url)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	_, err = p.UpTo(ctx, version)
	return err
}
