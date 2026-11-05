package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/store"
)

// RunImages runs the image registry's contract against a fresh registry
// from factory.
//
// @decision(DL-016)
func RunImages(t *testing.T, factory func(*testing.T) store.ImageStore) {
	ctx := context.Background()
	t0 := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

	t.Run("an unknown name is not found", func(t *testing.T) {
		s := factory(t)
		if _, err := s.Get(ctx, "spov-base-x"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("Get: %v", err)
		}
		if _, err := s.Newest(ctx, "spov-base-x"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("Newest: %v", err)
		}
		if err := s.MarkDeleted(ctx, "spov-base-x", t0); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("MarkDeleted: %v", err)
		}
	})

	t.Run("the newest build of a request is the last published that is not deleted", func(t *testing.T) {
		s := factory(t)
		for i, name := range []string{"v-old", "v-new", "v-newer"} {
			if err := s.Record(ctx, store.Image{
				Name: name, ID: "im-" + name, Kind: store.ImageVariant, RequestKey: "key",
				Versions: []string{"node@22." + name}, PublishedAt: t0.Add(time.Duration(i) * time.Hour),
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Record(ctx, store.Image{Name: "other", Kind: store.ImageVariant, RequestKey: "key2", PublishedAt: t0.Add(9 * time.Hour)}); err != nil {
			t.Fatal(err)
		}
		got, err := s.Newest(ctx, "key")
		if err != nil || got.Name != "v-newer" || got.ID != "im-v-newer" || len(got.Versions) != 1 || !got.LastUsedAt.Equal(got.PublishedAt) {
			t.Fatalf("Newest: %+v, %v", got, err)
		}
		if err := s.MarkDeleted(ctx, "v-newer", t0.Add(10*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if got, err := s.Newest(ctx, "key"); err != nil || got.Name != "v-new" {
			t.Fatalf("a deleted build is not the newest: %+v, %v", got, err)
		}
		deleted, err := s.Get(ctx, "v-newer")
		if err != nil || deleted.DeletedAt == nil || !deleted.DeletedAt.Equal(t0.Add(10*time.Hour)) {
			t.Fatalf("a deleted entry is kept, marked: %+v, %v", deleted, err)
		}
		// Publishing a deleted name again brings it back.
		if err := s.Record(ctx, store.Image{Name: "v-newer", ID: "im-again", Kind: store.ImageVariant, RequestKey: "key", PublishedAt: t0.Add(11 * time.Hour)}); err != nil {
			t.Fatal(err)
		}
		if got, err := s.Newest(ctx, "key"); err != nil || got.Name != "v-newer" || got.ID != "im-again" || got.DeletedAt != nil {
			t.Fatalf("a republished name: %+v, %v", got, err)
		}
	})

	t.Run("use enters an unknown name and only moves forward", func(t *testing.T) {
		s := factory(t)
		base := store.Image{Name: "spov-base-1", Kind: store.ImageBase}
		if err := s.Use(ctx, base, t0); err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(ctx, "spov-base-1")
		if err != nil || got.Kind != store.ImageBase || !got.PublishedAt.Equal(t0) || !got.LastUsedAt.Equal(t0) {
			t.Fatalf("entered: %+v, %v", got, err)
		}
		_ = s.Use(ctx, base, t0.Add(-time.Hour))
		_ = s.Use(ctx, base, t0.Add(2*time.Hour))
		if got, _ := s.Get(ctx, "spov-base-1"); !got.LastUsedAt.Equal(t0.Add(2 * time.Hour)) {
			t.Fatalf("last used: %v", got.LastUsedAt)
		}
		_ = s.MarkDeleted(ctx, "spov-base-1", t0.Add(3*time.Hour))
		_ = s.Use(ctx, base, t0.Add(4*time.Hour))
		if got, _ := s.Get(ctx, "spov-base-1"); got.DeletedAt == nil || !got.LastUsedAt.Equal(t0.Add(2*time.Hour)) {
			t.Fatalf("a deleted entry is left as it is: %+v", got)
		}
		list, err := s.List(ctx)
		if err != nil || len(list) != 1 {
			t.Fatalf("List: %+v, %v", list, err)
		}
	})

	t.Run("the last prune is remembered", func(t *testing.T) {
		s := factory(t)
		if at, err := s.PrunedAt(ctx); err != nil || !at.IsZero() {
			t.Fatalf("never pruned: %v, %v", at, err)
		}
		if err := s.SetPrunedAt(ctx, t0); err != nil {
			t.Fatal(err)
		}
		if at, err := s.PrunedAt(ctx); err != nil || !at.Equal(t0) {
			t.Fatalf("PrunedAt: %v, %v", at, err)
		}
	})
}
