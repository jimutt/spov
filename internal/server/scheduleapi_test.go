package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
)

func decodeSchedule(t *testing.T, body []byte) ScheduleView {
	t.Helper()
	var v ScheduleView
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("not a schedule: %v %s", err, body)
	}
	return v
}

// A schedule is created with its next runs, read back by name or id
// with its history, listed, and refused when its name, cadence or job
// is not one a schedule may have.
//
// @decision(DL-194) @decision(DL-191)
func TestSchedulesAreCreatedReadAndListed(t *testing.T) {
	w := newScheduleWorld(t, nil)
	resp, body := w.do(http.MethodPost, "/api/v1/schedules", ScheduleRequest{
		Name: "nightly", Cron: "0 2 * * *", Zone: "Europe/Stockholm", Job: jobRequest()})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d %s", resp.StatusCode, body)
	}
	v := decodeSchedule(t, body)
	if v.Name != "nightly" || v.State != "active" || len(v.NextRuns) != 3 || v.NextRunAt == nil || v.Job.Goal != jobRequest().Goal {
		t.Errorf("created %+v", v)
	}
	for _, ref := range []string{"nightly", v.ID} {
		resp, body := w.do(http.MethodGet, "/api/v1/schedules/"+ref, nil)
		if resp.StatusCode != http.StatusOK || decodeSchedule(t, body).ID != v.ID {
			t.Errorf("GET %s = %d %s", ref, resp.StatusCode, body)
		}
	}
	for name, req := range map[string]ScheduleRequest{
		"a taken name":       {Name: "nightly", Cron: "@daily", Job: jobRequest()},
		"a bad name":         {Name: "Not A Name", Cron: "@daily", Job: jobRequest()},
		"a cadence too fast": {Name: "fast", Cron: "*/10 * * * *", Job: jobRequest()},
		"a sync job":         {Name: "sync", Cron: "@daily", Job: func() JobRequest { r := jobRequest(); r.Kind = "sync"; return r }()},
	} {
		if resp, body := w.do(http.MethodPost, "/api/v1/schedules", req); resp.StatusCode < 400 || resp.StatusCode == http.StatusInternalServerError {
			t.Errorf("%s: %d %s", name, resp.StatusCode, body)
		}
	}
	resp, body = w.do(http.MethodGet, "/api/v1/schedules", nil)
	var list struct {
		Schedules []ScheduleView `json:"schedules"`
		Total     int            `json:"total"`
	}
	if err := json.Unmarshal(body, &list); err != nil || resp.StatusCode != http.StatusOK || list.Total != 1 || len(list.Schedules) != 1 {
		t.Errorf("list = %d %s", resp.StatusCode, body)
	}
}

// One owner may hold MaxSchedulesPerUser schedules.
//
// @decision(DL-191)
func TestAnOwnerHoldsAtMostSoManySchedules(t *testing.T) {
	w := newScheduleWorld(t, nil)
	for i := range MaxSchedulesPerUser {
		if resp, body := w.do(http.MethodPost, "/api/v1/schedules", ScheduleRequest{Name: fmt.Sprintf("s%d", i), Cron: "@daily", Job: jobRequest()}); resp.StatusCode != http.StatusCreated {
			t.Fatalf("schedule %d = %d %s", i, resp.StatusCode, body)
		}
	}
	resp, body := w.do(http.MethodPost, "/api/v1/schedules", ScheduleRequest{Name: "one-more", Cron: "@daily", Job: jobRequest()})
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(body), "20 of 20") {
		t.Errorf("one more = %d %s", resp.StatusCode, body)
	}
}

