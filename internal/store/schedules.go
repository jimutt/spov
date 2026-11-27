package store

import (
	"context"
	"time"
)

// ScheduleState is whether a schedule fires.
//
// @decision(DL-191)
type ScheduleState string

const (
	ScheduleActive ScheduleState = "active"
	SchedulePaused ScheduleState = "paused"
)

// Schedule is a stored job request and the cadence it is created on, a
// hosted server's alone.
//
// @decision(DL-191)
type Schedule struct {
	ID      string
	OrgID   string
	OwnerID string
	// Name is unique per owner.
	Name string
	// Request is the job's create body, as JSON, stored as sent.
	Request []byte
	// Cron is the cadence as written, and Zone the IANA zone it is read in.
	Cron  string
	Zone  string
	State ScheduleState
	// PausedReason says why a paused schedule does not fire.
	PausedReason string
	// NextRunAt is the next slot to fire.
	NextRunAt time.Time
	// Failures counts failed fires since the last one that started a job.
	//
	// @decision(DL-193)
	Failures  int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// FireOutcome is what one fire of a schedule did.
//
// @decision(DL-193)
type FireOutcome string

const (
	FireStarted FireOutcome = "started"
	FireSkipped FireOutcome = "skipped"
	FireFailed  FireOutcome = "failed"
	FireMissed  FireOutcome = "missed"
)

// ScheduleFire is one entry of a schedule's history.
//
// @decision(DL-193)
type ScheduleFire struct {
	ID         string
	OrgID      string
	ScheduleID string
	// Slot is the cadence's time the fire was for; FiredAt when it ran.
	Slot    time.Time
	FiredAt time.Time
	Outcome FireOutcome
	Reason  string
	// JobID is the job a started fire created, or the one a skip waited on.
	JobID string
	// Missed is how many slots a missed entry stands for.
	Missed int
}

// MaxScheduleHistory is how many fires one schedule keeps.
//
// @decision(DL-193)
const MaxScheduleHistory = 50

// FireRecord is how one fire changes its schedule: only these fields,
// and only while the row still holds the slot the fire read.
//
// @decision(DL-192) @decision(DL-193)
type FireRecord struct {
	// From is the row's next slot when the fire read it; a row moved on
	// since (an edit, a resume, another fire) is left as it is.
	From time.Time
	// Next is the slot to move to; zero keeps the row's.
	Next    time.Time
	Outcome FireOutcome
	// PauseAfter pauses an active row whose failures reach it with this
	// failed fire; Pause pauses it now. Either uses PauseReason.
	PauseAfter  int
	Pause       bool
	PauseReason string
}

// ScheduleFilter narrows a listing of schedules.
type ScheduleFilter struct {
	// OwnerID keeps one owner's; empty keeps the org's.
	OwnerID string
	// Before keeps ids below it; Limit, when positive, at most so many,
	// newest first. Count ignores both.
	Before string
	Limit  int
}

// ScheduleStore holds schedules and their history. Every query but Due
// is scoped to an org.
//
// @decision(DL-191) @decision(DL-192) @decision(DL-193)
type ScheduleStore interface {
	// Create adds a schedule; a name its owner already uses is
	// ErrConflict.
	Create(ctx context.Context, s Schedule) error
	Get(ctx context.Context, orgID, id string) (Schedule, error)
	GetByName(ctx context.Context, orgID, ownerID, name string) (Schedule, error)
	List(ctx context.Context, orgID string, f ScheduleFilter) ([]Schedule, error)
	Count(ctx context.Context, orgID string, f ScheduleFilter) (int, error)
	// Update writes every field but OrgID, OwnerID and CreatedAt.
	Update(ctx context.Context, s Schedule) error
	// Lock reads a schedule and, inside a transaction on Postgres, holds
	// its row until the transaction ends.
	Lock(ctx context.Context, orgID, id string) (Schedule, error)
	// RecordFire applies a fire's outcome to its schedule (FireRecord);
	// a row no longer at r.From is left unchanged.
	RecordFire(ctx context.Context, orgID, id string, r FireRecord) error
	// Delete removes a schedule and its history.
	Delete(ctx context.Context, orgID, id string) error
	// Due is up to limit active schedules, of any org, whose next slot is
	// at or before now, the oldest slot first.
	Due(ctx context.Context, now time.Time, limit int) ([]Schedule, error)
	// AddFire records a fire, keeping the schedule's newest
	// MaxScheduleHistory.
	AddFire(ctx context.Context, f ScheduleFire) error
	// Fires is a schedule's history, newest first.
	Fires(ctx context.Context, orgID, scheduleID string, limit int) ([]ScheduleFire, error)
	// PauseOwned pauses an owner's active schedules with a reason and
	// says how many.
	PauseOwned(ctx context.Context, orgID, ownerID, reason string) (int, error)
}
