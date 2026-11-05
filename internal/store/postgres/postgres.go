// Package postgres implements store.Store on PostgreSQL with pgx and
// sqlc-generated queries. It is what hosted mode runs on; local mode
// keeps the memory store and never opens a database.
//
// @decision(DL-003)
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/store/postgres/sqlcgen"
)

// Store is the Postgres implementation of store.Store.
//
// @decision(DL-003)
type Store struct {
	pool *pgxpool.Pool
	q    *sqlcgen.Queries
	// db is what q runs on: the pool, or a transaction inside Tx. The
	// one hand-written query needs it directly.
	db sqlcgen.DBTX
	// Now supplies timestamps; nil means time.Now.
	Now func() time.Time
	// inTx marks a store bound to a transaction, so Tx refuses to nest.
	inTx bool
}

// Open connects to url and verifies the connection.
//
// @decision(DL-003)
func Open(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("store: parse database url: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return NewWithPool(pool), nil
}

// NewWithPool wraps an existing pool, which the caller keeps ownership of.
func NewWithPool(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, q: sqlcgen.New(pool), db: pool}
}

// Pool exposes the underlying pool for health checks and the advisory
// lock.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

// Ping implements store.Store.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("store: ping: %w", err)
	}
	return nil
}

// Sessions implements store.Store.
func (s *Store) Sessions() store.SessionStore { return pgSessions{s} }

// Mints implements store.Store.
func (s *Store) Mints() store.MintStore { return pgMints{s} }

// Users implements store.Store.
func (s *Store) Users() store.UserStore { return pgUsers{s} }

// Tokens implements store.Store.
func (s *Store) Tokens() store.TokenStore { return pgTokens{s} }

// DeviceCodes implements store.Store.
func (s *Store) DeviceCodes() store.DeviceCodeStore { return pgDeviceCodes{s} }

// Credentials implements store.Store.
func (s *Store) Credentials() store.CredentialStore { return pgCredentials{s} }

// Allowlist implements store.Store.
//
// @decision(DL-007)
func (s *Store) Allowlist() store.AllowlistStore { return pgAllowlist{s} }

// Conversations implements store.Store.
//
// @decision(DL-017)
func (s *Store) Conversations() store.ConversationStore { return pgConversations{s} }

// Jobs implements store.Store.
//
// @decision(DL-045)
func (s *Store) Jobs() store.JobStore { return pgJobs{s} }

// Tx implements store.Store. The callback sees a store bound to the
// transaction; returning an error or panicking rolls it back.
//
// @decision(DL-003)
func (s *Store) Tx(ctx context.Context, fn func(store.Store) error) error {
	if s.inTx {
		return store.ErrNestedTx
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			// The rollback runs even when the callback panicked, and
			// must not be cancelled by the caller's context.
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	view := &Store{pool: s.pool, q: s.q.WithTx(tx), db: tx, Now: s.Now, inTx: true}
	if err := fn(view); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	committed = true
	return nil
}
