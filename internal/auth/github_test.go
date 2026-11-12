package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

// githubStub is a GitHub good enough to complete a login: it exchanges
// one code for one token and serves the two profile endpoints.
type githubStub struct {
	*httptest.Server
	// emails is what GET /user/emails returns.
	emails []map[string]any
	// login and name are what GET /user returns.
	login, name string
	// wantVerifier, when set, is the PKCE verifier the exchange must
	// carry.
	wantVerifier string
	// exchangeStatus, when set, fails the exchange with this status.
	exchangeStatus int
	// gotChallenge records the PKCE challenge the exchange presented.
	gotChallenge string
	// userCalls counts the profile requests, so a test can prove the
	// token was actually used.
	userCalls int
}

func newGitHubStub(t *testing.T) *githubStub {
	t.Helper()
	s := &githubStub{
		login: "octocat", name: "The Octocat",
		emails: []map[string]any{
			{"email": "other@example.com", "primary": false, "verified": true},
			{"email": "Octo@Example.com", "primary": true, "verified": true},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		if s.exchangeStatus != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(s.exchangeStatus)
			_, _ = w.Write([]byte(`{"error":"bad_verification_code"}`))
			return
		}
		_ = r.ParseForm()
		s.gotChallenge = r.Form.Get("code_verifier")
		if s.wantVerifier != "" && s.gotChallenge != s.wantVerifier {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"gho_stub","token_type":"bearer"}`))
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		s.userCalls++
		// The token must actually be presented, or the flow would be
		// reading a profile it never proved it owns.
		if !strings.Contains(r.Header.Get("Authorization"), "gho_stub") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 583231, "login": s.login, "name": s.name})
	})
	mux.HandleFunc("GET /user/emails", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(s.emails)
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// oauthFor points a GitHubOAuth at the stub.
func (s *githubStub) oauthFor() *GitHubOAuth {
	g := NewGitHubOAuth("client-id", "client-secret", "https://spov.example.com"+"/oauth/github/callback")
	g.Config.Endpoint = oauth2.Endpoint{
		AuthURL:   s.URL + "/login/oauth/authorize",
		TokenURL:  s.URL + "/login/oauth/access_token",
		AuthStyle: oauth2.AuthStyleInParams,
	}
	g.APIURL = s.URL
	g.HTTP = s.Client()
	return g
}

// TestExchangeReturnsThePrimaryVerifiedEmail: the address that is both
// primary and verified is the identity, not the first one listed.
//
// @decision(DL-007)
func TestExchangeReturnsThePrimaryVerifiedEmail(t *testing.T) {
	stub := newGitHubStub(t)
	got, err := stub.oauthFor().Exchange(context.Background(), "the-code", oauth2.GenerateVerifier())
	if err != nil {
		t.Fatal(err)
	}
	// Lowercased, so the allowlist and the user row agree on one
	// spelling.
	if got.Email != "octo@example.com" {
		t.Errorf("email = %q", got.Email)
	}
	if got.Login != "octocat" || got.Name != "The Octocat" {
		t.Errorf("identity = %+v", got)
	}
	if stub.userCalls == 0 {
		t.Error("the profile was never read")
	}
}

// TestExchangeRefusesAnythingButPrimaryAndVerified is the rule that
// separates an allowlist from an open door: GitHub lets anyone add an
// unverified address.
//
// @decision(DL-007)
func TestExchangeRefusesAnythingButPrimaryAndVerified(t *testing.T) {
	for _, tc := range []struct {
		name   string
		emails []map[string]any
	}{
		{"primary but unverified", []map[string]any{
			{"email": "attacker@example.com", "primary": true, "verified": false},
		}},
		{"verified but not primary", []map[string]any{
			{"email": "attacker@example.com", "primary": false, "verified": true},
		}},
		{"neither", []map[string]any{
			{"email": "attacker@example.com", "primary": false, "verified": false},
		}},
		{"none at all", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newGitHubStub(t)
			stub.emails = tc.emails
			_, err := stub.oauthFor().Exchange(context.Background(), "the-code", oauth2.GenerateVerifier())
			if !errors.Is(err, ErrNoVerifiedEmail) {
				t.Errorf("err = %v, want ErrNoVerifiedEmail", err)
			}
		})
	}
}

// TestExchangeSendsThePKCEVerifier: without it the authorization code
// alone is enough for anyone who intercepts it.
//
// @decision(DL-007)
func TestExchangeSendsThePKCEVerifier(t *testing.T) {
	stub := newGitHubStub(t)
	verifier := oauth2.GenerateVerifier()
	stub.wantVerifier = verifier
	if _, err := stub.oauthFor().Exchange(context.Background(), "the-code", verifier); err != nil {
		t.Fatal(err)
	}
	if stub.gotChallenge != verifier {
		t.Errorf("the exchange sent code_verifier %q, want %q", stub.gotChallenge, verifier)
	}
}

// TestAuthCodeURLCarriesStateAndAnS256Challenge.
//
// @decision(DL-007)
func TestAuthCodeURLCarriesStateAndAnS256Challenge(t *testing.T) {
	stub := newGitHubStub(t)
	verifier := oauth2.GenerateVerifier()
	raw := stub.oauthFor().AuthCodeURL("the-state", verifier)
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("state") != "the-state" {
		t.Errorf("state = %q", q.Get("state"))
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Errorf("code_challenge_method = %q, want S256", q.Get("code_challenge_method"))
	}
	// The challenge is the hash, never the verifier itself.
	if c := q.Get("code_challenge"); c == "" || c == verifier {
		t.Errorf("code_challenge = %q", c)
	}
	if scope := q.Get("scope"); !strings.Contains(scope, "user:email") {
		t.Errorf("scope = %q, want it to include user:email", scope)
	}
}

// TestExchangeErrorNamesNoSecret: the oauth2 error renders the request,
// and the request carries the client secret.
//
// @decision(DL-007)
func TestExchangeErrorNamesNoSecret(t *testing.T) {
	stub := newGitHubStub(t)
	stub.exchangeStatus = http.StatusUnauthorized
	_, err := stub.oauthFor().Exchange(context.Background(), "the-code", oauth2.GenerateVerifier())
	if err == nil {
		t.Fatal("a rejected exchange succeeded")
	}
	if strings.Contains(err.Error(), "client-secret") {
		t.Errorf("the error carries the client secret: %v", err)
	}
	// It still says enough to diagnose.
	if !strings.Contains(err.Error(), "bad_verification_code") {
		t.Errorf("error %q does not carry GitHub's reason", err)
	}
}

// TestExchangeReportsAProfileFailure rather than logging someone in with
// an empty identity.
//
// @decision(DL-007)
func TestExchangeReportsAProfileFailure(t *testing.T) {
	stub := newGitHubStub(t)
	// The exchange still succeeds; only the profile read fails, so the
	// error must come from there rather than from the token endpoint.
	g := stub.oauthFor()
	g.APIURL = stub.URL + "/gone"
	_, err := g.Exchange(context.Background(), "the-code", oauth2.GenerateVerifier())
	if err == nil {
		t.Fatal("a failed profile read succeeded")
	}
	if strings.Contains(err.Error(), "code exchange") {
		t.Fatalf("the exchange failed rather than the profile read: %v", err)
	}
	if !strings.Contains(err.Error(), "/user") {
		t.Errorf("error %q does not name what failed", err)
	}
	// A login must not proceed on an unread profile.
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error %q does not carry the status", err)
	}
}

// TestExchangeReportsAFailedEmailRead: the profile succeeded but the
// addresses did not, which must fail rather than log someone in with no
// e-mail.
//
// @decision(DL-007)
func TestExchangeReportsAFailedEmailRead(t *testing.T) {
	stub := newGitHubStub(t)
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 583231, "login": "octocat"})
			return
		}
		// Malformed JSON for /user/emails: a proxy or an outage can
		// produce this, and it must not decode to "no addresses".
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{not json`))
	}))
	t.Cleanup(failing.Close)

	g := stub.oauthFor()
	g.APIURL = failing.URL
	g.HTTP = failing.Client()
	_, err := g.Exchange(context.Background(), "the-code", oauth2.GenerateVerifier())
	if err == nil {
		t.Fatal("a malformed e-mail listing succeeded")
	}
	if errors.Is(err, ErrNoVerifiedEmail) {
		t.Errorf("a read failure was reported as a missing address: %v", err)
	}
	if !strings.Contains(err.Error(), "/user/emails") {
		t.Errorf("error %q does not name what failed", err)
	}
}

// TestGitHubOAuthDefaultsToTheRealAPI when no override is given.
//
// @decision(DL-007)
func TestGitHubOAuthDefaultsToTheRealAPI(t *testing.T) {
	g := NewGitHubOAuth("id", "secret", "https://spov.example.com/oauth/github/callback")
	if g.APIURL != "" {
		t.Errorf("APIURL = %q, want empty so the default applies", g.APIURL)
	}
	// The default is what get() falls back to.
	if GitHubUserAPI != "https://api.github.com" {
		t.Errorf("GitHubUserAPI = %q", GitHubUserAPI)
	}
	if g.Config.Endpoint.AuthURL == "" || g.Config.Endpoint.TokenURL == "" {
		t.Error("the GitHub endpoint was not configured")
	}
}
