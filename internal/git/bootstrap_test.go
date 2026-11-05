package git

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBootstrap(t *testing.T) {
	f := NewFake()
	f.Repos = []Repo{
		{Owner: "acme", Name: "app", CloneURL: "https://git.example.com/acme/app.git"},
		{Owner: "acme", Name: "api", CloneURL: "https://git.example.com/acme/api.git"},
	}

	result, revoke, err := Bootstrap(context.Background(), []Selection{{Platform: f, Repos: f.Repos}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Clones) != 2 {
		t.Fatalf("clones = %+v", result.Clones)
	}
	if result.Clones[0].Dir != "app" || result.Clones[1].Dir != "api" {
		t.Errorf("directories = %q, %q", result.Clones[0].Dir, result.Clones[1].Dir)
	}
	for _, c := range result.Clones {
		// The credential lives in the sandbox's git credential store, not
		// the URL: an embedded token is readable by anything running
		// `git remote -v`, and stops tea matching a repo to its login.
		if strings.Contains(c.URL, "fake-secret-") || strings.Contains(c.URL, "@") {
			t.Errorf("clone URL carries a credential: %q", RedactURL(c.URL))
		}
		if c.Slug == "" {
			t.Error("clone has no slug to log")
		}
	}
	// The token is still returned, for the credential store and the CLIs.
	if len(result.Tokens) != 1 || result.Tokens[0].Secret == "" {
		t.Errorf("tokens = %+v", result.Tokens)
	}

	// One token for the whole platform, not one per repo.
	if got := f.Requested(); len(got) != 1 {
		t.Errorf("minted %d tokens for one platform, want 1", len(got))
	}

	if err := revoke(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.Revoked() != 1 {
		t.Errorf("revoked %d tokens, want 1", f.Revoked())
	}
}

func TestBootstrapMintsOneTokenPerPlatform(t *testing.T) {
	a, b := NewFake(), NewFake()
	a.PlatformName, b.PlatformName = "codefloe", "github"
	b.HostName = "github.com"
	b.Repos = []Repo{{Owner: "acme", Name: "other", CloneURL: "https://github.com/acme/other.git"}}

	result, revoke, err := Bootstrap(context.Background(), []Selection{
		{Platform: a, Repos: a.Repos},
		{Platform: b, Repos: b.Repos},
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Clones) != 2 {
		t.Fatalf("clones = %+v", result.Clones)
	}
	if len(a.Requested()) != 1 || len(b.Requested()) != 1 {
		t.Errorf("tokens issued: a=%d b=%d, want 1 each", len(a.Requested()), len(b.Requested()))
	}
	if err := revoke(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.Revoked() != 1 || b.Revoked() != 1 {
		t.Errorf("revoked: a=%d b=%d, want 1 each", a.Revoked(), b.Revoked())
	}
}

// TestBootstrapRevokesTokensMintedBeforeAFailure is the leak case: a
// Forgejo token has no expiry, so one minted before a later failure would
// live forever if the caller could not revoke it.
//
// @decision(DL-041)
func TestBootstrapRevokesPartialWork(t *testing.T) {
	good := NewFake()
	good.PlatformName = "good"
	bad := NewFake()
	bad.PlatformName = "bad"
	bad.HostName = "bad.example.com"
	bad.IssueErr = errors.New("403 forbidden")

	_, revoke, err := Bootstrap(context.Background(), []Selection{
		{Platform: good, Repos: good.Repos},
		{Platform: bad, Repos: bad.Repos},
	}, time.Hour)
	if err == nil {
		t.Fatal("expected an error")
	}
	if revoke == nil {
		t.Fatal("no revoke function was returned, so the first token leaks")
	}
	if err := revoke(context.Background()); err != nil {
		t.Fatal(err)
	}
	if good.Revoked() != 1 {
		t.Errorf("the token minted before the failure was not revoked (revoked=%d)", good.Revoked())
	}
}

func TestBootstrapRevokeReportsFirstError(t *testing.T) {
	a, b := NewFake(), NewFake()
	a.PlatformName, b.PlatformName = "a", "b"
	b.HostName = "b.example.com"
	a.RevokeErr = errors.New("revoke failed")

	_, revoke, err := Bootstrap(context.Background(), []Selection{
		{Platform: a, Repos: a.Repos},
		{Platform: b, Repos: b.Repos},
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := revoke(context.Background()); err == nil {
		t.Error("a failed revocation was reported as success")
	}
	// The other token must still be revoked despite the first failing.
	if b.Revoked() != 1 {
		t.Errorf("a failing revocation stopped the others (b revoked=%d)", b.Revoked())
	}
}

func TestBootstrapDisambiguatesDirectories(t *testing.T) {
	a, b := NewFake(), NewFake()
	a.PlatformName, b.PlatformName = "a", "b"
	b.HostName = "b.example.com"
	a.Repos = []Repo{{Owner: "acme", Name: "app", CloneURL: "https://git.example.com/acme/app.git"}}
	b.Repos = []Repo{{Owner: "other", Name: "app", CloneURL: "https://git.example.com/other/app.git"}}

	result, _, err := Bootstrap(context.Background(), []Selection{
		{Platform: a, Repos: a.Repos},
		{Platform: b, Repos: b.Repos},
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if result.Clones[0].Dir != "app" {
		t.Errorf("first directory = %q, want the plain name", result.Clones[0].Dir)
	}
	if result.Clones[1].Dir != "other-app" {
		t.Errorf("second directory = %q, want it disambiguated by owner", result.Clones[1].Dir)
	}
}

func TestBootstrapRejects(t *testing.T) {
	t.Run("no selections", func(t *testing.T) {
		_, _, err := Bootstrap(context.Background(), nil, time.Hour)
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "no repositories selected") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("selection with no platform", func(t *testing.T) {
		_, _, err := Bootstrap(context.Background(), []Selection{
			{Repos: []Repo{{Owner: "a", Name: "b"}}},
		}, time.Hour)
		if err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("the same repo twice", func(t *testing.T) {
		f := NewFake()
		r := Repo{Owner: "acme", Name: "app", CloneURL: "https://git.example.com/acme/app.git"}
		_, _, err := Bootstrap(context.Background(), []Selection{{Platform: f, Repos: []Repo{r, r}}}, time.Hour)
		if err == nil {
			t.Fatal("the same repository was accepted twice")
		}
		if !strings.Contains(err.Error(), "selected twice") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("repo with a non-https clone URL", func(t *testing.T) {
		f := NewFake()
		f.Repos = []Repo{{Owner: "a", Name: "b", CloneURL: "ssh://git@x/a/b.git"}}
		_, revoke, err := Bootstrap(context.Background(), []Selection{{Platform: f, Repos: f.Repos}}, time.Hour)
		if err == nil {
			t.Fatal("expected an error")
		}
		// The token was already minted, so it must still be revocable.
		if err := revoke(context.Background()); err != nil {
			t.Fatal(err)
		}
		if f.Revoked() != 1 {
			t.Error("the token minted before the failure was not revocable")
		}
	})

	t.Run("empty selections are skipped", func(t *testing.T) {
		f := NewFake()
		empty := NewFake()
		empty.PlatformName = "empty"
		result, _, err := Bootstrap(context.Background(), []Selection{
			{Platform: empty, Repos: nil},
			{Platform: f, Repos: f.Repos},
		}, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Clones) != 1 {
			t.Errorf("clones = %+v", result.Clones)
		}
		if len(empty.Requested()) != 0 {
			t.Error("a token was minted for a platform with no selected repos")
		}
	})
}

// TestBootstrapErrorsNeverCarrySecrets: these errors reach the user.
func TestBootstrapErrorsNeverCarrySecrets(t *testing.T) {
	f := NewFake()
	f.Repos = []Repo{{Owner: "a", Name: "b", CloneURL: "ssh://git@x/a/b.git"}}

	_, _, err := Bootstrap(context.Background(), []Selection{{Platform: f, Repos: f.Repos}}, time.Hour)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "fake-secret") {
		t.Errorf("the error leaked a token: %q", err)
	}
}

func TestUniqueDir(t *testing.T) {
	taken := map[string]string{}
	if _, err := uniqueDir(taken, "p", KindForgejo, Repo{Name: "app"}); err == nil {
		t.Error("a repo with no owner was accepted")
	}
	if _, err := uniqueDir(taken, "p", KindForgejo, Repo{Owner: "acme"}); err == nil {
		t.Error("a repo with no name was accepted")
	}

	// The same owner/name on two platforms must disambiguate rather than
	// collide.
	first, err := uniqueDir(taken, "codefloe", KindForgejo, Repo{Owner: "acme", Name: "app"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := uniqueDir(taken, "github", KindForgejo, Repo{Owner: "acme", Name: "app"})
	if err != nil {
		t.Fatalf("the same slug on a second platform was rejected: %v", err)
	}
	if first == second {
		t.Errorf("both platforms got directory %q", first)
	}
	// The owner-prefixed form is still free, so it is preferred over the
	// longer platform-prefixed one.
	if second != "acme-app" {
		t.Errorf("second directory = %q, want the owner-prefixed form", second)
	}

	// A third platform with the same owner and name falls through to the
	// platform-prefixed form.
	third, err := uniqueDir(taken, "gitlab", KindForgejo, Repo{Owner: "acme", Name: "app"})
	if err != nil {
		t.Fatal(err)
	}
	if third != "gitlab-acme-app" {
		t.Errorf("third directory = %q", third)
	}
}

// A GitLab project in a subgroup is cloned under its name, and a clash
// falls back to its whole group path joined with dashes. A nested owner
// on another kind is refused.
//
// @decision(DL-041)
func TestANestedGroupBecomesOneDirectoryName(t *testing.T) {
	taken := map[string]string{}
	first, err := uniqueDir(taken, "gitlab", KindGitLab, Repo{Owner: "group/sub", Name: "app"})
	if err != nil || first != "app" {
		t.Fatalf("first = %q, %v", first, err)
	}
	second, err := uniqueDir(taken, "gitlab", KindGitLab, Repo{Owner: "group/other", Name: "app"})
	if err != nil || second != "group-other-app" {
		t.Fatalf("second = %q, %v", second, err)
	}
	if _, err := uniqueDir(taken, "codefloe", KindForgejo, Repo{Owner: "group/sub", Name: "x"}); err == nil {
		t.Error("a nested owner on forgejo was accepted")
	}
	if _, err := uniqueDir(taken, "gitlab", KindGitLab, Repo{Owner: "group/../x", Name: "y"}); err == nil {
		t.Error("a traversal segment was accepted")
	}
}

// A repository named after a directory spov owns is cloned into its
// owner-prefixed directory instead.
//
// @decision(DL-037)
func TestNoCloneTakesAReservedDirectory(t *testing.T) {
	f := NewFake()
	repos := []Repo{
		{Owner: "acme", Name: "out", CloneURL: "https://x.example/acme/out.git"},
		{Owner: "acme", Name: ".scratch", CloneURL: "https://x.example/acme/.scratch.git"},
	}
	res, _, err := Bootstrap(context.Background(), []Selection{{Platform: f, Repos: repos}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range res.Clones {
		got = append(got, c.Dir)
	}
	if want := []string{"acme-out", "acme-.scratch"}; !slices.Equal(got, want) {
		t.Errorf("clone directories %v, want %v", got, want)
	}
}

// TestBootstrapRevokeIsIdempotent: a deferred revoke plus an explicit one
// on an error path is a normal pattern, and a platform issuing real
// tokens would reject the second revocation.
//
// @decision(DL-041)
func TestBootstrapRevokeIsIdempotent(t *testing.T) {
	f := NewFake()
	_, revoke, err := Bootstrap(context.Background(), []Selection{{Platform: f, Repos: f.Repos}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	for i := range 3 {
		if err := revoke(context.Background()); err != nil {
			t.Fatalf("revoke %d: %v", i, err)
		}
	}
	if f.Revoked() != 1 {
		t.Errorf("the token was revoked %d times, want exactly 1", f.Revoked())
	}
}

func TestBootstrapRevokeIsConcurrencySafe(t *testing.T) {
	f := NewFake()
	_, revoke, err := Bootstrap(context.Background(), []Selection{{Platform: f, Repos: f.Repos}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := revoke(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if f.Revoked() != 1 {
		t.Errorf("concurrent revokes ran the underlying revoker %d times, want 1", f.Revoked())
	}
}

func TestBootstrapRejectsUnsafeRepoNames(t *testing.T) {
	// Repository names come from a remote API and become directories in
	// the sandbox workspace.
	unsafe := []Repo{
		{Owner: "acme", Name: "..", CloneURL: "https://git.example.com/acme/x.git"},
		{Owner: "acme", Name: ".", CloneURL: "https://git.example.com/acme/x.git"},
		{Owner: "acme", Name: "-rf", CloneURL: "https://git.example.com/acme/x.git"},
		{Owner: "acme", Name: "a/b", CloneURL: "https://git.example.com/acme/x.git"},
		{Owner: "acme", Name: "a b", CloneURL: "https://git.example.com/acme/x.git"},
		{Owner: "acme", Name: "a\nb", CloneURL: "https://git.example.com/acme/x.git"},
		{Owner: "..", Name: "app", CloneURL: "https://git.example.com/acme/x.git"},
		{Owner: "-x", Name: "app", CloneURL: "https://git.example.com/acme/x.git"},
	}
	for _, r := range unsafe {
		t.Run(r.Owner+"/"+r.Name, func(t *testing.T) {
			f := NewFake()
			f.Repos = []Repo{r}
			if _, _, err := Bootstrap(context.Background(), []Selection{{Platform: f, Repos: f.Repos}}, time.Hour); err == nil {
				t.Errorf("accepted repository %q/%q", r.Owner, r.Name)
			}
		})
	}
}

func TestValidDirComponent(t *testing.T) {
	valid := []string{"spov", "my-repo", "my_repo", "repo.git", "Repo2", "a-b-c"}
	for _, s := range valid {
		if err := validDirComponent(s); err != nil {
			t.Errorf("validDirComponent(%q) = %v, want nil", s, err)
		}
	}
	invalid := []string{"", ".", "..", "-x", "a/b", `a\b`, "a b", "a\tb", "a\nb", "a\x00b"}
	for _, s := range invalid {
		if err := validDirComponent(s); err == nil {
			t.Errorf("validDirComponent(%q) = nil, want an error", s)
		}
	}
}

// TestBootstrapReturnsPlatformTokens: the forge CLIs inside the sandbox
// need the token, and it must arrive separately from the clone URL so it
// can go in the environment rather than the entrypoint script.
//
// @decision(DL-032)
func TestBootstrapReturnsPlatformTokens(t *testing.T) {
	a, b := NewFake(), NewFake()
	a.PlatformName, a.HostName = "codefloe", "codefloe.com"
	b.PlatformName, b.HostName, b.PlatformKind = "gh", "github.com", "github"
	b.Repos = []Repo{{Owner: "acme", Name: "api", CloneURL: "https://github.com/acme/api.git"}}

	result, _, err := Bootstrap(context.Background(), []Selection{
		{Platform: a, Repos: a.Repos},
		{Platform: b, Repos: b.Repos},
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// One token per platform, not one per repository.
	if len(result.Tokens) != 2 {
		t.Fatalf("tokens = %+v", len(result.Tokens))
	}
	for _, tok := range result.Tokens {
		if tok.Secret == "" || tok.Username == "" || tok.Host == "" {
			t.Errorf("incomplete token for %s", tok.Platform)
		}
	}

	// Clones carry the platform and host so the workspace guide can name
	// the right CLI without re-deriving it.
	for _, c := range result.Clones {
		if c.Platform == "" || c.Host == "" {
			t.Errorf("clone %s has no platform or host", c.Slug)
		}
	}
}

// A selection with All mints a token for every repository even with
// nothing to clone; one without is still skipped.
//
// @decision(DL-043)
func TestBootstrapMintsForAllWithoutRepositories(t *testing.T) {
	a, b, c := NewFake(), NewFake(), NewFake()
	a.PlatformName, b.PlatformName, c.PlatformName = "a", "b", "c"
	b.HostName, c.HostName = "b.example.com", "c.example.com"
	result, _, err := Bootstrap(context.Background(), []Selection{
		{Platform: a, Repos: a.Repos},
		{Platform: b, All: true},
		{Platform: c},
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Tokens) != 2 || result.Tokens[0].All || !result.Tokens[1].All || result.Tokens[1].Platform != "b" {
		t.Errorf("tokens = %+v", result.Tokens)
	}
	if b.Wide() != 1 || len(c.Requested()) != 0 || a.Wide() != 0 {
		t.Errorf("wide: a %d, b %d; c asked %d times", a.Wide(), b.Wide(), len(c.Requested()))
	}
}

// narrowOnly is a platform that can mint only for named repositories.
type narrowOnly struct{ Platform }

// A platform that cannot mint for every repository says so rather than
// minting a narrow token under all.
//
// @decision(DL-043)
func TestIssueForRefusesAPlatformWithoutAWideToken(t *testing.T) {
	_, err := IssueFor(context.Background(), Selection{Platform: narrowOnly{NewFake()}, All: true}, time.Hour)
	if err == nil || !strings.Contains(err.Error(), "every repository") {
		t.Errorf("err = %v", err)
	}
}

// Two platforms on one host cannot share a session, and the refusal
// comes before any token is minted.
//
// @decision(DL-034)
func TestASessionHoldsOneCredentialPerHost(t *testing.T) {
	a, b := NewFake(), NewFake()
	a.PlatformName, b.PlatformName = "personal", "bot"
	a.HostName, b.HostName = "gitlab.com", "GitLab.com"
	_, _, err := Bootstrap(context.Background(), []Selection{{Platform: a, Repos: a.Repos}, {Platform: b, Repos: b.Repos}}, time.Hour)
	if err == nil || !strings.Contains(err.Error(), `"personal" and "bot"`) {
		t.Fatalf("err = %v", err)
	}
	if len(a.Requested()) != 0 || len(b.Requested()) != 0 {
		t.Errorf("tokens were minted: %v, %v", a.Requested(), b.Requested())
	}
	b.HostName = "gitlab.com:443"
	if _, _, err := Bootstrap(context.Background(), []Selection{{Platform: a, Repos: a.Repos}, {Platform: b, Repos: b.Repos}}, time.Hour); err == nil {
		t.Error("the default port made the same host another")
	}
	b.HostName = "codefloe.com"
	if _, _, err := Bootstrap(context.Background(), []Selection{{Platform: a, Repos: a.Repos}, {Platform: b, Repos: b.Repos}}, time.Hour); err != nil {
		t.Errorf("two hosts: %v", err)
	}
}
