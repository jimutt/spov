package credentials

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newFileStore(t *testing.T) *FileStore {
	t.Helper()
	return NewFileStore(filepath.Join(t.TempDir(), "credentials.json"))
}

func TestFileStoreRoundTrip(t *testing.T) {
	s := newFileStore(t)

	if _, err := s.Get("platform:codefloe"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get on an empty store: %v, want ErrNotFound", err)
	}
	if err := s.Set("platform:codefloe", "tok-1"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("platform:codefloe")
	if err != nil {
		t.Fatal(err)
	}
	if got != "tok-1" {
		t.Errorf("secret = %q", got)
	}

	// A second store on the same path must see it: `spov attach` runs in
	// a different process from `spov fly`.
	if got, err := NewFileStore(s.Path()).Get("platform:codefloe"); err != nil || got != "tok-1" {
		t.Errorf("reopened store: %q, %v", got, err)
	}

	if err := s.Set("platform:codefloe", "tok-2"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get("platform:codefloe"); got != "tok-2" {
		t.Errorf("secret was not overwritten: %q", got)
	}

	if err := s.Delete("platform:codefloe"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("platform:codefloe"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete: %v, want ErrNotFound", err)
	}
	if err := s.Delete("platform:codefloe"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete of a missing key: %v, want ErrNotFound", err)
	}
}

// TestFileStorePermissions is the security property of this backend: the
// file must never be readable by anyone but its owner.
//
// @decision(DL-008)
func TestFileStorePermissions(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission bits are not enforced")
	}
	s := newFileStore(t)
	if err := s.Set("k", "secret"); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != FileMode {
		t.Errorf("credentials file mode = %04o, want %04o", perm, FileMode)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Error("the credentials file is readable or writable by others")
	}

	// The file is 0600, but a writable directory lets someone replace it.
	dir, err := os.Stat(filepath.Dir(s.Path()))
	if err != nil {
		t.Fatal(err)
	}
	if dir.Mode().Perm()&0o022 != 0 {
		t.Errorf("the credentials directory is writable by others: %04o", dir.Mode().Perm())
	}
}

// TestFileStoreRefusesWideOpenFile: if something loosened the permissions,
// reading secrets out of it anyway would hide the problem.
func TestFileStoreRefusesWideOpenFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission bits are not enforced")
	}
	s := newFileStore(t)
	if err := s.Set("k", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.Path(), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := s.Get("k")
	if err == nil {
		t.Fatal("a world-readable credentials file was accepted")
	}
	if !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("error %q does not tell the user how to fix it", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Error("the error message leaked a secret value")
	}
}

func TestFileStoreWarnsOnFirstWrite(t *testing.T) {
	s := newFileStore(t)
	var warnings []string
	s.Warn = func(msg string) { warnings = append(warnings, msg) }

	const secret = "sk-ant-do-not-print-me"
	for range 3 {
		if err := s.Set("k", secret); err != nil {
			t.Fatal(err)
		}
	}
	if len(warnings) != 1 {
		t.Fatalf("got %d warnings, want exactly 1: %v", len(warnings), warnings)
	}
	w := warnings[0]
	if !strings.Contains(w, "UNENCRYPTED") {
		t.Errorf("the warning does not say the file is unencrypted: %q", w)
	}
	if !strings.Contains(w, s.Path()) {
		t.Errorf("the warning does not name the file: %q", w)
	}
	if strings.Contains(w, secret) {
		t.Errorf("the warning leaked the secret: %q", w)
	}
}

func TestFileStoreWithoutWarnFunc(t *testing.T) {
	s := newFileStore(t)
	if err := s.Set("k", "v"); err != nil {
		t.Fatalf("a nil Warn must not panic: %v", err)
	}
}

func TestFileStoreKeys(t *testing.T) {
	s := newFileStore(t)
	for _, k := range []string{"sandbox:sb-2", "platform:codefloe", "sandbox:sb-1"} {
		if err := s.Set(k, "secret-"+k); err != nil {
			t.Fatal(err)
		}
	}
	keys, err := s.Keys()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"platform:codefloe", "sandbox:sb-1", "sandbox:sb-2"}
	if len(keys) != len(want) {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("keys = %v, want %v", keys, want)
		}
	}
	// Keys must never carry values with them.
	for _, k := range keys {
		if strings.Contains(k, "secret-") {
			t.Errorf("a secret leaked into the key list: %q", k)
		}
	}
}

