// Package statefile is a local server's add-on state on disk: the
// resolved add-on versions, the image registry and the model list, in one JSON file
// beside the server's own state file. A local server exits after a
// minute with nothing to do, so what it learns must outlive the process.
package statefile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/jimutt/spov/internal/addonindex"
	"github.com/jimutt/spov/internal/modelcat"
	"github.com/jimutt/spov/internal/store"
)

// Name is the file's name in the state directory.
const Name = "variants.json"

// File is the state file at Path. It implements store.ImageStore,
// addonindex.Persist and modelcat.Persist. Every call reads the file afresh under an advisory
// lock on Path+".lock", and every change replaces it whole while holding
// the lock, so a CLI command and a running server see and keep each
// other's writes. A missing, unreadable or malformed file reads as
// empty, never as an error.
//
// @decision(DL-033) @decision(DL-016)
type File struct {
	Path string
	// Warn is told once when the file cannot be read.
	Warn func(string)

	mu     sync.Mutex
	warned bool
}

type contents struct {
	Answers map[string]addonindex.Answer `json:"answers,omitempty"`
	// @decision(DL-013)
	Models *modelcat.List `json:"models,omitempty"`
	store.ImageTable
}

// read loads the file. f.mu is held.
func (f *File) read() contents {
	var c contents
	raw, err := os.ReadFile(f.Path)
	if err == nil {
		err = json.Unmarshal(raw, &c)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		if !f.warned && f.Warn != nil {
			f.Warn(fmt.Sprintf("%s cannot be read, so it is treated as empty: %v", f.Path, err))
		}
		f.warned = true
		return contents{}
	}
	return c
}

// write replaces the file atomically. f.mu is held.
func (f *File) write(c contents) error {
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.Path), Name+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), f.Path)
}

// lock takes the cross-process lock, shared to read or exclusive to
// write. A lock that cannot be taken is no lock, never a failure.
func (f *File) lock(how int) func() {
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		return func() {}
	}
	lf, err := os.OpenFile(f.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}
	}
	if err := syscall.Flock(int(lf.Fd()), how); err != nil {
		_ = lf.Close()
		return func() {}
	}
	return func() {
		_ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
		_ = lf.Close()
	}
}

func (f *File) view(fn func(c *contents)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	defer f.lock(syscall.LOCK_SH)()
	c := f.read()
	fn(&c)
}

func (f *File) update(fn func(c *contents) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	defer f.lock(syscall.LOCK_EX)()
	c := f.read()
	if err := fn(&c); err != nil {
		return err
	}
	if err := f.write(c); err != nil {
		return fmt.Errorf("save %s: %w", f.Path, err)
	}
	return nil
}

// LoadAnswers is the saved resolutions.
func (f *File) LoadAnswers() (map[string]addonindex.Answer, error) {
	var out map[string]addonindex.Answer
	f.view(func(c *contents) { out = c.Answers })
	return out, nil
}

// SaveAnswers replaces the saved resolutions.
func (f *File) SaveAnswers(a map[string]addonindex.Answer) error {
	return f.update(func(c *contents) error {
		c.Answers = a
		return nil
	})
}

// LoadModels is the saved model list.
//
// @decision(DL-013)
func (f *File) LoadModels() (modelcat.List, bool) {
	var out *modelcat.List
	f.view(func(c *contents) { out = c.Models })
	if out == nil {
		return modelcat.List{}, false
	}
	return *out, true
}

// SaveModels replaces the saved model list.
//
// @decision(DL-013)
func (f *File) SaveModels(l modelcat.List) error {
	return f.update(func(c *contents) error {
		c.Models = &l
		return nil
	})
}

func (f *File) Record(_ context.Context, img store.Image) error {
	return f.update(func(c *contents) error {
		c.Record(img)
		return nil
	})
}

func (f *File) Get(_ context.Context, name string) (img store.Image, err error) {
	f.view(func(c *contents) { img, err = c.Get(name) })
	return img, err
}

func (f *File) Newest(_ context.Context, key string) (img store.Image, err error) {
	f.view(func(c *contents) { img, err = c.Newest(key) })
	return img, err
}

func (f *File) Use(_ context.Context, img store.Image, at time.Time) error {
	return f.update(func(c *contents) error {
		c.Use(img, at)
		return nil
	})
}

func (f *File) List(context.Context) (out []store.Image, _ error) {
	f.view(func(c *contents) { out = c.List() })
	return out, nil
}

func (f *File) MarkDeleted(_ context.Context, name string, at time.Time) error {
	return f.update(func(c *contents) error { return c.MarkDeleted(name, at) })
}

func (f *File) PrunedAt(context.Context) (at time.Time, _ error) {
	f.view(func(c *contents) { at = c.LastPrune })
	return at, nil
}

func (f *File) SetPrunedAt(_ context.Context, at time.Time) error {
	return f.update(func(c *contents) error {
		c.LastPrune = at
		return nil
	})
}
