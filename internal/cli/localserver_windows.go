//go:build windows

package cli

import "os"

// processAlive reports whether a process exists. On Windows FindProcess
// opens a handle, which fails for a pid that is gone; signals other
// than Kill are not supported, so Signal(0) cannot be used.
//
// @decision(DL-002)
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}

// terminateProcess ends a process. Windows has no SIGTERM; Kill is the
// only signal, and the server's state file is cleaned up by the CLI
// that replaces it rather than by the server on its way out.
//
// @decision(DL-002)
func terminateProcess(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}
