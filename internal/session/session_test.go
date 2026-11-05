package session

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/sandbox"
)

// fakeHealth stands in for the OpenCode client.
type fakeHealth struct {
	health opencode.Health
	err    error
	calls  int
}

func (f *fakeHealth) WaitHealthy(context.Context, time.Duration) (opencode.Health, error) {
	f.calls++
	return f.health, f.err
}

type fixture struct {
	launcher *Launcher
	provider *sandbox.Fake
	platform *git.Fake
	health   *fakeHealth
	opts     Options
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	provider := sandbox.NewFake()
	platform := git.NewFake()
	health := &fakeHealth{health: opencode.Health{Healthy: true, Version: "v1", PID: 2}}

	f := &fixture{
		provider: provider,
		platform: platform,
		health:   health,
		opts: Options{
			Name:        "morning",
			User:        "alice",
			Image:       "spov-base-v1",
			Selections:  []git.Selection{{Platform: platform, Repos: platform.Repos}},
			CPU:         0.5,
			MemoryMiB:   2048,
			Timeout:     4 * time.Hour,
			IdleTimeout: 30 * time.Minute,
			Port:        4096,
			TokenTTL:    time.Hour,
			Egress:      sandbox.EgressOpen(),
		},
	}
	f.launcher = &Launcher{
		Sandboxes:     provider,
		NewClient:     func(string, string) HealthChecker { return health },
		NewPassword:   func() (string, error) { return "test-password", nil },
		HealthTimeout: time.Second,
	}
	return f
}

