package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/server"
)

// A verifier with no environment cannot build a platform client, so it
// stores on the user's word rather than refusing.
func TestVerifyWithoutAnEnvIsUnsupported(t *testing.T) {
	v := &credentialVerifier{}
	if _, err := v.Verify(context.Background(), server.PlatformGitHub, "github_pat_x"); !errors.Is(err, server.ErrVerifyUnsupported) {
		t.Fatalf("err = %v, want ErrVerifyUnsupported", err)
	}
}

// A provider key has nothing to verify against, and must say so with the
// sentinel rather than an opaque failure.
func TestVerifyProviderKeyIsUnsupported(t *testing.T) {
	v := &credentialVerifier{env: envWith(nil)}
	_, err := v.Verify(context.Background(), server.ProviderPlatform("ANTHROPIC_API_KEY"), "sk-ant-x")
	if !errors.Is(err, server.ErrVerifyUnsupported) {
		t.Fatalf("err = %v, want ErrVerifyUnsupported", err)
	}
}

// The App private key is the deployment's credential and must never be
// accepted as a user's.
func TestVerifyGitHubRefusesAnAppCredential(t *testing.T) {
	v := &credentialVerifier{env: envWith(nil)}
	_, err := v.Verify(context.Background(), server.PlatformGitHub,
		`{"app_id":"1","private_key":"-----BEGIN RSA PRIVATE KEY-----"}`)
	if err == nil {
		t.Fatal("an App credential was accepted as a user credential")
	}
	if !strings.Contains(err.Error(), "personal access token") {
		t.Errorf("err = %v; it should name what a user credential is", err)
	}
}

func TestVerifyGitHubRefusesSomethingElse(t *testing.T) {
	v := &credentialVerifier{env: envWith(nil)}
	_, err := v.Verify(context.Background(), server.PlatformGitHub, "just-some-string")
	if err == nil {
		t.Fatal("a non-token was accepted")
	}
	if !strings.Contains(err.Error(), "github_pat_") {
		t.Errorf("err = %v; it should show the expected shape", err)
	}
}

