package session

import (
	"testing"

	"github.com/jimutt/spov/internal/opencode"
)

// An interactive launch must get no plugin and no job tag: DL-040
// substitutes the plugin's policy for DL-038's config, and a session
// that got both would have two mechanisms answering the same question.
func TestAnInteractiveLaunchHasNoJob(t *testing.T) {
	if got := jobPlugin(nil, nil, opencode.WorkspaceRoot); got != nil {
		t.Errorf("an interactive launch got a plugin: %+v", got)
	}
	if got := jobID(nil); got != "" {
		t.Errorf("an interactive launch got the job tag %q", got)
	}
}

// A job launch derives its permission allowlist from its kind, so a
// caller cannot pair one kind with another's policy.
func TestAJobLaunchDerivesItsPolicyFromItsKind(t *testing.T) {
	for _, kind := range []string{"pr", "report"} {
		p := jobPlugin(&JobOptions{ID: "job_1", Kind: kind, Goal: "g"}, nil, opencode.WorkspaceRoot)
		if p == nil {
			t.Fatalf("kind %q produced no plugin", kind)
		}
		want := opencode.PermissionsForKind(kind)
		if len(p.Permissions.Allow) != len(want.Allow) {
			t.Errorf("kind %q got %v, want %v", kind, p.Permissions.Allow, want.Allow)
		}
		for i, a := range want.Allow {
			if p.Permissions.Allow[i] != a {
				t.Errorf("kind %q allow[%d] = %q, want %q", kind, i, p.Permissions.Allow[i], a)
			}
		}
	}
}

// The tag is what makes a duplicate a re-attach rather than a second
// billable sandbox.
func TestAJobLaunchCarriesItsID(t *testing.T) {
	if got := jobID(&JobOptions{ID: "job_7"}); got != "job_7" {
		t.Errorf("jobID = %q, want job_7", got)
	}
}

// The job's goal survives the launch into the plugin.
func TestTheGoalReachesThePlugin(t *testing.T) {
	p := jobPlugin(&JobOptions{ID: "j", Kind: "report", Goal: "g"}, nil, opencode.WorkspaceRoot)
	if p.Goal != "g" {
		t.Errorf("Goal = %q, want g", p.Goal)
	}
}
