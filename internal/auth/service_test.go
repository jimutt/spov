package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/store"
)

// serviceFixture is the token fixture's authenticator with a service
// account and a token of its own, over an allowlist that (as a store
// written before the refusal might) holds an admin entry for the
// service account's address.
type serviceFixture struct {
	*tokenFixture
	svc   store.User
	token string
	rec   store.APIToken
}

func newServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	f := &serviceFixture{tokenFixture: newTokenFixture(t, "alice@example.com\n")}
	ctx := context.Background()
	f.svc = store.User{ID: "svc-1", OrgID: "default", Email: ServiceAccountAddress("ci"), Name: "ci", Kind: store.UserService}
	if err := f.store.Users().Create(ctx, f.svc); err != nil {
		t.Fatal(err)
	}
	tok, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	f.token = tok
	f.rec = store.APIToken{ID: "svc-token", OrgID: "default", UserID: f.svc.ID, Hash: HashToken(tok), Name: "ci", CreatedAt: f.now}
	if err := f.store.Tokens().Create(ctx, f.rec); err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{}
	src.set(map[string]bool{"alice@example.com": false, f.svc.Email: true, "CI@Service.Invalid.": true}, nil)
	list, err := NewAllowlist(ctx, src.load, AllowlistRefresh, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.auth.Allowlist = list
	return f
}

// A service account's token authenticates it without the allowlist,
// and its principal is never an admin, even with an admin entry for
// its address in the store.
//
// @decision(DL-140)
func TestAServicePrincipalIsNeverAnAdmin(t *testing.T) {
	f := newServiceFixture(t)
	p, err := f.auth.Authenticate(req(f.token))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Service || p.Admin || p.UserID != f.svc.ID || p.TokenID != f.rec.ID || p.GitHubID != 0 {
		t.Errorf("principal = %+v, want the service account, not an admin", p)
	}
	// Whatever produced it, a service principal in a request is not an
	// admin and reaches no one else's resources.
	got, _ := From(WithPrincipal(context.Background(), Principal{UserID: "svc-1", Service: true, Admin: true}))
	if got.Admin || got.CanAccess("someone-else") || !got.CanAccess("svc-1") {
		t.Errorf("a service principal claiming admin = %+v", got)
	}
	if (Principal{UserID: "svc-1", Service: true, Admin: true}).CanAccess("someone-else") {
		t.Error("CanAccess lets a service principal reach another's resource")
	}
	// A person's token is unchanged.
	if p, err := f.auth.Authenticate(req(f.tokenFixture.token)); err != nil || p.Service || p.UserID != f.user.ID {
		t.Errorf("the person's token = %+v, %v", p, err)
	}
}

