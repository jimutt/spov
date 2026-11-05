package jobs

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/jobs/jobstest"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/telemetry"
	"github.com/jimutt/spov/internal/telemetry/telemetrytest"
)

// A judged ending is counted once, with what ended it, its gates and
// how long delivery took; a redelivery after the verdict counts nothing.
//
// @decision(DL-011)
func TestAJudgedEndingIsCountedOnce(t *testing.T) {
	r := telemetrytest.New(t)
	c := newClockFixture(t, store.JobRunning, time.Hour)
	c.sup.Metrics = r.Metrics
	c.at(time.Hour + time.Minute)
	// The store banks the clock by its own time; it must be the
	// supervisor's.
	c.st.(*store.Memory).Now = c.sup.Now
	ctx := context.Background()
	if err := c.sup.followOnce(ctx, c.job); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := c.sup.deliver(ctx, c.state(t)); err != nil {
			t.Fatal(err)
		}
	}
	if got := r.Count("spov_job_endings", "ended_by", "wall-clock", "kind", "report"); got != 1 {
		t.Errorf("wall-clock endings = %d, want 1 across two deliveries", got)
	}
	if got := r.Count("spov_job_gates"); got != 3 {
		t.Errorf("gate results = %d, want the three gates once", got)
	}
	if got := r.Count("spov_job_delivery_duration"); got != 1 {
		t.Errorf("delivery durations = %d, want 1", got)
	}
	if got := r.Count("spov_job_clock_used"); got != 1 {
		t.Errorf("clock used recorded = %d, want 1", got)
	}
}

// A launch that fails ends the job as launch-failed, after one counted
// pass and one queue wait.
//
// @decision(DL-011)
func TestAFailedLaunchIsCountedAsLaunchFailed(t *testing.T) {
	r := telemetrytest.New(t)
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobQueued)
	sup := &Supervisor{
		Store: st, Warn: func(string) {}, Metrics: r.Metrics,
		Provision: func(context.Context, store.Job) error { return errors.New("modal said no") },
	}
	if err := sup.Run(context.Background(), JobArgs{OrgID: job.OrgID, JobID: job.ID}, 1); err != nil {
		t.Fatal(err)
	}
	if got := r.Count("spov_job_endings", "outcome", "failed", "ended_by", "launch-failed"); got != 1 {
		t.Errorf("launch-failed endings = %d, want 1", got)
	}
	if got := r.Count("spov_supervisor_passes", "outcome", "ok"); got != 1 {
		t.Errorf("passes = %d, want 1", got)
	}
	if got := r.Count("spov_job_queue_delay"); got != 1 {
		t.Errorf("queue waits = %d, want 1", got)
	}
}

// What ended a run is read from spov's wall-clock marker first, then the
// loop's own announcement; a run with neither was abandoned.
//
// @decision(DL-011)
func TestEndedByReadsTheMarkers(t *testing.T) {
	stop := func(reason string) store.Event {
		return stopFrame(t, store.SourceOpenCode, map[string]any{"reason": reason})
	}
	bound := store.Event{Source: store.SourceSpov, Name: MarkerBounded, Text: "the clock ran out"}
	for _, tc := range []struct {
		name   string
		events []store.Event
		want   telemetry.EndedBy
	}{
		{"declared", []store.Event{stop(opencode.CompletionReason)}, telemetry.EndedDeclared},
		{"denied", []store.Event{stop(opencode.DeniedReason)}, telemetry.EndedDenied},
		{"stopped", []store.Event{stop("stopped")}, telemetry.EndedStopped},
		{"no completion tool", []store.Event{stop("no-completion-tool")}, telemetry.EndedNoCompletionTool},
		{"the last stop wins", []store.Event{stop("stopped"), stop(opencode.CompletionReason)}, telemetry.EndedDeclared},
		{"the clock outranks the stop it caused", []store.Event{bound, stop("wall-clock")}, telemetry.EndedWallClock},
		{"nothing announced", nil, telemetry.EndedAbandoned},
	} {
		if got := endedBy(tc.events); got != tc.want {
			t.Errorf("%s: endedBy = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Every reason the plugin can stop its loop with maps to an ended_by of
// its own. The reasons are literals in the plugin's source, so a new one
// would otherwise be counted as other without any test noticing.
//
// @decision(DL-011) @decision(DL-049)
func TestEveryStopReasonThePluginSendsHasALabel(t *testing.T) {
	src, err := opencode.Plugin{Kind: "report", Goal: "g"}.Source()
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{
		"stopped":                 true, // loop.stopReason ?? "stopped"
		opencode.DeniedReason:     true, // CONFIG.deniedReason
		opencode.CompletionReason: true,
	}
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`announceStop\(ctx, sessionID, "([a-z-]+)"\)`),
		regexp.MustCompile(`loop\.stopReason = "([a-z-]+)"`),
	} {
		for _, m := range re.FindAllStringSubmatch(src, -1) {
			found[m[1]] = true
		}
	}
	if len(found) < 6 {
		t.Fatalf("found only %v in the plugin; the patterns no longer match its source", found)
	}
	for reason := range found {
		got := endedBy([]store.Event{stopFrame(t, store.SourceOpenCode, map[string]any{"reason": reason})})
		r := telemetrytest.New(t)
		r.Metrics.JobEnded(context.Background(), store.JobKindPR, store.JobFailed, got, 0)
		if r.Count("spov_job_endings", "ended_by", "other") != 0 {
			t.Errorf("the plugin's stop reason %q is recorded as ended_by other", reason)
		}
	}
}

// Every gate and outcome the verdict produces is a label value of its
// own, not other.
//
// @decision(DL-011) @decision(DL-050)
func TestEveryGateHasALabel(t *testing.T) {
	r := telemetrytest.New(t)
	for _, gate := range []string{GateMechanical, GateDeliverable, GateClassification} {
		for _, outcome := range []GateOutcome{GatePassed, GatePartial, GateFailed, GateUnverified} {
			r.Metrics.JobGate(context.Background(), gate, string(outcome))
		}
	}
	if n := r.Count("spov_job_gates", "gate", "other") + r.Count("spov_job_gates", "result", "other"); n != 0 {
		t.Errorf("%d gate results were recorded as other", n)
	}
	if n := r.Count("spov_job_gates"); n != 12 {
		t.Errorf("recorded %d gate results, want 12", n)
	}
	for _, kind := range []store.DeliverableKind{store.DeliverablePR, store.DeliverableReport} {
		for _, st := range store.DeliverableStatuses {
			r.Metrics.JobDeliverable(context.Background(), kind, st)
		}
	}
	if n := r.Count("spov_job_deliverables", "kind", "other") + r.Count("spov_job_deliverables", "status", "other"); n != 0 {
		t.Errorf("%d deliverables were recorded as other", n)
	}
}
