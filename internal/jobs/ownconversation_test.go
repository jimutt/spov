package jobs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/opencode/contract"
	"github.com/jimutt/spov/internal/store"
)

// A subagent's frames (capture_test.go).
const (
	subagentPrompt    = capturedChildEnqueued
	subagentDelivered = capturedChildDelivered
	subagentSucceeded = capturedChildSucceeded
)

// child records a subagent of the fixture's own conversation, created
// after it, and returns its row id.
func (f *followFixture) child(t *testing.T) string {
	t.Helper()
	conv, err := f.st.Conversations().UpsertConversation(context.Background(), store.Conversation{
		ID: store.NewID(time.Now()), OrgID: f.job.OrgID, SessionID: f.job.SessionID,
		OpenCodeID: capturedChild, ParentOpenCodeID: capturedRoot, CreatedAt: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	return conv.ID
}

// putIn is put in another conversation.
func (f *followFixture) putIn(t *testing.T, conv, name, raw string) {
	t.Helper()
	own := f.conv
	f.conv = conv
	defer func() { f.conv = own }()
	f.put(t, name, []byte(raw))
}

// A subagent's delivered prompt is the agent speaking, so it never steers
// the job, and its turn ending is not one of the run's; the job's own
// conversation still does both.
//
// @decision(DL-114)
func TestASubagentNeitherTakesTheJobOverNorEndsItsTurn(t *testing.T) {
	f := newFollowFixture(t, store.JobRunning)
	kid := f.child(t)
	f.putIn(t, kid, EventInboxEnqueued, subagentPrompt)
	f.putIn(t, kid, EventInboxDelivered, subagentDelivered)
	f.putIn(t, kid, "session.execution.succeeded", subagentSucceeded)
	res, err := f.sup.follow(context.Background(), f.job, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != store.JobRunning || res.TurnsEnded != 0 {
		t.Errorf("after a subagent's prompt and turn: %v with %d turns ended, want running with none", res.State, res.TurnsEnded)
	}

	f.put(t, "session.execution.succeeded", contract.Event(t, "session.execution.succeeded", 5, `{"sessionID":"`+capturedRoot+`"}`))
	f.putHuman(t)
	res, err = f.sup.follow(context.Background(), f.job, res.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != store.JobSteered || res.TurnsEnded != 1 {
		t.Errorf("the job's own turn and a person's message: %v with %d turns, want steered with 1", res.State, res.TurnsEnded)
	}
}

// A delivery read in a later pass joins only an enqueue of the job's own
// conversation, so a subagent's stored prompt cannot be matched to it.
//
// @decision(DL-114)
func TestAStoredSubagentPromptIsNotJoinedToADelivery(t *testing.T) {
	f := newFollowFixture(t, store.JobRunning)
	kid := f.child(t)
	f.putIn(t, kid, EventInboxEnqueued, subagentPrompt)
	first, err := f.sup.follow(context.Background(), f.job, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.put(t, EventInboxDelivered, []byte(subagentDelivered))
	res, err := f.sup.follow(context.Background(), f.job, first.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != store.JobRunning {
		t.Errorf("state = %v, want running", res.State)
	}
}

// What a person wrote is read from the job's own conversation: a
// subagent's prompt neither stops a proposed check deciding nor honours
// a withdrawal, and with no conversation known every one is read.
//
// @decision(DL-114) @decision(DL-050) @decision(DL-051)
func TestOnlyTheJobsOwnConversationIsAPersons(t *testing.T) {
	kid := store.Event{Source: store.SourceOpenCode, Name: EventInboxEnqueued, ConversationID: "conv_kid",
		At: time.Now(), Raw: contract.Frame(t, subagentPrompt)}
	mine := kid
	mine.ConversationID = "conv_job"
	permission := store.Event{Source: store.SourceEphemeral, Name: eventPermissionAsked, ConversationID: "conv_kid"}
	tool := store.Event{Source: store.SourceOpenCode, Name: "session.tool.called", ConversationID: "conv_kid"}

	kept := withoutOthersInbox([]store.Event{kid, permission, tool}, "conv_job")
	if personWrote(kept) || withdrawalHonoured(kept, time.Now().Add(-time.Hour)) {
		t.Error("a subagent's prompt was read as a person's")
	}
	if len(kept) != 2 {
		t.Errorf("kept %d events, want the subagent's ask and tool call", len(kept))
	}
	if !personWrote(withoutOthersInbox([]store.Event{kid, mine}, "conv_job")) {
		t.Error("the job's own user item was dropped")
	}
	if !personWrote(withoutOthersInbox([]store.Event{kid}, "")) {
		t.Error("with no conversation known, every one should be read as before")
	}
}

// Judge reads what a person wrote from the job's own conversation.
//
// @decision(DL-114)
func TestJudgeIgnoresASubagentsPrompt(t *testing.T) {
	f := newFollowFixture(t, store.JobDelivering)
	job := f.job
	job.CheckProposed, job.CheckCommand = true, "./check.sh"
	if err := f.st.Jobs().Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	kid := f.child(t)
	f.putIn(t, kid, EventInboxEnqueued, subagentPrompt)
	v, err := f.sup.Judge(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if g := gateNamed(v, GateMechanical); g.Outcome == GateUnverified && strings.Contains(g.Detail, "a person wrote") {
		t.Errorf("gate 1 says a person wrote: %q", g.Detail)
	}
}
