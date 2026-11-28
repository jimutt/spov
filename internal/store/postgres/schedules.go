package postgres

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/store/postgres/sqlcgen"
)

// pgSchedules is the Postgres ScheduleStore.
//
// @decision(DL-191) @decision(DL-192) @decision(DL-193)
type pgSchedules struct{ s *Store }

// Schedules implements store.Store.
//
// @decision(DL-191)
func (s *Store) Schedules() store.ScheduleStore { return pgSchedules{s} }

func scheduleFromRow(r sqlcgen.Schedule) store.Schedule {
	return store.Schedule{
		ID: r.ID, OrgID: r.OrgID, OwnerID: r.OwnerID, Name: r.Name, Request: r.Request,
		Cron: r.Cron, Zone: r.Zone, State: store.ScheduleState(r.State), PausedReason: r.PausedReason,
		NextRunAt: goTime(r.NextRunAt), Failures: int(r.Failures),
		CreatedAt: goTime(r.CreatedAt), UpdatedAt: goTime(r.UpdatedAt),
	}
}

func schedulesFromRows(rows []sqlcgen.Schedule) []store.Schedule {
	out := make([]store.Schedule, 0, len(rows))
	for _, r := range rows {
		out = append(out, scheduleFromRow(r))
	}
	return out
}

// Create implements store.ScheduleStore.
func (p pgSchedules) Create(ctx context.Context, sc store.Schedule) error {
	now := p.s.now()
	if sc.CreatedAt.IsZero() {
		sc.CreatedAt = now
	}
	return mapErr("create schedule", p.s.q.CreateSchedule(ctx, sqlcgen.CreateScheduleParams{
		ID: sc.ID, OrgID: sc.OrgID, OwnerID: sc.OwnerID, Name: sc.Name, Request: sc.Request,
		Cron: sc.Cron, Zone: sc.Zone, State: string(sc.State), PausedReason: sc.PausedReason,
		NextRunAt: ts(sc.NextRunAt), Failures: int32(min(sc.Failures, math.MaxInt32)),
		CreatedAt: ts(sc.CreatedAt), UpdatedAt: ts(now),
	}))
}

// Get implements store.ScheduleStore.
func (p pgSchedules) Get(ctx context.Context, orgID, id string) (store.Schedule, error) {
	r, err := p.s.q.GetSchedule(ctx, sqlcgen.GetScheduleParams{OrgID: orgID, ID: id})
	if err != nil {
		return store.Schedule{}, mapErr("get schedule", err)
	}
	return scheduleFromRow(r), nil
}

// GetByName implements store.ScheduleStore.
func (p pgSchedules) GetByName(ctx context.Context, orgID, ownerID, name string) (store.Schedule, error) {
	r, err := p.s.q.GetScheduleByName(ctx, sqlcgen.GetScheduleByNameParams{OrgID: orgID, OwnerID: ownerID, Name: name})
	if err != nil {
		return store.Schedule{}, mapErr("get schedule", err)
	}
	return scheduleFromRow(r), nil
}

func scheduleFilterArgs(f store.ScheduleFilter) (owner, before *string, limit *int32) {
	if f.OwnerID != "" {
		owner = &f.OwnerID
	}
	if f.Before != "" {
		before = &f.Before
	}
	if f.Limit > 0 {
		n := int32(min(f.Limit, math.MaxInt32))
		limit = &n
	}
	return owner, before, limit
}

// List implements store.ScheduleStore.
func (p pgSchedules) List(ctx context.Context, orgID string, f store.ScheduleFilter) ([]store.Schedule, error) {
	owner, before, limit := scheduleFilterArgs(f)
	rows, err := p.s.q.ListSchedules(ctx, sqlcgen.ListSchedulesParams{OrgID: orgID, OwnerID: owner, Before: before, RowLimit: limit})
	if err != nil {
		return nil, mapErr("list schedules", err)
	}
	return schedulesFromRows(rows), nil
}

// Count implements store.ScheduleStore.
func (p pgSchedules) Count(ctx context.Context, orgID string, f store.ScheduleFilter) (int, error) {
	owner, _, _ := scheduleFilterArgs(f)
	n, err := p.s.q.CountSchedules(ctx, sqlcgen.CountSchedulesParams{OrgID: orgID, OwnerID: owner})
	if err != nil {
		return 0, mapErr("count schedules", err)
	}
	return int(n), nil
}

