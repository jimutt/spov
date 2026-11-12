package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/session"
	"github.com/jimutt/spov/internal/store"
)

// A pr job's pull request policy and skip_prep are stored on the job and
// shown on its view only when they are not the default. spov's policy is
// stored empty, as no policy is.
//
// @decision(DL-137) @decision(DL-138)
func TestAJobsOptionsAreStoredAndShown(t *testing.T) {
	f, _ := jobFixture(t)
	ctx := context.Background()

	req := jobRequest()
	req.PRPolicy = store.PRPolicyGoal
	req.SkipPrep = true
	v := createJobView(t, f, req)
	if v.PRPolicy != store.PRPolicyGoal || !v.SkipPrep {
		t.Errorf("view = pr_policy %q, skip_prep %v; want goal, true", v.PRPolicy, v.SkipPrep)
	}
	got, err := f.store.Jobs().Get(ctx, auth.LocalID, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PRPolicy != store.PRPolicyGoal || !got.SkipPrep {
		t.Errorf("row = pr_policy %q, skip_prep %v; want goal, true", got.PRPolicy, got.SkipPrep)
	}

	spov := jobRequest()
	spov.Goal = "a second job"
	spov.PRPolicy = store.PRPolicySpov
	resp, body := f.do(http.MethodPost, "/api/v1/jobs", spov)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	if bytes.Contains(body, []byte(`"pr_policy"`)) || bytes.Contains(body, []byte(`"skip_prep"`)) {
		t.Errorf("a job at its defaults shows its options: %s", body)
	}
	var dv JobView
	if err := json.Unmarshal(body, &dv); err != nil {
		t.Fatal(err)
	}
	row, err := f.store.Jobs().Get(ctx, auth.LocalID, dv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.PRPolicy != "" || row.SkipPrep {
		t.Errorf("spov's policy was stored as %q, skip_prep %v; want empty, false", row.PRPolicy, row.SkipPrep)
	}
}

// pr_policy is valid only on a pr job and only as spov or goal; skip_prep
// is valid on every kind.
//
// @decision(DL-137) @decision(DL-138)
func TestOnlyAPRJobTakesAPullRequestPolicy(t *testing.T) {
	f, _ := jobFixture(t)
	report := jobRequest()
	report.Kind = store.JobKindReport
	report.PRPolicy = store.PRPolicyGoal
	reportSpov := jobRequest()
	reportSpov.Kind = store.JobKindReport
	reportSpov.PRPolicy = store.PRPolicySpov
	unknown := jobRequest()
	unknown.PRPolicy = "theirs"
	for name, req := range map[string]JobRequest{
		"a report job with goal": report, "a report job with spov": reportSpov, "an unknown value": unknown,
	} {
		if resp, body := f.do(http.MethodPost, "/api/v1/jobs", req); resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %s, want 422", name, resp.StatusCode, body)
		}
	}
	// The service refuses it too, for a caller that skips the API's own
	// validation.
	if _, err := f.srv.svc.jobSessionRequest(unknown, "j", nil); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("the service took an unknown policy: %v", err)
	}
	skipping := jobRequest()
	skipping.Kind = store.JobKindReport
	skipping.SkipPrep = true
	if v := createJobView(t, f, skipping); !v.SkipPrep {
		t.Error("a report job lost skip_prep")
	}
}

// The policy reaches the launch from the job's row, which is where the
// plugin's goal and the generated guide are built from.
//
// @decision(DL-137)
func TestAJobsLaunchCarriesItsPullRequestPolicy(t *testing.T) {
	var got *session.JobOptions
	s := testService(t, func(d *Deps) {
		d.Launch = func(_ context.Context, opts session.Options, _ session.StepReporter) (*session.Session, error) {
			got = opts.Job
			return nil, errors.New("stop here; the options are what this asserts")
		}
		d.Forge = &fakeForge{platform: git.NewFake()}
	})
	pr := seedJobSessionWith(t, s, store.StateStarting, "", func(j *store.Job) {
		j.Kind = store.JobKindPR
		j.PRPolicy = store.PRPolicyGoal
	})
	_ = s.ProvisionJob(context.Background(), pr)
	if got == nil || got.PRPolicy != string(store.PRPolicyGoal) || got.Kind != "pr" {
		t.Fatalf("the launch carried %+v, want the goal policy", got)
	}
}

// The plugin reads the policy by the store's spelling.
//
// @decision(DL-137)
func TestThePullRequestPolicyNamesAgree(t *testing.T) {
	if string(store.PRPolicyGoal) != opencode.PRPolicyGoal {
		t.Errorf("store %q, opencode %q", store.PRPolicyGoal, opencode.PRPolicyGoal)
	}
}
