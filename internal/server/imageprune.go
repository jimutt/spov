package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jimutt/spov/internal/store"
)

// How long an image may go unused before pruning deletes it.
//
// @decision(DL-016)
const (
	// ReplacedVariantRetention is for a variant a newer build of the
	// same request has replaced.
	ReplacedVariantRetention = 7 * 24 * time.Hour
	// NewestVariantRetention is for the newest build of a request, which
	// is deleted only while variants are built on demand.
	NewestVariantRetention = 30 * 24 * time.Hour
	// OldBaseRetention is for a base image of any pin but the current.
	OldBaseRetention = 30 * 24 * time.Hour
	// OldProxyRetention is for a proxy image of any binary but the
	// current.
	OldProxyRetention = 7 * 24 * time.Hour
	// PruneInterval is how often pruning runs.
	PruneInterval = 24 * time.Hour
	// pruneCheckInterval is how often the prune loop asks whether a
	// prune is due.
	pruneCheckInterval = time.Hour
)

// PrunePolicy is what pruning decides by.
//
// @decision(DL-016)
type PrunePolicy struct {
	Now time.Time
	// CurrentBase and CurrentProxy are never deleted.
	CurrentBase, CurrentProxy string
	// InUse are the images live sessions were created from.
	InUse map[string]bool
	// BuildOnDemand allows deleting a request's newest variant, which a
	// launch can then build again.
	BuildOnDemand bool
}

// Prunable picks the registry entries pruning deletes, with why.
//
// @decision(DL-016)
func Prunable(images []store.Image, p PrunePolicy) []PruneCandidate {
	replaced := map[string]bool{}
	newest := map[string]store.Image{}
	for _, img := range images {
		if img.Kind != store.ImageVariant || img.DeletedAt != nil {
			continue
		}
		if n, ok := newest[img.RequestKey]; !ok || img.PublishedAt.After(n.PublishedAt) {
			newest[img.RequestKey] = img
		}
	}
	for _, img := range images {
		if img.Kind == store.ImageVariant && img.DeletedAt == nil && newest[img.RequestKey].Name != img.Name {
			replaced[img.Name] = true
		}
	}
	var out []PruneCandidate
	for _, img := range images {
		if img.DeletedAt != nil || p.InUse[img.Name] || img.Name == p.CurrentBase || img.Name == p.CurrentProxy {
			continue
		}
		unused := p.Now.Sub(img.LastUsedAt)
		var limit time.Duration
		var why string
		switch {
		case img.Kind == store.ImageVariant && replaced[img.Name]:
			limit, why = ReplacedVariantRetention, "a newer build of the same add-ons replaced it"
		case img.Kind == store.ImageVariant && p.CurrentBase != "" && !strings.HasPrefix(img.Name, p.CurrentBase+"-"):
			limit, why = OldBaseRetention, "it is built on an older pin's base"
		case img.Kind == store.ImageVariant && p.BuildOnDemand:
			limit, why = NewestVariantRetention, "a launch builds it again if asked"
		case img.Kind == store.ImageBase:
			limit, why = OldBaseRetention, "it is not the current pin's base"
		case img.Kind == store.ImageProxy:
			limit, why = OldProxyRetention, "it is not the current proxy"
		default:
			continue
		}
		if unused >= limit {
			out = append(out, PruneCandidate{Image: img, Why: fmt.Sprintf("unused for %d days; %s", int(unused.Hours()/24), why)})
		}
	}
	return out
}

// PruneCandidate is an image pruning deletes, and why.
type PruneCandidate struct {
	Image store.Image
	Why   string
}

// ImageDeleter deletes published images.
//
// @decision(DL-016)
type ImageDeleter interface {
	ImageID(ctx context.Context, name string) (string, error)
	DeleteImage(ctx context.Context, id string) error
}

// pruneTimeout bounds marking and deleting one image, which carries on
// past a cancelled prune so the two never part.
const pruneTimeout = time.Minute

