package server

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/auth"
)

// A session's variables are the deployment's with the request's over
// them, stored, shown, and set in the plain environment of its sandbox.
//
// @decision(DL-096)
func TestASessionsVariablesAreStoredShownAndSet(t *testing.T) {
	f := newAPIFixtureWith(t, func(d *Deps) {
		d.Defaults.Env = map[string]string{"BASE": "1", "NODE_ENV": "production"}
	})
	req := repoReq("env")
	req.Env = map[string]string{"NODE_ENV": "test", "OPENCODE_LOG_LEVEL": "debug"}
	id := f.create(req)
	f.events(id)
	want := map[string]string{"BASE": "1", "NODE_ENV": "test", "OPENCODE_LOG_LEVEL": "debug"}

	_, raw := f.do(http.MethodGet, "/api/v1/sessions/"+id, nil)
	var view SessionView
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(view.Env, want) {
		t.Errorf("the view's env = %v, want %v", view.Env, want)
	}
	specs := f.provider.Specs()
	if len(specs) != 1 {
		t.Fatalf("%d sandboxes", len(specs))
	}
	for k, v := range want {
		if specs[0].Env[k] != v {
			t.Errorf("the sandbox's %s = %q, want %q", k, specs[0].Env[k], v)
		}
		if _, ok := specs[0].Secrets[k]; ok {
			t.Errorf("%s went into the Secret", k)
		}
	}
}

// A refused variable is a 422 that names it, before anything is made.
//
// @decision(DL-097)
func TestARefusedVariableIsRefusedAtCreate(t *testing.T) {
	f := newAPIFixture(t)
	for _, name := range []string{"GITHUB_TOKEN", "path", "SPOV_X", "OPENCODE_CONFIG", "ANTHROPIC_API_KEY"} {
		req := repoReq("refused")
		req.Env = map[string]string{name: "x"}
		res, body := f.do(http.MethodPost, "/api/v1/sessions", req)
		if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(body), name) {
			t.Errorf("%s: %d %s", name, res.StatusCode, body)
		}
	}
	if n := len(f.provider.Specs()); n != 0 {
		t.Errorf("%d sandboxes were created", n)
	}
}

// A resumed session's new sandbox gets the stored variables again.
//
// @decision(DL-096)
func TestAResumedSessionGetsItsVariablesAgain(t *testing.T) {
	f := resumable(t, func(d *Deps) { d.Defaults.Env = map[string]string{"NODE_ENV": "test"} })
	f.srv.svc.CollectSnapshots(context.Background())
	if code, _, body := f.resume(map[string]any{}); code != http.StatusAccepted {
		t.Fatalf("resume: %d %s", code, body)
	}
	f.waitRunning()
	specs := f.provider.Specs()
	if len(specs) < 2 {
		t.Fatalf("%d sandboxes", len(specs))
	}
	if got := specs[len(specs)-1].Env["NODE_ENV"]; got != "test" {
		t.Errorf("the resumed sandbox's NODE_ENV = %q", got)
	}
}

// A job's session carries the job's variables, checked as a session's.
//
// @decision(DL-096) @decision(DL-097)
func TestAJobsSessionCarriesItsVariables(t *testing.T) {
	f, _ := jobFixture(t)
	req := jobRequest()
	req.Env = map[string]string{"NODE_ENV": "test"}
	resp, body := f.do(http.MethodPost, "/api/v1/jobs", req)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	var view JobView
	_ = json.Unmarshal(body, &view)
	sess, err := f.store.Sessions().Get(context.Background(), auth.LocalID, view.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Request.Env["NODE_ENV"] != "test" {
		t.Errorf("the job's session holds %v", sess.Request.Env)
	}

	req.Env = map[string]string{"GH_TOKEN": "x"}
	if resp, body := f.do(http.MethodPost, "/api/v1/jobs", req); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("a refused variable on a job: %d %s", resp.StatusCode, body)
	}
}
