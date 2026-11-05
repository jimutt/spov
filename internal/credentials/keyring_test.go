package credentials

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

// fakeKeyring stands in for the OS keyring, including the WSL failure
// mode where a call blocks forever instead of returning an error.
type fakeKeyring struct {
	// The timeout wrapper deliberately leaks the goroutine of a hung
	// call, so this fake is touched concurrently and must be safe.
	mu      sync.Mutex
	secrets map[string]string
	err     error
	// block, when set, makes every call hang until it is closed.
	block chan struct{}
	calls atomic.Int32
}

func newFakeKeyring() *fakeKeyring {
	return &fakeKeyring{secrets: map[string]string{}}
}

func (f *fakeKeyring) wait() {
	f.calls.Add(1)
	if f.block != nil {
		<-f.block
	}
}

func (f *fakeKeyring) lock() func() {
	f.mu.Lock()
	return f.mu.Unlock
}

func (f *fakeKeyring) Get(_, user string) (string, error) {
	f.wait()
	defer f.lock()()
	if f.err != nil {
		return "", f.err
	}
	v, ok := f.secrets[user]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return v, nil
}

func (f *fakeKeyring) Set(_, user, password string) error {
	f.wait()
	defer f.lock()()
	if f.err != nil {
		return f.err
	}
	f.secrets[user] = password
	return nil
}

func (f *fakeKeyring) Delete(_, user string) error {
	f.wait()
	defer f.lock()()
	if f.err != nil {
		return f.err
	}
	if _, ok := f.secrets[user]; !ok {
		return keyring.ErrNotFound
	}
	delete(f.secrets, user)
	return nil
}

func newTestKeyringStore(api keyringAPI, timeout time.Duration) *KeyringStore {
	return &KeyringStore{api: api, timeout: timeout}
}

func TestKeyringStoreRoundTrip(t *testing.T) {
	s := newTestKeyringStore(newFakeKeyring(), time.Second)

	if _, err := s.Get("k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get on an empty keyring: %v, want ErrNotFound", err)
	}
	if err := s.Set("k", "secret"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get("k"); err != nil || got != "secret" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if err := s.Delete("k"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("k"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete of a missing key: %v, want ErrNotFound", err)
	}
}

// TestKeyringStoreTimesOutInsteadOfHanging is the reason this wrapper
// exists: on WSL with no D-Bus secret service, go-keyring blocks forever.
// An unbounded call would hang the CLI instead of falling back.
//
// @decision(DL-008)
func TestKeyringStoreTimesOutInsteadOfHanging(t *testing.T) {
	blocked := newFakeKeyring()
	blocked.block = make(chan struct{})
	t.Cleanup(func() { close(blocked.block) })

	s := newTestKeyringStore(blocked, 20*time.Millisecond)

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"Get", func() error { _, err := s.Get("k"); return err }},
		{"Set", func() error { return s.Set("k", "v") }},
		{"Delete", func() error { return s.Delete("k") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan error, 1)
			go func() { done <- tc.call() }()

			select {
			case err := <-done:
				if !errors.Is(err, ErrTimeout) {
					t.Errorf("err = %v, want ErrTimeout", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the call hung instead of timing out")
			}
		})
	}
}

func TestKeyringStoreReportsErrors(t *testing.T) {
	failing := newFakeKeyring()
	failing.err = errors.New("dbus: connection refused")
	s := newTestKeyringStore(failing, time.Second)

	_, err := s.Get("k")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v; a backend failure must not look like a missing key", err)
	}
	if !strings.Contains(err.Error(), "dbus") {
		t.Errorf("error %q loses the underlying cause", err)
	}
}

// TestKeyringStoreErrorsNeverCarrySecrets: an error surfaces to the user
// and may be logged.
func TestKeyringStoreErrorsNeverCarrySecrets(t *testing.T) {
	failing := newFakeKeyring()
	failing.err = errors.New("backend exploded")
	s := newTestKeyringStore(failing, time.Second)

	const secret = "sk-ant-do-not-log-me"
	err := s.Set("provider:ANTHROPIC_API_KEY", secret)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the error leaked the secret: %q", err)
	}
}

func TestKeyringStoreZeroTimeoutUsesDefault(t *testing.T) {
	s := newTestKeyringStore(newFakeKeyring(), 0)
	if err := s.Set("k", "v"); err != nil {
		t.Fatal(err)
	}
}

func TestKeyringStoreName(t *testing.T) {
	if got := NewKeyringStore().Name(); got != "os-keyring" {
		t.Errorf("Name = %q", got)
	}
}

func TestNewKeyringStoreUsesRealBackend(t *testing.T) {
	s := NewKeyringStore()
	if s.timeout != DefaultTimeout {
		t.Errorf("timeout = %s, want %s", s.timeout, DefaultTimeout)
	}
	if _, ok := s.api.(realKeyring); !ok {
		t.Errorf("api = %T, want realKeyring", s.api)
	}
}
