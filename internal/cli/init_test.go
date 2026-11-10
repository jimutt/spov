package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/credentials"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/version"
)

// initFixture is a machine `spov init` runs on: its user file, its
// credential store, and what gh, Modal and the terminal answer.
type initFixture struct {
	t        *testing.T
	env      Env
	cfgHome  string
	creds    *credentials.MemoryStore
	provider *buildingProvider
	// asked are the questions put, and ran the programs started.
	asked []string
	ran   []string
	// yes answers every question unless no names a word in it.
	no      string
	choices []int
	secret  string
	gh      func() (string, error)
	// offered are the options every choice listed, and account what
	// GitHub says the token is.
	offered []string
	account string
	accErr  error
	stopped []int
}

// inspectedPlatform is a git platform that also says whose token it is,
// as the real GitHub one does.
type inspectedPlatform struct {
	*git.Fake
	f *initFixture
}

func (p inspectedPlatform) Inspect(context.Context) (git.TokenInfo, error) {
	return git.TokenInfo{User: p.f.account}, p.f.accErr
}

func newInitFixture(t *testing.T, userFile string) *initFixture {
	t.Helper()
	f := &initFixture{t: t, cfgHome: t.TempDir(), creds: credentials.NewMemoryStore(), provider: newBuildingProvider(),
		secret: "sk-pasted-secret", account: "ada", gh: func() (string, error) { return "gho_token", nil }}
	if userFile != "" {
		writeUserConfig(t, f.cfgHome, userFile)
	}
	platform := git.NewFake()
	platform.PlatformName, platform.HostName = "github", "github.com"
	f.env = Env{
		Environ: config.MapEnv{Vars: map[string]string{
			"XDG_CONFIG_HOME": f.cfgHome, "MODAL_TOKEN_ID": "id", "MODAL_TOKEN_SECRET": "secret",
		}},
		Credentials: f.creds,
		Provider:    f.provider,
		GhToken:     func(context.Context, string) (string, error) { return f.gh() },
		NewPlatform: func(context.Context, config.Platform, string) (git.Platform, error) {
			return inspectedPlatform{platform, f}, nil
		},
		StateDir:    t.TempDir(),
		StopProcess: func(pid int) { f.stopped = append(f.stopped, pid) },
		LocateBinary: func(_ context.Context, v string) (opencode.Located, error) {
			return opencode.Located{Path: "/bin/opencode", Version: v}, nil
		},
		ProxyBinary: func() []byte { return []byte("proxy") },
		Confirm: func(q string) (bool, error) {
			f.asked = append(f.asked, q)
			return f.no == "" || !strings.Contains(q, f.no), nil
		},
		Choose: func(q string, options []string) (int, error) {
			f.asked = append(f.asked, q)
			f.offered = append(f.offered, options...)
			if len(f.choices) == 0 {
				return -1, nil
			}
			c := f.choices[0]
			f.choices = f.choices[1:]
			return c, nil
		},
		PromptSecret: func(string) (string, error) { return f.secret, nil },
		RunInteractive: func(_ context.Context, name string, args ...string) error {
			f.ran = append(f.ran, strings.Join(append([]string{name}, args...), " "))
			return nil
		},
		LookPath: func(name string) (string, error) { return "", errors.New("not found") },
		Spawn:    func(context.Context) error { t.Error("init spawned a server"); return nil },
	}
	return f
}

func (f *initFixture) run(args ...string) (string, string, error) {
	f.t.Helper()
	return runWith(f.t, f.env, append([]string{"init"}, args...)...)
}

func (f *initFixture) userFile() string {
	b, _ := os.ReadFile(filepath.Join(f.cfgHome, "spov", config.UserFileName))
	return string(b)
}