func TestFileStoreHandlesEmptyAndCorruptFiles(t *testing.T) {
	t.Run("empty file is an empty store", func(t *testing.T) {
		s := newFileStore(t)
		if err := os.MkdirAll(filepath.Dir(s.Path()), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(s.Path(), nil, FileMode); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Get("k"); !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("corrupt file is reported, not silently discarded", func(t *testing.T) {
		s := newFileStore(t)
		if err := os.MkdirAll(filepath.Dir(s.Path()), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(s.Path(), []byte("{not json"), FileMode); err != nil {
			t.Fatal(err)
		}
		_, err := s.Get("k")
		if err == nil || errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v; a corrupt file must not look like a missing key", err)
		}
		// Overwriting would destroy recoverable secrets.
		if err := s.Set("k", "v"); err == nil {
			t.Error("Set overwrote a file it could not parse")
		}
	})

	t.Run("json null is an empty store", func(t *testing.T) {
		s := newFileStore(t)
		if err := os.MkdirAll(filepath.Dir(s.Path()), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(s.Path(), []byte("null"), FileMode); err != nil {
			t.Fatal(err)
		}
		if err := s.Set("k", "v"); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.Get("k"); got != "v" {
			t.Errorf("secret = %q", got)
		}
	})
}

// TestFileStoreWriteIsAtomic checks no partial file is left behind: a
// truncated write would lose every stored secret at once.
func TestFileStoreWriteIsAtomic(t *testing.T) {
	s := newFileStore(t)
	if err := s.Set("a", "1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("b", "2"); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(filepath.Dir(s.Path()))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("a temporary file was left behind: %s", e.Name())
		}
	}
	if got, _ := s.Get("a"); got != "1" {
		t.Error("the second write lost the first secret")
	}
}

func TestFileStoreReportsUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission bits are not enforced")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(path, []byte("{}"), 0o000); err != nil {
		t.Fatal(err)
	}
	_, err := NewFileStore(path).Get("k")
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Error("an unreadable file was reported as missing")
	}
}

func TestDefaultFilePath(t *testing.T) {
	got := DefaultFilePath("/home/u/.config/spov")
	want := filepath.Join("/home/u/.config/spov", "credentials.json")
	if got != want {
		t.Errorf("DefaultFilePath = %q, want %q", got, want)
	}
}

func TestFileStoreName(t *testing.T) {
	s := newFileStore(t)
	if !strings.HasPrefix(s.Name(), "file:") || !strings.Contains(s.Name(), s.Path()) {
		t.Errorf("Name = %q; it must tell the user where secrets live", s.Name())
	}
}

// TestFileStoreTightensExistingDirectory covers the realistic case:
// ~/.config/spov already exists with default permissions when spov first
// writes a secret. MkdirAll leaves an existing directory alone, so the
// store has to tighten it.
func TestFileStoreTightensExistingDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission bits are not enforced")
	}
	dir := filepath.Join(t.TempDir(), "spov")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}

	s := NewFileStore(filepath.Join(dir, "credentials.json"))
	if err := s.Set("k", "v"); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		t.Errorf("directory left writable by others: %04o", perm)
	}
}

// TestFileStoreSaveErrorPaths covers the security-relevant failure modes
// of the write path: a failed write must leave no temporary file holding
// secrets, and must not destroy what was already stored.
func TestFileStoreSaveErrorPaths(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission bits are not enforced")
	}

	t.Run("read-only directory", func(t *testing.T) {
		dir := t.TempDir()
		s := NewFileStore(filepath.Join(dir, "credentials.json"))
		if err := s.Set("k", "first"); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

		err := s.Set("k2", "second")
		if err == nil {
			t.Fatal("a write to a read-only directory succeeded")
		}
		if strings.Contains(err.Error(), "second") {
			t.Errorf("the error leaked the secret: %q", err)
		}

		// The earlier secret must survive a failed write.
		_ = os.Chmod(dir, 0o700)
		if got, err := s.Get("k"); err != nil || got != "first" {
			t.Errorf("a failed write destroyed an existing secret: %q, %v", got, err)
		}
	})

	t.Run("no temporary file survives a failure", func(t *testing.T) {
		dir := t.TempDir()
		s := NewFileStore(filepath.Join(dir, "credentials.json"))
		if err := s.Set("k", "v"); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		_ = s.Set("k2", "v2")
		_ = os.Chmod(dir, 0o700)

		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.Contains(e.Name(), ".tmp") {
				t.Errorf("a temporary file holding secrets was left behind: %s", e.Name())
			}
		}
	})

	t.Run("path is a directory", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "credentials.json")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := NewFileStore(path).Set("k", "v"); err == nil {
			t.Error("writing over a directory succeeded")
		}
	})
}
