package cli

import (
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/server"
)

// A platform with no base URL is the public GitHub or GitLab; any other
// kind with none has no host, so it never matches a checkout. A default
// port is dropped and the host is lowercased.
//
// @decision(DL-027)
func TestAPlatformsHostComesFromItsBaseURL(t *testing.T) {
	for _, tc := range []struct {
		p          config.Platform
		host, base string
	}{
		{config.Platform{Kind: config.KindGitHub}, "github.com", ""},
		{config.Platform{Kind: config.KindGitLab}, "gitlab.com", ""},
		{config.Platform{Kind: config.KindForgejo}, "", ""},
		{config.Platform{Kind: config.KindGitea, BaseURL: "https://Git.Example.com:443/"}, "git.example.com", ""},
		{config.Platform{Kind: config.KindGitLab, BaseURL: "https://example.com/gitlab/"}, "example.com", "/gitlab"},
	} {
		host, base := platformHostAndBase(tc.p)
		if host != tc.host || base != tc.base {
			t.Errorf("%+v = %q %q, want %q %q", tc.p, host, base, tc.host, tc.base)
		}
	}
}

// The platform a synced checkout uses decides whose credential reads the
// repository, so a clear match is taken, an ambiguous one is refused
// naming the choices, and an unknown host is refused.
//
// @decision(DL-027)
func TestACheckoutIsMatchedToExactlyOnePlatform(t *testing.T) {
	cfg := config.Config{Platforms: []config.Platform{
		{Name: "gh-work", Kind: config.KindGitHub, Account: "acme"},
		{Name: "gh-me", Kind: config.KindGitHub, Account: "me"},
		{Name: "codeberg", Kind: config.KindForgejo, BaseURL: "https://codeberg.org"},
		{Name: "codeberg-2", Kind: config.KindForgejo, BaseURL: "https://codeberg.org"},
		{Name: "gl", Kind: config.KindGitLab},
	}}
	at := func(host, owner, name string) git.LocalCheckout {
		return git.LocalCheckout{Host: host, Owner: owner, Name: name, Remote: "origin"}
	}

	// Two platforms on one host: the account owning the repository wins.
	p, err := matchPlatformForCheckout(cfg, at("github.com", "me", "tool"), "")
	if err != nil || p.Name != "gh-me" {
		t.Errorf("by account = %q, %v; want gh-me", p.Name, err)
	}
	// A configured repository settles it when the account cannot.
	byRepo := cfg
	byRepo.Repos = []config.Repo{{Platform: "codeberg-2", Owner: "o", Name: "r"}}
	p, err = matchPlatformForCheckout(byRepo, at("codeberg.org", "o", "r"), "")
	if err != nil || p.Name != "codeberg-2" {
		t.Errorf("by repos entry = %q, %v; want codeberg-2", p.Name, err)
	}
	// Otherwise it is ambiguous, and both names are offered.
	_, err = matchPlatformForCheckout(cfg, at("codeberg.org", "o", "r"), "")
	if err == nil || !strings.Contains(err.Error(), "codeberg, codeberg-2") || !strings.Contains(err.Error(), "--repo") {
		t.Errorf("ambiguous err = %v", err)
	}
	// An unknown host names the host and the file to edit.
	_, err = matchPlatformForCheckout(cfg, at("example.org", "o", "r"), "")
	if err == nil || !strings.Contains(err.Error(), "example.org") || !strings.Contains(err.Error(), "platforms:") {
		t.Errorf("unknown host err = %v", err)
	}
	// A name that is not a legal repository path is refused before use.
	if _, err = matchPlatformForCheckout(cfg, at("github.com", "me", "bad name"), ""); err == nil {
		t.Error("an illegal repository name matched")
	}
}

// An explicit platform is checked against the remote rather than trusted.
//
// @decision(DL-027)
func TestAnExplicitPlatformMustMatchTheRemote(t *testing.T) {
	cfg := config.Config{Platforms: []config.Platform{
		{Name: "gh", Kind: config.KindGitHub},
		{Name: "gl", Kind: config.KindGitLab},
	}}
	checkout := git.LocalCheckout{Host: "github.com", Owner: "o", Name: "r"}
	if p, err := matchPlatformForCheckout(cfg, checkout, "gh"); err != nil || p.Name != "gh" {
		t.Errorf("matching explicit = %q, %v", p.Name, err)
	}
	if _, err := matchPlatformForCheckout(cfg, checkout, "gl"); err == nil || !strings.Contains(err.Error(), "does not match platform gl") {
		t.Errorf("mismatched explicit err = %v", err)
	}
	if _, err := matchPlatformForCheckout(cfg, checkout, "nope"); err == nil || !strings.Contains(err.Error(), "unknown platform") {
		t.Errorf("unknown explicit err = %v", err)
	}
}

// A session with no recorded repository but a single clone still names
// its one repository, a GitLab subgroup path included.
//
// @decision(DL-041)
func TestASessionsOnlyRepositoryIsFoundFromItsClones(t *testing.T) {
	if _, ok := singleViewRepo(serverView(nil, nil)); ok {
		t.Error("no repositories gave one")
	}
	if r, ok := singleViewRepo(serverView(nil, []string{"grp/sub/proj"})); !ok || r.Owner != "grp/sub" || r.Name != "proj" {
		t.Errorf("clone = %+v, %v", r, ok)
	}
	if _, ok := singleViewRepo(serverView(nil, []string{"a/b", "c/d"})); ok {
		t.Error("two clones gave one")
	}
}

func serverView(repos []server.RepoRef, clones []string) server.SessionView {
	return server.SessionView{Repos: repos, Clones: clones}
}
