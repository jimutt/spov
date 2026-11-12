package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/sandbox"
)

// waitForBuilds waits until n builds have started.
func waitForBuilds(t *testing.T, b *fakeBuilder, n int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for b.builds.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d builds started, want %d", b.builds.Load(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// aptSpec is a variant that differs by one package name.
func aptSpec(i int) sandbox.ImageSpec {
	return sandbox.ImageSpec{OpenCodeVersion: "2.0.22", ExtraPackages: []string{"pkg" + strings.Repeat("x", i)}}
}

// TestASecondBuildForTheSameUserWaitsWhileAnotherUsersProceeds: one
// build runs at a time for a user, and the wait is for that user only.
//
// @decision(DL-134)
func TestASecondBuildForTheSameUserWaitsWhileAnotherUsersProceeds(t *testing.T) {
	b := &fakeBuilder{release: make(chan struct{})}
	v := &VariantBuilder{Builder: b, MaxBuilds: 2, PerOwnerLimits: true}
	ctx := context.Background()
	rust, ruby, dotnet := variantSpec(t, "rust"), variantSpec(t, "ruby"), variantSpec(t, "dotnet")
	done := make(chan error, 3)
	go func() { done <- v.Ensure(ctx, "ann", rust, false, noNote) }()
	waitForBuilds(t, b, 1)
	var waiting atomic.Int32
	go func() {
		done <- v.Ensure(ctx, "ann", ruby, false, func(s string) {
			if strings.Contains(s, "waiting for your earlier") {
				waiting.Add(1)
			}
		})
	}()
	go func() { done <- v.Ensure(ctx, "bob", dotnet, false, noNote) }()
	waitForBuilds(t, b, 2)
	for waiting.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if n := b.builds.Load(); n != 2 {
		t.Fatalf("%d builds started: ann's second should wait, bob's should not", n)
	}
	close(b.release)
	for range 3 {
		if err := <-done; err != nil {
			t.Errorf("a launch: %v", err)
		}
	}
	if n := b.builds.Load(); n != 3 {
		t.Errorf("%d builds, want 3: the waiting launch builds once its turn comes", n)
	}
}

// TestAWaitingBuildGivesUpWithItsLaunch: the wait is bounded by the
// launch's own context.
//
// @decision(DL-134)
func TestAWaitingBuildGivesUpWithItsLaunch(t *testing.T) {
	b := &fakeBuilder{release: make(chan struct{})}
	v := &VariantBuilder{Builder: b, MaxBuilds: 2, PerOwnerLimits: true}
	go func() { _ = v.Ensure(context.Background(), "ann", variantSpec(t, "rust"), false, noNote) }()
	waitForBuilds(t, b, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := v.Ensure(ctx, "ann", variantSpec(t, "ruby"), false, noNote); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Ensure = %v", err)
	}
	if b.builds.Load() != 1 {
		t.Errorf("%d builds", b.builds.Load())
	}
	close(b.release)
}

// TestABackgroundBuildDoesNotQueueBehindTheUsersOwn: the background path
// skips, as it does past the global bound.
//
// @decision(DL-134)
func TestABackgroundBuildDoesNotQueueBehindTheUsersOwn(t *testing.T) {
	b := &fakeBuilder{release: make(chan struct{})}
	v := &VariantBuilder{Builder: b, MaxBuilds: 2, PerOwnerLimits: true}
	ctx := context.Background()
	if !v.Start(ctx, "ann", variantSpec(t, "rust"), false) {
		t.Fatal("the first build should start")
	}
	if v.Start(ctx, "ann", variantSpec(t, "ruby"), false) {
		t.Error("a second background build for the same user must not start")
	}
	if !v.Start(ctx, "bob", variantSpec(t, "dotnet"), false) {
		t.Error("another user's background build should start")
	}
	close(b.release)
}

// TestTheSixthNewVariantInADayIsRefusedAndTheWindowFrees: failed builds
// count, and the next is allowed as the oldest leaves the window.
//
// @decision(DL-134)
func TestTheSixthNewVariantInADayIsRefusedAndTheWindowFrees(t *testing.T) {
	b := &fakeBuilder{err: errors.New("apt broke")}
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	v := &VariantBuilder{Builder: b, MaxBuilds: 2, PerOwnerLimits: true, Now: clock}
	ctx := context.Background()
	var limit *BuildLimitError
	for i := range 5 {
		if err := v.Ensure(ctx, "ann", aptSpec(i), false, noNote); err == nil || errors.As(err, &limit) {
			t.Fatalf("build %d should start and fail on its own: %v", i+1, err)
		}
		advance(time.Hour)
	}
	err := v.Ensure(ctx, "ann", aptSpec(5), false, noNote)
	if !errors.As(err, &limit) {
		t.Fatalf("the sixth build in 24 hours: %v", err)
	}
	if want := 19 * time.Hour; limit.Wait != want {
		t.Errorf("Wait = %s, want %s (the first build leaves the window then)", limit.Wait, want)
	}
	for _, want := range []string{"5 new image builds", "24h0m0s", "19h0m0s", "which is the limit"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q lacks %q", err, want)
		}
	}
	if v.Allow("ann") == nil {
		t.Error("Allow should refuse too")
	}
	if err := v.Allow("bob"); err != nil {
		t.Errorf("another user: %v", err)
	}
	if n := b.builds.Load(); n != 5 {
		t.Errorf("%d builds started, want 5", n)
	}
	advance(19 * time.Hour)
	if err := v.Allow("ann"); err != nil {
		t.Errorf("after the window: %v", err)
	}
	if err := v.Ensure(ctx, "ann", aptSpec(6), false, noNote); errors.As(err, &limit) {
		t.Errorf("the first slot frees as the oldest build leaves the window: %v", err)
	}
}

