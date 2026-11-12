package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/store"
)

// serviceRow puts a service account's row in a login fixture's store.
func serviceRow(t *testing.T, st store.Store, email string) store.User {
	t.Helper()
	u := store.User{ID: store.NewID(time.Now()), OrgID: DefaultOrgID, Email: email, Name: "ci", Kind: store.UserService}
	if err := st.Users().Create(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

// No browser sign-in resolves to, binds or takes the address of a
// service account's row, even when an account entry admits the GitHub
// account that signs in: under service.invalid it is refused outright,
// and a service row found by address is never bound.
//
// @decision(DL-140) @decision(DL-128)
func TestASignInCannotTakeAServiceRow(t *testing.T) {
	for name, email := range map[string]string{
		"its own address":         "ci@service.invalid",
		"its address in capitals": "CI@SERVICE.INVALID",
		"another service address": "someone@service.invalid",
		"a row with any address":  "ci@example.com",
	} {
		t.Run(name, func(t *testing.T) {
			f := newLoginFixture(t, "@octocat\n")
			row := email
			if auth.IsServiceAddress(email) {
				row = "ci@service.invalid"
			}
			svc := serviceRow(t, f.store, row)
			f.github.emails = []map[string]any{{"email": email, "primary": true, "verified": true}}
			resp := f.doLogin("")
			if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(resp.Body), "service account") {
				t.Fatalf("the sign-in = %d: %s", resp.StatusCode, resp.Body)
			}
			got, err := f.store.Users().Get(context.Background(), DefaultOrgID, svc.ID)
			if err != nil || got.GithubID != 0 || got.Kind != store.UserService || got.LastLoginAt != nil {
				t.Errorf("the service row after the sign-in = %+v, %v", got, err)
			}
			users, _ := f.store.Users().List(context.Background(), DefaultOrgID)
			if len(users) != 1 {
				t.Errorf("the refused sign-in made a row: %+v", users)
			}
			if me := f.get("/api/v1/me"); me.StatusCode != http.StatusUnauthorized {
				t.Errorf("me after the refusal = %d", me.StatusCode)
			}
		})
	}
}

// A person's sign-in is untouched by the service rows beside it.
//
// @decision(DL-140)
func TestAPersonsSignInIsUnchangedBesideAServiceRow(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	serviceRow(t, f.store, "ci@service.invalid")
	if resp := f.doLogin(""); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("the sign-in = %d: %s", resp.StatusCode, resp.Body)
	}
	person := f.onlyPerson(t)
	if person.Kind != store.UserPerson || person.GithubID != 583231 {
		t.Errorf("the person's row = %+v", person)
	}
}

func (f *loginFixture) onlyPerson(t *testing.T) store.User {
	t.Helper()
	users, _ := f.store.Users().List(context.Background(), DefaultOrgID)
	var out []store.User
	for _, u := range users {
		if u.Person() {
			out = append(out, u)
		}
	}
	if len(out) != 1 {
		t.Fatalf("people = %+v, want one", out)
	}
	return out[0]
}

// A device code approved for a service account's row, however it got
// there, issues no token: the exchange is refused and the code spent.
// The approval page refuses a service principal too.
//
// @decision(DL-141) @decision(DL-006)
func TestAServiceAccountsTokenCannotBeIssuedByTheDeviceFlow(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	svc := serviceRow(t, f.store, "ci@service.invalid")
	deviceCode := f.startFlow()
	ctx := context.Background()
	dc, err := f.store.DeviceCodes().ByHash(ctx, auth.HashDeviceCode(deviceCode))
	if err != nil {
		t.Fatal(err)
	}
	dc.Status, dc.UserID, dc.OrgID = store.DeviceApproved, svc.ID, DefaultOrgID
	if err := f.store.DeviceCodes().Update(ctx, dc); err != nil {
		t.Fatal(err)
	}
	if got := f.poll(deviceCode); got != "access_denied" {
		t.Errorf("the exchange = %q, want access_denied", got)
	}
	if toks, _ := f.store.Tokens().ListByUser(ctx, DefaultOrgID, svc.ID); len(toks) != 0 {
		t.Errorf("the device flow issued the service account %d tokens", len(toks))
	}
	if got := f.poll(deviceCode); got != "invalid_grant" {
		t.Errorf("a second exchange = %q, want the code spent", got)
	}

	r, _ := http.NewRequest(http.MethodPost, "/device", nil)
	r = r.WithContext(auth.WithPrincipal(ctx, auth.Principal{UserID: svc.ID, OrgID: DefaultOrgID, Service: true}))
	if _, ok := approvingUser(r); ok {
		t.Error("a service principal may approve a device flow")
	}
}