// @decision(DL-140)
func TestADisabledServiceAccountsTokenIsRefused(t *testing.T) {
	f := newServiceFixture(t)
	if err := f.store.Users().Disable(context.Background(), "default", f.svc.ID, f.now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth.Authenticate(req(f.token)); !errors.Is(err, ErrBadCredential) {
		t.Errorf("a disabled account's token = %v, want a bad credential", err)
	}
}

// @decision(DL-140) @decision(DL-141)
func TestAnExpiredAndARevokedServiceTokenAreRefused(t *testing.T) {
	f := newServiceFixture(t)
	f.now = f.now.Add(time.Hour)
	expired := f.now.Add(-time.Minute)
	tok, _ := NewToken()
	if err := f.store.Tokens().Create(context.Background(), store.APIToken{ID: "svc-expired", OrgID: "default",
		UserID: f.svc.ID, Hash: HashToken(tok), Name: "old", ExpiresAt: &expired}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth.Authenticate(req(tok)); !errors.Is(err, ErrBadCredential) {
		t.Errorf("an expired service token = %v", err)
	}
	if err := f.store.Tokens().Revoke(context.Background(), "default", f.rec.ID, f.now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth.Authenticate(req(f.token)); !errors.Is(err, ErrBadCredential) {
		t.Errorf("a revoked service token = %v", err)
	}
}

// kindUsers answers every read with a row of the kind it was given.
type kindUsers struct {
	store.UserStore
	kind store.UserKind
	err  error
}

func (k kindUsers) Get(ctx context.Context, orgID, id string) (store.User, error) {
	if k.err != nil {
		return store.User{}, k.err
	}
	u, err := k.UserStore.Get(ctx, orgID, id)
	u.Kind = k.kind
	return u, err
}

// A row of no known kind never authenticates, by token or by cookie,
// and a store failure loading a service account is not a principal.
//
// @decision(DL-140)
func TestAUserOfAnUnknownKindNeverAuthenticates(t *testing.T) {
	f := newServiceFixture(t)
	f.auth.Users = kindUsers{UserStore: f.store.Users(), kind: "robot"}
	if p, err := f.auth.Authenticate(req(f.token)); !errors.Is(err, ErrBadCredential) {
		t.Errorf("a token of a robot row = %+v, %v", p, err)
	}
	f.auth.Users = kindUsers{UserStore: f.store.Users(), err: errors.New("store: get user: connection refused")}
	if p, err := f.auth.Authenticate(req(f.token)); err == nil || p.UserID != "" {
		t.Errorf("a store failure = %+v, %v", p, err)
	}
	a, _, _ := cookieFixture(t, "alice@example.com\n")
	a.Users = kindUsers{UserStore: a.Users, kind: "robot"}
	if p, err := a.Authenticate(httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)); !errors.Is(err, ErrBadCredential) {
		t.Errorf("a cookie of a robot row = %+v, %v", p, err)
	}
}

// A browser session that names a service account's row is refused,
// whatever the allowlist says.
//
// @decision(DL-140)
func TestTheCookieRefusesAServiceAccountsSession(t *testing.T) {
	a, _, _ := cookieFixture(t, "alice@example.com\n")
	st := store.NewMemory()
	svc := store.User{ID: "svc-1", OrgID: "default", Email: ServiceAccountAddress("ci"), Kind: store.UserService}
	if err := st.Users().Create(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{}
	src.set(map[string]bool{svc.Email: true}, nil)
	list, err := NewAllowlist(context.Background(), src.load, AllowlistRefresh, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.Users, a.Allowlist, a.Sessions = st.Users(), list, fakeSessions{userID: svc.ID}
	if p, err := a.Authenticate(httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)); !errors.Is(err, ErrBadCredential) {
		t.Errorf("a service account's session = %+v, %v", p, err)
	}
}

// @decision(DL-140)
func TestNoAllowlistEntryCoversAServiceAddress(t *testing.T) {
	set := AllowlistSet{Emails: map[string]bool{"ci@service.invalid": true, "bob@example.com": false}}
	for _, addr := range []string{"ci@service.invalid", "CI@SERVICE.INVALID", "ci@service.invalid."} {
		if admin, ok := set.Covers(0, addr); ok || admin {
			t.Errorf("%s is covered (admin %v)", addr, admin)
		}
	}
	if _, ok := set.Covers(0, "bob@example.com"); !ok {
		t.Error("a person's address is no longer covered")
	}
}

// @decision(DL-140)
func TestIsServiceAddress(t *testing.T) {
	for addr, want := range map[string]bool{
		"ci@service.invalid":       true,
		"CI@Service.Invalid":       true,
		"ci@service.invalid.":      true,
		" ci@service.invalid ":     true,
		"ci@sub.service.invalid":   true,
		"ci@example.com":           false,
		"ci@notservice.invalid":    false,
		"ci@service.invalid.com":   false,
		"service.invalid":          false,
		"a@b@service.invalid":      true,
		"ci@service.invalid.local": false,
	} {
		if got := IsServiceAddress(addr); got != want {
			t.Errorf("IsServiceAddress(%q) = %v, want %v", addr, got, want)
		}
	}
	if err := CheckAllowlistAddress("bob@example.com"); err != nil {
		t.Errorf("a person's address was refused: %v", err)
	}
	if err := CheckAllowlistAddress("Bob@Service.Invalid"); !errors.Is(err, ErrServiceAddress) {
		t.Errorf("a service address was allowed: %v", err)
	}
}

// The seed file cannot put a service account's address on the
// allowlist either.
//
// @decision(DL-140)
func TestTheSeedRefusesAServiceAddress(t *testing.T) {
	for _, line := range []string{"ci@service.invalid admin\n", "CI@SERVICE.INVALID.\n", "ci@x.service.invalid\n"} {
		st, _, err := seed(t, "alice@example.com admin\n"+line)
		if err == nil || !strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), ServiceAccountDomain) {
			t.Errorf("seed of %q = %v, want a refusal naming line 2", line, err)
		}
		if list, _ := st.Allowlist().List(context.Background(), "org"); len(list) != 0 {
			t.Errorf("a refused seed left %d entries", len(list))
		}
	}
}
