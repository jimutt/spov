package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/routedcred"
	"github.com/jimutt/spov/internal/store"
)

func withTemplates(d *Deps) { d.Templates = &store.FileTemplates{} }

func putTemplate(t *testing.T, f *apiFixture, name string, body TemplateSettingsView) {
	t.Helper()
	if resp, out := f.do(http.MethodPut, "/api/v1/me/templates/"+name, body); resp.StatusCode != http.StatusOK {
		t.Fatalf("put template %s: %d %s", name, resp.StatusCode, out)
	}
}

// Each setting comes from the request's own field, then the template,
// then what applied before; variables merge by name, and add-ons and apt
// are one pair.
//
// @decision(DL-120)
func TestARequestsOwnFieldWinsOverATemplates(t *testing.T) {
	tmpl := store.Template{Name: "acme", TemplateSettings: store.TemplateSettings{
		Egress: "install", EgressExtraDomains: []string{"pkg.acme.com"},
		Env: map[string]string{"A": "template", "B": "template"}, Addons: []string{"rust"}, Apt: []string{"jq"},
		GitAccess: store.GitAccessAll,
	}}
	got := applyTemplate(store.SessionRequest{}, tmpl)
	if got.Template != "acme" || got.Egress != "install" || !slices.Equal(got.EgressExtraDomains, []string{"pkg.acme.com"}) ||
		!slices.Equal(got.Addons, []string{"rust"}) || !slices.Equal(got.Apt, []string{"jq"}) ||
		got.GitAccess != store.GitAccessAll || !got.GitAccessPreferred || got.Env["A"] != "template" {
		t.Errorf("a bare request took %+v", got)
	}
	got = applyTemplate(store.SessionRequest{Egress: "agent", Env: map[string]string{"A": "mine"}, Apt: []string{"ripgrep"},
		GitAccess: store.GitAccessRepos}, tmpl)
	if got.Egress != "agent" || len(got.EgressExtraDomains) != 0 {
		t.Errorf("the request's egress did not replace the preset and its domains: %s %v", got.Egress, got.EgressExtraDomains)
	}
	if got.Env["A"] != "mine" || got.Env["B"] != "template" {
		t.Errorf("variables did not merge by name: %v", got.Env)
	}
	if len(got.Addons) != 0 || !slices.Equal(got.Apt, []string{"ripgrep"}) {
		t.Errorf("a request naming apt still took the template's add-ons: %v %v", got.Addons, got.Apt)
	}
	if got.GitAccess != store.GitAccessRepos || got.GitAccessPreferred {
		t.Errorf("the request's own git access lost to the template: %s", got.GitAccess)
	}
	// A saved preference ranks under the template; extra domains alone
	// join the template's preset.
	got = applyTemplate(store.SessionRequest{GitAccess: store.GitAccessRepos, GitAccessPreferred: true,
		EgressExtraDomains: []string{"mine.acme.com"}}, tmpl)
	if got.GitAccess != store.GitAccessAll || !got.GitAccessPreferred {
		t.Errorf("a preference beat the template: %s", got.GitAccess)
	}
	if got.Egress != "install" || !slices.Equal(got.EgressExtraDomains, []string{"mine.acme.com"}) {
		t.Errorf("extra domains alone: %s %v", got.Egress, got.EgressExtraDomains)
	}
}

