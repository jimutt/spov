package credproxy

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var (
	github  = Platform{Name: "gh", Kind: KindGitHub, Host: "github.com", User: "x-access-token", Token: "ghs_real", Scoped: true}
	forgejo = Platform{Name: "codefloe", Kind: KindForgejo, Host: "codefloe.com", User: "alice", Token: "f00d", Repos: []string{"o/r"}}
	ghe     = Platform{Name: "ghe", Kind: KindGitHub, Host: "git.example.com", User: "x-access-token", Token: "ghs_ghe", Scoped: true}
)

func newStore(t *testing.T, platforms ...Platform) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credentials.json")
	s, err := NewStore(path, SidecarConfig{Platforms: platforms})
	if err != nil {
		t.Fatal(err)
	}
	return s, path
}

func rewritten(t *testing.T, s *Store, rawURL string) (string, bool) {
	t.Helper()
	r := originForm(httptest.NewRequest(http.MethodGet, rawURL, nil))
	ok := s.Rewrite(r)
	return r.Header.Get("Authorization"), ok
}

// Each forge gets the header it authenticates with: Basic for git, a
// bearer for GitHub's API, and Basic for Forgejo's API.
//
// @decision(DL-034)
func TestEachHostGetsItsPlatformsCredential(t *testing.T) {
	s, _ := newStore(t, github, forgejo, ghe)
	b := func(user, tok string) string {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+tok))
	}
	for url, want := range map[string]string{
		"https://github.com/o/r.git/info/refs":      b("x-access-token", "ghs_real"),
		"https://api.github.com/repos/o/r/pulls":    "Bearer ghs_real",
		"https://uploads.github.com/repos/o/r/x":    "Bearer ghs_real",
		"https://codefloe.com/o/r.git/info/refs":    b("alice", "f00d"),
		"https://codefloe.com/api/v1/repos/o/r":     b("alice", "f00d"),
		"https://git.example.com/o/r.git/info/refs": b("x-access-token", "ghs_ghe"),
		"https://git.example.com/api/v3/repos/o/r":  "Bearer ghs_ghe",
		"https://GIT.example.com:443/api/graphql":   "Bearer ghs_ghe",
	} {
		got, ok := rewritten(t, s, url)
		if !ok || got != want {
			t.Errorf("%s: %q (added=%v), want %q", url, got, ok, want)
		}
	}
	if got, ok := rewritten(t, s, "https://example.org/x"); ok || got != "" {
		t.Errorf("a host with no platform got %q", got)
	}
}

