package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
	"github.com/jimutt/spov/internal/session"
	"github.com/jimutt/spov/internal/store"
)

// recordingSteps is a StepReporter that remembers what it was told.
type recordingSteps struct{ log []string }

func (r *recordingSteps) Start(s session.Step) { r.log = append(r.log, "start:"+string(s)) }
func (r *recordingSteps) Done(s session.Step)  { r.log = append(r.log, "done:"+string(s)) }
func (r *recordingSteps) Note(msg string)      { r.log = append(r.log, "note:"+msg) }

// TestLaunchReporterDrivesTheView: the server's step events map onto
// the same StepReporter the launcher drives, so
// the drawn view is the one the launcher would draw.
//
// @decision(DL-024)
func TestLaunchReporterDrivesTheView(t *testing.T) {
	rec := &recordingSteps{}
	r := &launchReporter{env: &Env{Err: &strings.Builder{}}, steps: rec}
	r.Step(server.StepEvent{Step: "preparing tokens", State: "started"})
	r.Step(server.StepEvent{Step: "preparing tokens", State: "done"})
	r.Note(server.NoteEvent{Message: "hello"})
	r.Step(server.StepEvent{Step: "starting sandbox", State: "failed"})
	want := "start:preparing tokens done:preparing tokens note:hello"
	if got := strings.Join(rec.log, " "); got != want {
		t.Errorf("view saw %q, want %q", got, want)
	}
}

// TestLaunchReporterPlainLinesMatchTheLauncher: without a view the
// lines are the ones session.Launcher printed, and notes are deferred
// like every other warning.
func TestLaunchReporterPlainLinesMatchTheLauncher(t *testing.T) {
	var out strings.Builder
	env := &Env{Err: &out}
	r := &launchReporter{env: env, progress: &out}
	r.Step(server.StepEvent{Step: "preparing tokens", State: "started"})
	r.Step(server.StepEvent{Step: "preparing tokens", State: "done"})
	r.Note(server.NoteEvent{Message: "careful"})
	if got := out.String(); got != "preparing tokens...\nspov: careful\n" {
		t.Errorf("output = %q", got)
	}
}

// stubAPI is a scripted server for the launch flow's failure branches.
func stubAPI(t *testing.T, events http.HandlerFunc) *client.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/sessions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprint(w, `{"id":"01X"}`)
	})
	mux.HandleFunc("GET /api/v1/sessions/{ref}/events", events)
	mux.HandleFunc("GET /api/v1/sessions/{ref}", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusBadGateway)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return client.New(srv.URL, "t")
}

// TestLaunchSessionHintsWhenTheServerIsLost: a launch whose outcome is
// unknown may be billing, so the error says how to stop it.
//
// @decision(DL-029)
func TestLaunchSessionHintsWhenTheServerIsLost(t *testing.T) {
	c := stubAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream exploded", http.StatusInternalServerError)
	})
	env := &Env{Err: &strings.Builder{}}
	_, err := env.launchSession(context.Background(), c, server.SessionRequest{Name: "lost"})
	if err == nil || !strings.Contains(err.Error(), "spov stop lost") || !strings.Contains(err.Error(), "upstream exploded") {
		t.Errorf("err = %v", err)
	}
}

// TestLaunchSessionReportsAFailedCancel: when the cancel itself fails
// the user is told the sandbox may still be running.
func TestLaunchSessionReportsAFailedCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		cancel()
		<-r.Context().Done()
	})
	env := &Env{Err: &strings.Builder{}}
	_, err := env.launchSession(ctx, c, server.SessionRequest{Name: "half"})
	if err == nil || !strings.Contains(err.Error(), "could not be cancelled") || !strings.Contains(err.Error(), "spov stop half") {
		t.Errorf("err = %v", err)
	}
}

func TestSessionErrorMapsProblems(t *testing.T) {
	stopped := &client.Error{Status: 409, Detail: "not running", Type: server.ProblemTypeSessionStopped}
	if err := sessionError("s", stopped); !strings.Contains(err.Error(), "no longer running") {
		t.Errorf("stopped: %v", err)
	}
	other := errors.New("plain")
	if err := sessionError("s", other); !errors.Is(err, other) {
		t.Errorf("other: %v", err)
	}
}

func TestLiveSessionNameFallsBackToTheID(t *testing.T) {
	s := &liveSession{View: server.SessionView{ID: "01X"}}
	if s.Name() != "01X" {
		t.Errorf("Name = %q", s.Name())
	}
	if err := s.Detach(); err != nil {
		t.Errorf("detaching without a proxy: %v", err)
	}
}

func TestEnvDefaults(t *testing.T) {
	e := &Env{}
	if e.now().IsZero() {
		t.Error("now is zero")
	}
	if e.newHealthChecker("http://127.0.0.1:1", "") == nil {
		t.Error("no default health checker")
	}
	if e.credentialBackend() != "" {
		t.Error("a backend was named without a store")
	}
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e.Now = func() time.Time { return fixed }
	if !e.now().Equal(fixed) {
		t.Error("Now was not used")
	}
}