// ready makes the fixture a finished setup.
func (f *initFixture) ready() {
	f.t.Helper()
	writeUserConfig(f.t, f.cfgHome, "platforms:\n  - name: github\n    kind: github\n    account: ada\n")
	key, _ := credentials.PlatformKey("github")
	_ = f.creds.Set(key, "gh:github.com")
	pkey, _ := credentials.ProviderKey("ANTHROPIC_API_KEY")
	_ = f.creds.Set(pkey, "sk-stored")
	_, _ = f.provider.EnsureImage(context.Background(), sandbox.ImageSpec{OpenCodeVersion: config.DefaultOpenCodeVersion})
	f.provider.built = f.provider.built[:1]
}

func exitCode(err error) int {
	var exit *ExitError
	if errors.As(err, &exit) {
		return exit.Code
	}
	if err != nil {
		return -1
	}
	return 0
}

// A finished setup is five ticks, no question and what to run next.
//
// @decision(DL-123)
func TestInitOnAReadySetupAsksNothing(t *testing.T) {
	f := newInitFixture(t, "")
	f.ready()
	out, _, err := f.run()
	if err != nil || strings.Count(out, "✓ ok") != 5 || !strings.Contains(out, "Ready.") || !strings.Contains(out, "spov fly --repo owner/name") {
		t.Errorf("a ready setup: %v\n%s", err, out)
	}
	if len(f.asked) != 0 || len(f.ran) != 0 || len(f.provider.built) != 1 {
		t.Errorf("a ready setup asked %v, ran %v, built %d", f.asked, f.ran, len(f.provider.built))
	}
}

// A fresh machine with gh signed in is taken to ready by saying yes:
// the platform is written, gh's login opted into, a pasted key stored
// and the image built.
//
// @decision(DL-124)
func TestInitSetsUpAFreshMachine(t *testing.T) {
	f := newInitFixture(t, "sandbox:\n  egress: install # mine\n")
	f.choices = []int{0, 0}
	out, stderr, err := f.run()
	if err != nil || !strings.Contains(out, "Ready.") {
		t.Fatalf("init: %v\n%s", err, out)
	}
	out += stderr
	file := f.userFile()
	if !strings.HasPrefix(file, "sandbox:\n  egress: install # mine\n") || !strings.Contains(file, "name: github") ||
		!strings.Contains(file, `account: "ada"`) {
		t.Errorf("the user file:\n%s", file)
	}
	key, _ := credentials.PlatformKey("github")
	if v, _ := f.creds.Get(key); v != "gh:github.com" {
		t.Errorf("the platform's credential is %q, want gh's marker and never gh's token", v)
	}
	pkey, _ := credentials.ProviderKey(providerEnvVars[0])
	found := false
	for _, name := range providerEnvVars {
		k, _ := credentials.ProviderKey(name)
		if v, _ := f.creds.Get(k); v == f.secret {
			found = true
		}
	}
	if _, err := f.creds.Get(pkey); !found {
		t.Errorf("the pasted key was not stored: %v", err)
	}
	if strings.Contains(out, f.secret) || strings.Contains(out, "gho_token") {
		t.Errorf("a secret was printed:\n%s", out)
	}
	if len(f.provider.built) != 1 {
		t.Errorf("the image was built %d times", len(f.provider.built))
	}
	// Run again: nothing to do.
	f.asked = nil
	if out, _, err := f.run(); err != nil || len(f.asked) != 0 || strings.Count(out, "✓ ok") != 5 {
		t.Errorf("a second run asked %v: %v\n%s", f.asked, err, out)
	}
}

// A no at every question changes nothing and leaves the setup not ready.
//
// @decision(DL-124)
func TestInitChangesNothingWithoutAYes(t *testing.T) {
	f := newInitFixture(t, "")
	f.no = "?"
	out, _, err := f.run()
	if exitCode(err) != 1 || !strings.Contains(out, "Not ready yet") {
		t.Errorf("init: %v\n%s", err, out)
	}
	if f.userFile() != "" || len(f.provider.built) != 0 || len(f.ran) != 0 {
		t.Errorf("a no still wrote %q, built %d, ran %v", f.userFile(), len(f.provider.built), f.ran)
	}
	key, _ := credentials.PlatformKey("github")
	if _, err := f.creds.Get(key); err == nil {
		t.Error("gh's login was opted into without a yes")
	}
}

