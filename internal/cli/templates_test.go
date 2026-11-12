package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/store"
)

func (f *apiFixture) putTemplate(name string, s store.TemplateSettings) {
	f.t.Helper()
	p := auth.Local()
	if err := f.deps.Templates.Put(context.Background(), store.Template{OrgID: p.OrgID, UserID: p.UserID, Name: name, TemplateSettings: s}); err != nil {
		f.t.Fatal(err)
	}
}

// With a template named, the config files' values for what a template
// covers are not sent, a flag's is, and the launch says what the
// template took the place of.
//
// @decision(DL-122)
func TestATemplateTakesThePlaceOfTheConfigFiles(t *testing.T) {
	f := newAPIFixtureWithConfig(t, defaultConfig+"sandbox:\n  egress: agent\n  credentials: [acme-npm]\n  env:\n    FROM_USER: \"1\"\n")
	writeProject(t, f.wd, "sandbox:\n  egress: open\n  env:\n    FROM_PROJECT: \"1\"\n")
	f.putTemplate("acme", store.TemplateSettings{Egress: "install", Env: map[string]string{"STAGE": "dev", "FLAGGED": "template"}})
	_, stderr, err := runWith(t, f.env, "fly", "--keep", "--name", "templated", "--template", "acme", "--env", "FLAGGED=flag")
	if err != nil {
		t.Fatalf("fly: %v\n%s", err, stderr)
	}
	r := f.session("templated").Request
	if r.Template != "acme" || r.Egress != "install" {
		t.Errorf("template %q, egress %q: a config file's egress beat the template's", r.Template, r.Egress)
	}
	if r.Env["STAGE"] != "dev" || r.Env["FLAGGED"] != "flag" {
		t.Errorf("env %v: want the template's, with the flag over it", r.Env)
	}
	if _, sent := r.Env["FROM_PROJECT"]; sent {
		t.Errorf("env %v: a project file's variable was sent under a template", r.Env)
	}
	if !strings.Contains(stderr, "takes the place of") || !strings.Contains(stderr, "sandbox.env") {
		t.Errorf("the launch does not say what the template replaced:\n%s", stderr)
	}
	// The narrowing still goes: it can only take access away.
	if got := r.Routed.Preferred; len(got) != 1 || got[0] != "acme-npm" {
		t.Errorf("the configuration's narrowing was not sent under a template: %v", got)
	}
	// Without a template the same files apply as they always have.
	if _, stderr, err := runWith(t, f.env, "fly", "--keep", "--name", "plain"); err != nil {
		t.Fatalf("fly: %v\n%s", err, stderr)
	}
	if r := f.session("plain").Request; r.Template != "" || r.Egress != "open" || r.Env["FROM_PROJECT"] != "1" {
		t.Errorf("a plain launch: template %q, egress %q, env %v", r.Template, r.Egress, r.Env)
	}
}

// The user file may name a template, --no-template launches without it,
// a job takes only its own key, and a configured one that is gone names
// its key.
//
// @decision(DL-122)
func TestTheUserFileMayNameATemplate(t *testing.T) {
	f := newAPIFixtureWithConfig(t, defaultConfig+"sandbox:\n  template: acme\n")
	_, _, err := runWith(t, f.env, "fly", "--keep", "--name", "gone")
	if err == nil || !strings.Contains(err.Error(), "sandbox.template") || !strings.Contains(err.Error(), "--no-template") {
		t.Fatalf("a configured template the server lacks: %v", err)
	}
	f.putTemplate("acme", store.TemplateSettings{Egress: "install"})
	if _, stderr, err := runWith(t, f.env, "fly", "--keep", "--name", "configured"); err != nil {
		t.Fatalf("fly: %v\n%s", err, stderr)
	}
	if r := f.session("configured").Request; r.Template != "acme" || r.Egress != "install" {
		t.Errorf("template %q, egress %q", r.Template, r.Egress)
	}
	if _, stderr, err := runWith(t, f.env, "fly", "--keep", "--name", "without", "--no-template"); err != nil {
		t.Fatalf("fly: %v\n%s", err, stderr)
	}
	if r := f.session("without").Request; r.Template != "" {
		t.Errorf("--no-template launched from %q", r.Template)
	}

	cfg := config.Config{}
	cfg.Sandbox.Template, cfg.Sandbox.JobTemplate = "acme", ""
	if name, _, _ := templateFor(cfg, templateFlags{}, true); name != "" {
		t.Errorf("a job took sandbox.template: %q", name)
	}
	cfg.Sandbox.JobTemplate = "jobs"
	if name, key, configured := templateFor(cfg, templateFlags{}, true); name != "jobs" || key != "sandbox.job_template" || !configured {
		t.Errorf("a job's own key: %q %q %v", name, key, configured)
	}
	if name, _, configured := templateFor(cfg, templateFlags{Name: "flag"}, false); name != "flag" || configured {
		t.Errorf("the flag: %q %v", name, configured)
	}
}

