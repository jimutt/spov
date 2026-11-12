// Package git abstracts the git platforms spov bootstraps repos from.
//
// @decision(DL-041)
package git

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Repo identifies a repository on a platform.
type Repo struct {
	Owner string
	Name  string
	// CloneURL is the HTTPS clone URL, without credentials.
	CloneURL string
	Private  bool
	Archived bool
	// UpdatedAt orders the interactive picker so recent work is first.
	UpdatedAt time.Time
}

// Slug is the "owner/name" identifier.
func (r Repo) Slug() string { return r.Owner + "/" + r.Name }

// Token is a short-lived credential for cloning and pushing.
type Token struct {
	// Username is the git credential username. Forgejo uses the account
	// name; GitHub App installation tokens use "x-access-token".
	Username string
	// Secret is the token value. It must never be logged.
	Secret string
	// ExpiresAt is zero when the platform issues no expiry, in which case
	// the token must be revoked explicitly.
	ExpiresAt time.Time
	// Revoke releases the token. It is nil when the platform expires
	// tokens on its own.
	Revoke func(context.Context) error
}

// renewer is implemented by a platform whose sandbox tokens expire on a
// schedule and can be minted again for a sandbox that is still running.
type renewer interface {
	RenewsTokens() bool
}

// Renewable reports whether the server re-mints this platform's token
// into a running sandbox. Only a GitHub App is: a PAT or a Forgejo token
// is the stored secret itself.
//
// @decision(DL-021)
func Renewable(p Platform) bool {
	r, ok := p.(renewer)
	return ok && r.RenewsTokens()
}

// wideIssuer is implemented by a platform that can hand a sandbox a
// token for every repository its credential reaches.
type wideIssuer interface {
	IssueWideToken(ctx context.Context, ttl time.Duration) (Token, error)
}

// IssueFor mints the token a selection needs: one for its repositories,
// or, when the selection is All, one for every repository the
// credential reaches.
//
// @decision(DL-043)
func IssueFor(ctx context.Context, sel Selection, ttl time.Duration) (Token, error) {
	if !sel.All {
		return sel.Platform.IssueSandboxToken(ctx, sel.Repos, ttl)
	}
	w, ok := sel.Platform.(wideIssuer)
	if !ok {
		return Token{}, fmt.Errorf("git: %s cannot issue a token for every repository", sel.Platform.Name())
	}
	return w.IssueWideToken(ctx, ttl)
}

// Expired reports whether the token is known to have expired.
func (t Token) Expired(now time.Time) bool {
	return !t.ExpiresAt.IsZero() && !now.Before(t.ExpiresAt)
}

// RepoChecker is a platform that can say, before a launch, whether its
// credential can see a repository. CheckRepo returns ErrRepoUnreachable
// when the forge says it cannot, and nil when it can or when the forge's
// answer settles nothing.
//
// @decision(DL-043)
type RepoChecker interface {
	CheckRepo(ctx context.Context, repo Repo) error
}

// repoCheckTimeout bounds one repository check, so a slow forge API does
// not hold up a launch.
const repoCheckTimeout = 5 * time.Second

// ErrRepoUnreachable is a repository the platform's credential cannot
// see.
//
// @decision(DL-043)
var ErrRepoUnreachable = errors.New("the credential cannot see this repository")

// Platform is one configured git account.
//
// @decision(DL-041)
type Platform interface {
	// Name is the configured platform name, used as the credential key.
	Name() string
	// Host is the hostname git credentials are bound to.
	Host() string
	// Kind identifies the platform's API dialect ("forgejo", "github"),
	// which decides the forge CLI the sandbox should use.
	//
	// @decision(DL-032)
	Kind() string
	// ListRepos returns the repositories the account can access.
	ListRepos(ctx context.Context) ([]Repo, error)
	// IssueSandboxToken mints a credential for the given repos. Platforms
	// that cannot scope to individual repos ignore the argument and say
	// so through TokenScope.
	IssueSandboxToken(ctx context.Context, repos []Repo, ttl time.Duration) (Token, error)
	// TokenScope describes what IssueSandboxToken actually grants, so the
	// CLI can tell the user before it mints anything.
	TokenScope() Scope
	// FindPullRequest reports the best pull request in any state whose
	// source is branch (open, then merged, then closed), if there is one;
	// it is gate 2 of a pr job's verdict. The
	// branch is required and is never guessed. Three answers, kept
	// distinct: a pull request and nil (gate 2 passes), nil and nil (the
	// query found none, gate 2 fails), and ErrUnsupported (this platform
	// cannot answer, gate 2 is unverified). Any other error also reads as
	// unverified.
	//
	// @decision(DL-050)
	FindPullRequest(ctx context.Context, repo RepoRef, branch string) (*PullRequest, error)
}

