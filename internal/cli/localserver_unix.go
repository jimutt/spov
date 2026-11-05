//go:build !windows

package cli

import (
	"os"
	"syscall"
)

// processAlive reports whether a process exists. Signal 0 checks
// without delivering anything; EPERM means it exists but is not ours,
// which for a state file this user wrote is as good as dead.
//
// @decision(DL-002)
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// terminateProcess asks a process to exit.
//
// @decision(DL-002)
func terminateProcess(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Signal(syscall.SIGTERM)
	}
}
