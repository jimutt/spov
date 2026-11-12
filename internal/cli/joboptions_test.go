package cli

import (
	"bytes"
	"context"
	"net"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
	"github.com/jimutt/spov/internal/store"
)

// `spov jobs create --pr-policy goal --no-prep` sends both options, and
// the job is stored with them.
//
// @decision(DL-137) @decision(DL-138)
func TestJobsCreateSendsThePullRequestPolicyAndNoPrep(t *testing.T) {
	f := newAPIFixture(t)
	f.deps.Jobs = &jobs.Supervisor{
		Store: f.deps.Store, Provider: f.provider,
		Queue: func(context.Context, jobs.JobArgs) error { return nil },
		Warn:  func(string) {},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(f.deps, ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	hs := httptest.NewUnstartedServer(srv.Handler())
	hs.Listener = ln
	hs.Start()
	t.Cleanup(hs.Close)
	env := f.env
	env.Client = client.New(hs.URL, f.token)

	out, _, err := runWith(t, env, "jobs", "create", "add to the open branch", "--repo", "alice/spov",
		"--pr-policy", "goal", "--no-prep")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	id := strings.Fields(out)[1]
	got, err := f.deps.Store.Jobs().Get(context.Background(), auth.LocalID, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.PRPolicy != store.PRPolicyGoal || !got.SkipPrep {
		t.Errorf("stored pr policy %q, skip prep %v; want goal, true", got.PRPolicy, got.SkipPrep)
	}
}

// --pr-policy is refused before the server is asked on another kind or
// with a value that is neither spov nor goal.
//
// @decision(DL-137)
func TestJobsCreateRefusesAPullRequestPolicyItCannotSend(t *testing.T) {
	env := &Env{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}
	for _, opts := range []createOptions{
		{Goal: "g", Kind: "report", PRPolicy: "goal"},
		{Goal: "g", Kind: "pr", PRPolicy: "theirs"},
	} {
		err := createJob(context.Background(), env, opts)
		if err == nil || !strings.Contains(err.Error(), "--pr-policy") {
			t.Errorf("%+v: %v, want a refusal naming --pr-policy", opts, err)
		}
	}
}

// The detail view says when a job's goal opens its pull request and when
// it skipped the preparation pass, and says nothing at the defaults.
//
// @decision(DL-137) @decision(DL-138)
func TestJobDetailShowsTheJobsOptions(t *testing.T) {
	for _, tt := range []struct {
		view server.JobView
		want bool
	}{
		{server.JobView{ID: "01JOB", Kind: store.JobKindPR, State: store.JobQueued, PRPolicy: store.PRPolicyGoal, SkipPrep: true}, true},
		{server.JobView{ID: "01JOB", Kind: store.JobKindPR, State: store.JobQueued}, false},
	} {
		var out bytes.Buffer
		if err := writeJobDetail(&Env{Out: &out, Err: &bytes.Buffer{}}, tt.view); err != nil {
			t.Fatal(err)
		}
		for _, line := range []string{"as the goal says", "Preparation"} {
			if strings.Contains(out.String(), line) != tt.want {
				t.Errorf("%+v: shows %q = %v:\n%s", tt.view, line, !tt.want, out.String())
			}
		}
	}
}
