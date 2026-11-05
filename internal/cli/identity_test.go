package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/git"
)

// TestGitIdentityPrecedence: flags, then config, then the local git
// config, with halves resolved independently.
//
// @decision(DL-044)
func TestGitIdentityPrecedence(t *testing.T) {
	cfg := config.Defaults()
	cfg.Git = config.Git{Email: "cfg@example.com"}
	env := &Env{
		Err:     &strings.Builder{},
		Environ: config.MapEnv{Vars: map[string]string{}},
		LocalIdentity: func(context.Context, string) (git.Identity, error) {
			return git.Identity{Name: "Local Person", Email: "local@example.com"}, nil
		},
	}
	got := env.gitIdentity(context.Background(), cfg, git.Identity{})
	if got != (git.Identity{Name: "Local Person", Email: "cfg@example.com"}) {
		t.Errorf("identity = %+v", got)
	}
	got = env.gitIdentity(context.Background(), cfg, git.Identity{Name: "Flag Person"})
	if got != (git.Identity{Name: "Flag Person", Email: "cfg@example.com"}) {
		t.Errorf("identity with a flag = %+v", got)
	}
}

func TestGitIdentityWarnsWhenGitCannotBeRead(t *testing.T) {
	var errBuf strings.Builder
	env := &Env{
		Err:           &errBuf,
		Environ:       config.MapEnv{Vars: map[string]string{}},
		LocalIdentity: func(context.Context, string) (git.Identity, error) { return git.Identity{}, errors.New("no git") },
	}
	got := env.gitIdentity(context.Background(), config.Defaults(), git.Identity{})
	if got != (git.Identity{}) {
		t.Errorf("identity = %+v", got)
	}
	if !strings.Contains(errBuf.String(), "no git") {
		t.Errorf("no warning:\n%s", errBuf.String())
	}
}

func TestIdentityWarningNamesOnlyWhatIsMissing(t *testing.T) {
	if w := identityWarning(git.Identity{Name: "x", Email: "y"}); w != "" {
		t.Errorf("warned for a complete identity: %q", w)
	}
	w := identityWarning(git.Identity{Name: "x"})
	if !strings.Contains(w, "user.email") || strings.Contains(w, "user.name") {
		t.Errorf("warning = %q", w)
	}
	w = identityWarning(git.Identity{})
	if !strings.Contains(w, "user.name or user.email") {
		t.Errorf("warning = %q", w)
	}
}

// TestFlyPassesTheIdentityToTheSandbox: the identity the client
// resolves travels in the request and reaches the sandbox.
//
// @decision(DL-044) @decision(DL-002)
func TestFlyPassesTheIdentityToTheSandbox(t *testing.T) {
	f := newAPIFixture(t)
	_, stderr, err := runWith(t, f.env, "fly", "--git-name", "Flag Person")
	if err != nil {
		t.Fatalf("fly: %v\n%s", err, stderr)
	}
	// The default runWith identity supplies the email; the flag the name.
	sess := f.session(f.provider.Created[0].Name)
	if sess.Request.GitName != "Flag Person" || sess.Request.GitEmail != "test@example.com" {
		t.Errorf("request identity = %q <%q>", sess.Request.GitName, sess.Request.GitEmail)
	}
	joined := strings.Join(f.provider.Created[0].Command, " ")
	if !strings.Contains(joined, "Flag Person") || !strings.Contains(joined, "test@example.com") {
		t.Errorf("the sandbox command does not configure the identity:\n%s", joined)
	}
}

