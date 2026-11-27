package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/store"
)

// A schedule's limits and the firing loop's pace.
//
// @decision(DL-191) @decision(DL-192) @decision(DL-193)
const (
	// MinScheduleInterval is the shortest gap a cadence may have
	// between two slots.
	MinScheduleInterval = time.Hour
	// MaxSchedulesPerUser is how many schedules one owner may have.
	MaxSchedulesPerUser = 20
	// ScheduleTick is how often the loop looks for due schedules.
	ScheduleTick = time.Minute
	// MaxSchedulesPerTick is how many due schedules one tick fires.
	MaxSchedulesPerTick = 50
	// ScheduleCatchUp is how late a slot may be before the slots it
	// stands for are counted missed and only the newest is fired.
	ScheduleCatchUp = time.Hour
	// MaxFailedFires pauses a schedule whose fires failed so many times
	// in a row.
	MaxFailedFires = 3
	// intervalSlots is how many coming slots the interval check reads.
	intervalSlots = 400
	// maxCatchUpSteps bounds the walk over slots missed while nothing
	// fired.
	maxCatchUpSteps = 10000
)

// errScheduleChanged stops a fire whose schedule was paused, deleted or
// edited after the tick read it.
var errScheduleChanged = errors.New("the schedule changed while it ran")

// ScheduleOwnerRemoved is the paused reason off-boarding gives.
//
// @decision(DL-193)
const ScheduleOwnerRemoved = "owner removed"

// cronParser reads five fields, or a descriptor.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// cronDescriptors are the descriptors a cadence may be.
var cronDescriptors = map[string]bool{"@hourly": true, "@daily": true, "@weekly": true, "@monthly": true}

// cadence is a parsed schedule and the zone it is read in.
//
// @decision(DL-191)
type cadence struct {
	sched cron.Schedule
	loc   *time.Location
}

// next is the first slot strictly after t, in UTC; the zero time when
// the cadence has none. The wall-clock time a change back from summer
// time repeats is not a second slot; a time a change forward skips has
// none that day.
//
// @decision(DL-191)
func (c cadence) next(t time.Time) time.Time {
	prev := t.In(c.loc)
	n := c.sched.Next(prev)
	for range 8 {
		if n.IsZero() || !sameWallMinute(n, prev) {
			break
		}
		n = c.sched.Next(n)
	}
	if n.IsZero() {
		return n
	}
	return n.UTC()
}

// sameWallMinute reports whether two times read the same on a clock in
// their own zone, to the minute.
func sameWallMinute(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd && a.Hour() == b.Hour() && a.Minute() == b.Minute()
}

// parseCadence reads a cron expression in five fields, or one of the
// four descriptors, in an IANA zone, "" being UTC.
//
// @decision(DL-191)
func parseCadence(expr, zone string) (cadence, error) {
	expr = strings.TrimSpace(expr)
	if zone = strings.TrimSpace(zone); zone == "" {
		zone = "UTC"
	}
	if expr == "" {
		return cadence{}, fmt.Errorf("%w: a schedule needs a cadence: five cron fields, or @hourly, @daily, @weekly or @monthly", ErrInvalidRequest)
	}
	if strings.HasPrefix(expr, "@") && !cronDescriptors[expr] {
		return cadence{}, fmt.Errorf("%w: the cadence may be @hourly, @daily, @weekly or @monthly, or five cron fields", ErrInvalidRequest)
	}
	if strings.Contains(expr, "TZ=") {
		return cadence{}, fmt.Errorf("%w: name the time zone in zone, not in the cadence", ErrInvalidRequest)
	}
	if zone == "Local" {
		return cadence{}, fmt.Errorf("%w: name an IANA time zone, such as Europe/Stockholm", ErrInvalidRequest)
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return cadence{}, fmt.Errorf("%w: %q is not an IANA time zone, such as Europe/Stockholm", ErrInvalidRequest, zone)
	}
	sched, err := cronParser.Parse(expr)
	if err != nil {
		return cadence{}, fmt.Errorf("%w: the cadence is not five cron fields (minute hour day-of-month month day-of-week): %s",
			ErrInvalidRequest, redact.Error(err))
	}
	return cadence{sched: sched, loc: loc}, nil
}

// checkCadence parses a cadence and refuses one that never fires or
// fires more often than MinScheduleInterval over its coming slots.
//
// @decision(DL-191)
func checkCadence(expr, zone string, now time.Time) (cadence, error) {
	c, err := parseCadence(expr, zone)
	if err != nil {
		return cadence{}, err
	}
	prev := c.next(now)
	if prev.IsZero() {
		return cadence{}, fmt.Errorf("%w: the cadence never fires", ErrInvalidRequest)
	}
	shortest := time.Duration(0)
	for range intervalSlots {
		n := c.next(prev)
		if n.IsZero() {
			break
		}
		if gap := n.Sub(prev); shortest == 0 || gap < shortest {
			shortest = gap
		}
		prev = n
	}
	if shortest != 0 && shortest < MinScheduleInterval {
		return cadence{}, fmt.Errorf("%w: the cadence fires %s apart at its closest; a schedule fires at most once an hour",
			ErrInvalidRequest, shortest)
	}
	return c, nil
}

