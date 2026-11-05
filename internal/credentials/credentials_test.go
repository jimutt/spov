package credentials

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestKey(t *testing.T) {
	tests := []struct {
		name     string
		kind, id string
		want     string
		wantErr  bool
	}{
		{"platform", KindPlatform, "codefloe", "platform:codefloe", false},
		{"sandbox", KindSandbox, "sb-abc123", "sandbox:sb-abc123", false},
		{"provider", KindProvider, "ANTHROPIC_API_KEY", "provider:ANTHROPIC_API_KEY", false},
		{"empty kind", "", "x", "", true},
		{"empty id", KindPlatform, "", "", true},
		{"colon in the id", KindPlatform, "a:b", "", true},
		{"colon in the kind", "a:b", "x", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Key(tt.kind, tt.id)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("Key = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestKeysAreDistinct: a collision would hand one credential's secret to
// another consumer.
func TestKeysAreDistinct(t *testing.T) {
	seen := map[string]string{}
	for _, tc := range []struct {
		label string
		build func() (string, error)
	}{
		{"platform codefloe", func() (string, error) { return PlatformKey("codefloe") }},
		{"platform github", func() (string, error) { return PlatformKey("github") }},
		{"sandbox codefloe", func() (string, error) { return SandboxKey("codefloe") }},
		{"provider codefloe", func() (string, error) { return ProviderKey("codefloe") }},
	} {
		key, err := tc.build()
		if err != nil {
			t.Fatal(err)
		}
		if prev, dup := seen[key]; dup {
			t.Errorf("%q collides with %q on key %q", tc.label, prev, key)
		}
		seen[key] = tc.label
	}
}

func TestKeyHelpersRejectBadInput(t *testing.T) {
	for _, build := range []func(string) (string, error){PlatformKey, SandboxKey, ProviderKey} {
		if _, err := build(""); err == nil {
			t.Error("an empty id was accepted")
		}
		if _, err := build("a:b"); err == nil {
			t.Error("an id containing a colon was accepted")
		}
	}
}

func TestErrNotFoundIsMatchable(t *testing.T) {
	s := NewMemoryStore()
	_, err := s.Get("absent")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "absent") {
		t.Error("the error does not name the key that was missing")
	}
	// Callers branch on this after their own wrapping, so it has to
	// survive another layer.
	if wrapped := fmt.Errorf("resolving platform token: %w", err); !errors.Is(wrapped, ErrNotFound) {
		t.Error("ErrNotFound did not survive wrapping")
	}
}

func TestMemoryStore(t *testing.T) {
	s := NewMemoryStore()
	if s.Name() != "memory" {
		t.Errorf("Name = %q", s.Name())
	}
	if err := s.Set("k", "v"); err != nil {
		t.Fatal(err)
	}
	if got := s.Snapshot(); got["k"] != "v" {
		t.Errorf("Snapshot = %v", got)
	}
	// Snapshot must be a copy.
	s.Snapshot()["k"] = "tampered"
	if got, _ := s.Get("k"); got != "v" {
		t.Error("Snapshot aliased the internal map")
	}

	if err := s.Delete("k"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("k"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}

	sentinel := errors.New("forced")
	s.GetErr, s.SetErr, s.DeleteErr = sentinel, sentinel, sentinel
	if _, err := s.Get("k"); !errors.Is(err, sentinel) {
		t.Error("GetErr was not honoured")
	}
	if err := s.Set("k", "v"); !errors.Is(err, sentinel) {
		t.Error("SetErr was not honoured")
	}
	if err := s.Delete("k"); !errors.Is(err, sentinel) {
		t.Error("DeleteErr was not honoured")
	}
}

// TestServerKeyNormalisesTheHost: the same server must be one entry
// however its URL was written, or a login and the request that follows
// would look in different places.
//
// @decision(DL-006)
func TestServerKeyNormalisesTheHost(t *testing.T) {
	same := []string{
		"https://spov.example.com",
		"https://spov.example.com/",
		"https://spov.example.com/some/path",
		"https://SPOV.example.COM",
		// The default port is the same server as none.
		"https://spov.example.com:443",
		"  https://spov.example.com  ",
	}
	want, err := ServerKey(same[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range same[1:] {
		got, err := ServerKey(u)
		if err != nil {
			t.Fatalf("ServerKey(%q): %v", u, err)
		}
		if got != want {
			t.Errorf("ServerKey(%q) = %q, want %q", u, got, want)
		}
	}

	// Different servers are different entries, so two of them never
	// share a token.
	for _, u := range []string{
		"https://other.example.com",
		"https://spov.example.com:8443",
		// A different scheme on the same host is a different origin.
		"http://spov.example.com",
	} {
		got, err := ServerKey(u)
		if err != nil {
			t.Fatalf("ServerKey(%q): %v", u, err)
		}
		if got == want {
			t.Errorf("ServerKey(%q) collides with %q", u, want)
		}
	}
}

// TestServerKeyIsAValidKey: the host carries a colon, which the flat key
// space does not allow.
//
// @decision(DL-006)
func TestServerKeyIsAValidKey(t *testing.T) {
	key, err := ServerKey("https://spov.example.com:8443")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(key, ":") != 1 {
		t.Errorf("key %q has a colon outside the namespace separator", key)
	}
	if !strings.HasPrefix(key, KindServer+":") {
		t.Errorf("key %q is not in the server namespace", key)
	}
}

// TestServerKeyRefusesWhatIsNotAServerURL.
//
// @decision(DL-006)
func TestServerKeyRefusesWhatIsNotAServerURL(t *testing.T) {
	for _, bad := range []string{"", "spov.example.com", "ftp://spov.example.com", "https://"} {
		if _, err := ServerKey(bad); err == nil {
			t.Errorf("ServerKey(%q) was accepted", bad)
		}
	}
}
