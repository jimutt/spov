package credentials

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFallbackUsesKeyringWhenItWorks(t *testing.T) {
	primary := NewMemoryStore()
	fallback := NewMemoryStore()
	s := NewFallbackStore(primary, fallback)

	if err := s.Set("k", "v"); err != nil {
		t.Fatal(err)
	}
	if len(primary.Snapshot()) != 1 {
		t.Error("the secret did not go to the primary store")
	}
	if len(fallback.Snapshot()) != 0 {
		t.Error("the secret leaked into the fallback store")
	}
	if s.Name() != primary.Name() {
		t.Errorf("Name = %q, want %q", s.Name(), primary.Name())
	}
}

// TestFallbackUsesFileWhenKeyringHangs is the WSL path: a keyring that
// times out must not stop spov from working.
//
// @decision(DL-008)
func TestFallbackUsesFileWhenKeyringHangs(t *testing.T) {
	blocked := newFakeKeyring()
	blocked.block = make(chan struct{})
	t.Cleanup(func() { close(blocked.block) })

	primary := newTestKeyringStore(blocked, 10*time.Millisecond)
	fallback := NewFileStore(filepath.Join(t.TempDir(), "credentials.json"))
	s := NewFallbackStore(primary, fallback)

	done := make(chan error, 1)
	go func() { done <- s.Set("platform:codefloe", "tok") }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Set: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Set hung instead of falling back")
	}

	got, err := fallback.Get("platform:codefloe")
	if err != nil || got != "tok" {
		t.Fatalf("the fallback store does not hold the secret: %q, %v", got, err)
	}
	if !strings.HasPrefix(s.Name(), "file:") {
		t.Errorf("Name = %q; it must report the backend actually in use", s.Name())
	}
}

// TestFallbackProbesOnce: a hanging keyring costs the timeout, so paying
// it on every call would make the CLI unusable.
func TestFallbackProbesOnce(t *testing.T) {
	blocked := newFakeKeyring()
	blocked.block = make(chan struct{})
	t.Cleanup(func() { close(blocked.block) })

	primary := newTestKeyringStore(blocked, 10*time.Millisecond)
	s := NewFallbackStore(primary, NewMemoryStore())

	for range 5 {
		if err := s.Set("k", "v"); err != nil {
			t.Fatal(err)
		}
	}
	if got := blocked.calls.Load(); got != 1 {
		t.Errorf("the keyring was probed %d times, want 1", got)
	}
}

func TestFallbackTreatsNotFoundAsWorking(t *testing.T) {
	// An empty keyring answers "not found" to the probe. That is a
	// working keyring, not a broken one.
	primary := NewMemoryStore()
	s := NewFallbackStore(primary, NewMemoryStore())

	if _, err := s.Get("absent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if s.Name() != primary.Name() {
		t.Errorf("a not-found probe was treated as a broken keyring: %q", s.Name())
	}
}

func TestFallbackWithoutFallbackConfigured(t *testing.T) {
	primary := NewMemoryStore()
	primary.GetErr = errors.New("keyring exploded")
	s := NewFallbackStore(primary, nil)

	_, err := s.Get("k")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "no fallback") {
		t.Errorf("err = %v", err)
	}
}

func TestFallbackNameBeforeUse(t *testing.T) {
	s := NewFallbackStore(NewMemoryStore(), NewMemoryStore())
	if !strings.Contains(s.Name(), "untested") {
		t.Errorf("Name = %q; before any call the backend is not yet known", s.Name())
	}
}

func TestFallbackDeleteAndGetUseTheSameBackend(t *testing.T) {
	primary := NewMemoryStore()
	primary.GetErr = errors.New("broken")
	fallback := NewMemoryStore()
	s := NewFallbackStore(primary, fallback)

	if err := s.Set("k", "v"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("k"); err != nil {
		t.Fatalf("Delete went to a different backend than Set: %v", err)
	}
}

func TestFallbackIsConcurrencySafe(t *testing.T) {
	s := NewFallbackStore(NewMemoryStore(), NewMemoryStore())
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("sandbox:sb-%d", i)
			if err := s.Set(key, fmt.Sprintf("secret-%d", i)); err != nil {
				t.Error(err)
			}
			got, err := s.Get(key)
			if err != nil {
				t.Error(err)
				return
			}
			// Concurrent distinct writes must not cross over.
			if want := fmt.Sprintf("secret-%d", i); got != want {
				t.Errorf("key %s holds %q, want %q", key, got, want)
			}
			_ = s.Name()
		}(i)
	}
	wg.Wait()
}

// TestFallbackDemotesOnWriteFailure covers the read-only keyring: the
// probe is a Get, so a keyring that reads but cannot write (a locked
// keychain, a read-only D-Bus policy) passes the probe and then fails
// every Set. Without demotion the user is stuck for the whole process.
//
// @decision(DL-008)
func TestFallbackDemotesOnWriteFailure(t *testing.T) {
	readOnly := NewMemoryStore()
	readOnly.SetErr = errors.New("keyring is locked")
	fallback := NewMemoryStore()
	s := NewFallbackStore(readOnly, fallback)

	if err := s.Set("platform:codefloe", "tok"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := fallback.Snapshot()["platform:codefloe"]; got != "tok" {
		t.Errorf("the secret did not reach the fallback: %v", fallback.Snapshot())
	}
	if s.Name() != fallback.Name() {
		t.Errorf("Name = %q, want the demoted backend", s.Name())
	}

	// Subsequent reads must use the demoted backend too, or the secret
	// just written would be invisible.
	got, err := s.Get("platform:codefloe")
	if err != nil || got != "tok" {
		t.Errorf("Get after demotion: %q, %v", got, err)
	}
}

func TestFallbackDemotionIsSticky(t *testing.T) {
	readOnly := NewMemoryStore()
	readOnly.SetErr = errors.New("locked")
	fallback := NewMemoryStore()
	s := NewFallbackStore(readOnly, fallback)

	for i := range 3 {
		if err := s.Set("k", "v"); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if len(readOnly.Snapshot()) != 0 {
		t.Error("a write reached the demoted primary")
	}
}

func TestFallbackReportsWriteFailureWithNoFallback(t *testing.T) {
	readOnly := NewMemoryStore()
	readOnly.SetErr = errors.New("locked")
	s := NewFallbackStore(readOnly, nil)

	// With no fallback configured the probe itself fails, so the error is
	// about the missing fallback rather than the write.
	if err := s.Set("k", "v"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestFallbackDemotionIsConcurrencySafe(t *testing.T) {
	readOnly := NewMemoryStore()
	readOnly.SetErr = errors.New("locked")
	fallback := NewMemoryStore()
	s := NewFallbackStore(readOnly, fallback)

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := "k" + string(rune('a'+i%5))
			if err := s.Set(key, "v"); err != nil {
				t.Error(err)
			}
			if _, err := s.Get(key); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if len(fallback.Snapshot()) != 5 {
		t.Errorf("fallback holds %d keys, want 5", len(fallback.Snapshot()))
	}
}
