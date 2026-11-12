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
	"github.com/jimutt/spov/internal/toolchain"
)

// fakeBuilder publishes what it builds. A build waits for release when
// it is set, so a test can hold one in progress.
type fakeBuilder struct {
	mu        sync.Mutex
	published map[string]bool
	builds    atomic.Int32
	rebuilds  atomic.Int32
	release   chan struct{}
	err       error
	lookupErr error
}

func (b *fakeBuilder) HasImage(_ context.Context, name string) (bool, error) {
	if b.lookupErr != nil {
		return false, b.lookupErr
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.published[name], nil
}

func (b *fakeBuilder) EnsureImage(ctx context.Context, spec sandbox.ImageSpec) (string, error) {
	b.builds.Add(1)
	if b.release != nil {
		select {
		case <-b.release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if b.err != nil {
		return "", b.err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.published == nil {
		b.published = map[string]bool{}
	}
	b.published[spec.Name()] = true
	return spec.Name(), nil
}

// BuildImage builds without looking the name up, as EnsureImage does
// after a miss.
func (b *fakeBuilder) BuildImage(ctx context.Context, spec sandbox.ImageSpec) (string, error) {
	b.rebuilds.Add(1)
	return b.EnsureImage(ctx, spec)
}

func (b *fakeBuilder) ImageID(_ context.Context, name string) (string, error) {
	return "im-" + name, nil
}

func (b *fakeBuilder) DeleteImage(context.Context, string) error { return nil }

func variantSpec(t *testing.T, names ...string) sandbox.ImageSpec {
	t.Helper()
	sel, err := toolchain.Parse(names)
	if err != nil {
		t.Fatal(err)
	}
	return sandbox.ImageSpec{OpenCodeVersion: "2.0.22", Addons: sel}
}

func noNote(string) {}

// TestAPublishedVariantIsNotRebuilt.
//
// @decision(DL-016)
func TestAPublishedVariantIsNotRebuilt(t *testing.T) {
	spec := variantSpec(t, "rust")
	b := &fakeBuilder{published: map[string]bool{spec.Name(): true}}
	var notes []string
	if err := (&VariantBuilder{Builder: b}).Ensure(context.Background(), "", spec, false, func(s string) { notes = append(notes, s) }); err != nil {
		t.Fatal(err)
	}
	if b.builds.Load() != 0 || len(notes) != 0 {
		t.Errorf("a published variant was built %d times, notes %v", b.builds.Load(), notes)
	}
}

// TestConcurrentLaunchesShareOneBuild: one build per name, and every
// waiting launch is told about it.
//
// @decision(DL-016)
func TestConcurrentLaunchesShareOneBuild(t *testing.T) {
	spec := variantSpec(t, "rust", "postgres")
	b := &fakeBuilder{release: make(chan struct{})}
	v := &VariantBuilder{Builder: b}
	var notes atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for range 3 {
		wg.Go(func() {
			errs <- v.Ensure(context.Background(), "", spec, false, func(s string) {
				if strings.Contains(s, spec.Name()) && strings.Contains(s, "rust@") {
					notes.Add(1)
				}
			})
		})
	}
	for notes.Load() < 3 {
		time.Sleep(time.Millisecond)
	}
	close(b.release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if b.builds.Load() != 1 {
		t.Errorf("%d builds for one variant", b.builds.Load())
	}
}

// TestABuildOutlivesTheLaunchThatStartedIt: the launch gives up, the
// build finishes and publishes, and the next launch finds the image.
//
// @decision(DL-016)
func TestABuildOutlivesTheLaunchThatStartedIt(t *testing.T) {
	spec := variantSpec(t, "dotnet")
	b := &fakeBuilder{release: make(chan struct{})}
	v := &VariantBuilder{Builder: b}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- v.Ensure(ctx, "", spec, false, noNote) }()
	for b.builds.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	err := <-done
	if err == nil || !strings.Contains(err.Error(), "still building") {
		t.Fatalf("a cancelled launch says %v", err)
	}
	close(b.release)
	for {
		if ok, _ := b.HasImage(context.Background(), spec.Name()); ok {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err := v.Ensure(context.Background(), "", spec, false, noNote); err != nil || b.builds.Load() != 1 {
		t.Errorf("the next launch: %v after %d builds", err, b.builds.Load())
	}
}

// TestAFailedBuildIsNotRemembered: the launch says what failed, and the
// next launch tries again.
//
// @decision(DL-016)
func TestAFailedBuildIsNotRemembered(t *testing.T) {
	spec := variantSpec(t, "ruby")
	b := &fakeBuilder{err: errors.New("apt: 503")}
	v := &VariantBuilder{Builder: b}
	err := v.Ensure(context.Background(), "", spec, false, noNote)
	if err == nil || !strings.Contains(err.Error(), "apt: 503") || !strings.Contains(err.Error(), "ruby@") {
		t.Fatalf("Ensure = %v", err)
	}
	b.err = nil
	if err := v.Ensure(context.Background(), "", spec, false, noNote); err != nil || b.builds.Load() != 2 {
		t.Errorf("the retry: %v after %d builds", err, b.builds.Load())
	}
	b.lookupErr = errors.New("modal: 401")
	if err := v.Ensure(context.Background(), "", variantSpec(t, "php"), false, noNote); err == nil || b.builds.Load() != 2 {
		t.Errorf("a failed lookup built anyway: %v", err)
	}
}

// TestALaunchBuildsItsVariantBeforeMintingAnything.
//
// @decision(DL-016)
func TestALaunchBuildsItsVariantBeforeMintingAnything(t *testing.T) {
	f := newFixture(t)
	spec := variantSpec(t, "rust")
	b := &fakeBuilder{err: errors.New("build failed")}
	f.launcher.Variants = &VariantBuilder{Builder: b}
	f.opts.Image, f.opts.Variant, f.opts.BuildVariant = spec.Name(), &spec, true
	if _, err := f.launcher.Launch(context.Background(), f.opts); err == nil {
		t.Fatal("a failed build launched anyway")
	}
	if len(f.platform.Requested()) != 0 || len(f.provider.Created) != 0 {
		t.Errorf("a failed build still minted %d tokens and created %d sandboxes", len(f.platform.Requested()), len(f.provider.Created))
	}

	b.err = nil
	f.opts.Addons = spec.Addons
	if _, err := f.launcher.Launch(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	if b.builds.Load() != 2 || f.provider.Created[0].Image != spec.Name() {
		t.Errorf("builds %d, image %s", b.builds.Load(), f.provider.Created[0].Image)
	}
}

// TestAnUnbuiltVariantNamesTheCommandThatBuildsIt: with on-demand
// building off, the refusal tells an operator what to run.
//
// @decision(DL-016)
func TestAnUnbuiltVariantNamesTheCommandThatBuildsIt(t *testing.T) {
	f := newFixture(t)
	spec := variantSpec(t, "rust", "java@17")
	spec.ExtraPackages = []string{"jq"}
	f.opts.Image, f.opts.Variant = spec.Name(), &spec
	f.provider.CreateErr = &sandbox.ImageNotFoundError{Name: spec.Name(), Err: errors.New("not found")}
	_, err := f.launcher.Launch(context.Background(), f.opts)
	want := "spov serve image build --addons java@17,rust@" + mustDefault(t, "rust") + " --apt jq"
	if err == nil || !strings.Contains(err.Error(), want) || !errors.Is(err, sandbox.ErrImageNotFound) {
		t.Errorf("Launch = %v, want it to name %q", err, want)
	}
}

func mustDefault(t *testing.T, name string) string {
	t.Helper()
	sel, err := toolchain.Parse([]string{name})
	if err != nil {
		t.Fatal(err)
	}
	return sel[0].Version
}

// TestAMissingSidecarImageIsNotBlamedOnTheVariant.
//
// @decision(DL-016)
func TestAMissingSidecarImageIsNotBlamedOnTheVariant(t *testing.T) {
	f := newFixture(t)
	spec := variantSpec(t, "rust")
	f.opts.Image, f.opts.Variant = spec.Name(), &spec
	f.provider.CreateErr = &sandbox.ImageNotFoundError{Name: "spov-proxy-abc", Err: errors.New("not found")}
	if _, err := f.launcher.Launch(context.Background(), f.opts); err == nil || strings.Contains(err.Error(), "image build") {
		t.Errorf("Launch = %v", err)
	}
}

// TestBuildsAreBoundedPerProcess: a variant past the bound is refused at
// once, and a launch joining a running build is not.
//
// @decision(DL-016)
func TestBuildsAreBoundedPerProcess(t *testing.T) {
	b := &fakeBuilder{release: make(chan struct{})}
	v := &VariantBuilder{Builder: b, MaxBuilds: 1}
	first := variantSpec(t, "rust")
	done := make(chan error, 2)
	go func() { done <- v.Ensure(context.Background(), "", first, false, noNote) }()
	for b.builds.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	go func() { done <- v.Ensure(context.Background(), "", first, false, noNote) }()
	if err := v.Ensure(context.Background(), "", variantSpec(t, "ruby"), false, noNote); !errors.Is(err, ErrTooManyBuilds) {
		t.Errorf("a second variant past the bound: %v", err)
	}
	close(b.release)
	for range 2 {
		if err := <-done; err != nil {
			t.Errorf("a launch of the running variant: %v", err)
		}
	}
}

// TestARebuildBuildsAPublishedName: the registry says the image was
// deleted, so its name resolving proves nothing.
//
// @decision(DL-016)
func TestARebuildBuildsAPublishedName(t *testing.T) {
	spec := variantSpec(t, "rust")
	b := &fakeBuilder{published: map[string]bool{spec.Name(): true}}
	var built []string
	v := &VariantBuilder{Builder: b, OnBuilt: func(_ context.Context, s sandbox.ImageSpec) { built = append(built, s.Name()) }}
	if err := v.Ensure(context.Background(), "", spec, true, noNote); err != nil {
		t.Fatal(err)
	}
	if b.rebuilds.Load() != 1 || len(built) != 1 || built[0] != spec.Name() {
		t.Fatalf("rebuilds %d, recorded %v", b.rebuilds.Load(), built)
	}
}

// TestABackgroundBuildIsRecordedAndBounded: Start returns at once, the
// finished build is handed to OnBuilt, and a build past the bound is
// not started.
//
// @decision(DL-016)
func TestABackgroundBuildIsRecordedAndBounded(t *testing.T) {
	b := &fakeBuilder{release: make(chan struct{})}
	built := make(chan string, 4)
	var warnings atomic.Int32
	v := &VariantBuilder{Builder: b, MaxBuilds: 1,
		OnBuilt: func(_ context.Context, s sandbox.ImageSpec) { built <- s.Name() },
		Warn:    func(string) { warnings.Add(1) }}
	first, second := variantSpec(t, "rust"), variantSpec(t, "dotnet")
	if !v.Start(context.Background(), "", first, false) {
		t.Fatal("the first build should start")
	}
	if !v.Start(context.Background(), "", first, false) {
		t.Fatal("a second ask for the same name joins the running build")
	}
	if v.Start(context.Background(), "", second, false) {
		t.Fatal("a build past the bound must not start")
	}
	close(b.release)
	if got := <-built; got != first.Name() {
		t.Fatalf("recorded %s", got)
	}
	if b.builds.Load() != 1 {
		t.Fatalf("%d builds", b.builds.Load())
	}

	failing := &fakeBuilder{err: errors.New("apt broke")}
	w := &VariantBuilder{Builder: failing, OnBuilt: func(context.Context, sandbox.ImageSpec) { t.Error("a failed build was recorded") },
		Warn: func(string) { warnings.Add(1) }}
	w.Start(context.Background(), "", second, false)
	for warnings.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
}
