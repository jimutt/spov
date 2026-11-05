//go:build integration

package git

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These tests hit the real GitHub API with a real App. Run them with:
//
//	SPOV_TEST_GITHUB_APP_ID=<app or client id> \
//	SPOV_TEST_GITHUB_KEY_FILE=/path/to/app.private-key.pem \
//	SPOV_TEST_GITHUB_ACCOUNT=<user or org the App is installed on> \
//	go test -tags integration ./internal/git/ -run IntegrationGitHub -v
//
// Optionally set SPOV_TEST_GITHUB_INSTALLATION_ID to skip discovery, and
// SPOV_TEST_GITHUB_REPO to a repository name the App can reach.
//
// TestIntegrationGitHubEndToEnd additionally needs SPOV_TEST_CRED_FILE
// pointing at a spov credentials.json holding a `platform:github` entry.
//
// Tokens are minted and revoked; nothing else on the account is touched.

func integrationGitHub(t *testing.T) *GitHub {
	t.Helper()
	appID := os.Getenv("SPOV_TEST_GITHUB_APP_ID")
	keyFile := os.Getenv("SPOV_TEST_GITHUB_KEY_FILE")
	account := os.Getenv("SPOV_TEST_GITHUB_ACCOUNT")
	if appID == "" || keyFile == "" || account == "" {
		t.Skip("set SPOV_TEST_GITHUB_APP_ID, _KEY_FILE and _ACCOUNT to run GitHub integration tests")
	}

	key, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatalf("read the App private key: %v", err)
	}

	cfg := GitHubConfig{
		Name:          "github",
		Account:       account,
		AppID:         appID,
		PrivateKeyPEM: string(key),
	}
	if raw := os.Getenv("SPOV_TEST_GITHUB_INSTALLATION_ID"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			t.Fatalf("SPOV_TEST_GITHUB_INSTALLATION_ID: %v", err)
		}
		cfg.InstallationID = id
	}

	g, err := NewGitHub(cfg)
	if err != nil {
		t.Fatalf("build the platform: %v", err)
	}
	return g
}