// --check and a run without a terminal ask nothing, write nothing and
// exit 1 when a required step is missing; --json is init's own document.
//
// @decision(DL-123)
func TestInitCheckChangesNothing(t *testing.T) {
	f := newInitFixture(t, "")
	for _, args := range [][]string{{"--check"}, {"--json"}} {
		out, _, err := f.run(args...)
		key, _ := credentials.PlatformKey("github")
		_, stored := f.creds.Get(key)
		if exitCode(err) != 1 || len(f.asked) != 0 || f.userFile() != "" || len(f.provider.built) != 0 || len(f.ran) != 0 || stored == nil {
			t.Errorf("%v: exit %v, asked %v, file %q\n%s", args, err, f.asked, f.userFile(), out)
		}
		if args[0] != "--json" {
			continue
		}
		var view InitView
		if err := json.Unmarshal([]byte(out), &view); err != nil || view.Ready || len(view.Steps) != 5 {
			t.Fatalf("--json: %v\n%s", err, out)
		}
		states := map[string]string{}
		for _, s := range view.Steps {
			states[s.Name] = s.State
		}
		if states[stepModal] != stepOK || states[stepGit] != stepMissing || states[stepModel] != stepMissing ||
			states[stepClient] != stepOK || states[stepImage] != stepMissing {
			t.Errorf("states %v", states)
		}
	}
	f.env.Confirm = nil
	_, stderr, err := f.run()
	if exitCode(err) != 1 || !strings.Contains(stderr, "only checks") || f.userFile() != "" {
		t.Errorf("without a terminal: %v, %q, file %q", err, stderr, f.userFile())
	}
	f.ready()
	if out, _, err := f.run("--json"); err != nil || !strings.Contains(out, `"ready": true`) {
		t.Errorf("a ready setup's --json: %v\n%s", err, out)
	}
}

// Modal is checked by using it, the image waits for it, and the OpenCode
// sign-in is not offered without it.
//
// @decision(DL-123) @decision(DL-124)
func TestInitWithoutModal(t *testing.T) {
	f := newInitFixture(t, "")
	f.env.Environ = config.MapEnv{Vars: map[string]string{"XDG_CONFIG_HOME": f.cfgHome}}
	f.env.LookPath = func(name string) (string, error) {
		if name == "uvx" {
			return "/bin/uvx", nil
		}
		return "", errors.New("not found")
	}
	f.choices = []int{1}
	out, _, err := f.run()
	if exitCode(err) != 1 || len(f.ran) != 1 || f.ran[0] != "uvx modal setup" {
		t.Errorf("exit %v, ran %v\n%s", err, f.ran, out)
	}
	if !strings.Contains(out, "5. Sandbox image  – waiting") || !strings.Contains(out, "waits for Modal") {
		t.Errorf("the image step did not wait for Modal:\n%s", out)
	}
	if strings.Contains(strings.Join(f.offered, "|"), "Sign in to OpenCode") || !strings.Contains(out, "offered once Modal is connected") {
		t.Errorf("the OpenCode sign-in was offered without Modal: %v\n%s", f.offered, out)
	}
	// With Modal it is one of the choices.
	m := newInitFixture(t, "")
	_, _, _ = m.run()
	if !strings.Contains(strings.Join(m.offered, "|"), "Sign in to OpenCode") {
		t.Errorf("with Modal the sign-in was not offered: %v", m.offered)
	}

	// Credentials that are found and refused are step 1's failure.
	g := newInitFixture(t, "")
	g.ready()
	g.env.Provider = nil
	g.env.NewProvider = func(context.Context, config.Config) (sandbox.Provider, error) {
		return nil, errors.New("token rejected")
	}
	out, _, err = g.run("--check")
	if exitCode(err) != 1 || !strings.Contains(out, "could not use them with Modal: token rejected") {
		t.Errorf("refused credentials: %v\n%s", err, out)
	}

	// Half a pair is said as that, and no sign-in is offered for it.
	h := newInitFixture(t, "")
	h.env.Environ = config.MapEnv{Vars: map[string]string{"XDG_CONFIG_HOME": h.cfgHome, "MODAL_TOKEN_ID": "id"}}
	h.env.Provider = nil
	h.env.NewProvider = func(context.Context, config.Config) (sandbox.Provider, error) {
		return nil, errors.New("no token")
	}
	out, _, _ = h.run()
	if !strings.Contains(out, "must be set together") || len(h.ran) != 0 {
		t.Errorf("half a pair: ran %v\n%s", h.ran, out)
	}
}

