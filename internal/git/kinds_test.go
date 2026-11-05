package git

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// Every kind is known and named for a person; anything else is neither,
// and is given back as typed rather than dressed as a forge.
//
// @decision(DL-041)
func TestEveryKindIsKnownAndNamedForAPerson(t *testing.T) {
	names := map[string]string{KindGitHub: "GitHub", KindForgejo: "Forgejo", KindGitea: "Gitea", KindGitLab: "GitLab"}
	for _, k := range Kinds {
		if !KnownKind(k) {
			t.Errorf("KnownKind(%q) = false", k)
		}
		if got := KindName(k); got != names[k] {
			t.Errorf("KindName(%q) = %q, want %q", k, got, names[k])
		}
	}
	for _, k := range []string{"", "bitbucket", "GitHub"} {
		if KnownKind(k) {
			t.Errorf("KnownKind(%q) = true", k)
		}
		if got := KindName(k); got != k {
			t.Errorf("KindName(%q) = %q, want it unchanged", k, got)
		}
	}
}

// Only GitLab has merge requests, written !12; every other kind, an
// unknown one included, has pull requests, written #12. The web mirrors
// these in deliverables.ts.
//
// @decision(DL-051)
func TestOnlyGitLabCallsAChangeAMergeRequest(t *testing.T) {
	for _, tc := range []struct {
		kind, noun, ref string
	}{
		{KindGitLab, "merge request", "!12"},
		{KindGitHub, "pull request", "#12"},
		{KindForgejo, "pull request", "#12"},
		{KindGitea, "pull request", "#12"},
		{"", "pull request", "#12"},
	} {
		if got := ChangeNoun(tc.kind); got != tc.noun {
			t.Errorf("ChangeNoun(%q) = %q, want %q", tc.kind, got, tc.noun)
		}
		if got := ChangeRef(tc.kind, 12); got != tc.ref {
			t.Errorf("ChangeRef(%q) = %q, want %q", tc.kind, got, tc.ref)
		}
	}
}

// A fine-grained token is told apart by its prefix, whitespace around a
// pasted token aside.
//
// @decision(DL-042)
func TestAFineGrainedTokenIsToldByItsPrefix(t *testing.T) {
	for in, want := range map[string]bool{
		"github_pat_11ABC_secret":   true,
		"  github_pat_11ABC_secret": true,
		"ghp_classic":               false,
		"":                          false,
		`{"app_id":1}`:              false,
	} {
		if got := IsFineGrainedGitHubPAT(in); got != want {
			t.Errorf("IsFineGrainedGitHubPAT(%q) = %v, want %v", in, got, want)
		}
	}
}

// A token check tells a bad token, a missing permission and an
// unsupported server apart, and says nothing for any other status.
//
// @decision(DL-041)
func TestATokenCheckStatusMapsToTheErrorAPersonCanActOn(t *testing.T) {
	for code, want := range map[int]error{
		http.StatusUnauthorized: ErrBadToken,
		http.StatusForbidden:    ErrMissingPermission,
		http.StatusNotFound:     ErrUnsupportedServer,
	} {
		if got := statusError(code); !errors.Is(got, want) {
			t.Errorf("statusError(%d) = %v, want %v", code, got, want)
		}
	}
	for _, code := range []int{200, 400, 429, 500, 502} {
		if got := statusError(code); got != nil {
			t.Errorf("statusError(%d) = %v, want nil", code, got)
		}
	}
}

type bareInspect struct{ Platform }

// A platform that cannot describe its token answers ErrUnsupported
// rather than an empty, successful TokenInfo.
//
// @decision(DL-041)
func TestAPlatformThatCannotInspectItsTokenIsUnsupported(t *testing.T) {
	info, err := Inspect(context.Background(), bareInspect{})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	if info.User != "" || !info.ExpiresAt.Equal(time.Time{}) {
		t.Errorf("info = %+v, want zero", info)
	}
}

// A GitLab token cannot be narrowed, so a wide issue hands back the
// stored token and the scope says it reaches every project, while a
// cancelled launch gets nothing.
//
// @decision(DL-043) @decision(DL-041)
func TestAGitLabWideTokenIsTheStoredTokenAndSaysItIsNotNarrowed(t *testing.T) {
	g, err := NewGitLab(GitLabConfig{Name: "gl", BaseURL: "https://gitlab.example.com", Token: gitlabToken})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := g.IssueWideToken(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Username != GitLabUser || tok.Secret != gitlabToken {
		t.Errorf("token = %+v", tok)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.IssueWideToken(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled issue err = %v, want context.Canceled", err)
	}
	if g.TokenScope().PerRepo {
		t.Error("a GitLab token claims to be narrowed to repositories")
	}
	if g.Name() != "gl" {
		t.Errorf("Name = %q", g.Name())
	}
}