// CheckScheduledJob checks a schedule's job request as a job create
// would, creating nothing: its kind may be report or pr.
//
// @decision(DL-191)
func (s *Service) CheckScheduledJob(ctx context.Context, p auth.Principal, req JobRequest) error {
	if req.Kind != store.JobKindReport && req.Kind != store.JobKindPR {
		return fmt.Errorf("%w: a schedule creates report or pr jobs; a sync job needs a checkout to start from", ErrInvalidRequest)
	}
	_, err := s.prepareJob(ctx, p, req)
	return err
}

// RunSchedules fires due schedules every ScheduleTick until ctx ends. It
// runs where the reconciler runs.
//
// @decision(DL-192)
func (s *Service) RunSchedules(ctx context.Context) {
	for {
		s.FireDueSchedules(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(ScheduleTick):
		}
	}
}

// FireDueSchedules fires the schedules whose next slot has come.
//
// @decision(DL-192)
func (s *Service) FireDueSchedules(ctx context.Context) {
	now := s.deps.Now().UTC()
	due, err := s.deps.Store.Schedules().Due(ctx, now, MaxSchedulesPerTick)
	if err != nil {
		s.deps.Warn(fmt.Sprintf("schedules: reading the due ones: %s", redact.Error(err)))
		return
	}
	for _, sc := range due {
		if ctx.Err() != nil {
			return
		}
		s.fireSchedule(ctx, sc, now, false)
	}
}

// scheduleFire is what one fire did.
//
// @decision(DL-193)
type scheduleFire struct {
	Outcome store.FireOutcome
	Reason  string
	JobID   string
}

// fireSchedule fires a schedule for its slot, or for now when manual,
// records the fire and moves the schedule on.
//
// @decision(DL-192) @decision(DL-193)
func (s *Service) fireSchedule(ctx context.Context, sc store.Schedule, now time.Time, manual bool) scheduleFire {
	slot := sc.NextRunAt.UTC()
	if manual {
		slot = now.Truncate(time.Minute)
	}
	var missed *store.ScheduleFire
	c, cerr := parseCadence(sc.Cron, sc.Zone)
	if cerr == nil && !manual && now.Sub(slot) > ScheduleCatchUp {
		latest, n, more := slot, 0, false
		for next := c.next(latest); !next.IsZero() && !next.After(now); next = c.next(next) {
			if n == maxCatchUpSteps {
				more = true
				break
			}
			latest, n = next, n+1
		}
		if more {
			// Too far behind to walk: the newest slot is found from an
			// hour before now.
			if l := c.next(now.Add(-ScheduleCatchUp)); !l.IsZero() && !l.After(now) {
				latest = l
			}
		}
		if n > 0 {
			reason := fmt.Sprintf("%d slots from %s were not run; only the newest is", n, slot.Format(time.RFC3339))
			if more {
				reason = fmt.Sprintf("at least %d slots from %s were not run; only the newest is", n, slot.Format(time.RFC3339))
			}
			missed = &store.ScheduleFire{ID: store.NewID(now), OrgID: sc.OrgID, ScheduleID: sc.ID, Slot: slot, FiredAt: now,
				Outcome: store.FireMissed, Missed: n, Reason: reason}
			slot = latest
		}
	}
	var fired scheduleFire
	if cerr != nil {
		fired = scheduleFire{Outcome: store.FireFailed, Reason: clientText(cerr)}
	} else {
		fired = s.createScheduledJob(ctx, sc, slot, manual)
	}
	rec := store.FireRecord{From: sc.NextRunAt, Outcome: fired.Outcome, PauseAfter: MaxFailedFires,
		PauseReason: fmt.Sprintf("paused after %d failed runs in a row: %s", MaxFailedFires, fired.Reason)}
	switch {
	case cerr != nil:
		rec.Pause, rec.PauseReason = true, "its cadence cannot be read: "+clientText(cerr)
	case !manual:
		after := slot
		if now.After(after) {
			after = now
		}
		if rec.Next = c.next(after); rec.Next.IsZero() {
			rec.Pause, rec.PauseReason = true, "its cadence fires no more"
		}
	}
	// After any missed entry, so the two read in order in both stores.
	entry := store.ScheduleFire{ID: store.NewID(now.Add(time.Millisecond)), OrgID: sc.OrgID, ScheduleID: sc.ID, Slot: slot,
		FiredAt: now, Outcome: fired.Outcome, Reason: fired.Reason, JobID: fired.JobID}
	err := s.deps.Store.Tx(ctx, func(tx store.Store) error {
		if missed != nil {
			if err := tx.Schedules().AddFire(ctx, *missed); err != nil {
				return err
			}
		}
		if err := tx.Schedules().AddFire(ctx, entry); err != nil {
			return err
		}
		return tx.Schedules().RecordFire(ctx, sc.OrgID, sc.ID, rec)
	})
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.deps.Warn(fmt.Sprintf("schedule %s: recording its run: %s", sc.ID, redact.Error(err)))
	}
	return fired
}

