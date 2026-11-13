package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/store"
)

// serviceFixture is an allowlist fixture whose admin is me@example.com,
// with the real token authenticator ahead of the admin's static
// principal, so a request carrying a spov token is that token's user.
type serviceFixture struct {
	*apiFixture
	mu   sync.Mutex
	logs []string
}

func newServiceFixture(t *testing.T, entries map[string]bool, adjust func(*Deps)) *serviceFixture {
	t.Helper()
	sf := &serviceFixture{}
	if entries == nil {
		entries = map[string]bool{"me@example.com": true}
	}
	sf.apiFixture = allowlistFixtureWith(t, true, entries, func(d *Deps) {
		d.Warn = func(s string) {
			sf.mu.Lock()
			sf.logs = append(sf.logs, s)
			sf.mu.Unlock()
		}
		if adjust != nil {
			adjust(d)
		}
	})
	admin := sf.auth.inner
	sf.auth.inner = auth.Chain{&auth.TokenAuthenticator{
		Tokens: sf.store.Tokens(), Users: sf.store.Users(), Allowlist: sf.srv.deps.Login.Allowlist,
		Warn: sf.srv.deps.Warn,
	}, admin}
	return sf
}

// as sends a request with a spov token in place of the admin.
func bearer(token string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

func (f *serviceFixture) createAccount(t *testing.T, name string) ServiceAccountView {
	t.Helper()
	resp, out := f.do(http.MethodPost, "/api/v1/service-accounts", map[string]any{"name": name})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create %s = %d: %s", name, resp.StatusCode, out)
	}
	var v ServiceAccountView
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func (f *serviceFixture) issue(t *testing.T, name string, body map[string]any) IssuedServiceToken {
	t.Helper()
	resp, out := f.do(http.MethodPost, "/api/v1/service-accounts/"+name+"/tokens", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("issue a token for %s = %d: %s", name, resp.StatusCode, out)
	}
	var v IssuedServiceToken
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func (f *serviceFixture) events(t *testing.T) []store.AdminEvent {
	t.Helper()
	evs, err := f.store.AdminEvents().List(context.Background(), auth.LocalID, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func (f *serviceFixture) actions(t *testing.T, action string) []store.AdminEvent {
	t.Helper()
	var out []store.AdminEvent
	for _, e := range f.events(t) {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

// An admin creates an account, issues it a token, and the token
// authenticates it as itself: not an admin, listed with its kind, with
// a commit author defaulting to its name and address.
//
// @decision(DL-140) @decision(DL-141)
func TestAServiceAccountsTokenAuthenticatesItAsItself(t *testing.T) {
	f := newServiceFixture(t, nil, nil)
	v := f.createAccount(t, "ci-release")
	if v.Name != "ci-release" || v.GitName != "ci-release" || v.GitEmail != "ci-release@service.invalid" || v.Tokens != 0 {
		t.Errorf("created = %+v", v)
	}
	tok := f.issue(t, "ci-release", map[string]any{"name": "release workflow"})
	if _, err := auth.ParseToken(tok.Token); err != nil {
		t.Fatalf("the issued token is not a spov token: %v", err)
	}
	if tok.ExpiresAt == nil || tok.ExpiresAt.Sub(tok.CreatedAt) != DefaultServiceTokenDays*24*time.Hour {
		t.Errorf("a token with no lifetime named expires at %v, want a year after %v", tok.ExpiresAt, tok.CreatedAt)
	}

	resp, out := f.do(http.MethodGet, "/api/v1/me", nil, bearer(tok.Token))
	var me MeView
	if resp.StatusCode != http.StatusOK || json.Unmarshal(out, &me) != nil {
		t.Fatalf("me with the token = %d: %s", resp.StatusCode, out)
	}
	if me.UserID != v.ID || me.Admin || me.GitName != "ci-release" {
		t.Errorf("me = %+v, want the service account, not an admin", me)
	}
	// Its own tokens are listed through /me/tokens, and it cannot read
	// the admin routes.
	if resp, out := f.do(http.MethodGet, "/api/v1/me/tokens", nil, bearer(tok.Token)); resp.StatusCode != http.StatusOK ||
		!strings.Contains(string(out), tok.ID) {
		t.Errorf("its own tokens = %d: %s", resp.StatusCode, out)
	}
	if resp, _ := f.do(http.MethodGet, "/api/v1/service-accounts", nil, bearer(tok.Token)); resp.StatusCode != http.StatusForbidden {
		t.Errorf("the service account listed the service accounts: %d", resp.StatusCode)
	}

	// The users listing names its kind.
	_, out = f.do(http.MethodGet, "/api/v1/users", nil)
	var users struct{ Users []UserView }
	if err := json.Unmarshal(out, &users); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, u := range users.Users {
		if u.ID == v.ID {
			found = true
			if u.Kind != store.UserService || u.Admin {
				t.Errorf("the users listing shows the account as %+v", u)
			}
		}
	}
	if !found {
		t.Errorf("the users listing does not show the account: %s", out)
	}

	// The author can be changed, and the listing counts the token.
	if resp, out := f.do(http.MethodPatch, "/api/v1/service-accounts/ci-release",
		map[string]any{"git_name": "Release Bot", "git_email": "release@example.com"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("patch = %d: %s", resp.StatusCode, out)
	}
	_, out = f.do(http.MethodGet, "/api/v1/service-accounts", nil)
	var list struct{ Accounts []ServiceAccountView }
	if err := json.Unmarshal(out, &list); err != nil || len(list.Accounts) != 1 {
		t.Fatalf("listing = %s", out)
	}
	if a := list.Accounts[0]; a.GitName != "Release Bot" || a.GitEmail != "release@example.com" || a.Tokens != 1 || a.LastUsedAt == nil {
		t.Errorf("listed = %+v", a)
	}
	for _, action := range []string{AdminServiceAccountCreated, AdminServiceAccountUpdated, AdminServiceTokenCreated} {
		if evs := f.actions(t, action); len(evs) != 1 || evs[0].Actor != "me@example.com" {
			t.Errorf("%s audit rows = %+v", action, evs)
		}
	}

	// Revoked, the token answers 401, and the revoke is audited.
	if resp, out := f.do(http.MethodDelete, "/api/v1/service-accounts/ci-release/tokens/"+tok.ID, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke = %d: %s", resp.StatusCode, out)
	}
	if resp, _ := f.do(http.MethodGet, "/api/v1/me", nil, bearer(tok.Token)); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a revoked service token = %d, want 401", resp.StatusCode)
	}
	if evs := f.actions(t, AdminServiceTokenRevoked); len(evs) != 1 || !strings.Contains(evs[0].Detail, tok.ID) {
		t.Errorf("revoke audit rows = %+v", evs)
	}
	if resp, _ := f.do(http.MethodDelete, "/api/v1/service-accounts/ci-release/tokens/"+tok.ID, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a second revoke = %d, want 404", resp.StatusCode)
	}
}

// The token's text is in the create response and nowhere else: not in
// a listing, an audit row or a log line.
//
// @decision(DL-141)
func TestTheTokenTextAppearsOnlyInTheCreateResponse(t *testing.T) {
	f := newServiceFixture(t, nil, nil)
	f.createAccount(t, "ci-docs")
	resp, out := f.do(http.MethodPost, "/api/v1/service-accounts/ci-docs/tokens", map[string]any{"name": "docs", "no_expiry": true})
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("issue = %d (Cache-Control %q): %s", resp.StatusCode, resp.Header.Get("Cache-Control"), out)
	}
	var tok IssuedServiceToken
	if err := json.Unmarshal(out, &tok); err != nil || tok.Token == "" || tok.ExpiresAt != nil {
		t.Fatalf("issued = %+v, %v", tok, err)
	}
	// It is used once, and a disabled account's use of it is refused
	// and logged, so both kinds of log line are there to be read.
	f.do(http.MethodGet, "/api/v1/me", nil, bearer(tok.Token))
	var seen []string
	for _, path := range []string{"/api/v1/service-accounts", "/api/v1/service-accounts/ci-docs/tokens", "/api/v1/users"} {
		resp, out := f.do(http.MethodGet, path, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, resp.StatusCode, out)
		}
		seen = append(seen, string(out))
	}
	_, out = f.do(http.MethodGet, "/api/v1/me/tokens", nil, bearer(tok.Token))
	seen = append(seen, string(out))
	if resp, out := f.do(http.MethodDelete, "/api/v1/service-accounts/ci-docs", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", resp.StatusCode, out)
	}
	f.do(http.MethodGet, "/api/v1/me", nil, bearer(tok.Token))
	for _, e := range f.events(t) {
		seen = append(seen, e.Detail, e.Actor)
	}
	f.mu.Lock()
	seen = append(seen, f.logs...)
	f.mu.Unlock()
	secret := strings.TrimPrefix(tok.Token, "spov_pat_")
	for _, s := range seen {
		if strings.Contains(s, secret) {
			t.Errorf("the token's text is in %q", s)
		}
	}
	if !strings.Contains(strings.Join(seen, "\n"), tok.ID) {
		t.Error("nothing names the token by its id, so this test read nothing")
	}
}

// @decision(DL-141)
func TestTheEleventhUsableServiceTokenIsAConflict(t *testing.T) {
	f := newServiceFixture(t, nil, nil)
	acct := f.createAccount(t, "ci-many")
	// An expired token is not usable, and does not count.
	past := time.Now().Add(-time.Hour)
	if err := f.store.Tokens().Create(context.Background(), store.APIToken{ID: store.NewID(past), OrgID: auth.LocalID,
		UserID: acct.ID, Hash: []byte("expired"), Name: "old", ExpiresAt: &past}); err != nil {
		t.Fatal(err)
	}
	var first IssuedServiceToken
	for i := range MaxServiceTokens {
		tok := f.issue(t, "ci-many", map[string]any{"name": fmt.Sprintf("t%d", i), "expires_in_days": 1 + i})
		if i == 0 {
			first = tok
		}
	}
	resp, out := f.do(http.MethodPost, "/api/v1/service-accounts/ci-many/tokens", map[string]any{"name": "eleventh"})
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(out), "revoke one first") {
		t.Fatalf("the eleventh = %d: %s", resp.StatusCode, out)
	}
	// Revoking one makes room.
	if resp, _ := f.do(http.MethodDelete, "/api/v1/service-accounts/ci-many/tokens/"+first.ID, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatal("revoke failed")
	}
	f.issue(t, "ci-many", map[string]any{"name": "replacement"})
}

// @decision(DL-141)
func TestAServiceTokensLabelAndLifetimeAreChecked(t *testing.T) {
	f := newServiceFixture(t, nil, nil)
	f.createAccount(t, "ci-check")
	for name, body := range map[string]map[string]any{
		"no label":        {"name": " "},
		"a long label":    {"name": strings.Repeat("x", MaxServiceTokenLabel+1)},
		"a control":       {"name": "bad\u0007label"},
		"zero days":       {"name": "x", "expires_in_days": 0},
		"too many days":   {"name": "x", "expires_in_days": MaxServiceTokenDays + 1},
		"both lifetimes":  {"name": "x", "expires_in_days": 5, "no_expiry": true},
		"a negative term": {"name": "x", "expires_in_days": -1},
	} {
		if resp, out := f.do(http.MethodPost, "/api/v1/service-accounts/ci-check/tokens", body); resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("%s = %d: %s", name, resp.StatusCode, out)
		}
	}
	tok := f.issue(t, "ci-check", map[string]any{"name": "longest", "expires_in_days": MaxServiceTokenDays})
	if got := tok.ExpiresAt.Sub(tok.CreatedAt); got != MaxServiceTokenDays*24*time.Hour {
		t.Errorf("the longest lifetime = %v", got)
	}
	if resp, _ := f.do(http.MethodPost, "/api/v1/service-accounts/nobody/tokens", map[string]any{"name": "x"}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a token for no account = %d", resp.StatusCode)
	}
}

// @decision(DL-140)
func TestTheHundredAndFirstServiceAccountIsRefused(t *testing.T) {
	// The request limits (DL-133) are not what this tests.
	f := newServiceFixture(t, nil, func(d *Deps) {
		limits := DefaultLimits()
		limits.Requests = Rate{PerMinute: 1_000_000, Burst: 1_000_000}
		d.Limits = &limits
	})
	for i := range MaxServiceAccounts {
		f.createAccount(t, fmt.Sprintf("ci-%03d", i))
	}
	resp, out := f.do(http.MethodPost, "/api/v1/service-accounts", map[string]any{"name": "ci-extra"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("the hundred-and-first = %d: %s", resp.StatusCode, out)
	}
	// A deleted account frees a place but keeps its name.
	if resp, out := f.do(http.MethodDelete, "/api/v1/service-accounts/ci-000", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", resp.StatusCode, out)
	}
	if resp, out := f.do(http.MethodPost, "/api/v1/service-accounts", map[string]any{"name": "ci-000"}); resp.StatusCode != http.StatusConflict {
		t.Errorf("a deleted account's name was taken again: %d %s", resp.StatusCode, out)
	}
	f.createAccount(t, "ci-extra")
}

// @decision(DL-140)
func TestAServiceAccountsNameAndAuthorAreChecked(t *testing.T) {
	f := newServiceFixture(t, nil, nil)
	for _, name := range []string{"ab", "Ab-c", "1abc", "abc-", "a_bc", strings.Repeat("a", 33), "abc.def", ""} {
		if resp, out := f.do(http.MethodPost, "/api/v1/service-accounts", map[string]any{"name": name}); resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("name %q = %d: %s", name, resp.StatusCode, out)
		}
	}
	for field, value := range map[string]string{"git_email": "not an address", "git_name": "  "} {
		if resp, out := f.do(http.MethodPost, "/api/v1/service-accounts", map[string]any{"name": "ci-ok", field: value}); resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("%s %q = %d: %s", field, value, resp.StatusCode, out)
		}
	}
	f.createAccount(t, "abc")
	f.createAccount(t, strings.Repeat("a", 32))
	if resp, _ := f.do(http.MethodPost, "/api/v1/service-accounts", map[string]any{"name": "abc"}); resp.StatusCode != http.StatusConflict {
		t.Errorf("a taken name = %d", resp.StatusCode)
	}
	if resp, _ := f.do(http.MethodPatch, "/api/v1/service-accounts/nobody", map[string]any{"git_name": "x"}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("patch of no account = %d", resp.StatusCode)
	}
	if resp, _ := f.do(http.MethodPatch, "/api/v1/service-accounts/abc", map[string]any{"git_email": "x"}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("patch with a bad address = %d", resp.StatusCode)
	}
}

// The service-account routes name only service accounts of the
// caller's org: a person's row, through its address, and a person's
// token id are not found, and nothing of theirs changes.
//
// @decision(DL-140) @decision(DL-141)
func TestTheServiceAccountRoutesReachOnlyServiceAccounts(t *testing.T) {
	f := newServiceFixture(t, nil, nil)
	ctx := context.Background()
	// A person whose row carries what would be an account's address.
	person := store.User{ID: store.NewID(time.Now()), OrgID: auth.LocalID, Email: "ci-person@service.invalid"}
	if err := f.store.Users().Create(ctx, person); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Tokens().Create(ctx, store.APIToken{ID: "person-token", OrgID: auth.LocalID, UserID: person.ID,
		Hash: []byte("person"), Name: "laptop"}); err != nil {
		t.Fatal(err)
	}
	// And a service account of another org of the same name.
	if err := f.store.Users().Create(ctx, store.User{ID: store.NewID(time.Now()), OrgID: "org-other",
		Email: "ci-far@service.invalid", Name: "ci-far", Kind: store.UserService}); err != nil {
		t.Fatal(err)
	}
	f.createAccount(t, "ci-near")
	for _, r := range []struct{ method, path string }{
		{http.MethodPatch, "/api/v1/service-accounts/ci-person"},
		{http.MethodDelete, "/api/v1/service-accounts/ci-person"},
		{http.MethodGet, "/api/v1/service-accounts/ci-person/tokens"},
		{http.MethodPost, "/api/v1/service-accounts/ci-person/tokens"},
		{http.MethodDelete, "/api/v1/service-accounts/ci-person/tokens/person-token"},
		{http.MethodDelete, "/api/v1/service-accounts/ci-near/tokens/person-token"},
		{http.MethodDelete, "/api/v1/service-accounts/ci-far"},
		{http.MethodGet, "/api/v1/service-accounts/ci-far/tokens"},
	} {
		var body any
		switch r.method {
		case http.MethodPost:
			body = map[string]any{"name": "x"}
		case http.MethodPatch:
			body = map[string]any{"git_name": "x"}
		}
		if resp, out := f.do(r.method, r.path, body); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404: %s", r.method, r.path, resp.StatusCode, out)
		}
	}
	if tok, _ := f.store.Tokens().ByID(ctx, auth.LocalID, "person-token"); tok.RevokedAt != nil {
		t.Error("a person's token was revoked through a service account's route")
	}
	_, out := f.do(http.MethodGet, "/api/v1/service-accounts", nil)
	if strings.Contains(string(out), "ci-person") || strings.Contains(string(out), "ci-far") {
		t.Errorf("the listing shows a person or another org's account: %s", out)
	}
}

