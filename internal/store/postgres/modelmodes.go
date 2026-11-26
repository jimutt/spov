package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/store/postgres/sqlcgen"
)

// pgModelModes is the Postgres ModelModeStore.
//
// @decision(DL-186)
type pgModelModes struct{ s *Store }

// ModelModes implements store.Store.
//
// @decision(DL-186)
func (s *Store) ModelModes() store.ModelModeStore { return pgModelModes{s} }

// Keep implements store.ModelModeStore, in one statement.
//
// @decision(DL-186)
func (p pgModelModes) Keep(ctx context.Context, orgID, openCode string, models map[string][]string) error {
	if models == nil {
		models = map[string][]string{}
	}
	raw, err := json.Marshal(models)
	if err != nil {
		return fmt.Errorf("store: encode model modes: %w", err)
	}
	err = p.s.q.KeepModelModes(ctx, sqlcgen.KeepModelModesParams{
		OrgID: orgID, Opencode: openCode, Models: raw, UpdatedAt: ts(p.s.now()), MaxBytes: store.MaxSeenModelModesBytes,
	})
	return mapErr("keep model modes", err)
}

// Seen implements store.ModelModeStore.
//
// @decision(DL-186)
func (p pgModelModes) Seen(ctx context.Context, orgID, openCode string) (map[string][]string, error) {
	raw, err := p.s.q.GetModelModes(ctx, sqlcgen.GetModelModesParams{OrgID: orgID, Opencode: openCode})
	if errors.Is(err, pgx.ErrNoRows) {
		return map[string][]string{}, nil
	}
	if err != nil {
		return nil, mapErr("get model modes", err)
	}
	out := map[string][]string{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("store: decode model modes: %w", err)
	}
	return out, nil
}
