package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/store"
)

// sandboxCall is one request the fake sandbox saw.
type sandboxCall struct {
	method, path, query, body string
}

// recordingSandbox is a job whose fake sandbox records every request and
// answers each with an empty success.
func recordingSandbox(t *testing.T, conversation string) (*Service, store.Job, func() []sandboxCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []sandboxCall
	svc, job, _ := jobSandbox(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, sandboxCall{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, string(body)})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"interrupted":true}`))
	})
	ctx := context.Background()
	if _, err := svc.deps.Store.Conversations().UpsertConversation(ctx, store.Conversation{
		ID: store.NewID(svc.deps.Now()), OrgID: job.OrgID, SessionID: job.SessionID, OpenCodeID: conversation,
	}); err != nil {
		t.Fatal(err)
	}
	return svc, job, func() []sandboxCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]sandboxCall(nil), calls...)
	}
}

// At the wall clock the loop is stopped for good and then the turn is
// interrupted with resume=false, so nothing queued starts. No marker is
// written: an interrupt marker would read as a human steering the job.
//
// @decision(DL-049) @decision(DL-053)
func TestEndingARunStopsTheLoopThenTheTurn(t *testing.T) {
	svc, job, calls := recordingSandbox(t, "ses_1")
	sess, err := svc.deps.Store.Sessions().Get(context.Background(), job.OrgID, job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.EndJobRun(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(got) != 2 {
		t.Fatalf("the sandbox saw %d requests, want 2: %+v", len(got), got)
	}
	if !strings.HasSuffix(got[0].path, "/api/session/ses_1/command") ||
		!strings.Contains(got[0].body, `"name":"`+opencode.PluginCommand+`"`) ||
		!strings.Contains(got[0].body, `"text":"`+LoopBound+`"`) {
		t.Errorf("first request = %+v, want the plugin's stop command", got[0])
	}
	if !strings.HasSuffix(got[1].path, "/api/session/ses_1/interrupt") || got[1].query != "resume=false" {
		t.Errorf("second request = %+v, want the interrupt with resume=false", got[1])
	}
	if n := len(markersOn(t, svc, sess, jobs.MarkerInterrupted)); n != 0 {
		t.Errorf("%d interrupt markers were written; the supervisor would read one as a steer", n)
	}
}

// A job whose sandbox is not running has nothing to stop.
//
// @decision(DL-049)
func TestEndingARunNeedsARunningSession(t *testing.T) {
	svc, job, calls := recordingSandbox(t, "ses_1")
	ctx := context.Background()
	sess, err := svc.deps.Store.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	sess.State = store.StateStopped
	if err := svc.deps.Store.Sessions().Update(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if err := svc.EndJobRun(ctx, job); !errors.Is(err, ErrSessionNotRunning) {
		t.Errorf("EndJobRun = %v, want ErrSessionNotRunning", err)
	}
	if n := len(calls()); n != 0 {
		t.Errorf("the sandbox saw %d requests", n)
	}
}

// The conversation id is chosen by the sandbox, so every server call that
// puts it in a path escapes it; a `?` would otherwise cut the path short
// and post somewhere nobody intended.
//
// @decision(DL-019) @decision(DL-049)
func TestEveryConversationPathIsEscaped(t *testing.T) {
	svc, job, calls := recordingSandbox(t, "ses_1?x=1")
	ctx := context.Background()
	if err := svc.EndJobRun(ctx, job); err != nil {
		t.Fatal(err)
	}
	sess, err := svc.deps.Store.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.withConnection(ctx, auth.Principal{OrgID: sess.OrgID, UserID: sess.OwnerID}, sess,
		func(conn Connection) error {
			if err := setConversationModel(ctx, conn, "ses_1?x=1", "anthropic/claude-sonnet-5"); err != nil {
				return err
			}
			return openJob(ctx, conn, "ses_1?x=1", job)
		}); err != nil {
		t.Fatal(err)
	}
	for _, c := range calls() {
		if !strings.Contains(c.path, "/api/session/ses_1%3Fx=1/") {
			t.Errorf("%s %s?%s: the conversation id was not escaped", c.method, c.path, c.query)
		}
	}
	if n := len(calls()); n != 4 {
		t.Errorf("want the command, the interrupt, the model call and the opening; got %d requests", n)
	}
}

// A job's wall clock and its sandbox timeout: the request's duration, the
// configured preference, the profile's default, then the built-in one.
// An explicit duration that does not fit is refused; the others are
// clamped. The timeout is the clock, the margin and the steering
// headroom, cut to the sandbox limit.
//
// @decision(DL-049)
func TestAJobsWallClockIsResolvedAtCreate(t *testing.T) {
	const hour = time.Hour
	margin := jobs.WallClockMargin
	timeoutFor := func(clock time.Duration) time.Duration { return clock + margin + jobs.SteerHeadroom }
	for _, tc := range []struct {
		name        string
		asked       string
		preferred   bool
		profile     time.Duration
		limit       time.Duration
		sandbox     time.Duration
		want        time.Duration
		wantTimeout time.Duration
		refused     string
	}{
		{name: "the default", limit: 4 * hour, sandbox: 8 * hour, want: hour, wantTimeout: timeoutFor(hour)},
		{name: "the default under a lower limit", limit: 30 * time.Minute, sandbox: 8 * hour, want: 30 * time.Minute, wantTimeout: timeoutFor(30 * time.Minute)},
		{name: "the default under a short sandbox", limit: 4 * hour, sandbox: 20*time.Minute + margin, want: 20 * time.Minute, wantTimeout: 20*time.Minute + margin},
		{name: "asked for", asked: "45m", limit: 4 * hour, sandbox: 8 * hour, want: 45 * time.Minute, wantTimeout: timeoutFor(45 * time.Minute)},
		{name: "asked for with spaces", asked: " 2h ", limit: 4 * hour, sandbox: 8 * hour, want: 2 * hour, wantTimeout: timeoutFor(2 * hour)},
		{name: "steering room is cut first", asked: "4h", limit: 4 * hour, sandbox: 4*hour + margin + 20*time.Minute, want: 4 * hour, wantTimeout: 4*hour + margin + 20*time.Minute},
		{name: "the profile's default", profile: 2 * hour, limit: 4 * hour, sandbox: 8 * hour, want: 2 * hour, wantTimeout: timeoutFor(2 * hour)},
		{name: "the profile's default over the limit is clamped", profile: 6 * hour, limit: 4 * hour, sandbox: 8 * hour, want: 4 * hour, wantTimeout: timeoutFor(4 * hour)},
		{name: "a request wins over the profile", asked: "45m", profile: 2 * hour, limit: 4 * hour, sandbox: 8 * hour, want: 45 * time.Minute, wantTimeout: timeoutFor(45 * time.Minute)},
		{name: "a preference wins over the profile", asked: "3h", preferred: true, profile: 2 * hour, limit: 4 * hour, sandbox: 8 * hour, want: 3 * hour, wantTimeout: timeoutFor(3 * hour)},
		{name: "a preference over the limit is clamped", asked: "9h", preferred: true, limit: 4 * hour, sandbox: 8 * hour, want: 4 * hour, wantTimeout: timeoutFor(4 * hour)},
		{name: "a preference that does not parse", asked: "an hour", preferred: true, limit: 4 * hour, sandbox: 8 * hour, refused: "sandbox.job_max_duration"},
		{name: "over the limit", asked: "5h", limit: 4 * hour, sandbox: 8 * hour, refused: "exceeds your limit of 4h0m0s"},
		{name: "longer than the sandbox allows", asked: "3h", limit: 4 * hour, sandbox: 3 * hour, refused: "does not fit in your 3h0m0s sandbox limit"},
		{name: "not a duration", asked: "an hour", limit: 4 * hour, sandbox: 8 * hour, refused: "not a duration"},
		{name: "too short", asked: "10s", limit: 4 * hour, sandbox: 8 * hour, refused: "at least 1m0s"},
		{name: "a sandbox too short for any job", limit: 4 * hour, sandbox: margin, refused: "leaves no time"},
		{name: "a limit below the minimum", limit: 30 * time.Second, sandbox: 8 * hour, refused: "your job duration limit of 30s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, timeout, err := jobClock(tc.asked, tc.preferred, tc.profile, Quotas{MaxJobDuration: tc.limit, MaxTimeout: tc.sandbox})
			if tc.refused != "" {
				if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), tc.refused) {
					t.Fatalf("err = %v, want an invalid request saying %q", err, tc.refused)
				}
				return
			}
			if err != nil || got != tc.want || timeout != tc.wantTimeout {
				t.Fatalf("= %s, %s, %v; want %s, %s", got, timeout, err, tc.want, tc.wantTimeout)
			}
		})
	}
}

// A job's sandbox timeout follows from its clock, whatever timeout the
// request carried, and the profile's default duration applies when the
// request names none.
//
// @decision(DL-049)
func TestAJobsSandboxTimeoutFollowsItsClock(t *testing.T) {
	f, _ := jobFixture(t)
	if _, err := f.store.Users().Get(context.Background(), auth.LocalID, auth.LocalID); err == nil {
		t.Fatal("the fixture already has a user row, so the profile default would not be the one set here")
	}
	resp, body := f.do(http.MethodPatch, "/api/v1/me", map[string]any{"default_job_duration": "2h"})
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"default_job_duration":"2h0m0s"`) {
		t.Fatalf("setting the default: %d %s", resp.StatusCode, body)
	}
	req := jobRequest()
	req.Sandbox.TimeoutSecs = int((10 * time.Minute).Seconds())
	resp, body = f.do(http.MethodPost, "/api/v1/jobs", req)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	var job JobView
	if err := json.Unmarshal(body, &job); err != nil {
		t.Fatal(err)
	}
	if job.MaxDuration != "2h0m0s" {
		t.Errorf("the job's clock = %s, want the profile's 2h", job.MaxDuration)
	}
	sess, err := f.store.Sessions().Get(context.Background(), auth.LocalID, job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if want := 2*time.Hour + jobs.WallClockMargin + jobs.SteerHeadroom; sess.Request.Sandbox.Timeout != want {
		t.Errorf("the sandbox timeout = %s, want %s from the clock", sess.Request.Sandbox.Timeout, want)
	}

	resp, body = f.do(http.MethodPatch, "/api/v1/me", map[string]any{"default_job_duration": "20s"})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("a default under the minimum = %d %s, want 422", resp.StatusCode, body)
	}
}

