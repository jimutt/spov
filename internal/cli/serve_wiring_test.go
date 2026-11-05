package cli

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/server"
)

// Every function seam on the Supervisor must be wired in serve.go. The
// supervisor is only built against Postgres, so this reads the source:
// setting fields on a bare struct would pass with serve.go wiring nothing.
//
// @decision(DL-045)
func TestServeWiresEverySupervisorSeam(t *testing.T) {
	src, err := os.ReadFile("serve.go")
	if err != nil {
		t.Fatal(err)
	}
	wiring := string(src)

	// The seams are discovered from the type, so a new one added to the
	// Supervisor fails here until serve.go sets it.
	sup := reflect.TypeOf(jobs.Supervisor{})
	var missing []string
	for i := range sup.NumField() {
		f := sup.Field(i)
		if !f.IsExported() || f.Type.Kind() != reflect.Func {
			continue
		}
		// Queue is wired in serve_river.go once River's client exists,
		// and Snooze by NewSupervisorWorker.
		if f.Name == "Queue" || f.Name == "Snooze" || f.Name == "Warn" {
			continue
		}
		if !strings.Contains(wiring, "Jobs."+f.Name+" =") {
			missing = append(missing, f.Name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("serve.go wires no %v on the supervisor;"+
			" a job would stall or panic on that path and no unit test would catch it", missing)
	}
}

// Queue and Snooze are wired where River's client is, and nowhere else.
//
// @decision(DL-045)
func TestTheQueueSeamsAreWiredWithRiver(t *testing.T) {
	src, err := os.ReadFile("serve_river.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "sup.Queue = ") {
		t.Error("serve_river.go does not wire Queue; nothing would enqueue a created job")
	}
	// Snooze is River's own sentinel and is wired by NewSupervisorWorker.
	worker, err := os.ReadFile("../jobs/supervisor_worker.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(worker), "sup.Snooze = river.JobSnooze") {
		t.Error("NewSupervisorWorker does not wire Snooze; every job would make one pass and never return")
	}
}

// Every On* recorder seam on the Service must be wired in serve.go. The
// seams are discovered from the type, so a new one fails here until
// serve.go sets it.
//
// @decision(DL-017)
func TestServeWiresTheRecorderToTheService(t *testing.T) {
	src, err := os.ReadFile("serve.go")
	if err != nil {
		t.Fatal(err)
	}
	wiring := string(src)

	svc := reflect.TypeOf(server.Service{})
	var missing []string
	for i := range svc.NumField() {
		f := svc.Field(i)
		// The recorder seams, and only those: Service has other
		// exported function fields that are not the recorder's.
		if !f.IsExported() || f.Type.Kind() != reflect.Func {
			continue
		}
		if !strings.HasPrefix(f.Name, "On") {
			continue
		}
		if !strings.Contains(wiring, f.Name+" = ") {
			missing = append(missing, f.Name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("serve.go wires no %v on the Service; nothing would fire on that"+
			" lifecycle moment and no unit test would notice", missing)
	}
}

// Gate 2's forge is resolved for the job's owner, never from deps.Forge.
// This reads the source, since the supervisor is built only against
// Postgres.
//
// @decision(DL-050)
func TestGateTwoIsWiredToTheJobOwnersForge(t *testing.T) {
	src, err := os.ReadFile("serve.go")
	if err != nil {
		t.Fatal(err)
	}
	// One line, because gofmt keeps this call on one. A longer resolver
	// name that made it wrap would fail here rather than pass silently,
	// which is the safe direction — widen the match then.
	line := ""
	for _, l := range strings.Split(string(src), "\n") {
		if strings.Contains(l, "Jobs.FindPullRequest =") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatal("serve.go wires no Jobs.FindPullRequest")
	}
	if !strings.Contains(line, "JobOwnerForge") {
		t.Errorf("gate 2 is wired as %q; it must resolve the forge per job owner"+
			" (svc.JobOwnerForge), or a deployment with only user credentials"+
			" can never verify a pull request", strings.TrimSpace(line))
	}
	if strings.Contains(line, "deps.Forge") {
		t.Errorf("gate 2 is wired to the deployment's forge in %q;"+
			" that is the DL-050 defect", strings.TrimSpace(line))
	}
}