// A template's list is the launch's own, narrowed by the configuration,
// and what is left is never an empty list, which would attach the
// default.
//
// @decision(DL-120) @decision(DL-075)
func TestATemplatesCredentialsNeverFallBackToTheDefault(t *testing.T) {
	cands, _ := candidates([]HeldRoute{held("npm", "npm.acme.com", ""), held("pypi", "pypi.acme.com", ""),
		held("asked", "a.acme.com", "asked")}, nil)
	attached := func(req store.RoutedRequest, tmpl store.TemplateSettings) []string {
		t.Helper()
		got := applyTemplate(store.SessionRequest{Routed: req}, store.Template{Name: "acme", TemplateSettings: tmpl}).Routed
		if got.Names != nil && len(got.Names) == 0 {
			t.Errorf("an empty list was left: %+v", got)
		}
		if len(req.Names) == 0 && len(got.Names) > 0 && len(got.Preferred) > 0 {
			t.Errorf("a list was left beside a narrowing: %+v", got)
		}
		out, _, err := attachRoutes(cands, got)
		if err != nil {
			t.Fatal(err)
		}
		return names(out)
	}
	list := store.TemplateSettings{Credentials: []string{"asked", "pypi"}}
	for _, c := range []struct {
		name string
		req  store.RoutedRequest
		tmpl store.TemplateSettings
		want []string
	}{
		{"a list attaches exactly, an asked one included", store.RoutedRequest{}, list, []string{"asked:user", "pypi:user"}},
		{"a narrowing still takes away", store.RoutedRequest{Preferred: []string{"pypi", "npm"}}, list, []string{"pypi:user"}},
		{"a narrowing that leaves nothing attaches nothing", store.RoutedRequest{Preferred: []string{"npm"}}, list, nil},
		{"[none] attaches nothing", store.RoutedRequest{Preferred: []string{NoCredentials}}, list, nil},
		{"the request's own list is not narrowed", store.RoutedRequest{Names: []string{"npm"}, Preferred: []string{"pypi"}}, list, []string{"npm:user"}},
		{"the request's none wins", store.RoutedRequest{None: true}, list, nil},
		{"no_credentials attaches none", store.RoutedRequest{}, store.TemplateSettings{NoCredentials: true}, nil},
		{"a template without credentials leaves the default", store.RoutedRequest{}, store.TemplateSettings{}, []string{"npm:user", "pypi:user"}},
		{"and its narrowing", store.RoutedRequest{Preferred: []string{"pypi"}}, store.TemplateSettings{}, []string{"pypi:user"}},
	} {
		if got := attached(c.req, c.tmpl); !slices.Equal(got, c.want) {
			t.Errorf("%s: attached %v, want %v", c.name, got, c.want)
		}
	}
}

// A template is written whole, listed by name, read back and removed,
// and a setting a launch would refuse is refused at the write.
//
// @decision(DL-119)
func TestATemplateIsCheckedWhenWritten(t *testing.T) {
	f := newAPIFixtureWith(t, withTemplates)
	putTemplate(t, f, "acme", TemplateSettingsView{Description: " the acme backend ", Egress: "install",
		EgressExtraDomains: []string{"pkg.acme.com"}, Env: map[string]string{"STAGE": "dev"}, Addons: []string{}})
	resp, out := f.do(http.MethodGet, "/api/v1/me/templates/acme", nil)
	var got TemplateView
	if err := json.Unmarshal(out, &got); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("get: %d %s", resp.StatusCode, out)
	}
	if got.Description != "the acme backend" || got.Egress != "install" || got.Env["STAGE"] != "dev" || got.Addons != nil || got.UpdatedAt.IsZero() {
		t.Errorf("read back %+v", got)
	}
	for name, body := range map[string]TemplateSettingsView{
		"a variable spov sets":           {Env: map[string]string{"SPOV_X": "1"}},
		"an unknown egress":              {Egress: "everywhere"},
		"domains without a preset":       {EgressExtraDomains: []string{"pkg.acme.com"}},
		"domains with open":              {Egress: "open", EgressExtraDomains: []string{"pkg.acme.com"}},
		"an unknown add-on":              {Addons: []string{"cobol"}},
		"a list and none":                {Credentials: []string{"npm"}, NoCredentials: true},
		"a credential name out of shape": {Credentials: []string{"NPM!"}},
		"an unknown git access":          {GitAccess: "everything"},
	} {
		if resp, out := f.do(http.MethodPut, "/api/v1/me/templates/bad", body); resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %s", name, resp.StatusCode, out)
		}
	}
	if resp, out := f.do(http.MethodPut, "/api/v1/me/templates/bad", map[string]any{"credentials": []string{}}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("an empty credential list: %d %s", resp.StatusCode, out)
	}
	for _, name := range []string{"Acme", "none", strings.Repeat("a", 41)} {
		if resp, _ := f.do(http.MethodPut, "/api/v1/me/templates/"+name, TemplateSettingsView{}); resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("the name %q was accepted", name)
		}
	}
	resp, out = f.do(http.MethodGet, "/api/v1/me/templates", nil)
	var list struct{ Templates []TemplateView }
	if err := json.Unmarshal(out, &list); err != nil || len(list.Templates) != 1 || list.Templates[0].Name != "acme" {
		t.Errorf("list: %d %s", resp.StatusCode, out)
	}
	for range 2 {
		if resp, out := f.do(http.MethodDelete, "/api/v1/me/templates/acme", nil); resp.StatusCode != http.StatusNoContent {
			t.Errorf("delete: %d %s", resp.StatusCode, out)
		}
	}
	if resp, _ := f.do(http.MethodGet, "/api/v1/me/templates/acme", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a removed template: %d", resp.StatusCode)
	}
	bare := newAPIFixture(t)
	if resp, _ := bare.do(http.MethodGet, "/api/v1/me/templates", nil); resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("a server that keeps none: %d", resp.StatusCode)
	}
}

