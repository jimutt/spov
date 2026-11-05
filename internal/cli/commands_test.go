package cli

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/credentials"
	"github.com/jimutt/spov/internal/credproxy"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
	"github.com/jimutt/spov/internal/session"
	"github.com/jimutt/spov/internal/store"
)

func TestLsEmpty(t *testing.T) {
	f := newAPIFixture(t)
	out, _, err := runWith(t, f.env, "ls")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No spov sessions") {
		t.Errorf("output = %q", out)
	}
}

// TestLs: a sandbox the server did not start is listed once the
// reconciler has adopted it, with the columns a user scans for.
//
// @decision(DL-002) @decision(DL-004)
func TestLs(t *testing.T) {
	f := newAPIFixture(t)
	f.seedSession("morning", 90*time.Minute)

	out, _, err := runWith(t, f.env, "ls")
	if err != nil {
		t.Fatal(err)
	}
	// REPOS comes from the clones the tags recorded, since an adopted
	// sandbox has no request.
	for _, want := range []string{"NAME", "STATE", "morning", "running", "1h30m", "acme/app"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
}

// TestLsShowsAge: a forgotten sandbox bills for its whole lifetime, so age
// is the number that matters.
//
// @decision(DL-029)
func TestLsShowsAge(t *testing.T) {
	tests := []struct {
		age  time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{5 * time.Minute, "5m"},
		{2*time.Hour + 15*time.Minute, "2h15m"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			f := newAPIFixture(t)
			f.seedSession("s", tt.age)
			out, _, err := runWith(t, f.env, "ls")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, tt.want) {
				t.Errorf("output does not show age %q:\n%s", tt.want, out)
			}
		})
	}
}

// seedStored puts n sessions in the server's store, a minute apart,
// oldest first, in state, and returns their IDs in that order.
func (f *apiFixture) seedStored(n int, state store.SessionState, from time.Time) []string {
	f.t.Helper()
	ids := make([]string, n)
	for i := range n {
		at := from.Add(time.Duration(i) * time.Minute)
		ids[i] = store.NewID(at)
		sess := store.Session{ID: ids[i], OrgID: auth.LocalID, OwnerID: auth.LocalID, Name: "s-" + ids[i][len(ids[i])-6:], State: state, CreatedAt: at}
		if err := f.deps.Store.Sessions().Create(context.Background(), sess); err != nil {
			f.t.Fatal(err)
		}
	}
	return ids
}

// TestLsShowsTheNewestFiveAndKeepsLiveOnes: finished sessions are
// listed, a page at a time, and a live one is never paged out of sight.
//
// @decision(DL-012)
func TestLsShowsTheNewestFiveAndKeepsLiveOnes(t *testing.T) {
	f := newAPIFixture(t)
	base := time.Now().Add(-2 * time.Hour)
	live := f.seedStored(1, store.StateRunning, base)
	done := f.seedStored(6, store.StateStopped, base.Add(time.Minute))

	out, _, err := runWith(t, f.env, "ls")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range done[1:] {
		if !strings.Contains(out, id) {
			t.Errorf("the newest five do not include %s:\n%s", id, out)
		}
	}
	if strings.Contains(out, done[0]) {
		t.Errorf("the sixth newest is on the first page:\n%s", out)
	}
	if !strings.Contains(out, live[0]) {
		t.Errorf("a live session older than the page is hidden:\n%s", out)
	}
	if !strings.Contains(out, "1–5 of 7, plus 1 still running older than these · next: spov ls --page 2") {
		t.Errorf("the footer does not say where this is:\n%s", out)
	}

	out, _, err = runWith(t, f.env, "ls", "--page", "2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, done[0]) || !strings.Contains(out, live[0]) || !strings.Contains(out, "6–7 of 7") {
		t.Errorf("page 2 is not the two oldest:\n%s", out)
	}
	if strings.Contains(out, "next:") {
		t.Errorf("the last page offers a next one:\n%s", out)
	}

	out, _, err = runWith(t, f.env, "ls", "-n", "10")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "\n") != 8 || strings.Contains(out, "of 7") {
		t.Errorf("everything fits, so the table is the whole answer:\n%s", out)
	}

	out, _, err = runWith(t, f.env, "ls", "--page", "9")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Page 9 is past the end: 7 in all.") {
		t.Errorf("a page past the end:\n%s", out)
	}
	if _, _, err := runWith(t, f.env, "ls", "--page", "0"); err == nil {
		t.Error("--page 0 was accepted")
	}
	if _, _, err := runWith(t, f.env, "ls", "--all"); err == nil {
		t.Error("--all still works; paging replaced it")
	}
}

func TestLsReportsServerErrors(t *testing.T) {
	f := newAPIFixture(t)
	f.http.Close()
	if _, _, err := runWith(t, f.env, "ls"); err == nil {
		t.Fatal("expected an error")
	}
}

