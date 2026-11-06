package server

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/session"
	"github.com/jimutt/spov/internal/store"
)

// withSnapshots turns exit snapshots on, as a hosted server under the
// proxy has them.
func withSnapshots(d *Deps) {
	d.Snapshots = &SnapshotSettings{Retention: DefaultSnapshotRetention, MaxPerUser: DefaultSnapshotsPerUser}
}

func (f *renewFixture) run() store.Run {
	f.t.Helper()
	r, err := f.store.Runs().Current(context.Background(), auth.LocalID, f.id)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

// A launch under the proxy with snapshots on asks for an exit snapshot
// and records run 1 pending; the session's stop ends the run.
//
// @decision(DL-084) @decision(DL-085)
func TestALaunchRecordsItsRunAndAsksForAnExitSnapshot(t *testing.T) {
	f := newRecoverFixtureWith(t, withSnapshots)
	run := f.run()
	if run.N != 1 || run.SandboxID != f.row().SandboxID || run.Origin != store.RunFromLaunch ||
		run.SnapshotState != store.SnapshotPending || !run.Ran() || !run.StartedAt.Equal(f.row().CreatedAt) {
		t.Fatalf("run 1 = %+v", run)
	}
	specs := f.provider.Specs()
	if !specs[len(specs)-1].ExitSnapshot || specs[len(specs)-1].Tags[sandbox.TagRun] != "1" {
		t.Errorf("the sandbox was not asked for an exit snapshot: %+v", specs[len(specs)-1])
	}
	if _, err := f.store.Sessions().MarkStopped(context.Background(), auth.LocalID, f.id, store.StateStopped, time.Now(), ""); err != nil {
		t.Fatal(err)
	}
	if f.run().EndedAt == nil {
		t.Error("the stop did not end the run")
	}
}

// Without the proxy, or with snapshots off, a sandbox gets none and its
// run says so.
//
// @decision(DL-084)
func TestNoExitSnapshotWithoutTheProxyOrWithThemOff(t *testing.T) {
	for name, f := range map[string]*renewFixture{
		"no proxy": func() *renewFixture {
			api := newAPIFixtureWith(t, withSnapshots)
			return &renewFixture{apiFixture: api, id: api.create(repoReq("plain"))}
		}(),
		"snapshots off": newRecoverFixture(t),
	} {
		f.events(f.id)
		if spec := f.provider.Specs()[0]; spec.ExitSnapshot {
			t.Errorf("%s: the sandbox was asked for an exit snapshot", name)
		}
		if run := f.run(); run.SnapshotState != store.SnapshotNone {
			t.Errorf("%s: run = %+v", name, run)
		}
	}
}

// Collection keeps an ended run's snapshot, settles one created without
// any as none, gives a missing one up as failed, and leaves a pending
// one until its time runs out.
//
// @decision(DL-085)
func TestCollectionSettlesEachEndedRun(t *testing.T) {
	f := newRecoverFixtureWith(t, withSnapshots)
	ctx := context.Background()
	svc := f.srv.svc
	h := f.handle()

	// Still running: nothing to collect.
	if kept := svc.CollectSnapshots(ctx); kept != 0 {
		t.Fatalf("collected %d from a live run", kept)
	}
	ended := time.Now()
	if _, err := f.store.Sessions().MarkStopped(ctx, auth.LocalID, f.id, store.StateStopped, ended, ""); err != nil {
		t.Fatal(err)
	}
	// Ended in the store, but the sandbox has not exited yet: pending.
	svc.CollectSnapshots(ctx)
	if run := f.run(); run.SnapshotState != store.SnapshotPending {
		t.Fatalf("a snapshot still being taken was settled: %+v", run)
	}
	h.End()
	if kept := svc.CollectSnapshots(ctx); kept != 1 {
		t.Fatalf("kept %d", kept)
	}
	if run := f.run(); run.SnapshotState != store.SnapshotKept || run.Snapshot != "im-snap-"+h.ID() || run.SnapshotAt == nil {
		t.Fatalf("run = %+v", run)
	}

	// One Modal says failed, and one never settles within the give-up.
	for name, c := range map[string]struct {
		err  error
		ago  time.Duration
		want store.SnapshotState
	}{
		"failed":      {sandbox.ErrSnapshotFailed, 0, store.SnapshotFailed},
		"gone":        {sandbox.ErrNotFound, 0, store.SnapshotFailed},
		"none":        {sandbox.ErrNoExitSnapshot, 0, store.SnapshotNone},
		"pending":     {sandbox.ErrSnapshotPending, time.Minute, store.SnapshotPending},
		"pending out": {sandbox.ErrSnapshotPending, SnapshotGiveUp + time.Minute, store.SnapshotFailed},
	} {
		run := f.run()
		end := time.Now().Add(-c.ago)
		run.SandboxID, run.SnapshotState, run.Snapshot, run.EndedAt = "sb-"+name, store.SnapshotPending, "", &end
		if err := f.store.Runs().Update(ctx, run); err != nil {
			t.Fatal(err)
		}
		f.provider.SnapshotErrs = map[string]error{"sb-" + name: c.err}
		svc.CollectSnapshots(ctx)
		if got := f.run().SnapshotState; got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
}

// A run that never reached running holds no work, so its snapshot is
// deleted as soon as it is kept.
//
// @decision(DL-085) @decision(DL-086)
func TestAnUnusedRunsSnapshotIsDeletedAtOnce(t *testing.T) {
	f := newRecoverFixtureWith(t, withSnapshots)
	ctx := context.Background()
	h := f.handle()
	run := f.run()
	run.RunningAt = nil
	if err := f.store.Runs().Update(ctx, run); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Sessions().MarkStopped(ctx, auth.LocalID, f.id, store.StateFailed, time.Now(), "x"); err != nil {
		t.Fatal(err)
	}
	h.End()
	if kept := f.srv.svc.CollectSnapshots(ctx); kept != 0 {
		t.Errorf("an unused snapshot counted as kept")
	}
	if run := f.run(); run.SnapshotState != store.SnapshotDeleted || run.Snapshot != "" {
		t.Errorf("run = %+v", run)
	}
	if got := f.provider.Deleted(); !slices.Equal(got, []string{"im-snap-" + h.ID()}) {
		t.Errorf("deleted %v", got)
	}
}

// Pruning goes by retention, then supersedes an earlier run's snapshot
// with a later one, then holds each owner to the cap, oldest first.
//
// @decision(DL-086)
func TestPruneReasons(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	ended := func(ago time.Duration) *time.Time { t := now.Add(-ago); return &t }
	run := func(sess, owner string, n int, ago time.Duration) store.Run {
		return store.Run{SessionID: sess, OwnerID: owner, N: n, EndedAt: ended(ago), SnapshotState: store.SnapshotKept, Snapshot: sess}
	}
	kept := []store.Run{
		run("old", "u1", 1, 8*24*time.Hour),
		run("twice", "u1", 1, 3*time.Hour),
		run("twice", "u1", 2, 2*time.Hour),
		run("a", "u2", 1, 5*time.Hour),
		run("b", "u2", 1, 4*time.Hour),
		run("c", "u2", 1, 1*time.Hour),
	}
	got := pruneReasons(kept, SnapshotSettings{Retention: 7 * 24 * time.Hour, MaxPerUser: 2}, now)
	names := func(rs []store.Run) []string {
		var out []string
		for _, r := range rs {
			out = append(out, r.SessionID)
		}
		return out
	}
	if r := names(got["retention"]); !slices.Equal(r, []string{"old"}) {
		t.Errorf("retention: %v", r)
	}
	if s := got["superseded"]; len(s) != 1 || s[0].SessionID != "twice" || s[0].N != 1 {
		t.Errorf("superseded: %+v", s)
	}
	if c := names(got["cap"]); !slices.Equal(c, []string{"a"}) {
		t.Errorf("cap: %v", c)
	}
	if none := pruneReasons(kept[3:], SnapshotSettings{Retention: 7 * 24 * time.Hour}, now); len(none["cap"]) != 0 {
		t.Errorf("no cap pruned %v", none["cap"])
	}
}

// A pruned snapshot is marked deleted before its image goes, and a
// delete that failed is retried on the next pass.
//
// @decision(DL-086)
func TestPruningMarksThenDeletesAndRetries(t *testing.T) {
	failing := &deleteFailing{Fake: sandbox.NewFake(), fail: true}
	f := newRecoverFixtureWith(t, func(d *Deps) {
		withSnapshots(d)
		d.Snapshots.Retention = MinSnapshotRetention
		d.Provider = failing
		d.Launch = NewLaunch(&session.Launcher{
			Sandboxes: failing, NewClient: func(string, string) session.HealthChecker { return stubHealth{} },
			HealthTimeout: 5 * time.Second, ProxyImage: d.ProxyImage,
		})
	})
	ctx := context.Background()
	run := f.run()
	end := time.Now().Add(-2 * MinSnapshotRetention)
	run.EndedAt, run.SnapshotState, run.Snapshot = &end, store.SnapshotKept, "im-old"
	if err := f.store.Runs().Update(ctx, run); err != nil {
		t.Fatal(err)
	}
	f.srv.svc.PruneSnapshots(ctx)
	if run := f.run(); run.SnapshotState != store.SnapshotDeleted || run.Snapshot != "im-old" {
		t.Fatalf("after a failed delete: %+v", run)
	}
	failing.fail = false
	f.srv.svc.PruneSnapshots(ctx)
	if run := f.run(); run.SnapshotState != store.SnapshotDeleted || run.Snapshot != "" {
		t.Fatalf("after the retry: %+v", run)
	}
}

type deleteFailing struct {
	*sandbox.Fake
	fail bool
}

func (d *deleteFailing) DeleteImage(ctx context.Context, id string) error {
	if d.fail {
		return errors.New("modal is down")
	}
	return d.Fake.DeleteImage(ctx, id)
}

// A resumed session is billed only for the time its sandboxes ran.
//
// @decision(DL-085) @decision(DL-009)
func TestUsageCountsOnlyTheRuns(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	created := now.Add(-48 * time.Hour)
	sess := store.Session{CreatedAt: created}
	firstEnd := created.Add(time.Hour)
	runs := []store.Run{
		{N: 1, StartedAt: created, EndedAt: &firstEnd},
		{N: 2, StartedAt: now.Add(-30 * time.Minute)},
	}
	if got := billableWithin(sess, runs, created, now); got != 90*time.Minute {
		t.Errorf("billed %s, want 1h30m", got)
	}
	if got := billableWithin(sess, nil, created, now); got != 48*time.Hour {
		t.Errorf("a session with no runs: %s", got)
	}
}

// The settings DL-086 rules out are refused.
//
// @decision(DL-086)
func TestSnapshotSettingsAreBounded(t *testing.T) {
	for _, c := range []SnapshotSettings{
		{Retention: 30 * time.Minute}, {Retention: 721 * time.Hour}, {Retention: time.Hour, MaxPerUser: -1},
	} {
		if err := c.Validate(); err == nil {
			t.Errorf("%+v was accepted", c)
		}
	}
	if err := (SnapshotSettings{Retention: DefaultSnapshotRetention, MaxPerUser: 0}).Validate(); err != nil {
		t.Error(err)
	}
}
