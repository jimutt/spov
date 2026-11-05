package store

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"
)

// ImageKind is what a published image is for.
//
// @decision(DL-016)
type ImageKind string

const (
	// ImageBase is an OpenCode pin's base image (DL-032).
	ImageBase ImageKind = "base"
	// ImageProxy is a spov build's credential-proxy image (DL-034).
	ImageProxy ImageKind = "proxy"
	// ImageVariant is an add-on variant (DL-016).
	ImageVariant ImageKind = "variant"
)

// Image is one image spov published, by name. It is deployment-wide,
// not scoped to an org: every org's sessions boot the same images.
//
// @decision(DL-016)
type Image struct {
	Name string `json:"name"`
	// ID is the Modal image the name was published as, or empty when it
	// was not looked up.
	ID   string    `json:"id,omitempty"`
	Kind ImageKind `json:"kind"`
	// RequestKey is a variant's name with its add-on versions as they
	// were asked for, and Versions its add-ons as installed
	// (`name@version`). Both are empty for any other kind.
	RequestKey string   `json:"request_key,omitempty"`
	Versions   []string `json:"versions,omitempty"`
	// PublishedAt is when the name was last published, and LastUsedAt
	// when a launch last used it, updated at most hourly.
	PublishedAt time.Time `json:"published_at"`
	LastUsedAt  time.Time `json:"last_used_at"`
	// DeletedAt is when pruning deleted it. The name may still resolve
	// in Modal, but no sandbox can be created from it.
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

// ImageStore is the registry of published images: what a launch uses to
// find the newest build of a request, and what pruning reads. A hosted
// server keeps it in Postgres; a local server in its state file.
//
// @decision(DL-016)
type ImageStore interface {
	// Record enters img as published at img.PublishedAt, replacing any
	// entry for its name, a deleted one included. LastUsedAt is set to
	// the publish time when it is zero.
	Record(ctx context.Context, img Image) error
	// Get is the entry for name, or ErrNotFound.
	Get(ctx context.Context, name string) (Image, error)
	// Newest is the variant of key published last that is not deleted,
	// or ErrNotFound.
	Newest(ctx context.Context, key string) (Image, error)
	// Use moves name's LastUsedAt forward to at. A name with no entry is
	// entered from img, as published at at; a deleted entry is left as
	// it is.
	Use(ctx context.Context, img Image, at time.Time) error
	List(ctx context.Context) ([]Image, error)
	MarkDeleted(ctx context.Context, name string, at time.Time) error
	// PrunedAt is when pruning last ran, zero if never.
	PrunedAt(ctx context.Context) (time.Time, error)
	SetPrunedAt(ctx context.Context, at time.Time) error
}

// ImageTable is ImageStore's logic over a plain list, shared by the
// memory registry and the local state file. It is not safe for
// concurrent use.
//
// @decision(DL-016)
type ImageTable struct {
	Images    []Image   `json:"images,omitempty"`
	LastPrune time.Time `json:"pruned_at,omitzero"`
}

func (t *ImageTable) find(name string) int {
	return slices.IndexFunc(t.Images, func(i Image) bool { return i.Name == name })
}

// Record enters img, replacing any entry for its name.
func (t *ImageTable) Record(img Image) {
	if img.LastUsedAt.IsZero() {
		img.LastUsedAt = img.PublishedAt
	}
	img.DeletedAt = nil
	img.Versions = slices.Clone(img.Versions)
	if i := t.find(img.Name); i >= 0 {
		t.Images[i] = img
		return
	}
	t.Images = append(t.Images, img)
}

// Get is the entry for name.
func (t *ImageTable) Get(name string) (Image, error) {
	if i := t.find(name); i >= 0 {
		return copyImage(t.Images[i]), nil
	}
	return Image{}, ErrNotFound
}

// copyImage is img with nothing shared with the table.
func copyImage(img Image) Image {
	img.Versions = slices.Clone(img.Versions)
	if img.DeletedAt != nil {
		at := *img.DeletedAt
		img.DeletedAt = &at
	}
	return img
}

// Newest is key's latest variant that is not deleted.
func (t *ImageTable) Newest(key string) (Image, error) {
	var best *Image
	for i := range t.Images {
		img := &t.Images[i]
		if img.Kind != ImageVariant || img.RequestKey != key || img.DeletedAt != nil {
			continue
		}
		if best == nil || img.PublishedAt.After(best.PublishedAt) {
			best = img
		}
	}
	if best == nil {
		return Image{}, ErrNotFound
	}
	return copyImage(*best), nil
}

// Use moves name's last use forward, entering img if it is unknown.
func (t *ImageTable) Use(img Image, at time.Time) {
	i := t.find(img.Name)
	if i < 0 {
		img.PublishedAt, img.LastUsedAt = at, at
		t.Record(img)
		return
	}
	if t.Images[i].DeletedAt == nil && at.After(t.Images[i].LastUsedAt) {
		t.Images[i].LastUsedAt = at
	}
}

// List is every entry, by name.
func (t *ImageTable) List() []Image {
	out := make([]Image, len(t.Images))
	for i, img := range t.Images {
		out[i] = copyImage(img)
	}
	slices.SortFunc(out, func(a, b Image) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// MarkDeleted marks name deleted at at.
func (t *ImageTable) MarkDeleted(name string, at time.Time) error {
	i := t.find(name)
	if i < 0 {
		return ErrNotFound
	}
	t.Images[i].DeletedAt = &at
	return nil
}

// MemoryImages is an ImageStore held in memory, for tests and for a
// hosted server without a database.
type MemoryImages struct {
	mu sync.Mutex
	t  ImageTable
}

// NewMemoryImages is an empty registry.
func NewMemoryImages() *MemoryImages { return &MemoryImages{} }

func (m *MemoryImages) Record(_ context.Context, img Image) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.t.Record(img)
	return nil
}

func (m *MemoryImages) Get(_ context.Context, name string) (Image, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.t.Get(name)
}

func (m *MemoryImages) Newest(_ context.Context, key string) (Image, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.t.Newest(key)
}

func (m *MemoryImages) Use(_ context.Context, img Image, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.t.Use(img, at)
	return nil
}

func (m *MemoryImages) List(context.Context) ([]Image, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.t.List(), nil
}

func (m *MemoryImages) MarkDeleted(_ context.Context, name string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.t.MarkDeleted(name, at)
}

func (m *MemoryImages) PrunedAt(context.Context) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.t.LastPrune, nil
}

func (m *MemoryImages) SetPrunedAt(_ context.Context, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.t.LastPrune = at
	return nil
}
