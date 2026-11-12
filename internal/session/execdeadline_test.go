package session

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/sandbox"
)

// TestEverySandboxExecThisPackageMakesIsBounded: a program in the sandbox
// that never exits holds an exec forever, so each helper sets its own
// deadline. The fake refuses an exec whose context has none, and these
// are called with a context that has none.
//
// @decision(DL-135)
func TestEverySandboxExecThisPackageMakesIsBounded(t *testing.T) {
	f := newFixture(t)
	sess, err := f.launcher.Launch(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	h := sess.Handle
	ctx := context.Background()
	base := strings.Repeat("a", 40)

	checks := map[string]error{
		"seed": applySyncSeed(ctx, h, "app", SyncSeed{Branch: "main", BaseCommit: base}),
	}
	_, checks["export"] = ExportSync(ctx, h, "app", base)
	_, checks["inspect"] = InspectSync(ctx, h, "app", base)
	_, checks["password"] = PasswordFromSandbox(ctx, h)
	for name, err := range checks {
		if errors.Is(err, sandbox.ErrExecNoDeadline) {
			t.Errorf("the %s exec ran without a deadline: %v", name, err)
		}
	}
	if len(h.(*sandbox.FakeHandle).ExecLog()) < 4 {
		t.Errorf("the fake saw %d execs, want all four", len(h.(*sandbox.FakeHandle).ExecLog()))
	}
}
