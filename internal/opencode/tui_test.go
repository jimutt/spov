package opencode

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestTUICommand(t *testing.T) {
	tui := TUI{
		BinaryPath: "/usr/local/bin/opencode",
		ServerURL:  "http://127.0.0.1:45123",
		Environ:    []string{"PATH=/usr/bin", "TERM=xterm"},
	}
	cmd, err := tui.Command(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"/usr/local/bin/opencode", "--server", "http://127.0.0.1:45123"}
	if !slices.Equal(cmd.Args, want) {
		t.Errorf("args = %v, want %v", cmd.Args, want)
	}
	// A directory argument would make the TUI chdir locally, which says
	// nothing about the remote working directory.
	if len(cmd.Args) != 3 {
		t.Errorf("unexpected extra arguments: %v", cmd.Args)
	}
	// The client authenticates through the proxy, so it must be given no
	// credential of its own.
	//
	// @decision(DL-030)
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, PasswordEnvVar+"=") {
			t.Errorf("the client was given a password: %q", kv)
		}
	}
}

// TestTUIScrubsInheritedPassword: a password in the user's own
// environment must not reach the client, which would make it authenticate
// directly rather than through the proxy.
//
// @decision(DL-025) @decision(DL-030)
// @decision(DL-035)
func TestTUIScrubsInheritedPassword(t *testing.T) {
	tui := TUI{
		BinaryPath: "/bin/true",
		ServerURL:  "http://127.0.0.1:1",
		Environ:    []string{PasswordEnvVar + "=stale", serverPasswordEnvVar + "=stale", "PATH=/usr/bin"},
	}
	cmd, err := tui.Command(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, PasswordEnvVar+"=") || strings.HasPrefix(kv, serverPasswordEnvVar+"=") {
			t.Errorf("an inherited password reached the client: %q", kv)
		}
	}
	if !slices.Contains(cmd.Env, "PATH=/usr/bin") {
		t.Error("the rest of the environment was discarded")
	}
}

func TestTUICommandRejects(t *testing.T) {
	tests := []struct {
		name string
		tui  TUI
		want string
	}{
		{"no binary", TUI{ServerURL: "http://127.0.0.1:1"}, "client binary"},
		{"no server", TUI{BinaryPath: "/bin/true"}, "server URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.tui.Command(context.Background())
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}

func TestTUIRun(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-opencode")
	// The stand-in records its arguments and environment, then exits.
	body := "#!/bin/sh\n" +
		"echo \"args: $*\" > " + dir + "/out\n" +
		"echo \"password: [$" + PasswordEnvVar + "]\" >> " + dir + "/out\n" +
		"exit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	code, err := TUI{
		BinaryPath: script,
		ServerURL:  "http://127.0.0.1:45123",
		Environ:    []string{PasswordEnvVar + "=leaked", "PATH=/usr/bin:/bin"},
	}.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Errorf("exit code = %d", code)
	}

	out, err := os.ReadFile(filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "--server http://127.0.0.1:45123") {
		t.Errorf("the client did not receive the server URL: %s", out)
	}
	// Checked in the child rather than in cmd.Env, so an environment
	// leaking through some other path would still be caught.
	//
	// @decision(DL-030)
	if !strings.Contains(string(out), "password: []") {
		t.Errorf("a password reached the running client: %s", out)
	}
}

// TestTUIRunReportsExitCode: a user quitting the TUI is not a spov
// failure, so a non-zero exit is returned rather than raised.
//
// @decision(DL-025)
func TestTUIRunReportsExitCode(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "failing")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	code, err := TUI{BinaryPath: script, ServerURL: "http://127.0.0.1:1"}.Run(context.Background())
	if err != nil {
		t.Fatalf("a non-zero exit must not be an error: %v", err)
	}
	if code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
}

func TestTUIRunReportsRealFailures(t *testing.T) {
	_, err := TUI{
		BinaryPath: filepath.Join(t.TempDir(), "does-not-exist"),
		ServerURL:  "http://127.0.0.1:1",
	}.Run(context.Background())
	if err == nil {
		t.Fatal("a missing binary was not reported")
	}
}

func TestTUIRunRejectsInvalidConfig(t *testing.T) {
	if _, err := (TUI{}).Run(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
}

// TestTUIRunTreatsCancellationAsANormalExit is the Ctrl-C path: the
// terminal signals the whole process group, so the child exits cleanly
// while exec reports the cancelled context.
//
// @decision(DL-025) @decision(DL-029)
func TestTUIRunTreatsCancellationAsANormalExit(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "sleeper")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 0.3\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	code, err := TUI{BinaryPath: script, ServerURL: "http://127.0.0.1:1"}.Run(ctx)
	if err != nil {
		t.Fatalf("an interrupted session must not be reported as a failure: %v", err)
	}
	if code != interruptedExitCode {
		t.Errorf("exit code = %d, want %d", code, interruptedExitCode)
	}
}

// TestTUIRunStillReportsRealFailuresUnderCancellation guards against the
// fix swallowing genuine errors.
func TestTUIRunStillReportsRealFailuresUnderCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := TUI{
		BinaryPath: filepath.Join(t.TempDir(), "missing"),
		ServerURL:  "http://127.0.0.1:1",
	}.Run(ctx)
	if err == nil {
		t.Fatal("a missing binary was reported as a normal exit")
	}
}

// TestTUIOpensTheGivenSession pins that a session reaches the client as
// `--session`, a plain argument since it is not a credential.
//
// @decision(DL-025)
func TestTUIOpensTheGivenSession(t *testing.T) {
	cmd, err := TUI{
		BinaryPath: "/usr/local/bin/opencode",
		ServerURL:  "http://127.0.0.1:45123",
		Session:    "ses_0000000000samplesession000",
	}.Command(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/usr/local/bin/opencode",
		"--server", "http://127.0.0.1:45123",
		"--session", "ses_0000000000samplesession000",
	}
	if !slices.Equal(cmd.Args, want) {
		t.Errorf("args = %v, want %v", cmd.Args, want)
	}
}

// TestTUIWithNoSessionPassesNoFlag: a fresh sandbox has no conversation
// to open, and an empty --session would be an argument the client has to
// resolve to nothing.
//
// @decision(DL-025)
func TestTUIWithNoSessionPassesNoFlag(t *testing.T) {
	cmd, err := TUI{
		BinaryPath: "/usr/local/bin/opencode",
		ServerURL:  "http://127.0.0.1:45123",
	}.Command(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range cmd.Args {
		if arg == "--session" {
			t.Fatalf("an empty session still passed the flag: %v", cmd.Args)
		}
	}
}
