package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jimutt/spov/internal/addonindex"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/telemetry"
	"github.com/jimutt/spov/internal/toolchain"
)

// variantOf is the recipe of the image a request with add-ons or extra
// packages boots: the deployment's pinned base plus what it asks for.
// ok is false for a request that asks for neither, or whose add-ons do
// not parse.
//
// @decision(DL-033)
func (s *Service) variantOf(req store.SessionRequest) (sandbox.ImageSpec, bool) {
	sel, err := toolchain.Parse(req.Addons)
	apt := sandbox.ExtraPackages(req.Apt)
	if err != nil || (len(sel) == 0 && len(apt) == 0) {
		return sandbox.ImageSpec{}, false
	}
	return sandbox.ImageSpec{
		OpenCodeVersion: s.deps.Defaults.OpenCodeVersion,
		ExtraPackages:   apt,
		Addons:          sel,
	}, true
}

// baseImage is the name of the current pin's base image.
func (s *Service) baseImage() string {
	return sandbox.ImageSpec{OpenCodeVersion: s.deps.Defaults.OpenCodeVersion}.Name()
}

// variantProblems refuses add-ons or extra packages that cannot be built:
// an unknown add-on or version, a bad package name, or an image spov did
// not build, whether the request named it or the deployment boots one.
//
// @decision(DL-033)
func (s *Service) variantProblems(req store.SessionRequest) []string {
	if len(req.Addons) == 0 && len(req.Apt) == 0 {
		return nil
	}
	var problems []string
	if _, err := toolchain.Parse(req.Addons); err != nil {
		problems = append(problems, err.Error())
	}
	d := s.deps.Defaults
	if err := sandbox.ValidatePackages(req.Apt); err != nil {
		problems = append(problems, err.Error())
	}
	if len(problems) > 0 {
		return problems
	}
	spec, ok := s.variantOf(req)
	if !ok {
		// Only packages the base already has: the base image.
		return nil
	}
	if d.Image != s.baseImage() {
		return []string{"this deployment boots its own image (sandbox.image), which takes no add-ons or apt packages"}
	}
	if req.Image != spec.Name() {
		return []string{fmt.Sprintf("image %q names an image spov did not build, so it cannot take add-ons or apt packages", req.Image)}
	}
	return nil
}

// variantPick is the image a launch boots and what it installs.
//
// @decision(DL-016)
type variantPick struct {
	image string
	// addons are the image's add-ons at the versions it installs.
	addons []toolchain.Selection
	// build is the recipe to build on the launch path when the image is
	// not published (DL-016), and rebuild says its name still resolves
	// although the registry says it was deleted (DL-016).
	build   *sandbox.ImageSpec
	rebuild bool
}

// imageUseInterval is how often a launch writes an image's last use.
//
// @decision(DL-016)
const imageUseInterval = time.Hour

// pickVariant decides which image a launch with add-ons or extra
// packages boots: the exact build of its resolved versions when it is
// published, else the newest build of the same request while the exact
// one builds in the background, else the exact build on the launch path.
// Without a registry it is the variant the request names. owner is the
// user whose launch this is, to whom a background build counts (DL-134).
//
// @decision(DL-033) @decision(DL-016) @decision(DL-134)
func (s *Service) pickVariant(ctx context.Context, owner string, requested sandbox.ImageSpec, note func(string)) variantPick {
	key := requested.Name()
	exact := requested
	if s.deps.Resolver != nil {
		exact.Addons = s.deps.Resolver.Resolve(ctx, requested.Addons)
	}
	name := exact.Name()
	onLaunchPath := variantPick{image: name, addons: exact.Addons, build: &exact}
	reg := s.deps.Images
	if reg == nil {
		return onLaunchPath
	}
	entry, known := s.imageEntry(ctx, name)
	switch {
	case known && entry.DeletedAt == nil:
		s.useImage(ctx, VariantImage(exact))
		return variantPick{image: name, addons: exact.Addons}
	case known:
		onLaunchPath.rebuild = true
	case s.published(ctx, name):
		s.RecordVariant(ctx, exact)
		return variantPick{image: name, addons: exact.Addons}
	}
	older, ok := s.olderBuild(ctx, requested, key, name)
	if !ok {
		return onLaunchPath
	}
	sel, err := toolchain.ResolveTo(requested.Addons, older.Versions)
	if err != nil {
		s.deps.Warn(fmt.Sprintf("the recorded build %s does not match its request: %v", older.Name, err))
		return onLaunchPath
	}
	s.useImage(ctx, older)
	if s.deps.Defaults.BuildOnDemand && s.deps.Variants != nil && worthBuilding(exact.Addons, sel) {
		if s.deps.Variants.Start(ctx, owner, exact, onLaunchPath.rebuild) {
			note(fmt.Sprintf("using the image built with %s; %s is building for later launches",
				strings.Join(older.Versions, ", "), strings.Join(toolchain.Installed(exact.Addons), ", ")))
		}
	}
	return variantPick{image: older.Name, addons: sel}
}

