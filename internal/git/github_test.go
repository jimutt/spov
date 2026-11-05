package git

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// testKeyOnce keeps RSA generation out of every test: 2048-bit key
// generation is slow enough to dominate the package's runtime otherwise.
var (
	testKeyOnce sync.Once
	testKey     *rsa.PrivateKey
	testKeyPEM  string
)

func githubTestKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	testKeyOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		testKey = key
		testKeyPEM = string(pem.EncodeToMemory(&pem.Block{
			Type:  "RSA PRIVATE KEY",
			Bytes: x509.MarshalPKCS1PrivateKey(key),
		}))
	})
	return testKey, testKeyPEM
}

// githubServer is a fake GitHub API recording what spov asked for.
type githubServer struct {
	*httptest.Server

	mu sync.Mutex
	// requests records method+path, in order.
	requests []string
	// auth records the Authorization header per path.
	auth map[string]string
	// mintBody records the decoded body of the last token mint.
	mintBody map[string]any
	// revoked counts DELETE /installation/token calls.
	revoked int

	// repos is served by /installation/repositories.
	repos []map[string]any
	// pulls is served by /repos/:owner/:name/pulls, pullHead is the
	// `head` value that must match for them to be returned, pullStatus
	// forces a failure, and pullQuery records the last query string.
	//
	// @decision(DL-050)
	pulls      []map[string]any
	pullHead   string
	pullStatus int
	pullQuery  string
	// pageSize, when non-zero, splits repos into pages of this size.
	pageSize int
	// installationID is returned by discovery.
	installationID int64
	// userInstallStatus lets a test make the user lookup fail so the org
	// fallback is exercised.
	userInstallStatus int
	// mintStatus and mintMessage force a mint failure.
	mintStatus  int
	mintMessage string
	// tokenValue is the minted token.
	tokenValue string
	// expiresAt is the mint response's expires_at.
	expiresAt string
	// installPerms is what GET /app/installations/:id grants; nil answers
	// 404.
	installPerms map[string]string
}

func newGitHubServer(t *testing.T) *githubServer {
	t.Helper()
	s := &githubServer{
		auth:           map[string]string{},
		installationID: 42,
		tokenValue:     "ghs_test_token",
		expiresAt:      time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Close)
	return s
}

func (s *githubServer) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, r.Method+" "+r.URL.Path)
	s.auth[r.URL.Path] = r.Header.Get("Authorization")
	s.mu.Unlock()

	switch {
	case strings.HasPrefix(r.URL.Path, "/users/") && strings.HasSuffix(r.URL.Path, "/installation"):
		if s.userInstallStatus != 0 {
			http.Error(w, `{"message":"Not Found"}`, s.userInstallStatus)
			return
		}
		writeJSON(w, map[string]any{"id": s.installationID})

	case strings.HasPrefix(r.URL.Path, "/orgs/") && strings.HasSuffix(r.URL.Path, "/installation"):
		writeJSON(w, map[string]any{"id": s.installationID})

	case strings.HasPrefix(r.URL.Path, "/app/installations/") && r.Method == http.MethodGet:
		if s.installPerms == nil {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]any{"id": s.installationID, "permissions": s.installPerms, "account": map[string]any{"login": "acme"}})

	case strings.HasSuffix(r.URL.Path, "/access_tokens"):
		if s.mintStatus != 0 {
			w.WriteHeader(s.mintStatus)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": s.mintMessage})
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.mintBody = body
		s.mu.Unlock()
		writeJSON(w, map[string]any{"token": s.tokenValue, "expires_at": s.expiresAt})

	case r.URL.Path == "/installation/repositories":
		s.serveRepos(w, r)

	// Gate 2's pull-request query. GitHub filters by head server-side,
	// so the test server does too.
	//
	// @decision(DL-050)
	case strings.HasSuffix(r.URL.Path, "/pulls"):
		s.mu.Lock()
		s.pullQuery = r.URL.RawQuery
		s.mu.Unlock()
		if s.pullStatus != 0 {
			http.Error(w, `{"message":"nope"}`, s.pullStatus)
			return
		}
		if s.pullHead != "" && r.URL.Query().Get("head") != s.pullHead {
			writeJSON(w, []any{})
			return
		}
		writeJSON(w, s.pulls)

	case r.URL.Path == "/installation/token" && r.Method == http.MethodDelete:
		s.mu.Lock()
		s.revoked++
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, `{"message":"unexpected path"}`, http.StatusNotFound)
	}
}

