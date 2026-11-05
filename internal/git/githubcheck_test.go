package git

import (
	"context"
	"strings"
	"testing"
)

// A check finds the installation, compares its grants with what a
// sandbox asks for, lists what it reaches and revokes the token it used.
//
// @decision(DL-042)
func TestACheckReportsTheAppsScope(t *testing.T) {
	s := newGitHubServer(t)
	s.installPerms = map[string]string{"contents": "write", "pull_requests": "read", "metadata": "read"}
	s.repos = []map[string]any{
		{"name": "app", "owner": map[string]any{"login": "acme"}, "clone_url": "https://github.com/acme/app.git", "updated_at": "2026-10-01T00:00:00Z"},
		{"name": "lib", "owner": map[string]any{"login": "acme"}, "clone_url": "https://github.com/acme/lib.git", "updated_at": "2026-10-02T00:00:00Z"},
	}
	got, err := newTestGitHub(t, s).Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.InstallationID != 42 || got.Account != "acme" || got.RepoCount != 2 || strings.Join(got.Repos, ",") != "acme/lib,acme/app" {
		t.Errorf("status = %+v", got)
	}
	if strings.Join(got.Missing, ",") != "issues:write,pull_requests:write" {
		t.Errorf("missing = %v", got.Missing)
	}
	if s.revoked != 1 {
		t.Errorf("%d tokens revoked, want the one the check minted", s.revoked)
	}
}

// A step that fails fails the check and is named.
//
// @decision(DL-042)
func TestACheckNamesTheStepThatFailed(t *testing.T) {
	s := newGitHubServer(t)
	if _, err := newTestGitHub(t, s).Check(context.Background()); err == nil || !strings.Contains(err.Error(), "reading the installation") {
		t.Errorf("no grants: %v", err)
	}
	s.installPerms = map[string]string{"contents": "write"}
	s.mintStatus, s.mintMessage = 401, "bad key"
	if _, err := newTestGitHub(t, s).Check(context.Background()); err == nil || !strings.Contains(err.Error(), "listing") {
		t.Errorf("a failed mint: %v", err)
	}
}