// A user holds a bounded number, and replacing one at the bound is not
// one more.
//
// @decision(DL-119)
func TestAUserHoldsABoundedNumberOfTemplates(t *testing.T) {
	f := newAPIFixtureWith(t, withTemplates)
	for i := range MaxTemplatesPerUser {
		putTemplate(t, f, "t"+string(rune('a'+i/26))+string(rune('a'+i%26)), TemplateSettingsView{})
	}
	if resp, out := f.do(http.MethodPut, "/api/v1/me/templates/more", TemplateSettingsView{}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("one too many: %d %s", resp.StatusCode, out)
	}
	putTemplate(t, f, "taa", TemplateSettingsView{Description: "again"})
}

// One user's templates are not another's, and the file holds them
// across a restart.
//
// @decision(DL-119)
func TestTemplatesAreAUsersOwnAndOutliveTheServer(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), store.TemplatesName)
	for _, st := range []*store.FileTemplates{{}, {Path: path}} {
		mine := store.Template{OrgID: "o", UserID: "me", Name: "acme", TemplateSettings: store.TemplateSettings{Egress: "install"}}
		if err := st.Put(ctx, mine); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Get(ctx, "o", "you", "acme"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("another user read it: %v", err)
		}
		if list, _ := st.List(ctx, "o", "you"); len(list) != 0 {
			t.Errorf("another user lists %v", list)
		}
		if removed, _ := st.Delete(ctx, "o", "you", "acme"); removed {
			t.Error("another user removed it")
		}
	}
	again := &store.FileTemplates{Path: path}
	if got, err := again.Get(ctx, "o", "me", "acme"); err != nil || got.Egress != "install" {
		t.Errorf("after a restart: %+v, %v", got, err)
	}
}

// A launch that names a template records the resolved settings and the
// name, and a later change to the template changes no session.
//
// @decision(DL-120)
func TestALaunchRecordsWhatItsTemplateResolvedTo(t *testing.T) {
	f := newAPIFixtureWith(t, withTemplates)
	putTemplate(t, f, "acme", TemplateSettingsView{Egress: "install", Env: map[string]string{"STAGE": "dev", "REGION": "eu"}})
	req := repoReq("alpha")
	req.Template, req.Env = "acme", map[string]string{"STAGE": "mine"}
	id := f.create(req)
	launch := f.events(id)
	putTemplate(t, f, "acme", TemplateSettingsView{Egress: "agent"})
	sess, err := f.store.Sessions().Get(context.Background(), auth.Local().OrgID, id)
	if err != nil {
		t.Fatal(err)
	}
	r := sess.Request
	if r.Template != "acme" || r.Egress != "install" || r.Env["STAGE"] != "mine" || r.Env["REGION"] != "eu" {
		t.Errorf("recorded %+v", r)
	}
	resp, out := f.do(http.MethodGet, "/api/v1/sessions/"+id, nil)
	var view SessionView
	if err := json.Unmarshal(out, &view); err != nil || view.Template != "acme" {
		t.Errorf("the session view: %d %s", resp.StatusCode, out)
	}
	noted := false
	for _, e := range launch {
		noted = noted || strings.Contains(string(e.Data), "your template acme")
	}
	if !noted {
		t.Error("the launch's progress does not name the template")
	}
	req = repoReq("beta")
	req.Template = "gone"
	if resp, out := f.do(http.MethodPost, "/api/v1/sessions", req); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(out), "gone") {
		t.Errorf("a template the caller does not hold: %d %s", resp.StatusCode, out)
	}
}

