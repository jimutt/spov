package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/routedcred"
	"github.com/jimutt/spov/internal/server"
)

// The routed-credential calls carry the secret in the body, never the
// path, and name the credential in the path, escaped.
//
// @decision(DL-073) @decision(DL-074)
func TestTheRoutedCredentialCallsCarryTheSecretInTheBody(t *testing.T) {
	var puts, deletes []string
	put := func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Secret string           `json:"secret"`
			Route  routedcred.Route `json:"route"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if strings.Contains(r.URL.String(), body.Secret) || body.Route.Name != r.PathValue("name") {
			problem(w, 422, "bad", "")
			return
		}
		puts = append(puts, r.URL.Path+"="+body.Secret)
		_, _ = fmt.Fprintf(w, `{"route":{"name":%q,"hosts":["npm.acme.com"],"scheme":"bearer","attach":"always"}}`, body.Route.Name)
	}
	del := func(w http.ResponseWriter, r *http.Request) {
		deletes = append(deletes, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}
	list := func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"routed_credentials":[{"route":{"name":"acme","hosts":["npm.acme.com"],"scheme":"bearer"}}]}`)
	}
	c := stub(t, map[string]http.HandlerFunc{
		"GET /api/v1/me/routed-credentials":            list,
		"PUT /api/v1/me/routed-credentials/{name}":     put,
		"DELETE /api/v1/me/routed-credentials/{name}":  del,
		"GET /api/v1/org/routed-credentials":           list,
		"PUT /api/v1/org/routed-credentials/{name}":    put,
		"DELETE /api/v1/org/routed-credentials/{name}": del,
	})
	ctx := context.Background()
	r := routedcred.Route{Name: "acme", Hosts: []string{"npm.acme.com"}, Scheme: "bearer"}
	if v, err := c.PutRoutedCredential(ctx, r, "s3cret"); err != nil || v.Route.Name != "acme" {
		t.Fatalf("put = %+v, %v", v, err)
	}
	if err := c.PutOrgRoutedCredential(ctx, r, "s3cret"); err != nil {
		t.Fatal(err)
	}
	for _, f := range []func(context.Context) ([]server.RoutedCredentialView, error){c.ListRoutedCredentials, c.ListOrgRoutedCredentials} {
		views, err := f(ctx)
		if err != nil || len(views) != 1 || views[0].Route.Name != "acme" {
			t.Errorf("list = %+v, %v", views, err)
		}
	}
	if err := c.DeleteRoutedCredential(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteOrgRoutedCredential(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if len(puts) != 2 || len(deletes) != 2 || !strings.HasPrefix(deletes[1], "/api/v1/org/") {
		t.Errorf("puts %v, deletes %v", puts, deletes)
	}
}