// gh missing or signed out are told apart, a marker counts only while gh
// answers, and any working platform makes the step ok.
//
// @decision(DL-123) @decision(DL-124)
func TestInitAndGh(t *testing.T) {
	f := newInitFixture(t, "")
	f.gh = func() (string, error) { return "", errGhMissing }
	out, _, _ := f.run()
	if !strings.Contains(out, "https://cli.github.com") || f.userFile() != "" {
		t.Errorf("gh not installed:\n%s", out)
	}

	f = newInitFixture(t, "")
	signedIn := false
	f.gh = func() (string, error) {
		if !signedIn {
			return "", errGhSignedOut
		}
		return "gho_token", nil
	}
	f.env.RunInteractive = func(_ context.Context, name string, args ...string) error {
		f.ran = append(f.ran, name+" "+strings.Join(args, " "))
		signedIn = true
		return nil
	}
	out, _, _ = f.run()
	if len(f.ran) == 0 || !strings.HasPrefix(f.ran[0], "gh auth login --hostname github.com") || !strings.Contains(f.userFile(), "name: github") {
		t.Errorf("gh signed out: ran %v\n%s", f.ran, out)
	}

	// The marker is there but gh has since signed out.
	f = newInitFixture(t, "")
	f.ready()
	f.gh = func() (string, error) { return "", errGhSignedOut }
	out, _, err := f.run("--check")
	if exitCode(err) != 1 || !strings.Contains(out, "2. Git platform  ✗ missing") {
		t.Errorf("a marker with gh signed out: %v\n%s", err, out)
	}

	// A Forgejo-only setup is ready, and init leaves it alone.
	f = newInitFixture(t, "")
	f.ready()
	writeUserConfig(t, f.cfgHome, "platforms:\n  - name: codefloe\n    kind: forgejo\n    base_url: https://codefloe.com\n    account: ada\n")
	key, _ := credentials.PlatformKey("codefloe")
	_ = f.creds.Set(key, "forgejo-token")
	out, _, err = f.run()
	if err != nil || !strings.Contains(out, "codefloe (forgejo)") || len(f.asked) != 0 {
		t.Errorf("a Forgejo setup: %v, asked %v\n%s", err, f.asked, out)
	}
}

// A user file that already has a platforms list is never rewritten: the
// lines to add are printed.
//
// @decision(DL-124)
func TestInitDoesNotRewriteAPlatformsList(t *testing.T) {
	before := "platforms:\n  - name: codefloe # mine\n    kind: forgejo\n    base_url: https://codefloe.com\n"
	f := newInitFixture(t, before)
	out, _, _ := f.run()
	if f.userFile() != before || !strings.Contains(out, "- name: github") || !strings.Contains(out, "is not rewritten") {
		t.Errorf("file:\n%s\nout:\n%s", f.userFile(), out)
	}
	key, _ := credentials.PlatformKey("github")
	if _, err := f.creds.Get(key); err == nil {
		t.Error("the marker was stored for a platform the file does not have")
	}
}