func TestLaunch(t *testing.T) {
	f := newFixture(t)

	sess, err := f.launcher.Launch(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if sess.URL == "" {
		t.Error("no tunnel URL")
	}
	if sess.Password != "test-password" {
		t.Errorf("password = %q", sess.Password)
	}
	if sess.Health.Version != "v1" {
		t.Errorf("health = %+v", sess.Health)
	}
	if len(sess.Clones) != 1 || sess.Clones[0] != "acme/app" {
		t.Errorf("clones = %v", sess.Clones)
	}
	// The server starts at the workspace root, since no landing
	// directory was configured. Nothing picks the clone on its behalf.
	//
	// @decision(DL-037)
	if sess.Landing != opencode.WorkspaceRoot {
		t.Errorf("landing = %q", sess.Landing)
	}

	spec := f.provider.Created[0]
	if spec.Name != "morning" || spec.Image != "spov-base-v1" {
		t.Errorf("spec = %+v", spec)
	}
	if spec.Tags[sandbox.TagSpov] != "1" {
		t.Errorf("tags = %v; ls and down find sandboxes by tag", spec.Tags)
	}
	if spec.Tags[sandbox.TagRepos] != "acme/app" {
		t.Errorf("repos tag = %q", spec.Tags[sandbox.TagRepos])
	}
	// The password is a credential, so it travels as a Modal secret
	// rather than plain sandbox metadata.
	if spec.Secrets[opencode.PasswordEnvVar] != "test-password" {
		t.Error("the password did not reach the sandbox secrets")
	}
	if _, plain := spec.Env[opencode.PasswordEnvVar]; plain {
		t.Error("the password was sent as plain environment metadata")
	}
	for _, arg := range spec.Command {
		if strings.Contains(arg, "test-password") {
			t.Error("the password leaked into the sandbox command line")
		}
	}
}

func TestLaunchUsesConfiguredLanding(t *testing.T) {
	f := newFixture(t)
	f.opts.Landing = "app"

	sess, err := f.launcher.Launch(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Landing != "/workspace/app" {
		t.Errorf("landing = %q", sess.Landing)
	}
}

// TestLaunchTerminatesSandboxWhenTheServerNeverAnswers is the cost path:
// a sandbox that is created but never usable must not be left billing.
//
// @decision(DL-029)
func TestLaunchCleansUpWhenTheServerNeverAnswers(t *testing.T) {
	f := newFixture(t)
	f.health.err = errors.New("server did not become healthy")

	_, err := f.launcher.Launch(context.Background(), f.opts)
	if err == nil {
		t.Fatal("expected an error")
	}
	if f.provider.Running() != 0 {
		t.Errorf("%d sandboxes are still running and billing", f.provider.Running())
	}
	if f.platform.Revoked() != 1 {
		t.Errorf("git tokens were not revoked after a failed launch (revoked=%d)", f.platform.Revoked())
	}
}

// probe reports whether the proxy at url is still answering.
func probe(url string) error {
	resp, err := http.Get(url + "/api/info")
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// TestLaunchExposesNoPorts is the security invariant DL-030 rests on: a
// sandbox with an exposed port has a public URL that anyone who learns it
// can reach, guarded only by the OpenCode password. With no exposed port
// there is no such URL, and Modal's edge rejects an untokened request
// before it reaches OpenCode at all.
//
// @decision(DL-030)
func TestLaunchExposesNoPorts(t *testing.T) {
	f := newFixture(t)

	sess, err := f.launcher.Launch(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close(context.Background()) })

	if len(f.provider.Created) != 1 {
		t.Fatalf("created %d sandboxes", len(f.provider.Created))
	}
	if ports := f.provider.Created[0].Ports; len(ports) != 0 {
		t.Errorf("the sandbox exposed ports %v; it must have no public address", ports)
	}
	// Readiness still gates on the server listening, which needs no
	// exposure: the probe runs against the container port.
	if got := f.provider.Created[0].ReadyPort; got != f.opts.Port {
		t.Errorf("ReadyPort = %d, want %d", got, f.opts.Port)
	}
	// A token was minted for the port the server listens on.
	if len(f.provider.ConnectTokens) != 1 {
		t.Fatalf("minted %d connect tokens, want 1", len(f.provider.ConnectTokens))
	}
	if got := f.provider.ConnectTokens[0].Port; got != f.opts.Port {
		t.Errorf("token minted for port %d, want %d", got, f.opts.Port)
	}
	// Clients are handed the loopback proxy, never an upstream address.
	if !strings.HasPrefix(sess.URL, "http://127.0.0.1:") {
		t.Errorf("session URL = %q, want a loopback address", sess.URL)
	}
}

// TestSessionCloseStopsTheProxy: the proxy holds both credentials and is
// the only way into the session, so it must not outlive it.
//
// @decision(DL-030)
func TestSessionCloseStopsTheProxy(t *testing.T) {
	f := newFixture(t)

	sess, err := f.launcher.Launch(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	url := sess.URL
	if err := probe(url); err != nil {
		t.Fatalf("the proxy was not serving before Close: %v", err)
	}

	if err := sess.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := probe(url); err == nil {
		t.Error("the proxy is still serving after the session was closed")
	}
}

// TestSessionDetachStopsTheProxy: a detached session leaves the sandbox
// running, but the proxy belongs to this process and goes with it.
//
// @decision(DL-030)
func TestSessionDetachStopsTheProxy(t *testing.T) {
	f := newFixture(t)

	sess, err := f.launcher.Launch(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	url := sess.URL
	if err := sess.Detach(); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if err := probe(url); err == nil {
		t.Error("the proxy is still serving after the session was detached")
	}
}

// TestLaunchCleansUpWhenTheProxyCannotStart covers a sandbox that came up
// but cannot be reached: without a connect token there is no way in, so
// the launch must fail and tear down rather than return a session nothing
// can talk to.
//
// @decision(DL-030)
func TestLaunchCleansUpWhenTheProxyCannotStart(t *testing.T) {
	f := newFixture(t)
	f.provider.ConnectErr = errors.New("no connect token")

	if _, err := f.launcher.Launch(context.Background(), f.opts); err == nil {
		t.Fatal("expected an error")
	}
	if f.provider.Running() != 0 {
		t.Error("a sandbox was left running after a failed launch")
	}
	if f.platform.Revoked() != 1 {
		t.Error("git tokens were not revoked")
	}
}

func TestLaunchRevokesTokensWhenSandboxCreationFails(t *testing.T) {
	f := newFixture(t)
	f.provider.CreateErr = errors.New("modal is down")

	if _, err := f.launcher.Launch(context.Background(), f.opts); err == nil {
		t.Fatal("expected an error")
	}
	// The token was minted before the sandbox was attempted, so it must
	// be released even though no sandbox exists.
	if f.platform.Revoked() != 1 {
		t.Errorf("git tokens were not revoked (revoked=%d)", f.platform.Revoked())
	}
}

func TestLaunchRevokesTokensMintedBeforeABootstrapFailure(t *testing.T) {
	f := newFixture(t)
	good := git.NewFake()
	good.PlatformName = "good"
	bad := git.NewFake()
	bad.PlatformName = "bad"
	bad.HostName = "bad.example.com"
	bad.IssueErr = errors.New("403")
	f.opts.Selections = []git.Selection{
		{Platform: good, Repos: good.Repos},
		{Platform: bad, Repos: bad.Repos},
	}

	if _, err := f.launcher.Launch(context.Background(), f.opts); err == nil {
		t.Fatal("expected an error")
	}
	if good.Revoked() != 1 {
		t.Error("the token minted before the failure was not revoked")
	}
	if f.provider.Running() != 0 {
		t.Error("a sandbox was created despite the bootstrap failing")
	}
}

func TestLaunchFailsWithoutRepos(t *testing.T) {
	f := newFixture(t)
	f.opts.Selections = nil

	if _, err := f.launcher.Launch(context.Background(), f.opts); err == nil {
		t.Fatal("a session with no repositories was launched")
	}
	if f.provider.Running() != 0 {
		t.Error("a sandbox was created despite there being nothing to clone")
	}
}

func TestLaunchPasswordFailureIsFatal(t *testing.T) {
	f := newFixture(t)
	f.launcher.NewPassword = func() (string, error) { return "", errors.New("no entropy") }

	if _, err := f.launcher.Launch(context.Background(), f.opts); err == nil {
		t.Fatal("a session was launched without a password")
	}
	if len(f.platform.Requested()) != 0 {
		t.Error("a git token was minted before the password failure was noticed")
	}
}

func TestSessionClose(t *testing.T) {
	f := newFixture(t)
	sess, err := f.launcher.Launch(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}

	if err := sess.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.provider.Running() != 0 {
		t.Error("Close left the sandbox running")
	}
	if f.platform.Revoked() != 1 {
		t.Error("Close did not revoke the git tokens")
	}
}

// TestSessionDetachKeepsEverythingAlive covers --keep: the sandbox and its
// tokens must survive spov exiting.
//
// @decision(DL-029)
func TestSessionDetachKeepsEverythingAlive(t *testing.T) {
	f := newFixture(t)
	sess, err := f.launcher.Launch(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}

	if err := sess.Detach(); err != nil {
		t.Fatal(err)
	}
	if f.provider.Running() != 1 {
		t.Error("Detach terminated the sandbox")
	}
	if f.platform.Revoked() != 0 {
		t.Error("Detach revoked tokens the running session still needs")
	}
}

func TestSessionCloseReportsBothFailures(t *testing.T) {
	f := newFixture(t)
	sess, err := f.launcher.Launch(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	f.provider.TerminateErr = errors.New("terminate failed")
	f.platform.RevokeErr = errors.New("revoke failed")

	if err := sess.Close(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	// Revocation must still be attempted even though termination failed.
	if len(f.platform.Requested()) != 1 {
		t.Error("no token was ever requested")
	}
}

func TestSessionCloseAndDetachTolerateNoHandle(t *testing.T) {
	var s Session
	if err := s.Close(context.Background()); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := s.Detach(); err != nil {
		t.Errorf("Detach: %v", err)
	}
}

func TestLaunchReportsProgress(t *testing.T) {
	f := newFixture(t)
	var progress strings.Builder
	f.launcher.Progress = &progress

	if _, err := f.launcher.Launch(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	// Asserted against the declared steps rather than fixed prose: the
	// step names are budgeted to the drawn view's block width (DL-024),
	// so the wording belongs to the constants, not to this test.
	out := progress.String()
	for _, step := range LaunchSteps() {
		if !strings.Contains(out, string(step)) {
			t.Errorf("progress does not mention step %q:\n%s", step, out)
		}
	}
	// Progress is written to the user's terminal.
	if strings.Contains(out, "test-password") {
		t.Error("progress output leaked the session password")
	}
}

// TestALaunchLandsInTheWorkspaceAndNotInARepository holds the half of
// DL-037 the interactive path reaches: nothing picks a repository on the
// caller's behalf, however many were cloned.
//
// It asserts on the launched session rather than on a helper, because
// a default applied one layer above the function that reads the landing
// would pass a test of that function alone.
//
// @decision(DL-037)
func TestALaunchLandsInTheWorkspaceAndNotInARepository(t *testing.T) {
	f := newFixture(t)

	sess, err := f.launcher.Launch(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.Clones) == 0 {
		t.Fatal("nothing was cloned, so this proves nothing about not picking one")
	}
	if sess.Landing != opencode.WorkspaceRoot {
		t.Errorf("landing = %q, want %q: a clone was picked as the landing",
			sess.Landing, opencode.WorkspaceRoot)
	}
	// The script the sandbox actually runs, not just the reported view.
	script := strings.Join(f.provider.Created[0].Command, " ")
	if !strings.Contains(script, "cd '"+opencode.WorkspaceRoot+"'\n") {
		t.Errorf("the entrypoint does not cd to %s:\n%s", opencode.WorkspaceRoot, script)
	}
}

// TestLaunchTerminatesOnContextCancellation is the Ctrl-C path: a user
// interrupting a launch must not leave a sandbox billing.
//
// @decision(DL-029)
func TestLaunchTerminatesOnContextCancellation(t *testing.T) {
	f := newFixture(t)

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel while the launcher is waiting for the server, which is the
	// longest phase and the likeliest moment for an interrupt.
	f.launcher.NewClient = func(string, string) HealthChecker {
		cancel()
		return &fakeHealth{err: context.Canceled}
	}

	_, err := f.launcher.Launch(ctx, Options(f.opts))
	if err == nil {
		t.Fatal("expected the cancellation to be reported")
	}
	if f.provider.Running() != 0 {
		t.Errorf("%d sandboxes left running after an interrupted launch", f.provider.Running())
	}
	if f.platform.Revoked() != 1 {
		t.Error("git tokens were not revoked after an interrupted launch")
	}
}

// TestCleanupSurvivesACancelledContext: cleanup runs with a context
// detached from the caller's, or a Ctrl-C would prevent the very teardown
// it should trigger. The cancellation happens after the sandbox exists,
// which is the case that actually matters — cancelling earlier just stops
// the launch before anything is created.
//
// @decision(DL-029)
func TestCleanupSurvivesACancelledContext(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())

	// Cancel while waiting for the server, by which point the sandbox is
	// created and billing. The fake rejects calls on a cancelled context,
	// so a cleanup that reused ctx would fail to terminate.
	f.launcher.NewClient = func(string, string) HealthChecker {
		cancel()
		return &fakeHealth{err: context.Canceled}
	}

	if _, err := f.launcher.Launch(ctx, Options(f.opts)); err == nil {
		t.Fatal("expected an error")
	}
	if len(f.provider.Created) != 1 {
		t.Fatalf("the sandbox was never created, so this proves nothing: %+v", f.provider.Created)
	}
	if f.provider.Running() != 0 {
		t.Error("cleanup did not terminate the sandbox because the context was cancelled")
	}
}

// TestLaunchConfiguresForgeCLIs: the sandbox must be able to open a pull
// request, which means the CLI is authenticated and the agent is told
// which one to use.
//
// @decision(DL-032)
func TestLaunchConfiguresForgeCLIs(t *testing.T) {
	f := newFixture(t)
	f.platform.PlatformName = "codefloe"
	f.platform.HostName = "codefloe.com"

	sess, err := f.launcher.Launch(context.Background(), Options(f.opts))
	if err != nil {
		t.Fatal(err)
	}
	_ = sess

	spec := f.provider.Created[0]
	script := strings.Join(spec.Command, " ")

	// The guide names the repository and its CLI.
	if !strings.Contains(script, "AGENTS.md") {
		t.Error("no workspace guide was written")
	}
	if !strings.Contains(script, "`tea`") {
		t.Errorf("the guide does not name tea for a forgejo platform:\n%s", script)
	}

	// The CLI reads its token from the environment. The same secret is
	// necessarily present in the clone URL (DL-041), so the property
	// worth asserting is that the login step references the variable
	// rather than embedding the value a second time.
	tokenVar := gitTokenEnvVar("codefloe")
	if spec.Secrets[tokenVar] == "" {
		t.Errorf("the platform token did not reach the sandbox secrets as %s", tokenVar)
	}
	if _, plain := spec.Env[tokenVar]; plain {
		t.Error("the platform token was sent as plain environment metadata")
	}
	if !strings.Contains(script, "--token \"$"+tokenVar+"\"") {
		t.Errorf("tea login does not read the token from the environment:\n%s", script)
	}
	if !strings.Contains(script, "tea login add") {
		t.Error("tea was not authenticated")
	}
}

func TestLaunchSkipsCLIsItDoesNotKnow(t *testing.T) {
	f := newFixture(t)
	f.platform.PlatformKind = "bitbucket"

	if _, err := f.launcher.Launch(context.Background(), Options(f.opts)); err != nil {
		t.Fatal(err)
	}
	script := strings.Join(f.provider.Created[0].Command, " ")
	if strings.Contains(script, "tea login") || strings.Contains(script, opencode.GhConfigDir) || strings.Contains(script, opencode.GlabConfigDir) {
		t.Error("a CLI was configured for a platform spov has no CLI for")
	}
	// The guide should still tell the agent what to do.
	if !strings.Contains(script, "web UI") {
		t.Error("the guide does not say what to do without a CLI")
	}
}

func TestGitTokenEnvVar(t *testing.T) {
	tests := []struct{ in, want string }{
		{"codefloe", "SPOV_GIT_TOKEN_CODEFLOE"},
		{"my-forge", "SPOV_GIT_TOKEN_MY_FORGE"},
		{"a.b", "SPOV_GIT_TOKEN_A_B"},
	}
	for _, tt := range tests {
		if got := gitTokenEnvVar(tt.in); got != tt.want {
			t.Errorf("gitTokenEnvVar(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	// Two platforms must not collide onto one variable.
	if gitTokenEnvVar("a-b") == gitTokenEnvVar("a_c") {
		t.Error("distinct platform names produced the same variable")
	}
}

// TestLaunchSendsNoCredentialAsPlainMetadata is the property the Modal
// Secret exists for: Env values are ordinary sandbox metadata in Modal's
// control plane, Secrets are redacted there.
//
// @decision(DL-036)
func TestLaunchSendsNoCredentialAsPlainMetadata(t *testing.T) {
	f := newFixture(t)
	f.opts.ProviderKeys = map[string]string{"OPENROUTER_API_KEY": "sk-or-secret"}

	if _, err := f.launcher.Launch(context.Background(), Options(f.opts)); err != nil {
		t.Fatal(err)
	}
	spec := f.provider.Created[0]

	// Plain env carries only what a Dockerfile ENV would have, since the
	// sandbox entrypoint does not inherit it.
	for k := range spec.Env {
		if k != "HOME" && k != "PATH" {
			t.Errorf("unexpected plain environment variable %q; credentials belong in Secrets", k)
		}
	}

	secrets := []string{"test-password", "sk-or-secret", "fake-secret-1"}
	for _, secret := range secrets {
		for k, v := range spec.Env {
			if strings.Contains(v, secret) {
				t.Errorf("a credential leaked into plain env as %q", k)
			}
		}
		for _, arg := range spec.Command {
			if strings.Contains(arg, secret) {
				t.Error("a credential leaked into the entrypoint command")
			}
		}
	}

	// And they did arrive, via the secret channel.
	if spec.Secrets[opencode.PasswordEnvVar] == "" || spec.Secrets["OPENROUTER_API_KEY"] == "" {
		t.Errorf("credentials did not reach the sandbox: %d secrets", len(spec.Secrets))
	}
}

// recordingSteps captures what a drawn view would be told.
type recordingSteps struct {
	started []Step
	done    []Step
	notes   []string
}

func (r *recordingSteps) Start(s Step)  { r.started = append(r.started, s) }
func (r *recordingSteps) Done(s Step)   { r.done = append(r.done, s) }
func (r *recordingSteps) Note(m string) { r.notes = append(r.notes, m) }

// TestLaunchReportsEveryStep: a view lays the steps out before any run,
// so each declared step must actually start and finish.
//
// @decision(DL-024)
func TestLaunchReportsEveryStep(t *testing.T) {
	f := newFixture(t)
	rec := &recordingSteps{}
	f.launcher.Steps = rec

	if _, err := f.launcher.Launch(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}

	for _, step := range LaunchSteps() {
		if !containsStep(rec.started, step) {
			t.Errorf("step %q never started", step)
		}
		if !containsStep(rec.done, step) {
			t.Errorf("step %q never completed", step)
		}
	}
	if len(rec.started) != len(LaunchSteps()) {
		t.Errorf("started %d steps, want %d", len(rec.started), len(LaunchSteps()))
	}
}

// TestStepsTakePrecedenceOverProgress: a view and an io.Writer must not
// both receive the same launch, or the plain lines would corrupt the
// drawn screen.
//
// @decision(DL-024)
func TestStepsTakePrecedenceOverProgress(t *testing.T) {
	f := newFixture(t)
	var progress strings.Builder
	rec := &recordingSteps{}
	f.launcher.Progress = &progress
	f.launcher.Steps = rec

	if _, err := f.launcher.Launch(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	if progress.Len() != 0 {
		t.Errorf("plain progress was written while a view was attached:\n%s", progress.String())
	}
	if len(rec.started) == 0 {
		t.Error("the view received no steps")
	}
}

// TestNotesReachTheView: warnings during cleanup must not be lost when
// a view replaces the writer.
//
// @decision(DL-024)
func TestNotesReachTheView(t *testing.T) {
	f := newFixture(t)
	rec := &recordingSteps{}
	f.launcher.Steps = rec
	f.provider.TerminateErr = errors.New("modal refused")
	f.health.err = errors.New("never healthy")

	if _, err := f.launcher.Launch(context.Background(), f.opts); err == nil {
		t.Fatal("expected the launch to fail")
	}
	if len(rec.notes) == 0 {
		t.Error("a cleanup warning did not reach the view")
	}
}

func containsStep(steps []Step, want Step) bool {
	for _, s := range steps {
		if s == want {
			return true
		}
	}
	return false
}

// TestStepNamesFitTheBudget: step names are drawn inside a fixed block
// (DL-024), so a longer name would be silently truncated on screen.
// Enforced here, next to the names, rather than only in the view.
//
// @decision(DL-024)
func TestStepNamesFitTheBudget(t *testing.T) {
	for _, step := range LaunchSteps() {
		if n := len([]rune(string(step))); n > MaxStepNameWidth {
			t.Errorf("step %q is %d cells, want at most %d", step, n, MaxStepNameWidth)
		}
	}
}

// TestLaunchCarriesSandboxConfigIntoTheSpec: every option a caller sets
// must reach the provider, or a user configures something that is
// silently ignored.
//
// @decision(DL-029) @decision(DL-031)
func TestLaunchCarriesSandboxConfigIntoTheSpec(t *testing.T) {
	f := newFixture(t)
	f.opts.CPULimit = 2
	f.opts.MemoryLimitMiB = 4096
	f.opts.Regions = []string{"us-east-1", "eu-west-1"}
	f.opts.Cloud = "aws"
	f.opts.Workdir = "/workspace"
	f.opts.InboundCIDRs = []string{"203.0.113.0/24"}
	f.opts.Egress = sandbox.EgressAgent()

	if _, err := f.launcher.Launch(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	if len(f.provider.Created) != 1 {
		t.Fatalf("%d sandboxes created", len(f.provider.Created))
	}
	spec := f.provider.Created[0]
	if spec.CPULimit != 2 || spec.MemoryLimitMiB != 4096 {
		t.Errorf("limits = %v/%d", spec.CPULimit, spec.MemoryLimitMiB)
	}
	if len(spec.Regions) != 2 || spec.Cloud != "aws" || spec.Workdir != "/workspace" {
		t.Errorf("placement = %v %q %q", spec.Regions, spec.Cloud, spec.Workdir)
	}
	if len(spec.InboundCIDRs) != 1 {
		t.Errorf("inbound = %v", spec.InboundCIDRs)
	}
	if spec.Egress.Preset != sandbox.PresetAgent || spec.Egress.Open() {
		t.Errorf("egress = %+v, want the agent preset", spec.Egress)
	}
}

// TestLaunchWritesTheModelIntoTheSandboxConfig: Options.Model is only
// worth carrying if it reaches the sandbox, and the config file is the
// one hop that puts it there. Read out of the entrypoint script the
// sandbox is actually created with, rather than off the Options — a
// field that is plumbed to the launcher and dropped before the spec
// looks identical from every other angle.
//
// @decision(DL-014) @decision(DL-038)
func TestLaunchWritesTheModelIntoTheSandboxConfig(t *testing.T) {
	f := newFixture(t)
	f.opts.Model = "anthropic/claude-sonnet-5"

	if _, err := f.launcher.Launch(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	if len(f.provider.Created) != 1 {
		t.Fatalf("created %d sandboxes", len(f.provider.Created))
	}
	script := strings.Join(f.provider.Created[0].Command, " ")
	if !strings.Contains(script, `"model": "anthropic/claude-sonnet-5"`) {
		t.Errorf("the entrypoint carries no model:\n%s", script)
	}
}
