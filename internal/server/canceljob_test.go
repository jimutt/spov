package server

import (
	"context"
	"testing"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/store"
)

// CancelJob's transaction, tested through the window a concurrent write
// actually lands in: UpdateJob writes the whole row, so a copy read
// before another writer landed would put the old values back, and
// CancelJob re-reads inside its transaction for that reason. The
// concurrent writer is a direct store write rather than the
// supervisor's preparation pass, which would need exporting its
// internals to reach from this side of the DL-046 boundary.
//
// @decision(DL-047) @decision(DL-045) @decision(DL-046)

// hookedStore runs a function the first time a job is read, which is
// how a test gets INTO the window between CancelJob's read and its
// write. Without that the two happen inside one call and no test can
// interleave anything.
type hookedStore struct {
	store.Store
	once func()
}

type hookedJobs struct {
	store.JobStore
	once *func()
}

func (h hookedJobs) Get(ctx context.Context, org, id string) (store.Job, error) {
	j, err := h.JobStore.Get(ctx, org, id)
	if f := *h.once; f != nil {
		*h.once = nil
		f()
	}
	return j, err
}

func (h *hookedStore) Jobs() store.JobStore { return hookedJobs{h.Store.Jobs(), &h.once} }

func TestACancelDoesNotUndoAWriteThatLandedFirst(t *testing.T) {
	f, _ := jobFixture(t)
	ctx := context.Background()
	svc := f.srv.svc
	p := auth.Principal{UserID: "u-alice", OrgID: auth.LocalID}
	created, err := svc.CreateJob(ctx, p, jobRequest())
	if err != nil {
		t.Fatal(err)
	}
	job, err := f.store.Jobs().Get(ctx, auth.LocalID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	job.State = store.JobProvisioning
	if err := f.store.Jobs().Update(ctx, job); err != nil {
		t.Fatal(err)
	}

	// The write lands after CancelJob has read the row and before it
	// writes — the only place the race exists.
	hooked := &hookedStore{Store: f.store}
	hooked.once = func() {
		landed, err := f.store.Jobs().Get(ctx, auth.LocalID, job.ID)
		if err != nil {
			t.Error(err)
			return
		}
		landed.Assessment = "S"
		landed.CheckProposed = true
		if err := f.store.Jobs().Update(ctx, landed); err != nil {
			t.Error(err)
		}
	}
	svc.deps.Store = hooked
	if err := svc.CancelJob(ctx, p, job.ID); err != nil {
		t.Fatalf("CancelJob: %v", err)
	}

	after, err := f.store.Jobs().Get(ctx, auth.LocalID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != store.JobCancelled {
		t.Errorf("State = %s, want cancelled", after.State)
	}
	if after.Assessment != "S" || !after.CheckProposed {
		t.Errorf("the cancel undid the write: assessment=%q proposed=%v",
			after.Assessment, after.CheckProposed)
	}
}