// An allowlist entry under service.invalid is refused, in any case and
// with a trailing dot, and an allowlist delete never reaches a service
// account, even through an entry stored before the refusal.
//
// @decision(DL-140)
func TestAnAddressEntryUnderServiceInvalidIsRefused(t *testing.T) {
	f := newServiceFixture(t, nil, nil)
	ctx := context.Background()
	acct := f.createAccount(t, "ci-entry")
	tok := f.issue(t, "ci-entry", map[string]any{"name": "ci"})
	for _, entry := range []string{"ci-entry@service.invalid", "CI-ENTRY@Service.Invalid", "x@sub.service.invalid",
		"ci-entry@service.invalid."} {
		resp, out := f.do(http.MethodPut, "/api/v1/allowlist/"+entry, map[string]any{"admin": true})
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("PUT %s = %d: %s", entry, resp.StatusCode, out)
		}
	}
	entries, _ := f.store.Allowlist().List(ctx, auth.LocalID)
	for _, e := range entries {
		if auth.IsServiceAddress(e.Email) {
			t.Errorf("a service address was stored: %+v", e)
		}
	}
	// A stored one changes nothing and its delete off-boards nothing.
	if err := f.store.Allowlist().Put(ctx, store.AllowlistEntry{OrgID: auth.LocalID, Email: "ci-entry@service.invalid", Admin: true}); err != nil {
		t.Fatal(err)
	}
	f.srv.allowlistChanged(ctx)
	if resp, out := f.do(http.MethodGet, "/api/v1/me", nil, bearer(tok.Token)); resp.StatusCode != http.StatusOK ||
		strings.Contains(string(out), `"admin":true`) {
		t.Errorf("with an admin entry for its address the account is %d: %s", resp.StatusCode, out)
	}
	if resp, out := f.do(http.MethodDelete, "/api/v1/allowlist/ci-entry@service.invalid", nil); resp.StatusCode != http.StatusNoContent {
		t.Errorf("deleting the stored entry = %d: %s", resp.StatusCode, out)
	}
	if resp, _ := f.do(http.MethodDelete, "/api/v1/allowlist/ci-entry@service.invalid", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a retry by the account's address = %d, want 404", resp.StatusCode)
	}
	if resp, _ := f.do(http.MethodGet, "/api/v1/me", nil, bearer(tok.Token)); resp.StatusCode != http.StatusOK {
		t.Errorf("an allowlist delete ended the service account's token: %d", resp.StatusCode)
	}
	if u, _ := f.store.Users().Get(ctx, auth.LocalID, acct.ID); u.DisabledAt != nil {
		t.Error("an allowlist delete disabled the service account")
	}
	if len(f.actions(t, AdminMemberOffboarded)) != 0 {
		t.Error("an allowlist delete off-boarded the service account")
	}
}