// A new cadence moves the next run from now; a pause hides it and a
// resume starts from the next slot with no failures counted.
//
// @decision(DL-194) @decision(DL-193)
func TestAScheduleIsChangedPausedAndResumed(t *testing.T) {
	w := newScheduleWorld(t, nil)
	w.do(http.MethodPost, "/api/v1/schedules", ScheduleRequest{Name: "weekly", Cron: "@weekly", Job: jobRequest()})
	cron := "0 6 * * *"
	resp, body := w.do(http.MethodPatch, "/api/v1/schedules/weekly", SchedulePatch{Cron: &cron})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch = %d %s", resp.StatusCode, body)
	}
	if v := decodeSchedule(t, body); v.Cron != cron || v.NextRunAt == nil || !v.NextRunAt.Equal(time.Date(2026, 11, 28, 6, 0, 0, 0, time.UTC)) {
		t.Errorf("after the patch %+v", v)
	}
	resp, body = w.do(http.MethodPost, "/api/v1/schedules/weekly/pause", nil)
	if v := decodeSchedule(t, body); resp.StatusCode != http.StatusOK || v.State != "paused" || v.NextRunAt != nil || v.PausedReason == "" {
		t.Errorf("pause = %d %s", resp.StatusCode, body)
	}
	resp, body = w.do(http.MethodPost, "/api/v1/schedules/weekly/resume", nil)
	if v := decodeSchedule(t, body); resp.StatusCode != http.StatusOK || v.State != "active" || v.PausedReason != "" || v.Failures != 0 {
		t.Errorf("resume = %d %s", resp.StatusCode, body)
	}
	if resp, body := w.do(http.MethodDelete, "/api/v1/schedules/weekly", nil); resp.StatusCode != http.StatusNoContent {
		t.Errorf("delete = %d %s", resp.StatusCode, body)
	}
	if resp, body := w.do(http.MethodGet, "/api/v1/schedules/weekly", nil); resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "no such schedule") {
		t.Errorf("a deleted schedule = %d %s, want a 404 naming a schedule", resp.StatusCode, body)
	}
	if resp, body := w.do(http.MethodPost, "/api/v1/schedules/weekly/pause", nil); resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "no such schedule") {
		t.Errorf("pausing a deleted schedule = %d %s, want a 404 naming a schedule", resp.StatusCode, body)
	}
}

// Run now creates a job as the schedule's owner and names it; a second
// run while that job is unfinished is a 409 saying why. The job says
// which schedule created it.
//
// @decision(DL-194) @decision(DL-192)
func TestRunNowCreatesAJobThatNamesItsSchedule(t *testing.T) {
	w := newScheduleWorld(t, nil)
	_, body := w.do(http.MethodPost, "/api/v1/schedules", ScheduleRequest{Name: "adhoc", Cron: "@monthly", Job: jobRequest()})
	sc := decodeSchedule(t, body)
	resp, body := w.do(http.MethodPost, "/api/v1/schedules/adhoc/run", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("run = %d %s", resp.StatusCode, body)
	}
	var out struct {
		JobID string `json:"job_id"`
	}
	_ = json.Unmarshal(body, &out)
	resp, body = w.do(http.MethodGet, "/api/v1/jobs/"+out.JobID, nil)
	var job JobView
	_ = json.Unmarshal(body, &job)
	if resp.StatusCode != http.StatusOK || job.Schedule == nil || job.Schedule.ID != sc.ID || job.Schedule.Name != "adhoc" {
		t.Errorf("the job = %d %s", resp.StatusCode, body)
	}
	// Within the same minute a run is the same slot, and answers with its job.
	if resp, body := w.do(http.MethodPost, "/api/v1/schedules/adhoc/run", nil); resp.StatusCode != http.StatusAccepted || !strings.Contains(string(body), out.JobID) {
		t.Errorf("a second run in the same minute = %d %s", resp.StatusCode, body)
	}
	w.at(w.now().Add(time.Minute))
	resp, body = w.do(http.MethodPost, "/api/v1/schedules/adhoc/run", nil)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "still running") {
		t.Errorf("a second run = %d %s", resp.StatusCode, body)
	}
	_, body = w.do(http.MethodGet, "/api/v1/schedules/adhoc", nil)
	// A skipped run now leaves no history; the slot it ran stays put.
	if v := decodeSchedule(t, body); len(v.History) != 2 || v.LastRun == nil || v.LastRun.Outcome != "started" || v.NextRunAt == nil || !v.NextRunAt.Equal(*sc.NextRunAt) {
		t.Errorf("after two runs %+v", v)
	}
}

