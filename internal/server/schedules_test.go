package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/store"
)

// A cadence is five cron fields or one of four descriptors, in an IANA
// zone, firing at most once an hour.
//
// @decision(DL-191)
func TestACadenceIsFiveFieldsOrADescriptorAtMostHourly(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	for expr, zone := range map[string]string{
		"@hourly": "", "@daily": "UTC", "@weekly": "Europe/Stockholm", "@monthly": "America/New_York",
		"0 9 * * 1-5": "Europe/Stockholm", "30 */2 * * *": "", "0 0 1 */3 *": "Asia/Tokyo",
	} {
		if _, err := checkCadence(expr, zone, now); err != nil {
			t.Errorf("%q in %q was refused: %v", expr, zone, err)
		}
	}
	for expr, want := range map[string]string{
		"":                   "needs a cadence",
		"*/30 * * * *":       "at most once an hour",
		"* * * * *":          "at most once an hour",
		"0,30 9 * * *":       "at most once an hour",
		"@every 2h":          "may be @hourly",
		"@yearly":            "may be @hourly",
		"0 0 9 * * *":        "five cron fields",
		"CRON_TZ=UTC @daily": "not in the cadence",
		"0 0 30 2 *":         "never fires",
	} {
		_, err := checkCadence(expr, "UTC", now)
		if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want a refusal saying %q", expr, err, want)
		}
	}
	for _, zone := range []string{"Local", "Mars/Olympus", "../etc"} {
		if _, err := checkCadence("@daily", zone, now); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("zone %q: %v", zone, err)
		}
	}
	// A weekday morning in Stockholm stays at nine across the change.
	c, _ := parseCadence("0 9 * * 1-5", "Europe/Stockholm")
	stockholm, _ := time.LoadLocation("Europe/Stockholm")
	for _, after := range []time.Time{
		time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC), time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC),
	} {
		if n := c.next(after).In(stockholm); n.Hour() != 9 || n.Weekday() == time.Saturday || n.Weekday() == time.Sunday {
			t.Errorf("after %s the next slot is %s", after, n)
		}
	}
}

// scheduleWorld is a hosted server with jobs, an owner the allowlist
// covers, and a clock a test moves.
type scheduleWorld struct {
	*apiFixture
	q     *recordingQueue
	mu    sync.Mutex
	clock time.Time
	owner store.User
}

func (w *scheduleWorld) now() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.clock
}

func (w *scheduleWorld) at(t time.Time) {
	w.mu.Lock()
	w.clock = t
	w.mu.Unlock()
}

