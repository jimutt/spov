package server

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/store"
)

// launchNoted reports whether a launch's progress carried text.
func launchNoted(events []event, text string) bool {
	for _, e := range events {
		if strings.Contains(string(e.Data), text) {
			return true
		}
	}
	return false
}

// A launch's plugins are stored, shown, written into the sandbox's
// OpenCode config and named in the launch's progress.
//
// @decision(DL-171)
func TestAPluginListIsStoredShownAndWritten(t *testing.T) {
	f := newAPIFixture(t)
	req := repoReq("plugins")
	req.Plugins = []string{"envsitter-guard@0.1.1", "opencode-rules"}
	id := f.create(req)
	launch := f.events(id)
	if view := f.viewOfSession(id); !slices.Equal(view.Plugins, req.Plugins) {
		t.Errorf("the view's plugins = %v", view.Plugins)
	}
	specs := f.provider.Specs()
	if len(specs) != 1 {
		t.Fatalf("%d sandboxes", len(specs))
	}
	script := strings.Join(specs[0].Command, " ")
	if !strings.Contains(script, `"plugins": [`) || !strings.Contains(script, `"envsitter-guard@0.1.1"`) || !strings.Contains(script, `"opencode-rules"`) {
		t.Errorf("the sandbox's config does not list the plugins:\n%s", script)
	}
	if !launchNoted(launch, "envsitter-guard@0.1.1, opencode-rules install from npm") {
		t.Error("the launch's progress does not name the plugins")
	}
	if launchNoted(launch, sandbox.PluginRegistryDomain+" is added") {
		t.Error("the open preset was said to need the registry")
	}
}

// A spec that is not an npm package, or too many, is a 422 that names
// it, before anything is made.
//
// @decision(DL-171)
func TestARefusedPluginIsRefusedAtCreate(t *testing.T) {
	f := newAPIFixture(t)
	for _, spec := range []string{"git+https://github.com/a/b", "../local", "pkg@file:../x"} {
		req := repoReq("refused")
		req.Plugins = []string{spec}
		res, body := f.do(http.MethodPost, "/api/v1/sessions", req)
		if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(body), "plugin") {
			t.Errorf("%s: %d %s", spec, res.StatusCode, body)
		}
	}
	if len(f.provider.Created) != 0 {
		t.Error("a sandbox was created for a refused plugin")
	}
}

// Under the agent preset a session with plugins also reaches the npm
// registry, and says so; without plugins the preset is unchanged.
//
// @decision(DL-172)
func TestAPluginSessionReachesTheRegistryUnderTheAgentPreset(t *testing.T) {
	f := newAPIFixture(t)
	req := repoReq("agent")
	req.Egress, req.Plugins = sandbox.PresetAgent, []string{"envsitter-guard"}
	launch := f.events(f.create(req))
	plain := repoReq("plain")
	plain.Egress = sandbox.PresetAgent
	f.events(f.create(plain))
	specs := f.provider.Specs()
	if len(specs) != 2 {
		t.Fatalf("%d sandboxes", len(specs))
	}
	if !slices.Contains(specs[0].Egress.Domains, sandbox.PluginRegistryDomain) {
		t.Errorf("the plugin session's egress lacks the registry: %v", specs[0].Egress.Domains)
	}
	if slices.Contains(specs[1].Egress.Domains, sandbox.PluginRegistryDomain) {
		t.Errorf("a session without plugins reaches the registry under agent: %v", specs[1].Egress.Domains)
	}
	if !launchNoted(launch, sandbox.PluginRegistryDomain+" is added to the agent egress preset") {
		t.Error("the launch does not say the registry was added")
	}
}

// A template's plugins apply when the request names none, and the
// request's own list wins; a template with a bad spec is refused when
// written.
//
// @decision(DL-171) @decision(DL-120)
func TestATemplatesPluginsApplyWhenTheRequestNamesNone(t *testing.T) {
	tmpl := store.Template{Name: "acme", TemplateSettings: store.TemplateSettings{Plugins: []string{"envsitter-guard"}}}
	if got := applyTemplate(store.SessionRequest{}, tmpl); !slices.Equal(got.Plugins, []string{"envsitter-guard"}) {
		t.Errorf("a bare request took %v", got.Plugins)
	}
	if got := applyTemplate(store.SessionRequest{Plugins: []string{"mine"}}, tmpl); !slices.Equal(got.Plugins, []string{"mine"}) {
		t.Errorf("the request's own list lost: %v", got.Plugins)
	}

	f := newAPIFixtureWith(t, withTemplates)
	putTemplate(t, f, "acme", TemplateSettingsView{Plugins: []string{"envsitter-guard@0.1.1"}})
	_, out := f.do(http.MethodGet, "/api/v1/me/templates/acme", nil)
	var view TemplateView
	if err := json.Unmarshal(out, &view); err != nil || !slices.Equal(view.Plugins, []string{"envsitter-guard@0.1.1"}) {
		t.Errorf("the stored template: %s", out)
	}
	req := repoReq("from-template")
	req.Template = "acme"
	id := f.create(req)
	f.events(id)
	if v := f.viewOfSession(id); !slices.Equal(v.Plugins, []string{"envsitter-guard@0.1.1"}) {
		t.Errorf("the launch's plugins = %v", v.Plugins)
	}
	resp, body := f.do(http.MethodPut, "/api/v1/me/templates/bad", TemplateSettingsView{Plugins: []string{"https://x.example/p.tgz"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(body), "plugin") {
		t.Errorf("a template with a bad plugin: %d %s", resp.StatusCode, body)
	}
}

// A job takes a template's other settings but not its plugins: a job's
// config carries none.
//
// @decision(DL-171)
func TestAJobDoesNotTakeATemplatesPlugins(t *testing.T) {
	f, _ := jobFixtureWith(t, withTemplates)
	putTemplate(t, f, "acme", TemplateSettingsView{Egress: "install", Plugins: []string{"envsitter-guard"}})
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
	if sess.Request.Egress != "install" || len(sess.Request.Plugins) != 0 {
		t.Errorf("the job's session request: egress %s, plugins %v", sess.Request.Egress, sess.Request.Plugins)
	}
}
