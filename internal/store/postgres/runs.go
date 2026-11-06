package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/store/postgres/sqlcgen"
)

// pgRuns is the Postgres RunStore.
//
// @decision(DL-085)
type pgRuns struct{ s *Store }

// Runs implements store.Store.
//
// @decision(DL-085)
func (s *Store) Runs() store.RunStore { return pgRuns{s} }

func runFromRow(r sqlcgen.SessionRun) store.Run {
	return store.Run{
		OrgID: r.OrgID, SessionID: r.SessionID, OwnerID: r.OwnerID, N: int(r.N),
		SandboxID: r.SandboxID, Booted: r.Booted, BootedImageID: r.BootedImageID, OpenCode: r.Opencode,
		Origin: store.RunOrigin(r.Origin), StartedAt: goTime(r.StartedAt),
		RunningAt: goTimePtr(r.RunningAt), EndedAt: goTimePtr(r.EndedAt),
		Snapshot: r.Snapshot, SnapshotState: store.SnapshotState(r.SnapshotState), SnapshotAt: goTimePtr(r.SnapshotAt),
	}
}

func (p pgRuns) Insert(ctx context.Context, r store.Run) error {
	n, err := p.s.q.InsertRun(ctx, sqlcgen.InsertRunParams{
		ID: r.SessionID, OrgID: r.OrgID, OwnerID: r.OwnerID, N: int32(r.N),
		SandboxID: r.SandboxID, Booted: r.Booted, BootedImageID: r.BootedImageID, Opencode: r.OpenCode,
		Origin: string(r.Origin), StartedAt: ts(r.StartedAt), RunningAt: tsPtr(r.RunningAt), EndedAt: tsPtr(r.EndedAt),
		Snapshot: r.Snapshot, SnapshotState: string(r.SnapshotState), SnapshotAt: tsPtr(r.SnapshotAt),
	})
	if err != nil {
		return mapErr("insert run", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (p pgRuns) Update(ctx context.Context, r store.Run) error {
	n, err := p.s.q.UpdateRun(ctx, sqlcgen.UpdateRunParams{
		SessionID: r.SessionID, OrgID: r.OrgID, N: int32(r.N),
		SandboxID: r.SandboxID, Booted: r.Booted, BootedImageID: r.BootedImageID, Opencode: r.OpenCode,
		Origin: string(r.Origin), RunningAt: tsPtr(r.RunningAt), EndedAt: tsPtr(r.EndedAt),
		Snapshot: r.Snapshot, SnapshotState: string(r.SnapshotState), SnapshotAt: tsPtr(r.SnapshotAt),
	})
	if err != nil {
		return mapErr("update run", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (p pgRuns) Current(ctx context.Context, orgID, sessionID string) (store.Run, error) {
	row, err := p.s.q.CurrentRun(ctx, sqlcgen.CurrentRunParams{SessionID: sessionID, OrgID: orgID})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Run{}, store.ErrNotFound
	}
	if err != nil {
		return store.Run{}, mapErr("current run", err)
	}
	return runFromRow(row), nil
}

func (p pgRuns) List(ctx context.Context, orgID string, sessionIDs []string) (map[string][]store.Run, error) {
	out := map[string][]store.Run{}
	if len(sessionIDs) == 0 {
		return out, nil
	}
	rows, err := p.s.q.ListRuns(ctx, sqlcgen.ListRunsParams{OrgID: orgID, Column2: sessionIDs})
	if err != nil {
		return nil, mapErr("list runs", err)
	}
	for _, r := range rows {
		out[r.SessionID] = append(out[r.SessionID], runFromRow(r))
	}
	return out, nil
}

func (p pgRuns) Snapshots(ctx context.Context, states ...store.SnapshotState) ([]store.Run, error) {
	names := make([]string, len(states))
	for i, s := range states {
		names[i] = string(s)
	}
	rows, err := p.s.q.SnapshotRuns(ctx, names)
	if err != nil {
		return nil, mapErr("snapshot runs", err)
	}
	out := make([]store.Run, 0, len(rows))
	for _, r := range rows {
		out = append(out, runFromRow(r))
	}
	return out, nil
}

func (p pgRuns) Reopen(ctx context.Context, orgID, sessionID string) error {
	n, err := p.s.q.ReopenCurrentRun(ctx, sqlcgen.ReopenCurrentRunParams{SessionID: sessionID, OrgID: orgID})
	if err != nil {
		return mapErr("reopen run", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}
