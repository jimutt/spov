package server

import (
	"testing"
	"time"

	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/store"
)

// TestASessionCarriesItsOwnEstimatedCost pins that a session's view
// carries its cost over its whole life, from the same helper /me/usage
// uses, so the two differ in period but never in model.
//
// @decision(DL-055) @decision(DL-009)
func TestASessionCarriesItsOwnEstimatedCost(t *testing.T) {
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	now := start.Add(2 * time.Hour)
	sess := store.Session{
		ID: "s1", CreatedAt: start, UpdatedAt: start,
		Request: store.SessionRequest{Sandbox: store.SandboxConfig{CPU: 2, MemoryMiB: 4096}},
	}

	got := viewOf(sess, now).EstimatedCost
	want := round2(sandbox.EstimatedCost(2, 4096, 2*time.Hour))
	if got != want {
		t.Errorf("running session cost = %v, want %v", got, want)
	}
	if got <= 0 {
		t.Fatalf("a two-hour session cost %v", got)
	}

	// A stopped session stops accruing: the clock moving on must not
	// keep charging for a sandbox that is gone.
	stopped := sess
	at := start.Add(30 * time.Minute)
	stopped.StoppedAt = &at
	frozen := viewOf(stopped, now).EstimatedCost
	if frozen != round2(sandbox.EstimatedCost(2, 4096, 30*time.Minute)) {
		t.Errorf("stopped session cost = %v, want the half-hour figure", frozen)
	}
	if later := viewOf(stopped, now.Add(10*time.Hour)).EstimatedCost; later != frozen {
		t.Errorf("a stopped session kept accruing: %v then %v", frozen, later)
	}

	// A session with no creation time cannot be aged, and must not bill
	// from the zero time.
	broken := sess
	broken.CreatedAt = time.Time{}
	if c := viewOf(broken, now).EstimatedCost; c != 0 {
		t.Errorf("a session with no creation time cost %v", c)
	}
}
