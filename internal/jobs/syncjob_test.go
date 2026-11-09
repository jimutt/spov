package jobs

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/jobs/jobstest"
	"github.com/jimutt/spov/internal/store"
)

// syncFixture is a delivering sync job whose seams record the order
// delivery calls them in.
type syncFixture struct {
	*followFixture
	calls []string
	state SyncState
	err   error
}

func newSyncFixture(t *testing.T) *syncFixture {
	t.Helper()
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJobWith(t, st, store.JobDelivering, func(j *store.Job) { j.Kind = store.JobKindSync })
	got, err := st.Jobs().Get(ctx, job.OrgID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	f := &followFixture{sup: &Supervisor{Store: st, Warn: func(string) {}}, st: st, job: got}
	s := &syncFixture{followFixture: f, state: SyncState{Branch: "main", HeadCommit: strings.Repeat("b", 40), Commits: 2, Bytes: 2048}}
	f.sup.EndRun = func(context.Context, store.Job) error {
		s.calls = append(s.calls, "end-run")
		return nil
	}
	f.sup.InspectSync = func(context.Context, store.Job) (SyncState, error) {
		s.calls = append(s.calls, "inspect")
		return s.state, s.err
	}
	f.sup.Terminate = func(context.Context, store.Job) error {
		s.calls = append(s.calls, "terminate")
		return nil
	}
	f.sup.Mark = func(_ context.Context, _ store.Job, name, text string) error {
		f.putSpovMarker(t, name, text)
		return nil
	}
	return s
}

// Delivery stops a sync job's run before it reads the repository, and
// leaves the sandbox running once the job has finished, whichever way
// it went.
//
// @decision(DL-101)
func TestASyncJobStopsItsRunBeforeItIsJudged(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{{"read", nil}, {"unreadable", errors.New("exec failed")}} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSyncFixture(t)
			s.err = tc.err
			ctx := context.Background()
			if err := s.sup.deliver(ctx, s.job); err != nil {
				t.Fatal(err)
			}
			if strings.Join(s.calls, ",") != "end-run,inspect" {
				t.Errorf("delivery called %v, want end-run then inspect and no terminate", s.calls)
			}
			got, err := s.st.Jobs().Get(ctx, s.job.OrgID, s.job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !got.State.Terminal() {
				t.Errorf("state = %s, want a finished job", got.State)
			}
			// A redelivery reads nothing again.
			s.calls = nil
			if err := s.sup.collectSync(ctx, got); err != nil {
				t.Fatal(err)
			}
			if tc.err == nil && len(s.calls) != 0 {
				t.Errorf("a second collection called %v", s.calls)
			}
		})
	}
}

// Gate 2 of a sync job reads its one measured member.
//
// @decision(DL-101) @decision(DL-050)
func TestASyncJobIsJudgedOnItsRepository(t *testing.T) {
	base := store.Deliverable{Key: store.SyncDeliverableKey, Kind: store.DeliverableSync}
	bounded := []store.Event{{Source: store.SourceSpov, Name: MarkerBounded, Text: "the 1h0m0s wall clock ran out"}}
	broke := []store.Event{{Source: store.SourceSpov, Name: MarkerCollectFailed, Text: "the sandbox had gone"}}
	for _, tc := range []struct {
		name       string
		state      *SyncState
		unfinished string
		events     []store.Event
		want       GateOutcome
		says       string
	}{
		{name: "commits", state: &SyncState{Branch: "main", Commits: 2, Bytes: 4096}, want: GatePassed, says: "2 commits on main"},
		{name: "files only", state: &SyncState{Files: 3, Bytes: 100}, want: GatePassed, says: "3 uncommitted changed files"},
		{name: "unchanged", state: &SyncState{}, want: GateFailed, says: "unchanged since the job started"},
		{name: "rewritten", state: &SyncState{Rewritten: true, Bytes: 10}, want: GatePassed, says: "rewritten past the base"},
		{name: "over the bound", state: &SyncState{Commits: 1, Bytes: git.MaxSyncExportBytes + 1}, want: GateFailed, says: "over the 32 MiB a sync can take"},
		{name: "unreadable", events: broke, want: GateUnverified, says: "could not be read: the sandbox had gone"},
		{name: "at the wall clock", state: &SyncState{Commits: 1}, events: bounded, want: GatePartial, says: "wall clock"},
		{name: "unfinished", state: &SyncState{Commits: 1}, unfinished: "the docs", want: GatePartial, says: "left unfinished: the docs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var members []store.Deliverable
			if tc.state != nil {
				members = []store.Deliverable{syncMember(base, *tc.state)}
			}
			job := store.Job{Kind: store.JobKindSync, Unfinished: tc.unfinished}
			got := judgeDeliverable(job, tc.events, members)
			if got.Outcome != tc.want || !strings.Contains(got.Detail, tc.says) {
				t.Errorf("gate 2 = %s %q, want %s saying %q", got.Outcome, got.Detail, tc.want, tc.says)
			}
		})
	}
}

// A sync member says where it is, how much changed and its size.
//
// @decision(DL-101)
func TestASyncMemberCarriesItsMeasurements(t *testing.T) {
	m := syncMember(store.Deliverable{}, SyncState{Branch: "feat", HeadCommit: "abc", Commits: 1, Files: 2, Bytes: 3 << 20})
	if m.Status != store.DeliverableVerified || m.Branch != "feat" || m.HeadCommit != "abc" ||
		m.Commits != 1 || m.Files != 2 || m.Size != 3<<20 || !strings.Contains(m.Detail, "3.0 MiB") {
		t.Errorf("member = %+v", m)
	}
}
