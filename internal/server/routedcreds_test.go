package server

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/credentials"
	"github.com/jimutt/spov/internal/routedcred"
	"github.com/jimutt/spov/internal/store"
)

var acmeNPM = routedcred.Route{Name: "acme-npm", Hosts: []string{"npm.acme.com"}, Scheme: "bearer"}

// routedFixture is an API fixture holding routed credentials sealed.
func routedFixture(t *testing.T) (*apiFixture, *SealedRoutedCredentials) {
	t.Helper()
	var src *SealedRoutedCredentials
	f := newAPIFixtureWith(t, func(d *Deps) {
		src = &SealedRoutedCredentials{Store: d.Store.Credentials(), Sealer: testSealer(t)}
		d.RoutedCredentials = src
		d.Credentials = &CredentialStore{Store: d.Store.Credentials(), Sealer: src.Sealer}
	})
	return f, src
}

// A routed credential is stored with its route, listed by its route
// alone, opened with its secret, and kept out of /me/credentials, which
// lists platforms and keys.
//
// @decision(DL-073)
func TestARoutedCredentialIsSealedWithItsRoute(t *testing.T) {
	f, src := routedFixture(t)
	resp, body := f.do(http.MethodPut, "/api/v1/me/routed-credentials/acme-npm",
		map[string]any{"secret": "npm_realsecret", "route": acmeNPM})
	if resp.StatusCode != http.StatusOK || strings.Contains(string(body), "npm_realsecret") {
		t.Fatalf("PUT = %d: %s", resp.StatusCode, body)
	}
	resp, body = f.do(http.MethodGet, "/api/v1/me/routed-credentials", nil)
	var out struct {
		RoutedCredentials []RoutedCredentialView `json:"routed_credentials"`
	}
	if err := json.Unmarshal(body, &out); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d: %s", resp.StatusCode, body)
	}
	if len(out.RoutedCredentials) != 1 || out.RoutedCredentials[0].Route.Hosts[0] != "npm.acme.com" ||
		out.RoutedCredentials[0].Route.Attach != routedcred.AttachAlways || strings.Contains(string(body), "npm_realsecret") {
		t.Errorf("listing = %s", body)
	}
	_, body = f.do(http.MethodGet, "/api/v1/me/credentials", nil)
	if strings.Contains(string(body), "acme-npm") {
		t.Errorf("/me/credentials lists a routed credential: %s", body)
	}
	held, err := src.Open(context.Background(), auth.Local().OrgID, auth.Local().UserID)
	if err != nil || len(held) != 1 || held[0].Secret != "npm_realsecret" || held[0].Proxied().Hosts[0] != "npm.acme.com" {
		t.Fatalf("open = %+v, %v", held, err)
	}
	if resp, _ := f.do(http.MethodDelete, "/api/v1/me/routed-credentials/acme-npm", nil); resp.StatusCode/100 != 2 {
		t.Errorf("DELETE = %d", resp.StatusCode)
	}
	if resp, _ := f.do(http.MethodDelete, "/api/v1/me/routed-credentials/acme-npm", nil); resp.StatusCode/100 != 2 {
		t.Errorf("a second DELETE = %d", resp.StatusCode)
	}
	if held, _ := src.Open(context.Background(), auth.Local().OrgID, auth.Local().UserID); len(held) != 0 {
		t.Errorf("a deleted credential still opens: %+v", held)
	}
}

// A route changed beside its sealed secret, as a write to the database
// alone would change it, no longer opens.
//
// @decision(DL-073)
func TestAChangedRouteDoesNotOpen(t *testing.T) {
	st := store.NewMemory()
	src := &SealedRoutedCredentials{Store: st.Credentials(), Sealer: testSealer(t)}
	ctx := context.Background()
	if err := src.Put(ctx, "org", "u1", acmeNPM, "npm_realsecret"); err != nil {
		t.Fatal(err)
	}
	rec, err := st.Credentials().Get(ctx, "org", "u1", "route:acme-npm")
	if err != nil {
		t.Fatal(err)
	}
	moved := acmeNPM
	moved.Hosts = []string{"attacker.example.com"}
	rec.Route = moved.Encode()
	if err := st.Credentials().Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if held, err := src.Open(ctx, "org", "u1"); err == nil {
		t.Fatalf("a moved route opened: %+v", held)
	}
	// Another user's row does not open as this one's either.
	rec.Route, rec.UserID = acmeNPM.Encode(), "u2"
	if err := st.Credentials().Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Open(ctx, "org", "u2"); err == nil {
		t.Fatal("another user's sealed secret opened")
	}
}