// worthBuilding reports whether the exact build should replace the older
// one in the background: every partial version resolved, since an
// unresolved one would build the request as written, and none resolved
// to a release older than the older build has, as a replica whose cached
// answer lags would.
//
// @decision(DL-016)
func worthBuilding(exact, older []toolchain.Selection) bool {
	have := map[string]string{}
	for _, o := range older {
		have[o.Name] = o.Exact()
	}
	for _, e := range exact {
		if _, partial := e.Index(); partial && e.Resolved == "" {
			return false
		}
		if v, ok := have[e.Name]; ok && v != e.Exact() && addonindex.Newer(v, e.Exact()) {
			return false
		}
	}
	return true
}

// olderBuild is the newest recorded build of the request, or the image
// named by the request as written when nothing is recorded and it is
// published: an image built before versions were resolved, entered now
// with its versions as written.
//
// @decision(DL-016)
func (s *Service) olderBuild(ctx context.Context, requested sandbox.ImageSpec, key, name string) (store.Image, bool) {
	img, legacy, ok := s.olderCandidate(ctx, requested, key, name)
	if ok && legacy {
		s.useImage(ctx, img)
	}
	return img, ok
}

// olderCandidate is olderBuild's choice without its write. legacy is set
// for the image named by the request as written.
func (s *Service) olderCandidate(ctx context.Context, requested sandbox.ImageSpec, key, name string) (img store.Image, legacy, ok bool) {
	newest, err := s.deps.Images.Newest(ctx, key)
	if err == nil && newest.Name != name {
		return newest, false, true
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.deps.Warn(fmt.Sprintf("read the image registry: %v", err))
	}
	if key == name {
		return store.Image{}, false, false
	}
	if _, known := s.imageEntry(ctx, key); known || !s.published(ctx, key) {
		return store.Image{}, false, false
	}
	return VariantImage(requested), true, true
}

// refuseBuildOverLimit refuses, before anything exists, a create whose
// launch would have to build an image on its own path while the owner has
// no new build left in their window. A launch that boots an image that
// exists, or an older build while the exact one builds in the background,
// is not refused: the background build is skipped past the limit instead.
// The launch checks the limit again when the build starts.
//
// @decision(DL-134) @decision(DL-016)
func (s *Service) refuseBuildOverLimit(ctx context.Context, ownerID string, req store.SessionRequest) error {
	if s.deps.Variants == nil || !s.deps.Defaults.BuildOnDemand {
		return nil
	}
	spec, ok := s.variantOf(req)
	if !ok {
		return nil
	}
	err := s.deps.Variants.Allow(attributionUser(ownerID))
	if err == nil || !s.needsLaunchBuild(ctx, spec) {
		return nil
	}
	s.deps.Metrics.QuotaRefused(ctx, telemetry.QuotaImageBuilds)
	return fmt.Errorf("%w: %s", ErrQuotaExceeded, err.Error())
}

// needsLaunchBuild reports whether a launch of requested would build its
// image on the launch path, as pickVariant would decide, without
// recording anything.
func (s *Service) needsLaunchBuild(ctx context.Context, requested sandbox.ImageSpec) bool {
	exact := requested
	if s.deps.Resolver != nil {
		exact.Addons = s.deps.Resolver.Resolve(ctx, requested.Addons)
	}
	name := exact.Name()
	if s.deps.Images == nil {
		return !s.published(ctx, name)
	}
	entry, known := s.imageEntry(ctx, name)
	if known && entry.DeletedAt == nil {
		return false
	}
	if !known && s.published(ctx, name) {
		return false
	}
	older, _, ok := s.olderCandidate(ctx, requested, requested.Name(), name)
	if !ok {
		return true
	}
	_, err := toolchain.ResolveTo(requested.Addons, older.Versions)
	return err != nil
}

// imageEntry is the registry's entry for name. A failed read is treated
// as no entry, so the launch falls back to asking Modal.
func (s *Service) imageEntry(ctx context.Context, name string) (store.Image, bool) {
	img, err := s.deps.Images.Get(ctx, name)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.deps.Warn(fmt.Sprintf("read the image registry: %v", err))
	}
	return img, err == nil
}

