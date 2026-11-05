package cli

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/config"
)

// Local mode is one process against no database, so the reconcile loop
// runs directly: gating it on a lock nobody can take would leave a
// local server never reconciling.
//
// @decision(DL-004) @decision(DL-002)
func TestReconcilerRunsDirectlyInLocalMode(t *testing.T) {
	for _, tc := range []struct {
		name  string
		local bool
		vars  map[string]string
	}{
		{"local with a database configured", true,
			map[string]string{DatabaseURLEnvVar: "postgres://nope:nope@127.0.0.1:1/none"}},
		{"hosted with no database", false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &Env{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, Environ: config.MapEnv{Vars: tc.vars}}
			run := e.reconcilerRunner(tc.local, func(string) {})
			ran := make(chan struct{})
			go run(context.Background(), func(context.Context) { close(ran) })
			select {
			case <-ran:
			case <-time.After(5 * time.Second):
				t.Fatal("the reconciler never ran")
			}
		})
	}
}

// A hosted deployment with a database takes the lock first. The work
// must not run while the lock cannot be had — that is the whole point,
// and running it anyway is the bug this test exists for: every replica
// reconciling means the stale-starting sweep arbitrates launches it is
// not running.
//
// @decision(DL-004)
func TestReconcilerWaitsForTheLockWhenHosted(t *testing.T) {
	// An unreachable database: the lock can never be taken, so the work
	// must never run.
	e := &Env{Out: &bytes.Buffer{}, Err: &bytes.Buffer{},
		Environ: config.MapEnv{Vars: map[string]string{
			DatabaseURLEnvVar: "postgres://nope:nope@127.0.0.1:1/none",
		}}}
	run := e.reconcilerRunner(false, func(string) {})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ran := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		run(ctx, func(context.Context) { close(ran) })
	}()

	select {
	case <-ran:
		t.Fatal("the reconciler ran without holding the lock")
	case <-time.After(500 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("the runner did not stop with its context")
	}
}
