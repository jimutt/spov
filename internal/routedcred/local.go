package routedcred

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/jimutt/spov/internal/credentials"
)

// ErrFull refuses a routed credential beyond the most an owner holds.
var ErrFull = errors.New("routed credentials: no room for another")

// IndexName is the local index's file, in the local server's state
// directory beside server.json.
//
// @decision(DL-073)
const IndexName = "routes.json"

// Local keeps a local server's routed credentials: each secret in this
// machine's credential store and each route in the index file, which a
// listing reads because the store cannot enumerate.
//
// @decision(DL-073)
type Local struct {
	Creds credentials.Store
	// Path is the index file.
	Path string

	mu sync.Mutex
}

// Held is one routed credential as a store holds it: its route, and
// whether its secret could be found.
type Held struct {
	Route
	// Missing is set when the index names a credential whose secret the
	// store does not hold.
	Missing bool
}

type index struct {
	Version int     `json:"version"`
	Routes  []Route `json:"routes"`
}

// List is every routed credential the index names.
func (l *Local) List() ([]Held, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	routes, err := l.read()
	if err != nil {
		return nil, err
	}
	out := make([]Held, 0, len(routes))
	for _, r := range routes {
		_, err := l.Creds.Get(Key(r.Name))
		out = append(out, Held{Route: r, Missing: err != nil})
	}
	return out, nil
}

// Put stores a secret and its route, replacing any of the same name.
func (l *Local) Put(r Route, secret string) error {
	r = r.Normal()
	if err := r.Validate(); err != nil {
		return err
	}
	if err := r.WithSecret(secret).Validate(); err != nil {
		return errors.New(strings.TrimPrefix(err.Error(), "credproxy: "))
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	routes, err := l.read()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(routes, func(x Route) bool { return x.Name == r.Name })
	if i < 0 && len(routes) >= MaxPerUser {
		return fmt.Errorf("%w: this machine already holds %d, the most it keeps", ErrFull, MaxPerUser)
	}
	if err := l.Creds.Set(Key(r.Name), secret); err != nil {
		return err
	}
	if i < 0 {
		routes = append(routes, r)
	} else {
		routes[i] = r
	}
	return l.write(routes)
}

// Delete removes a routed credential, reporting whether there was one.
func (l *Local) Delete(name string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	routes, err := l.read()
	if err != nil {
		return false, err
	}
	i := slices.IndexFunc(routes, func(x Route) bool { return x.Name == name })
	if err := l.Creds.Delete(Key(name)); err != nil && !errors.Is(err, credentials.ErrNotFound) {
		return false, err
	}
	if i < 0 {
		return false, nil
	}
	return true, l.write(slices.Delete(routes, i, i+1))
}

// Secret reads one credential's secret.
func (l *Local) Secret(name string) (string, error) {
	return l.Creds.Get(Key(name))
}

func (l *Local) read() ([]Route, error) {
	b, err := os.ReadFile(l.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("routed credentials: %w", err)
	}
	var idx index
	if err := json.Unmarshal(b, &idx); err != nil {
		return nil, fmt.Errorf("routed credentials: read %s: %w", l.Path, err)
	}
	if idx.Version != 1 {
		return nil, fmt.Errorf("routed credentials: %s has version %d, which this build does not read", l.Path, idx.Version)
	}
	for i := range idx.Routes {
		idx.Routes[i] = idx.Routes[i].Normal()
	}
	return idx.Routes, nil
}

// write replaces the index in one rename, readable by its owner only.
func (l *Local) write(routes []Route) error {
	if routes == nil {
		routes = []Route{}
	}
	b, err := json.MarshalIndent(index{Version: 1, Routes: routes}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(l.Path)
	if err := os.MkdirAll(dir, credentials.DirMode); err != nil {
		return fmt.Errorf("routed credentials: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".routes-*")
	if err != nil {
		return fmt.Errorf("routed credentials: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(credentials.FileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("routed credentials: %w", err)
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("routed credentials: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("routed credentials: %w", err)
	}
	if err := os.Rename(tmp.Name(), l.Path); err != nil {
		return fmt.Errorf("routed credentials: %w", err)
	}
	return nil
}
