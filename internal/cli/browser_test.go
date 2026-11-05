package cli

import (
	"errors"
	"os/exec"
	"runtime"
	"testing"

	"github.com/jimutt/spov/internal/config"
)

// TestBrowserCommandPrefersWSLViewUnderWSL: on WSL the Linux opener is
// the wrong one — xdg-open is often absent, and when present is
// frequently wired to nothing — so the Windows browser wins.
//
// @decision(DL-025)
func TestBrowserCommandPrefersWSLViewUnderWSL(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the WSL path only applies on Linux")
	}
	env := envWithLookPath(map[string]string{"WSL_DISTRO_NAME": "Ubuntu"},
		map[string]string{"wslview": "/usr/bin/wslview", "xdg-open": "/usr/bin/xdg-open", "cmd.exe": "/mnt/c/cmd.exe"})

	got, args := browserCommand(env)
	if got != "/usr/bin/wslview" {
		t.Errorf("opener = %q, want wslview even though xdg-open exists", got)
	}
	if len(args) != 0 {
		t.Errorf("wslview was given arguments %v", args)
	}
}

// TestBrowserCommandFallsBackToCmdExeUnderWSL covers the common WSL
// install where wslview is not present.
//
// @decision(DL-025)
func TestBrowserCommandFallsBackToCmdExeUnderWSL(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the WSL path only applies on Linux")
	}
	env := envWithLookPath(map[string]string{"WSL_INTEROP": "/run/WSL/1_interop"},
		map[string]string{"cmd.exe": "/mnt/c/Windows/System32/cmd.exe", "xdg-open": "/usr/bin/xdg-open"})

	got, args := browserCommand(env)
	if got != "/mnt/c/Windows/System32/cmd.exe" {
		t.Errorf("opener = %q, want cmd.exe", got)
	}
	// start treats its first quoted argument as a window title, so the
	// empty slot is what keeps it from swallowing the URL.
	if len(args) != 3 || args[0] != "/c" || args[1] != "start" || args[2] != "" {
		t.Errorf("args = %q, want [/c start \"\"]", args)
	}
}

// TestBrowserCommandUsesXdgOpenOnPlainLinux.
//
// @decision(DL-025)
func TestBrowserCommandUsesXdgOpenOnPlainLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	env := envWithLookPath(map[string]string{"kernel": "Linux version 6.6.0-generic"},
		map[string]string{"xdg-open": "/usr/bin/xdg-open"})

	got, _ := browserCommand(env)
	if got != "/usr/bin/xdg-open" {
		t.Errorf("opener = %q, want xdg-open", got)
	}
}

// TestBrowserCommandDetectsOldWSLByKernel covers WSL builds that set
// neither environment variable.
//
// @decision(DL-025)
func TestBrowserCommandDetectsOldWSLByKernel(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	env := envWithLookPath(map[string]string{"kernel": "Linux version 6.6.87.2-microsoft-standard-WSL2"},
		map[string]string{"wslview": "/usr/bin/wslview", "xdg-open": "/usr/bin/xdg-open"})

	if got, _ := browserCommand(env); got != "/usr/bin/wslview" {
		t.Errorf("opener = %q, want wslview: an old WSL kernel was not detected", got)
	}
}

// TestBrowserCommandFallsBackToXdgOpenUnderWSL: WSL with neither
// wslview nor cmd.exe on PATH falls back to xdg-open.
//
// @decision(DL-025)
func TestBrowserCommandFallsBackToXdgOpenUnderWSL(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	env := envWithLookPath(map[string]string{"WSL_DISTRO_NAME": "Ubuntu"},
		map[string]string{"xdg-open": "/usr/bin/xdg-open"})

	if got, _ := browserCommand(env); got != "/usr/bin/xdg-open" {
		t.Errorf("opener = %q, want xdg-open as the last resort", got)
	}
}

// TestBrowserCommandReportsNoOpener: a missing opener is not an error,
// and the caller prints the URL instead.
//
// @decision(DL-025)
func TestBrowserCommandReportsNoOpener(t *testing.T) {
	env := envWithLookPath(map[string]string{"kernel": "Linux version 6.6.0-generic"}, nil)
	if got, _ := browserCommand(env); got != "" {
		t.Errorf("opener = %q, want none", got)
	}
}

// envWithLookPath builds an Env whose environment and PATH lookups are
// both canned, so the test does not depend on the machine it runs on.
func envWithLookPath(vars, paths map[string]string) *Env {
	return &Env{
		Environ: config.MapEnv{Vars: vars},
		LookPath: func(name string) (string, error) {
			if p, ok := paths[name]; ok {
				return p, nil
			}
			return "", errors.New("not found")
		},
		// Pinned rather than inherited: a test that read the real
		// /proc/version would take the WSL branch on a WSL machine and
		// pass for the wrong reason.
		ReadFile: func(string) ([]byte, error) {
			return []byte(vars["kernel"]), nil
		},
	}
}

// TestBrowserIsDetachedFromTheTerminal: an opener that execs the browser
// in the foreground rather than handing off would otherwise inherit
// spov's stdin and compete for the terminal.
//
// @decision(DL-025)
func TestBrowserIsDetachedFromTheTerminal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the opener hands off on Windows")
	}
	cmd := exec.Command("/bin/true")
	detachProcess(cmd)

	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Error("the browser shares spov's session, so Ctrl-C would signal it and it holds the terminal")
	}
}
