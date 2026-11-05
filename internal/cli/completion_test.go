package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/server/client"
	"github.com/jimutt/spov/internal/version"
)

// completions runs cobra's hidden completion command and returns the
// names offered, without their descriptions.
func completions(t *testing.T, env Env, args ...string) []string {
	t.Helper()
	out, _, err := runWith(t, env, append([]string{"__complete"}, args...)...)
	if err != nil {
		t.Fatalf("__complete %v: %v", args, err)
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		name, _, _ := strings.Cut(line, "\t")
		names = append(names, name)
	}
	return names
}

// attach and stop complete the live sessions by name, and by ID once
// one is being typed.
//
// @decision(DL-023)
func TestSessionsCompleteFromTheServer(t *testing.T) {
	f := newAPIFixture(t)
	f.seedSession("morning", time.Hour)
	f.seedSession("gone", time.Minute)
	if _, _, err := runWith(t, f.env, "stop", "gone"); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"attach", "stop"} {
		if got := completions(t, f.env, cmd, ""); strings.Join(got, " ") != "morning" {
			t.Errorf("%s offers %v, want the live session's name", cmd, got)
		}
	}
	var body sessionsBody
	out, _, err := runWith(t, f.env, "ls", "--json")
	if err != nil {
		t.Fatal(err)
	}
	decodeOnly(t, out, &body)
	// The listing holds the stopped one too (DL-012); the live one is
	// what completes.
	var id string
	for _, s := range body.Sessions {
		if s.Name == "morning" {
			id = s.ID
		}
	}
	if got := completions(t, f.env, "attach", id[:3]); len(got) != 1 || got[0] != id {
		t.Errorf("an ID prefix offers %v, want %s", got, id)
	}
	if got := completions(t, f.env, "attach", "morning", ""); len(got) != 0 {
		t.Errorf("a second argument offers %v", got)
	}
}

// Job IDs come unfinished first, newest first within each; the acts
// leave the finished out.
//
// @decision(DL-023)
func TestJobsCompleteUnfinishedFirst(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/jobs" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jobs":[
			{"id":"job_done","state":"succeeded","goal":"g","created_at":"2026-10-05T12:00:00Z"},
			{"id":"job_old","state":"running","goal":"g","created_at":"2026-10-05T09:00:00Z"},
			{"id":"job_new","state":"blocked","goal":"g","created_at":"2026-10-05T11:00:00Z"}]}`)
	}))
	t.Cleanup(api.Close)
	env := Env{Client: client.New(api.URL, "tok")}

	if got := strings.Join(completions(t, env, "jobs", "show", ""), " "); got != "job_new job_old job_done" {
		t.Errorf("show offers %s", got)
	}
	for _, act := range []string{"cancel", "resume", "interrupt"} {
		if got := strings.Join(completions(t, env, "jobs", act, ""), " "); got != "job_new job_old" {
			t.Errorf("%s offers %s", act, got)
		}
	}
	if got := strings.Join(completions(t, env, "jobs", "show", "job_o"), " "); got != "job_old" {
		t.Errorf("a prefix offers %s", got)
	}
}

// A hosted server's credentials complete under the names remove takes.
//
// @decision(DL-023)
func TestCredentialsCompleteFromTheServer(t *testing.T) {
	f := keyedFixture(t)
	env := f.env
	env.PromptSecret = func(string) (string, error) { return "tok", nil }
	for _, name := range []string{"forgejo:codefloe.com", "OPENROUTER_API_KEY"} {
		if _, _, err := runWith(t, env, "credentials", "add", name); err != nil {
			t.Fatalf("add %s: %v", name, err)
		}
	}
	got := strings.Join(completions(t, env, "credentials", "remove", ""), " ")
	if got != "OPENROUTER_API_KEY forgejo:codefloe.com" {
		t.Errorf("remove offers %s", got)
	}
}

// A completion takes only a server that is already up: it never spawns
// one and never stops a server of another build.
//
// @decision(DL-023) @decision(DL-002)
func TestCompletionNeverStartsOrStopsAServer(t *testing.T) {
	ctx := context.Background()
	env := localEnv(t)
	env.Spawn = func(context.Context) error { t.Error("a completion spawned a server"); return nil }
	env.StopProcess = func(int) { t.Error("a completion stopped a server") }
	if _, ok := env.existingClient(ctx); ok {
		t.Error("no state file, yet a server was found")
	}

	srv := fakeServer(t, "tok")
	env.ProcessAlive = func(int) bool { return true }
	env.writeTestState(t, localState{PID: 4242, URL: srv.URL, Token: "tok", Version: "spov 0.0.1 (deadbeef)"})
	if _, ok := env.existingClient(ctx); ok {
		t.Error("a server of another build was used")
	}

	env.writeTestState(t, localState{PID: 4242, URL: srv.URL, Token: "tok", Version: version.Get().String()})
	if c, ok := env.existingClient(ctx); !ok || c.BaseURL != srv.URL {
		t.Errorf("the running server was not found: %v, %v", c, ok)
	}

	if got := completions(t, Env{StateDir: t.TempDir(), Spawn: env.Spawn}, "attach", ""); len(got) != 0 {
		t.Errorf("with no server running, attach offers %v", got)
	}
}

// A server that does not answer in time gets no suggestions, not a
// hung shell.
//
// @decision(DL-023)
func TestASlowServerOffersNothing(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(api.Close)
	env := Env{Client: client.New(api.URL, "tok"), CompletionTimeout: 50 * time.Millisecond}
	start := time.Now()
	if got := completions(t, env, "jobs", "show", ""); len(got) != 0 {
		t.Errorf("a hung server offered %v", got)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("the completion took %s", d)
	}
}

// A configured hosted server is asked as a command would ask it, with
// the environment's token; --all on stop takes no session.
//
// @decision(DL-023)
func TestAHostedServerCompletesWithTheCallersToken(t *testing.T) {
	var auth string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"sessions":[{"id":"sess_1","name":"morning","state":"running"}]}`)
	}))
	t.Cleanup(api.Close)
	env := Env{
		Environ: config.MapEnv{Vars: map[string]string{ServerEnvVar: api.URL, ServerTokenEnvVar: "tok-env"}},
		Spawn:   func(context.Context) error { t.Error("a completion spawned a server"); return nil },
	}
	if got := strings.Join(completions(t, env, "stop", ""), " "); got != "morning" {
		t.Errorf("stop offers %s", got)
	}
	if auth != "Bearer tok-env" {
		t.Errorf("the server saw Authorization %q", auth)
	}
	if got := completions(t, env, "stop", "--all", ""); len(got) != 0 {
		t.Errorf("stop --all offers %v", got)
	}
}
