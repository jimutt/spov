//go:build unix

package credentials

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestFileStoreUnderPermissiveUmask checks the file never lands with wider
// permissions than 0600 even when the process umask would allow it.
func TestFileStoreUnderPermissiveUmask(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}
	// A permissive umask must not widen the file: CreateTemp respects
	// umask, so the explicit chmod is what guarantees 0600.
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })

	s := NewFileStore(filepath.Join(t.TempDir(), "credentials.json"))
	if err := s.Set("k", "secret"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != FileMode {
		t.Errorf("with umask 0 the file is %04o, want %04o", perm, FileMode)
	}
	dir, err := os.Stat(filepath.Dir(s.Path()))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("dir mode %04o, file mode %04o", dir.Mode().Perm(), info.Mode().Perm())
}
