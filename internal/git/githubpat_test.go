package git

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLooksLikeGitHubPAT(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"github_pat_11ABCDEF_secret", true},
		{"ghp_classicpersonaltoken", true},
		{"ghs_installationtoken", true},
		{`{"app_id":"1","private_key":"x"}`, false},
		{"  {\"app_id\":\"1\"}", false},
		{"", false},
		{"some-forgejo-token", false},
	} {
		if got := LooksLikeGitHubPAT(tc.in); got != tc.want {
			t.Errorf("LooksLikeGitHubPAT(%.20q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestNewGitHubPATValidates(t *testing.T) {
	if _, err := NewGitHubPAT(GitHubPATConfig{Token: "github_pat_x"}); err == nil {
		t.Error("a platform with no name was accepted")
	}
	if _, err := NewGitHubPAT(GitHubPATConfig{Name: "gh"}); err == nil {
		t.Error("a platform with no token was accepted")
	}
}

// The token authenticates as a header and must never reach a URL, where
// it would land in a proxy log.
func TestGitHubPATSendsTheTokenAsAHeader(t *testing.T) {
	var gotAuth, gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotURL = r.URL.String()
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	p, err := NewGitHubPAT(GitHubPATConfig{Name: "gh", APIURL: srv.URL, Token: "github_pat_secret"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = p.ListRepos(context.Background())
	if gotAuth != "Bearer github_pat_secret" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if strings.Contains(gotURL, "github_pat_secret") {
		t.Errorf("the token reached the URL: %q", gotURL)
	}
}

func TestGitHubPATListRepos(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/user/repos") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`[
			{"name":"old","private":false,"owner":{"login":"acme"},"clone_url":"https://x/acme/old.git","pushed_at":"2026-01-01T00:00:00Z"},
			{"name":"new","private":true,"owner":{"login":"acme"},"clone_url":"https://x/acme/new.git","pushed_at":"2026-08-01T00:00:00Z"}
		]`))
	}))
	defer srv.Close()

	p, err := NewGitHubPAT(GitHubPATConfig{Name: "gh", APIURL: srv.URL, Token: "github_pat_secret"})
	if err != nil {
		t.Fatal(err)
	}
	repos, err := p.ListRepos(context.Background())
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("got %d repos", len(repos))
	}
	// Most recently pushed first, as the picker expects.
	if repos[0].Name != "new" {
		t.Errorf("repos[0] = %q, want the most recent", repos[0].Name)
	}
}

func TestGitHubPATListReposReportsAnEmptyAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	p, _ := NewGitHubPAT(GitHubPATConfig{Name: "gh", APIURL: srv.URL, Token: "github_pat_x"})
	_, err := p.ListRepos(context.Background())
	if err == nil || !strings.Contains(err.Error(), ErrNoRepos.Error()) {
		t.Fatalf("err = %v, want ErrNoRepos", err)
	}
}

// The token is the user's own: spov hands it over unchanged, and has
// nothing to revoke.
func TestGitHubPATIssueSandboxToken(t *testing.T) {
	p, err := NewGitHubPAT(GitHubPATConfig{Name: "gh", Token: "github_pat_secret"})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := p.IssueSandboxToken(context.Background(),
		[]Repo{{Owner: "acme", Name: "app"}}, time.Hour)
	if err != nil {
		t.Fatalf("IssueSandboxToken: %v", err)
	}
	if tok.Secret != "github_pat_secret" {
		t.Errorf("Secret = %q", tok.Secret)
	}
	if tok.Username != GitHubPATUser {
		t.Errorf("Username = %q, want %q", tok.Username, GitHubPATUser)
	}
	if tok.Revoke != nil {
		t.Error("a PAT platform claims it can revoke the user's own token")
	}
	if !tok.ExpiresAt.IsZero() {
		t.Error("a PAT platform claims an expiry it does not know")
	}
}

func TestGitHubPATIssueRefusesNoRepos(t *testing.T) {
	p, _ := NewGitHubPAT(GitHubPATConfig{Name: "gh", Token: "github_pat_x"})
	if _, err := p.IssueSandboxToken(context.Background(), nil, time.Hour); err == nil {
		t.Fatal("a token was issued for no repositories")
	}
}

// TokenScope must not claim the App's per-repo guarantee: spov cannot
// narrow a token it did not mint.
func TestGitHubPATScopeIsHonest(t *testing.T) {
	p, _ := NewGitHubPAT(GitHubPATConfig{Name: "gh", Token: "github_pat_x"})
	scope := p.TokenScope()
	if scope.PerRepo {
		t.Error("TokenScope claims per-repo narrowing")
	}
	if !strings.Contains(scope.Description, "cannot narrow") {
		t.Errorf("Description = %q; it should say what spov cannot do", scope.Description)
	}
}

func TestGitHubPATLogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"login":"alice"}`))
	}))
	defer srv.Close()
	p, _ := NewGitHubPAT(GitHubPATConfig{Name: "gh", APIURL: srv.URL, Token: "github_pat_x"})
	login, err := p.Login(context.Background())
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if login != "alice" {
		t.Errorf("Login = %q", login)
	}
}

