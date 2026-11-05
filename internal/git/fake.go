package git

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"
)

// Fake is an in-memory Platform for tests.
//
// @decision(DL-041)
type Fake struct {
	mu sync.Mutex

	PlatformName string
	HostName     string
	PlatformKind string
	Repos        []Repo
	Scope        Scope

	// ListErr and IssueErr force the corresponding call to fail.
	ListErr  error
	IssueErr error
	// PullRequests are the open pull requests this platform will report,
	// keyed by "owner/name@branch". A branch with no entry answers "no
	// pull request", which is what gate 2 reads as a failed deliverable.
	//
	// @decision(DL-050)
	PullRequests map[string]PullRequest
	// PullErr forces FindPullRequest to fail. Set it to ErrUnsupported
	// to stand in for a platform that cannot answer at all, which gate 2
	// reads as unverified rather than failed.
	PullErr error
	// RevokeErr fails a token's Revoke.
	RevokeErr error
	// Renews makes this platform renewable, as a GitHub App is.
	//
	// @decision(DL-021)
	Renews bool

	// Issued records every token minted, in order.
	issued  []issuedToken
	revoked int
	seq     int
}

type issuedToken struct {
	Repos []Repo
	TTL   time.Duration
	Wide  bool
}

// NewFake returns a Fake with one repository.
func NewFake() *Fake {
	return &Fake{
		PlatformName: "fake",
		HostName:     "git.example.com",
		Repos: []Repo{{
			Owner: "acme", Name: "app",
			CloneURL:  "https://git.example.com/acme/app.git",
			UpdatedAt: time.Now(),
		}},
		Scope: Scope{PerRepo: true, Description: "the requested repositories"},
	}
}

func (f *Fake) Name() string { return f.PlatformName }
func (f *Fake) Host() string { return f.HostName }

func (f *Fake) Kind() string {
	if f.PlatformKind == "" {
		return "forgejo"
	}
	return f.PlatformKind
}

func (f *Fake) TokenScope() Scope { return f.Scope }

// FindPullRequest answers from PullRequests.
//
// @decision(DL-050)
func (f *Fake) FindPullRequest(ctx context.Context, repo RepoRef, branch string) (*PullRequest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.PullErr != nil {
		return nil, f.PullErr
	}
	if branch == "" {
		return nil, nil
	}
	pr, ok := f.PullRequests[fmt.Sprintf("%s/%s@%s", repo.Owner, repo.Name, branch)]
	if !ok {
		return nil, nil
	}
	// One a test gave no state, or an open forge's word, is open.
	if (pr.State == "" && !pr.Merged) || pr.State == "open" || pr.State == "opened" {
		pr.Open = true
	}
	return &pr, nil
}

func (f *Fake) ListRepos(ctx context.Context) ([]Repo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ListErr != nil {
		return nil, f.ListErr
	}
	if len(f.Repos) == 0 {
		return nil, ErrNoRepos
	}
	return slices.Clone(f.Repos), nil
}

// RenewsTokens reports the Renews field.
func (f *Fake) RenewsTokens() bool { return f.Renews }

func (f *Fake) IssueSandboxToken(ctx context.Context, repos []Repo, ttl time.Duration) (Token, error) {
	return f.issue(ctx, issuedToken{Repos: slices.Clone(repos), TTL: ttl})
}

// IssueWideToken mints as IssueSandboxToken does, recorded as wide.
//
// @decision(DL-043)
func (f *Fake) IssueWideToken(ctx context.Context, ttl time.Duration) (Token, error) {
	return f.issue(ctx, issuedToken{TTL: ttl, Wide: true})
}

func (f *Fake) issue(ctx context.Context, t issuedToken) (Token, error) {
	if err := ctx.Err(); err != nil {
		return Token{}, err
	}
	ttl := t.TTL
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issued = append(f.issued, t)
	if f.IssueErr != nil {
		return Token{}, f.IssueErr
	}

	f.seq++
	expires := time.Time{}
	if ttl > 0 {
		expires = time.Now().Add(ttl)
	}
	return Token{
		Username:  "fake-user",
		Secret:    fmt.Sprintf("fake-secret-%d", f.seq),
		ExpiresAt: expires,
		Revoke: func(ctx context.Context) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.RevokeErr != nil {
				return f.RevokeErr
			}
			f.revoked++
			return nil
		},
	}, nil
}

// Requested returns the repo sets a token was asked for, in order,
// including attempts that failed.
func (f *Fake) Requested() [][]Repo {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]Repo, len(f.issued))
	for i, t := range f.issued {
		out[i] = slices.Clone(t.Repos)
	}
	return out
}

// Wide counts the tokens asked for every repository, including
// attempts that failed.
func (f *Fake) Wide() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, t := range f.issued {
		if t.Wide {
			n++
		}
	}
	return n
}

// Revoked counts successful token revocations.
func (f *Fake) Revoked() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.revoked
}

var _ Platform = (*Fake)(nil)