// A project file comes with a repository, so it may not choose a
// template, which can attach credentials.
//
// @decision(DL-122)
func TestAProjectFileCannotNameATemplate(t *testing.T) {
	for _, key := range []string{"template", "job_template"} {
		f := newAPIFixtureWithConfig(t, defaultConfig)
		writeProject(t, f.wd, "sandbox:\n  "+key+": acme\n")
		_, _, err := runWith(t, f.env, "fly", "--keep", "--name", "evil")
		if err == nil || !strings.Contains(err.Error(), "a project file may not set") || !strings.Contains(err.Error(), "sandbox."+key) {
			t.Errorf("%s in a project file: %v", key, err)
		}
		if f.provider.Running() != 0 {
			t.Error("a sandbox was created")
		}
	}
}

// The group lists, shows and removes; --json is the API's view and
// nothing else on stdout.
//
// @decision(DL-122) @decision(DL-023)
func TestTemplatesAreListedShownAndRemoved(t *testing.T) {
	f := newAPIFixtureWithConfig(t, defaultConfig)
	if out, stderr, err := runWith(t, f.env, "templates"); err != nil || out != "" || !strings.Contains(stderr, "Settings") {
		t.Errorf("with none: %q %q %v", out, stderr, err)
	}
	f.putTemplate("acme", store.TemplateSettings{Description: "the backend", Egress: "install", NoCredentials: true,
		Env: map[string]string{"STAGE": "dev"}})
	out, _, err := runWith(t, f.env, "templates", "list")
	if err != nil || !strings.Contains(out, "acme") || !strings.Contains(out, "egress install; no credentials; 1 variables") {
		t.Errorf("list: %v\n%s", err, out)
	}
	out, _, err = runWith(t, f.env, "templates", "--json")
	var body struct {
		Templates []server.TemplateView `json:"templates"`
	}
	if err != nil || json.Unmarshal([]byte(out), &body) != nil || len(body.Templates) != 1 || body.Templates[0].Egress != "install" {
		t.Errorf("list --json: %v\n%s", err, out)
	}
	out, _, err = runWith(t, f.env, "templates", "show", "acme", "--json")
	var view server.TemplateView
	if err != nil || json.Unmarshal([]byte(out), &view) != nil || view.Name != "acme" || view.Env["STAGE"] != "dev" {
		t.Errorf("show --json: %v\n%s", err, out)
	}
	if out, _, err := runWith(t, f.env, "templates", "show", "acme"); err != nil || !strings.Contains(out, "STAGE=dev") {
		t.Errorf("show: %v\n%s", err, out)
	}
	if got := completions(t, f.env, "templates", "show", ""); len(got) != 1 || got[0] != "acme" {
		t.Errorf("completion offers %v", got)
	}
	if _, _, err := runWith(t, f.env, "templates", "remove", "acme"); err != nil {
		t.Errorf("remove: %v", err)
	}
	if _, _, err := runWith(t, f.env, "templates", "remove", "acme"); err == nil {
		t.Error("removing one that is not there said nothing")
	}
}

// Completing a template's name asks only a server that is already up.
//
// @decision(DL-122) @decision(DL-023)
func TestTemplateCompletionNeverStartsAServer(t *testing.T) {
	env := Env{StateDir: t.TempDir(), Spawn: func(context.Context) error { t.Error("a completion spawned a server"); return nil }}
	for _, args := range [][]string{{"templates", "show", ""}, {"templates", "remove", ""}, {"fly", "--template", ""}} {
		if got := completions(t, env, args...); len(got) != 0 {
			t.Errorf("%v with no server running offers %v", args, got)
		}
	}
}