// RepoRef names one repository on this platform.
//
// @decision(DL-050)
type RepoRef struct {
	Owner string
	Name  string
}

// PullRequest is the little a verdict needs to know about one: that it
// exists, and where a human can go to look at it.
//
// @decision(DL-050)
type PullRequest struct {
	Number int
	URL    string
	Title  string
	// State is the forge's own word, kept verbatim so a reader sees
	// what the forge said rather than spov's translation of it.
	State string
	// Open and Merged are what gate 2 decides on: a pull request that is
	// neither was closed without merging.
	//
	// @decision(DL-051)
	Open   bool
	Merged bool
	// Created is when the forge says it was opened, zero when unknown.
	Created time.Time
}

// OwnURL is raw when it is an address of the forge at forgeURL: https,
// or http when the forge itself is configured with http, on the forge's
// own host, with no user information. Anything else is "" so a forge's
// answer cannot put another scheme or site in a link.
//
// @decision(DL-135)
func OwnURL(raw, forgeURL string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil {
		return ""
	}
	f, err := url.Parse(forgeURL)
	if err != nil || f.Host == "" || !strings.EqualFold(u.Host, f.Host) {
		return ""
	}
	if u.Scheme == "https" || (u.Scheme == "http" && f.Scheme == "http") {
		return raw
	}
	return ""
}

// rank orders a branch's pull requests for gate 2: open, then merged,
// then closed.
//
// @decision(DL-051)
func (p PullRequest) rank() int {
	switch {
	case p.Open:
		return 3
	case p.Merged:
		return 2
	default:
		return 1
	}
}

// bestPullRequest is the better of a and b, keeping a on a tie, so a
// forge's newest-first order decides between equals.
//
// @decision(DL-051)
func bestPullRequest(a *PullRequest, b PullRequest) *PullRequest {
	if a == nil || b.rank() > a.rank() {
		return &b
	}
	return a
}

// Scope describes the blast radius of a token a platform issues.
//
// @decision(DL-041)
type Scope struct {
	// PerRepo is true when the token is limited to the requested repos.
	PerRepo bool
	// Description is a one-line summary for the user, e.g.
	// "read/write on all repositories of the account".
	Description string
}

// ErrNoRepos means the account has no repositories to bootstrap from.
var ErrNoRepos = errors.New("git: no repositories available")

// ErrUnsupported is a platform that cannot answer a query at all, as
// opposed to one that answered and found nothing.
//
// @decision(DL-050)
var ErrUnsupported = errors.New("git: the platform does not support this query")

// CloneURLWithToken embeds a credential in an HTTPS clone URL, handed to
// git inside the sandbox. It carries a secret, so callers must not log it.
//
// @decision(DL-041)
func CloneURLWithToken(cloneURL string, tok Token) (string, error) {
	const prefix = "https://"
	if !strings.HasPrefix(cloneURL, prefix) {
		return "", fmt.Errorf("git: clone URL %q is not https", cloneURL)
	}
	if tok.Username == "" || tok.Secret == "" {
		return "", fmt.Errorf("git: token is incomplete")
	}
	if strings.ContainsAny(tok.Username, ":@/") {
		return "", fmt.Errorf("git: token username contains a character that cannot appear in a URL")
	}
	if strings.ContainsAny(tok.Secret, ":@/") {
		return "", fmt.Errorf("git: token secret contains a character that cannot appear in a URL")
	}
	return prefix + tok.Username + ":" + tok.Secret + "@" + strings.TrimPrefix(cloneURL, prefix), nil
}

// RedactURL removes the credential from a single URL so it can be logged.
// Input that is not a URL is returned unchanged. For free text that may
// contain several URLs, use RedactAll instead.
//
// @decision(DL-041)
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	if _, hasPassword := u.User.Password(); !hasPassword {
		return raw
	}
	u.User = url.UserPassword(u.User.Username(), "***")
	// url.URL.String() escapes the redaction marker; put it back so the
	// output stays readable.
	return strings.ReplaceAll(u.String(), "%2A%2A%2A", "***")
}

// credentialURL matches an http(s) URL carrying a user:password pair. It
// deliberately stops at whitespace and quotes so it does not swallow
// surrounding log text.
var credentialURL = regexp.MustCompile(`(https?://)([^\s/@:]+):([^\s/@]+)@`)

// RedactAll removes credentials from every URL in a block of text, so
// command output and error messages can be logged safely.
//
// @decision(DL-041)
func RedactAll(text string) string {
	return credentialURL.ReplaceAllString(text, "${1}${2}:***@")
}
