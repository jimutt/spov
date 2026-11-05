package server

import (
	"context"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/session"
	"github.com/jimutt/spov/internal/store"
)

// TestDeleteFromAnotherReplicaDoesNotResurrect is the cross-process
// half of DL-003's "a launch is cancellable only while it has not
// recorded a sandbox". Two Services over one shared store stand in for
// two replicas; the DELETE lands on the one that is not launching, in
// the window before a sandbox is recorded.
//
// @decision(DL-003)
func TestDeleteFromAnotherReplicaDoesNotResurrect(t *testing.T) {
	shared := store.NewMemory()
	provider := sandbox.NewFake()
	release := make(chan struct{})
	launched := make(chan struct{})

	forge := &fakeForge{platform: git.NewFake()}
	forge.platform.Repos = []git.Repo{{Owner: "acme", Name: "app", CloneURL: "https://git.test/acme/app.git"}}

	launcher := &session.Launcher{
		Sandboxes:     provider,
		NewClient:     func(string, string) session.HealthChecker { return stubHealth{} },
		HealthTimeout: 5 * time.Second,
	}
	realLaunch := NewLaunch(launcher)

	base := func(hold bool) Deps {
		return Deps{
			Store: shared, Provider: provider, Forge: forge,
			Warn: func(s string) { t.Log(s) },
			Launch: func(ctx context.Context, opts session.Options, steps session.StepReporter) (*session.Session, error) {
				if hold {
					close(launched)
					<-release // the pre-sandbox window, held open
				}
				return realLaunch(ctx, opts, steps)
			},
			Defaults: Defaults{Image: "img", Port: 4096, Egress: "open",
				Sandbox: store.SandboxConfig{CPU: 0.5, MemoryMiB: 2048, Timeout: time.Hour, IdleTimeout: 10 * time.Minute}},
		}
	}

	a := newService(base(true))  // the launching replica
	b := newService(base(false)) // the one the DELETE lands on
	defer a.Close()
	defer b.Close()

	p := auth.Principal{UserID: auth.LocalID, OrgID: auth.LocalID}
	sess, err := a.Create(context.Background(), p, store.SessionRequest{
		Name:  "victim",
		Repos: []store.RepoRef{{Platform: "fake", Owner: "acme", Name: "app"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	<-launched // replica A is inside the launch, no sandbox recorded yet

	// The user deletes, and the request lands on replica B.
	if err := b.Delete(context.Background(), p, sess.ID); err != nil {
		t.Fatalf("Delete on the other replica: %v", err)
	}
	after, _ := shared.Sessions().Get(context.Background(), p.OrgID, sess.ID)
	if after.CancelRequestedAt == nil {
		t.Fatal("the delete recorded no durable cancel")
	}

	// Now let replica A's launch finish.
	close(release)
	deadline := time.After(10 * time.Second)
	for {
		got, _ := shared.Sessions().Get(context.Background(), p.OrgID, sess.ID)
		if !got.State.Live() {
			// A delete is the outcome the user asked for, not a failure.
			if got.State != store.StateStopped {
				t.Errorf("the deleted session settled %s, want stopped (error %q)", got.State, got.Error)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatalf("the session never settled: it is still %s", got.State)
		case <-time.After(20 * time.Millisecond):
		}
	}
	// Whatever the row says, no sandbox may outlive the delete: that is
	// the part the user is billed for.
	time.Sleep(200 * time.Millisecond)
	alive, err := provider.List(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range alive {
		if h.State != sandbox.StateStopped {
			t.Errorf("LEAKED SANDBOX: %s is %s after the delete", h.ID, h.State)
		}
	}
}
