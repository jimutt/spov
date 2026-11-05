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

// recordTimeout bounds recording a finished build.
const recordTimeout = 30 * time.Second

// ErrTooManyBuilds refuses a launch whose variant would exceed the bound.
var ErrTooManyBuilds = errors.New("too many image builds in progress")

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

	mu      sync.Mutex
	running map[string]*variantBuild
}

type variantBuild struct {
	done chan struct{}
	err  error
}

// Ensure returns once spec's image is published, building it if no one
// has. rebuild builds it even though its name resolves: the registry
// says the image was deleted (DL-016). note is told when a build starts
// or is joined.
func (v *VariantBuilder) Ensure(ctx context.Context, spec sandbox.ImageSpec, rebuild bool, note func(string)) error {
	name := spec.Name()
	if !rebuild {
		ok, err := v.Builder.HasImage(ctx, name)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
	b, err := v.start(ctx, spec, rebuild)
	if err != nil {
		return err
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
func (v *VariantBuilder) Start(ctx context.Context, spec sandbox.ImageSpec, rebuild bool) bool {
	_, err := v.start(ctx, spec, rebuild)
	return err == nil
}

// start joins the build of spec's name or begins one, within the bound.
func (v *VariantBuilder) start(ctx context.Context, spec sandbox.ImageSpec, rebuild bool) (*variantBuild, error) {
	name := spec.Name()
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.running == nil {
		v.running = map[string]*variantBuild{}
	}
	if b, ok := v.running[name]; ok {
		return b, nil
	}
	if len(v.running) >= v.maxBuilds() {
		return nil, fmt.Errorf("%w (%d); launch again once one has finished", ErrTooManyBuilds, len(v.running))
	}
	b := &variantBuild{done: make(chan struct{})}
	v.running[name] = b
	go v.build(context.WithoutCancel(ctx), spec, rebuild, b)
	return b, nil
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
