//go:build unix

package credproxy

import (
	"fmt"
	"os"
	"syscall"
)

// lockFile takes an exclusive lock beside path for a read-modify-write
// of it, so a set-token and a set-key run from two server replicas take
// turns rather than one undoing the other.
//
// @decision(DL-015) @decision(DL-021)
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("credproxy: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("credproxy: lock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
