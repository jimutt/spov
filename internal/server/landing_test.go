package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// viewOfSession reads a session's view through the API.
func (f *apiFixture) viewOfSession(id string) SessionView {
	f.t.Helper()
	_, raw := f.do(http.MethodGet, "/api/v1/sessions/"+id, nil)
	var view SessionView
	if err := json.Unmarshal(raw, &view); err != nil {
		f.t.Fatal(err)
	}
	return view
}

// An interactive session with one repository starts inside it, so
// OpenCode reads that repository's opencode.json and .opencode/.
//
// @decision(DL-170)
func TestASingleRepositorySessionStartsInsideIt(t *testing.T) {
	f := newAPIFixture(t)
	id := f.create(repoReq("one"))
	f.events(id)
	if view := f.viewOfSession(id); view.Landing != "/workspace/app" {
		t.Errorf("landing = %q, want /workspace/app", view.Landing)
	}
	specs := f.provider.Specs()
	if len(specs) != 1 || !strings.Contains(strings.Join(specs[0].Command, " "), "cd '/workspace/app'") {
		t.Errorf("the entrypoint does not start in the repository: %v", specs)
	}
}

// Several repositories still share the workspace, and a request's own
// landing wins over the default.
//
// @decision(DL-170) @decision(DL-037)
func TestASessionWithSeveralRepositoriesStartsInTheWorkspace(t *testing.T) {
	f := newAPIFixture(t)
	two := repoReq("two")
	two.Repos = append(two.Repos, RepoRef{Platform: "fake", Owner: "acme", Name: "lib"})
	id := f.create(two)
	f.events(id)
	if view := f.viewOfSession(id); view.Landing != "/workspace" {
		t.Errorf("landing = %q, want /workspace", view.Landing)
	}

	chosen := repoReq("chosen")
	chosen.Repos = append(chosen.Repos, RepoRef{Platform: "fake", Owner: "acme", Name: "lib"})
	chosen.Landing = "lib"
	id = f.create(chosen)
	f.events(id)
	if view := f.viewOfSession(id); view.Landing != "/workspace/lib" {
		t.Errorf("landing = %q, want /workspace/lib", view.Landing)
	}
}
