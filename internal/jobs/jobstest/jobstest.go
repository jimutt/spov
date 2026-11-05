// Package jobstest holds test helpers that both internal/jobs and
// internal/server need. PathTo lives here so both sides share one
// encoding of internal/jobs' state machine. Like internal/store's
// storetest, nothing outside a _test.go file may import it.
//
// @decision(DL-046)
package jobstest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/store"
)

// PathTo is a legal route from queued to want.
func PathTo(want store.JobState) []store.JobState {
	switch want {
	case store.JobQueued:
		return nil
	case store.JobProvisioning:
		return []store.JobState{store.JobProvisioning}
	case store.JobRunning, store.JobBlocked, store.JobSteered, store.JobDelivering:
		base := []store.JobState{store.JobProvisioning, store.JobRunning}
		if want == store.JobRunning {
			return base
		}
		return append(base, want)
	case store.JobSucceeded:
		return []store.JobState{
			store.JobProvisioning, store.JobRunning, store.JobDelivering, store.JobSucceeded,
		}
	case store.JobFailed, store.JobCancelled:
		return []store.JobState{store.JobProvisioning, want}
	}
	return nil
}

// SeedJob stores a job in the given state, walking it there through
// legal transitions so the fixture cannot assert a state the machine
// forbids.
func SeedJob(t *testing.T, st store.Store, want store.JobState) store.Job {
	t.Helper()
	return SeedJobWith(t, st, want, nil)
}

// SeedJobWith is SeedJob with a chance to set the job's create-time
// fields, such as its wall clock, before it is stored.
func SeedJobWith(t *testing.T, st store.Store, want store.JobState, set func(*store.Job)) store.Job {
	t.Helper()
	ctx := context.Background()
	sessID := store.NewID(time.Now())
	sess := store.Session{
		ID: sessID, OrgID: "org_1", OwnerID: "user_1",
		// Named after the session, not a fixed literal: session names
		// are unique among live ones, so a test seeding two jobs would
		// otherwise get a name conflict rather than two jobs.
		Name: "s-" + strings.ToLower(sessID), State: store.StateRunning,
	}
	if err := st.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	job := store.Job{
		ID: store.NewID(time.Now()), OrgID: sess.OrgID, OwnerID: sess.OwnerID,
		SessionID: sess.ID, Kind: store.JobKindReport,
		State: store.JobQueued, Goal: "do the thing",
	}
	if set != nil {
		set(&job)
	}
	if err := st.Jobs().Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	for _, step := range PathTo(want) {
		job.State = step
		if err := st.Jobs().Update(ctx, job); err != nil {
			t.Fatalf("seeding %v via %v: %v", want, step, err)
		}
	}
	return job
}
