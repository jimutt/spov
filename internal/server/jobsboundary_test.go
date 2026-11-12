package server

import (
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/opencode"
)

// The values internal/jobs and internal/server each declare for
// themselves, pinned to each other from the side that can see both,
// since internal/jobs cannot import internal/server.
//
// @decision(DL-046)

// The recorder writes MarkerGap; the supervisor reads it to tell a gap
// from a signal it should act on.
//
// @decision(DL-046) @decision(DL-017)
func TestTheGapMarkerNamesMatch(t *testing.T) {
	if jobs.MarkerGap != MarkerGap {
		t.Errorf("internal/jobs reads %q as the gap marker; the recorder writes %q."+
			" A gap would be read as an ordinary event and a job's state"+
			" concluded from a range nothing was listening to",
			jobs.MarkerGap, MarkerGap)
	}
}

// The recorder writes MarkerStopped when it stops recording a session,
// and the server MarkerSteered on every send; a job's supervisor reads
// the two together, since a send after the stop has no inbox frame.
//
// @decision(DL-131)
func TestTheRecordingMarkerNamesMatch(t *testing.T) {
	if jobs.MarkerRecordingStopped != MarkerStopped {
		t.Errorf("internal/jobs reads %q as the recording's stop; the recorder writes %q."+
			" A person's handover to a job whose recording stopped would never be read",
			jobs.MarkerRecordingStopped, MarkerStopped)
	}
	if jobs.MarkerSteered != MarkerSteered {
		t.Errorf("internal/jobs reads %q as a send; the server writes %q", jobs.MarkerSteered, MarkerSteered)
	}
}

// The supervisor writes MarkerEvidence at delivery and gate 1 reads it
// back; the recorder's marker list still declares the name.
//
// @decision(DL-045) @decision(DL-050)
func TestTheEvidenceMarkerNamesMatch(t *testing.T) {
	if jobs.MarkerEvidence != MarkerEvidence {
		t.Errorf("internal/jobs writes evidence as %q; the recorder's list says %q",
			jobs.MarkerEvidence, MarkerEvidence)
	}
}

// turnEnded is defined on both sides of the package boundary and matches
// event names by suffix, so this asserts the behaviour rather than a
// literal — a new prefix must not need a change here to keep them in step.
//
// @decision(DL-017) @decision(DL-046)
func TestTurnEndedAgreesAcrossThePackages(t *testing.T) {
	for _, name := range []string{
		"session.execution.succeeded",
		"session.execution.failed",
		"session.execution.interrupted",
		"anything.execution.succeeded",
		"session.inbox.enqueued",
		"session.reasoning.ended",
		"",
		"execution.succeeded.but.not.at.the.end",
	} {
		if got, want := jobs.TurnEnded(name), turnEnded(name); got != want {
			t.Errorf("turnEnded(%q): internal/jobs says %v, the recorder says %v",
				name, got, want)
		}
	}
}

// OutDir must be internal/opencode's constant rather than a literal
// that could drift from the prompt the agent is given.
//
// @decision(DL-050) @decision(DL-039) @decision(DL-051)
func TestTheOutDirIsTheOpencodeConstant(t *testing.T) {
	if OutDir != opencode.OutDir || !strings.HasPrefix(OutDir, opencode.WorkspaceRoot+"/") {
		t.Errorf("OutDir = %q, want opencode's, under the workspace root", OutDir)
	}
}
