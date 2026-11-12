package auth

import (
	"context"
	"errors"
	"testing"
)

// A CLI token of a row not yet bound to an account works only while an
// address entry covers it; once bound, an account entry covers it and
// the principal carries the account.
//
// @decision(DL-128)
func TestATokenIsCoveredByTheAccountOnceItsRowIsBound(t *testing.T) {
	f := newTokenFixture(t, "")
	src := &fakeSource{accounts: map[int64]bool{42: true}}
	list, err := NewAllowlist(context.Background(), src.load, AllowlistRefresh, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.auth.Allowlist = list
	if _, err := f.auth.Authenticate(req(f.token)); !errors.Is(err, ErrBadCredential) {
		t.Fatalf("an unbound row was admitted by an account entry: %v", err)
	}
	src.set(map[string]bool{"alice@example.com": false}, nil)
	if err := list.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p, err := f.auth.Authenticate(req(f.token)); err != nil || p.Admin || p.GitHubID != 0 {
		t.Fatalf("an unbound row under an address entry = %+v, %v", p, err)
	}
	if err := f.store.Users().BindGithub(context.Background(), f.user.OrgID, f.user.ID, 42); err != nil {
		t.Fatal(err)
	}
	p, err := f.auth.Authenticate(req(f.token))
	if err != nil || !p.Admin || p.GitHubID != 42 {
		t.Errorf("a bound row = %+v, %v; want the account entry's admin", p, err)
	}
}

// The cookie authenticator asks with the row's account and address
// alike.
//
// @decision(DL-128)
func TestACookieIsCoveredByTheRowsAccount(t *testing.T) {
	f := newTokenFixture(t, "")
	if err := f.store.Users().BindGithub(context.Background(), f.user.OrgID, f.user.ID, 7); err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{accounts: map[int64]bool{7: false}}
	list, err := NewAllowlist(context.Background(), src.load, AllowlistRefresh, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &CookieAuthenticator{Sessions: fixedSession(f.user.ID), Users: f.store.Users(), Allowlist: list, OrgID: f.user.OrgID}
	p, err := c.Authenticate(req(""))
	if err != nil || p.GitHubID != 7 || p.Admin {
		t.Fatalf("principal = %+v, %v", p, err)
	}
	src.set(nil, nil)
	src.accounts = map[int64]bool{8: true}
	if err := list.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Authenticate(req("")); !errors.Is(err, ErrBadCredential) {
		t.Errorf("another account's entry admitted the row: %v", err)
	}
}

// fixedSession is a session that always names one user.
type fixedSession string

func (s fixedSession) GetString(context.Context, string) string { return string(s) }
