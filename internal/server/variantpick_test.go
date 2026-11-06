package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/session"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/toolchain"
)

// imageFake is the Modal side of the image registry: which names
// resolve, what was built and what was deleted. A build waits for
// release when it is set.
type imageFake struct {
	mu        sync.Mutex
	published map[string]bool
	built     []string
	rebuilt   []string
	deleted   []string
	release   chan struct{}
	deleteErr map[string]error
}

func newImageFake(published ...string) *imageFake {
	f := &imageFake{published: map[string]bool{}}
	for _, n := range published {
		f.published[n] = true
	}
	return f
}

func (f *imageFake) HasImage(_ context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.published[name], nil
}

func (f *imageFake) EnsureImage(ctx context.Context, spec sandbox.ImageSpec) (string, error) {
	if ok, _ := f.HasImage(ctx, spec.Name()); ok {
		return spec.Name(), nil
	}
	return f.build(ctx, spec, false)
}

func (f *imageFake) BuildImage(ctx context.Context, spec sandbox.ImageSpec) (string, error) {
	return f.build(ctx, spec, true)
}

func (f *imageFake) build(ctx context.Context, spec sandbox.ImageSpec, rebuild bool) (string, error) {
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.built = append(f.built, spec.Name())
	if rebuild {
		f.rebuilt = append(f.rebuilt, spec.Name())
	}
	f.published[spec.Name()] = true
	return spec.Name(), nil
}

func (f *imageFake) ImageID(_ context.Context, name string) (string, error) { return "im-" + name, nil }

func (f *imageFake) DeleteImage(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.deleteErr[id]; err != nil {
		return err
	}
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *imageFake) builds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.built)
}

// pinResolver resolves each named add-on to a fixed release.
type pinResolver map[string]string

func (r pinResolver) Resolve(_ context.Context, sel []toolchain.Selection) []toolchain.Selection {
	out := toolchain.Unresolved(sel)
	for i := range out {
		if v, ok := r[out[i].Name]; ok {
			out[i], _ = out[i].Resolve(v)
		}
	}
	return out
}

// registryFixture is a variant deployment with an image registry,
// node@22 resolving to 22.23.3.
type registryFixture struct {
	*apiFixture
	launched func() []session.Options
	images   *store.MemoryImages
	modal    *imageFake
	now      time.Time
	// launchErr, when set, fails the next launch.
	launchErr error
}

func newRegistryFixture(t *testing.T, onDemand bool, modal *imageFake) *registryFixture {
	t.Helper()
	rf := &registryFixture{images: store.NewMemoryImages(), modal: modal, now: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}
	var mu sync.Mutex
	var launched []session.Options
	variants := &session.VariantBuilder{Builder: modal}
	rf.apiFixture = newAPIFixtureWith(t, func(d *Deps) {
		d.Defaults.Image = sandbox.ImageSpec{OpenCodeVersion: variantPin}.Name()
		d.Defaults.OpenCodeVersion = variantPin
		d.Defaults.BuildOnDemand = onDemand
		d.Images, d.Builder, d.Variants = rf.images, modal, variants
		d.Resolver = pinResolver{"node": "22.23.3"}
		inner := d.Launch
		d.Launch = func(ctx context.Context, opts session.Options, steps session.StepReporter) (*session.Session, error) {
			mu.Lock()
			launched = append(launched, opts)
			err := rf.launchErr
			rf.launchErr = nil
			mu.Unlock()
			if err != nil {
				return nil, err
			}
			return inner(ctx, opts, steps)
		}
	})
	variants.OnBuilt = rf.srv.Service().RecordVariant
	rf.launched = func() []session.Options {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(launched)
	}
	return rf
}

// specs are node@22's request, as written, and its exact build.
func nodeSpecs(t *testing.T) (requested, exact sandbox.ImageSpec) {
	t.Helper()
	sel, err := toolchain.Parse([]string{"node@22"})
	if err != nil {
		t.Fatal(err)
	}
	requested = sandbox.ImageSpec{OpenCodeVersion: variantPin, Addons: sel}
	exact = requested
	exact.Addons = pinResolver{"node": "22.23.3"}.Resolve(context.Background(), sel)
	return requested, exact
}

