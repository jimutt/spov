package cli

import (
	"context"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/store"
)

// TestLoginIsOffWithoutAnOAuthApp: a deployment with no registered app
// serves the API on its bearer token alone, which is what a
// single-operator server runs until it has users.
//
// @decision(DL-007)
func TestLoginIsOffWithoutAnOAuthApp(t *testing.T) {
	env := Env{Environ: config.MapEnv{Vars: map[string]string{}}}
	login, stop, err := env.newLogin(context.Background(), store.NewMemory(), "https://spov.example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if login != nil {
		t.Error("login was configured without an OAuth app")
	}
}

// TestLoginNeedsBothOAuthVariables: half a credential is a
// misconfiguration, not a request to disable login.
//
// @decision(DL-007)
func TestLoginNeedsBothOAuthVariables(t *testing.T) {
	for _, vars := range []map[string]string{
		{GitHubClientIDEnvVar: "id"},
		{GitHubClientSecretEnvVar: "secret"},
	} {
		env := Env{Environ: config.MapEnv{Vars: vars}}
		_, _, err := env.newLogin(context.Background(), store.NewMemory(), "https://spov.example.com", nil)
		if err == nil {
			t.Fatalf("%v was accepted", vars)
		}
		if !strings.Contains(err.Error(), GitHubClientIDEnvVar) {
			t.Errorf("error %q does not name the variables", err)
		}
	}
}

// TestLoginNeedsAPublicURL: it is the origin GitHub redirects back to,
// so it cannot be inferred from the listen address.
//
// @decision(DL-007)
func TestLoginNeedsAPublicURL(t *testing.T) {
	env := Env{Environ: config.MapEnv{Vars: map[string]string{
		GitHubClientIDEnvVar:     "id",
		GitHubClientSecretEnvVar: "secret",
	}}}
	_, _, err := env.newLogin(context.Background(), store.NewMemory(), "", nil)
	if err == nil {
		t.Fatal("login was configured without a public url")
	}
	if !strings.Contains(err.Error(), PublicURLEnvVar) {
		t.Errorf("error %q does not name %s", err, PublicURLEnvVar)
	}
}

// TestLoginNeedsADatabase: the session store and the user rows both live
// in Postgres, so the memory store cannot serve a login.
//
// @decision(DL-007)
func TestLoginNeedsADatabase(t *testing.T) {
	env := Env{Environ: config.MapEnv{Vars: map[string]string{
		GitHubClientIDEnvVar:     "id",
		GitHubClientSecretEnvVar: "secret",
	}}}
	_, _, err := env.newLogin(context.Background(), store.NewMemory(), "https://spov.example.com", nil)
	if err == nil {
		t.Fatal("login was configured on the memory store")
	}
	if !strings.Contains(err.Error(), DatabaseURLEnvVar) {
		t.Errorf("error %q does not name %s", err, DatabaseURLEnvVar)
	}
}

// TestAllowlistPathDefaultsToTheMountPoint.
//
// @decision(DL-007)
func TestAllowlistPathDefaultsToTheMountPoint(t *testing.T) {
	env := Env{Environ: config.MapEnv{Vars: map[string]string{}}}
	if got := env.allowlistPath(); got != DefaultAllowlistFile {
		t.Errorf("allowlistPath = %q, want %q", got, DefaultAllowlistFile)
	}
	env.Environ = config.MapEnv{Vars: map[string]string{AllowlistFileEnvVar: "/custom/allowlist"}}
	if got := env.allowlistPath(); got != "/custom/allowlist" {
		t.Errorf("allowlistPath = %q", got)
	}
}