// TestCommandsSpawnALocalServerByDefault: with nothing naming a server,
// every session command starts the local one; when that fails, the
// error names the server log.
//
// @decision(DL-002)
func TestCommandsSpawnALocalServerByDefault(t *testing.T) {
	for _, args := range [][]string{{"ls"}, {"stop", "x"}, {"attach", "x"}, {"fly"}} {
		t.Run(args[0], func(t *testing.T) {
			spawned := 0
			env := Env{Spawn: func(context.Context) error { spawned++; return errors.New("exec failed") }}
			_, _, err := runWith(t, env, args...)
			if err == nil {
				t.Fatal("expected an error")
			}
			if spawned != 1 {
				t.Errorf("spawned %d times", spawned)
			}
			if !strings.Contains(err.Error(), "exec failed") {
				t.Errorf("err = %v", err)
			}
		})
	}
}

// TestServerResolution: flag, then environment, then config.
//
// @decision(DL-002)
func TestServerResolution(t *testing.T) {
	wd := t.TempDir()
	cfg := userConfigEnv(t, "server:\n  url: https://config.example\n")
	tests := []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{"config", nil, nil, "https://config.example"},
		{"env", map[string]string{ServerEnvVar: "https://env.example"}, nil, "https://env.example"},
		{"flag", map[string]string{ServerEnvVar: "https://env.example"}, []string{"--server", "https://flag.example"}, "https://flag.example"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vars := map[string]string{ServerTokenEnvVar: "secret", "XDG_CONFIG_HOME": cfg.Vars["XDG_CONFIG_HOME"]}
			for k, v := range tt.env {
				vars[k] = v
			}
			e := &Env{Workdir: wd, Environ: config.MapEnv{Vars: vars}}
			if len(tt.args) > 0 {
				e.ServerURL = tt.args[1]
			}
			c, err := e.client(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if c.BaseURL != tt.want {
				t.Errorf("server = %q, want %q", c.BaseURL, tt.want)
			}
			if c.Token != "secret" {
				t.Errorf("the token was not taken from %s", ServerTokenEnvVar)
			}
		})
	}
}

func TestServerURLMustBeHTTP(t *testing.T) {
	e := &Env{Environ: config.MapEnv{Vars: map[string]string{}}, ServerURL: "ftp://x"}
	if _, err := e.client(context.Background()); err == nil || !strings.Contains(err.Error(), "http") {
		t.Errorf("err = %v", err)
	}
}

func TestStop(t *testing.T) {
	f := newAPIFixture(t)
	f.seedSession("morning", time.Minute)

	out, _, err := runWith(t, f.env, "stop", "morning")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Stopped") {
		t.Errorf("output = %q", out)
	}
	if f.provider.Running() != 0 {
		t.Error("the sandbox is still running")
	}
	if got := f.session("morning").State; got != store.StateStopped {
		t.Errorf("state = %s, want stopped", got)
	}
}

func TestStopUnknownSession(t *testing.T) {
	f := newAPIFixture(t)
	_, _, err := runWith(t, f.env, "stop", "nope")
	if err == nil {
		t.Fatal("expected an error")
	}
	// The message has to say how to find the right name.
	if !strings.Contains(err.Error(), "spov ls") {
		t.Errorf("err = %v", err)
	}
}

func TestStopAll(t *testing.T) {
	f := newAPIFixture(t)
	f.seedSession("one", time.Minute)
	f.seedSession("two", time.Minute)

	out, _, err := runWith(t, f.env, "stop", "--all")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "Stopped") != 2 {
		t.Errorf("output = %q", out)
	}
	if f.provider.Running() != 0 {
		t.Errorf("%d sandboxes still running", f.provider.Running())
	}
}

func TestStopAllEmpty(t *testing.T) {
	f := newAPIFixture(t)
	out, _, err := runWith(t, f.env, "stop", "--all")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No running spov sessions") {
		t.Errorf("output = %q", out)
	}
}

// TestDownAllContinuesPastFailures: one stuck sandbox must not leave the
// rest billing.
//
// @decision(DL-029)
func TestStopAllContinuesPastFailures(t *testing.T) {
	f := newAPIFixture(t)
	f.seedSession("one", time.Minute)
	f.seedSession("two", time.Minute)
	f.provider.TerminateErr = errors.New("stuck")

	_, stderr, err := runWith(t, f.env, "stop", "--all")
	if err == nil {
		t.Fatal("expected an error reporting the failures")
	}
	if !strings.Contains(err.Error(), "still billing") {
		t.Errorf("err = %v; the user must be told they are still paying", err)
	}
	if strings.Count(stderr, "stuck") != 2 {
		t.Errorf("both failures should be reported:\n%s", stderr)
	}
}

func TestStopRejectsBadArguments(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"nothing", []string{"stop"}, "name a session"},
		{"both", []string{"stop", "x", "--all"}, "not both"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := runWith(t, Env{}, tt.args...)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v", err)
			}
		})
	}
}

// TestFlyWithoutRepos: nothing configured and no picker must fail with a
// clear message rather than hang on a prompt nobody can answer.
func TestFlyWithoutRepos(t *testing.T) {
	f := newAPIFixtureWithConfig(t, "")
	_, _, err := runWith(t, f.env, "fly")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "--repo") {
		t.Errorf("error %q does not offer the non-interactive alternative", err)
	}
}

