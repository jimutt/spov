package session

import (
	"context"
	"testing"
	"time"
)

// TestALocalServersBuildsAreNotLimited: without PerOwnerLimits, which
// only a shared (hosted) server sets, a user may build as many variants
// as they like, and in parallel.
//
// @decision(DL-134)
func TestALocalServersBuildsAreNotLimited(t *testing.T) {
	b := &fakeBuilder{release: make(chan struct{})}
	v := &VariantBuilder{Builder: b, MaxBuilds: 8, MaxNewPerOwner: 1, MaxRunningPerOwner: 1}
	ctx := context.Background()
	done := make(chan error, 7)
	for i := range 7 {
		go func() { done <- v.Ensure(ctx, "local", aptSpec(i), false, noNote) }()
	}
	waitForBuilds(t, b, 7)
	if err := v.Allow("local"); err != nil {
		t.Errorf("Allow = %v", err)
	}
	close(b.release)
	for range 7 {
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("a build: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a build never finished")
		}
	}
	if err := v.Allow("local"); err != nil {
		t.Errorf("Allow after %d builds = %v", b.builds.Load(), err)
	}
}