// TestFlyWarnsWithoutAnIdentity: git refuses to commit without one, and
// that refusal happens later, inside the sandbox.
//
// @decision(DL-044)
func TestFlyWarnsWithoutAnIdentity(t *testing.T) {
	f := newAPIFixture(t)
	f.env.LocalIdentity = func(context.Context, string) (git.Identity, error) { return git.Identity{}, nil }
	_, stderr, err := runWith(t, f.env, "fly")
	if err != nil {
		t.Fatalf("fly: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "no git user.name or user.email configured") {
		t.Errorf("no warning:\n%s", stderr)
	}
}

// TestServerIdentityFillsWhatTheClientLacks: a hosted server may carry
// a house identity; the client's halves win where set.
//
// @decision(DL-002)
func TestServerIdentityFillsWhatTheClientLacks(t *testing.T) {
	f := newAPIFixtureWithConfig(t, defaultConfig+"git:\n  name: Server Person\n  email: server@example.com\n")
	f.env.LocalIdentity = func(context.Context, string) (git.Identity, error) { return git.Identity{}, nil }
	// The client's config file is the same one here, so clear its view
	// of the identity to isolate the server's.
	f.env.cachedConfig = nil
	_, stderr, err := runWith(t, f.env, "fly", "--git-email", "me@example.com")
	if err != nil {
		t.Fatalf("fly: %v\n%s", err, stderr)
	}
	joined := strings.Join(f.provider.Created[0].Command, " ")
	if !strings.Contains(joined, "Server Person") || !strings.Contains(joined, "me@example.com") {
		t.Errorf("identity in the sandbox:\n%s", joined)
	}
}

// TestGitIdentityPrecedenceExhaustively covers every combination of the
// three sources against both halves — 64 in all. The resolution composes
// ResolveIdentity twice, which is easy to get subtly wrong for a
// combination no hand-written case happens to cover.
//
// @decision(DL-044)
func TestGitIdentityPrecedenceExhaustively(t *testing.T) {
	// set returns v when present, so each source can be independently
	// absent for either half.
	set := func(present bool, v string) string {
		if present {
			return v
		}
		return ""
	}
	firstNonEmpty := func(vs ...string) string {
		for _, v := range vs {
			if v != "" {
				return v
			}
		}
		return ""
	}

	for i := range 64 {
		flagName, flagEmail := i&1 != 0, i&2 != 0
		cfgName, cfgEmail := i&4 != 0, i&8 != 0
		localName, localEmail := i&16 != 0, i&32 != 0

		local := git.Identity{Name: set(localName, "Local"), Email: set(localEmail, "local@x")}
		env := &Env{LocalIdentity: func(context.Context, string) (git.Identity, error) { return local, nil }}
		cfg := config.Config{Git: config.Git{
			Name:  set(cfgName, "Cfg"),
			Email: set(cfgEmail, "cfg@x"),
		}}
		flags := git.Identity{Name: set(flagName, "Flag"), Email: set(flagEmail, "flag@x")}

		want := git.Identity{
			Name:  firstNonEmpty(flags.Name, cfg.Git.Name, local.Name),
			Email: firstNonEmpty(flags.Email, cfg.Git.Email, local.Email),
		}
		if got := env.gitIdentity(context.Background(), cfg, flags); got != want {
			t.Errorf("flags=%+v cfg=%+v local=%+v -> %+v, want %+v",
				flags, cfg.Git, local, got, want)
		}
	}
}

// TestConfigShowIncludesTheGitIdentity: `spov config show` exists to
// explain the merged configuration, so a section it silently omits is a
// section the user cannot debug.
//
// @decision(DL-044) @decision(DL-026)
func TestConfigShowIncludesTheGitIdentity(t *testing.T) {
	wd := t.TempDir()
	environ := userConfigEnv(t, `platforms:
  - name: codefloe
    kind: forgejo
    base_url: https://codefloe.com
    account: alice
git:
  name: Cfg Person
  email: cfg@example.com
`)
	out, _, err := runWith(t, Env{Workdir: wd, Environ: environ}, "config", "show")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Cfg Person", "cfg@example.com"} {
		if !strings.Contains(out, want) {
			t.Errorf("config show does not report %q:\n%s", want, out)
		}
	}
}

// TestConfigShowOmitsAnUnsetGitIdentity: printing an empty section would
// imply spov has an identity it does not have — what is actually used
// then is the launching machine's git config.
//
// @decision(DL-044)
func TestConfigShowOmitsAnUnsetGitIdentity(t *testing.T) {
	wd := t.TempDir()
	environ := userConfigEnv(t, `platforms:
  - name: codefloe
    kind: forgejo
    base_url: https://codefloe.com
    account: alice
`)
	out, _, err := runWith(t, Env{Workdir: wd, Environ: environ}, "config", "show")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "git:") {
		t.Errorf("config show reports an empty git section:\n%s", out)
	}
}
