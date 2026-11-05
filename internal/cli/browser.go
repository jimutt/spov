package cli

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// openBrowser points the system browser at a URL.
//
// Failing to open a browser never fails a session: the URL is printed
// either way, and a session that is up and reachable is not broken by a
// missing opener.
//
// @decision(DL-025)
func openBrowser(url string, env *Env) {
	opener, args := browserCommand(env)
	if opener == "" {
		return
	}
	// Deliberately not CommandContext: cancelling spov's context must
	// not kill the user's browser.
	cmd := exec.Command(opener, append(args, url)...)
	// Fully detached from spov's terminal; an opener that execs the
	// browser in the foreground rather than handing off otherwise
	// inherits spov's stdin and competes with it for the terminal.
	//
	// @decision(DL-025)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	detachProcess(cmd)
	if err := cmd.Start(); err != nil {
		env.deferWarning(fmt.Sprintf("spov: warning: could not open a browser (%v); open the URL above yourself", err))
		return
	}
	// Reaped in the background: an opener typically hands off to an
	// already-running browser and exits, but waiting for it would block
	// the session behind a browser that does not.
	go func() { _ = cmd.Wait() }()
}

// browserCommand picks the platform's URL opener.
//
// On WSL the Linux opener is the wrong one: xdg-open is often absent, and
// when present is frequently wired to nothing, so the Windows browser is
// preferred there.
//
// @decision(DL-025)
func browserCommand(env *Env) (string, []string) {
	if runtime.GOOS == "darwin" {
		return lookPath(env, "open"), nil
	}
	if runtime.GOOS == "windows" {
		return lookPath(env, "cmd.exe"), []string{"/c", "start", ""}
	}
	if isWSL(env) {
		if p := lookPath(env, "wslview"); p != "" {
			return p, nil
		}
		if p := lookPath(env, "cmd.exe"); p != "" {
			// The empty argument is start's window-title slot; without it
			// start treats a quoted URL as the title and opens nothing.
			return p, []string{"/c", "start", ""}
		}
	}
	return lookPath(env, "xdg-open"), nil
}

// isWSL reports whether spov is running under the Windows Subsystem for
// Linux, where the browser lives on the Windows side.
//
// @decision(DL-025)
func isWSL(env *Env) bool {
	if env.Environ.Getenv("WSL_DISTRO_NAME") != "" || env.Environ.Getenv("WSL_INTEROP") != "" {
		return true
	}
	// Older WSL builds set neither variable but always report a kernel
	// naming Microsoft. Read through the Env so a test can state which
	// kernel it means rather than inheriting the one it runs on.
	read := env.ReadFile
	if read == nil {
		read = os.ReadFile
	}
	data, err := read("/proc/version")
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(data)), "microsoft")
}

// lookPath finds an executable, returning empty when it is absent.
func lookPath(env *Env, name string) string {
	if env.LookPath != nil {
		path, err := env.LookPath(name)
		if err != nil {
			return ""
		}
		return path
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return path
}
