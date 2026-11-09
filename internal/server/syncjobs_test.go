package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/jobs/jobstest"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/session"
	"github.com/jimutt/spov/internal/store"
)

// syncJobRequest is a sync job seeded from a checkout's branch.
func syncJobRequest(bundle []byte) JobRequest {
	req := jobRequest()
	req.Kind = store.JobKindSync
	req.Sync = &SessionSyncRequest{Branch: "feat/x", BaseCommit: strings.Repeat("a", 40)}
	if bundle != nil {
		req.Sync.Bundle = base64.StdEncoding.EncodeToString(bundle)
	}
	return req
}

func createJobView(t *testing.T, f *apiFixture, req JobRequest) JobView {
	t.Helper()
	resp, body := f.do(http.MethodPost, "/api/v1/jobs", req)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	var v JobView
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// A sync job is refused without a seed, and only it takes a seed or a
// hold.
//
// @decision(DL-100) @decision(DL-101)
func TestASyncJobNeedsASeedAndOnlyItHasOne(t *testing.T) {
	f, _ := jobFixture(t)
	unseeded := syncJobRequest(nil)
	unseeded.Sync = nil
	seededPR := jobRequest()
	seededPR.Sync = syncJobRequest(nil).Sync
	heldPR := jobRequest()
	heldPR.Hold = "1h"
	for name, req := range map[string]JobRequest{
		"a sync job with no seed": unseeded, "a pr job with a seed": seededPR, "a pr job with a hold": heldPR,
	} {
		if resp, body := f.do(http.MethodPost, "/api/v1/jobs", req); resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %s, want 422", name, resp.StatusCode, body)
		}
	}
	createJobView(t, f, syncJobRequest(nil))
}

// A sync job lands in its repository's directory, and a landing that
// names anything else is refused.
//
// @decision(DL-100)
func TestASyncJobLandsInItsRepository(t *testing.T) {
	f, _ := jobFixture(t)
	v := createJobView(t, f, syncJobRequest(nil))
	sess, err := f.store.Sessions().Get(context.Background(), auth.LocalID, v.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Request.Landing != "app" || sess.Request.Sync == nil || sess.Request.Sync.Branch != "feat/x" {
		t.Errorf("session request = landing %q, sync %+v", sess.Request.Landing, sess.Request.Sync)
	}
	elsewhere := syncJobRequest(nil)
	elsewhere.Landing = "other"
	if resp, body := f.do(http.MethodPost, "/api/v1/jobs", elsewhere); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("a landing elsewhere: %d %s, want 422", resp.StatusCode, body)
	}
	same := syncJobRequest(nil)
	same.Landing = "app"
	createJobView(t, f, same)
}

// The seed travels in the create's body, past huma's 1 MiB default, and
// waits in its own row rather than the session's request.
//
// @decision(DL-100)
func TestAJobCreateTakesASyncBundleOverOneMiB(t *testing.T) {
	f, _ := jobFixture(t)
	bundle := []byte(strings.Repeat("b", 3<<19))
	v := createJobView(t, f, syncJobRequest(bundle))
	got, err := f.store.Jobs().Seed(context.Background(), auth.LocalID, v.ID)
	if err != nil || string(got) != string(bundle) {
		t.Fatalf("the stored seed is %d bytes, %v; want the %d sent", len(got), err, len(bundle))
	}
	_, body := f.do(http.MethodGet, "/api/v1/sessions/"+v.SessionID, nil)
	if strings.Contains(string(body), base64.StdEncoding.EncodeToString(bundle[:64])) {
		t.Error("the session view carries the seed")
	}
}