// A fine-grained token without the user grant answers 200 with no login.
// That is not a failure: the token still works for repositories.
func TestGitHubPATLoginToleratesNoGrant(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	p, _ := NewGitHubPAT(GitHubPATConfig{Name: "gh", APIURL: srv.URL, Token: "github_pat_x"})
	login, err := p.Login(context.Background())
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if login != "" {
		t.Errorf("Login = %q, want empty", login)
	}
}

// An error from GitHub must not carry the token.
func TestGitHubPATErrorsDoNotLeakTheToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer srv.Close()
	const token = "github_pat_a_very_distinctive_secret"
	p, _ := NewGitHubPAT(GitHubPATConfig{Name: "gh", APIURL: srv.URL, Token: token})
	_, err := p.ListRepos(context.Background())
	if err == nil {
		t.Fatal("ListRepos succeeded against a 401")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("the error carries the token: %v", err)
	}
}

// Pagination: a full page is followed by another request.
func TestGitHubPATListReposPaginates(t *testing.T) {
	pages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		pages++
		if pages == 1 {
			var b strings.Builder
			b.WriteString("[")
			for i := range githubPageSize {
				if i > 0 {
					b.WriteString(",")
				}
				fmt.Fprintf(&b, `{"name":"r%d","owner":{"login":"acme"},"clone_url":"https://x/acme/r%d.git"}`, i, i)
			}
			b.WriteString("]")
			_, _ = w.Write([]byte(b.String()))
			return
		}
		_, _ = w.Write([]byte(`[{"name":"last","owner":{"login":"acme"},"clone_url":"https://x/acme/last.git"}]`))
	}))
	defer srv.Close()

	p, _ := NewGitHubPAT(GitHubPATConfig{Name: "gh", APIURL: srv.URL, Token: "github_pat_x"})
	repos, err := p.ListRepos(context.Background())
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(repos) != githubPageSize+1 {
		t.Errorf("got %d repos over %d pages", len(repos), pages)
	}
}