// Update implements store.ScheduleStore.
//
// @decision(DL-191)
func (p pgSchedules) Update(ctx context.Context, sc store.Schedule) error {
	n, err := p.s.q.UpdateSchedule(ctx, sqlcgen.UpdateScheduleParams{
		OrgID: sc.OrgID, ID: sc.ID, Name: sc.Name, Request: sc.Request, Cron: sc.Cron, Zone: sc.Zone,
		State: string(sc.State), PausedReason: sc.PausedReason, NextRunAt: ts(sc.NextRunAt),
		Failures: int32(min(sc.Failures, math.MaxInt32)), UpdatedAt: ts(p.s.now()),
	})
	if err != nil {
		return mapErr("update schedule", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// Delete implements store.ScheduleStore; its history goes with it.
func (p pgSchedules) Delete(ctx context.Context, orgID, id string) error {
	n, err := p.s.q.DeleteSchedule(ctx, sqlcgen.DeleteScheduleParams{OrgID: orgID, ID: id})
	if err != nil {
		return mapErr("delete schedule", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// Due implements store.ScheduleStore.
//
// @decision(DL-192)
func (p pgSchedules) Due(ctx context.Context, now time.Time, limit int) ([]store.Schedule, error) {
	rows, err := p.s.q.DueSchedules(ctx, sqlcgen.DueSchedulesParams{NextRunAt: ts(now), Limit: int32(min(limit, math.MaxInt32))})
	if err != nil {
		return nil, mapErr("due schedules", err)
	}
	return schedulesFromRows(rows), nil
}

// AddFire implements store.ScheduleStore, trimming the history in the
// same transaction.
//
// @decision(DL-193)
func (p pgSchedules) AddFire(ctx context.Context, f store.ScheduleFire) error {
	write := func(q *sqlcgen.Queries) error {
		n, err := q.AddScheduleFire(ctx, sqlcgen.AddScheduleFireParams{
			ID: f.ID, OrgID: f.OrgID, ScheduleID: f.ScheduleID, Slot: ts(f.Slot), FiredAt: ts(f.FiredAt),
			Outcome: string(f.Outcome), Reason: f.Reason, JobID: f.JobID, Missed: int32(min(f.Missed, math.MaxInt32)),
		})
		if err != nil {
			return mapErr("add schedule fire", err)
		}
		if n == 0 {
			return store.ErrNotFound
		}
		return mapErr("trim schedule fires", q.TrimScheduleFires(ctx, sqlcgen.TrimScheduleFiresParams{
			ScheduleID: f.ScheduleID, Keep: store.MaxScheduleHistory,
		}))
	}
	if p.s.inTx {
		return write(p.s.q)
	}
	return p.s.Tx(ctx, func(tx store.Store) error { return write(tx.(*Store).q) })
}

// Fires implements store.ScheduleStore.
func (p pgSchedules) Fires(ctx context.Context, orgID, scheduleID string, limit int) ([]store.ScheduleFire, error) {
	if _, err := p.Get(ctx, orgID, scheduleID); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = store.MaxScheduleHistory
	}
	rows, err := p.s.q.ListScheduleFires(ctx, sqlcgen.ListScheduleFiresParams{OrgID: orgID, ScheduleID: scheduleID, Limit: int32(min(limit, math.MaxInt32))})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, mapErr("list schedule fires", err)
	}
	out := make([]store.ScheduleFire, 0, len(rows))
	for _, r := range rows {
		out = append(out, store.ScheduleFire{
			ID: r.ID, OrgID: r.OrgID, ScheduleID: r.ScheduleID, Slot: goTime(r.Slot), FiredAt: goTime(r.FiredAt),
			Outcome: store.FireOutcome(r.Outcome), Reason: r.Reason, JobID: r.JobID, Missed: int(r.Missed),
		})
	}
	return out, nil
}

// PauseOwned implements store.ScheduleStore.
//
// @decision(DL-193)
func (p pgSchedules) PauseOwned(ctx context.Context, orgID, ownerID, reason string) (int, error) {
	n, err := p.s.q.PauseOwnedSchedules(ctx, sqlcgen.PauseOwnedSchedulesParams{
		OrgID: orgID, OwnerID: ownerID, PausedReason: reason, UpdatedAt: ts(p.s.now()),
	})
	if err != nil {
		return 0, mapErr("pause schedules", err)
	}
	return int(n), nil
}

// Lock implements store.ScheduleStore: the row is held until the
// transaction it is read in ends.
//
// @decision(DL-192)
func (p pgSchedules) Lock(ctx context.Context, orgID, id string) (store.Schedule, error) {
	r, err := p.s.q.LockSchedule(ctx, sqlcgen.LockScheduleParams{OrgID: orgID, ID: id})
	if err != nil {
		return store.Schedule{}, mapErr("lock schedule", err)
	}
	return scheduleFromRow(r), nil
}

// RecordFire implements store.ScheduleStore in one statement.
//
// @decision(DL-192) @decision(DL-193)
func (p pgSchedules) RecordFire(ctx context.Context, orgID, id string, r store.FireRecord) error {
	var next pgtype.Timestamptz
	if !r.Next.IsZero() {
		next = ts(r.Next)
	}
	return mapErr("record schedule fire", p.s.q.RecordScheduleFire(ctx, sqlcgen.RecordScheduleFireParams{
		Outcome: string(r.Outcome), Pause: r.Pause, PauseAfter: int32(min(r.PauseAfter, math.MaxInt32)),
		PauseReason: r.PauseReason, Next: next, Now: ts(p.s.now()), OrgID: orgID, ID: id, FromSlot: ts(r.From),
	}))
}