// TestSIGHUPIsAnsweredButNoLongerReloads pins that the signal is
// delivered and answered, not that anything is reloaded.
//
// @decision(DL-007)
func TestSIGHUPIsAnsweredButNoLongerReloads(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := store.NewMemory()
	if err := st.Allowlist().Put(ctx, store.AllowlistEntry{
		OrgID: server.DefaultOrgID, Email: "alice@example.com", Admin: true,
	}); err != nil {
		t.Fatal(err)
	}
	list, err := auth.NewAllowlist(ctx,
		auth.StoreAllowlist(st.Allowlist(), server.DefaultOrgID), auth.AllowlistRefresh, nil)
	if err != nil {
		t.Fatal(err)
	}
	login := &server.Login{
		Allowlist: list,
		Users:     st.Users(),
		OrgID:     server.DefaultOrgID,
		Warn:      func(string) {},
	}

	answered := make(chan string, 4)
	// The trap is installed before anything signals: until Notify
	// runs, SIGHUP's default action would kill the test binary.
	hup, stop := trapHUP()
	defer stop()
	go watchAllowlistOn(ctx, hup, login, func(msg string) { answered <- msg })

	if err := raiseHUP(); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-answered:
		// It must point at the store, or an operator is left editing a
		// file that does nothing.
		for _, want := range []string{"nothing to reload", "database", "/api/v1/allowlist"} {
			if !strings.Contains(msg, want) {
				t.Errorf("the reply %q does not mention %q", msg, want)
			}
		}
	case <-time.After(10 * time.Second):
		t.Fatal("SIGHUP was never answered")
	}
	// And the process is still here, which is the whole reason the trap
	// exists.
	if _, ok := list.Lookup("alice@example.com"); !ok {
		t.Error("the list was disturbed by the signal")
	}
}

// TestWatchAllowlistStopsWithTheContext, so a shutdown does not leave
// the signal handler installed.
//
// @decision(DL-007)
func TestWatchAllowlistStopsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchAllowlist(ctx, &server.Login{Warn: func(string) {}}, func(string) {})
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher outlived its context")
	}
}

// raiseHUP sends SIGHUP to this process. The test binary installs a
// handler for it before signalling, so the default "terminate" action
// never applies.
func raiseHUP() error {
	p, err := os.FindProcess(os.Getpid())
	if err != nil {
		return err
	}
	return p.Signal(syscall.SIGHUP)
}

// TestWatchAllowlistTrapsSIGHUPWithoutLogin pins that a hosted server
// with no login configured still traps SIGHUP.
//
// @decision(DL-007)
func TestWatchAllowlistTrapsSIGHUPWithoutLogin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	warned := make(chan string, 4)
	hup, stop := trapHUP()
	defer stop()
	// A nil login is a server without one.
	go watchAllowlistOn(ctx, hup, nil, func(msg string) { warned <- msg })

	if err := raiseHUP(); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-warned:
		// Reaching here at all is the point: the process is still
		// running, so the signal was trapped rather than fatal.
		if !strings.Contains(msg, "no login") {
			t.Errorf("warned %q, want it to say there is nothing to reload", msg)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("SIGHUP was never handled")
	}
}

// TestSessionCookiePolicy: the cookie is the credential, so its
// attributes are what stop a page on another site from using it.
//
// @decision(DL-007)
func TestSessionCookiePolicy(t *testing.T) {
	var warned []string
	s := newSessionManager("https://spov.example.com", func(m string) { warned = append(warned, m) })

	if !s.Cookie.Secure {
		t.Error("an https origin did not produce a Secure cookie")
	}
	if !s.Cookie.HttpOnly {
		t.Error("the cookie is not HttpOnly, so script can read it")
	}
	if s.Cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", s.Cookie.SameSite)
	}
	if s.Cookie.Name != WebSessionCookie || s.Cookie.Path != "/" {
		t.Errorf("cookie = %q at %q", s.Cookie.Name, s.Cookie.Path)
	}
	if s.Lifetime != WebSessionLifetime || s.IdleTimeout != WebSessionIdle {
		t.Errorf("lifetime %s, idle %s", s.Lifetime, s.IdleTimeout)
	}
	if len(warned) != 0 {
		t.Errorf("an https origin warned: %v", warned)
	}

	// Over plain http the cookie cannot be Secure — it would never be
	// sent back — and that is worth saying out loud.
	warned = nil
	plain := newSessionManager("http://localhost:8080", func(m string) { warned = append(warned, m) })
	if plain.Cookie.Secure {
		t.Error("an http origin produced a Secure cookie, which is never sent")
	}
	if len(warned) != 1 || !strings.Contains(warned[0], "not https") {
		t.Errorf("warnings = %v, want one about the missing Secure flag", warned)
	}
	// A nil warn is not a panic.
	newSessionManager("http://localhost:8080", nil)
}
