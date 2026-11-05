package opencode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// TUI describes a client process to hand the terminal to.
//
// @decision(DL-025)
type TUI struct {
	// BinaryPath is the local opencode to run.
	BinaryPath string
	// ServerURL is the loopback proxy fronting the sandbox. The proxy
	// authenticates on the client's behalf, so no credential is passed
	// to the child.
	//
	// @decision(DL-030)
	ServerURL string
	// Session is the OpenCode session to open (`ses_…`). Empty means the
	// client opens on its home screen, which is what a fresh sandbox
	// wants.
	//
	// @decision(DL-025)
	Session string
	// Stdin, Stdout and Stderr are inherited by the child. A full-screen
	// TUI cannot be piped.
	Stdin  *os.File
	Stdout *os.File
	Stderr *os.File
	// Environ is the base environment; defaults to the process's own.
	Environ []string
}

// Command builds the client invocation. No directory argument is passed:
// the client chdirs locally, which says nothing about the remote working
// directory. A given session is resolved by the server, naming a
// conversation in the sandbox rather than the user's local store.
//
// @decision(DL-025)
func (t TUI) Command(ctx context.Context) (*exec.Cmd, error) {
	if t.BinaryPath == "" {
		return nil, fmt.Errorf("opencode: no client binary to run")
	}
	if t.ServerURL == "" {
		return nil, fmt.Errorf("opencode: no server URL to connect to")
	}

	args := []string{"--server", t.ServerURL}
	if t.Session != "" {
		args = append(args, "--session", t.Session)
	}
	cmd := exec.CommandContext(ctx, t.BinaryPath, args...)
	cmd.Stdin = orStd(t.Stdin, os.Stdin)
	cmd.Stdout = orStd(t.Stdout, os.Stdout)
	cmd.Stderr = orStd(t.Stderr, os.Stderr)

	base := t.Environ
	if base == nil {
		base = os.Environ()
	}
	// The password is deliberately absent and any inherited one is
	// removed: the client authenticates through the proxy, and a
	// password in a child's environment is readable by every process
	// running as this user.
	//
	// @decision(DL-030)
	cmd.Env = scrubEnv(base)

	// A TUI must be interrupted by the user's own Ctrl-C via the
	// terminal's process group, not killed when spov's context is
	// cancelled mid-session.
	cmd.Cancel = func() error { return nil }
	return cmd, nil
}

// Run hands the terminal to the client and waits for it to exit. The exit
// code is returned rather than treated as an error: a user quitting the
// TUI is not a spov failure. Ctrl-C signals the whole foreground process
// group, so the child exits cleanly while exec reports the cancelled
// context; that combination is reported as a normal exit rather than a
// failure.
//
// @decision(DL-025) @decision(DL-029)
func (t TUI) Run(ctx context.Context) (exitCode int, err error) {
	cmd, err := t.Command(ctx)
	if err != nil {
		return 0, err
	}

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("opencode: start the client: %w", err)
	}

	// Past Start the process exists, so a cancelled context means the
	// session was interrupted rather than that spov failed to run it.
	runErr := cmd.Wait()
	if runErr == nil {
		return 0, nil
	}

	var exit *exec.ExitError
	if errors.As(runErr, &exit) {
		return exit.ExitCode(), nil
	}
	if ctx.Err() != nil {
		return interruptedExitCode, nil
	}
	return 0, fmt.Errorf("opencode: run the client: %w", runErr)
}

// interruptedExitCode is the conventional shell code for a process ended
// by SIGINT (128 + 2). The caller uses it to tell an interrupted session
// from a clean quit without treating either as an error.
const interruptedExitCode = 130

// scrubEnv removes any inherited OPENCODE_PASSWORD, and the
// OPENCODE_SERVER_PASSWORD the client falls back to, so a password in the
// caller's environment never reaches the client.
//
// @decision(DL-035)
func scrubEnv(env []string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, PasswordEnvVar+"=") || strings.HasPrefix(kv, serverPasswordEnvVar+"=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// serverPasswordEnvVar is the variable the client reads when
// PasswordEnvVar is unset.
const serverPasswordEnvVar = "OPENCODE_SERVER_PASSWORD"

func orStd(f, fallback *os.File) *os.File {
	if f != nil {
		return f
	}
	return fallback
}
