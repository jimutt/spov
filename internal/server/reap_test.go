package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/store"
)

// The backstop terminates a sandbox nothing is tracking. It is the only
// thing standing between a lost sandbox and Modal's own 12 h cap.
//
// @decision(DL-004)
func TestReaperTerminatesAnOverAgeSandbox(t *testing.T) {
	provider := sandbox.NewFake()
	st := store.NewMemory()
	now := time.Now()

	// A sandbox the store knows nothing about, older than any session
	// should be.
	provider.Now = func() time.Time { return now.Add(-20 * time.Hour) }
	old, err := provider.Create(context.Background(), sandbox.Spec{
		Image: "img", Command: []string{"true"}, CPU: 0.5, MemoryMiB: 1024,
		Timeout: time.Hour, IdleTimeout: 10 * time.Minute, Egress: sandbox.EgressOpen(),
		Tags: sandbox.BuildTags(sandbox.TagSet{Name: "ancient", CreatedAt: now.Add(-20 * time.Hour)}),
	})
	if err != nil {
		t.Fatal(err)
	}
	provider.Now = func() time.Time { return now }

	r := &Reconciler{
		Store: st, Provider: provider, Now: func() time.Time { return now },
		Warn: func(s string) { t.Log(s) }, MaxSessionAge: 8 * time.Hour,
	}
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}

	snap, err := old.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.State != sandbox.StateStopped {
		t.Errorf("the over-age sandbox is %s, want stopped", snap.State)
	}
	// It must not also have been adopted: reaping and adopting the same
	// sandbox would leave a running session pointing at a dead one.
	sessions, err := st.Sessions().ListLive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Errorf("a reaped sandbox was adopted: %+v", sessions)
	}
}

// A sandbox within the age limit is left alone, and adopted as usual.
func TestReaperLeavesAYoungSandbox(t *testing.T) {
	provider := sandbox.NewFake()
	st := store.NewMemory()
	now := time.Now()

	provider.Now = func() time.Time { return now.Add(-time.Hour) }
	if _, err := provider.Create(context.Background(), sandbox.Spec{
		Image: "img", Command: []string{"true"}, CPU: 0.5, MemoryMiB: 1024,
		Timeout: time.Hour, IdleTimeout: 10 * time.Minute, Egress: sandbox.EgressOpen(),
		Tags: sandbox.BuildTags(sandbox.TagSet{Name: "fresh", CreatedAt: now.Add(-time.Hour)}),
	}); err != nil {
		t.Fatal(err)
	}
	provider.Now = func() time.Time { return now }

	r := &Reconciler{
		Store: st, Provider: provider, Now: func() time.Time { return now },
		Warn: func(s string) { t.Log(s) }, MaxSessionAge: 8 * time.Hour,
		DefaultOrg: auth.LocalID, DefaultOwner: auth.LocalID,
	}
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	sessions, err := st.Sessions().ListLive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("a young sandbox was not adopted: %d sessions", len(sessions))
	}
}

// Without a configured age the backstop does nothing.
func TestReaperIsOffWithoutAMaxAge(t *testing.T) {
	provider := sandbox.NewFake()
	st := store.NewMemory()
	now := time.Now()
	provider.Now = func() time.Time { return now.Add(-100 * time.Hour) }
	h, err := provider.Create(context.Background(), sandbox.Spec{
		Image: "img", Command: []string{"true"}, CPU: 0.5, MemoryMiB: 1024,
		Timeout: time.Hour, IdleTimeout: 10 * time.Minute, Egress: sandbox.EgressOpen(),
		Tags: sandbox.BuildTags(sandbox.TagSet{Name: "ancient", CreatedAt: now.Add(-100 * time.Hour)}),
	})
	if err != nil {
		t.Fatal(err)
	}
	provider.Now = func() time.Time { return now }

	r := &Reconciler{
		Store: st, Provider: provider, Now: func() time.Time { return now },
		Warn: func(s string) { t.Log(s) }, DefaultOrg: auth.LocalID, DefaultOwner: auth.LocalID,
	}
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap, _ := h.Info(context.Background())
	if snap.State == sandbox.StateStopped {
		t.Error("the backstop ran with no maximum age configured")
	}
}

