package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// @decision(DL-128)
func TestParseGitHubLogin(t *testing.T) {
	for _, ok := range []string{"bob", "@bob", "  @Bob-Smith ", "a", strings.Repeat("x", MaxGitHubLogin), "0-1"} {
		if _, err := ParseGitHubLogin(ok); err != nil {
			t.Errorf("%q was refused: %v", ok, err)
		}
	}
	if got, _ := ParseGitHubLogin("@Bob"); got != "Bob" {
		t.Errorf("the @ was kept, or the case changed: %q", got)
	}
	for _, bad := range []string{
		"", "@", "-bob", "bob-", "bo--b", "bob smith", "bob/../users", "bob%2F..", "bob?x=1", "bob#x",
		"b.ob", "bob_", "évé", "@@bob", strings.Repeat("x", MaxGitHubLogin+1), "api.github.com", "bo\nb",
	} {
		if got, err := ParseGitHubLogin(bad); err == nil {
			t.Errorf("%q was accepted as %q", bad, got)
		}
	}
}

// @decision(DL-128)
func TestCleanLabelDropsControlCharactersAndBoundsTheLength(t *testing.T) {
	got := CleanLabel("Bob\x1b[31m Smith\u202e\u0000\r\n")
	if got != "Bob[31m Smith" {
		t.Errorf("got %q", got)
	}
	if n := len([]rune(CleanLabel(strings.Repeat("é", 1000)))); n != maxLabel {
		t.Errorf("a long name kept %d runes, want %d", n, maxLabel)
	}
	if got := CleanLabel("\xff\xfeAda"); got != "Ada" {
		t.Errorf("invalid UTF-8 became %q", got)
	}
}

// githubUsers is a stub of GET /users/{login}.
func githubUsers(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		handler(w, r)
	}))
	t.Cleanup(s.Close)
	return s, &calls
}

// @decision(DL-128)
func TestTheLookupReadsTheAccountsIDLoginAndName(t *testing.T) {
	var path string
	s, _ := githubUsers(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{"id": 42, "login": "Bob", "name": "Bob\u001b Smith", "type": "User"}`))
	})
	got, err := GitHubAccounts{APIURL: s.URL, HTTP: s.Client()}.Lookup(context.Background(), "@bob")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != 42 || got.Login != "Bob" || got.Name != "Bob Smith" {
		t.Errorf("got %+v", got)
	}
	if path != "/users/bob" {
		t.Errorf("asked %q", path)
	}
}

// Each answer GitHub can give maps to its own error, so the API can say
// which one it was.
//
// @decision(DL-128)
func TestTheLookupTellsItsRefusalsApart(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"unknown", http.StatusNotFound, `{"message":"Not Found"}`, ErrNoSuchAccount},
		{"an organisation", http.StatusOK, `{"id": 9, "login": "acme", "type": "Organization"}`, ErrNotAPersonAccount},
		{"rate limited", http.StatusForbidden, `{"message":"API rate limit exceeded"}`, ErrGitHubRateLimited},
		{"too many requests", http.StatusTooManyRequests, ``, ErrGitHubRateLimited},
		{"an outage", http.StatusBadGateway, ``, ErrGitHubUnavailable},
		{"no ID", http.StatusOK, `{"login": "bob", "type": "User"}`, ErrGitHubUnavailable},
		{"a login of another shape", http.StatusOK, `{"id": 3, "login": "../x", "type": "User"}`, ErrGitHubUnavailable},
		{"not JSON", http.StatusOK, `<html>`, ErrGitHubUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := githubUsers(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			_, err := GitHubAccounts{APIURL: s.URL, HTTP: s.Client()}.Lookup(context.Background(), "bob")
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// The login's text can never point the lookup anywhere: a malformed one
// is refused before a request, and a redirect to another host is not
// followed.
//
// @decision(DL-128)
func TestTheLookupCannotBePointedAtAnotherHost(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		elsewhere.Add(1)
		_, _ = w.Write([]byte(`{"id": 1, "login": "mallory", "type": "User"}`))
	}))
	t.Cleanup(other.Close)
	s, calls := githubUsers(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/users/mallory", http.StatusFound)
	})
	lookup := GitHubAccounts{APIURL: s.URL, HTTP: s.Client()}
	for _, bad := range []string{"../../evil", "bob@evil.example", "bob/x", "%2e%2e", "evil.example:80"} {
		if _, err := lookup.Lookup(context.Background(), bad); err == nil {
			t.Errorf("%q was looked up", bad)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("a malformed login reached GitHub %d times", calls.Load())
	}
	if _, err := lookup.Lookup(context.Background(), "bob"); !errors.Is(err, ErrGitHubUnavailable) {
		t.Errorf("a redirect to another host = %v", err)
	}
	if elsewhere.Load() != 0 {
		t.Error("the lookup followed a redirect to another host")
	}
}

// A body larger than any profile is not read whole.
//
// @decision(DL-128)
func TestTheLookupReadsABoundedBody(t *testing.T) {
	s, _ := githubUsers(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id": 5, "login": "bob", "type": "User", "bio": "`))
		_, _ = w.Write([]byte(strings.Repeat("x", 2*maxLookupBody)))
		_, _ = w.Write([]byte(`"}`))
	})
	if _, err := (GitHubAccounts{APIURL: s.URL, HTTP: s.Client()}).Lookup(context.Background(), "bob"); !errors.Is(err, ErrGitHubUnavailable) {
		t.Errorf("an oversized answer = %v, want it refused", err)
	}
}

// A new entry's account is GitHub's answer for the login, even when a
// user row still carries that login: a renamed former member leaves it
// there, and someone else may have registered it since.
//
// @decision(DL-128)
func TestResolveAccountAlwaysAsksGitHub(t *testing.T) {
	ctx := context.Background()
	asked := 0
	lookup := func(_ context.Context, login string) (GitHubAccount, error) {
		asked++
		return GitHubAccount{ID: 2, Login: login}, nil
	}
	got, err := ResolveAccount(ctx, lookup, "@jsmith")
	if err != nil || got.ID != 2 || got.Login != "jsmith" || asked != 1 {
		t.Errorf("got %+v, %v after %d lookups", got, err, asked)
	}
	if _, err := ResolveAccount(ctx, nil, "carol"); !errors.Is(err, ErrGitHubUnavailable) {
		t.Errorf("no lookup = %v", err)
	}
	if _, err := ResolveAccount(ctx, lookup, "-x"); err == nil || asked != 1 {
		t.Error("a malformed login was resolved")
	}
}