// createScheduledJob creates the job of one slot as the schedule's
// owner, or says why it did not.
//
// @decision(DL-192) @decision(DL-193)
func (s *Service) createScheduledJob(ctx context.Context, sc store.Schedule, slot time.Time, manual bool) scheduleFire {
	// A slot fired already (a second tick after a lost lock, or a record
	// that failed after the create) keeps the job it has.
	if j, ok := s.slotJob(ctx, sc, slot); ok {
		return scheduleFire{Outcome: store.FireStarted, JobID: j.ID, Reason: "already run"}
	}
	running, err := s.deps.Store.Jobs().List(ctx, sc.OrgID, store.JobFilter{ScheduleID: sc.ID, States: store.JobCounts, Limit: 1})
	if err != nil {
		return scheduleFire{Outcome: store.FireFailed, Reason: clientText(err)}
	}
	if len(running) > 0 {
		return scheduleFire{Outcome: store.FireSkipped, Reason: "the previous run is still running", JobID: running[0].ID}
	}
	p, err := s.scheduleOwner(ctx, sc)
	if err != nil {
		return scheduleFire{Outcome: store.FireFailed, Reason: clientText(err)}
	}
	var req JobRequest
	if err := json.Unmarshal(sc.Request, &req); err != nil {
		return scheduleFire{Outcome: store.FireFailed, Reason: "the stored job request cannot be read"}
	}
	// Read again, and held, inside the create's transaction: a schedule
	// paused, deleted or edited since the tick read it creates nothing,
	// and an off-boarding's pause waits for the create.
	check := func(tx store.Store) error {
		cur, err := tx.Schedules().Lock(ctx, sc.OrgID, sc.ID)
		if err != nil {
			return errScheduleChanged
		}
		if !cur.UpdatedAt.Equal(sc.UpdatedAt) || (!manual && cur.State != store.ScheduleActive) {
			return errScheduleChanged
		}
		return nil
	}
	job, err := s.createJob(ctx, p, req, &jobOrigin{scheduleID: sc.ID, slot: slot, check: check})
	switch {
	case err == nil:
		return scheduleFire{Outcome: store.FireStarted, JobID: job.ID}
	case job.ID != "":
		// Recorded but not queued: the sweep queues it.
		return scheduleFire{Outcome: store.FireStarted, JobID: job.ID, Reason: clientText(err)}
	case errors.Is(err, errScheduleChanged):
		return scheduleFire{Outcome: store.FireSkipped, Reason: errScheduleChanged.Error()}
	case errors.Is(err, ErrQuotaExceeded):
		return scheduleFire{Outcome: store.FireSkipped, Reason: "at the job limit: " + clientText(err)}
	}
	if j, ok := s.slotJob(ctx, sc, slot); ok {
		return scheduleFire{Outcome: store.FireStarted, JobID: j.ID, Reason: "already run"}
	}
	return scheduleFire{Outcome: store.FireFailed, Reason: clientText(err)}
}

// slotJob is the job a schedule created for a slot, if it has one among
// its newest.
//
// @decision(DL-192)
func (s *Service) slotJob(ctx context.Context, sc store.Schedule, slot time.Time) (store.Job, bool) {
	done, err := s.deps.Store.Jobs().List(ctx, sc.OrgID, store.JobFilter{ScheduleID: sc.ID, Limit: 20})
	if err != nil {
		return store.Job{}, false
	}
	for _, j := range done {
		if j.ScheduledFor != nil && j.ScheduledFor.Equal(slot) {
			return j, true
		}
	}
	return store.Job{}, false
}

// scheduleOwner is the principal a schedule's fire acts as: its owner,
// as authentication would take them now, never as an admin.
//
// @decision(DL-192)
func (s *Service) scheduleOwner(ctx context.Context, sc store.Schedule) (auth.Principal, error) {
	user, err := s.deps.Store.Users().Get(ctx, sc.OrgID, sc.OwnerID)
	if err != nil {
		return auth.Principal{}, fmt.Errorf("the schedule's owner cannot be read: %w", err)
	}
	if s.deps.Login == nil || s.deps.Login.Allowlist == nil {
		return auth.Principal{}, errors.New("this server has no sign-in, so a schedule has no one to run as")
	}
	p, err := auth.PrincipalOf(s.deps.Login.Allowlist, user)
	if err != nil {
		return auth.Principal{}, fmt.Errorf("the schedule's owner may no longer sign in: %w", err)
	}
	p.Admin = false
	return p, nil
}
