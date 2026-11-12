package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// GitHubAccount is one GitHub account as an allowlist entry records it.
// Login and Name are labels: the ID is the identity.
//
// @decision(DL-128)
type GitHubAccount struct {
	ID    int64
	Login string
	Name  string
}

// AccountLookup resolves a GitHub login to its account. The real one is
// GitHubAccounts.Lookup; tests pass a fake.
//
// @decision(DL-128)
type AccountLookup func(ctx context.Context, login string) (GitHubAccount, error)

// The lookup's refusals. Each is a different answer for the person
// adding an entry.
//
// @decision(DL-128)
var (
	ErrNoSuchAccount     = errors.New("auth: github: no account has that login")
	ErrNotAPersonAccount = errors.New("auth: github: that login is an organisation's account")
	ErrGitHubRateLimited = errors.New("auth: github: rate limited")
	ErrGitHubUnavailable = errors.New("auth: github: could not be asked")
)

// LookupTimeout bounds one account lookup.
//
// @decision(DL-128)
const LookupTimeout = 10 * time.Second

// SeedLookupTimeout bounds every lookup a seed file's import makes,
// together, so a server never hangs at boot on an unreachable GitHub.
//
// @decision(DL-128)
const SeedLookupTimeout = 30 * time.Second

// maxLookupBody is the most of a profile the lookup reads.
const maxLookupBody = 64 << 10

// MaxGitHubLogin is GitHub's limit on a login's length.
//
// @decision(DL-128)
const MaxGitHubLogin = 39

// maxLabel bounds a stored display name, in runes.
const maxLabel = 255

// ParseGitHubLogin checks a login against GitHub's rules (1 to 39
// letters, digits or single hyphens, neither first nor last) and
// returns it without a leading "@". Nothing else may reach a URL or the
// store as a login.
//
// @decision(DL-128)
func ParseGitHubLogin(s string) (string, error) {
	login := strings.TrimPrefix(strings.TrimSpace(s), "@")
	if !ValidGitHubLogin(login) {
		return "", fmt.Errorf("%q is not a GitHub username: it has 1 to %d letters, digits or single hyphens,"+
			" and does not start or end with a hyphen", s, MaxGitHubLogin)
	}
	return login, nil
}

// ValidGitHubLogin reports whether s has the shape of a GitHub login.
//
// @decision(DL-128)
func ValidGitHubLogin(s string) bool {
	if s == "" || len(s) > MaxGitHubLogin || s[0] == '-' || s[len(s)-1] == '-' || strings.Contains(s, "--") {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

// CleanLabel makes a display name GitHub returned safe to store and
// show: control and format characters go, and it is cut to a bounded
// length. It is still shown only as text.
//
// @decision(DL-128)
func CleanLabel(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	var b strings.Builder
	n := 0
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		if n == maxLabel {
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// GitHubAccounts asks GitHub's REST API who a login is. It is
// unauthenticated: a public profile needs no token.
//
// @decision(DL-128)
type GitHubAccounts struct {
	// APIURL defaults to GitHubUserAPI, the API the login reads.
	APIURL string
	// HTTP is the client; nil is a plain one. Redirects to another
	// host are refused either way.
	HTTP *http.Client
}

// Lookup resolves login with one GET /users/{login}, under
// LookupTimeout. It checks the login's shape before any request, so the
// text can only ever name a path segment under the API root.
//
// @decision(DL-128)
func (g GitHubAccounts) Lookup(ctx context.Context, login string) (GitHubAccount, error) {
	login, err := ParseGitHubLogin(login)
	if err != nil {
		return GitHubAccount{}, err
	}
	base := g.APIURL
	if base == "" {
		base = GitHubUserAPI
	}
	root, err := url.Parse(base)
	if err != nil || root.Host == "" {
		return GitHubAccount{}, fmt.Errorf("%w: the API address %q is not a URL", ErrGitHubUnavailable, base)
	}
	ctx, cancel := context.WithTimeout(ctx, LookupTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimSuffix(base, "/")+"/users/"+url.PathEscape(login), nil)
	if err != nil {
		return GitHubAccount{}, fmt.Errorf("%w: %w", ErrGitHubUnavailable, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	client := http.Client{}
	if g.HTTP != nil {
		client = *g.HTTP
	}
	client.CheckRedirect = sameHostRedirect(root)
	resp, err := client.Do(req)
	if err != nil {
		return GitHubAccount{}, fmt.Errorf("%w: %w", ErrGitHubUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return GitHubAccount{}, ErrNoSuchAccount
	case http.StatusForbidden, http.StatusTooManyRequests:
		return GitHubAccount{}, ErrGitHubRateLimited
	default:
		return GitHubAccount{}, fmt.Errorf("%w: %s", ErrGitHubUnavailable, resp.Status)
	}
	var profile struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
		Type  string `json:"type"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxLookupBody)).Decode(&profile); err != nil {
		return GitHubAccount{}, fmt.Errorf("%w: the answer could not be read", ErrGitHubUnavailable)
	}
	if profile.ID <= 0 || !ValidGitHubLogin(profile.Login) {
		return GitHubAccount{}, fmt.Errorf("%w: the answer named no account", ErrGitHubUnavailable)
	}
	if profile.Type != "User" {
		return GitHubAccount{}, ErrNotAPersonAccount
	}
	return GitHubAccount{ID: profile.ID, Login: profile.Login, Name: CleanLabel(profile.Name)}, nil
}

// sameHostRedirect follows a redirect only on the API's own scheme and
// host, and at most three.
func sameHostRedirect(root *url.URL) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != root.Scheme || req.URL.Host != root.Host {
			return fmt.Errorf("refusing a redirect to %s", req.URL.Host)
		}
		if len(via) >= 3 {
			return errors.New("too many redirects")
		}
		return nil
	}
}

// ResolveAccount is who a new "@login" entry admits: what GitHub answers
// for the login now. A login stored on a user row is never used for it,
// since a renamed account leaves its old login there and someone else
// may have registered it since.
//
// @decision(DL-128)
func ResolveAccount(ctx context.Context, lookup AccountLookup, login string) (GitHubAccount, error) {
	login, err := ParseGitHubLogin(login)
	if err != nil {
		return GitHubAccount{}, err
	}
	if lookup == nil {
		return GitHubAccount{}, fmt.Errorf("%w: this server cannot look up GitHub accounts", ErrGitHubUnavailable)
	}
	return lookup(ctx, login)
}
