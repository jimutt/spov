//go:build integration

package git

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// These tests hit a real Forgejo instance. Run them with:
//
//	SPOV_TEST_FORGEJO_URL=https://codefloe.com \
//	SPOV_TEST_FORGEJO_ACCOUNT=alice \
//	SPOV_TEST_FORGEJO_TOKEN=<repository-scoped token> \
//	go test -tags integration ./internal/git/ -run Integration -v
//
// The token needs write:repository and write:issue, scoped to "Specific
// repositories".
// Nothing is created or deleted on the server.

func integrationForgejo(t *testing.T) *Forgejo {
	t.Helper()
	cfg := ForgejoConfig{
		Name:    "codefloe",
		BaseURL: os.Getenv("SPOV_TEST_FORGEJO_URL"),
		Account: os.Getenv("SPOV_TEST_FORGEJO_ACCOUNT"),
		Token:   os.Getenv("SPOV_TEST_FORGEJO_TOKEN"),
	}
	if cfg.BaseURL == "" || cfg.Account == "" || cfg.Token == "" {
		t.Skip("set SPOV_TEST_FORGEJO_URL, _ACCOUNT and _TOKEN to run Forgejo integration tests")
	}
	f, err := NewForgejo(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return f
}

func TestIntegrationForgejoListRepos(t *testing.T) {
	f := integrationForgejo(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	repos, err := f.ListRepos(ctx)
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	t.Logf("%d repositories", len(repos))
	for i, r := range repos {
		if i < 5 {
			t.Logf("  %s private=%v updated=%s", r.Slug(), r.Private, r.UpdatedAt.Format(time.RFC3339))
		}
		if r.Owner == "" || r.Name == "" {
			t.Errorf("repo %d has no owner or name: %+v", i, r)
		}
		if !strings.HasPrefix(r.CloneURL, "https://") {
			t.Errorf("%s clone URL is not https: %q", r.Slug(), r.CloneURL)
		}
	}
	// Newest first, so the picker shows recent work at the top.
	for i := 1; i < len(repos); i++ {
		if repos[i-1].UpdatedAt.Before(repos[i].UpdatedAt) {
			t.Errorf("repos are not newest-first at index %d", i)
			break
		}
	}
}

// TestIntegrationForgejoStoredTokenClones proves the stored token is
// usable for what a sandbox actually does: cloning over HTTPS with the
// credential embedded in the URL.
func TestIntegrationForgejoStoredTokenClones(t *testing.T) {
	f := integrationForgejo(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	repos, err := f.ListRepos(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) == 0 {
		t.Skip("the account has no repositories")
	}
	target := repos[0]

	tok, err := f.IssueSandboxToken(ctx, []Repo{target}, time.Hour)
	if err != nil {
		t.Fatalf("IssueSandboxToken: %v", err)
	}
	if tok.Revoke != nil {
		t.Error("spov did not create this token and must not claim it can revoke it")
	}

	url, err := CloneURLWithToken(target.CloneURL, tok)
	if err != nil {
		t.Fatal(err)
	}
	out, err := gitClone(ctx, url, t.TempDir())
	if err != nil {
		t.Fatalf("clone %s failed: %v\n%s", target.Slug(), err, RedactAll(string(out)))
	}
	t.Logf("cloned %s with the stored token", target.Slug())

	t.Run("the clone URL redacts cleanly for logging", func(t *testing.T) {
		if red := RedactURL(url); strings.Contains(red, tok.Secret) {
			t.Error("the token survived redaction")
		}
	})
}