// Deleting an account disables it at once, so its token answers 401,
// then ends what it has running and stored. A failed step is named and
// retried by the same delete; the name stays taken and the row is kept.
//
// @decision(DL-140) @decision(DL-132)
func TestDeletingAServiceAccountOffboardsItAndTheSameDeleteRetries(t *testing.T) {
	var fail atomic.Bool
	f := newServiceFixture(t, nil, func(d *Deps) { d.Store = failingStore{Store: d.Store, fail: &fail} })
	ctx := context.Background()
	acct := f.createAccount(t, "ci-gone")
	tok := f.issue(t, "ci-gone", map[string]any{"name": "ci"})
	// A running session of its own, and stored credentials.
	h, err := f.provider.Create(ctx, sandbox.Spec{Name: "svc", Image: "i", Command: []string{"x"},
		CPU: 0.5, MemoryMiB: 1024, Timeout: time.Hour, IdleTimeout: 10 * time.Minute,
		Tags: sandbox.BuildTags(sandbox.TagSet{Name: "svc", User: acct.ID, CreatedAt: time.Now()}), Egress: sandbox.EgressOpen()})
	if err != nil {
		t.Fatal(err)
	}
	info, _ := h.Info(ctx)
	sess := store.Session{ID: store.NewID(time.Now()), OrgID: auth.LocalID, OwnerID: acct.ID, Name: "svc",
		State: store.StateRunning, SandboxID: info.ID}
	if err := f.store.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	for _, platform := range []string{"github", "route:npm"} {
		if err := f.store.Credentials().Put(ctx, store.UserCredential{ID: store.NewID(time.Now()), OrgID: auth.LocalID,
			UserID: acct.ID, Platform: platform, Ciphertext: []byte("sealed"), Nonce: []byte("n")}); err != nil {
			t.Fatal(err)
		}
	}

	fail.Store(true)
	resp, out := f.do(http.MethodDelete, "/api/v1/service-accounts/ci-gone", nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("delete with a failing step = %d: %s", resp.StatusCode, out)
	}
	for _, want := range []string{"service account ci-gone is deleted", "2 stored credentials not deleted", "send the same delete again"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the answer does not say %q: %s", want, out)
		}
	}
	u, _ := f.store.Users().Get(ctx, auth.LocalID, acct.ID)
	if u.DisabledAt == nil {
		t.Fatal("the account is not disabled")
	}
	if resp, _ := f.do(http.MethodGet, "/api/v1/me", nil, bearer(tok.Token)); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a deleted account's token = %d, want 401", resp.StatusCode)
	}
	if got, _ := f.store.Sessions().Get(ctx, auth.LocalID, sess.ID); got.State.Live() {
		t.Error("a failed credential step kept the session alive")
	}

	fail.Store(false)
	if resp, out := f.do(http.MethodDelete, "/api/v1/service-accounts/ci-gone", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("the retry = %d: %s", resp.StatusCode, out)
	}
	if creds, _ := f.store.Credentials().ListByUser(ctx, auth.LocalID, acct.ID); len(creds) != 0 {
		t.Errorf("credentials left after the retry: %+v", creds)
	}
	toks, _ := f.store.Tokens().ListByUser(ctx, auth.LocalID, acct.ID)
	for _, tk := range toks {
		if tk.RevokedAt == nil {
			t.Errorf("token %s is not revoked", tk.ID)
		}
	}
	if again, _ := f.store.Users().Get(ctx, auth.LocalID, acct.ID); again.DisabledAt == nil || !again.DisabledAt.Equal(*u.DisabledAt) {
		t.Errorf("the retry changed the row: %+v", again)
	}
	if evs := f.actions(t, AdminServiceAccountDeleted); len(evs) != 1 {
		t.Errorf("deleted audit rows = %+v, want one", evs)
	}
	if evs := f.actions(t, AdminServiceAccountOffboarded); len(evs) != 2 || !strings.Contains(evs[1].Detail, "left: 2 stored credentials") {
		t.Errorf("off-boarded audit rows = %+v", evs)
	}
	// A deleted account takes no new token and no new author.
	if resp, _ := f.do(http.MethodPost, "/api/v1/service-accounts/ci-gone/tokens", map[string]any{"name": "again"}); resp.StatusCode != http.StatusConflict {
		t.Errorf("a token for a deleted account = %d", resp.StatusCode)
	}
	if resp, _ := f.do(http.MethodPatch, "/api/v1/service-accounts/ci-gone", map[string]any{"git_name": "x"}); resp.StatusCode != http.StatusConflict {
		t.Errorf("a patch of a deleted account = %d", resp.StatusCode)
	}
	_, out = f.do(http.MethodGet, "/api/v1/service-accounts", nil)
	if !strings.Contains(string(out), `"disabled_at"`) {
		t.Errorf("the listing does not show the deletion: %s", out)
	}
}