// Without the credential proxy the token would live inside the sandbox,
// which takes a question of its own.
//
// @decision(DL-124) @decision(DL-034)
func TestInitAsksTwiceWithoutTheProxy(t *testing.T) {
	f := newInitFixture(t, "")
	f.env.ProxyBinary = func() []byte { return nil }
	f.no = "inside the sandboxes"
	out, _, _ := f.run()
	if !strings.Contains(out, "INSIDE every sandbox") || f.userFile() != "" {
		t.Errorf("without the proxy a first yes was enough:\n%s\nfile: %q", out, f.userFile())
	}
	key, _ := credentials.PlatformKey("github")
	if _, err := f.creds.Get(key); err == nil {
		t.Error("gh's login was stored after a no to the sandbox question")
	}
}

// An exported key satisfies the model step and is not copied into the
// store; a missing client is no failure and changes the closing command.
//
// @decision(DL-123) @decision(DL-124)
func TestInitModelAndClient(t *testing.T) {
	f := newInitFixture(t, "")
	f.ready()
	pkey, _ := credentials.ProviderKey("ANTHROPIC_API_KEY")
	_ = f.creds.Delete(pkey)
	f.env.Environ.(config.MapEnv).Vars["OPENROUTER_API_KEY"] = "sk-exported"
	f.env.LocateBinary = func(context.Context, string) (opencode.Located, error) {
		return opencode.Located{}, errors.New("no opencode client found\nmore")
	}
	out, _, err := f.run()
	if err != nil || !strings.Contains(out, "OPENROUTER_API_KEY exported") || !strings.Contains(out, "spov fly --web") ||
		!strings.Contains(out, "4. OpenCode client  ✗ not found") {
		t.Errorf("init: %v\n%s", err, out)
	}
	okey, _ := credentials.ProviderKey("OPENROUTER_API_KEY")
	if _, err := f.creds.Get(okey); err == nil {
		t.Error("an exported key was copied into the store")
	}
}

// Init reads configuration as the local server does: a project file's
// opencode version is not the image the server boots.
//
// @decision(DL-123)
func TestInitIgnoresAProjectFile(t *testing.T) {
	f := newInitFixture(t, "")
	f.ready()
	f.env.Workdir = t.TempDir()
	writeProject(t, f.env.Workdir, "opencode:\n  version: 9.9.9\n")
	out, _, err := f.run("--check")
	if err != nil || strings.Contains(out, "9.9.9") {
		t.Errorf("a project file changed the check: %v\n%s", err, out)
	}
}

// With a hosted server the local steps are the server's, and init only
// says whether the CLI is signed in.
//
// @decision(DL-123)
func TestInitWithAHostedServer(t *testing.T) {
	srv := fakeServer(t, "tok")
	f := newInitFixture(t, "server:\n  url: "+srv.URL+"\n")
	f.env.Environ.(config.MapEnv).Vars[ServerTokenEnvVar] = "tok"
	f.env.Environ.(config.MapEnv).Vars["SPOV_SERVER_ALLOW_HTTP"] = "1"
	out, _, err := f.run()
	if strings.Contains(out, "Modal  ") || len(f.asked) != 0 {
		t.Errorf("a hosted setup ran the local steps: %v\n%s", err, out)
	}
	if err != nil || !strings.Contains(out, "1. Server  ✓ ok") || !strings.Contains(out, "signed in to "+srv.URL) {
		t.Errorf("a signed-in hosted setup: %v\n%s", err, out)
	}
	f.env.Environ.(config.MapEnv).Vars[ServerTokenEnvVar] = "wrong"
	out, _, err = f.run("--check")
	if exitCode(err) != 1 || !strings.Contains(out, "not signed in") || !strings.Contains(out, "spov login") {
		t.Errorf("a signed-out hosted setup: %v\n%s", err, out)
	}
}

