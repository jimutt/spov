// Package jobs is the job supervisor: the run loop, the job state
// machine, DL-050's three gates, DL-047's prep pass, the River worker
// and the sweeps. It is the *Supervisor half of the job code; job
// creation and the HTTP surface live in internal/server. It
// reaches the server only through the function seams on Supervisor,
// which DL-045 introduced for testability, and the compiler enforces
// that internal/jobs never imports internal/server.
//
// @decision(DL-046)
package jobs

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/store"
)

// ErrNoFile means a file delivery reads from the sandbox is not there, or
// is not a plain file. It is returned by the Collect and OutDir seams,
// which the server implements, and matched here, so it belongs to the
// seams' contract rather than either side.
//
// @decision(DL-046) @decision(DL-051)
var ErrNoFile = errors.New("jobs: no such file in the sandbox")

// ErrSandboxGone means the job's sandbox had stopped before delivery
// could read it, which is spov's inability to look rather than anything
// the job did.
//
// @decision(DL-050) @decision(DL-051)
var ErrSandboxGone = errors.New("jobs: the sandbox had already stopped")

// ErrFileTruncated means a file was larger than the bound it was read
// with. The bytes read are still returned.
//
// @decision(DL-046) @decision(DL-051)
var ErrFileTruncated = errors.New("jobs: the file was larger than its bound")

// Marker names this package writes into a session's events, in
// store.SpovNamespace, which only a spov-sourced row may carry.
// MarkerGap is the recorder's own and declared there too; this package
// only reads it, to tell a gap from a signal it should act on.
// TestTheGapMarkerNamesMatch pins the two together.
//
// @decision(DL-017) @decision(DL-058) @decision(DL-046)
const (
	MarkerGap      = store.SpovNamespace + "recorder.gap"
	MarkerEvidence = store.SpovNamespace + "job.evidence"
	// MarkerOutcome holds the plugin's outcome file, the copy of the
	// lines that decide the outcome (opencode.PluginOutcomePath).
	MarkerOutcome = store.SpovNamespace + "job.outcome"
	// MarkerCollected is written when a report job's out directory has
	// been collected, so a retry collects again only if it was not.
	MarkerCollected = store.SpovNamespace + "job.collected"
)

// Forge is the half of the server's forge the gates need: a pull request
// lookup under the credential the job's owner launched with (DL-050).
// Declared here rather than imported so the dependency points one way,
// and deliberately narrower than the server's Forge since the gates
// never list repositories.
//
// @decision(DL-050) @decision(DL-046)
type Forge interface {
	Selections(ctx context.Context, refs []store.RepoRef) ([]git.Selection, error)
}

// TurnEnded reports whether an event closes a turn. The recorder has its
// own copy for reading messages when a turn ends; this one decides a job
// signal. TestTurnEndedAgreesAcrossThePackages pins them together, from
// internal/server, since internal/jobs cannot import it (DL-046's wall)
// so the comparison has to be made from over there.
//
// @decision(DL-046)
func TurnEnded(name string) bool {
	return strings.HasSuffix(name, ".execution.succeeded") ||
		strings.HasSuffix(name, ".execution.failed") ||
		strings.HasSuffix(name, ".execution.interrupted")
}

// maxProjectDepth bounds how deep a frame is searched, matching the
// recorder's own projection bound.
//
// @decision(DL-018) @decision(DL-046)
const maxProjectDepth = 5

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