func newScheduleWorld(t *testing.T, adjust func(*Deps)) *scheduleWorld {
	t.Helper()
	w := &scheduleWorld{clock: time.Date(2026, 11, 27, 9, 0, 30, 0, time.UTC)}
	list, err := auth.NewAllowlist(context.Background(), func(context.Context) (auth.AllowlistSet, error) {
		return auth.AllowlistSet{Emails: map[string]bool{"owner@example.com": false}}, nil
	}, auth.AllowlistRefresh, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.apiFixture, w.q = jobFixtureWith(t, func(d *Deps) {
		testModelKeys(d)
		d.Now = w.now
		d.Login = &Login{Allowlist: list, OrgID: auth.LocalID}
		if adjust != nil {
			adjust(d)
		}
	})
	w.owner = store.User{ID: auth.LocalID, OrgID: auth.LocalID, Email: "owner@example.com"}
	if err := w.store.Users().Create(context.Background(), w.owner); err != nil {
		t.Fatal(err)
	}
	return w
}

// schedule stores a schedule of the owner's for a request, due at next.
func (w *scheduleWorld) schedule(t *testing.T, name, cron string, req JobRequest, next time.Time) store.Schedule {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	sc := store.Schedule{ID: store.NewID(next.Add(-time.Hour)), OrgID: auth.LocalID, OwnerID: w.owner.ID, Name: name,
		Request: raw, Cron: cron, Zone: "UTC", State: store.ScheduleActive, NextRunAt: next}
	if err := w.store.Schedules().Create(context.Background(), sc); err != nil {
		t.Fatal(err)
	}
	return sc
}

func (w *scheduleWorld) reread(t *testing.T, id string) (store.Schedule, []store.ScheduleFire) {
	t.Helper()
	ctx := context.Background()
	sc, err := w.store.Schedules().Get(ctx, auth.LocalID, id)
	if err != nil {
		t.Fatal(err)
	}
	fires, err := w.store.Schedules().Fires(ctx, auth.LocalID, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	return sc, fires
}

func (w *scheduleWorld) jobsOf(t *testing.T, id string) []store.Job {
	t.Helper()
	js, err := w.store.Jobs().List(context.Background(), auth.LocalID, store.JobFilter{ScheduleID: id})
	if err != nil {
		t.Fatal(err)
	}
	return js
}

// A due schedule creates its job as its owner, for its slot, and moves
// to its next slot; nothing fires before a slot is due.
//
// @decision(DL-192)
func TestADueScheduleCreatesItsJobAsItsOwnerForItsSlot(t *testing.T) {
	w := newScheduleWorld(t, nil)
	slot := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	sc := w.schedule(t, "hourly", "@hourly", jobRequest(), slot)
	w.srv.Service().FireDueSchedules(context.Background())
	js := w.jobsOf(t, sc.ID)
	if len(js) != 1 || js[0].OwnerID != w.owner.ID || js[0].ScheduledFor == nil || !js[0].ScheduledFor.Equal(slot) {
		t.Fatalf("jobs %+v, want one of the owner's for %s", js, slot)
	}
	if len(w.q.args) != 1 {
		t.Errorf("%d jobs queued", len(w.q.args))
	}
	got, fires := w.reread(t, sc.ID)
	if want := slot.Add(time.Hour); !got.NextRunAt.Equal(want) {
		t.Errorf("next run %s, want %s", got.NextRunAt, want)
	}
	if len(fires) != 1 || fires[0].Outcome != store.FireStarted || fires[0].JobID != js[0].ID {
		t.Errorf("history %+v", fires)
	}
	w.srv.Service().FireDueSchedules(context.Background())
	if n := len(w.jobsOf(t, sc.ID)); n != 1 {
		t.Errorf("a schedule not yet due fired: %d jobs", n)
	}
}

// A slot is skipped while the schedule's previous job is unfinished or
// the owner is at the job limit, and a skip is not a failure.
//
// @decision(DL-193)
func TestASlotIsSkippedWhileBusyOrAtTheJobLimit(t *testing.T) {
	w := newScheduleWorld(t, func(d *Deps) { d.Quotas.MaxConcurrentJobs = 1 })
	slot := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	a := w.schedule(t, "a", "@hourly", jobRequest(), slot)
	ctx := context.Background()
	w.srv.Service().FireDueSchedules(ctx)
	w.at(slot.Add(time.Hour + 30*time.Second))
	b := w.schedule(t, "b", "@hourly", jobRequest(), slot.Add(time.Hour))
	w.srv.Service().FireDueSchedules(ctx)
	ga, fa := w.reread(t, a.ID)
	if len(fa) != 2 || fa[0].Outcome != store.FireSkipped || !strings.Contains(fa[0].Reason, "still running") || ga.Failures != 0 {
		t.Errorf("a busy schedule's history %+v, failures %d", fa, ga.Failures)
	}
	gb, fb := w.reread(t, b.ID)
	if len(fb) != 1 || fb[0].Outcome != store.FireSkipped || !strings.Contains(fb[0].Reason, "job limit") || gb.Failures != 0 {
		t.Errorf("a schedule at the limit: history %+v, failures %d", fb, gb.Failures)
	}
	if n := len(w.jobsOf(t, a.ID)) + len(w.jobsOf(t, b.ID)); n != 1 {
		t.Errorf("%d jobs, want the first alone", n)
	}
}

// Three failed fires in a row pause a schedule; a started fire resets
// the count.
//
// @decision(DL-193)
func TestThreeFailedFiresInARowPauseTheSchedule(t *testing.T) {
	w := newScheduleWorld(t, nil)
	slot := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	req := jobRequest()
	req.Model = retiredModel
	sc := w.schedule(t, "broken", "@hourly", req, slot)
	ctx := context.Background()
	for i := range MaxFailedFires {
		w.at(slot.Add(time.Duration(i)*time.Hour + 30*time.Second))
		w.srv.Service().FireDueSchedules(ctx)
	}
	got, fires := w.reread(t, sc.ID)
	if got.State != store.SchedulePaused || !strings.Contains(got.PausedReason, "3 failed runs") || !strings.Contains(got.PausedReason, "retired") {
		t.Errorf("after three failures: %+v", got)
	}
	if len(fires) != 3 || fires[0].Outcome != store.FireFailed {
		t.Errorf("history %+v", fires)
	}
	w.at(slot.Add(5 * time.Hour))
	w.srv.Service().FireDueSchedules(ctx)
	if _, again := w.reread(t, sc.ID); len(again) != 3 {
		t.Error("a paused schedule fired")
	}
}

// Slots missed while nothing fired are one history entry, and only the
// newest is fired.
//
// @decision(DL-192)
func TestMissedSlotsFireOnceForTheNewest(t *testing.T) {
	w := newScheduleWorld(t, nil)
	now := time.Date(2026, 11, 27, 9, 0, 30, 0, time.UTC)
	sc := w.schedule(t, "late", "@hourly", jobRequest(), now.Add(-5*time.Hour).Truncate(time.Hour))
	w.srv.Service().FireDueSchedules(context.Background())
	js := w.jobsOf(t, sc.ID)
	if len(js) != 1 || !js[0].ScheduledFor.Equal(now.Truncate(time.Hour)) {
		t.Fatalf("jobs %+v, want one for the newest slot", js)
	}
	got, fires := w.reread(t, sc.ID)
	if len(fires) != 2 || fires[1].Outcome != store.FireMissed || fires[1].Missed != 5 || fires[0].Outcome != store.FireStarted {
		t.Errorf("history %+v", fires)
	}
	if !got.NextRunAt.Equal(now.Truncate(time.Hour).Add(time.Hour)) {
		t.Errorf("next run %s", got.NextRunAt)
	}
}

// A fire runs as its owner as authentication would take them now: an
// owner the allowlist no longer covers is a failed fire.
//
// @decision(DL-192)
func TestAFireNeedsAnOwnerWhoMayStillSignIn(t *testing.T) {
	w := newScheduleWorld(t, nil)
	slot := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	sc := w.schedule(t, "gone", "@hourly", jobRequest(), slot)
	u := w.owner
	u.Email = "elsewhere@example.com"
	if err := w.store.Users().Update(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	w.srv.Service().FireDueSchedules(context.Background())
	_, fires := w.reread(t, sc.ID)
	if len(fires) != 1 || fires[0].Outcome != store.FireFailed || !strings.Contains(fires[0].Reason, "no longer sign in") {
		t.Errorf("history %+v", fires)
	}
	if n := len(w.jobsOf(t, sc.ID)); n != 0 {
		t.Errorf("%d jobs were created", n)
	}
}

// A schedule's request is checked as a job create, creating nothing,
// and may be report or pr.
//
// @decision(DL-191)
func TestAScheduledJobIsCheckedAsACreateWithoutCreating(t *testing.T) {
	w := newScheduleWorld(t, nil)
	p := auth.Principal{UserID: w.owner.ID, OrgID: auth.LocalID, Email: w.owner.Email}
	ctx := context.Background()
	if err := w.srv.Service().CheckScheduledJob(ctx, p, jobRequest()); err != nil {
		t.Fatalf("a valid request: %v", err)
	}
	sync := jobRequest()
	sync.Kind = store.JobKindSync
	if err := w.srv.Service().CheckScheduledJob(ctx, p, sync); !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "report or pr") {
		t.Errorf("a sync job: %v", err)
	}
	bad := jobRequest()
	bad.Model = retiredModel
	if err := w.srv.Service().CheckScheduledJob(ctx, p, bad); err == nil || !strings.Contains(err.Error(), "retired") {
		t.Errorf("a retired model: %v", err)
	}
	if js, _ := w.store.Jobs().List(ctx, auth.LocalID, store.JobFilter{}); len(js) != 0 || len(w.q.args) != 0 {
		t.Errorf("a check created %d jobs", len(js))
	}
}

// A daily slot in the hour a change back from summer time repeats fires
// once that day, and a slot a change forward skips has none that day.
//
// @decision(DL-191)
func TestADailySlotInTheChangedHourFiresOnce(t *testing.T) {
	c, err := parseCadence("0 2 * * *", "Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	autumn := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC) // 02:00 CEST
	if got, want := c.next(autumn), time.Date(2026, 10, 26, 1, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("after the first 02:00 of 25 October the next slot is %s, want %s (02:00 CET on the 26th)", got, want)
	}
	spring := time.Date(2026, 3, 28, 1, 0, 0, 0, time.UTC) // 02:00 CET on the 28th
	if got, want := c.next(spring), time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("after 02:00 on 28 March the next slot is %s, want %s (none on the 29th)", got, want)
	}
}

// A schedule paused after the tick read it creates nothing, and stays
// paused with its own reason.
//
// @decision(DL-192)
func TestAScheduleChangedAfterTheTickReadItCreatesNothing(t *testing.T) {
	w := newScheduleWorld(t, nil)
	slot := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	stale := w.schedule(t, "paused-meanwhile", "@hourly", jobRequest(), slot)
	ctx := context.Background()
	cur, _ := w.store.Schedules().Get(ctx, auth.LocalID, stale.ID)
	cur.State, cur.PausedReason = store.SchedulePaused, "by its owner"
	if err := w.store.Schedules().Update(ctx, cur); err != nil {
		t.Fatal(err)
	}
	fired := w.srv.Service().fireSchedule(ctx, stale, w.now(), false)
	if fired.Outcome != store.FireSkipped || !strings.Contains(fired.Reason, "changed") {
		t.Errorf("the fire %+v", fired)
	}
	if n := len(w.jobsOf(t, stale.ID)); n != 0 {
		t.Errorf("%d jobs were created", n)
	}
	if got, _ := w.reread(t, stale.ID); got.State != store.SchedulePaused || got.PausedReason != "by its owner" {
		t.Errorf("after the fire %+v", got)
	}
}
