package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/toolchain"
)

// DefaultVariantBuildTimeout bounds one variant build. It is not the
// launch's bound: a build outlives a launch that stops waiting for it.
//
// @decision(DL-016)
const DefaultVariantBuildTimeout = 30 * time.Minute

// DefaultMaxVariantBuilds bounds how many different variants one server
// process builds at once. A launch past it is refused rather than queued,
// so no caller can stack up detached builds.
//
// @decision(DL-016)
const DefaultMaxVariantBuilds = 2

// DefaultMaxRunningPerOwner bounds the variant builds one user has
// running at once in a server process. A second launch of theirs waits
// for the first build to end.
//
// @decision(DL-134)
const DefaultMaxRunningPerOwner = 1

// DefaultMaxNewPerOwner bounds the variant builds one user's requests may
// start in DefaultOwnerWindow, failed ones included.
//
// @decision(DL-134)
const DefaultMaxNewPerOwner = 5

// DefaultOwnerWindow is the span DefaultMaxNewPerOwner counts over.
//
// @decision(DL-134)
const DefaultOwnerWindow = 24 * time.Hour

// recordTimeout bounds recording a finished build.
const recordTimeout = 30 * time.Second

// ErrTooManyBuilds refuses a launch whose variant would exceed the bound.
var ErrTooManyBuilds = errors.New("too many image builds in progress")

// BuildLimitError refuses a launch whose user has started as many new
// image builds as their window allows.
//
// @decision(DL-134)
type BuildLimitError struct {
	Max    int
	Window time.Duration
	// Until is when the user's oldest counted build leaves the window,
	// and so when the next build is allowed.
	Until time.Time
	// Wait is Until less the time of the refusal.
	Wait time.Duration
}

func (e *BuildLimitError) Error() string {
	return fmt.Sprintf("you have started %d new image builds in the last %s, which is the limit; "+
		"the next one is allowed in %s (at %s UTC). A launch whose image already exists is not limited",
		e.Max, e.Window, e.Wait.Round(time.Minute), e.Until.UTC().Format("15:04 on 2 Jan"))
}

// VariantBuilder builds an add-on variant the first time a launch asks
// for it. One build runs per image name; a launch that arrives while it
// runs waits for the same build. The build is detached from the launch,
// so a launch that times out leaves it to finish and publish.
//
// @decision(DL-016)
type VariantBuilder struct {
	Builder sandbox.ImageBuilder
	// Timeout bounds one build; zero means DefaultVariantBuildTimeout.
	Timeout time.Duration
	// MaxBuilds bounds the builds running at once; zero means
	// DefaultMaxVariantBuilds.
	MaxBuilds int
	// OnBuilt is told of every build that published, on the launch path
	// or in the background, so the server can record it.
	OnBuilt func(ctx context.Context, spec sandbox.ImageSpec)
	// OnFinished is told how every build ended, for the metrics.
	//
	// @decision(DL-011)
	OnFinished func(ctx context.Context, err error)
	// Warn is told of a background build that failed.
	Warn func(string)

	// MaxRunningPerOwner, MaxNewPerOwner and OwnerWindow bound what one
	// owner's requests start, in this process only; zero means the
	// Default constants. An empty owner is not limited, which is how
	// the operator's own paths build.
	//
	// @decision(DL-134)
	MaxRunningPerOwner int
	MaxNewPerOwner     int
	OwnerWindow        time.Duration
	// Now is the clock for the window; nil means time.Now.
	Now func() time.Time

	mu      sync.Mutex
	running map[string]*variantBuild
	// starts are the times each owner's builds began, newest last, kept
	// for OwnerWindow.
	starts map[string][]time.Time
}

type variantBuild struct {
	done  chan struct{}
	err   error
	owner string
}

