package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/telemetry"
	"github.com/jimutt/spov/internal/telemetry/telemetrytest"
)

// The server counts API requests by operation and status class, a
// launch by its outcome, a stop by its reason, and a refusal by the
// limit that refused it.
//
// @decision(DL-011)
func TestTheServerRecordsRequestsLaunchesStopsAndRefusals(t *testing.T) {
	r := telemetrytest.New(t)
	f := newAPIFixtureWith(t, func(d *Deps) {
		d.Metrics = r.Metrics
		d.Quotas = Quotas{MaxConcurrentSessions: 1}
	})
	id := f.create(repoReq("beta"))
	f.events(id)
	if got := r.Count("spov_session_launches", "kind", "interactive", "outcome", "succeeded", "failed_step", "none"); got != 1 {
		t.Errorf("launches recorded = %d, want 1 interactive success", got)
	}
	if got := r.Count("spov_session_launch_duration", "outcome", "succeeded"); got != 1 {
		t.Errorf("launch durations recorded = %d, want 1", got)
	}

	f.do(http.MethodGet, "/api/v1/sessions/"+id, nil)
	f.do(http.MethodGet, "/api/v1/sessions/no-such-session", nil)
	if got := r.Count("spov_api_requests", "operation", "get-session", "status_class", "2xx"); got != 1 {
		t.Errorf("2xx get-session requests = %d, want 1", got)
	}
	if got := r.Count("spov_api_requests", "operation", "get-session", "status_class", "4xx"); got != 1 {
		t.Errorf("4xx get-session requests = %d, want 1", got)
	}
	if got := r.Count("spov_api_request_duration", "operation", "get-session"); got != 2 {
		t.Errorf("get-session durations = %d, want 2", got)
	}

	if resp, body := f.do(http.MethodPost, "/api/v1/sessions", repoReq("gamma")); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("a second session over a quota of one = %d %s", resp.StatusCode, body)
	}
	if got := r.Count("spov_quota_refusals", "limit", "sessions"); got != 1 {
		t.Errorf("session quota refusals = %d, want 1", got)
	}

	if resp, body := f.do(http.MethodDelete, "/api/v1/sessions/"+id, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("stop: %d %s", resp.StatusCode, body)
	}
	if got := r.Count("spov_session_terminations", "reason", "stopped"); got != 1 {
		t.Errorf("stops recorded = %d, want 1", got)
	}
	if got := r.Count("spov_session_lifetime", "kind", "interactive"); got != 1 {
		t.Errorf("lifetimes recorded = %d, want 1", got)
	}
}

// A stream is counted, but its duration is not recorded as latency.
//
// @decision(DL-011)
func TestAStreamIsCountedButNotTimed(t *testing.T) {
	r := telemetrytest.New(t)
	f := newAPIFixtureWith(t, func(d *Deps) { d.Metrics = r.Metrics })
	id := f.create(repoReq("beta"))
	f.events(id)
	if got := r.Count("spov_api_requests", "operation", "session-events"); got != 1 {
		t.Errorf("event streams counted = %d, want 1", got)
	}
	if got := r.Count("spov_api_request_duration", "operation", "session-events"); got != 0 {
		t.Errorf("event streams timed = %d, want 0", got)
	}
}

// A cancel is counted as an ending once, by the call that wrote it.
//
// @decision(DL-011)
func TestACancelIsCountedOnce(t *testing.T) {
	r := telemetrytest.New(t)
	f, _ := jobFixtureWith(t, func(d *Deps) { d.Metrics = r.Metrics })
	resp, body := f.do(http.MethodPost, "/api/v1/jobs", jobRequest())
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	var job JobView
	if err := json.Unmarshal(body, &job); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		f.do(http.MethodDelete, "/api/v1/jobs/"+job.ID, nil)
	}
	if got := r.Count("spov_job_endings", "outcome", "cancelled", "ended_by", "cancelled", "kind", "pr"); got != 1 {
		t.Errorf("cancelled endings = %d, want 1 for two cancels", got)
	}
}

// The snapshot counts unfinished jobs and live sessions from the store,
// tells a job's session apart by its job, and is withdrawn when the
// replica stops leading.
//
// @decision(DL-011)
func TestTheSnapshotIsSharedStateWhileLeading(t *testing.T) {
	r := telemetrytest.New(t)
	f, _ := jobFixtureWith(t, func(d *Deps) { d.Metrics = r.Metrics })
	if resp, body := f.do(http.MethodPost, "/api/v1/jobs", jobRequest()); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	snap, err := f.srv.svc.metricsSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Jobs[store.JobQueued] != 1 {
		t.Errorf("queued jobs = %d, want 1", snap.Jobs[store.JobQueued])
	}
	if n := snap.Sessions[telemetry.SessionKey{State: store.StateStarting, Kind: telemetry.KindJob}]; n != 1 {
		t.Errorf("starting job sessions = %d, want 1", n)
	}
	if snap.OldestQueued < 0 {
		t.Errorf("the oldest queued wait is negative: %s", snap.OldestQueued)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); f.srv.svc.RunMetricsSnapshot(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for r.Count("spov_jobs", "state", "queued") != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the leader never published its snapshot")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if got := r.Count("spov_jobs"); got != 0 {
		t.Errorf("after leading ended, spov_jobs still reports %d", got)
	}
}

// A sandbox that ended on its own is counted as ended, not stopped.
//
// @decision(DL-011)
func TestTheReconcilerCountsASandboxThatEndedOnItsOwn(t *testing.T) {
	r := telemetrytest.New(t)
	f := newAPIFixtureWith(t, func(d *Deps) { d.Metrics = r.Metrics })
	id := f.create(repoReq("beta"))
	f.events(id)
	sess, err := f.store.Sessions().Get(context.Background(), auth.LocalID, id)
	if err != nil {
		t.Fatal(err)
	}
	h, err := f.provider.Get(context.Background(), sess.SandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Terminate(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec := &Reconciler{Store: f.store, Provider: f.provider, Metrics: r.Metrics}
	if err := rec.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := r.Count("spov_session_terminations", "reason", "ended"); got != 1 {
		t.Errorf("ended sessions = %d, want 1", got)
	}
}