// A re-mint replaces the file, and the running proxy uses the new token
// on its next request, even when the new token is the same length.
//
// @decision(DL-021) @decision(DL-034)
func TestARemintedTokenIsUsedOnTheNextRequest(t *testing.T) {
	s, path := newStore(t, github, forgejo)
	if err := SetToken(path, "gh", "ghs_next\n"); err != nil {
		t.Fatal(err)
	}
	if got, _ := rewritten(t, s, "https://api.github.com/user"); got != "Bearer ghs_next" {
		t.Errorf("after the re-mint: %q", got)
	}
	if got, _ := rewritten(t, s, "https://codefloe.com/api/v1/user"); !strings.HasPrefix(got, "Basic ") {
		t.Errorf("the other platform lost its credential: %q", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("credentials file is %v", info.Mode().Perm())
	}
}

func TestSetTokenRefusesWhatItCannotApply(t *testing.T) {
	_, path := newStore(t, github)
	for name, c := range map[string]struct{ path, platform, token string }{
		"no token":         {path, "gh", "  \n"},
		"unknown platform": {path, "gitlab", "t"},
		"no file":          {filepath.Join(t.TempDir(), "missing"), "gh", "t"},
	} {
		if err := SetToken(c.path, c.platform, c.token); err == nil {
			t.Errorf("%s: SetToken succeeded", name)
		}
	}
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetToken(path, "gh", "t"); err == nil {
		t.Error("SetToken accepted a corrupt file")
	}
}

func TestAConfigIsValidatedBeforeItIsTrusted(t *testing.T) {
	for name, p := range map[string][]Platform{
		"no name":      {{Kind: KindGitHub, Host: "github.com", Token: "t"}},
		"twice":        {github, github},
		"unknown kind": {{Name: "x", Kind: "gitlab", Host: "gitlab.com", Token: "t"}},
		"bad host":     {{Name: "x", Kind: KindForgejo, Host: "https://x.com", Token: "t"}},
		"no token":     {{Name: "x", Kind: KindForgejo, Host: "x.com", Repos: []string{"o/r"}}},
		"no repos":     {{Name: "x", Kind: KindForgejo, Host: "x.com", Token: "t"}},
	} {
		if _, err := NewStore(filepath.Join(t.TempDir(), "c.json"), SidecarConfig{Platforms: p}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestTheDecryptedHostsAreEveryPlatformsHosts(t *testing.T) {
	got := SidecarConfig{Platforms: []Platform{github, forgejo, github}}.DecryptHosts()
	want := []string{"github.com", "api.github.com", "uploads.github.com", "codefloe.com"}
	if !slices.Equal(got, want) {
		t.Errorf("%v, want %v", got, want)
	}
}

// A placeholder keeps a GitHub token's prefix and nothing else of it.
//
// @decision(DL-034)
func TestAPlaceholderKeepsOnlyThePrefix(t *testing.T) {
	for real, prefix := range map[string]string{
		"ghs_abc123":          "ghs_",
		"github_pat_11AB_xyz": "github_pat_",
		"ghp_secret":          "ghp_",
		"0123456789abcdef":    "",
	} {
		ph, err := Placeholder(real)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(ph, prefix) || strings.Contains(ph, strings.TrimPrefix(real, prefix)) || len(ph) < 40 {
			t.Errorf("Placeholder(%q) = %q", real, ph)
		}
		if again, _ := Placeholder(real); again == ph {
			t.Errorf("two placeholders for %q are the same", real)
		}
	}
}

// The main container is pointed at the proxy in both spellings, and
// OpenCode's own calls and localhost are kept straight.
//
// @decision(DL-034)
func TestTheMainContainerIsPointedAtTheProxy(t *testing.T) {
	env := MainEnv()
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		if env[k] != "http://proxy:3128" {
			t.Errorf("%s = %q", k, env[k])
		}
	}
	for _, k := range []string{"NO_PROXY", "no_proxy"} {
		for _, h := range []string{"localhost", "127.0.0.1", "::1", "main"} {
			if !slices.Contains(strings.Split(env[k], ","), h) {
				t.Errorf("%s lacks %s", k, h)
			}
		}
	}
	if env["NODE_EXTRA_CA_CERTS"] != MainCAFile || env["SSL_CERT_FILE"] != SystemBundle {
		t.Errorf("CA variables: %v", env)
	}
	if got := SetTokenCommand("gh"); !slices.Equal(got, []string{BinaryPath, "set-token", "gh"}) {
		t.Errorf("SetTokenCommand = %v", got)
	}
}

// A platform on another port is keyed by it, and the CA names the host
// without it.
//
// @decision(DL-034)
func TestAPlatformOnAnotherPortKeepsItsPort(t *testing.T) {
	onPort := Platform{Name: "fj", Kind: KindForgejo, Host: "Git.Example.com:3000", User: "u", Token: "t", Repos: []string{"o/r"}}
	on443 := Platform{Name: "gh443", Kind: KindGitHub, Host: "ghe.example.com:443", User: "u", Token: "t", Scoped: true}
	cfg := SidecarConfig{Platforms: []Platform{onPort, on443}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := cfg.DecryptHosts(); !slices.Equal(got, []string{"git.example.com:3000", "ghe.example.com"}) {
		t.Errorf("targets %v", got)
	}
	if got := cfg.CAHosts(); !slices.Equal(got, []string{"git.example.com", "ghe.example.com"}) {
		t.Errorf("CA hosts %v", got)
	}
	s, _ := newStore(t, onPort)
	if got, ok := rewritten(t, s, "https://git.example.com:3000/api/v1/user"); !ok || !strings.HasPrefix(got, "Basic ") {
		t.Errorf(":3000 got %q", got)
	}
	if _, ok := rewritten(t, s, "https://git.example.com/api/v1/user"); ok {
		t.Error("the same host on 443 got the :3000 platform's credential")
	}
	for _, bad := range []string{"host:abc", "a/b", "user@host", ":3000", "a b"} {
		if validHost(bad) {
			t.Errorf("validHost(%q)", bad)
		}
	}
}

// A proxy restarted in the same sidecar keeps what a re-mint wrote.
//
// @decision(DL-021) @decision(DL-034)
func TestARestartedProxyKeepsTheRemintedToken(t *testing.T) {
	_, path := newStore(t, github)
	if err := SetToken(path, "gh", "ghs_reminted"); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(path, SidecarConfig{Platforms: []Platform{github}})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := rewritten(t, s, "https://api.github.com/user"); got != "Bearer ghs_reminted" {
		t.Errorf("after a restart: %q", got)
	}
	if err := os.WriteFile(path, []byte("{}garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err = NewStore(path, SidecarConfig{Platforms: []Platform{github}})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := rewritten(t, s, "https://api.github.com/user"); got != "Bearer ghs_real" {
		t.Errorf("an unreadable file was kept: %q", got)
	}
}