// A template attaches an asked credential, and one it lists that the
// caller no longer holds refuses the launch, naming the template; the
// listing says which is missing.
//
// @decision(DL-120) @decision(DL-119)
func TestATemplateAttachesAnAskedCredentialAndRefusesAMissingOne(t *testing.T) {
	f, src := routedLaunchFixture(t)
	putTemplate(t, f.apiFixture, "acme", TemplateSettingsView{Credentials: []string{"acme-asked"}})
	req := repoReq("alpha")
	req.Template = "acme"
	f.id = f.create(req)
	f.events(f.id)
	routes := sidecarRoutes(t, f.provider.Created[0].Sidecar.Secrets)
	if len(routes) != 1 || routes[0].Name != "acme-asked" {
		t.Errorf("the sidecar holds %+v, want only the template's asked credential", routes)
	}
	p := auth.Local()
	if _, err := src.Delete(context.Background(), p.OrgID, p.UserID, "acme-asked"); err != nil {
		t.Fatal(err)
	}
	req = repoReq("beta")
	req.Template = "acme"
	resp, out := f.do(http.MethodPost, "/api/v1/sessions", req)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(out), "template acme") || !strings.Contains(string(out), "acme-asked") {
		t.Errorf("a missing credential: %d %s", resp.StatusCode, out)
	}
	_, out = f.do(http.MethodGet, "/api/v1/me/templates/acme", nil)
	var view TemplateView
	if err := json.Unmarshal(out, &view); err != nil || !slices.Equal(view.MissingCredentials, []string{"acme-asked"}) {
		t.Errorf("the listing: %s", out)
	}
}

// A job takes a template as a session does, and a template's git access
// is a preference under the request's own.
//
// @decision(DL-120)
func TestAJobTakesATemplate(t *testing.T) {
	f, _ := jobFixtureWith(t, withTemplates)
	putTemplate(t, f, "acme", TemplateSettingsView{Egress: "install", Env: map[string]string{"STAGE": "dev"}, GitAccess: store.GitAccessAll})
	req := jobRequest()
	req.Template = "acme"
	resp, out := f.do(http.MethodPost, "/api/v1/jobs", req)
	var view JobView
	if err := json.Unmarshal(out, &view); err != nil || resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create: %d %s", resp.StatusCode, out)
	}
	sess, err := f.store.Sessions().Get(context.Background(), auth.Local().OrgID, view.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	r := sess.Request
	if r.Template != "acme" || r.Egress != "install" || r.Env["STAGE"] != "dev" {
		t.Errorf("recorded %+v", r)
	}
	if r.GitAccess != store.GitAccessAll || !r.GitAccessPreferred {
		t.Errorf("git access %s, preferred %v", r.GitAccess, r.GitAccessPreferred)
	}
}

// The server expands a preset route itself, so a client that sends only
// the preset's options cannot leave the secret on every path of a
// shared host, and a name that is a platform's is refused.
//
// @decision(DL-118)
func TestTheServerExpandsAPresetRoute(t *testing.T) {
	f, src := routedFixture(t)
	body := map[string]any{"secret": "s3cretvalue", "route": map[string]any{
		"name": "team-npm", "preset": "npm", "url": "https://art.acme.com/api/npm/team-a/", "scope": "@acme"}}
	resp, out := f.do(http.MethodPut, "/api/v1/me/routed-credentials/team-npm", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a preset by its options: %d %s", resp.StatusCode, out)
	}
	p := auth.Local()
	heldRoutes, err := src.Open(context.Background(), p.OrgID, p.UserID)
	if err != nil || len(heldRoutes) != 1 {
		t.Fatalf("open: %v %v", heldRoutes, err)
	}
	r := heldRoutes[0].Route
	if !slices.Equal(r.Hosts, []string{"art.acme.com"}) || !slices.Equal(r.Paths, []string{"/api/npm/team-a/"}) || r.Scheme != "bearer" {
		t.Errorf("stored %+v", r)
	}
	for name, route := range map[string]map[string]any{
		"another host":   {"preset": "npm", "url": "https://art.acme.com/api/npm/team-a/", "hosts": []string{"evil.example.com"}},
		"another scheme": {"preset": "npm", "url": "https://art.acme.com/api/npm/team-a/", "scheme": "basic", "user": "x"},
		"a foreign path": {"preset": "npm", "url": "https://art.acme.com/api/npm/team-a/", "paths": []string{"/other/"}},
		"no url":         {"preset": "npm"},
	} {
		if resp, out := f.do(http.MethodPut, "/api/v1/me/routed-credentials/team-npm",
			map[string]any{"secret": "s3cretvalue", "route": withName(route, "team-npm")}); resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %s", name, resp.StatusCode, out)
		}
	}
	// A stored preset route without its path no longer validates.
	bare := routedcred.Route{Name: "x", Preset: "npm", URL: "https://art.acme.com/api/npm/team-a/",
		Hosts: []string{"art.acme.com"}, Scheme: "bearer"}.Normal()
	if err := bare.Validate(); err == nil {
		t.Error("a preset route with no path passed Validate")
	}
	if resp, out := f.do(http.MethodPut, "/api/v1/me/routed-credentials/github", map[string]any{"secret": "s3cretvalue",
		"route": routedcred.Route{Hosts: []string{"npm.acme.com"}, Scheme: "bearer"}}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("github as a name: %d %s", resp.StatusCode, out)
	}
}