// PruneImages marks what Prunable picks deleted in the registry and then
// deletes it, or with dryRun only lists them. The mark comes first: an
// image marked but not deleted is only rebuilt, while one deleted but not
// marked would be launched and fail. One that fails is reported and the
// rest go on.
//
// @decision(DL-016)
func PruneImages(ctx context.Context, reg store.ImageStore, del ImageDeleter, p PrunePolicy, dryRun bool) ([]PruneCandidate, error) {
	images, err := reg.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list the image registry: %w", err)
	}
	picked := Prunable(images, p)
	if dryRun {
		return picked, nil
	}
	var done []PruneCandidate
	var errs []error
	for _, c := range picked {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		if err := pruneOne(context.WithoutCancel(ctx), reg, del, c.Image, p.Now); err != nil {
			errs = append(errs, err)
			continue
		}
		done = append(done, c)
	}
	return done, errors.Join(errs...)
}

func pruneOne(ctx context.Context, reg store.ImageStore, del ImageDeleter, img store.Image, now time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, pruneTimeout)
	defer cancel()
	id := img.ID
	if id == "" {
		var err error
		if id, err = del.ImageID(ctx, img.Name); err != nil {
			return err
		}
	}
	if err := reg.MarkDeleted(ctx, img.Name, now); err != nil {
		return fmt.Errorf("mark %s deleted: %w", img.Name, err)
	}
	return del.DeleteImage(ctx, id)
}

// PruneImages prunes this deployment's images, sparing the current base
// and proxy and every image a live session was created from.
//
// @decision(DL-016)
func (s *Service) PruneImages(ctx context.Context, dryRun bool) ([]PruneCandidate, error) {
	if s.deps.Images == nil || s.deps.Builder == nil {
		return nil, errors.New("this server keeps no image registry or cannot delete images")
	}
	live, err := s.deps.Store.Sessions().ListLive(ctx)
	if err != nil {
		return nil, fmt.Errorf("list live sessions: %w", err)
	}
	inUse := map[string]bool{}
	for _, sess := range live {
		inUse[sess.Booted.Image] = true
		inUse[sess.Request.Image] = true
	}
	return PruneImages(ctx, s.deps.Images, s.deps.Builder, PrunePolicy{
		Now: s.deps.Now(), CurrentBase: s.baseImage(), CurrentProxy: s.deps.ProxyImageName,
		InUse: inUse, BuildOnDemand: s.deps.Defaults.BuildOnDemand,
	}, dryRun)
}

// RunImagePrune prunes once PruneInterval has passed since the last
// prune, checking at start and every pruneCheckInterval until ctx ends.
// It runs where the reconciler runs.
//
// @decision(DL-016)
func (s *Service) RunImagePrune(ctx context.Context) {
	if s.deps.Images == nil || s.deps.Builder == nil {
		return
	}
	t := time.NewTicker(pruneCheckInterval)
	defer t.Stop()
	for {
		s.pruneIfDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) pruneIfDue(ctx context.Context) {
	last, err := s.deps.Images.PrunedAt(ctx)
	if err != nil {
		s.deps.Warn(fmt.Sprintf("image prune: %v", err))
		return
	}
	now := s.deps.Now()
	if now.Sub(last) < PruneInterval {
		return
	}
	// Stamped first, so a prune that keeps failing is not retried every
	// check.
	if err := s.deps.Images.SetPrunedAt(ctx, now); err != nil {
		s.deps.Warn(fmt.Sprintf("image prune: %v", err))
		return
	}
	done, err := s.PruneImages(ctx, false)
	for _, c := range done {
		s.deps.Warn(fmt.Sprintf("pruned image %s (%s)", c.Image.Name, c.Why))
	}
	if err != nil {
		s.deps.Warn(fmt.Sprintf("image prune: %v", err))
	}
}