// Ensure returns once spec's image is published, building it if no one
// has. rebuild builds it even though its name resolves: the registry
// says the image was deleted (DL-016). note is told when a build starts
// or is joined.
//
// owner is the user the build is for. A build the owner already has
// running makes this one wait for it, up to ctx; a new build past the
// owner's window is refused with a *BuildLimitError. An image that is
// already published, or building for anyone, is neither (DL-134).
//
// @decision(DL-134)
func (v *VariantBuilder) Ensure(ctx context.Context, owner string, spec sandbox.ImageSpec, rebuild bool, note func(string)) error {
	name := spec.Name()
	var b *variantBuild
	for {
		if !rebuild {
			ok, err := v.Builder.HasImage(ctx, name)
			if err != nil {
				return err
			}
			if ok {
				return nil
			}
		}
		built, wait, err := v.start(ctx, owner, spec, rebuild, true)
		if err != nil {
			return err
		}
		if wait == nil {
			b = built
			break
		}
		note("waiting for your earlier image build to finish; one runs at a time for each user")
		select {
		case <-wait:
		case <-ctx.Done():
			return fmt.Errorf("image %s is waiting for your earlier image build to finish: %w", name, ctx.Err())
		}
	}
	note(fmt.Sprintf("building image %s (%s) on first use; this takes a few minutes and happens once", name, describeVariant(spec)))
	select {
	case <-b.done:
		if b.err != nil {
			return fmt.Errorf("build the image for %s: %w", describeVariant(spec), b.err)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("image %s is still building, and will be ready for a later launch: %w", name, ctx.Err())
	}
}

// Start builds spec in the background and reports whether a build is now
// running for it. rebuild is as for Ensure. A build past the bound is
// not started; the next launch asks again.
//
// @decision(DL-016)
func (v *VariantBuilder) Start(ctx context.Context, owner string, spec sandbox.ImageSpec, rebuild bool) bool {
	b, _, err := v.start(ctx, owner, spec, rebuild, false)
	return err == nil && b != nil
}

// Allow reports whether owner may start a new build now: nil, or a
// *BuildLimitError saying when. It starts and counts nothing.
//
// @decision(DL-134)
func (v *VariantBuilder) Allow(owner string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.limit(owner, v.now())
}

func (v *VariantBuilder) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

// limit is the window's refusal for owner, with v.mu held. It drops
// every owner's starts that have left the window, so the map holds only
// users with a build in the last day.
func (v *VariantBuilder) limit(owner string, now time.Time) error {
	if owner == "" {
		return nil
	}
	window, max := v.ownerWindow(), v.maxNewPerOwner()
	for o, ts := range v.starts {
		i := 0
		for i < len(ts) && !ts[i].After(now.Add(-window)) {
			i++
		}
		if i == len(ts) {
			delete(v.starts, o)
		} else if i > 0 {
			v.starts[o] = ts[i:]
		}
	}
	ts := v.starts[owner]
	if len(ts) < max {
		return nil
	}
	until := ts[len(ts)-max].Add(window)
	return &BuildLimitError{Max: max, Window: window, Until: until, Wait: until.Sub(now)}
}

func (v *VariantBuilder) ownerWindow() time.Duration {
	if v.OwnerWindow > 0 {
		return v.OwnerWindow
	}
	return DefaultOwnerWindow
}

func (v *VariantBuilder) maxNewPerOwner() int {
	if v.MaxNewPerOwner > 0 {
		return v.MaxNewPerOwner
	}
	return DefaultMaxNewPerOwner
}

func (v *VariantBuilder) maxRunningPerOwner() int {
	if v.MaxRunningPerOwner > 0 {
		return v.MaxRunningPerOwner
	}
	return DefaultMaxRunningPerOwner
}

// start joins the build of spec's name or begins one, within the bounds.
// A build joined is not the owner's: it is neither counted nor waited
// for. When the owner already has a build running, start returns that
// build's done channel to wait on if queue is set, and refuses otherwise,
// as the background path does not queue.
//
// @decision(DL-134)
func (v *VariantBuilder) start(ctx context.Context, owner string, spec sandbox.ImageSpec, rebuild, queue bool) (*variantBuild, <-chan struct{}, error) {
	name := spec.Name()
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.running == nil {
		v.running = map[string]*variantBuild{}
	}
	if b, ok := v.running[name]; ok {
		return b, nil, nil
	}
	now := v.now()
	if err := v.limit(owner, now); err != nil {
		return nil, nil, err
	}
	if owner != "" {
		n := 0
		var own *variantBuild
		for _, b := range v.running {
			if b.owner == owner {
				n++
				own = b
			}
		}
		if n >= v.maxRunningPerOwner() {
			if queue {
				return nil, own.done, nil
			}
			return nil, nil, fmt.Errorf("%w for you; launch again once it has finished", ErrTooManyBuilds)
		}
	}
	if len(v.running) >= v.maxBuilds() {
		return nil, nil, fmt.Errorf("%w (%d); launch again once one has finished", ErrTooManyBuilds, len(v.running))
	}
	b := &variantBuild{done: make(chan struct{}), owner: owner}
	v.running[name] = b
	if owner != "" {
		if v.starts == nil {
			v.starts = map[string][]time.Time{}
		}
		v.starts[owner] = append(v.starts[owner], now)
	}
	go v.build(context.WithoutCancel(ctx), spec, rebuild, b)
	return b, nil, nil
}

func (v *VariantBuilder) maxBuilds() int {
	if v.MaxBuilds > 0 {
		return v.MaxBuilds
	}
	return DefaultMaxVariantBuilds
}

func (v *VariantBuilder) build(ctx context.Context, spec sandbox.ImageSpec, rebuild bool, b *variantBuild) {
	timeout := v.Timeout
	if timeout <= 0 {
		timeout = DefaultVariantBuildTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if rebuild {
		_, b.err = v.Builder.BuildImage(ctx, spec)
	} else {
		_, b.err = v.Builder.EnsureImage(ctx, spec)
	}
	if b.err == nil && v.OnBuilt != nil {
		// Its own bound, not what is left of the build's.
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
		v.OnBuilt(rctx, spec)
		rcancel()
	}
	if b.err != nil && v.Warn != nil {
		v.Warn(fmt.Sprintf("building image %s (%s) failed: %v", spec.Name(), describeVariant(spec), b.err))
	}
	if v.OnFinished != nil {
		v.OnFinished(context.WithoutCancel(ctx), b.err)
	}
	v.mu.Lock()
	delete(v.running, spec.Name())
	v.mu.Unlock()
	close(b.done)
}

// describeVariant names what a variant adds to the base image.
func describeVariant(spec sandbox.ImageSpec) string {
	var parts []string
	if len(spec.Addons) > 0 {
		parts = append(parts, "add-ons "+strings.Join(toolchain.Installed(spec.Addons), ", "))
	}
	if len(spec.ExtraPackages) > 0 {
		parts = append(parts, "apt "+strings.Join(spec.ExtraPackages, ", "))
	}
	return strings.Join(parts, "; ")
}

// BuildCommand is the operator command that prebuilds spec's image.
//
// @decision(DL-016)
func BuildCommand(spec sandbox.ImageSpec) string {
	cmd := "spov serve image build"
	if len(spec.Addons) > 0 {
		cmd += " --addons " + strings.Join(toolchain.Strings(spec.Addons), ",")
	}
	if len(spec.ExtraPackages) > 0 {
		cmd += " --apt " + strings.Join(spec.ExtraPackages, ",")
	}
	return cmd
}

// unpublishedVariant explains a launch whose variant image is missing.
// Any other failure, including a missing sidecar image, passes through.
func unpublishedVariant(spec sandbox.ImageSpec, built bool, err error) error {
	var nf *sandbox.ImageNotFoundError
	if !errors.As(err, &nf) || nf.Name != spec.Name() {
		return err
	}
	why := "this server does not build images on demand"
	if built {
		why = "it was not found after being built"
	}
	return fmt.Errorf("the image for %s has not been published, and %s; "+
		"an operator can build it with `%s`: %w", describeVariant(spec), why, BuildCommand(spec), err)
}
