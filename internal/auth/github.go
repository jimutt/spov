package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/github"
)

// GitHubUserAPI is the REST root the login flow reads the profile from.
const GitHubUserAPI = "https://api.github.com"

// GitHubScopes are what login asks for: the profile and the e-mail
// addresses. A user's e-mail may be private, in which case GET /user
// returns none and only /user/emails has it, so the scope is required
// rather than a convenience.
//
// @decision(DL-007)
var GitHubScopes = []string{"read:user", "user:email"}

// GitHubIdentity is what a completed login proves.
//
// @decision(DL-007) @decision(DL-128)
type GitHubIdentity struct {
	// ID is the account's numeric ID, which is the identity; Login and
	// Name are labels.
	//
	// @decision(DL-128)
	ID int64
	// Email is the address that was both primary and verified.
	Email string
	Login string
	Name  string
}

// GitHubOAuth exchanges an authorization code for the identity behind
// it. It is the only part of login that talks to GitHub.
//
// @decision(DL-007)
type GitHubOAuth struct {
	Config oauth2.Config
	// APIURL defaults to GitHubUserAPI; tests point it at a stub.
	APIURL string
	// HTTP is the client used for the profile calls and, through the
	// context, for the token exchange. Nil means http.DefaultClient.
	HTTP *http.Client
}

// NewGitHubOAuth builds the flow for an OAuth app. redirectURL must be
// the callback registered with GitHub, on the public origin.
//
// @decision(DL-007)
func NewGitHubOAuth(clientID, clientSecret, redirectURL string) *GitHubOAuth {
	return &GitHubOAuth{Config: oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		Endpoint:     github.Endpoint,
		Scopes:       GitHubScopes,
	}}
}

// AuthCodeURL is where the browser is sent. state is the nonce the
// callback must return; verifier is the PKCE secret kept server-side.
//
// @decision(DL-007)
func (g *GitHubOAuth) AuthCodeURL(state, verifier string) string {
	return g.Config.AuthCodeURL(state, oauth2.AccessTypeOnline, oauth2.S256ChallengeOption(verifier))
}

// Exchange completes the flow: it trades the code for a token and reads
// the identity behind it. The returned identity always carries a primary
// and verified e-mail; anything else is an error.
//
// @decision(DL-007)
func (g *GitHubOAuth) Exchange(ctx context.Context, code, verifier string) (GitHubIdentity, error) {
	if g.HTTP != nil {
		ctx = context.WithValue(ctx, oauth2.HTTPClient, g.HTTP)
	}
	token, err := g.Config.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		// The error carries GitHub's response body, which for a bad
		// exchange names no secret, but the client secret is in the
		// request; only the reason travels.
		return GitHubIdentity{}, fmt.Errorf("auth: github: the code exchange failed: %s", exchangeReason(err))
	}
	client := g.Config.Client(ctx, token)

	var profile struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
	}
	if err := g.get(ctx, client, "/user", &profile); err != nil {
		return GitHubIdentity{}, err
	}
	// The ID is who the person is, and the login is printed on pages
	// and in logs, so an answer without either in shape is refused.
	//
	// @decision(DL-128)
	if profile.ID <= 0 || !ValidGitHubLogin(profile.Login) {
		return GitHubIdentity{}, errors.New("auth: github: /user named no account")
	}
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := g.get(ctx, client, "/user/emails", &emails); err != nil {
		return GitHubIdentity{}, err
	}
	// Only primary *and* verified counts. GitHub lets anyone add an
	// unverified address, so trusting any other would turn the
	// allowlist into an open door.
	for _, e := range emails {
		if e.Primary && e.Verified {
			return GitHubIdentity{
				ID: profile.ID, Email: normalizeEmail(e.Email), Login: profile.Login, Name: CleanLabel(profile.Name),
			}, nil
		}
	}
	return GitHubIdentity{}, ErrNoVerifiedEmail
}

// ErrNoVerifiedEmail is a GitHub account whose primary address is not
// verified. The login page says so, since the user can fix it.
var ErrNoVerifiedEmail = errors.New("auth: github: no primary, verified e-mail address")

func (g *GitHubOAuth) get(ctx context.Context, client *http.Client, path string, into any) error {
	base := g.APIURL
	if base == "" {
		base = GitHubUserAPI
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return fmt.Errorf("auth: github: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("auth: github: %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// The body may echo the token in an error message on some
		// proxies, so only the status travels.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("auth: github: %s: %s", path, resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(into); err != nil {
		return fmt.Errorf("auth: github: %s: %w", path, err)
	}
	return nil
}

// exchangeReason reduces an oauth2 error to its reason. The full error
// renders the request, and the request carries the client secret.
func exchangeReason(err error) string {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		if re.ErrorCode != "" {
			return re.ErrorCode
		}
		return strings.TrimSpace(re.Response.Status)
	}
	return err.Error()
}