// published asks Modal whether name is published, for a name the
// registry does not know.
func (s *Service) published(ctx context.Context, name string) bool {
	if s.deps.Builder == nil {
		return false
	}
	ok, err := s.deps.Builder.HasImage(ctx, name)
	if err != nil {
		s.deps.Warn(fmt.Sprintf("look up image %s: %v", name, err))
	}
	return ok
}

// VariantImage is spec's registry entry, without its ID or times.
//
// @decision(DL-016)
func VariantImage(spec sandbox.ImageSpec) store.Image {
	return store.Image{
		Name: spec.Name(), Kind: store.ImageVariant,
		RequestKey: spec.Requested().Name(), Versions: toolchain.Installed(spec.Addons),
	}
}

// RecordVariant enters spec's image as published now, the newest build
// of its request. The launcher's VariantBuilder calls it after every
// build.
//
// @decision(DL-016)
func (s *Service) RecordVariant(ctx context.Context, spec sandbox.ImageSpec) {
	s.RecordImage(ctx, VariantImage(spec))
}

// RecordImage enters img as published now, with its Modal ID.
//
// @decision(DL-016)
func (s *Service) RecordImage(ctx context.Context, img store.Image) {
	if s.deps.Images == nil {
		return
	}
	if s.deps.Builder != nil && img.ID == "" {
		if id, err := s.deps.Builder.ImageID(ctx, img.Name); err == nil {
			img.ID = id
		}
	}
	img.PublishedAt = s.deps.Now()
	if err := s.deps.Images.Record(ctx, img); err != nil {
		s.deps.Warn(fmt.Sprintf("record image %s: %v", img.Name, err))
	}
	s.mu.Lock()
	s.imageUses[img.Name] = img.PublishedAt
	s.mu.Unlock()
}

// UseImage records that a launch used img, at most once an hour per
// image in this process.
//
// @decision(DL-016)
func (s *Service) UseImage(ctx context.Context, img store.Image) {
	s.useImage(ctx, img)
}

func (s *Service) useImage(ctx context.Context, img store.Image) {
	if s.deps.Images == nil || img.Name == "" {
		return
	}
	now := s.deps.Now()
	s.mu.Lock()
	last, ok := s.imageUses[img.Name]
	s.mu.Unlock()
	if ok && now.Sub(last) < imageUseInterval {
		return
	}
	if err := s.deps.Images.Use(ctx, img, now); err != nil {
		s.deps.Warn(fmt.Sprintf("record the use of image %s: %v", img.Name, err))
		return
	}
	s.mu.Lock()
	s.imageUses[img.Name] = now
	s.mu.Unlock()
}

// ImageDeleted reports whether the registry says pruning deleted name.
//
// @decision(DL-016)
func (s *Service) ImageDeleted(ctx context.Context, name string) bool {
	if s.deps.Images == nil {
		return false
	}
	img, known := s.imageEntry(ctx, name)
	return known && img.DeletedAt != nil
}

// imageGone marks a launch's image deleted when the sandbox could not be
// created because the image no longer exists, though the registry said
// it did: another deployment on the same Modal environment pruned it, or
// a prune's mark was lost. The next launch then builds the variant again,
// or names the command that rebuilds a base. err is returned with what
// happens next.
//
// @decision(DL-016)
func (s *Service) imageGone(ctx context.Context, pick variantPick, err error) error {
	var nf *sandbox.ImageNotFoundError
	if !errors.As(err, &nf) || nf.Name != pick.image {
		return err
	}
	if s.deps.Images == nil {
		return s.unbuiltBase(pick, err)
	}
	img, known := s.imageEntry(ctx, pick.image)
	if !known || img.DeletedAt != nil {
		return s.unbuiltBase(pick, err)
	}
	if merr := s.deps.Images.MarkDeleted(ctx, pick.image, s.deps.Now()); merr != nil {
		s.deps.Warn(fmt.Sprintf("mark image %s deleted: %v", pick.image, merr))
		return s.unbuiltBase(pick, err)
	}
	if img.Kind == store.ImageBase {
		return s.unbuiltBase(pick, err)
	}
	return fmt.Errorf("%w; the image no longer exists, so the next launch builds it again", err)
}

// unbuiltBase names the build for a base image Modal does not have: it is
// built deliberately, never by a launch, so a first launch on a new Modal
// account or OpenCode pin meets this as well as a pruned one.
//
// @decision(DL-035) @decision(DL-016)
func (s *Service) unbuiltBase(pick variantPick, err error) error {
	if pick.image != s.baseImage() {
		return err
	}
	return fmt.Errorf("%w; the sandbox image for this OpenCode version is not on Modal: run `spov serve image build` once (it takes a few minutes; `spov init` does it for a local setup)", err)
}
