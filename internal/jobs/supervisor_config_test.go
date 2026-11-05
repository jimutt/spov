package jobs

import (
	"testing"
	"time"

	"github.com/jimutt/spov/internal/config"
)

// River refuses a client whose RescueStuckJobsAfter does not exceed its
// JobTimeout, and the failure is a rescheduled job that is merely still
// running — a second sandbox for work already in flight.
func TestSupervisorRescueExceedsItsTimeout(t *testing.T) {
	if SupervisorRescueAfter <= SupervisorJobTimeout {
		t.Errorf("RescueStuckJobsAfter %v must exceed JobTimeout %v",
			SupervisorRescueAfter, SupervisorJobTimeout)
	}
}

// TestOnePassHasRoomToProvision checks JobTimeout is bounded from below
// by what one pass needs (a launch), not from above by the sandbox's
// life: an attempt advances the job one step and snoozes,
// so a timeout shorter than a launch would cut off provisioning halfway
// and leave an abandoned sandbox.
//
// @decision(DL-045) @decision(DL-046)
func TestOnePassHasRoomToProvision(t *testing.T) {
	if SupervisorJobTimeout <= ProvisionTimeout {
		t.Errorf("JobTimeout %v does not exceed the launch budget %v;"+
			" an attempt cut off mid-launch leaves a sandbox nothing is watching",
			SupervisorJobTimeout, ProvisionTimeout)
	}
	// And it must stay well under the sandbox ceiling, or a wedged
	// pass holds one of SupervisorMaxWorkers slots for hours.
	if SupervisorJobTimeout >= config.MaxSandboxTimeout {
		t.Errorf("JobTimeout %v is at or above the sandbox ceiling %v;"+
			" an attempt is one pass now, not the whole run",
			SupervisorJobTimeout, config.MaxSandboxTimeout)
	}
}

// River's own defaults are what this exists to override.
func TestSupervisorDoesNotInheritRiverDefaults(t *testing.T) {
	// River's JobTimeoutDefault is one minute.
	if SupervisorJobTimeout <= time.Minute {
		t.Error("JobTimeout is at or below River's one-minute default")
	}
	// River's MaxAttemptsDefault is 25, which would re-provision a
	// sandbox 25 times for a job whose goal is what is broken.
	if SupervisorMaxAttempts >= 25 {
		t.Errorf("MaxAttempts %d is not narrowed from River's default", SupervisorMaxAttempts)
	}
}