// TestIntegrationGitHubListRepos proves the JWT is accepted, the
// installation resolves, and the response shape is what spov expects.
func TestIntegrationGitHubListRepos(t *testing.T) {
	g := integrationGitHub(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	repos, err := g.ListRepos(ctx)
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	t.Logf("the installation can reach %d repositories", len(repos))
	for i, r := range repos {
		if i >= 5 {
			break
		}
		t.Logf("  %s private=%v archived=%v updated=%s", r.Slug(), r.Private, r.Archived,
			r.UpdatedAt.Format(time.RFC3339))
	}

	for _, r := range repos {
		if r.Owner == "" || r.Name == "" {
			t.Errorf("repository has no owner or name: %+v", r)
		}
		if !strings.HasPrefix(r.CloneURL, "https://") {
			t.Errorf("%s has a non-https clone URL %q", r.Slug(), r.CloneURL)
		}
	}
}

// TestIntegrationGitHubMintAndRevoke is the claim DL-041 rests on: a
// token narrowed to the requested repositories, expiring in an hour, and
// revocable early.
func TestIntegrationGitHubMintAndRevoke(t *testing.T) {
	g := integrationGitHub(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	name := os.Getenv("SPOV_TEST_GITHUB_REPO")
	if name == "" {
		repos, err := g.ListRepos(ctx)
		if err != nil {
			t.Fatalf("ListRepos: %v", err)
		}
		if len(repos) == 0 {
			t.Skip("the installation can reach no repositories")
		}
		name = repos[0].Name
	}

	start := time.Now()
	tok, err := g.IssueSandboxToken(ctx, []Repo{{Owner: g.account, Name: name}}, 4*time.Hour)
	if err != nil {
		t.Fatalf("IssueSandboxToken: %v", err)
	}
	// Registered before any assertion can call t.Fatal: a token this
	// test minted must not outlive it, and revocation is idempotent
	// enough that the explicit revoke below is not disturbed by it.
	var revoked bool
	t.Cleanup(func() {
		if revoked {
			return
		}
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := tok.Revoke(c); err != nil {
			t.Errorf("cleanup: the minted token was not revoked: %v", err)
		}
	})
	t.Logf("minted in %s, expires %s", time.Since(start).Round(time.Millisecond),
		tok.ExpiresAt.Format(time.RFC3339))

	if tok.Username != GitHubTokenUser {
		t.Errorf("username = %q, want %q", tok.Username, GitHubTokenUser)
	}
	if tok.Secret == "" {
		t.Fatal("no token")
	}

	// The requested 4h TTL cannot be honoured: GitHub fixes the lifetime
	// at an hour. This is the fact DL-021's launch warning exists for.
	lifetime := time.Until(tok.ExpiresAt)
	if lifetime > 90*time.Minute {
		t.Errorf("the token lives %s; GitHub documents one hour", lifetime)
	}
	if tok.Expired(time.Now()) {
		t.Error("the freshly minted token is already expired")
	}

	if tok.Revoke == nil {
		t.Fatal("a minted token has no revoker")
	}
	if err := tok.Revoke(ctx); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	revoked = true

	// A revoked token must actually stop working, or revocation is
	// decorative and a leaked token stays live for its full hour.
	//
	// Retried briefly: revocation is not guaranteed to be visible to the
	// next request immediately, and a spurious failure here would be
	// read as "revocation does not work", which is worse than waiting.
	var lastErr error
	for attempt := range 5 {
		if attempt > 0 {
			time.Sleep(time.Second)
		}
		lastErr = g.do(ctx, "GET", "/installation/repositories", "token "+tok.Secret, nil, nil)
		if lastErr != nil {
			break
		}
	}
	if lastErr == nil {
		t.Error("the revoked token still authenticates after several seconds")
	} else {
		t.Logf("the revoked token is rejected: %v", lastErr)
	}
}

// TestIntegrationGitHubTokenIsNarrowed checks that a token minted for one
// repository cannot reach another. It needs an installation with at
// least two repositories.
func TestIntegrationGitHubTokenIsNarrowed(t *testing.T) {
	g := integrationGitHub(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	repos, err := g.ListRepos(ctx)
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(repos) < 2 {
		t.Skip("need an installation with at least two repositories")
	}

	tok, err := g.IssueSandboxToken(ctx, []Repo{repos[0]}, time.Hour)
	if err != nil {
		t.Fatalf("IssueSandboxToken: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = tok.Revoke(c)
	})

	// The token's own view of the installation must list only what was
	// requested.
	var body struct {
		TotalCount   int              `json:"total_count"`
		Repositories []githubRepoJSON `json:"repositories"`
	}
	if err := g.do(ctx, "GET", "/installation/repositories?per_page=100", "token "+tok.Secret, nil, &body); err != nil {
		t.Fatalf("list with the narrowed token: %v", err)
	}

	t.Logf("the narrowed token sees %d of %d repositories", len(body.Repositories), len(repos))
	if len(body.Repositories) != 1 {
		t.Errorf("the narrowed token reaches %d repositories, want exactly the 1 requested",
			len(body.Repositories))
	}
	if len(body.Repositories) == 1 && body.Repositories[0].Name != repos[0].Name {
		t.Errorf("the narrowed token reaches %q, want %q", body.Repositories[0].Name, repos[0].Name)
	}
}

// TestIntegrationGitHubEndToEnd is the whole user-facing path: the
// credential file the README tells you to write, parsed by spov, used to
// mint a narrowed token, used by git to clone a real private repository
// through the credential-store mechanism the sandbox uses.
func TestIntegrationGitHubEndToEnd(t *testing.T) {
	credFile := os.Getenv("SPOV_TEST_CRED_FILE")
	if credFile == "" {
		t.Skip("set SPOV_TEST_CRED_FILE")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	raw, err := os.ReadFile(credFile)
	if err != nil {
		t.Fatal(err)
	}
	var store map[string]string
	if err := json.Unmarshal(raw, &store); err != nil {
		t.Fatal(err)
	}
	cred, err := ParseGitHubCredential(store["platform:github"])
	if err != nil {
		t.Fatalf("spov cannot parse the README-produced credential: %v", err)
	}
	t.Logf("parsed credential: app_id=%s installation_id=%d", cred.AppID, cred.InstallationID)

	g, err := NewGitHub(GitHubConfig{
		Name: "github", Account: "alice",
		AppID: cred.AppID, PrivateKeyPEM: cred.PrivateKey,
		InstallationID: cred.InstallationID,
	})
	if err != nil {
		t.Fatal(err)
	}

	repos, err := g.ListRepos(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A private repo proves the token is doing the work: an anonymous
	// clone of a public repo would succeed regardless.
	var target Repo
	for _, r := range repos {
		if r.Private && !r.Archived {
			target = r
			break
		}
	}
	if target.Name == "" {
		t.Skip("no private repository available to prove authentication")
	}
	t.Logf("cloning private repo %s", target.Slug())

	tok, err := g.IssueSandboxToken(ctx, []Repo{target}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = tok.Revoke(c)
	})

	// Reproduce the sandbox's credential-store mechanism exactly
	// (DL-041): a 0600 credentials file, a credential-free clone URL.
	home := t.TempDir()
	credPath := filepath.Join(home, ".git-credentials")
	line := "https://" + tok.Username + ":" + tok.Secret + "@github.com\n"
	if err := os.WriteFile(credPath, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), "clone")
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", "--", target.CloneURL, dest)
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL="+filepath.Join(home, ".gitconfig"),
	)
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"),
		[]byte("[credential]\n\thelper = store --file="+credPath+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("clone failed: %v\n%s", err, RedactAll(string(out)))
	}
	t.Logf("clone output: %s", strings.TrimSpace(RedactAll(string(out))))

	if _, err := os.Stat(filepath.Join(dest, ".git")); err != nil {
		t.Fatalf("no .git in the clone: %v", err)
	}

	// The remote must carry no credential: `git remote -v` inside the
	// sandbox is readable by the agent.
	remote, err := exec.CommandContext(ctx, "git", "-C", dest, "remote", "-v").Output()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("remote: %s", strings.TrimSpace(string(remote)))
	if strings.Contains(string(remote), tok.Secret) {
		t.Error("the clone URL carries the token")
	}
	if strings.Contains(string(remote), "x-access-token") {
		t.Error("the remote carries credentials")
	}
}