func (s *githubServer) lastPullQuery() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pullQuery
}

func (s *githubServer) serveRepos(w http.ResponseWriter, r *http.Request) {
	repos := s.repos
	if s.pageSize > 0 {
		page := 1
		fmt.Sscanf(r.URL.Query().Get("page"), "%d", &page)
		start := (page - 1) * s.pageSize
		end := min(start+s.pageSize, len(repos))
		if start >= len(repos) {
			repos = nil
		} else {
			repos = repos[start:end]
		}
	}
	writeJSON(w, map[string]any{"total_count": len(s.repos), "repositories": repos})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *githubServer) log() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func (s *githubServer) revokeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revoked
}

func (s *githubServer) mintRequest() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mintBody
}

func (s *githubServer) authFor(path string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.auth[path]
}

func newTestGitHub(t *testing.T, s *githubServer) *GitHub {
	t.Helper()
	_, keyPEM := githubTestKey(t)
	g, err := NewGitHub(GitHubConfig{
		Name:          "gh",
		Account:       "alice",
		AppID:         "Iv23liTEST",
		PrivateKeyPEM: keyPEM,
		APIURL:        s.URL,
		HTTP:          s.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestGitHubIdentity(t *testing.T) {
	g := newTestGitHub(t, newGitHubServer(t))
	if g.Name() != "gh" {
		t.Errorf("Name = %q", g.Name())
	}
	if g.Host() != "github.com" {
		t.Errorf("Host = %q", g.Host())
	}
	if g.Kind() != "github" {
		t.Errorf("Kind = %q", g.Kind())
	}
}

// TestGitHubTokenIsNarrowedToTheSelectedRepos checks that a minted token
// reaches only what the session bootstrapped, even when the App is
// installed on more.
//
// @decision(DL-041)
func TestGitHubTokenIsNarrowedToTheSelectedRepos(t *testing.T) {
	s := newGitHubServer(t)
	g := newTestGitHub(t, s)

	tok, err := g.IssueSandboxToken(context.Background(), []Repo{
		{Owner: "alice", Name: "spov"},
		{Owner: "alice", Name: "gadgets"},
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	body := s.mintRequest()
	raw, ok := body["repositories"].([]any)
	if !ok {
		t.Fatalf("the mint request did not narrow to repositories: %#v", body)
	}
	got := make([]string, 0, len(raw))
	for _, v := range raw {
		got = append(got, v.(string))
	}
	if strings.Join(got, ",") != "spov,gadgets" {
		t.Errorf("repositories = %v, want the selected names", got)
	}
	// GitHub takes bare names; sending owner/name silently matches
	// nothing.
	for _, name := range got {
		if strings.Contains(name, "/") {
			t.Errorf("repository %q is owner-qualified; GitHub expects a bare name", name)
		}
	}
	// repository_ids is deliberately not sent alongside repositories.
	if _, both := body["repository_ids"]; both {
		t.Error("the mint request sent both repositories and repository_ids")
	}

	if tok.Username != GitHubTokenUser {
		t.Errorf("username = %q, want %q", tok.Username, GitHubTokenUser)
	}
	if tok.Secret != "ghs_test_token" {
		t.Errorf("secret = %q", tok.Secret)
	}
	if tok.ExpiresAt.IsZero() {
		t.Error("the token has no expiry")
	}
}

// TestGitHubTokenIsRevocable: spov mints this token, so unlike Forgejo it
// can and does release it when the session ends.
//
// @decision(DL-041)
func TestGitHubTokenIsRevocable(t *testing.T) {
	s := newGitHubServer(t)
	g := newTestGitHub(t, s)

	tok, err := g.IssueSandboxToken(context.Background(), []Repo{{Owner: "o", Name: "r"}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Revoke == nil {
		t.Fatal("a minted token has no revoker")
	}
	if err := tok.Revoke(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.revokeCount() != 1 {
		t.Errorf("revoked %d times, want 1", s.revokeCount())
	}
	// Revocation authenticates with the token being revoked, not the JWT.
	if got := s.authFor("/installation/token"); got != "token ghs_test_token" {
		t.Errorf("revoke Authorization = %q, want the token itself", got)
	}
}

// TestGitHubMintUsesTheJWT: minting authenticates as the App, not as an
// installation.
//
// @decision(DL-041)
func TestGitHubMintUsesTheJWT(t *testing.T) {
	s := newGitHubServer(t)
	g := newTestGitHub(t, s)

	if _, err := g.IssueSandboxToken(context.Background(), []Repo{{Owner: "o", Name: "r"}}, time.Hour); err != nil {
		t.Fatal(err)
	}
	auth := s.authFor("/app/installations/42/access_tokens")
	if !strings.HasPrefix(auth, "Bearer ") {
		t.Fatalf("mint Authorization = %q, want a Bearer JWT", auth)
	}
	jwt := strings.TrimPrefix(auth, "Bearer ")
	if parts := strings.Split(jwt, "."); len(parts) != 3 {
		t.Errorf("the JWT has %d parts, want 3", len(parts))
	}
}

func TestGitHubIssueRejectsNoRepos(t *testing.T) {
	g := newTestGitHub(t, newGitHubServer(t))
	if _, err := g.IssueSandboxToken(context.Background(), nil, time.Hour); err == nil {
		t.Fatal("expected an error with no repositories selected")
	}
}

// TestGitHubListRepos covers the response shape: GitHub nests the list in
// an object rather than returning a bare array.
//
// @decision(DL-041)
func TestGitHubListRepos(t *testing.T) {
	s := newGitHubServer(t)
	older := time.Now().Add(-48 * time.Hour)
	newer := time.Now().Add(-time.Hour)
	s.repos = []map[string]any{
		{
			"name": "old", "private": false, "archived": true,
			"clone_url": "https://github.com/alice/old.git",
			"owner":     map[string]any{"login": "alice"},
			"pushed_at": older.Format(time.RFC3339),
		},
		{
			"name": "new", "private": true,
			"clone_url": "https://github.com/alice/new.git",
			"owner":     map[string]any{"login": "alice"},
			"pushed_at": newer.Format(time.RFC3339),
		},
	}
	g := newTestGitHub(t, s)

	repos, err := g.ListRepos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 {
		t.Fatalf("got %d repos, want 2", len(repos))
	}
	// Most recent first, so the picker shows current work at the top.
	if repos[0].Name != "new" {
		t.Errorf("first repo = %q, want the most recently pushed", repos[0].Name)
	}
	if !repos[0].Private {
		t.Error("private flag was lost")
	}
	if !repos[1].Archived {
		t.Error("archived flag was lost")
	}
	if repos[0].Owner != "alice" {
		t.Errorf("owner = %q, want it read from owner.login", repos[0].Owner)
	}
	if repos[0].CloneURL != "https://github.com/alice/new.git" {
		t.Errorf("clone URL = %q", repos[0].CloneURL)
	}
}

// TestGitHubListReposRevokesItsToken: listing needs a token covering the
// whole installation, which is the broadest credential spov holds. It
// must not be left live for its full hour.
//
// @decision(DL-041)
func TestGitHubListReposRevokesItsToken(t *testing.T) {
	s := newGitHubServer(t)
	s.repos = []map[string]any{{
		"name": "r", "clone_url": "https://github.com/o/r.git",
		"owner": map[string]any{"login": "o"},
	}}
	g := newTestGitHub(t, s)

	if _, err := g.ListRepos(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.revokeCount() != 1 {
		t.Errorf("the listing token was revoked %d times, want 1", s.revokeCount())
	}

	// The listing token must not be narrowed: it is how spov discovers
	// which repositories exist.
	if body := s.mintRequest(); body != nil {
		if _, narrowed := body["repositories"]; narrowed {
			t.Error("the listing token was narrowed, so it cannot discover repositories")
		}
	}
}

func TestGitHubListReposPaginates(t *testing.T) {
	s := newGitHubServer(t)
	s.pageSize = githubPageSize
	for i := range githubPageSize + 5 {
		s.repos = append(s.repos, map[string]any{
			"name": fmt.Sprintf("r%d", i), "clone_url": fmt.Sprintf("https://github.com/o/r%d.git", i),
			"owner": map[string]any{"login": "o"},
		})
	}
	g := newTestGitHub(t, s)

	repos, err := g.ListRepos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != githubPageSize+5 {
		t.Errorf("got %d repos, want %d; pagination stopped early", len(repos), githubPageSize+5)
	}
}

// TestGitHubListReposStopsExactlyOnAFullPage checks that with a repo
// count that is an exact multiple of the page size, the loop does not
// spend a request discovering an empty page.
//
// @decision(DL-041)
func TestGitHubListReposStopsExactlyOnAFullPage(t *testing.T) {
	s := newGitHubServer(t)
	s.pageSize = githubPageSize
	for i := range githubPageSize {
		s.repos = append(s.repos, map[string]any{
			"name": fmt.Sprintf("r%d", i), "clone_url": fmt.Sprintf("https://github.com/o/r%d.git", i),
			"owner": map[string]any{"login": "o"},
		})
	}
	g := newTestGitHub(t, s)

	repos, err := g.ListRepos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != githubPageSize {
		t.Fatalf("got %d repos, want %d", len(repos), githubPageSize)
	}

	var pages int
	for _, r := range s.log() {
		if strings.HasPrefix(r, "GET /installation/repositories") {
			pages++
		}
	}
	if pages != 1 {
		t.Errorf("listing %d repositories took %d requests, want 1", githubPageSize, pages)
	}
}

func TestGitHubListReposEmpty(t *testing.T) {
	s := newGitHubServer(t)
	g := newTestGitHub(t, s)

	_, err := g.ListRepos(context.Background())
	if !errors.Is(err, ErrNoRepos) {
		t.Fatalf("err = %v, want ErrNoRepos", err)
	}
}

// TestGitHubDiscoversInstallation: spov is not told whether the account
// is a user or an organisation, so it tries both.
//
// @decision(DL-041)
func TestGitHubDiscoversInstallation(t *testing.T) {
	t.Run("user", func(t *testing.T) {
		s := newGitHubServer(t)
		g := newTestGitHub(t, s)
		if _, err := g.IssueSandboxToken(context.Background(), []Repo{{Owner: "o", Name: "r"}}, time.Hour); err != nil {
			t.Fatal(err)
		}
		if !containsRequest(s.log(), "GET /users/alice/installation") {
			t.Errorf("the user endpoint was not tried: %v", s.log())
		}
	})

	t.Run("org fallback", func(t *testing.T) {
		s := newGitHubServer(t)
		s.userInstallStatus = http.StatusNotFound
		g := newTestGitHub(t, s)
		if _, err := g.IssueSandboxToken(context.Background(), []Repo{{Owner: "o", Name: "r"}}, time.Hour); err != nil {
			t.Fatal(err)
		}
		if !containsRequest(s.log(), "GET /orgs/alice/installation") {
			t.Errorf("the org endpoint was not tried after the user lookup failed: %v", s.log())
		}
	})
}

// TestGitHubCachesTheInstallation: discovery is two extra calls, and the
// answer does not change during a run.
func TestGitHubCachesTheInstallation(t *testing.T) {
	s := newGitHubServer(t)
	g := newTestGitHub(t, s)

	for range 3 {
		if _, err := g.IssueSandboxToken(context.Background(), []Repo{{Owner: "o", Name: "r"}}, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	var lookups int
	for _, r := range s.log() {
		if strings.Contains(r, "/installation") && strings.HasPrefix(r, "GET /users") {
			lookups++
		}
	}
	if lookups != 1 {
		t.Errorf("the installation was looked up %d times, want 1", lookups)
	}
}

// TestGitHubConfiguredInstallationSkipsDiscovery avoids the lookup
// entirely when the id is stored.
func TestGitHubConfiguredInstallationSkipsDiscovery(t *testing.T) {
	s := newGitHubServer(t)
	_, keyPEM := githubTestKey(t)
	g, err := NewGitHub(GitHubConfig{
		Name: "gh", Account: "alice", AppID: "app", PrivateKeyPEM: keyPEM,
		APIURL: s.URL, HTTP: s.Client(), InstallationID: 99,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.IssueSandboxToken(context.Background(), []Repo{{Owner: "o", Name: "r"}}, time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, r := range s.log() {
		if strings.HasSuffix(r, "/installation") {
			t.Errorf("discovery ran despite a configured installation id: %v", s.log())
		}
	}
	if !containsRequest(s.log(), "POST /app/installations/99/access_tokens") {
		t.Errorf("the configured installation id was not used: %v", s.log())
	}
}

func TestGitHubInstallationNotFound(t *testing.T) {
	s := newGitHubServer(t)
	s.userInstallStatus = http.StatusNotFound
	s.installationID = 0
	g := newTestGitHub(t, s)

	// Both lookups answer with no id.
	_, err := g.IssueSandboxToken(context.Background(), []Repo{{Owner: "o", Name: "r"}}, time.Hour)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "install") {
		t.Errorf("err = %v; it should say the App is not installed", err)
	}
}

// TestGitHubErrorsCarryGitHubsMessage: "404 Not Found" alone does not
// tell a user what to fix.
func TestGitHubErrorsCarryGitHubsMessage(t *testing.T) {
	s := newGitHubServer(t)
	s.mintStatus = http.StatusUnprocessableEntity
	s.mintMessage = "There is at least one repository that does not exist or is not accessible"
	g := newTestGitHub(t, s)

	_, err := g.IssueSandboxToken(context.Background(), []Repo{{Owner: "o", Name: "missing"}}, time.Hour)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "not accessible") {
		t.Errorf("err = %v; it should carry GitHub's own message", err)
	}
}

// TestGitHubErrorsNeverCarryTheKeyOrToken: errors reach logs.
//
// @decision(DL-041)
func TestGitHubErrorsNeverCarryTheKeyOrToken(t *testing.T) {
	s := newGitHubServer(t)
	s.mintStatus = http.StatusForbidden
	s.mintMessage = "forbidden"
	_, keyPEM := githubTestKey(t)
	g := newTestGitHub(t, s)

	_, err := g.IssueSandboxToken(context.Background(), []Repo{{Owner: "o", Name: "r"}}, time.Hour)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "BEGIN RSA") || strings.Contains(err.Error(), keyPEM[:40]) {
		t.Errorf("the error carries the private key: %v", err)
	}
	if strings.Contains(err.Error(), "ghs_") {
		t.Errorf("the error carries a token: %v", err)
	}
}

// TestGitHubMissingExpiryFallsBackToTheDocumentedLifetime: a token that
// works but whose expiry cannot be parsed is still usable.
func TestGitHubMissingExpiryFallsBackToTheDocumentedLifetime(t *testing.T) {
	s := newGitHubServer(t)
	s.expiresAt = "not-a-timestamp"

	// A fixed clock, so the fallback is asserted exactly rather than
	// within a window that time.Now would satisfy anyway.
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	_, keyPEM := githubTestKey(t)
	g, err := NewGitHub(GitHubConfig{
		Name: "gh", Account: "alice", AppID: "app", PrivateKeyPEM: keyPEM,
		APIURL: s.URL, HTTP: s.Client(),
		Now: func() time.Time { return fixed },
	})
	if err != nil {
		t.Fatal(err)
	}

	tok, err := g.IssueSandboxToken(context.Background(), []Repo{{Owner: "o", Name: "r"}}, time.Hour)
	if err != nil {
		t.Fatalf("an unparseable expiry failed the mint: %v", err)
	}
	if want := fixed.Add(GitHubTokenLifetime); !tok.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v (the documented lifetime from the clock)", tok.ExpiresAt, want)
	}
}

func TestGitHubEmptyTokenIsRejected(t *testing.T) {
	s := newGitHubServer(t)
	s.tokenValue = ""
	g := newTestGitHub(t, s)

	if _, err := g.IssueSandboxToken(context.Background(), []Repo{{Owner: "o", Name: "r"}}, time.Hour); err == nil {
		t.Fatal("an empty token was accepted")
	}
}

// TestGitHubTokenScopeIsVerified: unlike Forgejo's, this describes what
// spov itself requested.
//
// @decision(DL-041)
func TestGitHubTokenScopeIsVerified(t *testing.T) {
	g := newTestGitHub(t, newGitHubServer(t))
	scope := g.TokenScope()
	if !scope.PerRepo {
		t.Error("a minted GitHub token is per-repo; TokenScope says otherwise")
	}
	if scope.Description == "" {
		t.Error("no description")
	}
}

func TestNewGitHubValidation(t *testing.T) {
	_, keyPEM := githubTestKey(t)
	valid := GitHubConfig{Name: "gh", Account: "o", AppID: "a", PrivateKeyPEM: keyPEM}

	tests := []struct {
		name   string
		mutate func(*GitHubConfig)
		want   string
	}{
		{"no name", func(c *GitHubConfig) { c.Name = "" }, "name"},
		{"no app id", func(c *GitHubConfig) { c.AppID = "" }, "app id"},
		{"no key", func(c *GitHubConfig) { c.PrivateKeyPEM = "" }, "private key"},
		{"no account or installation", func(c *GitHubConfig) { c.Account = "" }, "account"},
		{"bad key", func(c *GitHubConfig) { c.PrivateKeyPEM = "not a pem" }, "PEM"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid
			tt.mutate(&cfg)
			_, err := NewGitHub(cfg)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to mention %q", err, tt.want)
			}
		})
	}

	// An installation id stands in for an account.
	cfg := valid
	cfg.Account = ""
	cfg.InstallationID = 7
	if _, err := NewGitHub(cfg); err != nil {
		t.Errorf("an installation id should remove the need for an account: %v", err)
	}
}

// TestNewGitHubKeyIsNotLoggedOnFailure checks a malformed key is never
// echoed back, in case it is a real key with a stray character.
//
// @decision(DL-041)
func TestNewGitHubKeyIsNotLoggedOnFailure(t *testing.T) {
	_, keyPEM := githubTestKey(t)
	// The base64 body, with the armour and newlines stripped: this is
	// the part that must never be echoed.
	body := strings.NewReplacer(
		"-----BEGIN RSA PRIVATE KEY-----", "",
		"-----END RSA PRIVATE KEY-----", "",
		"\n", "",
	).Replace(keyPEM)
	if len(body) < 100 {
		t.Fatalf("the test key looks wrong: %d bytes of body", len(body))
	}

	for _, tt := range []struct {
		name string
		key  string
	}{
		{"not PEM at all", "this is not a key"},
		{"PEM armour, corrupt body", "-----BEGIN RSA PRIVATE KEY-----\n" + body[:64] + "\n-----END RSA PRIVATE KEY-----\n"},
		{"truncated", keyPEM[:len(keyPEM)/2]},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewGitHub(GitHubConfig{
				Name: "gh", Account: "o", AppID: "a", PrivateKeyPEM: tt.key,
			})
			if err == nil {
				t.Fatal("a malformed key was accepted")
			}
			// Any run of the real key's base64 body in the error is a leak.
			for i := 0; i+32 <= len(body); i += 32 {
				if strings.Contains(err.Error(), body[i:i+32]) {
					t.Fatalf("the error carries key material: %v", err)
				}
			}
		})
	}
}

func TestGitHubAPIURL(t *testing.T) {
	tests := []struct {
		name, apiURL, baseURL, want string
	}{
		{"default", "", "", defaultGitHubAPI},
		{"github.com", "", "https://github.com", defaultGitHubAPI},
		{"github.com trailing slash", "", "https://github.com/", defaultGitHubAPI},
		{"enterprise", "", "https://git.acme.com", "https://git.acme.com/api/v3"},
		{"explicit", "https://api.acme.com/", "https://git.acme.com", "https://api.acme.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := githubAPIURL(tt.apiURL, tt.baseURL)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}

	if _, err := githubAPIURL("not-a-url", ""); err == nil {
		t.Error("a malformed api URL was accepted")
	}
}

// TestGitHubCancellation: a cancelled context must stop the work rather
// than continuing to mint against GitHub.
func TestGitHubCancellation(t *testing.T) {
	s := newGitHubServer(t)
	g := newTestGitHub(t, s)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := g.IssueSandboxToken(ctx, []Repo{{Owner: "o", Name: "r"}}, time.Hour); err == nil {
		t.Error("a cancelled context still minted a token")
	}
	if _, err := g.ListRepos(ctx); err == nil {
		t.Error("a cancelled context still listed repositories")
	}
}

func containsRequest(log []string, want string) bool {
	for _, r := range log {
		if r == want {
			return true
		}
	}
	return false
}

// TestBootstrapRevokesGitHubToken checks the whole path: mint during
// Bootstrap, release on teardown, and only once however many times the
// caller asks.
//
// @decision(DL-041)
func TestBootstrapRevokesGitHubToken(t *testing.T) {
	s := newGitHubServer(t)
	s.repos = nil
	g := newTestGitHub(t, s)

	_, revoke, err := Bootstrap(context.Background(), []Selection{{
		Platform: g,
		Repos:    []Repo{{Owner: "alice", Name: "spov", CloneURL: "https://github.com/alice/spov.git"}},
	}}, 4*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if s.revokeCount() != 0 {
		t.Fatalf("revoked before teardown: %d", s.revokeCount())
	}
	if err := revoke(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.revokeCount() != 1 {
		t.Errorf("Bootstrap did not revoke the GitHub token: %d", s.revokeCount())
	}
	// Bootstrap's revoker runs at most once.
	if err := revoke(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.revokeCount() != 1 {
		t.Errorf("double revocation: %d", s.revokeCount())
	}
}

// TestGitHubDiscoveryFallsBackOnAnEmptyBody checks that a 2xx carrying no
// id is treated as a miss and falls through to the org lookup, rather
// than being read as an answer.
//
// @decision(DL-041)
func TestGitHubDiscoveryFallsBackOnAnEmptyBody(t *testing.T) {
	var mu sync.Mutex
	var paths []string

	s := newGitHubServer(t)
	s.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/users/"):
			// 200, but no id: an unexpected shape rather than a 404.
			writeJSON(w, map[string]any{})
		case strings.HasPrefix(r.URL.Path, "/orgs/"):
			writeJSON(w, map[string]any{"id": 555})
		default:
			writeJSON(w, map[string]any{
				"token":      "ghs_test_token",
				"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			})
		}
	})
	g := newTestGitHub(t, s)

	if _, err := g.IssueSandboxToken(context.Background(), []Repo{{Owner: "o", Name: "r"}}, time.Hour); err != nil {
		t.Fatalf("the org fallback did not run after an idless 200: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !containsRequest(paths, "/app/installations/555/access_tokens") {
		t.Errorf("the org installation was not used: %v", paths)
	}
}

// TestGitHubDiscoveryReportsNoInstallation covers both endpoints coming
// back without one.
func TestGitHubDiscoveryReportsNoInstallation(t *testing.T) {
	s := newGitHubServer(t)
	s.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{})
	})
	g := newTestGitHub(t, s)

	_, err := g.IssueSandboxToken(context.Background(), []Repo{{Owner: "o", Name: "r"}}, time.Hour)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "install") {
		t.Errorf("err = %v; it should say the App is not installed", err)
	}
}

// TestGitHubConcurrentMintsDiscoverOnce: releasing the lock between the
// cache read and the lookup let every concurrent caller run the same two
// requests against GitHub's rate limit.
//
// @decision(DL-041)
func TestGitHubConcurrentMintsDiscoverOnce(t *testing.T) {
	s := newGitHubServer(t)
	g := newTestGitHub(t, s)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := g.IssueSandboxToken(context.Background(), []Repo{{Owner: "o", Name: "r"}}, time.Hour); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	var lookups int
	for _, r := range s.log() {
		if strings.HasPrefix(r, "GET /users/") || strings.HasPrefix(r, "GET /orgs/") {
			lookups++
		}
	}
	if lookups != 1 {
		t.Errorf("%d concurrent mints ran discovery %d times, want 1", 8, lookups)
	}
}

// Gate 2's query on a GitHub App platform.
//
// @decision(DL-050)

func TestGitHubFindsThePullRequestForABranch(t *testing.T) {
	s := newGitHubServer(t)
	s.pullHead = "alice:impl/widgets"
	s.pulls = []map[string]any{{
		"number": 74, "html_url": "https://github.com/alice/spov/pull/74",
		"title": "a title", "state": "open",
	}}
	got, err := newTestGitHub(t, s).FindPullRequest(
		context.Background(), RepoRef{Owner: "alice", Name: "spov"}, "impl/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Number != 74 {
		t.Fatalf("got %+v, want pull request 74", got)
	}
	if got.URL != "https://github.com/alice/spov/pull/74" {
		t.Errorf("URL = %q, want the browser URL", got.URL)
	}
	// The head filter is what makes this evidence about THIS job rather
	// than about the repository.
	q := s.lastPullQuery()
	// The colon is legal unescaped in a query value, so it survives
	// QueryEscape; the slash in the branch does not.
	if !strings.Contains(q, "head=alice:impl%2Fwidgets") {
		t.Errorf("query = %q, want it scoped to the branch", q)
	}
	// Every state: a merged pull request is verified too (DL-051).
	if !strings.Contains(q, "state=all") {
		t.Errorf("query = %q, want pull requests in every state", q)
	}
	if !got.Open || got.Merged {
		t.Errorf("an open pull request reads open %v, merged %v", got.Open, got.Merged)
	}
	// The credential is minted for this one repository and revoked on
	// the way out, exactly as the listing's is.
	if s.revokeCount() != 1 {
		t.Errorf("revoked %d tokens, want 1", s.revokeCount())
	}
}

func TestGitHubReportsNoPullRequestForAnUnmatchedBranch(t *testing.T) {
	s := newGitHubServer(t)
	s.pullHead = "alice:some-other-branch"
	s.pulls = []map[string]any{{"number": 11, "html_url": "https://example.com/11"}}
	got, err := newTestGitHub(t, s).FindPullRequest(
		context.Background(), RepoRef{Owner: "alice", Name: "spov"}, "impl/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("reported another branch's pull request as this job's: %+v", got)
	}
}

func TestGitHubWithNoBranchAsksNothing(t *testing.T) {
	s := newGitHubServer(t)
	s.pulls = []map[string]any{{"number": 11, "html_url": "https://example.com/11"}}
	got, err := newTestGitHub(t, s).FindPullRequest(
		context.Background(), RepoRef{Owner: "alice", Name: "spov"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("answered a question it was not asked: %+v", got)
	}
	if s.lastPullQuery() != "" {
		t.Errorf("made a request with no branch to match: %q", s.lastPullQuery())
	}
	// And it does not mint a credential to ask nothing.
	if s.revokeCount() != 0 {
		t.Errorf("minted a token for a query it did not make")
	}
}

func TestGitHubAPullRequestQueryThatFailsIsAnError(t *testing.T) {
	s := newGitHubServer(t)
	s.pullStatus = http.StatusInternalServerError
	_, err := newTestGitHub(t, s).FindPullRequest(
		context.Background(), RepoRef{Owner: "alice", Name: "spov"}, "impl/widgets")
	if err == nil {
		t.Fatal("a failing query answered as though it found nothing")
	}
	// The caller reads an error as UNVERIFIED, which is why it must be
	// an error rather than a silent nil: "spov could not check" and
	// "the job did not deliver" are different findings.
	if !strings.Contains(err.Error(), "find pull request") {
		t.Errorf("err = %v, want it to say what failed", err)
	}
}

// A sandbox token asks for the narrowed permissions, capped at what the
// installation holds; a server-side token asks for none.
//
// @decision(DL-034)
func TestASandboxTokenAsksForFewerPermissions(t *testing.T) {
	s := newGitHubServer(t)
	_, keyPEM := githubTestKey(t)
	g, err := NewGitHub(GitHubConfig{
		Name: "gh", Account: "alice", AppID: "app", PrivateKeyPEM: keyPEM,
		APIURL: s.URL, HTTP: s.Client(), InstallationID: 42,
	})
	if err != nil {
		t.Fatal(err)
	}
	mint := func() map[string]any {
		t.Helper()
		if _, err := g.IssueSandboxToken(context.Background(), []Repo{{Owner: "o", Name: "r"}}, time.Hour); err != nil {
			t.Fatal(err)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		perms, _ := s.mintBody["permissions"].(map[string]any)
		return perms
	}

	// Unreadable installation: the narrowed set as it is.
	if got := mint(); len(got) != len(SandboxPermissions) || got["contents"] != "write" || got["administration"] != nil {
		t.Errorf("permissions %v", got)
	}
	// An App with more than spov asks for, and one with less.
	s.installPerms = map[string]string{"contents": "read", "metadata": "read", "administration": "write", "pull_requests": "write"}
	got := mint()
	want := map[string]any{"contents": "read", "metadata": "read", "pull_requests": "write"}
	if len(got) != len(want) {
		t.Errorf("permissions %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("permissions %v, want %v", got, want)
		}
	}

	// A token for every repository names none, and is narrowed all the
	// same.
	//
	// @decision(DL-043)
	if _, err := g.IssueWideToken(context.Background(), time.Hour); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	if _, named := s.mintBody["repositories"]; named || s.mintBody["permissions"] == nil {
		t.Errorf("a wide token asked for %v", s.mintBody)
	}
	s.mu.Unlock()

	if _, err := g.FindPullRequest(context.Background(), RepoRef{Owner: "o", Name: "r"}, "b"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, narrowed := s.mintBody["permissions"]; narrowed {
		t.Error("the server's own token was narrowed")
	}
}

// A branch whose pull request was merged is found merged; one only
// closed is found closed, neither open nor merged.
//
// @decision(DL-051)
func TestGitHubFindsAMergedPullRequest(t *testing.T) {
	s := newGitHubServer(t)
	s.pullHead = "alice:spov/x"
	s.pulls = []map[string]any{
		{"number": 5, "html_url": "https://github.com/alice/spov/pull/5", "state": "closed", "merged_at": nil},
		{"number": 4, "html_url": "https://github.com/alice/spov/pull/4", "state": "closed", "merged_at": "2026-11-04T07:00:00Z"},
	}
	got, err := newTestGitHub(t, s).FindPullRequest(context.Background(), RepoRef{Owner: "alice", Name: "spov"}, "spov/x")
	if err != nil || got == nil || got.Number != 4 || !got.Merged || got.Open {
		t.Fatalf("found %+v, %v; want #4 merged", got, err)
	}
	s.pulls = s.pulls[:1]
	got, err = newTestGitHub(t, s).FindPullRequest(context.Background(), RepoRef{Owner: "alice", Name: "spov"}, "spov/x")
	if err != nil || got == nil || got.Number != 5 || got.Merged || got.Open {
		t.Fatalf("found %+v, %v; want #5 closed", got, err)
	}
}
