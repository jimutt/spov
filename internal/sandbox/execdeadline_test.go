package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"
)

// execCtx is a context an exec may run under: it has a deadline.
func execCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// TestTheFakeRefusesAnExecWithNoDeadline: a program in the sandbox that
// never exits holds a call whose context has no deadline for ever, so
// the fake fails one, and every caller in the server runs its exec under
// its own bound.
//
// @decision(DL-135)
func TestTheFakeRefusesAnExecWithNoDeadline(t *testing.T) {
	f := NewFake()
	spec := validSpec()
	spec.Sidecar = proxySidecar()
	h, err := f.Create(execCtx(t), spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Exec(context.Background(), []string{"sleep", "infinity"}); !errors.Is(err, ErrExecNoDeadline) {
		t.Errorf("Exec with no deadline = %v, want ErrExecNoDeadline", err)
	}
	if _, err := h.ExecInput(context.Background(), []string{"cat"}, []byte("x")); !errors.Is(err, ErrExecNoDeadline) {
		t.Errorf("ExecInput with no deadline = %v, want ErrExecNoDeadline", err)
	}
	if _, err := h.ExecBounded(context.Background(), []string{"cat"}, nil, MaxExecOutput); !errors.Is(err, ErrExecNoDeadline) {
		t.Errorf("ExecBounded with no deadline = %v, want ErrExecNoDeadline", err)
	}
	if _, err := h.SidecarExecInput(context.Background(), "proxy", []string{"set-token", "gh"}, nil); !errors.Is(err, ErrExecNoDeadline) {
		t.Errorf("SidecarExecInput with no deadline = %v, want ErrExecNoDeadline", err)
	}
	if got := h.(*FakeHandle).ExecLog(); len(got) != 0 {
		t.Errorf("an exec the fake refused was recorded: %v", got)
	}
	if _, err := h.Exec(execCtx(t), []string{"true"}); err != nil {
		t.Errorf("Exec with a deadline: %v", err)
	}
}

// TestAnExecWithNoDeadlineGetsTheDefault: the real handle's net.
//
// @decision(DL-135)
func TestAnExecWithNoDeadlineGetsTheDefault(t *testing.T) {
	ctx, cancel := execContext(context.Background())
	defer cancel()
	d, ok := ctx.Deadline()
	if !ok || time.Until(d) > DefaultExecTimeout || time.Until(d) < DefaultExecTimeout-time.Minute {
		t.Errorf("deadline = %v, %v; want about %s away", d, ok, DefaultExecTimeout)
	}
	own, cancelOwn := context.WithTimeout(context.Background(), time.Second)
	defer cancelOwn()
	got, cancelGot := execContext(own)
	defer cancelGot()
	if d2, _ := got.Deadline(); d2.After(time.Now().Add(2 * time.Second)) {
		t.Errorf("a caller's own, shorter deadline was replaced: %v", d2)
	}
}