// A bad route, a bad secret, a name that differs from the path and one
// credential too many are each refused with a 422 that never quotes the
// secret.
//
// @decision(DL-073)
func TestABadRoutedCredentialIsRefused(t *testing.T) {
	f, src := routedFixture(t)
	for name, body := range map[string]map[string]any{
		"a wildcard host": {"secret": "s3cretvalue", "route": routedcred.Route{Hosts: []string{"*.acme.com"}, Scheme: "bearer"}},
		"no scheme":       {"secret": "s3cretvalue", "route": routedcred.Route{Hosts: []string{"npm.acme.com"}}},
		"a forbidden header": {"secret": "s3cretvalue", "route": routedcred.Route{Hosts: []string{"npm.acme.com"},
			Scheme: "header", Header: "Cookie"}},
		"another name":     {"secret": "s3cretvalue", "route": routedcred.Route{Name: "other", Hosts: []string{"npm.acme.com"}, Scheme: "bearer"}},
		"an empty secret":  {"secret": " ", "route": acmeNPM},
		"a newline secret": {"secret": "s3cret\nvalue", "route": acmeNPM},
	} {
		resp, out := f.do(http.MethodPut, "/api/v1/me/routed-credentials/acme-npm", body)
		if resp.StatusCode != http.StatusUnprocessableEntity || strings.Contains(string(out), "s3cretvalue") {
			t.Errorf("%s: %d %s", name, resp.StatusCode, out)
		}
	}
	ctx := context.Background()
	for i := range routedcred.MaxPerUser {
		r := acmeNPM
		r.Name, r.Hosts = "r"+string(rune('a'+i/26))+string(rune('a'+i%26)), []string{"h.acme.com"}
		if err := src.Put(ctx, auth.Local().OrgID, auth.Local().UserID, r, "s"); err != nil {
			t.Fatal(err)
		}
	}
	if resp, out := f.do(http.MethodPut, "/api/v1/me/routed-credentials/acme-npm",
		map[string]any{"secret": "s", "route": acmeNPM}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("one too many: %d %s", resp.StatusCode, out)
	}
	if err := src.Put(ctx, auth.Local().OrgID, auth.Local().UserID, routedcred.Route{Name: "raa",
		Hosts: []string{"other.acme.com"}, Scheme: "bearer"}, "s"); err != nil {
		t.Errorf("replacing one at the limit was refused: %v", err)
	}
}

// A server that can hold no routed credential says so with a 501.
//
// @decision(DL-073)
func TestRoutedCredentialsWithoutAKeyAre501(t *testing.T) {
	f := newAPIFixture(t)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		path := "/api/v1/me/routed-credentials"
		if method != http.MethodGet {
			path += "/acme-npm"
		}
		if resp, body := f.do(method, path, map[string]any{"secret": "s", "route": acmeNPM}); resp.StatusCode != http.StatusNotImplemented {
			t.Errorf("%s = %d: %s", method, resp.StatusCode, body)
		}
	}
}

// Locally the secret is in this machine's store and the route in the
// index, which a listing reads; a secret the store lost is listed as
// missing and left out of a launch.
//
// @decision(DL-073)
func TestALocalRoutedCredentialIsListedFromTheIndex(t *testing.T) {
	creds := credentials.NewMemoryStore()
	src := LocalRoutedCredentials{Local: &routedcred.Local{Creds: creds, Path: filepath.Join(t.TempDir(), "routes.json")}}
	ctx := context.Background()
	other := routedcred.Route{Name: "acme-maven", Hosts: []string{"repo.acme.com"}, Scheme: "basic", User: "ci", Attach: "asked"}
	for _, r := range []routedcred.Route{acmeNPM, other} {
		if err := src.Put(ctx, "", "", r, "secret-"+r.Name); err != nil {
			t.Fatal(err)
		}
	}
	if err := creds.Delete(routedcred.Key("acme-maven")); err != nil {
		t.Fatal(err)
	}
	views, err := src.List(ctx, "", "")
	if err != nil || len(views) != 2 || views[0].Missing || !views[1].Missing || views[1].Route.Attach != "asked" {
		t.Fatalf("views = %+v, %v", views, err)
	}
	held, err := src.Open(ctx, "", "")
	if err != nil || len(held) != 1 || held[0].Secret != "secret-acme-npm" {
		t.Fatalf("held = %+v, %v", held, err)
	}
	if removed, err := src.Delete(ctx, "", "", "acme-npm"); err != nil || !removed {
		t.Errorf("delete = %v, %v", removed, err)
	}
	if removed, err := src.Delete(ctx, "", "", "acme-npm"); err != nil || removed {
		t.Errorf("a second delete = %v, %v", removed, err)
	}
	if _, err := creds.Get(routedcred.Key("acme-npm")); err == nil {
		t.Error("the secret outlived its route")
	}
}