// The errors a first run meets name init, and the file platforms may
// actually be set in.
//
// @decision(DL-125)
func TestFirstRunErrorsNameInit(t *testing.T) {
	err := (&Env{Environ: config.MapEnv{Vars: map[string]string{}}}).noPlatforms()
	if !strings.Contains(err.Error(), "spov init") || strings.Contains(err.Error(), config.ProjectFileName) {
		t.Errorf("no platforms: %v", err)
	}
	// A server answering for its own configuration does not send its
	// caller to a local wizard.
	if err := (&Env{ServerConfig: true, Environ: config.MapEnv{Vars: map[string]string{}}}).noPlatforms(); strings.Contains(err.Error(), "spov init") {
		t.Errorf("a server's own answer: %v", err)
	}
	// With no Modal credentials the CLI says so before it spawns a
	// server that could not start.
	env := &Env{Environ: config.MapEnv{Vars: map[string]string{}}, StateDir: t.TempDir(), Err: &strings.Builder{}}
	_, err = env.spawnLocal(context.Background(), filepath.Join(env.StateDir, "server.json"))
	if err == nil || !strings.Contains(err.Error(), "Modal") || !strings.Contains(err.Error(), "spov init") {
		t.Errorf("no Modal credentials: %v", err)
	}
}

// Nothing is stored for a token GitHub would not vouch for, and a write
// that does not take effect is put back with the lines to add by hand.
//
// @decision(DL-124)
func TestInitStoresNothingItCouldNotCheck(t *testing.T) {
	f := newInitFixture(t, "")
	f.accErr = errors.New("401 bad credentials gho_token")
	out, _, _ := f.run()
	key, _ := credentials.PlatformKey("github")
	if _, err := f.creds.Get(key); err == nil || f.userFile() != "" {
		t.Errorf("an unchecked token was stored, file %q", f.userFile())
	}
	if !strings.Contains(out, "could not check gh's login") || strings.Contains(out, "gho_token") {
		t.Errorf("the refusal:\n%s", out)
	}

	// An indented root: an appended block would be silently ignored.
	before := "  sandbox:\n    egress: install\n"
	g := newInitFixture(t, before)
	out, _, _ = g.run()
	if g.userFile() != before || !strings.Contains(out, "by hand") || !strings.Contains(out, "platforms:") {
		t.Errorf("file %q\n%s", g.userFile(), out)
	}
	if _, err := g.creds.Get(key); err == nil {
		t.Error("the marker was stored for a platform the file does not have")
	}
}

// After a change, an idle local server of this build is stopped so the
// next command starts one that sees it; one with a live session, and one
// of another build, are left alone.
//
// @decision(DL-124)
func TestInitSettlesARunningLocalServer(t *testing.T) {
	for name, tc := range map[string]struct {
		sessions string
		version  string
		stopped  bool
	}{
		"idle":           {`[]`, version.Get().String(), true},
		"a live session": {`[{"id":"s1","name":"x","state":"running"}]`, version.Get().String(), false},
		"another build":  {`[]`, "spov 0.0.1 (old)", false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/me":
				_, _ = w.Write([]byte(`{"user_id":"local","org_id":"local","admin":true}`))
			case "/api/v1/sessions":
				_, _ = w.Write([]byte(`{"sessions":` + tc.sessions + `,"total":0}`))
			default:
				http.NotFound(w, r)
			}
		}))
		f := newInitFixture(t, "")
		f.choices = []int{0, 0}
		f.env.ProcessAlive = func(int) bool { return true }
		f.env.writeTestState(t, localState{PID: 4242, URL: srv.URL, Token: "tok", Version: tc.version})
		out, _, err := f.run()
		srv.Close()
		if err != nil {
			t.Fatalf("%s: %v\n%s", name, err, out)
		}
		if got := len(f.stopped) == 1 && f.stopped[0] == 4242; got != tc.stopped {
			t.Errorf("%s: stopped %v, want stopped=%v\n%s", name, f.stopped, tc.stopped, out)
		}
	}
	// A run that changed nothing touches no server.
	f := newInitFixture(t, "")
	f.ready()
	f.env.ProcessAlive = func(int) bool { return true }
	f.env.writeTestState(t, localState{PID: 4242, URL: "http://127.0.0.1:1", Token: "tok", Version: version.Get().String()})
	if _, _, _ = f.run(); len(f.stopped) != 0 {
		t.Errorf("a run that changed nothing stopped %v", f.stopped)
	}
}

