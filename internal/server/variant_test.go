package server

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/session"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/telemetry"
	"github.com/jimutt/spov/internal/toolchain"
)

const variantPin = "2.0.22"

// variantFixture is a deployment that boots spov's own base image, so
// add-ons can be asked for, and that records what each launch was given.
func variantFixture(t *testing.T, onDemand bool) (*apiFixture, func() []session.Options) {
	t.Helper()
	var mu sync.Mutex
	var launched []session.Options
	f := newAPIFixtureWith(t, func(d *Deps) {
		d.Defaults.Image = sandbox.ImageSpec{OpenCodeVersion: variantPin}.Name()
		d.Defaults.OpenCodeVersion = variantPin
		d.Defaults.BuildOnDemand = onDemand
		inner := d.Launch
		d.Launch = func(ctx context.Context, opts session.Options, steps session.StepReporter) (*session.Session, error) {
			mu.Lock()
			launched = append(launched, opts)
			mu.Unlock()
			return inner(ctx, opts, steps)
		}
	})
	return f, func() []session.Options {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(launched)
	}
}

// TestAddonsMakeTheSessionAVariant: the request's add-ons are resolved
// and stored canonically, the image is the variant's, and the launch is
// handed the recipe to build and the add-ons for the entrypoint.
//
// @decision(DL-033) @decision(DL-016)
func TestAddonsMakeTheSessionAVariant(t *testing.T) {
	f, launched := variantFixture(t, true)
	req := repoReq("v")
	req.Addons = []string{"rust", " java@17"}
	req.Apt = []string{"libvips-dev", "jq", "libvips-dev"}
	id := f.create(req)
	f.events(id)

	sel, _ := toolchain.Parse([]string{"java@17", "rust"})
	want := sandbox.ImageSpec{OpenCodeVersion: variantPin, Addons: sel, ExtraPackages: []string{"libvips-dev"}}
	_, body := f.do(http.MethodGet, "/api/v1/sessions/"+id, nil)
	var view SessionView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatal(err)
	}
	if view.Image != want.Name() {
		t.Errorf("image = %s, want the variant %s", view.Image, want.Name())
	}
	if !slices.Equal(view.Addons, toolchain.Strings(sel)) || !slices.Equal(view.Apt, []string{"libvips-dev"}) {
		t.Errorf("stored addons %v, apt %v", view.Addons, view.Apt)
	}
	opts := launched()
	if len(opts) != 1 {
		t.Fatalf("launched %d times", len(opts))
	}
	o := opts[0]
	if o.Variant == nil || o.Variant.Name() != want.Name() || !o.BuildVariant {
		t.Errorf("the launch was not handed the variant to build: %+v, build %v", o.Variant, o.BuildVariant)
	}
	if !slices.Equal(toolchain.Strings(o.Addons), toolchain.Strings(sel)) {
		t.Errorf("the entrypoint gets add-ons %v", toolchain.Strings(o.Addons))
	}
}

// TestASessionWithoutAddonsBootsTheBase: nothing to build.
//
// @decision(DL-016)
func TestASessionWithoutAddonsBootsTheBase(t *testing.T) {
	f, launched := variantFixture(t, true)
	f.events(f.create(repoReq("plain")))
	o := launched()[0]
	if o.Variant != nil || len(o.Addons) != 0 || o.Image != (sandbox.ImageSpec{OpenCodeVersion: variantPin}).Name() {
		t.Errorf("a plain session launched %+v", o)
	}
}

// TestOnDemandBuildingCanBeSwitchedOff: the variant is still the image,
// but the launch may not build it.
//
// @decision(DL-016)
func TestOnDemandBuildingCanBeSwitchedOff(t *testing.T) {
	f, launched := variantFixture(t, false)
	req := repoReq("off")
	req.Addons = []string{"rust"}
	f.events(f.create(req))
	if o := launched()[0]; o.Variant == nil || o.BuildVariant {
		t.Errorf("variant %v, build %v", o.Variant, o.BuildVariant)
	}
}

// TestAnAddonRequestThatCannotBeBuiltIsRefused before anything runs.
//
// @decision(DL-033)
func TestAnAddonRequestThatCannotBeBuiltIsRefused(t *testing.T) {
	for _, tt := range []struct {
		name   string
		adjust func(*SessionRequest)
		custom bool
		want   string
	}{
		{"an unknown add-on", func(r *SessionRequest) { r.Addons = []string{"docker"} }, false, "unknown add-on"},
		{"a bad version", func(r *SessionRequest) { r.Addons = []string{"postgres@9"} }, false, "does not offer"},
		{"a bad package", func(r *SessionRequest) { r.Apt = []string{"x;rm"} }, false, "not a valid package"},
		{"an image of the caller's own", func(r *SessionRequest) { r.Image = "mine"; r.Addons = []string{"rust"} }, false, "spov did not build"},
		{"a deployment with its own image", func(r *SessionRequest) { r.Addons = []string{"rust"} }, true, "boots its own image"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newAPIFixtureWith(t, func(d *Deps) {
				d.Defaults.OpenCodeVersion = variantPin
				if !tt.custom {
					d.Defaults.Image = sandbox.ImageSpec{OpenCodeVersion: variantPin}.Name()
				}
			})
			req := repoReq("bad")
			tt.adjust(&req)
			resp, body := f.do(http.MethodPost, "/api/v1/sessions", req)
			if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(body), tt.want) {
				t.Errorf("create = %d %s, want 422 mentioning %q", resp.StatusCode, body, tt.want)
			}
		})
	}
}