func (rf *registryFixture) launchNode(t *testing.T) (session.Options, SessionView) {
	t.Helper()
	req := repoReq(fmt.Sprintf("node-%d", len(rf.launched())))
	req.Addons = []string{"node@22"}
	id := rf.create(req)
	rf.events(id)
	_, body := rf.do(http.MethodGet, "/api/v1/sessions/"+id, nil)
	var view SessionView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatal(err)
	}
	opts := rf.launched()
	return opts[len(opts)-1], view
}

// TestALaunchBootsTheExactBuildWhenItIsPublished: the registry knows the
// exact build, so nothing is built and the session records it.
//
// @decision(DL-033) @decision(DL-016)
func TestALaunchBootsTheExactBuildWhenItIsPublished(t *testing.T) {
	_, exact := nodeSpecs(t)
	rf := newRegistryFixture(t, true, newImageFake(exact.Name()))
	_ = rf.images.Record(context.Background(), VariantImage(exact))
	o, view := rf.launchNode(t)
	if o.Image != exact.Name() || o.Variant != nil {
		t.Fatalf("launched %s, build %v", o.Image, o.Variant)
	}
	if toolchain.Installed(o.Addons)[0] != "node@22.23.3" {
		t.Errorf("the entrypoint and guide get %v", toolchain.Installed(o.Addons))
	}
	if view.BootedImage != exact.Name() || !slices.Equal(view.InstalledAddons, []string{"node@22.23.3"}) || !slices.Equal(view.Addons, []string{"node@22"}) {
		t.Errorf("view: booted %s, installed %v, asked %v", view.BootedImage, view.InstalledAddons, view.Addons)
	}
}

// TestAnExactBuildModalHasIsEntered: the registry does not know it, but
// Modal does, so it is recorded and used.
//
// @decision(DL-016)
func TestAnExactBuildModalHasIsEntered(t *testing.T) {
	_, exact := nodeSpecs(t)
	rf := newRegistryFixture(t, true, newImageFake(exact.Name()))
	if o, _ := rf.launchNode(t); o.Image != exact.Name() || o.Variant != nil {
		t.Fatalf("launched %s, build %v", o.Image, o.Variant)
	}
	got, err := rf.images.Get(context.Background(), exact.Name())
	if err != nil || got.ID != "im-"+exact.Name() || got.Kind != store.ImageVariant {
		t.Fatalf("entered %+v, %v", got, err)
	}
}

// TestALaunchUsesTheOlderBuildAndBuildsTheNewOneInTheBackground: a new
// patch is out, the launch boots the recorded older build at once, and
// the exact build becomes the newest for the next launch.
//
// @decision(DL-016)
func TestALaunchUsesTheOlderBuildAndBuildsTheNewOneInTheBackground(t *testing.T) {
	requested, exact := nodeSpecs(t)
	older := requested
	older.Addons, _ = toolchain.ResolveTo(requested.Addons, []string{"node@22.23.1"})
	modal := newImageFake(older.Name())
	modal.release = make(chan struct{})
	rf := newRegistryFixture(t, true, modal)
	_ = rf.images.Record(context.Background(), store.Image{
		Name: older.Name(), Kind: store.ImageVariant, RequestKey: requested.Name(),
		Versions: []string{"node@22.23.1"}, PublishedAt: rf.now.Add(-48 * time.Hour),
	})

	o, view := rf.launchNode(t)
	if o.Image != older.Name() || o.Variant != nil {
		t.Fatalf("launched %s, build %v; want the older build at once", o.Image, o.Variant)
	}
	if !slices.Equal(view.InstalledAddons, []string{"node@22.23.1"}) || toolchain.Installed(o.Addons)[0] != "node@22.23.1" {
		t.Errorf("installed %v, guide %v", view.InstalledAddons, toolchain.Installed(o.Addons))
	}
	close(modal.release)
	waitFor(t, "the background build to be recorded", func() bool {
		n, err := rf.images.Newest(context.Background(), requested.Name())
		return err == nil && n.Name == exact.Name()
	})
	if o, _ := rf.launchNode(t); o.Image != exact.Name() {
		t.Fatalf("the next launch booted %s, want the new build", o.Image)
	}
}

// TestWithoutOnDemandBuildsALaunchStaysOnTheOlderBuild.
//
// @decision(DL-016)
func TestWithoutOnDemandBuildsALaunchStaysOnTheOlderBuild(t *testing.T) {
	requested, _ := nodeSpecs(t)
	rf := newRegistryFixture(t, false, newImageFake())
	_ = rf.images.Record(context.Background(), store.Image{
		Name: "older", Kind: store.ImageVariant, RequestKey: requested.Name(),
		Versions: []string{"node@22.23.1"}, PublishedAt: rf.now,
	})
	if o, _ := rf.launchNode(t); o.Image != "older" {
		t.Fatalf("launched %s", o.Image)
	}
	time.Sleep(20 * time.Millisecond)
	if b := rf.modal.builds(); len(b) != 0 {
		t.Fatalf("built %v with on-demand builds off", b)
	}
}