// A token that authenticates but reaches nothing is useless for a
// session, and must be refused where the user can still widen it.
func TestVerifyGitHubRefusesATokenThatReachesNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/user") {
			_, _ = w.Write([]byte(`{"login":"alice"}`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	v := &credentialVerifier{env: envWith(nil), cfg: config.Config{Platforms: []config.Platform{
		{Name: "gh", Kind: config.KindGitHub, BaseURL: srv.URL},
	}}}
	_, err := v.Verify(context.Background(), server.PlatformGitHub, "github_pat_x")
	if err == nil {
		t.Fatal("a token reaching no repositories was accepted")
	}
	if !strings.Contains(err.Error(), "no repositories") {
		t.Errorf("err = %v", err)
	}
}

func TestVerifyForgejoNeedsAConfiguredHost(t *testing.T) {
	v := &credentialVerifier{env: envWith(nil), cfg: config.Config{}}
	_, err := v.Verify(context.Background(), server.ForgejoPlatform("nowhere.example"), "token")
	if err == nil {
		t.Fatal("a token for an unconfigured host was accepted")
	}
	if !strings.Contains(err.Error(), "no forgejo platform") {
		t.Errorf("err = %v", err)
	}
}

func TestPlatformOn(t *testing.T) {
	cfg := config.Config{Platforms: []config.Platform{
		{Name: "gh", Kind: config.KindGitHub},
		{Name: "cf", Kind: config.KindForgejo, BaseURL: "https://codefloe.com", Account: "alice"},
		{Name: "gl", Kind: config.KindGitLab},
	}}
	p, ok := platformOn(cfg, "forgejo", "codefloe.com")
	if !ok || p.Name != "cf" {
		t.Fatalf("platformOn = %+v, %v", p, ok)
	}
	// Host comparison is case-insensitive, as hostnames are.
	if _, ok := platformOn(cfg, "forgejo", "CodeFloe.com"); !ok {
		t.Error("the host comparison is case-sensitive")
	}
	if _, ok := platformOn(cfg, "forgejo", "other.example"); ok {
		t.Error("an unconfigured host matched")
	}
	if _, ok := platformOn(cfg, "gitea", "codefloe.com"); ok {
		t.Error("a forgejo platform answered for gitea")
	}
	// A GitLab platform with no base_url is gitlab.com's.
	if p, ok := platformOn(cfg, "gitlab", "gitlab.com"); !ok || p.Name != "gl" {
		t.Errorf("gitlab.com = %+v, %v", p, ok)
	}
}

// A GitLab token's check records whose it is, its scopes and its expiry,
// and refuses one without the api scope for that reason.
//
// @decision(DL-041)
func TestVerifyGitLabReportsTheTokensFacts(t *testing.T) {
	scopes := `["api"]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/user":
			_, _ = w.Write([]byte(`{"username":"spov-bot"}`))
		case "/api/v4/personal_access_tokens/self":
			_, _ = w.Write([]byte(`{"scopes":` + scopes + `,"expires_at":"2026-11-30"}`))
		case "/api/v4/projects":
			_, _ = w.Write([]byte(`[{"id":1,"path":"p","namespace":{"full_path":"g/s"},"http_url_to_repo":"https://x/g/s/p.git"}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	v := &credentialVerifier{env: envWith(nil), cfg: config.Config{Platforms: []config.Platform{
		{Name: "gl", Kind: config.KindGitLab, BaseURL: srv.URL},
	}}}
	host := strings.TrimPrefix(srv.URL, "http://")
	got, err := v.Verify(context.Background(), server.HostPlatform("gitlab", host), "glpat-token")
	if err != nil {
		t.Fatal(err)
	}
	if got.As != "spov-bot" || len(got.Scopes) != 1 || got.ExpiresAt.Format("2006-01-02") != "2026-11-30" {
		t.Errorf("verified = %+v", got)
	}
	scopes = `["read_repository"]`
	if _, err := v.Verify(context.Background(), server.HostPlatform("gitlab", host), "glpat-token"); err == nil ||
		!strings.Contains(err.Error(), "api scope") {
		t.Errorf("err = %v", err)
	}
}

// A name is read the same way for every command, and a hosted server's
// key is refused before anything is stored.
//
// @decision(DL-008)
func TestCredentialNamesMapToServerKeys(t *testing.T) {
	tests := []struct {
		name, key string
		ok        bool
	}{
		{"github", server.PlatformGitHub, true},
		{"GitHub", server.PlatformGitHub, true},
		{"forgejo:CodeFloe.com", "forgejo:codefloe.com", true},
		{"ANTHROPIC_API_KEY", "provider:ANTHROPIC_API_KEY", true},
		{"anthropic_api_key", "provider:ANTHROPIC_API_KEY", true},
		{"provider:OPENROUTER_API_KEY", "provider:OPENROUTER_API_KEY", true},
		{"SHELL", "", false},
		{"codefloe", "", false},
		{"forgejo:", "", false},
		{"gitea:Gitea.com", "gitea:gitea.com", true},
		{"gitlab:gitlab.com", "gitlab:gitlab.com", true},
	}
	for _, tt := range tests {
		key, prompt, err := parseCredentialName(tt.name).serverKey()
		if (err == nil) != tt.ok || key != tt.key {
			t.Errorf("%q: key %q, err %v", tt.name, key, err)
		}
		if tt.ok && prompt == "" {
			t.Errorf("%q: no prompt", tt.name)
		}
	}
}

// A credential goes where the server the CLI talks to reads it: a hosted
// server when one is configured by any of the three means, this machine
// otherwise.
//
// @decision(DL-008)
func TestTheConfiguredServerDecidesWhereACredentialGoes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		env    Env
		hosted bool
	}{
		{"nothing configured", Env{}, false},
		{"--server", Env{ServerURL: "https://spov.example"}, true},
		{"SPOV_SERVER", Env{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			vars := map[string]string{"HOME": dir, "XDG_CONFIG_HOME": filepath.Join(dir, "config")}
			if tc.name == "SPOV_SERVER" {
				vars[ServerEnvVar] = "https://spov.example"
			}
			env := tc.env
			env.Out, env.Err = &bytes.Buffer{}, &bytes.Buffer{}
			env.Environ, env.Workdir = config.MapEnv{Vars: vars}, dir
			got, err := env.hostedServer()
			if err != nil || got != tc.hosted {
				t.Errorf("hostedServer = %v, %v; want %v", got, err, tc.hosted)
			}
		})
	}
}

// A check's facts read as one line: the user, then scopes and expiry.
//
// @decision(DL-041)
func TestTokenFactsReadAsOneLine(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	got := tokenFacts(git.TokenInfo{User: "bot", Scopes: []string{"api"}, ExpiresAt: now.AddDate(0, 0, 12)}, now)
	if got != ", verified as bot (scopes api; expires 2026-11-13, in 12 days)" {
		t.Errorf("facts = %q", got)
	}
	if got := tokenFacts(git.TokenInfo{}, now); got != "" {
		t.Errorf("nothing known = %q", got)
	}
	if got := expiryPhrase(now.AddDate(0, 0, 1), now); got != "expires 2026-11-02, in 1 day" {
		t.Errorf("one day = %q", got)
	}
	if got := expiryPhrase(now.Add(time.Hour), now); got != "expires 2026-11-01, within a day" {
		t.Errorf("hours = %q", got)
	}
	if got := expiryPhrase(now.AddDate(0, 0, -1), now); got != "expired 2026-10-31" {
		t.Errorf("expired = %q", got)
	}
}