// A sandbox whose creation time the provider does not report cannot be
// aged, so it is left alone rather than reaped on a guess.
func TestReaperSkipsASandboxWithNoCreationTime(t *testing.T) {
	provider := sandbox.NewFake()
	st := store.NewMemory()
	now := time.Now()
	r := &Reconciler{
		Store: st, Provider: provider, Now: func() time.Time { return now },
		Warn: func(s string) { t.Log(s) }, MaxSessionAge: time.Hour,
	}
	if reaped := r.reap(context.Background(), sandbox.Info{ID: "sb-x"}, now); reaped {
		t.Error("a sandbox with no creation time was reaped")
	}
}

// A sandbox the provider has already forgotten is not an error: it is
// gone, which is the outcome the reaper wanted.
func TestReaperToleratesAVanishedSandbox(t *testing.T) {
	provider := sandbox.NewFake()
	st := store.NewMemory()
	now := time.Now()
	r := &Reconciler{
		Store: st, Provider: provider, Now: func() time.Time { return now },
		Warn: func(s string) { t.Log(s) }, MaxSessionAge: time.Hour,
	}
	info := sandbox.Info{ID: "sb-gone", CreatedAt: now.Add(-100 * time.Hour)}
	if reaped := r.reap(context.Background(), info, now); reaped {
		t.Error("reap claimed to terminate a sandbox that does not exist")
	}
}

// Reaping a sandbox a session still claims marks that session stopped,
// so a listing does not keep showing a session whose sandbox is gone.
func TestReaperStopsTheSessionThatClaimedIt(t *testing.T) {
	provider := sandbox.NewFake()
	st := store.NewMemory()
	now := time.Now()

	provider.Now = func() time.Time { return now.Add(-20 * time.Hour) }
	h, err := provider.Create(context.Background(), sandbox.Spec{
		Image: "img", Command: []string{"true"}, CPU: 0.5, MemoryMiB: 1024,
		Timeout: time.Hour, IdleTimeout: 10 * time.Minute, Egress: sandbox.EgressOpen(),
		Tags: sandbox.BuildTags(sandbox.TagSet{Name: "stale", CreatedAt: now.Add(-20 * time.Hour)}),
	})
	if err != nil {
		t.Fatal(err)
	}
	provider.Now = func() time.Time { return now }
	snap, err := h.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	sess := store.Session{
		ID: store.NewID(now), OrgID: auth.LocalID, OwnerID: auth.LocalID,
		Name: "stale", State: store.StateRunning, SandboxID: snap.ID,
		CreatedAt: now.Add(-20 * time.Hour),
	}
	if err := st.Sessions().Create(context.Background(), sess); err != nil {
		t.Fatal(err)
	}

	var forgotten string
	r := &Reconciler{
		Store: st, Provider: provider, Now: func() time.Time { return now },
		Warn: func(s string) { t.Log(s) }, MaxSessionAge: 8 * time.Hour,
		Forget: func(id string) { forgotten = id },
	}
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := st.Sessions().Get(context.Background(), auth.LocalID, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.StateStopped {
		t.Errorf("the session is %s, want stopped", got.State)
	}
	if !strings.Contains(got.Error, "reaped") {
		t.Errorf("error = %q, want it to say the sandbox was reaped", got.Error)
	}
	if forgotten != sess.ID {
		t.Errorf("the reaped session was not forgotten: %q", forgotten)
	}
}

// An adopted sandbox goes back to the user who launched it, so it
// counts against their quota rather than the admin's.
//
// @decision(DL-004)
func TestAdoptUsesTheTaggedOwner(t *testing.T) {
	provider := sandbox.NewFake()
	st := store.NewMemory()
	now := time.Now()
	const owner = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	// A hosted login puts every user in this org (DL-007); it is not a
	// ULID, which is exactly what makes reading the tag non-obvious.
	const org = DefaultOrgID

	if _, err := provider.Create(context.Background(), sandbox.Spec{
		Image: "img", Command: []string{"true"}, CPU: 0.5, MemoryMiB: 1024,
		Timeout: time.Hour, IdleTimeout: 10 * time.Minute, Egress: sandbox.EgressOpen(),
		Tags: sandbox.BuildTags(sandbox.TagSet{Name: "theirs", User: owner, Org: org, CreatedAt: now}),
	}); err != nil {
		t.Fatal(err)
	}

	r := &Reconciler{
		Store: st, Provider: provider, Now: func() time.Time { return now },
		Warn: func(s string) { t.Log(s) }, DefaultOrg: auth.LocalID, DefaultOwner: auth.LocalID,
	}
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	sessions, err := st.Sessions().ListLive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions", len(sessions))
	}
	if sessions[0].OwnerID != owner {
		t.Errorf("owner = %q, want the tagged user", sessions[0].OwnerID)
	}
	if sessions[0].OrgID != org {
		t.Errorf("org = %q, want the tagged %q", sessions[0].OrgID, org)
	}
}

