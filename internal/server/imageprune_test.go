package server

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/store"
)

const day = 24 * time.Hour

// TestPrunableFollowsTheRetentions: each kind of image goes after its
// own time unused, and the current base and proxy and anything in use
// never go.
//
// @decision(DL-016)
func TestPrunableFollowsTheRetentions(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	img := func(name string, kind store.ImageKind, key string, published, unused time.Duration) store.Image {
		return store.Image{Name: name, Kind: kind, RequestKey: key, PublishedAt: now.Add(-published), LastUsedAt: now.Add(-unused)}
	}
	deleted := img("base-current-v-deleted", store.ImageVariant, "a", 90*day, 90*day)
	deleted.DeletedAt = &now
	images := []store.Image{
		img("base-current-v-a-old", store.ImageVariant, "a", 20*day, 8*day),    // replaced, past 7 days
		img("base-current-v-a-mid", store.ImageVariant, "a", 10*day, 6*day),    // replaced, within 7 days
		img("base-current-v-a-new", store.ImageVariant, "a", 1*day, 31*day),    // newest, past 30 days
		img("base-current-v-b-new", store.ImageVariant, "b", 40*day, 29*day),   // newest, within 30 days
		img("base-current-v-c-inuse", store.ImageVariant, "c", 90*day, 90*day), // a live session's
		img("base-old-v1", store.ImageVariant, "d", 90*day, 31*day),            // an older pin's variant
		img("base-old", store.ImageBase, "", 90*day, 31*day),
		img("base-current", store.ImageBase, "", 90*day, 90*day),
		img("proxy-old", store.ImageProxy, "", 90*day, 8*day),
		img("proxy-current", store.ImageProxy, "", 90*day, 90*day),
		deleted,
	}
	policy := PrunePolicy{
		Now: now, CurrentBase: "base-current", CurrentProxy: "proxy-current",
		InUse: map[string]bool{"base-current-v-c-inuse": true}, BuildOnDemand: true,
	}
	names := func(cs []PruneCandidate) []string {
		var out []string
		for _, c := range cs {
			out = append(out, c.Image.Name)
		}
		slices.Sort(out)
		return out
	}
	got := names(Prunable(images, policy))
	want := []string{"base-current-v-a-new", "base-current-v-a-old", "base-old", "base-old-v1", "proxy-old"}
	if !slices.Equal(got, want) {
		t.Fatalf("pruned %v, want %v", got, want)
	}

	policy.BuildOnDemand = false
	got = names(Prunable(images, policy))
	if slices.Contains(got, "base-current-v-a-new") || !slices.Contains(got, "base-current-v-a-old") || !slices.Contains(got, "base-old-v1") {
		t.Fatalf("without on-demand builds the newest of a request stays, and a replaced one still goes: %v", got)
	}
	for _, c := range Prunable(images, policy) {
		if !strings.Contains(c.Why, "unused for") {
			t.Errorf("%s: %q", c.Image.Name, c.Why)
		}
	}
}

// TestPruneImagesGoesOnPastAFailedDelete: one image that cannot be
// deleted is reported, and the rest go on. Each is marked before its
// delete.
//
// @decision(DL-016)
func TestPruneImagesGoesOnPastAFailedDelete(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	reg := store.NewMemoryImages()
	for _, n := range []string{"base-a", "base-b"} {
		_ = reg.Record(ctx, store.Image{Name: n, Kind: store.ImageBase, PublishedAt: now.Add(-60 * day)})
	}
	modal := newImageFake()
	modal.deleteErr = map[string]error{"im-base-a": errors.New("modal said no")}
	done, err := PruneImages(ctx, reg, modal, PrunePolicy{Now: now, CurrentBase: "base-current"}, false)
	if err == nil || !strings.Contains(err.Error(), "modal said no") {
		t.Fatalf("err %v", err)
	}
	if len(done) != 1 || done[0].Image.Name != "base-b" {
		t.Fatalf("done %+v", done)
	}
	// Marked first: an image marked but not deleted is only rebuilt.
	if a, _ := reg.Get(ctx, "base-a"); a.DeletedAt == nil {
		t.Error("an image that failed to delete was not marked first")
	}
	if b, _ := reg.Get(ctx, "base-b"); b.DeletedAt == nil {
		t.Error("the deleted image was not marked")
	}
}

// TestTheServerSparesLiveSessionsAndPrunesDaily: an image a live session
// booted from is kept however stale, and the prune loop runs again only
// a day after the last.
//
// @decision(DL-016)
func TestTheServerSparesLiveSessionsAndPrunesDaily(t *testing.T) {
	_, exact := nodeSpecs(t)
	rf := newRegistryFixture(t, true, newImageFake(exact.Name()))
	ctx := context.Background()
	_ = rf.images.Record(ctx, VariantImage(exact))
	rf.launchNode(t)

	now := time.Now().Add(90 * day)
	svc := rf.srv.Service()
	svc.deps.Now = func() time.Time { return now }
	_ = rf.images.Record(ctx, store.Image{Name: "stale-proxy", Kind: store.ImageProxy, PublishedAt: now.Add(-60 * day)})

	svc.pruneIfDue(ctx)
	if got, _ := rf.images.Get(ctx, exact.Name()); got.DeletedAt != nil {
		t.Fatal("the image a live session booted from was pruned")
	}
	if got, _ := rf.images.Get(ctx, "stale-proxy"); got.DeletedAt == nil {
		t.Fatal("a stale proxy image was not pruned")
	}

	_ = rf.images.Record(ctx, store.Image{Name: "stale-base", Kind: store.ImageBase, PublishedAt: now.Add(-60 * day)})
	now = now.Add(time.Hour)
	svc.pruneIfDue(ctx)
	if got, _ := rf.images.Get(ctx, "stale-base"); got.DeletedAt != nil {
		t.Fatal("pruned again within a day")
	}
	now = now.Add(PruneInterval)
	svc.pruneIfDue(ctx)
	if got, _ := rf.images.Get(ctx, "stale-base"); got.DeletedAt == nil {
		t.Fatal("not pruned a day later")
	}
}

// TestAServerWithoutARegistryDoesNotPrune.
//
// @decision(DL-016)
func TestAServerWithoutARegistryDoesNotPrune(t *testing.T) {
	f := newAPIFixture(t)
	if _, err := f.srv.Service().PruneImages(context.Background(), true); err == nil {
		t.Fatal("a server with no registry pruned")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.srv.Service().RunImagePrune(ctx)
}
