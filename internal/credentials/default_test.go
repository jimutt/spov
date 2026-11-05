package credentials

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultPrefersKeyringAndFallsBackToFile(t *testing.T) {
	dir := t.TempDir()
	var warnings bytes.Buffer

	s := Default(dir, &warnings)
	if s == nil {
		t.Fatal("Default returned nil")
	}
	// Before any call the backend is undecided, so nothing has probed the
	// keyring yet.
	if !strings.Contains(s.Name(), "untested") {
		t.Errorf("Name = %q, want the backend to be unresolved", s.Name())
	}

	// This machine may or may not have a working keyring, so assert only
	// that a round trip works and that a file backend warns.
	if err := s.Set("platform:test", "tok"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	t.Cleanup(func() { _ = s.Delete("platform:test") })

	got, err := s.Get("platform:test")
	if err != nil || got != "tok" {
		t.Fatalf("round trip: %q, %v", got, err)
	}

	if strings.HasPrefix(s.Name(), "file:") {
		if !strings.Contains(warnings.String(), "UNENCRYPTED") {
			t.Error("the file backend was used without warning the user")
		}
		if !strings.Contains(s.Name(), dir) {
			t.Errorf("Name = %q, want it to name the configured directory", s.Name())
		}
	}
	if strings.Contains(warnings.String(), "tok") {
		t.Error("the warning leaked the secret")
	}
}

func TestDefaultWithoutWarnWriter(t *testing.T) {
	s := Default(t.TempDir(), nil)
	if err := s.Set("k", "v"); err != nil {
		t.Fatalf("a nil warn writer must not break writing: %v", err)
	}
	t.Cleanup(func() { _ = s.Delete("k") })
}

func TestConfigDir(t *testing.T) {
	got := ConfigDir(filepath.Join("/home/u/.config/spov", "config.yaml"))
	if got != filepath.Clean("/home/u/.config/spov") {
		t.Errorf("ConfigDir = %q", got)
	}
}

func TestRealKeyringImplementsTheAPI(t *testing.T) {
	// The wrappers only adapt package functions to an interface. Reading
	// a key that cannot exist proves the dispatch without writing to the
	// developer's real keyring.
	var api keyringAPI = realKeyring{}
	if _, err := api.Get(ServiceName, "spov-test-nonexistent"); err == nil {
		t.Error("a key that should not exist was found in the OS keyring")
	}
}