// TestFlyReportsAMissingPlatformToken: the server resolves the token
// and its failure reaches the user through the launch stream.
func TestFlyReportsAMissingPlatformToken(t *testing.T) {
	f := newAPIFixture(t)
	pk, _ := credentials.PlatformKey("codefloe")
	if err := f.creds.Delete(pk); err != nil {
		t.Fatal(err)
	}
	_, _, err := runWith(t, f.env, "fly")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "codefloe") {
		t.Errorf("error %q does not name the platform whose token is missing", err)
	}
	if f.provider.Running() != 0 {
		t.Error("a sandbox was created without a platform token")
	}
}

// TestFlyTerminatesOnTUIExit is the primary cost control: an attached
// client defeats the idle timeout, so the sandbox must go when the TUI
// does.
//
// @decision(DL-029) @decision(DL-002)
func TestFlyTerminatesOnTUIExit(t *testing.T) {
	f := newAPIFixture(t)

	_, stderr, err := runWith(t, f.env, "fly")
	if err != nil {
		t.Fatalf("fly: %v\n%s", err, stderr)
	}
	if f.tuiRuns != 1 {
		t.Errorf("the TUI ran %d times, want 1", f.tuiRuns)
	}
	if f.provider.Running() != 0 {
		t.Error("the sandbox is still running after the TUI exited")
	}

	// The TUI receives the loopback proxy's URL and no credential; the
	// proxy is what authenticates.
	//
	// @decision(DL-030)
	if !strings.HasPrefix(f.tuiSeen.ServerURL, "http://127.0.0.1:") {
		t.Errorf("the TUI server URL = %q, want a loopback address", f.tuiSeen.ServerURL)
	}
	if f.proxyStarts != 1 {
		t.Errorf("%d client proxies started, want 1", f.proxyStarts)
	}
	// The password exists only on the server side; the terminal never
	// sees it.
	//
	// @decision(DL-030)
	if pw := f.launchedPassword(); strings.Contains(stderr, pw) {
		t.Error("the session password was printed to the terminal")
	}
	// The client's proxy minted through the server, as the launcher's
	// own mint is separate from it.
	if n := len(f.provider.ConnectTokens); n < 2 {
		t.Errorf("%d connect tokens minted, want the launcher's and the client's", n)
	}
	// The plain progress lines are the launcher's own, unchanged.
	for _, want := range []string{"preparing tokens...", "starting sandbox...", "waiting for server...", "is ready"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
}

// TestFlyKeepLeavesTheSandboxRunning covers --keep.
//
// @decision(DL-029)
func TestFlyKeepLeavesTheSandboxRunning(t *testing.T) {
	f := newAPIFixture(t)

	_, stderr, err := runWith(t, f.env, "fly", "--keep", "--name", "kept")
	if err != nil {
		t.Fatalf("fly: %v\n%s", err, stderr)
	}
	if f.provider.Running() != 1 {
		t.Error("the sandbox was terminated despite --keep")
	}
	for _, want := range []string{"spov attach kept", "spov stop kept", "still running"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	if got := f.session("kept").State; got != store.StateRunning {
		t.Errorf("state = %s, want running", got)
	}
}

func TestFlyNoAttach(t *testing.T) {
	f := newAPIFixture(t)

	_, stderr, err := runWith(t, f.env, "fly", "--no-attach", "--name", "bg")
	if err != nil {
		t.Fatalf("fly: %v\n%s", err, stderr)
	}
	if f.tuiRuns != 0 {
		t.Error("the TUI ran despite --no-attach")
	}
	if f.provider.Running() != 1 {
		t.Error("the sandbox is not running")
	}
	if !strings.Contains(stderr, "spov attach bg") {
		t.Errorf("no attach hint:\n%s", stderr)
	}
}

// TestFlyKeepsTheSandboxWhenTheTUIFails: a local failure must not
// discard a session the user can still reach.
//
// @decision(DL-029)
func TestFlyKeepsTheSandboxWhenTheTUIFails(t *testing.T) {
	f := newAPIFixture(t)
	f.tuiErr = errors.New("opencode crashed")

	_, stderr, err := runWith(t, f.env, "fly", "--name", "crashy")
	if err == nil {
		t.Fatal("expected the TUI failure to be reported")
	}
	if f.provider.Running() != 1 {
		t.Error("the sandbox was terminated after a TUI failure")
	}
	if !strings.Contains(stderr, "spov stop crashy") {
		t.Errorf("no hint how to stop the sandbox:\n%s", stderr)
	}
}

func TestFlyUsesTheGivenName(t *testing.T) {
	f := newAPIFixture(t)
	if _, stderr, err := runWith(t, f.env, "fly", "--keep", "--name", "chosen"); err != nil {
		t.Fatalf("fly: %v\n%s", err, stderr)
	}
	if f.provider.Created[0].Name != "chosen" {
		t.Errorf("sandbox name = %q", f.provider.Created[0].Name)
	}
}

// TestFlyReportsCostAndRepos: the summary comes from the server's view
// of the session, so it reflects the defaults the server applied.
//
// @decision(DL-029) @decision(DL-002)
func TestFlyReportsCostAndRepos(t *testing.T) {
	f := newAPIFixture(t)
	_, stderr, err := runWith(t, f.env, "fly")
	if err != nil {
		t.Fatalf("fly: %v\n%s", err, stderr)
	}
	for _, want := range []string{"cloned alice/spov", "/hour", "timeout 4h0m0s", "idle timeout 30m0s", "working directory"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("summary lacks %q:\n%s", want, stderr)
		}
	}
}

// TestFlyRejectsAVersionMismatch: the sandbox's reported version is the
// authoritative one.
//
// @decision(DL-035) @decision(DL-025)
func TestFlyRejectsAVersionMismatch(t *testing.T) {
	f := newAPIFixture(t)
	f.health.Version = "9.9.9"

	_, stderr, err := runWith(t, f.env, "fly", "--name", "mm")
	if err == nil {
		t.Fatal("expected the version gate to refuse")
	}
	if !strings.Contains(err.Error(), "--allow-version-mismatch") {
		t.Errorf("err = %v", err)
	}
	if f.tuiRuns != 0 {
		t.Error("the TUI ran despite the mismatch")
	}
	if f.provider.Running() != 1 || !strings.Contains(stderr, "spov stop mm") {
		t.Errorf("the sandbox must be left running with a hint:\n%s", stderr)
	}
}

func TestFlyAllowsAVersionMismatchOnRequest(t *testing.T) {
	f := newAPIFixture(t)
	f.health.Version = "9.9.9"

	_, stderr, err := runWith(t, f.env, "fly", "--allow-version-mismatch")
	if err != nil {
		t.Fatalf("fly: %v\n%s", err, stderr)
	}
	if f.tuiRuns != 1 {
		t.Error("the TUI did not run")
	}
	if !strings.Contains(stderr, "continuing because --allow-version-mismatch") {
		t.Errorf("no warning about the mismatch:\n%s", stderr)
	}
}

// TestFlyReportsAFailedTermination: the sandbox is still billing, and
// the user must be told.
//
// @decision(DL-029)
func TestFlyReportsAFailedTermination(t *testing.T) {
	f := newAPIFixture(t)
	f.provider.TerminateErr = errors.New("modal is wedged")

	_, stderr, err := runWith(t, f.env, "fly", "--name", "stuck")
	if err == nil {
		t.Fatal("expected the termination failure to be reported")
	}
	if !strings.Contains(stderr, "still billing") || !strings.Contains(stderr, "spov stop stuck") {
		t.Errorf("stderr = %q", stderr)
	}
	// And the session is not recorded as stopped, so `stop` can retry.
	if got := f.session("stuck").State; got != store.StateRunning {
		t.Errorf("state = %s, want running", got)
	}
}

// TestFlyTerminatesAfterAnInterruptedTUI: a Ctrl-C that reaches the TUI
// ends it, and the sandbox must still be torn down.
//
// @decision(DL-029)
func TestFlyTerminatesAfterAnInterruptedTUI(t *testing.T) {
	f := newAPIFixture(t)
	f.tuiCode = 130

	if _, stderr, err := runWith(t, f.env, "fly"); err != nil {
		t.Fatalf("fly: %v\n%s", err, stderr)
	}
	if f.provider.Running() != 0 {
		t.Error("the sandbox is still running")
	}
}

// TestFlyForwardsProviderKeys: the keys live in the server's
// environment, and they reach the sandbox: its proxy sidecar holds the
// key and the main container a placeholder.
//
// @decision(DL-036) @decision(DL-034)
func TestFlyForwardsProviderKeys(t *testing.T) {
	f := newAPIFixture(t)
	f.serverEnv.Environ = config.MapEnv{Vars: map[string]string{"ANTHROPIC_API_KEY": "sk-ant-test"}}
	f.http.Close()
	f.startServer()
	f.env.Client = client.New(f.http.URL, f.token)

	_, stderr, err := runWith(t, f.env, "fly")
	if err != nil {
		t.Fatalf("fly: %v\n%s", err, stderr)
	}
	spec := f.provider.Created[0]
	if got := spec.Secrets["ANTHROPIC_API_KEY"]; !strings.HasPrefix(got, "sk-ant-spovplaceholder") {
		t.Errorf("ANTHROPIC_API_KEY in the main container = %q", got)
	}
	if spec.Sidecar == nil || !strings.Contains(spec.Sidecar.Secrets[credproxy.EnvConfig], `"key":"sk-ant-test"`) {
		t.Error("the key did not reach the proxy sidecar")
	}
	if strings.Contains(stderr, "sk-ant-test") {
		t.Error("a provider key was printed")
	}
	if strings.Contains(stderr, "no model provider key") {
		t.Error("warned about a missing key that is set")
	}
}

// TestFlyWarnsWhenNoProviderKeyIsSet: the warning comes from the
// server, which is the side that knows, and reaches the terminal.
//
// @decision(DL-036) @decision(DL-002)
func TestFlyWarnsWhenNoProviderKeyIsSet(t *testing.T) {
	f := newAPIFixture(t)
	_, stderr, err := runWith(t, f.env, "fly")
	if err != nil {
		t.Fatalf("fly: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "no model provider key") {
		t.Errorf("no warning about the missing provider key:\n%s", stderr)
	}
}

// TestFlyWithRepoFlagAndNoProjectFile: --repo alone is enough when one
// platform is configured.
//
// @decision(DL-023)
func TestFlyWithRepoFlagAndNoProjectFile(t *testing.T) {
	f := newAPIFixtureWithConfig(t, `platforms:
  - name: codefloe
    kind: forgejo
    base_url: https://codefloe.com
    account: alice
`)
	_, stderr, err := runWith(t, f.env, "fly", "--repo", "alice/spov")
	if err != nil {
		t.Fatalf("fly: %v\n%s", err, stderr)
	}
	if got := f.provider.Created[0].Tags[sandbox.TagRepos]; got != "alice/spov" {
		t.Errorf("repos tag = %q", got)
	}
}

func TestFlyRejectsAMalformedRepoFlag(t *testing.T) {
	f := newAPIFixture(t)
	_, _, err := runWith(t, f.env, "fly", "--repo", "nonsense")
	if err == nil || !strings.Contains(err.Error(), "--repo") {
		t.Errorf("err = %v", err)
	}
	if f.provider.Running() != 0 || len(f.provider.Created) != 0 {
		t.Error("a sandbox was created for a malformed flag")
	}
}

// TestFlyRejectsABadSandboxConfigBeforeLaunching: the server validates
// synchronously, so the user sees 422 rather than a failed launch.
//
// @decision(DL-029)
func TestFlyRejectsABadSandboxConfigBeforeLaunching(t *testing.T) {
	f := newAPIFixture(t)
	// The client's own config validation would catch most values; the
	// request is built by hand to prove the server's gate.
	req := server.SessionRequest{Repos: []server.RepoRef{{Platform: "codefloe", Owner: "alice", Name: "spov"}}}
	req.Sandbox.Cloud = "atari"
	_, err := f.env.Client.CreateSession(context.Background(), req)
	if err == nil || client.StatusOf(err) != 422 {
		t.Fatalf("err = %v, want 422", err)
	}
	if len(f.provider.Created) != 0 {
		t.Error("a sandbox was created for an invalid request")
	}
}

// TestFlyRepoFlagOverridesConfiguredLanding: a landing that names a
// configured repo cannot apply to a list --repo replaced.
//
// @decision(DL-037)
func TestFlyRepoFlagOverridesConfiguredLanding(t *testing.T) {
	f := newAPIFixtureWithConfig(t, defaultConfig+"landing: spov\n")
	_, stderr, err := runWith(t, f.env, "fly", "--repo", "alice/other")
	if err != nil {
		t.Fatalf("fly: %v\n%s", err, stderr)
	}
	if got := f.session(f.provider.Created[0].Name).Request.Landing; got != "" {
		t.Errorf("landing = %q, want none when --repo replaced the list", got)
	}
}

func TestFlyUsesConfiguredLandingWithoutRepoFlags(t *testing.T) {
	f := newAPIFixtureWithConfig(t, defaultConfig+"landing: spov\n")
	_, stderr, err := runWith(t, f.env, "fly")
	if err != nil {
		t.Fatalf("fly: %v\n%s", err, stderr)
	}
	if got := f.session(f.provider.Created[0].Name).Request.Landing; got != "spov" {
		t.Errorf("landing = %q, want spov", got)
	}
}

// TestFlyCarriesTheConfiguredEgressIntoTheSandbox: the preset the
// client asked for reaches the provider through the API.
//
// @decision(DL-031) @decision(DL-029)
func TestFlyCarriesTheConfiguredEgressIntoTheSandbox(t *testing.T) {
	f := newAPIFixtureWithConfig(t, defaultConfig+"sandbox:\n  egress: agent\n")
	_, stderr, err := runWith(t, f.env, "fly")
	if err != nil {
		t.Fatalf("fly: %v\n%s", err, stderr)
	}
	if got := f.provider.Created[0].Egress; got.Preset != sandbox.PresetAgent {
		t.Errorf("egress preset = %q, want agent", got.Preset)
	}

	// Extra domains are the server's configuration, added to whatever
	// preset the request names.
	f = newAPIFixtureWithConfig(t, defaultConfig+"sandbox:\n  egress: install\n  egress_extra_domains: [pypi.example]\n")
	if _, stderr, err := runWith(t, f.env, "fly"); err != nil {
		t.Fatalf("fly: %v\n%s", err, stderr)
	}
	got := f.provider.Created[0].Egress
	if got.Open() || !strings.Contains(strings.Join(got.Domains, ","), "pypi.example") {
		t.Errorf("egress = %+v, want the install preset plus the configured domain", got)
	}
}

// TestFlySandboxConfigReachesTheSandbox: the sizing the client
// configured is what the provider is asked for.
//
// @decision(DL-029) @decision(DL-002)
func TestFlySandboxConfigReachesTheSandbox(t *testing.T) {
	f := newAPIFixtureWithConfig(t, defaultConfig+"sandbox:\n  cpu: 2\n  memory_mib: 4096\n  cpu_limit: 4\n  timeout: 2h\n  idle_timeout: 5m\n  regions: [eu-west]\n  workdir: /srv\n")
	_, stderr, err := runWith(t, f.env, "fly")
	if err != nil {
		t.Fatalf("fly: %v\n%s", err, stderr)
	}
	spec := f.provider.Created[0]
	if spec.CPU != 2 || spec.MemoryMiB != 4096 || spec.CPULimit != 4 || spec.Timeout != 2*time.Hour ||
		spec.IdleTimeout != 5*time.Minute || len(spec.Regions) != 1 || spec.Workdir != "/srv" {
		t.Errorf("spec = %+v", spec)
	}
	if !strings.Contains(stderr, "timeout 2h0m0s, idle timeout 5m0s") {
		t.Errorf("the summary does not report the effective config:\n%s", stderr)
	}
}

// TestFlyReportsAnUnreachableSession: a session the proxy cannot reach
// is a local failure, and the sandbox is left running with a hint.
//
// @decision(DL-030) @decision(DL-002)
func TestFlyReportsAnUnreachableSession(t *testing.T) {
	f := newAPIFixture(t)
	f.proxyHealthErr = errors.New("connection refused")

	_, stderr, err := runWith(t, f.env, "fly", "--name", "far")
	if err == nil || !strings.Contains(err.Error(), "local proxy") {
		t.Fatalf("err = %v", err)
	}
	if f.provider.Running() != 1 || !strings.Contains(stderr, "spov stop far") {
		t.Errorf("the sandbox must be left running with a hint:\n%s", stderr)
	}
	if f.tuiRuns != 0 {
		t.Error("the TUI ran against an unreachable session")
	}
}

// TestFlyReportsAFailedLaunch: the server's problem is the error, and
// nothing is left running.
func TestFlyReportsAFailedLaunch(t *testing.T) {
	f := newAPIFixture(t)
	f.provider.CreateErr = errors.New("no capacity in region")

	_, _, err := runWith(t, f.env, "fly")
	if err == nil || !strings.Contains(err.Error(), "no capacity") {
		t.Fatalf("err = %v", err)
	}
	if f.provider.Running() != 0 || f.tuiRuns != 0 {
		t.Error("something ran after a failed launch")
	}
}

// TestFlyCancelsTheLaunchOnInterrupt: a Ctrl-C during the launch tells
// the server to stop, so the sandbox it was creating does not bill on.
//
// @decision(DL-029) @decision(DL-002)
func TestFlyCancelsTheLaunchOnInterrupt(t *testing.T) {
	f := newAPIFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	// The health wait is where a launch spends its time, so a launch
	// that reaches it sits there until its context ends.
	f.serverEnv.NewHealthChecker = func(string, string) session.HealthChecker {
		return &blockingHealth{}
	}
	f.http.Close()
	f.startServer()
	f.env.Client = client.New(f.http.URL, f.token)

	// The interrupt has to land *during* the launch, and the launch
	// begins on the server the moment the create handler hands it to a
	// goroutine — before the CLI has finished reading the 201 that
	// starts it following. Cancelling on the server's side of that race
	// aborts the create instead of the launch, and the test then fails
	// on a "context canceled" POST rather than the cancellation it is
	// about. A step line is the CLI reporting the server's launch
	// events back, so it cannot be printed before the create returned:
	// it is the earliest moment the interrupt means what it should.
	var out strings.Builder
	errBuf := &cancelOnStep{cancel: cancel}
	f.env.Out, f.env.Err = &out, errBuf
	f.env.LocalIdentity = func(context.Context, string) (git.Identity, error) { return git.Identity{}, nil }
	root := NewRoot(f.env)
	root.SetArgs([]string{"fly", "--name", "cut"})
	err := root.ExecuteContext(ctx)
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("err = %v", err)
	}
	// The delete cancels the launch and answers at once; the launch
	// goroutine is what terminates the sandbox.
	f.waitFor("the cancelled launch to terminate its sandbox", func() bool {
		return f.provider.Running() == 0 && f.session("cut").State == store.StateStopped
	})
	if f.tuiRuns != 0 {
		t.Error("the TUI ran")
	}
}

// cancelOnStep fires the interrupt on the first launch progress line
// the CLI prints, and collects everything written to it. It matches the
// step lines specifically rather than the first write of any kind:
// warnings are printed before the launch starts, and cancelling on one
// of those would abort the create instead of the launch. Writes come
// from more than one goroutine once a launch is in flight, so it locks.
type cancelOnStep struct {
	mu     sync.Mutex
	b      strings.Builder
	cancel context.CancelFunc
	fired  bool
}

// stepLine matches what launchReporter.Step prints, and nothing the CLI
// writes before the launch does.
func stepLine(s string) bool { return strings.HasSuffix(strings.TrimRight(s, "\n"), "...") }

func (c *cancelOnStep) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.fired && stepLine(string(p)) {
		c.fired = true
		c.cancel()
	}
	return c.b.Write(p)
}

