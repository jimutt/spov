package credentials

import (
	"errors"
	"fmt"
	"sync"
)

// FallbackStore tries the OS keyring first and falls back to a file when
// the keyring is unavailable. The decision is made once per process: a
// keyring that hangs would otherwise cost the timeout on every call.
//
// @decision(DL-008)
type FallbackStore struct {
	primary  Store
	fallback Store

	mu       sync.Mutex
	resolved bool
	active   Store
}

// NewFallbackStore returns a store preferring primary over fallback.
func NewFallbackStore(primary, fallback Store) *FallbackStore {
	return &FallbackStore{primary: primary, fallback: fallback}
}

// Name reports the backend actually in use, so `spov config show` can tell
// the user where their secrets live.
func (s *FallbackStore) Name() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resolved {
		return s.active.Name()
	}
	return s.primary.Name() + " (untested)"
}

func (s *FallbackStore) Get(key string) (string, error) {
	store, err := s.pick()
	if err != nil {
		return "", err
	}
	secret, getErr := store.Get(key)
	if errors.Is(getErr, ErrNotFound) {
		// A concurrent write may have demoted the backend after pick()
		// returned, so a miss on the old one is not conclusive.
		if current, moved := s.movedOn(store); moved {
			return current.Get(key)
		}
	}
	return secret, getErr
}

func (s *FallbackStore) Set(key, secret string) error {
	store, err := s.pick()
	if err != nil {
		return err
	}
	if err := store.Set(key, secret); err != nil {
		if demoted, ok := s.demote(store); ok {
			// The probe only reads. A keyring that reads but cannot write
			// — a locked keychain, a read-only D-Bus policy — would
			// otherwise leave the user stuck for the whole process.
			return demoted.Set(key, secret)
		}
		return err
	}
	return nil
}

func (s *FallbackStore) Delete(key string) error {
	store, err := s.pick()
	if err != nil {
		return err
	}
	delErr := store.Delete(key)
	if errors.Is(delErr, ErrNotFound) {
		if current, moved := s.movedOn(store); moved {
			return current.Delete(key)
		}
	}
	return delErr
}

// probeKey is read once to find out whether the keyring works at all. A
// "not found" answer is a working keyring; a timeout or error is not. It
// follows the package's <kind>:<id> convention so the entry is
// self-describing if it ever shows up in a keyring browser.
const probeKey = "probe:keyring"

// pick chooses a backend, probing the keyring at most once.
//
// @decision(DL-008)
func (s *FallbackStore) pick() (Store, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resolved {
		return s.active, nil
	}

	_, err := s.primary.Get(probeKey)
	switch {
	case err == nil, errors.Is(err, ErrNotFound):
		s.active = s.primary
	default:
		if s.fallback == nil {
			return nil, fmt.Errorf("credentials: keyring unavailable and no fallback configured: %w", err)
		}
		s.active = s.fallback
	}
	s.resolved = true
	return s.active, nil
}

// movedOn reports whether the active backend changed since the caller
// picked one, so a read that missed can be retried against the new one.
func (s *FallbackStore) movedOn(picked Store) (Store, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == picked {
		return nil, false
	}
	return s.active, true
}

// demote switches to the fallback backend after the primary failed a
// write. It reports the new backend, or false when there is nothing to
// demote to or another goroutine already demoted.
//
// @decision(DL-008)
func (s *FallbackStore) demote(failed Store) (Store, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fallback == nil || failed == s.fallback {
		return nil, false
	}
	// Another goroutine may have demoted already; either way the caller
	// should retry against whatever is active now.
	s.active = s.fallback
	return s.active, true
}
