//go:build !windows

package cli

import (
	"os/exec"
	"syscall"
)

// detachProcess puts the browser in its own session, so it does not share
// spov's controlling terminal and is not signalled when the user presses
// Ctrl-C to end the session.
//
// @decision(DL-025)
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
