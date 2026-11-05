package credentials

import (
	"errors"
	"fmt"
	"time"

	"github.com/zalando/go-keyring"
)

// ServiceName is the service every spov secret is stored under.
const ServiceName = "spov"

// DefaultTimeout bounds every keyring call. On WSL and other systems with
// no D-Bus secret service, go-keyring blocks rather than failing, so an
// unbounded call would hang the CLI instead of falling back.
//
// @decision(DL-008)
const DefaultTimeout = 2 * time.Second

// ErrTimeout means the OS keyring did not answer in time.
//
// A timed-out write has an UNKNOWN outcome, not a failed one: the call
// cannot be cancelled, so it may still complete afterwards and leave the
// secret in the keyring. Callers that fall back to another backend should
// assume a copy may exist in the keyring too.
var ErrTimeout = errors.New("credentials: keyring timed out (the operation may still have completed)")

// keyringAPI is the subset of go-keyring the store uses, so the timeout
// and fallback logic can be tested without a real keyring.
type keyringAPI interface {
	Get(service, user string) (string, error)
	Set(service, user, password string) error
	Delete(service, user string) error
}

// realKeyring calls the OS keyring.
type realKeyring struct{}

func (realKeyring) Get(service, user string) (string, error) {
	return keyring.Get(service, user)
}
func (realKeyring) Set(service, user, password string) error {
	return keyring.Set(service, user, password)
}
func (realKeyring) Delete(service, user string) error {
	return keyring.Delete(service, user)
}

// KeyringStore stores secrets in the OS keyring, bounding every call.
//
// @decision(DL-008)
type KeyringStore struct {
	api     keyringAPI
	timeout time.Duration
}

// NewKeyringStore returns a store backed by the OS keyring.
func NewKeyringStore() *KeyringStore {
	return &KeyringStore{api: realKeyring{}, timeout: DefaultTimeout}
}

func (s *KeyringStore) Name() string { return "os-keyring" }

func (s *KeyringStore) Get(key string) (string, error) {
	var secret string
	err := s.call(func() error {
		v, err := s.api.Get(ServiceName, key)
		secret = v
		return err
	})
	if errors.Is(err, keyring.ErrNotFound) {
		return "", fmt.Errorf("credentials: %q: %w", key, ErrNotFound)
	}
	if err != nil {
		return "", err
	}
	return secret, nil
}

func (s *KeyringStore) Set(key, secret string) error {
	return s.call(func() error { return s.api.Set(ServiceName, key, secret) })
}

func (s *KeyringStore) Delete(key string) error {
	err := s.call(func() error { return s.api.Delete(ServiceName, key) })
	if errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("credentials: %q: %w", key, ErrNotFound)
	}
	return err
}

// call runs fn with a timeout. The goroutine is deliberately leaked on
// timeout: a hung D-Bus call cannot be cancelled, and blocking on it is
// exactly the failure this wrapper exists to avoid. The buffered channel
// lets that goroutine finish and exit whenever the call eventually
// returns.
//
// @decision(DL-008)
func (s *KeyringStore) call(fn func() error) error {
	done := make(chan error, 1)
	go func() { done <- fn() }()

	timeout := s.timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, keyring.ErrNotFound) {
			// The message can name the key but never the secret.
			return fmt.Errorf("credentials: keyring: %w", err)
		}
		return err
	case <-timer.C:
		return ErrTimeout
	}
}
