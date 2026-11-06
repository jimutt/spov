package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/routedcred"
	"github.com/jimutt/spov/internal/store"
)

func orgRoutedFixture(t *testing.T, admin bool) (*apiFixture, *SealedOrgRoutedCredentials) {
	t.Helper()
	var src *SealedOrgRoutedCredentials
	f := allowlistFixtureWith(t, admin, map[string]bool{"me@example.com": admin}, func(d *Deps) {
		src = &SealedOrgRoutedCredentials{Store: d.Store, Sealer: testSealer(t)}
		d.OrgRoutedCredentials = src
	})
	return f, src
}

var sharedNPM = map[string]any{"secret": "npm_sharedsecret",
	"route": routedcred.Route{Name: "shared-npm", Hosts: []string{"npm.acme.com"}, Scheme: "bearer"}}

// An admin stores the organisation's routed credential, which every
// member then lists by its route alone; the write is audited without the
// secret, and removing it is audited too.
//
// @decision(DL-074)
func TestAnAdminStoresAnOrgRoutedCredentialAndItIsAudited(t *testing.T) {
	f, src := orgRoutedFixture(t, true)
	resp, body := f.do(http.MethodPut, "/api/v1/org/routed-credentials/shared-npm", sharedNPM)
	if resp.StatusCode != http.StatusOK || strings.Contains(string(body), "npm_sharedsecret") {
		t.Fatalf("PUT = %d: %s", resp.StatusCode, body)
	}
	resp, body = f.do(http.MethodGet, "/api/v1/org/routed-credentials", nil)
	var out struct {
		RoutedCredentials []RoutedCredentialView `json:"routed_credentials"`
	}
	if err := json.Unmarshal(body, &out); err != nil || resp.StatusCode != http.StatusOK ||
		len(out.RoutedCredentials) != 1 || out.RoutedCredentials[0].Route.Hosts[0] != "npm.acme.com" {
		t.Fatalf("GET = %d: %s", resp.StatusCode, body)
	}
	held, err := src.Open(context.Background(), auth.LocalID)
	if err != nil || len(held) != 1 || held[0].Secret != "npm_sharedsecret" {
		t.Fatalf("open = %+v, %v", held, err)
	}
	if resp, body := f.do(http.MethodDelete, "/api/v1/org/routed-credentials/shared-npm", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE = %d: %s", resp.StatusCode, body)
	}
	if resp, _ := f.do(http.MethodDelete, "/api/v1/org/routed-credentials/shared-npm", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a second DELETE = %d", resp.StatusCode)
	}
	evs, err := f.store.AdminEvents().List(context.Background(), auth.LocalID, 10)
	if err != nil || len(evs) != 2 || evs[0].Action != AdminRoutedRemoved || evs[1].Action != AdminRoutedStored ||
		!strings.Contains(evs[1].Detail, "npm.acme.com") {
		t.Fatalf("events = %+v, %v", evs, err)
	}
	for _, e := range evs {
		if strings.Contains(e.Detail, "npm_sharedsecret") {
			t.Errorf("the audit trail holds the secret: %+v", e)
		}
	}
}

// A member reads the organisation's routed credentials but cannot store
// or remove one.
//
// @decision(DL-074)
func TestAMemberCannotStoreAnOrgRoutedCredential(t *testing.T) {
	f, _ := orgRoutedFixture(t, false)
	if resp, body := f.do(http.MethodPut, "/api/v1/org/routed-credentials/shared-npm", sharedNPM); resp.StatusCode != http.StatusForbidden {
		t.Errorf("PUT = %d: %s", resp.StatusCode, body)
	}
	if resp, body := f.do(http.MethodDelete, "/api/v1/org/routed-credentials/shared-npm", nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("DELETE = %d: %s", resp.StatusCode, body)
	}
	if resp, body := f.do(http.MethodGet, "/api/v1/org/routed-credentials", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("GET = %d: %s", resp.StatusCode, body)
	}
}

// An organisation's sealed secret opens only as the organisation's and
// with its own route: not as a user's, not moved to another host.
//
// @decision(DL-074)
func TestAnOrgRoutedCredentialOpensOnlyAsTheOrganisations(t *testing.T) {
	st := store.NewMemory()
	src := &SealedOrgRoutedCredentials{Store: st, Sealer: testSealer(t)}
	ctx := context.Background()
	r := routedcred.Route{Name: "shared", Hosts: []string{"npm.acme.com"}, Scheme: "bearer"}
	if err := src.Put(ctx, st, "org", "admin@example.com", r, "npm_sharedsecret"); err != nil {
		t.Fatal(err)
	}
	row, err := st.OrgCredentials().Get(ctx, "org", "route:shared")
	if err != nil {
		t.Fatal(err)
	}
	user := &SealedRoutedCredentials{Store: st.Credentials(), Sealer: src.Sealer}
	if err := st.Credentials().Put(ctx, store.UserCredential{ID: "x", OrgID: "org", UserID: "u1", Platform: "route:shared",
		Ciphertext: row.Ciphertext, Nonce: row.Nonce, KeyVersion: row.KeyVersion, Route: r.Normal().Encode()}); err != nil {
		t.Fatal(err)
	}
	if _, err := user.Open(ctx, "org", "u1"); err == nil {
		t.Error("an organisation's secret opened as a user's")
	}
	moved := r
	moved.Hosts = []string{"evil.example.com"}
	row.Meta = moved.Normal().Encode()
	if err := st.OrgCredentials().Put(ctx, row); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Open(ctx, "org"); err == nil {
		t.Error("a moved organisation route opened")
	}
}

// A launch attaches the organisation's routed credential with its tier,
// unless the caller's own takes its host.
//
// @decision(DL-074) @decision(DL-075)
func TestALaunchAttachesTheOrganisationsRoutedCredential(t *testing.T) {
	f, src := routedLaunchFixture(t)
	org := &SealedOrgRoutedCredentials{Store: f.store, Sealer: src.Sealer}
	f.srv.svc.deps.OrgRoutedCredentials = org
	ctx := context.Background()
	for _, r := range []routedcred.Route{
		{Name: "shared-maven", Hosts: []string{"repo.acme.com"}, Scheme: "basic", User: "ci"},
		{Name: "shared-npm", Hosts: []string{"npm.acme.com"}, Scheme: "bearer"},
	} {
		if err := org.Put(ctx, f.store, auth.Local().OrgID, "admin", r, "org-"+r.Name); err != nil {
			t.Fatal(err)
		}
	}
	f.id = f.create(repoReq("alpha"))
	f.events(f.id)
	sess, err := f.store.Sessions().Get(ctx, auth.Local().OrgID, f.id)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, rc := range sess.RoutedCredentials {
		got[rc.Name] = rc.Tier
	}
	if len(got) != 2 || got["acme-npm"] != "user" || got["shared-maven"] != "org" {
		t.Errorf("attached %v", got)
	}
}