// The seed is read into the launch, dropped once the session runs, and
// dropped with a job cancelled before it launched.
//
// @decision(DL-100)
func TestASyncJobsSeedIsKeptUntilItsLaunchAndNoLonger(t *testing.T) {
	f, _ := jobFixture(t)
	ctx := context.Background()
	var launched []byte
	inner := f.srv.svc.deps.Launch
	f.srv.svc.deps.Launch = func(ctx context.Context, opts session.Options, steps session.StepReporter) (*session.Session, error) {
		if opts.Sync != nil {
			launched = opts.Sync.Bundle
		}
		return inner(ctx, opts, steps)
	}
	v := createJobView(t, f, syncJobRequest([]byte("the seed")))
	job, err := f.store.Jobs().Get(ctx, auth.LocalID, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.srv.svc.ProvisionJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if string(launched) != "the seed" {
		t.Errorf("the launch was handed %q, want the stored seed", launched)
	}
	if _, err := f.store.Jobs().Seed(ctx, auth.LocalID, v.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the seed outlived the launch: %v", err)
	}

	queued := createJobView(t, f, syncJobRequest([]byte("never launched")))
	if resp, body := f.do(http.MethodDelete, "/api/v1/jobs/"+queued.ID, nil); resp.StatusCode >= 300 {
		t.Fatalf("cancel: %d %s", resp.StatusCode, body)
	}
	if _, err := f.store.Jobs().Seed(ctx, auth.LocalID, queued.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a cancelled job kept its seed: %v", err)
	}
}

// A sync job's timeout is its clock, the margin, the steering headroom
// and the hold, cut headroom first, then the hold.
//
// @decision(DL-101) @decision(DL-049)
func TestASyncJobsHoldIsAddedToItsTimeout(t *testing.T) {
	const hour = time.Hour
	margin, headroom := jobs.WallClockMargin, jobs.SteerHeadroom
	for _, tc := range []struct {
		name        string
		hold        string
		preferred   bool
		sandbox     time.Duration
		wantTimeout time.Duration
		refused     string
	}{
		{name: "the default hold", sandbox: 24 * hour, wantTimeout: hour + margin + headroom + jobs.DefaultSyncHold},
		{name: "an explicit hold", hold: "2h", sandbox: 24 * hour, wantTimeout: hour + margin + headroom + 2*hour},
		{name: "headroom is cut first", hold: "30m", sandbox: hour + margin + 40*time.Minute, wantTimeout: hour + margin + 40*time.Minute},
		{name: "then a preferred hold", hold: "2h", preferred: true, sandbox: hour + margin + 20*time.Minute, wantTimeout: hour + margin + 20*time.Minute},
		{name: "an explicit hold that cannot fit", hold: "2h", sandbox: hour + margin + 20*time.Minute, refused: "does not fit"},
		{name: "too short", hold: "10s", sandbox: 24 * hour, refused: "at least 1m0s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := JobRequest{Kind: store.JobKindSync, Hold: tc.hold, HoldPreferred: tc.preferred}
			clock, timeout, err := jobClockFor(req, 0, Quotas{MaxJobDuration: 4 * hour, MaxTimeout: tc.sandbox})
			if tc.refused != "" {
				if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), tc.refused) {
					t.Fatalf("err = %v, want a refusal saying %q", err, tc.refused)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if clock != hour || timeout != tc.wantTimeout {
				t.Errorf("clock %s, timeout %s; want 1h and %s", clock, timeout, tc.wantTimeout)
			}
		})
	}
	// Another kind's timeout is unchanged.
	_, timeout, err := jobClockFor(JobRequest{Kind: store.JobKindPR}, 0, Quotas{MaxJobDuration: 4 * hour, MaxTimeout: 24 * hour})
	if err != nil || timeout != hour+margin+headroom {
		t.Errorf("a pr job's timeout = %s, %v", timeout, err)
	}
}

// syncFixture is a job API fixture with a running session that a sync
// job owns, its export answered with canned output.
type syncRoutesFixture struct {
	f    *apiFixture
	sess store.Session
	job  store.Job
}