// An admin whose entry no longer makes them one is refused at the
// write, whatever their credential still says, and a local server does
// not serve the routes.
//
// @decision(DL-140) @decision(DL-042)
func TestTheServiceAccountWritesRecheckTheAdminAndALocalServerHasNone(t *testing.T) {
	f := newServiceFixture(t, map[string]bool{"me@example.com": false, "other@example.com": true}, nil)
	if resp, out := f.do(http.MethodPost, "/api/v1/service-accounts", map[string]any{"name": "ci-stale"}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a removed admin's create = %d: %s", resp.StatusCode, out)
	}
	if users, _ := f.store.Users().List(context.Background(), auth.LocalID); len(users) != 0 {
		t.Errorf("a refused create made %+v", users)
	}

	local := newAPIFixtureWith(t, func(d *Deps) { d.Local = true })
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/service-accounts"},
		{http.MethodPost, "/api/v1/service-accounts"},
		{http.MethodGet, "/api/v1/service-accounts/ci/tokens"},
	} {
		var body any
		if r.method == http.MethodPost {
			body = map[string]any{"name": "ci-local"}
		}
		if resp, out := local.do(r.method, r.path, body); resp.StatusCode != http.StatusNotImplemented {
			t.Errorf("local %s %s = %d: %s", r.method, r.path, resp.StatusCode, out)
		}
	}
}

// failingUserGets makes every user read fail, as a store outage would.
type failingUserGets struct{ store.UserStore }

func (failingUserGets) Get(context.Context, string, string) (store.User, error) {
	return store.User{}, errors.New("store: get user: connection refused")
}

// A store failure while reading a service account's row is never a
// principal.
//
// @decision(DL-140)
func TestAStoreFailureNeverAuthenticatesAServiceAccount(t *testing.T) {
	f := newServiceFixture(t, nil, nil)
	f.createAccount(t, "ci-outage")
	tok := f.issue(t, "ci-outage", map[string]any{"name": "ci"})
	f.auth.inner = auth.Chain{&auth.TokenAuthenticator{
		Tokens: f.store.Tokens(), Users: failingUserGets{f.store.Users()}, Allowlist: f.srv.deps.Login.Allowlist,
	}}
	if resp, _ := f.do(http.MethodGet, "/api/v1/me", nil, bearer(tok.Token)); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a token whose user could not be read = %d, want 401", resp.StatusCode)
	}
}
