package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/statefile"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/toolchain"
)

// buildingProvider is a Fake that can also build images, which is what
// the real Modal provider is. It records what it was asked to build.
type buildingProvider struct {
	*sandbox.Fake
	built   []sandbox.ImageSpec
	rebuilt []string
	deleted []string
	err     error
}

func (p *buildingProvider) BuildImage(ctx context.Context, spec sandbox.ImageSpec) (string, error) {
	p.rebuilt = append(p.rebuilt, spec.Name())
	return p.EnsureImage(ctx, spec)
}

func (p *buildingProvider) ImageID(_ context.Context, name string) (string, error) {
	return "im-" + name, nil
}

func (p *buildingProvider) DeleteImage(_ context.Context, id string) error {
	p.deleted = append(p.deleted, id)
	return nil
}

// pinResolver resolves every partial version to the release in exact.
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

func (p *buildingProvider) EnsureImage(_ context.Context, spec sandbox.ImageSpec) (string, error) {
	p.built = append(p.built, spec)
	if p.err != nil {
		return "", p.err
	}
	return spec.Name(), nil
}

func (p *buildingProvider) HasImage(_ context.Context, name string) (bool, error) {
	for _, s := range p.built {
		if s.Name() == name {
			return true, nil
		}
	}
	return false, nil
}

func newBuildingProvider() *buildingProvider {
	return &buildingProvider{Fake: sandbox.NewFake()}
}

// TestImageBuildUsesThePinnedVersion: the image a session asks for is
// named from the configured version, so the one this builds has to be
// named the same way or the build helps nobody.
//
// @decision(DL-032) @decision(DL-035)
func TestImageBuildUsesThePinnedVersion(t *testing.T) {
	p := newBuildingProvider()
	out, _, err := run(t, Env{Provider: p}, "serve", "image", "build")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.built) != 1 {
		t.Fatalf("built %d images, want 1", len(p.built))
	}
	if got := p.built[0].OpenCodeVersion; got != config.DefaultOpenCodeVersion {
		t.Errorf("built %q, want the pinned %q", got, config.DefaultOpenCodeVersion)
	}
	// The name is what a launch looks up, so it belongs in the output.
	want := sandbox.ImageSpec{OpenCodeVersion: config.DefaultOpenCodeVersion}.Name()
	if !strings.Contains(out, want) {
		t.Errorf("output %q does not name the image %q", out, want)
	}
}

// TestImageBuildMatchesWhatALaunchWouldAskFor. The two names are
// computed in different places — imageName() for a launch, this command
// for the build — and a divergence would publish an image nothing ever
// looks up, which is invisible until a launch fails.
//
// @decision(DL-032)
func TestImageBuildMatchesWhatALaunchWouldAskFor(t *testing.T) {
	p := newBuildingProvider()
	if _, _, err := run(t, Env{Provider: p}, "serve", "image", "build"); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.MapEnv{Vars: map[string]string{}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := p.built[0].Name(), imageName(cfg.Config); got != want {
		t.Errorf("built %q, but a launch would ask for %q", got, want)
	}
}

// TestImageBuildCanPrepareANewPin, which is the whole point: the image
// has to exist before the version that names it is switched on.
//
// @decision(DL-032) @decision(DL-035)
func TestImageBuildCanPrepareANewPin(t *testing.T) {
	p := newBuildingProvider()
	_, _, err := run(t, Env{Provider: p}, "serve", "image", "build",
		"--opencode-version", "0.0.0-beta-99999")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.built[0].OpenCodeVersion; got != "0.0.0-beta-99999" {
		t.Errorf("built %q, want the requested version", got)
	}
}

// TestImageBuildRejectsABadVersionBeforeBuilding: a build is minutes
// long, so a name that could never be launched must fail immediately
// rather than after it.
//
// @decision(DL-032)
func TestImageBuildRejectsABadVersionBeforeBuilding(t *testing.T) {
	p := newBuildingProvider()
	_, _, err := run(t, Env{Provider: p}, "serve", "image", "build",
		"--opencode-version", "not a version")
	if err == nil {
		t.Fatal("an invalid version was accepted")
	}
	if len(p.built) != 0 {
		t.Errorf("the provider was asked to build %v", p.built)
	}
}

// An operator building for a deployment gets that deployment's pin
// rather than the embedded one, from the user file: `spov serve` and its
// subcommands read no project file.
//
// @decision(DL-032) @decision(DL-026)
func TestImageBuildReadsTheUserConfigAndNoProjectFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, config.ProjectFileName),
		[]byte("opencode:\n  version: 0.0.0-beta-99999\nserver:\n  url: https://evil.example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	environ := userConfigEnv(t, "opencode:\n  version: 0.0.0-beta-12345\n")
	p := newBuildingProvider()
	if _, _, err := run(t, Env{Provider: p, Workdir: dir, Environ: environ}, "serve", "image", "build"); err != nil {
		t.Fatal(err)
	}
	if got := p.built[0].OpenCodeVersion; got != "0.0.0-beta-12345" {
		t.Errorf("built %q, want the user file's version", got)
	}
}

