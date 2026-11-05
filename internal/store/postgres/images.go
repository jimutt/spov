package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/store/postgres/sqlcgen"
)

// Images is the image registry.
//
// @decision(DL-016)
func (s *Store) Images() store.ImageStore { return pgImages{s} }

type pgImages struct{ s *Store }

func versionsJSON(v []string) ([]byte, error) {
	if v == nil {
		return []byte("[]"), nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("store: encode image versions: %w", err)
	}
	return b, nil
}

func imageFromRow(r sqlcgen.Image) (store.Image, error) {
	img := store.Image{
		Name: r.Name, ID: r.ImageID, Kind: store.ImageKind(r.Kind), RequestKey: r.RequestKey,
		PublishedAt: goTime(r.PublishedAt), LastUsedAt: goTime(r.LastUsedAt), DeletedAt: goTimePtr(r.DeletedAt),
	}
	if len(r.Versions) > 0 {
		if err := json.Unmarshal(r.Versions, &img.Versions); err != nil {
			return store.Image{}, fmt.Errorf("store: decode versions of image %s: %w", r.Name, err)
		}
	}
	if len(img.Versions) == 0 {
		img.Versions = nil
	}
	return img, nil
}

func (p pgImages) Record(ctx context.Context, img store.Image) error {
	versions, err := versionsJSON(img.Versions)
	if err != nil {
		return err
	}
	if img.LastUsedAt.IsZero() {
		img.LastUsedAt = img.PublishedAt
	}
	return mapErr("record image", p.s.q.RecordImage(ctx, sqlcgen.RecordImageParams{
		Name: img.Name, ImageID: img.ID, Kind: string(img.Kind), RequestKey: img.RequestKey, Versions: versions,
		PublishedAt: ts(img.PublishedAt), LastUsedAt: ts(img.LastUsedAt),
	}))
}

func (p pgImages) Get(ctx context.Context, name string) (store.Image, error) {
	row, err := p.s.q.GetImage(ctx, name)
	if err != nil {
		return store.Image{}, mapErr("get image", err)
	}
	return imageFromRow(row)
}

func (p pgImages) Newest(ctx context.Context, key string) (store.Image, error) {
	row, err := p.s.q.NewestImage(ctx, key)
	if err != nil {
		return store.Image{}, mapErr("newest image", err)
	}
	return imageFromRow(row)
}

func (p pgImages) Use(ctx context.Context, img store.Image, at time.Time) error {
	versions, err := versionsJSON(img.Versions)
	if err != nil {
		return err
	}
	return mapErr("use image", p.s.q.UseImage(ctx, sqlcgen.UseImageParams{
		Name: img.Name, ImageID: img.ID, Kind: string(img.Kind), RequestKey: img.RequestKey, Versions: versions,
		PublishedAt: ts(at),
	}))
}

func (p pgImages) List(ctx context.Context) ([]store.Image, error) {
	rows, err := p.s.q.ListImages(ctx)
	if err != nil {
		return nil, mapErr("list images", err)
	}
	out := make([]store.Image, 0, len(rows))
	for _, r := range rows {
		img, err := imageFromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, img)
	}
	return out, nil
}

func (p pgImages) MarkDeleted(ctx context.Context, name string, at time.Time) error {
	n, err := p.s.q.MarkImageDeleted(ctx, sqlcgen.MarkImageDeletedParams{Name: name, DeletedAt: ts(at)})
	if err != nil {
		return mapErr("mark image deleted", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (p pgImages) PrunedAt(ctx context.Context) (time.Time, error) {
	at, err := p.s.q.GetImagePrune(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, mapErr("get image prune", err)
	}
	return goTime(at), nil
}

func (p pgImages) SetPrunedAt(ctx context.Context, at time.Time) error {
	return mapErr("set image prune", p.s.q.SetImagePrune(ctx, ts(at)))
}
