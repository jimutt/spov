package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/git"
)

// `--from-gh` stores a marker, never a token, verifies with the token gh
// gives now, and every later read asks gh again, so a re-login takes
// effect at once.
//
// @decision(DL-083)
func TestAGhLoginIsReadAtEachLaunch(t *testing.T) {
	f := newLoginFixture(t)
	f.env.StateDir = t.TempDir()
	tokens := []string{"gho_first", "gho_second"}
	var asked []string
	f.env.GhToken = func(_ context.Context, host string) (string, error) {
		asked = append(asked, host)
		tok := tokens[0]
		if len(tokens) > 1 {
			tokens = tokens[1:]
		}
		return tok, nil
	}
	var built []string
	f.env.NewPlatform = func(_ context.Context, _ config.Platform, token string) (git.Platform, error) {
		built = append(built, token)
		return f.platform, nil
	}
	if _, _, err := runWith(t, f.env, "credentials", "add", "gh", "--from-gh"); err != nil {
		t.Fatal(err)
	}
	stored, err := f.store.Get("platform:gh")
	if err != nil || stored != "gh:github.com" {
		t.Fatalf("stored %q, %v", stored, err)
	}
	if len(built) != 1 || built[0] != "gho_first" {
		t.Errorf("verified with %v", built)
	}
	tok, err := f.env.platformToken(context.Background(), config.Platform{Name: "gh", Kind: config.KindGitHub})
	if err != nil || tok != "gho_second" {
		t.Errorf("a later read = %q, %v", tok, err)
	}
	if len(asked) != 2 || asked[0] != "github.com" {
		t.Errorf("asked gh for %v", asked)
	}
	out, _, err := runWith(t, f.env, "credentials")
	if err != nil || !strings.Contains(out, "gh's login on github.com") {
		t.Errorf("listing = %q, %v", out, err)
	}
	if len(f.prompted) != 0 {
		t.Errorf("prompted %v", f.prompted)
	}
	for _, args := range [][]string{
		{"credentials", "add", "codefloe", "--from-gh"},
		{"credentials", "add", "OPENROUTER_API_KEY", "--from-gh"},
		{"credentials", "add", "gh", "--from-gh", "--app-id", "1"},
	} {
		if _, _, err := runWith(t, f.env, args...); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
}

// A hosted server never reads this machine's gh login, so --from-gh is
// refused there.
//
// @decision(DL-083)
func TestAGhLoginIsRefusedOnAHostedServer(t *testing.T) {
	f := hostedFixture(t)
	if _, _, err := runWith(t, f.env, "credentials", "add", "github", "--from-gh"); err == nil || !strings.Contains(err.Error(), "hosted") {
		t.Errorf("err = %v", err)
	}
}

// A missing gh, or one with no login, fails the read with that reason and
// never falls back to another credential.
//
// @decision(DL-083)
func TestAMissingGhFailsTheLaunch(t *testing.T) {
	e := &Env{}
	t.Setenv("PATH", t.TempDir())
	if _, err := e.ghToken(context.Background(), "github.com"); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Errorf("no gh: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\necho 'not logged in' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if _, err := e.ghToken(context.Background(), "github.com"); err == nil || !strings.Contains(err.Error(), "gh auth login") {
		t.Errorf("logged out: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\n[ \"$4\" = ghe.example.com ] && echo gho_enterprise\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if tok, err := e.ghToken(context.Background(), "ghe.example.com"); err != nil || tok != "gho_enterprise" {
		t.Errorf("an enterprise host = %q, %v", tok, err)
	}
	if got := ghHost(config.Platform{BaseURL: "https://GHE.example.com/api/v3"}); got != "ghe.example.com" {
		t.Errorf("host = %q", got)
	}
}