// Ctrl-C ends init with 130, also when it comes at a question.
//
// @decision(DL-123)
func TestInitEndsOnAnInterrupt(t *testing.T) {
	f := newInitFixture(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	f.env.Confirm = func(string) (bool, error) {
		cancel()
		return false, nil
	}
	var out, errBuf strings.Builder
	f.env.Out, f.env.Err = &out, &errBuf
	f.env.Workdir = t.TempDir()
	err := runInit(ctx, &f.env, false, false)
	if exitCode(err) != 130 || strings.Contains(out.String(), "3. Model") {
		t.Errorf("exit %v; init went on after the interrupt:\n%s", err, out.String())
	}
}

// The prompts: a number picks an option and anything else picks none, a
// question stops waiting when the run is interrupted, and a program is
// run with the terminal.
//
// @decision(DL-123) @decision(DL-124)
func TestInitPrompts(t *testing.T) {
	options := []string{"key", "sign in", "skip"}
	for in, want := range map[string]int{"2\n": 1, " 3 \n": 2, "0\n": -1, "9\n": -1, "yes\n": -1, "": -1} {
		var out strings.Builder
		if got := chooseFrom(strings.NewReader(in), &out, "Which?", options); got != want {
			t.Errorf("%q chose %d, want %d", in, got, want)
		}
		if !strings.Contains(out.String(), "2) sign in") {
			t.Errorf("the options were not listed:\n%s", out.String())
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	block := make(chan struct{})
	defer close(block)
	if _, err := answerOrInterrupt(ctx, false, func() (bool, error) { <-block; return true, nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("an interrupted question: %v", err)
	}
	env := &Env{Out: &strings.Builder{}, Err: &strings.Builder{}}
	if err := env.runInteractive(context.Background(), "true"); err != nil {
		t.Errorf("running a program: %v", err)
	}
	if err := env.runInteractive(context.Background(), "false"); err == nil {
		t.Error("a program that failed was reported as done")
	}
	if _, err := env.choose(context.Background(), "Which?", options); err == nil {
		t.Error("a choice was read with no terminal")
	}
}

// The image step's other answers: an image of the user's own is never
// built, and a build that fails leaves the step missing with the command.
//
// @decision(DL-123) @decision(DL-124)
func TestInitImageEdges(t *testing.T) {
	f := newInitFixture(t, "")
	f.ready()
	writeUserConfig(t, f.cfgHome, f.userFile()+"sandbox:\n  image: my-own-image\n")
	out, _, err := f.run()
	if exitCode(err) != 1 || !strings.Contains(out, "sandbox.image names my-own-image") || len(f.provider.built) != 1 {
		t.Errorf("an image of the user's own: %v, built %d\n%s", err, len(f.provider.built), out)
	}
	for _, q := range f.asked {
		if strings.Contains(q, "Build it now") {
			t.Error("init offered to build the user's own image")
		}
	}

	g := newInitFixture(t, "")
	g.ready()
	g.provider.built = nil
	g.provider.err = errors.New("modal is down")
	// The fake counts a failed build as built, so only the words are
	// checked here.
	out, _, _ = g.run()
	if !strings.Contains(out, "the build did not finish (run `spov serve image build` to try again): modal is down") {
		t.Errorf("a failed build:\n%s", out)
	}

	// Environment tokens win over a sign-in, so none is offered.
	h := newInitFixture(t, "")
	h.env.Provider = nil
	h.env.NewProvider = func(context.Context, config.Config) (sandbox.Provider, error) { return nil, errors.New("refused") }
	h.env.LookPath = func(string) (string, error) { return "/bin/modal", nil }
	out, _, _ = h.run()
	if len(h.ran) != 0 || !strings.Contains(out, "win over a sign-in") {
		t.Errorf("environment tokens: ran %v\n%s", h.ran, out)
	}
}