// TestAnImageBuiltBeforeResolutionCountsAsAnOlderBuild: nothing is
// recorded, but the image named by the request as written is published.
//
// @decision(DL-016)
func TestAnImageBuiltBeforeResolutionCountsAsAnOlderBuild(t *testing.T) {
	requested, _ := nodeSpecs(t)
	rf := newRegistryFixture(t, false, newImageFake(requested.Name()))
	o, view := rf.launchNode(t)
	if o.Image != requested.Name() || o.Variant != nil {
		t.Fatalf("launched %s, build %v", o.Image, o.Variant)
	}
	if !slices.Equal(view.InstalledAddons, []string{"node@22"}) {
		t.Errorf("its versions are recorded as written: %v", view.InstalledAddons)
	}
	got, err := rf.images.Newest(context.Background(), requested.Name())
	if err != nil || got.Name != requested.Name() || !slices.Equal(got.Versions, []string{"node@22"}) {
		t.Fatalf("entered %+v, %v", got, err)
	}
}

// TestWithNothingBuiltTheExactBuildIsBuiltOnTheLaunchPath, the
// launch-path build of DL-016.
//
// @decision(DL-016)
func TestWithNothingBuiltTheExactBuildIsBuiltOnTheLaunchPath(t *testing.T) {
	_, exact := nodeSpecs(t)
	rf := newRegistryFixture(t, true, newImageFake())
	o, _ := rf.launchNode(t)
	if o.Image != exact.Name() || o.Variant == nil || o.Variant.Name() != exact.Name() || !o.BuildVariant || o.RebuildVariant {
		t.Fatalf("launched %s, build %v, rebuild %v", o.Image, o.Variant, o.RebuildVariant)
	}
}

// TestAPrunedExactBuildIsRebuilt: the registry says it was deleted, so
// its name resolving in Modal is not trusted.
//
// @decision(DL-016)
func TestAPrunedExactBuildIsRebuilt(t *testing.T) {
	_, exact := nodeSpecs(t)
	rf := newRegistryFixture(t, true, newImageFake(exact.Name()))
	ctx := context.Background()
	_ = rf.images.Record(ctx, VariantImage(exact))
	_ = rf.images.MarkDeleted(ctx, exact.Name(), rf.now)
	o, _ := rf.launchNode(t)
	if o.Variant == nil || !o.RebuildVariant {
		t.Fatalf("launched %s, build %v, rebuild %v", o.Image, o.Variant, o.RebuildVariant)
	}
}

// TestAPrunedBaseFailsTheLaunchAndNamesTheCommand: the base is never
// built on demand.
//
// @decision(DL-016)
func TestAPrunedBaseFailsTheLaunchAndNamesTheCommand(t *testing.T) {
	rf := newRegistryFixture(t, true, newImageFake())
	base := sandbox.ImageSpec{OpenCodeVersion: variantPin}.Name()
	ctx := context.Background()
	_ = rf.images.Record(ctx, store.Image{Name: base, Kind: store.ImageBase, PublishedAt: rf.now})
	_ = rf.images.MarkDeleted(ctx, base, rf.now)
	id := rf.create(repoReq("plain"))
	var out strings.Builder
	for _, e := range rf.events(id) {
		out.Write(e.Data)
	}
	if !strings.Contains(out.String(), "spov serve image build") || len(rf.launched()) != 0 {
		t.Fatalf("events %q, launched %d", out.String(), len(rf.launched()))
	}
}

// TestALaunchRecordsItsImagesUseAtMostHourly.
//
// @decision(DL-016)
func TestALaunchRecordsItsImagesUseAtMostHourly(t *testing.T) {
	rf := newRegistryFixture(t, true, newImageFake())
	base := sandbox.ImageSpec{OpenCodeVersion: variantPin}.Name()
	rf.events(rf.create(repoReq("one")))
	first, err := rf.images.Get(context.Background(), base)
	if err != nil || first.Kind != store.ImageBase {
		t.Fatalf("the base was not entered: %+v, %v", first, err)
	}
	rf.events(rf.create(repoReq("two")))
	if again, _ := rf.images.Get(context.Background(), base); !again.LastUsedAt.Equal(first.LastUsedAt) {
		t.Fatalf("a second launch within the hour wrote again: %v then %v", first.LastUsedAt, again.LastUsedAt)
	}
}

