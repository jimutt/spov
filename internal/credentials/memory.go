package credentials

import (
	"fmt"
	"maps"
	"sync"
)

// MemoryStore is an in-memory Store for tests.
//
// @decision(DL-008)
type MemoryStore struct {
	mu      sync.Mutex
	secrets map[string]string

	// GetErr, SetErr and DeleteErr force the corresponding call to fail.
	GetErr    error
	SetErr    error
	DeleteErr error
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{secrets: map[string]string{}}
}

func (s *MemoryStore) Name() string { return "memory" }

func (s *MemoryStore) Get(key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.GetErr != nil {
		return "", s.GetErr
	}
	secret, ok := s.secrets[key]
	if !ok {
		return "", fmt.Errorf("credentials: %q: %w", key, ErrNotFound)
	}
	return secret, nil
}

func (s *MemoryStore) Set(key, secret string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.SetErr != nil {
		return s.SetErr
	}
	s.secrets[key] = secret
	return nil
}

func (s *MemoryStore) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.DeleteErr != nil {
		return s.DeleteErr
	}
	if _, ok := s.secrets[key]; !ok {
		return fmt.Errorf("credentials: %q: %w", key, ErrNotFound)
	}
	delete(s.secrets, key)
	return nil
}

// Snapshot returns a copy of the stored secrets, for assertions.
func (s *MemoryStore) Snapshot() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.secrets)
}

var (
	_ Store = (*MemoryStore)(nil)
	_ Store = (*FileStore)(nil)
	_ Store = (*KeyringStore)(nil)
	_ Store = (*FallbackStore)(nil)
)