func (c *cancelOnStep) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.b.String()
}

// blockingHealth never answers, so a launch sits in its health wait
// until its context ends.
type blockingHealth struct{}

func (blockingHealth) WaitHealthy(ctx context.Context, _ time.Duration) (opencode.Health, error) {
	<-ctx.Done()
	return opencode.Health{}, ctx.Err()
}

func TestImageName(t *testing.T) {
	cfg := config.Defaults()
	if got := imageName(cfg); !strings.Contains(got, cfg.OpenCode.Version) {
		t.Errorf("imageName = %q, want it derived from the version", got)
	}
	cfg.Sandbox.Image = "custom:1"
	if got := imageName(cfg); got != "custom:1" {
		t.Errorf("imageName = %q", got)
	}
}

func TestHourlyCost(t *testing.T) {
	got := hourlyCost(server.SandboxConfigView{CPU: 1, MemoryMiB: 2048})
	if got != "$0.18/hour" {
		t.Errorf("hourlyCost = %q", got)
	}
}

func TestGenerateName(t *testing.T) {
	got := generateName(time.Date(2026, 8, 29, 10, 15, 0, 0, time.UTC))
	if got != "spov-20260829-101500" {
		t.Errorf("generateName = %q", got)
	}
}

func TestFormatAge(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "-"},
		{45 * time.Second, "45s"},
		{3 * time.Minute, "3m"},
		{90 * time.Minute, "1h30m"},
	}
	for _, tt := range tests {
		if got := formatAge(tt.d); got != tt.want {
			t.Errorf("formatAge(%s) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestDataDir(t *testing.T) {
	e := Env{Environ: config.MapEnv{Vars: map[string]string{"XDG_DATA_HOME": "/xdg"}}}
	if got := e.dataDir(); got != "/xdg/spov" {
		t.Errorf("dataDir = %q", got)
	}
	e = Env{Environ: config.MapEnv{Home: "/home/u"}}
	if got := e.dataDir(); got != "/home/u/.local/share/spov" {
		t.Errorf("dataDir = %q", got)
	}
}

func TestUsername(t *testing.T) {
	e := Env{Environ: config.MapEnv{Vars: map[string]string{"USER": "alice"}}}
	if got := e.username(); got != "alice" {
		t.Errorf("username = %q", got)
	}
	e = Env{Environ: config.MapEnv{Vars: map[string]string{"USERNAME": "winuser"}}}
	if got := e.username(); got != "winuser" {
		t.Errorf("username = %q", got)
	}
}

func TestHomeDir(t *testing.T) {
	e := Env{Environ: config.MapEnv{Home: "/home/u"}}
	if got := e.homeDir(); got != "/home/u" {
		t.Errorf("homeDir = %q", got)
	}
	e = Env{Environ: config.MapEnv{}}
	if got := e.homeDir(); got != "" {
		t.Errorf("homeDir = %q", got)
	}
}

func TestDisplayName(t *testing.T) {
	if got := displayName(server.SessionView{Name: "n", SandboxID: "sb-1"}); got != "n (sb-1)" {
		t.Errorf("displayName = %q", got)
	}
	if got := displayName(server.SessionView{ID: "01H"}); got == "" {
		t.Error("displayName is empty for a nameless session")
	}
}

func TestAsVersionMismatch(t *testing.T) {
	var target opencode.ClientVersionMismatchError
	if asVersionMismatch(errors.New("other"), &target) {
		t.Error("an unrelated error matched")
	}
	if !asVersionMismatch(opencode.ClientVersionMismatchError{Found: "1", Want: "2"}, &target) {
		t.Error("a mismatch did not match")
	}
	// The SANDBOX's mismatch must NOT match here: it is a different
	// sentence about a different machine, and prepareTUI's
	// --allow-version-mismatch branch is about the local binary alone.
	//
	// @decision(DL-035)
	if asVersionMismatch(opencode.VersionMismatchError{Expected: "1", Reported: "2"}, &target) {
		t.Error("a sandbox mismatch matched the local client's check")
	}
}

func TestNewPlatformRejectsUnknownKinds(t *testing.T) {
	if _, err := newPlatform(context.Background(), config.Platform{Name: "x", Kind: "svn"}, "t"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestLocateBinaryUsesTheRealLocatorByDefault(t *testing.T) {
	e := &Env{Environ: config.MapEnv{Vars: map[string]string{"HOME": t.TempDir()}}}
	if _, err := e.locateBinary(context.Background(), "0.0.0-nope"); err == nil {
		t.Fatal("expected the locator to fail for a version that is not installed")
	}
}

// TestDetachReportsFailuresAsWarnings: releasing local resources cannot
// fail a launch the user can still reach.
//
// @decision(DL-029)
func TestDetachReportsFailuresAsWarnings(t *testing.T) {
	var errBuf strings.Builder
	env := &Env{Err: &errBuf}
	detach(env, &liveSession{})
	if errBuf.Len() != 0 {
		t.Errorf("a session without a proxy warned: %q", errBuf.String())
	}
}

// TestDownReportsATerminateFailure: the session stays running in the
// store so a retry is possible, and the user hears about it.
//
// @decision(DL-029)
func TestStopReportsATerminateFailure(t *testing.T) {
	f := newAPIFixture(t)
	f.seedSession("stuck", time.Minute)
	f.provider.TerminateErr = errors.New("wedged")
	_, _, err := runWith(t, f.env, "stop", "stuck")
	if err == nil || !strings.Contains(err.Error(), "wedged") || strings.Contains(err.Error(), "spov fly") {
		t.Errorf("err = %v", err)
	}
	if got := f.session("stuck").State; got != store.StateRunning {
		t.Errorf("state = %s, want running so `stop` can be retried", got)
	}
}

// TestDownOnAStoppedSessionIsNotAnError: the outcome the user asked
// for already holds.
func TestStopOnAStoppedSessionIsNotAnError(t *testing.T) {
	f := newAPIFixture(t)
	f.seedSession("s", time.Minute)
	id := f.session("s").ID
	if _, _, err := runWith(t, f.env, "stop", id); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runWith(t, f.env, "stop", id); err != nil {
		t.Errorf("second down: %v", err)
	}
}

// TestFlyReportsAProxyThatWillNotStart: without a proxy nothing can
// reach the sandbox; it is left running with a hint and a summary.
//
// @decision(DL-030)
func TestFlyReportsAProxyThatWillNotStart(t *testing.T) {
	f := newAPIFixture(t)
	f.proxyErr = errors.New("no free port")
	_, stderr, err := runWith(t, f.env, "fly", "--name", "np")
	if err == nil || !strings.Contains(err.Error(), "no free port") {
		t.Fatalf("err = %v", err)
	}
	if f.provider.Running() != 1 || f.tuiRuns != 0 {
		t.Error("the sandbox must be left running and the TUI not run")
	}
	for _, want := range []string{"spov stop np", "is ready (version unknown)", "cloned alice/spov"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	f.seedSession("s", time.Minute)
	if _, _, err := runWith(t, f.env, "attach", "s"); err == nil || !strings.Contains(err.Error(), "spov stop s") {
		t.Errorf("attach err = %v", err)
	}
}

// TestReconcileDuringALaunchDoesNotDuplicateTheSession reproduces what
// a real launch showed: the sandbox exists for a minute before the
// launch records its ID, and a reconcile in that window filed it a
// second time under the sandbox ID.
//
// @decision(DL-002) @decision(DL-004)
func TestReconcileDuringALaunchDoesNotDuplicateTheSession(t *testing.T) {
	f := newAPIFixture(t)
	release := make(chan struct{})
	reached := make(chan struct{}, 1)
	f.serverEnv.NewHealthChecker = func(string, string) session.HealthChecker {
		return &gatedHealth{reached: reached, release: release, health: f.health}
	}
	f.http.Close()
	f.startServer()
	f.env.Client = client.New(f.http.URL, f.token)

	done := make(chan error, 1)
	go func() {
		_, _, err := runWith(t, f.env, "fly", "--name", "once", "--keep")
		done <- err
	}()
	<-reached
	// The sandbox is running and the session is starting with no ID.
	f.reconcile()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	out, _, err := runWith(t, f.env, "ls")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out, "running"); n != 1 {
		t.Errorf("%d running sessions for one sandbox:\n%s", n, out)
	}
}

// gatedHealth signals when the launch reaches its health wait and holds
// it there until released.
type gatedHealth struct {
	reached chan struct{}
	release chan struct{}
	health  opencode.Health
}

func (g *gatedHealth) WaitHealthy(ctx context.Context, _ time.Duration) (opencode.Health, error) {
	select {
	case g.reached <- struct{}{}:
	default:
	}
	select {
	case <-g.release:
		return g.health, nil
	case <-ctx.Done():
		return opencode.Health{}, ctx.Err()
	}
}

// stoppedLine names a job only when stopping its session cancelled one.
//
// @decision(DL-023)
func TestStoppedLineNamesTheCancelledJob(t *testing.T) {
	tests := []struct {
		job  *server.SessionJobView
		want string
	}{
		{nil, "Stopped s."},
		{&server.SessionJobView{ID: "j1", State: store.JobRunning}, "Stopped s and cancelled its job j1."},
		{&server.SessionJobView{ID: "j1", State: store.JobSucceeded}, "Stopped s."},
	}
	for _, tt := range tests {
		if got := stoppedLine("s", tt.job); got != tt.want {
			t.Errorf("stoppedLine(%+v) = %q, want %q", tt.job, got, tt.want)
		}
	}
}