func newSyncRoutesFixture(t *testing.T, kind store.JobKind, state store.JobState) *syncRoutesFixture {
	t.Helper()
	f, _ := jobFixture(t)
	ctx := context.Background()
	id := f.create(repoReq("synced"))
	_ = f.events(id)
	sess, err := f.store.Sessions().Get(ctx, auth.LocalID, id)
	if err != nil {
		t.Fatal(err)
	}
	// Prime the export's command while the session has no job, then can
	// its output.
	_, _ = f.do(http.MethodPost, "/api/v1/sessions/"+id+"/sync-export", nil)
	if h := f.provider.Handles()[sess.SandboxID]; h != nil {
		if log := h.ExecLog(); len(log) > 0 {
			out, _ := json.Marshal(map[string]any{"branch": "main", "head_commit": strings.Repeat("c", 40)})
			f.provider.ExecResults[strings.Join(log[len(log)-1], " ")] = sandbox.ExecResult{Stdout: string(out)}
		}
	}
	job := store.Job{
		ID: store.NewID(time.Now()), OrgID: sess.OrgID, OwnerID: sess.OwnerID, SessionID: sess.ID,
		Kind: kind, State: store.JobQueued, Goal: "work",
	}
	if err := f.store.Jobs().Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	for _, st := range jobstest.PathTo(state) {
		job.State = st
		if err := f.store.Jobs().Update(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	return &syncRoutesFixture{f: f, sess: sess, job: job}
}

func (s *syncRoutesFixture) export() int {
	r, _ := s.f.do(http.MethodPost, "/api/v1/jobs/"+s.job.ID+"/sync-export", nil)
	return r.StatusCode
}

func (s *syncRoutesFixture) release(applied bool) (int, JobView) {
	r, body := s.f.do(http.MethodPost, "/api/v1/jobs/"+s.job.ID+"/release", map[string]bool{"applied": applied})
	var v JobView
	_ = json.Unmarshal(body, &v)
	return r.StatusCode, v
}

func (s *syncRoutesFixture) stopSession(t *testing.T) {
	t.Helper()
	if _, err := s.f.store.Sessions().MarkStopped(context.Background(), s.sess.OrgID, s.sess.ID, store.StateStopped, time.Now(), ""); err != nil {
		t.Fatal(err)
	}
}

// Every row of DL-102's table.
//
// @decision(DL-102)
func TestTheSyncRoutesAnswerAsTheTableSays(t *testing.T) {
	for _, tc := range []struct {
		name           string
		kind           store.JobKind
		state          store.JobState
		stopped        bool
		export, rel    int
		releaseApplied bool
	}{
		{name: "held", kind: store.JobKindSync, state: store.JobSucceeded, export: 200, rel: 200},
		{name: "held after a failure", kind: store.JobKindSync, state: store.JobFailed, export: 200, rel: 200},
		{name: "running", kind: store.JobKindSync, state: store.JobRunning, export: 200, rel: 409},
		{name: "steered", kind: store.JobKindSync, state: store.JobSteered, export: 200, rel: 409},
		{name: "delivering", kind: store.JobKindSync, state: store.JobDelivering, export: 409, rel: 409},
		{name: "released", kind: store.JobKindSync, state: store.JobSucceeded, stopped: true, export: 409, rel: 200},
		{name: "cancelled", kind: store.JobKindSync, state: store.JobCancelled, stopped: true, export: 409, rel: 200},
		{name: "a pr job", kind: store.JobKindPR, state: store.JobSucceeded, export: 404, rel: 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSyncRoutesFixture(t, tc.kind, tc.state)
			if tc.stopped {
				s.stopSession(t)
			}
			if got := s.export(); got != tc.export {
				t.Errorf("sync-export = %d, want %d", got, tc.export)
			}
			if got, _ := s.release(false); got != tc.rel {
				t.Errorf("release = %d, want %d", got, tc.rel)
			}
		})
	}
	t.Run("queued, with no sandbox yet", func(t *testing.T) {
		f, _ := jobFixture(t)
		v := createJobView(t, f, syncJobRequest(nil))
		for _, path := range []string{"/sync-export", "/release"} {
			if r, body := f.do(http.MethodPost, "/api/v1/jobs/"+v.ID+path, nil); r.StatusCode != http.StatusConflict {
				t.Errorf("%s = %d %s, want 409", path, r.StatusCode, body)
			}
		}
	})
}

// A held job's state can be exported again and again, then released as
// applied; after that it is gone and a second release changes nothing.
//
// @decision(DL-102) @decision(DL-101)
func TestAHeldSyncJobIsExportedThenReleased(t *testing.T) {
	s := newSyncRoutesFixture(t, store.JobKindSync, store.JobSucceeded)
	for range 2 {
		if got := s.export(); got != http.StatusOK {
			t.Fatalf("sync-export = %d", got)
		}
	}
	_, body := s.f.do(http.MethodGet, "/api/v1/jobs/"+s.job.ID, nil)
	var before JobView
	_ = json.Unmarshal(body, &before)
	if before.Sync == nil || !before.Sync.Held || before.Sync.HeldUntil == nil {
		t.Errorf("a held job's view = %+v", before.Sync)
	}
	code, v := s.release(true)
	if code != http.StatusOK || v.Sync == nil || v.Sync.Held || v.Sync.AppliedAt == nil || v.Sync.ReleasedAt == nil {
		t.Fatalf("release = %d, sync %+v", code, v.Sync)
	}
	sess, _ := s.f.store.Sessions().Get(context.Background(), s.sess.OrgID, s.sess.ID)
	if sess.State.Live() {
		t.Errorf("the session is still %s after a release", sess.State)
	}
	if got := s.export(); got != http.StatusConflict {
		t.Errorf("sync-export after a release = %d, want 409", got)
	}
	if code, again := s.release(false); code != http.StatusOK || again.Sync.AppliedAt == nil {
		t.Errorf("a second release = %d, %+v", code, again.Sync)
	}
}

// Stopping or joining a held job's session is refused, so its work is
// not lost or changed by accident.
//
// @decision(DL-102) @decision(DL-101)
func TestAHeldSessionIsNotStoppedOrJoinedByAccident(t *testing.T) {
	s := newSyncRoutesFixture(t, store.JobKindSync, store.JobSucceeded)
	r, body := s.f.do(http.MethodDelete, "/api/v1/sessions/"+s.sess.ID, nil)
	if r.StatusCode != http.StatusConflict || !strings.Contains(string(body), "spov jobs release") {
		t.Errorf("stop = %d %s, want 409 naming the release", r.StatusCode, body)
	}
	r, body = s.f.do(http.MethodPost, "/api/v1/sessions/"+s.sess.ID+"/connect", nil)
	if r.StatusCode != http.StatusConflict || !strings.Contains(string(body), "spov jobs apply") {
		t.Errorf("connect = %d %s, want 409 naming apply", r.StatusCode, body)
	}
	_, body = s.f.do(http.MethodGet, "/api/v1/sessions/"+s.sess.ID, nil)
	var view SessionView
	_ = json.Unmarshal(body, &view)
	if view.Job == nil || !view.Job.Held {
		t.Errorf("the session's job badge = %+v, want held", view.Job)
	}
	sess, _ := s.f.store.Sessions().Get(context.Background(), s.sess.OrgID, s.sess.ID)
	if !sess.State.Live() {
		t.Error("the refused stop ended the session")
	}
	// The server's own reads, the recorder's among them, still connect.
	p := auth.Principal{OrgID: sess.OrgID, UserID: sess.OwnerID}
	if _, err := s.f.srv.svc.Connect(context.Background(), p, sess.ID); err != nil {
		t.Errorf("the server's own connect to a held session: %v", err)
	}
}
