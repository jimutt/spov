package routedcred

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/jimutt/spov/internal/credentials"
)

// A route's encoding is its normal form, so the same route seals the same
// way however it was written, and a changed route seals differently.
//
// @decision(DL-073)
func TestTheSealBindsTheRoute(t *testing.T) {
	a := Route{Name: "acme", Hosts: []string{"NPM.acme.com."}, Scheme: "bearer"}
	b := Route{Name: "acme", Hosts: []string{"npm.acme.com"}, Scheme: "bearer", Attach: AttachAlways}
	if !bytes.Equal(a.Encode(), b.Encode()) {
		t.Errorf("%s != %s", a.Encode(), b.Encode())
	}
	moved := b
	moved.Hosts = []string{"evil.example.com"}
	base := []byte("org\x00user\x00route:acme")
	if bytes.Equal(AAD(base, b), AAD(base, moved)) {
		t.Error("a moved route seals the same")
	}
	back, err := Decode(b.Encode())
	if err != nil || !bytes.Equal(back.Encode(), b.Encode()) {
		t.Errorf("decode = %+v, %v", back, err)
	}
}

// The local index is written whole, readable by its owner only, holds at
// most MaxPerUser credentials, and refuses a version it does not know.
//
// @decision(DL-073)
func TestTheLocalIndexIsTheOwnersAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", IndexName)
	l := &Local{Creds: credentials.NewMemoryStore(), Path: path}
	for i := range MaxPerUser {
		r := Route{Name: "r" + string(rune('a'+i/26)) + string(rune('a'+i%26)), Hosts: []string{"h.acme.com"}, Scheme: "bearer"}
		if err := l.Put(r, "s"); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Put(Route{Name: "one-more", Hosts: []string{"h.acme.com"}, Scheme: "bearer"}, "s"); err == nil {
		t.Error("one past the limit was stored")
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != credentials.FileMode {
		t.Errorf("index = %v, %v", info, err)
	}
	if err := os.WriteFile(path, []byte(`{"version":2,"routes":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := l.List(); err == nil {
		t.Error("an index of an unknown version was read")
	}
	if err := l.Put(Route{Name: "bad", Hosts: []string{"npm.acme.com"}, Scheme: "basic"}, "s"); err == nil {
		t.Error("a Basic route without a user was stored")
	}
}