// An org tag naming an org this server does not serve is ignored: the
// session's foreign key would fail on insert, turning a stale tag into
// a lost sandbox. The owner is still read, since it is valid.
//
// @decision(DL-004)
func TestAdoptIgnoresAnUnknownOrgTag(t *testing.T) {
	provider := sandbox.NewFake()
	st := store.NewMemory()
	now := time.Now()
	const owner = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

	if _, err := provider.Create(context.Background(), sandbox.Spec{
		Image: "img", Command: []string{"true"}, CPU: 0.5, MemoryMiB: 1024,
		Timeout: time.Hour, IdleTimeout: 10 * time.Minute, Egress: sandbox.EgressOpen(),
		Tags: sandbox.BuildTags(sandbox.TagSet{
			Name: "elsewhere", User: owner, Org: "some-other-deployment", CreatedAt: now}),
	}); err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{
		Store: st, Provider: provider, Now: func() time.Time { return now },
		Warn: func(s string) { t.Log(s) }, DefaultOrg: auth.LocalID, DefaultOwner: auth.LocalID,
	}
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	sessions, _ := st.Sessions().ListLive(context.Background())
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions", len(sessions))
	}
	if sessions[0].OrgID != auth.LocalID {
		t.Errorf("org = %q, want the reconciler default", sessions[0].OrgID)
	}
}

// A sandbox whose user tag is an OS username rather than a user ID is
// not adopted under it: that would attribute someone else's session
// and count against a quota that is not theirs.
//
// @decision(DL-004)
func TestAdoptIgnoresANonIDOwnerTag(t *testing.T) {
	provider := sandbox.NewFake()
	st := store.NewMemory()
	now := time.Now()

	if _, err := provider.Create(context.Background(), sandbox.Spec{
		Image: "img", Command: []string{"true"}, CPU: 0.5, MemoryMiB: 1024,
		Timeout: time.Hour, IdleTimeout: 10 * time.Minute, Egress: sandbox.EgressOpen(),
		Tags: sandbox.BuildTags(sandbox.TagSet{Name: "legacy", User: "alice", CreatedAt: now}),
	}); err != nil {
		t.Fatal(err)
	}

	r := &Reconciler{
		Store: st, Provider: provider, Now: func() time.Time { return now },
		Warn: func(s string) { t.Log(s) }, DefaultOrg: auth.LocalID, DefaultOwner: auth.LocalID,
	}
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	sessions, _ := st.Sessions().ListLive(context.Background())
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions", len(sessions))
	}
	if sessions[0].OwnerID != auth.LocalID {
		t.Errorf("owner = %q, want the reconciler default", sessions[0].OwnerID)
	}
}