// The resolved clock is what the row stores and the API reports, and a
// request over the limit is refused over HTTP with the limit named.
//
// @decision(DL-049)
func TestACreatedJobCarriesItsWallClock(t *testing.T) {
	f, _ := jobFixture(t)
	req := jobRequest()
	req.MaxDuration = "45m"
	resp, body := f.do(http.MethodPost, "/api/v1/jobs", req)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"max_duration":"45m0s"`) {
		t.Errorf("the created job does not report its clock: %s", body)
	}
	jobsList, err := f.store.Jobs().List(context.Background(), auth.LocalID, store.JobFilter{})
	if err != nil || len(jobsList) != 1 || jobsList[0].MaxDuration != 45*time.Minute {
		t.Fatalf("stored = %+v, %v; want one job with a 45m clock", jobsList, err)
	}

	req.MaxDuration = "9h"
	resp, body = f.do(http.MethodPost, "/api/v1/jobs", req)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(body), "exceeds your limit") {
		t.Errorf("a clock over the limit = %d %s, want 422 naming the limit", resp.StatusCode, body)
	}
}

// A blocked job's loop may be held by DL-052 (a human wrote to it while
// it was parked), and the supervisor never steers a blocked job, so only
// the resume can release it.
//
// @decision(DL-052) @decision(DL-054)
func TestResumingABlockedJobReleasesItsLoop(t *testing.T) {
	svc, job, calls := recordingSandbox(t, "ses_1")
	setJobState(t, svc, job, store.JobBlocked)
	p := auth.Principal{OrgID: job.OrgID, UserID: job.OwnerID}
	if err := svc.ResumeJob(context.Background(), p, job.ID); err != nil {
		t.Fatal(err)
	}
	var released bool
	for _, c := range calls() {
		if strings.HasSuffix(c.path, "/api/session/ses_1/command") && strings.Contains(c.body, `"text":"`+LoopResume+`"`) {
			released = true
		}
	}
	if !released {
		t.Errorf("resuming a blocked job sent no resume to its loop: %+v", calls())
	}
	got, err := svc.deps.Store.Jobs().Get(context.Background(), job.OrgID, job.ID)
	if err != nil || got.State != store.JobRunning {
		t.Errorf("state = %v, %v; want running", got.State, err)
	}
}

// Each argument the server sends is a case in the plugin's own switch.
// One that is not falls through to `status`, answers with the loop's
// state, and does nothing.
//
// @decision(DL-049) @decision(DL-054)
func TestTheLoopCommandsAreOnesThePluginHandles(t *testing.T) {
	src, err := opencode.Plugin{Kind: "report", Goal: "g"}.Source()
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{LoopPause, LoopResume, LoopBound} {
		if !strings.Contains(src, `case "`+arg+`":`) {
			t.Errorf("the plugin has no case for %q; the command would only report status", arg)
		}
	}
}

// A job's sandbox idles out only at its sandbox timeout, whatever idle
// timeout the request or the server's defaults name: nothing inbound
// shows that a working job is in use.
//
// @decision(DL-049)
func TestAJobsSandboxIdlesOutOnlyAtItsTimeout(t *testing.T) {
	f, _ := jobFixture(t)
	// An idle timeout the server's default timeout would refuse is not
	// a reason to refuse a job, since the job does not use it.
	req := jobRequest()
	req.Sandbox.IdleSecs = int((30 * time.Hour).Seconds())
	resp, body := f.do(http.MethodPost, "/api/v1/jobs", req)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	var job JobView
	if err := json.Unmarshal(body, &job); err != nil {
		t.Fatal(err)
	}
	sess, err := f.store.Sessions().Get(context.Background(), auth.LocalID, job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Request.Sandbox.IdleTimeout != sess.Request.Sandbox.Timeout || sess.Request.Sandbox.Timeout == 0 {
		t.Errorf("idle timeout %s, sandbox timeout %s; want them equal", sess.Request.Sandbox.IdleTimeout, sess.Request.Sandbox.Timeout)
	}
}
