package auth

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func testKey(b byte) string {
	key := bytes.Repeat([]byte{b}, KeyBytes)
	return base64.StdEncoding.EncodeToString(key)
}

func newTestSealer(t *testing.T) *Sealer {
	t.Helper()
	s, err := NewSealer(map[int]string{1: testKey(0x11)})
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	return s
}

func TestSealRoundTrip(t *testing.T) {
	s := newTestSealer(t)
	aad := CredentialAAD("default", "user-1", "github")
	secret := []byte("github_pat_11ABCDEFG0abcdefghijklmnop")

	ct, nonce, version, err := s.Seal(secret, aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if version != 1 {
		t.Errorf("version = %d, want 1", version)
	}
	if bytes.Contains(ct, secret) {
		t.Error("the ciphertext contains the plaintext")
	}
	got, err := s.Open(ct, nonce, version, aad)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, secret) {
		t.Errorf("Open = %q, want %q", got, secret)
	}
}

// Two seals of the same value must differ: a deterministic ciphertext
// would tell anyone with the database which users share a token.
func TestSealIsRandomized(t *testing.T) {
	s := newTestSealer(t)
	aad := CredentialAAD("default", "user-1", "github")
	a, nonceA, _, err := s.Seal([]byte("same"), aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	b, nonceB, _, err := s.Seal([]byte("same"), aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Equal(nonceA, nonceB) {
		t.Error("two seals reused a nonce")
	}
	if bytes.Equal(a, b) {
		t.Error("two seals of the same plaintext produced the same ciphertext")
	}
}

// The AAD is what stops a row being moved. Without it, a database-level
// attacker could copy an admin's ciphertext into their own row.
func TestOpenRejectsAnotherUsersRow(t *testing.T) {
	s := newTestSealer(t)
	owner := CredentialAAD("default", "victim", "github")
	ct, nonce, version, err := s.Seal([]byte("secret"), owner)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	for _, tc := range []struct {
		name string
		aad  []byte
	}{
		{"another user", CredentialAAD("default", "attacker", "github")},
		{"another platform", CredentialAAD("default", "victim", "forgejo:example.com")},
		{"another org", CredentialAAD("other", "victim", "github")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.Open(ct, nonce, version, tc.aad); err == nil {
				t.Fatal("Open succeeded against a different row")
			}
		})
	}
}

func TestOpenRejectsTamperedCiphertext(t *testing.T) {
	s := newTestSealer(t)
	aad := CredentialAAD("default", "user-1", "github")
	ct, nonce, version, err := s.Seal([]byte("secret"), aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	ct[0] ^= 0xff
	if _, err := s.Open(ct, nonce, version, aad); err == nil {
		t.Fatal("Open accepted a tampered ciphertext")
	}
}

func TestOpenRejectsWrongKey(t *testing.T) {
	a := newTestSealer(t)
	b, err := NewSealer(map[int]string{1: testKey(0x22)})
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	aad := CredentialAAD("default", "user-1", "github")
	ct, nonce, version, err := a.Seal([]byte("secret"), aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := b.Open(ct, nonce, version, aad); err == nil {
		t.Fatal("Open succeeded under a different key")
	}
}

// Rotation: both keys are configured, new writes use the higher
// version, and rows sealed under the old one still open.
func TestRotation(t *testing.T) {
	old, err := NewSealer(map[int]string{1: testKey(0x11)})
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	aad := CredentialAAD("default", "user-1", "github")
	ct, nonce, version, err := old.Seal([]byte("secret"), aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	both, err := NewSealer(map[int]string{1: testKey(0x11), 2: testKey(0x22)})
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	if both.Version() != 2 {
		t.Errorf("Version = %d, want 2", both.Version())
	}
	got, err := both.Open(ct, nonce, version, aad)
	if err != nil {
		t.Fatalf("Open under rotation: %v", err)
	}
	if string(got) != "secret" {
		t.Errorf("Open = %q", got)
	}
	// New writes go under the new key.
	_, _, v, err := both.Seal([]byte("secret"), aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if v != 2 {
		t.Errorf("new seal used version %d, want 2", v)
	}
}

// A key's version comes from the key, so the same key always seals and
// opens under the same version however it is presented.
func TestKeyVersionIsStable(t *testing.T) {
	key := bytes.Repeat([]byte{0x11}, KeyBytes)
	v1, v2 := KeyVersion(key), KeyVersion(key)
	if v1 != v2 {
		t.Errorf("KeyVersion is not stable: %d then %d", v1, v2)
	}
	if v1 <= 0 {
		t.Errorf("KeyVersion = %d, want a positive value", v1)
	}
	other := KeyVersion(bytes.Repeat([]byte{0x22}, KeyBytes))
	if other == v1 {
		t.Error("two different keys derived the same version")
	}
}

func TestNewSealerFor(t *testing.T) {
	current := testKey(0x11)
	retired := testKey(0x22)
	aad := CredentialAAD("org", "u", "github")

	// Sealed under the retired key, before it was retired.
	old, err := NewSealerFor(retired)
	if err != nil {
		t.Fatalf("NewSealerFor: %v", err)
	}
	ct, nonce, version, err := old.Seal([]byte("secret"), aad)
	if err != nil {
		t.Fatal(err)
	}

	// After rotation both keys are configured; the row still opens and
	// new writes take the current key's version.
	both, err := NewSealerFor(current, retired)
	if err != nil {
		t.Fatalf("NewSealerFor: %v", err)
	}
	got, err := both.Open(ct, nonce, version, aad)
	if err != nil {
		t.Fatalf("a row sealed under the retired key did not open: %v", err)
	}
	if string(got) != "secret" {
		t.Errorf("Open = %q", got)
	}
	if _, _, v, err := both.Seal([]byte("new"), aad); err != nil || v == version {
		t.Errorf("new writes took version %d (the retired key's), err=%v", v, err)
	}
	// The order the keys are given in must not change what opens.
	swapped, err := NewSealerFor(retired, current)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := swapped.Open(ct, nonce, version, aad); err != nil {
		t.Errorf("swapping the keys made the row unreadable: %v", err)
	}
}

func TestNewSealerForRejectsNoKey(t *testing.T) {
	if _, err := NewSealerFor(""); !errors.Is(err, ErrNoKey) {
		t.Fatalf("err = %v, want ErrNoKey", err)
	}
}

func TestNewSealerForIgnoresAnEmptyRetiredKey(t *testing.T) {
	s, err := NewSealerFor(testKey(0x11), "")
	if err != nil {
		t.Fatalf("NewSealerFor: %v", err)
	}
	if len(s.keys) != 1 {
		t.Errorf("an empty retired key was registered: %d keys", len(s.keys))
	}
}

func TestNewSealerForRejectsABadKey(t *testing.T) {
	if _, err := NewSealerFor("not base64 !!"); err == nil {
		t.Error("a malformed current key was accepted")
	}
	if _, err := NewSealerFor(testKey(0x11), "not base64 !!"); err == nil {
		t.Error("a malformed retired key was accepted")
	}
}

// The same key given twice is harmless.
func TestNewSealerForToleratesADuplicateKey(t *testing.T) {
	key := testKey(0x11)
	s, err := NewSealerFor(key, key)
	if err != nil {
		t.Fatalf("NewSealerFor: %v", err)
	}
	if len(s.keys) != 1 {
		t.Errorf("a duplicate key was registered twice: %d keys", len(s.keys))
	}
}

func TestOpenUnknownVersionNamesIt(t *testing.T) {
	s := newTestSealer(t)
	_, err := s.Open([]byte("x"), make([]byte, 12), 7, nil)
	if err == nil || !strings.Contains(err.Error(), "version 7") {
		t.Fatalf("err = %v, want it to name version 7", err)
	}
}

func TestOpenRejectsWrongNonceLength(t *testing.T) {
	s := newTestSealer(t)
	aad := CredentialAAD("default", "user-1", "github")
	ct, _, version, err := s.Seal([]byte("secret"), aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := s.Open(ct, []byte("short"), version, aad); err == nil {
		t.Fatal("Open accepted a short nonce")
	}
}

func TestNewSealerRejectsBadKeys(t *testing.T) {
	short := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 16))
	for _, tc := range []struct {
		name string
		keys map[int]string
		want string
	}{
		{"no keys", nil, "no credential key"},
		{"not base64", map[int]string{1: "!!!not base64!!!"}, "not valid base64"},
		{"too short", map[int]string{1: short}, "need 32"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewSealer(tc.keys)
			if err == nil {
				t.Fatal("NewSealer accepted a bad key")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A key must never reach an error message, however malformed.
func TestKeyErrorsDoNotLeakTheKey(t *testing.T) {
	secret := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xAB}, 16))
	_, err := NewSealer(map[int]string{1: secret})
	if err == nil {
		t.Fatal("NewSealer accepted a short key")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the error contains the key: %v", err)
	}
}

// Raw (unpadded) base64 is what some secret managers hand back.
func TestNewSealerAcceptsRawBase64(t *testing.T) {
	raw := base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{0x33}, KeyBytes))
	if _, err := NewSealer(map[int]string{1: raw}); err != nil {
		t.Fatalf("NewSealer with raw base64: %v", err)
	}
}