// TestImageBuildOnAProviderThatCannotBuildSaysSo rather than panicking
// on a failed type assertion.
//
// @decision(DL-032)
func TestImageBuildOnAProviderThatCannotBuildSaysSo(t *testing.T) {
	_, _, err := run(t, Env{Provider: sandbox.NewFake()}, "serve", "image", "build")
	if err == nil {
		t.Fatal("a provider with no build support succeeded")
	}
	if !strings.Contains(err.Error(), "cannot build images") {
		t.Errorf("error %q does not explain the provider cannot build", err)
	}
}

// TestImageIsUnderServe keeps it with the other operator commands: it
// needs the server's Modal credentials, not a user's.
//
// @decision(DL-032)
func TestImageIsUnderServe(t *testing.T) {
	out, _, err := run(t, Env{}, "serve", "--help")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "image") {
		t.Error("serve --help does not mention image")
	}
}

// TestImageBuildPrebuildsAVariant: --addons and --apt build the image a
// launch with the same add-ons asks for, and a bad add-on is refused
// before the provider is touched.
//
// @decision(DL-033) @decision(DL-016)
func TestImageBuildPrebuildsAVariant(t *testing.T) {
	p := newBuildingProvider()
	out, _, err := run(t, Env{Provider: p, ResolveAddons: pinResolver{}}, "serve", "image", "build", "--addons", "rust,java@17", "--apt", "jq")
	if err != nil {
		t.Fatal(err)
	}
	sel, _ := toolchain.Parse([]string{"java@17", "rust"})
	want := sandbox.ImageSpec{OpenCodeVersion: config.DefaultOpenCodeVersion, Addons: sel, ExtraPackages: []string{"jq"}}
	if len(p.built) != 1 || p.built[0].Name() != want.Name() || !strings.Contains(out, want.Name()) {
		t.Errorf("built %v, output %q; want %s", p.built, out, want.Name())
	}

	p = newBuildingProvider()
	if _, _, err := run(t, Env{Provider: p}, "serve", "image", "build", "--addons", "docker"); err == nil || len(p.built) != 0 {
		t.Errorf("an unknown add-on: %v after %d builds", err, len(p.built))
	}
}

// TestImageBuildBuildsTheResolvedVersionsAndRecordsThem: the build is of
// what the add-ons resolve to, the output names those versions, and the
// state file records it as the newest build of the request.
//
// @decision(DL-033) @decision(DL-016)
func TestImageBuildBuildsTheResolvedVersionsAndRecordsThem(t *testing.T) {
	p := newBuildingProvider()
	dir := t.TempDir()
	env := Env{Provider: p, StateDir: dir, ResolveAddons: pinResolver{"node": "22.23.3", "postgres": "14.24"}}
	out, _, err := run(t, env, "serve", "image", "build", "--addons", "node@22,postgres@14")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "add-ons: node@22.23.3, postgres@14.24") {
		t.Errorf("the output does not name the resolved versions: %q", out)
	}
	if len(p.built) != 1 || p.built[0].Addons[0].Resolved != "22.23.3" {
		t.Fatalf("built %+v", p.built)
	}
	reg := &statefile.File{Path: filepath.Join(dir, statefile.Name)}
	got, err := reg.Newest(context.Background(), p.built[0].Requested().Name())
	if err != nil || got.Name != p.built[0].Name() || got.ID != "im-"+got.Name ||
		strings.Join(got.Versions, ",") != "node@22.23.3,postgres@14.24" {
		t.Fatalf("recorded %+v, %v", got, err)
	}

	// The base image is recorded as a base.
	if _, _, err := run(t, Env{Provider: p, StateDir: dir}, "serve", "image", "build"); err != nil {
		t.Fatal(err)
	}
	base, err := reg.Get(context.Background(), sandbox.ImageSpec{OpenCodeVersion: config.DefaultOpenCodeVersion}.Name())
	if err != nil || base.Kind != store.ImageBase {
		t.Fatalf("base: %+v, %v", base, err)
	}
}