// A cancelled context stops before anything is issued.
func TestGitHubPATRespectsCancellation(t *testing.T) {
	p, _ := NewGitHubPAT(GitHubPATConfig{Name: "gh", Token: "github_pat_x"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.ListRepos(ctx); err == nil {
		t.Error("ListRepos ignored a cancelled context")
	}
	if _, err := p.IssueSandboxToken(ctx, []Repo{{Owner: "a", Name: "b"}}, time.Hour); err == nil {
		t.Error("IssueSandboxToken ignored a cancelled context")
	}
}

// A response that is not JSON is an error, not a silent empty listing.
func TestGitHubPATRejectsAGarbageResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>not json</html>`))
	}))
	defer srv.Close()
	p, _ := NewGitHubPAT(GitHubPATConfig{Name: "gh", APIURL: srv.URL, Token: "github_pat_x"})
	if _, err := p.ListRepos(context.Background()); err == nil {
		t.Fatal("a non-JSON response was accepted")
	}
}

// A transport failure reaches the caller rather than being swallowed.
func TestGitHubPATReportsATransportFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close() // nothing is listening now
	p, _ := NewGitHubPAT(GitHubPATConfig{Name: "gh", APIURL: srv.URL, Token: "github_pat_x"})
	if _, err := p.Login(context.Background()); err == nil {
		t.Fatal("a dead server was accepted")
	}
}

func TestGitHubPATAccessors(t *testing.T) {
	p, err := NewGitHubPAT(GitHubPATConfig{Name: "gh", Token: "github_pat_x"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "gh" || p.Host() != "github.com" || p.Kind() != "github" {
		t.Errorf("accessors = %q %q %q", p.Name(), p.Host(), p.Kind())
	}
	// An enterprise base URL decides both the host and the API root.
	ent, err := NewGitHubPAT(GitHubPATConfig{
		Name: "ent", BaseURL: "https://github.example.com", Token: "github_pat_x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if ent.Host() != "github.example.com" {
		t.Errorf("Host = %q", ent.Host())
	}
}

func TestNewGitHubPATRejectsABadBaseURL(t *testing.T) {
	if _, err := NewGitHubPAT(GitHubPATConfig{
		Name: "gh", BaseURL: "ftp://nope", Token: "github_pat_x",
	}); err == nil {
		t.Error("a non-http base URL was accepted")
	}
}

var _ Platform = (*GitHubPAT)(nil)

// TestGitHubPATFindsAPullRequest checks gate 2 on a token-backed
// platform: it shares GitHub App's query, differing only in the
// credential, so this covers the token being passed through correctly.
//
// @decision(DL-050)
func TestGitHubPATFindsAPullRequest(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.String()
		_, _ = w.Write([]byte(
			`[{"number":74,"html_url":"https://github.com/alice/spov/pull/74","title":"t","state":"open"}]`))
	}))
	defer srv.Close()

	p, err := NewGitHubPAT(GitHubPATConfig{Name: "gh", APIURL: srv.URL, Token: "github_pat_secret"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.FindPullRequest(context.Background(), RepoRef{Owner: "alice", Name: "spov"}, "impl/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Number != 74 {
		t.Fatalf("got %+v, want pull request 74", got)
	}
	if gotAuth != "Bearer github_pat_secret" {
		t.Errorf("Authorization = %q; the shared query lost the token", gotAuth)
	}
	if !strings.Contains(gotPath, "head=alice:impl%2Fwidgets") {
		t.Errorf("path = %q, want the query scoped to the branch", gotPath)
	}
	if strings.Contains(gotPath, "github_pat_secret") {
		t.Errorf("the token reached the URL: %q", gotPath)
	}
}

func TestGitHubPATWithNoBranchAsksNothing(t *testing.T) {
	asked := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked = true
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	p, err := NewGitHubPAT(GitHubPATConfig{Name: "gh", APIURL: srv.URL, Token: "github_pat_secret"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.FindPullRequest(context.Background(), RepoRef{Owner: "alice", Name: "spov"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil || asked {
		t.Errorf("queried with no branch to match: got %+v, asked %v", got, asked)
	}
}

// A PAT for every repository is the PAT itself.
//
// @decision(DL-043)
func TestGitHubPATWideTokenIsTheToken(t *testing.T) {
	p, err := NewGitHubPAT(GitHubPATConfig{Name: "gh", Token: "github_pat_secret"})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := p.IssueWideToken(context.Background(), time.Hour)
	if err != nil || tok.Secret != "github_pat_secret" || tok.Username != GitHubPATUser || tok.Revoke != nil {
		t.Errorf("wide token %+v, %v", tok, err)
	}
}