// TestAnExistingVariantNeverCountsOrWaits: a published image, or one
// another user is building, costs the user nothing.
//
// @decision(DL-134)
func TestAnExistingVariantNeverCountsOrWaits(t *testing.T) {
	rust := variantSpec(t, "rust")
	b := &fakeBuilder{published: map[string]bool{rust.Name(): true}, release: make(chan struct{})}
	v := &VariantBuilder{Builder: b, MaxBuilds: 2, PerOwnerLimits: true, MaxNewPerOwner: 1}
	ctx := context.Background()
	for range 10 {
		if err := v.Ensure(ctx, "ann", rust, false, noNote); err != nil {
			t.Fatalf("a published image: %v", err)
		}
	}
	if b.builds.Load() != 0 {
		t.Fatalf("%d builds", b.builds.Load())
	}
	// Bob builds; ann joins his build while holding no slot of her own.
	ruby := variantSpec(t, "ruby")
	done := make(chan error, 2)
	go func() { done <- v.Ensure(ctx, "bob", ruby, false, noNote) }()
	waitForBuilds(t, b, 1)
	go func() { done <- v.Ensure(ctx, "ann", ruby, false, noNote) }()
	time.Sleep(20 * time.Millisecond)
	if err := v.Allow("ann"); err != nil {
		t.Errorf("joining a build is not a start: %v", err)
	}
	close(b.release)
	for range 2 {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
	if err := v.Allow("ann"); err != nil {
		t.Errorf("ann started nothing: %v", err)
	}
	if err := v.Allow("bob"); err == nil {
		t.Error("bob's one build is his limit here")
	}
}

// TestAnEmptyOwnerIsNotLimited: the operator's own paths name no owner.
//
// @decision(DL-134)
func TestAnEmptyOwnerIsNotLimited(t *testing.T) {
	b := &fakeBuilder{}
	v := &VariantBuilder{Builder: b, PerOwnerLimits: true, MaxNewPerOwner: 1}
	for i := range 3 {
		if err := v.Ensure(context.Background(), "", aptSpec(i), false, noNote); err != nil {
			t.Fatal(err)
		}
	}
}