// TestImageBuildRebuildsAPrunedName: the registry says the image was
// deleted, so the build does not trust the name resolving.
//
// @decision(DL-016)
func TestImageBuildRebuildsAPrunedName(t *testing.T) {
	p := newBuildingProvider()
	dir := t.TempDir()
	name := sandbox.ImageSpec{OpenCodeVersion: config.DefaultOpenCodeVersion}.Name()
	reg := &statefile.File{Path: filepath.Join(dir, statefile.Name)}
	ctx := context.Background()
	_ = reg.Record(ctx, store.Image{Name: name, Kind: store.ImageBase, PublishedAt: time.Now().Add(-60 * 24 * time.Hour)})
	_ = reg.MarkDeleted(ctx, name, time.Now())
	if _, _, err := run(t, Env{Provider: p, StateDir: dir}, "serve", "image", "build"); err != nil {
		t.Fatal(err)
	}
	if len(p.rebuilt) != 1 || p.rebuilt[0] != name {
		t.Fatalf("rebuilt %v", p.rebuilt)
	}
	if got, _ := reg.Get(ctx, name); got.DeletedAt != nil {
		t.Fatalf("the rebuilt image is still marked deleted: %+v", got)
	}
	// --rebuild forces it for a name the registry does not know was
	// deleted, as when another deployment pruned it.
	if _, _, err := run(t, Env{Provider: p, StateDir: t.TempDir()}, "serve", "image", "build", "--rebuild"); err != nil {
		t.Fatal(err)
	}
	if len(p.rebuilt) != 2 {
		t.Fatalf("rebuilt %v", p.rebuilt)
	}
}

// TestImagePruneListsThenDeletesStaleImages.
//
// @decision(DL-016)
func TestImagePruneListsThenDeletesStaleImages(t *testing.T) {
	p := newBuildingProvider()
	dir := t.TempDir()
	reg := &statefile.File{Path: filepath.Join(dir, statefile.Name)}
	ctx := context.Background()
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	current := sandbox.ImageSpec{OpenCodeVersion: config.DefaultOpenCodeVersion}.Name()
	for _, img := range []store.Image{
		{Name: current, Kind: store.ImageBase, PublishedAt: now.Add(-90 * 24 * time.Hour)},
		{Name: "spov-base-old", Kind: store.ImageBase, PublishedAt: now.Add(-40 * 24 * time.Hour)},
		{Name: "spov-base-recent", Kind: store.ImageBase, PublishedAt: now.Add(-2 * 24 * time.Hour)},
	} {
		_ = reg.Record(ctx, img)
	}
	env := Env{Provider: p, StateDir: dir, Now: func() time.Time { return now }}
	out, _, err := run(t, env, "serve", "image", "prune", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "would delete spov-base-old") || strings.Contains(out, current) || strings.Contains(out, "recent") || len(p.deleted) != 0 {
		t.Fatalf("dry run: %q, deleted %v", out, p.deleted)
	}
	if out, _, err = run(t, env, "serve", "image", "prune"); err != nil {
		t.Fatal(err)
	}
	if len(p.deleted) != 1 || p.deleted[0] != "im-spov-base-old" || !strings.Contains(out, "deleted spov-base-old") {
		t.Fatalf("prune: %q, deleted %v", out, p.deleted)
	}
	if got, _ := reg.Get(ctx, "spov-base-old"); got.DeletedAt == nil {
		t.Fatal("the deleted image is not marked deleted")
	}
	if at, _ := reg.PrunedAt(ctx); !at.Equal(now) {
		t.Fatalf("pruned at %v", at)
	}
	if out, _, _ = run(t, env, "serve", "image", "prune"); !strings.Contains(out, "nothing to prune") {
		t.Fatalf("second prune: %q", out)
	}
}