// TestALaunchWhoseImageVanishedMarksItForARebuild: the registry said the
// image existed, but another deployment on the same Modal environment
// deleted it. The failed launch marks it, and the next one rebuilds it.
//
// @decision(DL-016)
func TestALaunchWhoseImageVanishedMarksItForARebuild(t *testing.T) {
	_, exact := nodeSpecs(t)
	rf := newRegistryFixture(t, true, newImageFake(exact.Name()))
	ctx := context.Background()
	_ = rf.images.Record(ctx, VariantImage(exact))
	rf.launchErr = &sandbox.ImageNotFoundError{Name: exact.Name(), Err: errors.New("NotFound: image im-x not found")}
	req := repoReq("vanished")
	req.Addons = []string{"node@22"}
	var out strings.Builder
	for _, e := range rf.events(rf.create(req)) {
		out.Write(e.Data)
	}
	if !strings.Contains(out.String(), "next launch builds it again") {
		t.Fatalf("the failure does not say what happens next: %s", out.String())
	}
	if got, _ := rf.images.Get(ctx, exact.Name()); got.DeletedAt == nil {
		t.Fatal("the vanished image was not marked deleted")
	}
	if o, _ := rf.launchNode(t); o.Variant == nil || !o.RebuildVariant {
		t.Fatalf("the next launch: build %v, rebuild %v", o.Variant, o.RebuildVariant)
	}
}

// TestAPrunedVariantWithoutOnDemandBuildsNamesTheCommand: the launch
// would only meet Modal's NotFound, so it is refused first.
//
// @decision(DL-016)
func TestAPrunedVariantWithoutOnDemandBuildsNamesTheCommand(t *testing.T) {
	_, exact := nodeSpecs(t)
	rf := newRegistryFixture(t, false, newImageFake(exact.Name()))
	ctx := context.Background()
	_ = rf.images.Record(ctx, VariantImage(exact))
	_ = rf.images.MarkDeleted(ctx, exact.Name(), rf.now)
	req := repoReq("pruned")
	req.Addons = []string{"node@22"}
	var out strings.Builder
	for _, e := range rf.events(rf.create(req)) {
		out.Write(e.Data)
	}
	if !strings.Contains(out.String(), "spov serve image build --addons node@22") || len(rf.launched()) != 0 {
		t.Fatalf("events %s, launched %d", out.String(), len(rf.launched()))
	}
}

// TestABackgroundBuildNeedsEveryVersionResolvedAndNewer: an unresolved
// version would build the request as written, and a lagging answer an
// older patch than the one already built.
//
// @decision(DL-016)
func TestABackgroundBuildNeedsEveryVersionResolvedAndNewer(t *testing.T) {
	sel, _ := toolchain.Parse([]string{"node@22", "redis@7.4.6"})
	at := func(versions ...string) []toolchain.Selection {
		out, err := toolchain.ResolveTo(sel, versions)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	older := at("node@22.23.1")
	if !worthBuilding(at("node@22.23.3"), older) {
		t.Error("a newer patch should build")
	}
	if worthBuilding(sel, older) {
		t.Error("an unresolved partial version must not build")
	}
	if worthBuilding(at("node@22.23.0"), older) {
		t.Error("an older patch than the recorded build must not build")
	}
}

// TestAnUnbuiltBaseImageNamesTheBuildCommand: the first launch on a new
// Modal account or pin meets a base image nobody has built, and says how
// to build it.
//
// @decision(DL-035)
func TestAnUnbuiltBaseImageNamesTheBuildCommand(t *testing.T) {
	rf := newRegistryFixture(t, true, newImageFake())
	base := sandbox.ImageSpec{OpenCodeVersion: variantPin}.Name()
	rf.launchErr = &sandbox.ImageNotFoundError{Name: base, Err: errors.New("NotFound: image not found")}
	var out strings.Builder
	for _, e := range rf.events(rf.create(repoReq("first"))) {
		out.Write(e.Data)
	}
	if !strings.Contains(out.String(), "run `spov serve image build` once") {
		t.Fatalf("the failure does not name the build: %s", out.String())
	}
}