func withName(route map[string]any, name string) map[string]any {
	route["name"] = name
	return route
}

// All from a template is a preference: where the deployment's credential
// may not reach every repository the launch falls back to repos and says
// so, while a request's own all is still refused.
//
// @decision(DL-120) @decision(DL-043)
func TestAllFromATemplateFallsBackWhereARequestsIsRefused(t *testing.T) {
	f, _ := newAccessFixture(t, TierOrg, withTemplates)
	putTemplate(t, f, "wide", TemplateSettingsView{GitAccess: store.GitAccessAll})
	req := repoReq("alpha")
	req.Template = "wide"
	id := f.create(req)
	evs := f.events(id)
	if !slices.ContainsFunc(evs, func(e event) bool {
		return e.Name == "note" && strings.Contains(string(e.Data), "git access: fake reach only")
	}) {
		t.Error("the fallback was not noted in the launch log")
	}
	if got := accessOf(f.session(id).Credentials); got["fake"] != store.GitAccessRepos {
		t.Errorf("a template's all recorded %v", got)
	}
	req = accessReq(store.GitAccessAll, false)
	req.Template = "wide"
	if resp, body := f.do(http.MethodPost, "/api/v1/sessions", req); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("a request's own all beside the template: %d %s", resp.StatusCode, body)
	}
}

// A template that names only apt packages is used whole in place of the
// deployment's add-ons and apt, as a request's own pair is.
//
// @decision(DL-120) @decision(DL-033)
func TestATemplatesPackagesReplaceTheDeploymentsPair(t *testing.T) {
	f := newAPIFixtureWith(t, func(d *Deps) {
		withTemplates(d)
		d.Defaults.Image, d.Defaults.Addons, d.Defaults.Apt = "", []string{"rust"}, []string{"graphviz"}
	})
	svc := f.srv.Service()
	tmpl := store.Template{Name: "acme", TemplateSettings: store.TemplateSettings{Apt: []string{"ffmpeg"}}}
	got := svc.withDefaults(applyTemplate(store.SessionRequest{}, tmpl))
	if len(got.Addons) != 0 || !slices.Equal(got.Apt, []string{"ffmpeg"}) {
		t.Errorf("add-ons %v, apt %v: the deployment's add-ons joined a template that named apt", got.Addons, got.Apt)
	}
	// Applied after the defaults, the template would find them set.
	if got := svc.withDefaults(store.SessionRequest{}); len(got.Addons) != 1 || !strings.HasPrefix(got.Addons[0], "rust") ||
		!slices.Equal(got.Apt, []string{"graphviz"}) {
		t.Errorf("with no template: add-ons %v, apt %v", got.Addons, got.Apt)
	}
}

// The organisation's route expands a preset as the personal one does.
//
// @decision(DL-118)
func TestTheOrganisationsRouteExpandsAPresetToo(t *testing.T) {
	f, src := orgRoutedFixture(t, true)
	resp, out := f.do(http.MethodPut, "/api/v1/org/routed-credentials/team-npm", map[string]any{"secret": "s3cretvalue",
		"route": map[string]any{"name": "team-npm", "preset": "npm", "url": "https://art.acme.com/api/npm/team-a/"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put: %d %s", resp.StatusCode, out)
	}
	views, err := src.List(context.Background(), auth.LocalID)
	if err != nil || len(views) != 1 || !slices.Equal(views[0].Route.Paths, []string{"/api/npm/team-a/"}) ||
		!slices.Equal(views[0].Route.Hosts, []string{"art.acme.com"}) {
		t.Errorf("stored %+v, %v", views, err)
	}
	if resp, out := f.do(http.MethodPut, "/api/v1/org/routed-credentials/team-npm", map[string]any{"secret": "s3cretvalue",
		"route": map[string]any{"name": "team-npm", "preset": "npm", "url": "https://art.acme.com/api/npm/team-a/",
			"hosts": []string{"evil.example.com"}}}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("a foreign host: %d %s", resp.StatusCode, out)
	}
}

// A hand-written empty list attaches nothing, never the default.
//
// @decision(DL-120)
func TestAnEmptyStoredListAttachesNothing(t *testing.T) {
	got := applyTemplate(store.SessionRequest{}, store.Template{Name: "x",
		TemplateSettings: store.TemplateSettings{Credentials: []string{}}}).Routed
	if !got.None || got.Names != nil {
		t.Errorf("an empty stored list left %+v", got)
	}
}