// TestAJobCarriesItsAddonsToItsSession.
//
// @decision(DL-033)
func TestAJobCarriesItsAddonsToItsSession(t *testing.T) {
	f, _ := jobFixtureWith(t, func(d *Deps) {
		d.Defaults.Image = sandbox.ImageSpec{OpenCodeVersion: variantPin}.Name()
		d.Defaults.OpenCodeVersion = variantPin
	})
	req := jobRequest()
	req.Addons = []string{"postgres"}
	resp, body := f.do(http.MethodPost, "/api/v1/jobs", req)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	var view JobView
	_ = json.Unmarshal(body, &view)
	sess, err := f.store.Sessions().Get(context.Background(), auth.LocalID, view.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sess.Request.Addons, []string{"postgres@17"}) || !strings.HasPrefix(sess.Request.Image, "spov-base-"+variantPin) ||
		sess.Request.Image == (sandbox.ImageSpec{OpenCodeVersion: variantPin}).Name() {
		t.Errorf("the job's session asks for %v on %s", sess.Request.Addons, sess.Request.Image)
	}
}

// TestPackagesTheBaseHasBootTheBase: nothing is left to build, so the
// base image is never built on demand through sandbox.apt.
//
// @decision(DL-016)
func TestPackagesTheBaseHasBootTheBase(t *testing.T) {
	f, launched := variantFixture(t, true)
	req := repoReq("jq")
	req.Apt = []string{"jq", "git"}
	f.events(f.create(req))
	if o := launched()[0]; o.Variant != nil || o.Image != (sandbox.ImageSpec{OpenCodeVersion: variantPin}).Name() {
		t.Errorf("packages the base has launched variant %v on %s", o.Variant, o.Image)
	}
}

// TestTheDeploymentsAddonsApplyToARequestThatNamesNone, as its egress
// preset does; a request's own add-ons replace them.
//
// @decision(DL-033)
func TestTheDeploymentsAddonsApplyToARequestThatNamesNone(t *testing.T) {
	var launched []session.Options
	var mu sync.Mutex
	f := newAPIFixtureWith(t, func(d *Deps) {
		d.Defaults.Image = sandbox.ImageSpec{OpenCodeVersion: variantPin}.Name()
		d.Defaults.OpenCodeVersion = variantPin
		d.Defaults.Addons = []string{"postgres"}
		inner := d.Launch
		d.Launch = func(ctx context.Context, opts session.Options, steps session.StepReporter) (*session.Session, error) {
			mu.Lock()
			launched = append(launched, opts)
			mu.Unlock()
			return inner(ctx, opts, steps)
		}
	})
	f.events(f.create(repoReq("none")))
	own := repoReq("own")
	own.Addons = []string{"rust"}
	f.events(f.create(own))
	mu.Lock()
	defer mu.Unlock()
	if got := toolchain.Strings(launched[0].Addons); !slices.Equal(got, []string{"postgres@17"}) {
		t.Errorf("a request naming none got %v", got)
	}
	if got := toolchain.Strings(launched[1].Addons); len(got) != 1 || !strings.HasPrefix(got[0], "rust@") {
		t.Errorf("a request naming its own got %v", got)
	}
}

// TestALaunchDerivesItsVariantFromTheCurrentRecipe: a row written before
// a deploy that changed the pin launches the variant this server builds,
// and add-ons it no longer offers fail the launch plainly.
//
// @decision(DL-016)
func TestALaunchDerivesItsVariantFromTheCurrentRecipe(t *testing.T) {
	f, launched := variantFixture(t, true)
	sess := store.Session{ID: store.NewID(time.Now()), OrgID: auth.LocalID, OwnerID: auth.LocalID, Name: "stale", State: store.StateStarting,
		Request: store.SessionRequest{Name: "stale", Repos: []store.RepoRef{{Platform: "fake", Owner: "acme", Name: "app"}},
			Image: "spov-base-2.0.21-0123456789abcdef0123456789abcdef", Addons: []string{"rust@1.99.0"}, Egress: "open",
			Sandbox: store.SandboxConfig{CPU: 0.5, MemoryMiB: 2048, Timeout: time.Hour, IdleTimeout: 10 * time.Minute}}}
	if err := f.store.Sessions().Create(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	f.srv.svc.launch(context.Background(), sess, newLaunchLog(time.Now), telemetry.KindInteractive, runPlan{})
	sel, _ := toolchain.Parse([]string{"rust@1.99.0"})
	want := sandbox.ImageSpec{OpenCodeVersion: variantPin, Addons: sel}.Name()
	if o := launched(); len(o) != 1 || o[0].Image != want || o[0].Variant == nil {
		t.Fatalf("launched %+v, want %s", o, want)
	}

	sess.ID, sess.Name, sess.Request.Addons = store.NewID(time.Now()), "gone", []string{"cobol"}
	if err := f.store.Sessions().Create(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	f.srv.svc.launch(context.Background(), sess, newLaunchLog(time.Now), telemetry.KindInteractive, runPlan{})
	got, _ := f.store.Sessions().Get(context.Background(), auth.LocalID, sess.ID)
	if got.State != store.StateFailed || !strings.Contains(got.Error, "cannot be built by this server") || len(launched()) != 1 {
		t.Errorf("unknown stored add-ons: state %s, error %q, %d launches", got.State, got.Error, len(launched()))
	}
}