// A local server has no schedules.
//
// @decision(DL-194)
func TestALocalServerHasNoSchedules(t *testing.T) {
	w := newScheduleWorld(t, func(d *Deps) { d.Local = true })
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/schedules"}, {http.MethodPost, "/api/v1/schedules"}, {http.MethodGet, "/api/v1/schedules/x"},
	} {
		var body any
		if r.method == http.MethodPost {
			body = ScheduleRequest{Name: "x", Cron: "@daily", Job: jobRequest()}
		}
		if resp, out := w.do(r.method, r.path, body); resp.StatusCode != http.StatusNotImplemented {
			t.Errorf("%s %s = %d %s", r.method, r.path, resp.StatusCode, out)
		}
	}
}

// An admin reaches another person's schedule by id, never by name, may
// pause and run it (the run as its owner, with an audit row), and may
// not change the job it runs; the org's listing is the admin's alone.
//
// @decision(DL-194)
func TestAnAdminActsOnAnotherPersonsScheduleAsTheOwner(t *testing.T) {
	w := newScheduleWorld(t, nil)
	_, body := w.do(http.MethodPost, "/api/v1/schedules", ScheduleRequest{Name: "theirs", Cron: "@daily", Job: jobRequest()})
	sc := decodeSchedule(t, body)
	admin := auth.Principal{UserID: "u-admin", OrgID: auth.LocalID, Email: "admin@example.com", Admin: true}
	w.auth.inner = auth.StaticAuthenticator{Principal: admin}
	if resp, _ := w.do(http.MethodGet, "/api/v1/schedules/theirs", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("an admin's name lookup of another's schedule = %d", resp.StatusCode)
	}
	if resp, body := w.do(http.MethodGet, "/api/v1/schedules/"+sc.ID, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("an admin's id lookup = %d %s", resp.StatusCode, body)
	}
	job := jobRequest()
	job.Goal = "the admin's own goal"
	if resp, body := w.do(http.MethodPatch, "/api/v1/schedules/"+sc.ID, SchedulePatch{Job: &job}); resp.StatusCode != http.StatusUnprocessableEntity ||
		!strings.Contains(string(body), "only the schedule's owner") {
		t.Errorf("an admin's change of the job = %d %s", resp.StatusCode, body)
	}
	resp, body := w.do(http.MethodPost, "/api/v1/schedules/"+sc.ID+"/run", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("an admin's run = %d %s", resp.StatusCode, body)
	}
	var out struct {
		JobID string `json:"job_id"`
	}
	_ = json.Unmarshal(body, &out)
	if j, err := w.store.Jobs().Get(context.Background(), auth.LocalID, out.JobID); err != nil || j.OwnerID != w.owner.ID {
		t.Errorf("the admin's run created %+v, %v; want a job of the owner's", j, err)
	}
	evs, _ := w.store.AdminEvents().List(context.Background(), auth.LocalID, 10)
	if len(evs) != 1 || evs[0].Action != AdminScheduleRun || evs[0].Actor != "admin@example.com" || strings.Contains(evs[0].Detail, jobRequest().Goal) {
		t.Errorf("audit rows %+v", evs)
	}
	resp, body = w.do(http.MethodGet, "/api/v1/schedules?all=true", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), sc.ID) {
		t.Errorf("the org's listing = %d %s", resp.StatusCode, body)
	}
	if resp, body := w.do(http.MethodGet, "/api/v1/schedules", nil); !strings.Contains(string(body), `"total":0`) {
		t.Errorf("the admin's own listing = %d %s", resp.StatusCode, body)
	}
}

// A resume of a schedule whose owner may no longer sign in is refused.
//
// @decision(DL-194)
func TestAScheduleOfARemovedOwnerIsNotResumed(t *testing.T) {
	w := newScheduleWorld(t, nil)
	w.do(http.MethodPost, "/api/v1/schedules", ScheduleRequest{Name: "gone", Cron: "@daily", Job: jobRequest()})
	w.do(http.MethodPost, "/api/v1/schedules/gone/pause", nil)
	u := w.owner
	u.Email = "elsewhere@example.com"
	if err := w.store.Users().Update(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	if resp, body := w.do(http.MethodPost, "/api/v1/schedules/gone/resume", nil); resp.StatusCode != http.StatusUnprocessableEntity ||
		!strings.Contains(string(body), "no longer sign in") {
		t.Errorf("resume = %d %s", resp.StatusCode, body)
	}
}
