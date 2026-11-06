package sandbox

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	modal "github.com/modal-labs/modal-client/go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A create Modal refuses for the option, and only that, is retried
// without it.
//
// @decision(DL-084)
func TestExitSnapshotRefusedIsTheOptionOnly(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{modal.InvalidError{Exception: "unknown experimental option enable_exit_snapshot"}, true},
		{status.Error(codes.InvalidArgument, "enable_exit_snapshot is not supported"), true},
		{modal.InvalidError{Exception: "cpu must be positive"}, false},
		{fmt.Errorf("create: %w", errors.New("enable_exit_snapshot timed out")), false},
	} {
		if got := exitSnapshotRefused(c.err); got != c.want {
			t.Errorf("%v: %v, want %v", c.err, got, c.want)
		}
	}
}

// The run tag is written only for a run, and the fake answers each
// exit-snapshot outcome as Modal does.
//
// @decision(DL-084)
func TestTheFakesExitSnapshots(t *testing.T) {
	if tags := BuildTags(TagSet{Name: "a", CreatedAt: time.Now(), Run: 3}); tags[TagRun] != "3" {
		t.Errorf("tags = %v", tags)
	}
	if tags := BuildTags(TagSet{Name: "a", CreatedAt: time.Now()}); tags[TagRun] != "" {
		t.Errorf("an untagged run: %v", tags)
	}
	ctx := context.Background()
	f := NewFake()
	spec := func(name string, snap bool) Spec {
		return Spec{Name: name, Image: "img", Command: []string{"x"}, CPU: 0.5, MemoryMiB: 512, Timeout: time.Hour,
			IdleTimeout: time.Minute, Egress: EgressOpen(), ExitSnapshot: snap}
	}
	with, _ := f.Create(ctx, spec("with", true))
	without, _ := f.Create(ctx, spec("without", false))
	id := func(h Handle) string { return h.(*FakeHandle).ID() }
	if _, err := f.ExitSnapshot(ctx, id(with), time.Second); !errors.Is(err, ErrSnapshotPending) {
		t.Errorf("a running sandbox: %v", err)
	}
	with.(*FakeHandle).End()
	if img, err := f.ExitSnapshot(ctx, id(with), time.Second); err != nil || img != "im-snap-"+id(with) {
		t.Errorf("an ended one: %q, %v", img, err)
	}
	if _, err := f.ExitSnapshot(ctx, id(without), time.Second); !errors.Is(err, ErrNoExitSnapshot) {
		t.Errorf("one created without: %v", err)
	}
	if _, err := f.ExitSnapshot(ctx, "sb-nope", time.Second); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown one: %v", err)
	}
	f.RefuseExitSnapshots = true
	refused, _ := f.Create(ctx, spec("refused", true))
	refused.(*FakeHandle).End()
	if _, err := f.ExitSnapshot(ctx, id(refused), time.Second); !errors.Is(err, ErrNoExitSnapshot) {
		t.Errorf("a create Modal refused the option for: %v", err)
	}
}
