//go:build windows

package cli

import "os/exec"

// detachProcess is a no-op on Windows, where the opener is `cmd /c start`
// and hands off without holding the console.
//
// @decision(DL-025)
func detachProcess(*exec.Cmd) {}