// TestProviderIsBuiltOnceAndCached: `spov serve` connects to Modal on
// first use and keeps the connection.
func TestProviderIsBuiltOnceAndCached(t *testing.T) {
	builds := 0
	e := &Env{
		Environ: config.MapEnv{Vars: map[string]string{}},
		NewProvider: func(context.Context, config.Config) (sandbox.Provider, error) {
			builds++
			return sandbox.NewFake(), nil
		},
	}
	p1, err := e.provider(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p2, _ := e.provider(context.Background())
	if p1 != p2 || builds != 1 {
		t.Errorf("built %d providers", builds)
	}
	e = &Env{Environ: config.MapEnv{Vars: map[string]string{}},
		NewProvider: func(context.Context, config.Config) (sandbox.Provider, error) { return nil, errors.New("no modal") }}
	if _, err := e.provider(context.Background()); err == nil {
		t.Error("a failing builder was not reported")
	}
}

// TestAttachUsesTheRealClientProxyByDefault runs the whole client path
// with nothing substituted: the proxy mints through the server, and the
// health check goes through the proxy.
//
// @decision(DL-030)
func TestAttachUsesTheRealClientProxyByDefault(t *testing.T) {
	f := newAPIFixture(t)
	f.seedSession("s", time.Minute)
	f.env.StartClientProxy = nil
	if _, stderr, err := runWith(t, f.env, "attach", "s", "--no-tui"); err != nil {
		t.Fatalf("attach: %v\n%s", err, stderr)
	}
	if len(f.provider.ConnectTokens) != 1 {
		t.Errorf("%d tokens minted", len(f.provider.ConnectTokens))
	}
}

// TestFlyRequestCarriesTheConfig: what the client sends is what the
// config says, and only an explicit image is sent.
//
// @decision(DL-029)
func TestFlyRequestCarriesTheConfig(t *testing.T) {
	cfg := config.Defaults()
	cfg.Sandbox.CPU = 3
	cfg.Sandbox.InboundCIDRs = []string{"10.0.0.0/8"}
	cfg.Landing = "spov"
	req := sessionRequest(cfg, flyOptions{}, "n", []server.RepoRef{{Platform: "p", Owner: "o", Name: "r"}}, gitIdentityOf("A", "a@x"))
	if req.Image != "" {
		t.Errorf("image = %q, want none so the server's default applies", req.Image)
	}
	if req.Sandbox.CPU != 3 || req.Sandbox.TimeoutSecs != 4*3600 || len(req.Sandbox.InboundCIDRs) != 1 || req.Landing != "spov" || req.Git.Name != "A" {
		b, _ := json.Marshal(req)
		t.Errorf("request = %s", b)
	}
	cfg.Sandbox.Image = "custom:1"
	if got := sessionRequest(cfg, flyOptions{Repos: []string{"x/y"}}, "n", nil, gitIdentityOf("", "")); got.Image != "custom:1" || got.Landing != "" {
		t.Errorf("request = %+v", got)
	}
}

// TestFlyRequestCarriesTheAddons: the server derives the variant from
// them, so they travel as configured and the image stays unset.
//
// @decision(DL-033)
func TestFlyRequestCarriesTheAddons(t *testing.T) {
	cfg := config.Defaults()
	cfg.Sandbox.Addons = []string{"rust", "java@17"}
	cfg.Sandbox.Apt = []string{"jq"}
	req := sessionRequest(cfg, flyOptions{}, "n", []server.RepoRef{{Platform: "p", Owner: "o", Name: "r"}}, gitIdentityOf("", ""))
	if !slices.Equal(req.Addons, cfg.Sandbox.Addons) || !slices.Equal(req.Apt, cfg.Sandbox.Apt) || req.Image != "" {
		t.Errorf("request carries addons %v, apt %v, image %q", req.Addons, req.Apt, req.Image)
	}
}

// TestFlyRequestCarriesTheModel: --model is only worth having if it
// reaches the request. An unset one must stay unset rather than become
// a default of spov's own — an interactive session is meant to land on
// whatever OpenCode resolves, changeable in the TUI.
//
// @decision(DL-014)
func TestFlyRequestCarriesTheModel(t *testing.T) {
	cfg := config.Defaults()
	refs := []server.RepoRef{{Platform: "p", Owner: "o", Name: "r"}}

	req := sessionRequest(cfg, flyOptions{Model: "anthropic/claude-sonnet-5"}, "n", refs, gitIdentityOf("A", "a@x"))
	if req.Model != "anthropic/claude-sonnet-5" {
		t.Errorf("model = %q, want anthropic/claude-sonnet-5", req.Model)
	}
	if got := sessionRequest(cfg, flyOptions{}, "n", refs, gitIdentityOf("A", "a@x")); got.Model != "" {
		t.Errorf("an unset model became %q", got.Model)
	}
}

// The flag names git access outright; config names a preference, which
// the server may fall back from; neither sends nothing.
//
// @decision(DL-043)
func TestFlyRequestCarriesTheGitAccess(t *testing.T) {
	cfg := config.Defaults()
	refs := []server.RepoRef{{Platform: "p", Owner: "o", Name: "r"}}
	id := gitIdentityOf("A", "a@x")
	if got := sessionRequest(cfg, flyOptions{}, "n", refs, id); got.GitAccess != "" || got.GitAccessPreferred {
		t.Errorf("nothing set sent %q (preferred %v)", got.GitAccess, got.GitAccessPreferred)
	}
	cfg.Sandbox.GitAccess = "all"
	if got := sessionRequest(cfg, flyOptions{}, "n", refs, id); got.GitAccess != store.GitAccessAll || !got.GitAccessPreferred {
		t.Errorf("config sent %q (preferred %v)", got.GitAccess, got.GitAccessPreferred)
	}
	if got := sessionRequest(cfg, flyOptions{GitAccess: "repos"}, "n", refs, id); got.GitAccess != store.GitAccessRepos || got.GitAccessPreferred {
		t.Errorf("the flag sent %q (preferred %v)", got.GitAccess, got.GitAccessPreferred)
	}
}

func gitIdentityOf(name, email string) git.Identity { return git.Identity{Name: name, Email: email} }
