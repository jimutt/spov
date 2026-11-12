package jobs

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/jobs/jobstest"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/store"
)

// The verdict's tests. judge() takes rows and returns a conclusion, so
// every gate is exercised from fixtures, with no queue, database or sandbox.
//
// @decision(DL-050)

// seedMessages writes messages against a conversation for the job's
// session, which is what the verdict reads them from.
func seedMessages(t *testing.T, st store.Store, job store.Job, msgs []store.Message) {
	t.Helper()
	ctx := context.Background()
	conv, err := st.Conversations().UpsertConversation(ctx, store.Conversation{
		ID: store.NewID(time.Now()), OrgID: job.OrgID, SessionID: job.SessionID,
		OpenCodeID: "ses_verdict",
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := range msgs {
		msgs[i].ID = store.NewID(time.Now())
		msgs[i].OrgID = job.OrgID
		msgs[i].SessionID = job.SessionID
		msgs[i].ConversationID = conv.ID
		msgs[i].OpenCodeID = "msg_" + msgs[i].ID
		msgs[i].At = time.Now().Add(time.Duration(i) * time.Second)
	}
	if _, err := st.Conversations().PutMessages(ctx, msgs); err != nil {
		t.Fatal(err)
	}
}

// checkedJob is a job that asked for a check, which is what gate 1
// requires before it will read one at all.
func checkedJob() store.Job {
	return store.Job{ID: "j", OrgID: "o", Kind: store.JobKindReport, CheckCommand: "./scripts/check.sh"}
}

// evidenceEvent builds the marker the supervisor writes at delivery,
// carrying JSONL lines the plugin wrote.
func evidenceEvent(lines ...string) store.Event {
	return store.Event{
		Source: store.SourceSpov,
		Name:   MarkerEvidence,
		Text:   strings.Join(lines, "\n"),
	}
}

// checkLine renders one check.finished line in the shape the plugin's
// record() actually writes: flat, with the fields spread beside the
// event name.
func checkLine(fields map[string]any) string {
	fields["event"] = opencode.CheckEvent
	fields["at"] = "2026-09-04T10:00:00.000Z"
	b, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// toolMessage builds a stored message carrying tool parts, in the shape
// spike S6 measured.
func toolMessage(parts ...string) store.Message {
	return store.Message{
		Role: "assistant",
		Raw:  []byte(`{"id":"msg_1","type":"assistant","content":[` + strings.Join(parts, ",") + `]}`),
	}
}

const (
	// The refused shape, captured verbatim by spike S6.
	partRefusedByHuman = `{"type":"tool","name":"shell","executed":false,
	  "state":{"status":"error",
	           "error":{"type":"aborted","message":"The user declined this tool call"}}}`
	// The refused shape spike S3 measured from the plugin's own hook.
	partRefusedByPolicy = `{"type":"tool","name":"shell","executed":false,
	  "state":{"status":"error",
	           "error":{"type":"permission.rejected","message":"Permission denied: shell"}}}`
	partRan = `{"type":"tool","name":"read","executed":true,
	  "state":{"status":"completed"}}`
	partText = `{"type":"text","text":"I have finished the task."}`
)

// TestGateOneReadsTheLineShapeThePluginWrites pins gate 1 to the flat
// `{event, at, ...data}` line record() writes. A reader expecting a nested
// `data` would leave gate 1 silently unverified on every job.
//
// @decision(DL-050) @decision(DL-039)
func TestGateOneReadsTheLineShapeThePluginWrites(t *testing.T) {
	// Built the way the plugin builds it, rather than the way this
	// code would like to read it.
	line := checkLine(map[string]any{"exit": 0, "command": "./scripts/check.sh", "output": ""})
	if !strings.Contains(line, `"event":"check.finished"`) || strings.Contains(line, `"data":`) {
		t.Fatalf("the fixture is not the plugin's shape: %s", line)
	}

	// Distractor lines, so a reader that ignored the event name fails.
	others := []string{
		`{"event":"setup","at":"2026-09-04T09:59:00.000Z","kind":"report"}`,
		`{"event":"continue.sent","at":"2026-09-04T09:59:30.000Z","continuations":1}`,
		`{"event":"loop.stopped","at":"2026-09-04T10:00:01.000Z","reason":"limit","exit":1}`,
	}
	ev := evidenceEvent(others[0], others[1], line, others[2])

	got := judgeMechanical(checkedJob(), []store.Event{ev})
	if got.Outcome != GatePassed {
		t.Fatalf("outcome = %s (%s), want passed: gate 1 cannot read the line the plugin writes",
			got.Outcome, got.Detail)
	}

	// And a file with every OTHER line but no check must report
	// unverified rather than reading one of them as a check. The
	// loop.stopped line above carries an `exit` of its own, which is
	// exactly what a reader that skipped the name check would seize on.
	none := judgeMechanical(checkedJob(), []store.Event{evidenceEvent(others...)})
	if none.Outcome != GateUnverified {
		t.Errorf("outcome = %s (%s), want unverified: a line that is not the check was read as one",
			none.Outcome, none.Detail)
	}
}

// TestGateOneDistinguishesFailureFromTimeout: a failing check fails gate
// 1, while a timeout or an infrastructure failure leaves it unverified.
//
// @decision(DL-050)
func TestGateOneDistinguishesFailureFromTimeout(t *testing.T) {
	cases := map[string]struct {
		fields map[string]any
		want   GateOutcome
	}{
		"a passing check": {
			map[string]any{"exit": 0}, GatePassed,
		},
		"a failing check": {
			map[string]any{"exit": 1, "output": "FAIL internal/server"}, GateFailed,
		},
		"a check that timed out": {
			// Not failed: hitting the bound says nothing about the code.
			map[string]any{"timeout": true}, GateUnverified,
		},
		"a check that could not run": {
			map[string]any{"infra": true, "output": "cannot spawn"}, GateUnverified,
		},
		"a check with no exit status": {
			map[string]any{"command": "./scripts/check.sh"}, GateUnverified,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := judgeMechanical(checkedJob(), []store.Event{evidenceEvent(checkLine(tc.fields))})
			if got.Outcome != tc.want {
				t.Errorf("outcome = %s (%s), want %s", got.Outcome, got.Detail, tc.want)
			}
		})
	}
}

// A job that ran its check twice is judged on the LAST one: a run that
// broke something and then fixed it finished green, and reading the
// first would report the state it started from.
//
// @decision(DL-050)
func TestGateOneReadsTheLastCheck(t *testing.T) {
	ev := evidenceEvent(
		checkLine(map[string]any{"exit": 1, "output": "first attempt"}),
		checkLine(map[string]any{"exit": 0, "output": "second attempt"}),
	)
	got := judgeMechanical(checkedJob(), []store.Event{ev})
	if got.Outcome != GatePassed {
		t.Errorf("outcome = %s (%s), want passed: the run's last check is the one that counts",
			got.Outcome, got.Detail)
	}
}

// Evidence is spov's own marker, and an event the SANDBOX named the
// same thing must never be read as spov's record of the check —
// otherwise an agent writes its own passing grade.
//
// @decision(DL-050) @decision(DL-018)
func TestGateOneIgnoresACheckTheSandboxNamed(t *testing.T) {
	forged := store.Event{
		// The sandbox's own voice, wearing spov's event name.
		Source: store.SourceOpenCode,
		Name:   MarkerEvidence,
		Text:   checkLine(map[string]any{"exit": 0, "output": "trust me"}),
	}
	got := judgeMechanical(checkedJob(), []store.Event{forged})
	if got.Outcome == GatePassed {
		t.Fatal("a check the sandbox wrote passed gate 1: the agent can grade its own work")
	}
}

// Gate 3 sees a refusal whatever its error type: the policy's, a
// human's, and an unrecognised one.
//
// @decision(DL-050)
func TestGateThreeSeesBothRefusalPaths(t *testing.T) {
	for name, part := range map[string]string{
		"a refusal the policy made": partRefusedByPolicy,
		"a refusal a human made":    partRefusedByHuman,
		"a refusal with an unrecognised type": `{"type":"tool","name":"shell","executed":false,
		  "state":{"status":"error","error":{"type":"policy.blocked"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			got := judgeConduct(nil, []store.Message{toolMessage(part)}, nil)
			if got.Outcome != GateFailed {
				t.Fatalf("outcome = %s (%s), want failed: a refusal gate 3 cannot see reads as an unproductive run",
					got.Outcome, got.Detail)
			}
		})
	}
}

// A run that finished cleanly and was refused everything it tried is not
// a success, however good its other signals look.
//
// @decision(DL-050)
func TestARefusedRunIsNotASuccessHoweverWellItFinished(t *testing.T) {
	job := store.Job{ID: "j", OrgID: "o", Kind: store.JobKindReport,
		CheckCommand: "./scripts/check.sh"}
	events := []store.Event{
		// Gate 1 passed.
		evidenceEvent(checkLine(map[string]any{"exit": 0})),
		// The turn itself finished successfully — which is exactly the
		// signal spike S3 found lies.
		{Source: store.SourceOpenCode, Name: "session.execution.succeeded"},
	}
	messages := []store.Message{
		toolMessage(partRefusedByPolicy, partText),
	}

	got := judge(job, events, messages, aReport())
	if got.State == store.JobSucceeded {
		t.Fatalf("a run refused every tool call was called succeeded (%q): "+
			"gate 3 is what stops the turn's own outcome deciding this", got.Summary)
	}
	if got.State != store.JobBlocked {
		t.Errorf("state = %v, want blocked: the run was refused, which is a suspension rather than a failure",
			got.State)
	}
	if !strings.Contains(got.Summary, "refused") {
		t.Errorf("summary = %q, want it to say the run was refused", got.Summary)
	}
}

// An unanswered permission is the other way gate 3 fails: the run is
// parked, waiting on a question nobody answered.
//
// @decision(DL-050) @decision(DL-046)
func TestGateThreePairsPermissionsByRequestID(t *testing.T) {
	// The measured shapes (spike S-F1): the asked frame names its
	// request at data.id, the replied one at requestID.
	//
	// @decision(DL-050) @decision(DL-020)
	ask := func(id string) store.Event {
		return store.Event{Source: store.SourceEphemeral, Name: eventPermissionAsked,
			Raw: []byte(`{"type":"permission.asked","data":{"id":"` + id + `","sessionID":"ses_1","action":"read"}}`)}
	}
	reply := func(id string) store.Event {
		return store.Event{Source: store.SourceEphemeral, Name: eventPermissionReplied,
			Raw: []byte(`{"type":"permission.replied","data":{"sessionID":"ses_1","requestID":"` + id + `","reply":"once"}}`)}
	}

	t.Run("an answered permission leaves nothing outstanding", func(t *testing.T) {
		if n := unansweredPermissions([]store.Event{ask("p1"), reply("p1")}); n != 0 {
			t.Errorf("outstanding = %d, want 0", n)
		}
	})
	t.Run("an answer through spov settles its request", func(t *testing.T) {
		answered := store.Event{Source: store.SourceSpov, Name: MarkerAnswered,
			Raw: []byte(`{"requestID":"p1"}`)}
		if n := unansweredPermissions([]store.Event{ask("p1"), answered}); n != 0 {
			t.Errorf("outstanding = %d, want 0: a person answered it through spov", n)
		}
		if n := unansweredPermissions([]store.Event{ask("p1"), answered, ask("p2")}); n != 1 {
			t.Errorf("outstanding = %d, want 1 (p2)", n)
		}
	})
	t.Run("an older requestID-shaped ask still pairs", func(t *testing.T) {
		old := store.Event{Source: store.SourceEphemeral, Name: eventPermissionAsked,
			Raw: []byte(`{"data":{"requestID":"p1"}}`)}
		if n := unansweredPermissions([]store.Event{old, reply("p1")}); n != 0 {
			t.Errorf("outstanding = %d, want 0", n)
		}
	})
	t.Run("a turn ending does not settle a request", func(t *testing.T) {
		ended := store.Event{Source: store.SourceOpenCode, Name: "session.execution.interrupted",
			Raw: []byte(`{"data":{"reason":"shutdown"}}`)}
		if n := unansweredPermissions([]store.Event{ask("p1"), ended}); n != 1 {
			t.Errorf("outstanding = %d, want 1: delivery's interrupt must not hide a park", n)
		}
	})
	t.Run("an unanswered one is outstanding", func(t *testing.T) {
		if n := unansweredPermissions([]store.Event{ask("p1"), reply("p2"), ask("p3")}); n != 2 {
			t.Errorf("outstanding = %d, want 2 (p1 and p3)", n)
		}
	})
	t.Run("a reply nobody can pair clears nothing", func(t *testing.T) {
		// The unpaired reply must not be allowed to clear an arbitrary
		// question: one unreadable frame would then hide a real park.
		unpaired := store.Event{Source: store.SourceEphemeral, Name: eventPermissionReplied,
			Raw: []byte(`{"type":"permission.replied","data":{"sessionID":"ses_1"}}`)}
		if n := unansweredPermissions([]store.Event{ask("p1"), unpaired}); n != 1 {
			t.Errorf("outstanding = %d, want 1: an unpairable reply cleared a real question", n)
		}
	})
}

// A run that made no tool calls at all did not act. Reporting "nothing
// was refused" about it is true and useless, so gate 3 reports
// unverified and the job needs another gate to have passed.
//
// @decision(DL-050)
func TestAJobThatDidNothingIsNotASuccess(t *testing.T) {
	job := store.Job{ID: "j", OrgID: "o", Kind: store.JobKindPR}
	messages := []store.Message{{Role: "assistant", Raw: []byte(`{"id":"m","type":"assistant","content":[` + partText + `]}`)}}

	got := judge(job, nil, messages, nil)
	if got.State == store.JobSucceeded {
		t.Fatalf("a job that made no tool call and delivered nothing succeeded (%q)", got.Summary)
	}
	if got.State != store.JobFailed {
		t.Errorf("state = %v, want failed", got.State)
	}
}

// A report job that produced its report and passed its check
// succeeds. The happy path is asserted too, because a verdict that
// failed everything would pass every test above.
//
// @decision(DL-050)
func TestAJobThatDidTheWorkSucceeds(t *testing.T) {
	job := store.Job{ID: "j", OrgID: "o", Kind: store.JobKindReport,
		CheckCommand: "./scripts/check.sh"}
	events := []store.Event{evidenceEvent(checkLine(map[string]any{"exit": 0}))}
	messages := []store.Message{toolMessage(partRan, partText)}

	got := judge(job, events, messages, aReport())
	if got.State != store.JobSucceeded {
		t.Fatalf("state = %v (%q), want succeeded", got.State, got.Summary)
	}
	if len(got.Gates) != 3 {
		t.Errorf("recorded %d gates, want 3: the row should carry the reasoning, not only the conclusion", len(got.Gates))
	}
}

// A sandbox that shut down mid-run is the clock's fault, not the job's.
// DL-050 calls that a retry rather than a failure.
//
// @decision(DL-050)
func TestASandboxShutdownIsARetryNotAVerdict(t *testing.T) {
	events := []store.Event{{
		Source: store.SourceOpenCode, Name: "session.execution.interrupted",
		Raw: []byte(`{"type":"session.execution.interrupted","data":{"sessionID":"ses_1","reason":"shutdown"}}`),
	}}
	got := judge(store.Job{ID: "j", OrgID: "o", Kind: store.JobKindPR}, events, nil, nil)
	if !got.Retry {
		t.Fatal("a sandbox shutdown was not marked a retry: the job is blamed for its own timeout")
	}

	// And with tool calls recorded, the ordinary shape of a mid-run
	// timeout.
	working := judge(store.Job{ID: "j", OrgID: "o", Kind: store.JobKindPR}, events,
		[]store.Message{toolMessage(partRan)}, nil)
	if !working.Retry {
		t.Error("a job that was working when its sandbox timed out was not marked a retry")
	}

	// An interruption for any other reason is not a retry.
	other := []store.Event{{
		Source: store.SourceOpenCode, Name: "session.execution.interrupted",
		Raw: []byte(`{"type":"session.execution.interrupted","data":{"sessionID":"ses_1","reason":"aborted"}}`),
	}}
	if judge(store.Job{ID: "j", OrgID: "o", Kind: store.JobKindPR}, other, nil, nil).Retry {
		t.Error("an ordinary interruption was marked a retry")
	}
}

// The verdict writes a state the machine allows. DL-045 has no
// delivering -> blocked edge, so a refused run that reached delivery is
// recorded failed rather than left mid-flight forever.
//
// @decision(DL-050) @decision(DL-045)
func TestARefusedRunAtDeliveryIsRecordedRatherThanStranded(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobDelivering)

	sup := &Supervisor{Store: st, Warn: func(string) {}}
	if _, err := sup.recordVerdict(ctx, job, Verdict{
		State: store.JobBlocked, Summary: "the run was refused",
		Gates: []GateResult{{Gate: GateClassification, Outcome: GateFailed, Detail: "refused"}},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.Jobs().Get(ctx, job.OrgID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State == store.JobDelivering {
		t.Fatal("the job was left delivering: a verdict the machine refuses must still end the job")
	}
	if got.State != store.JobFailed {
		t.Errorf("state = %v, want failed", got.State)
	}
	if !strings.Contains(got.Error, "refused") {
		t.Errorf("error = %q, want it to carry the reason", got.Error)
	}
}

// The gates land on the row, so a verdict can be argued with.
//
// @decision(DL-050)
func TestTheRowCarriesWhatEachGateFound(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobDelivering)

	sup := &Supervisor{Store: st, Warn: func(string) {}}
	if _, err := sup.recordVerdict(ctx, job, Verdict{
		State: store.JobSucceeded, Summary: "the check passed",
		Gates: []GateResult{
			{Gate: GateMechanical, Outcome: GatePassed, Detail: "the check passed"},
			{Gate: GateDeliverable, Outcome: GateUnverified, Detail: "not checked"},
			{Gate: GateClassification, Outcome: GatePassed, Detail: "nothing refused"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.Jobs().Get(ctx, job.OrgID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{GateMechanical, GateDeliverable, GateClassification} {
		if !strings.Contains(got.Verdict, want) {
			t.Errorf("the row does not name gate %q: %q", want, got.Verdict)
		}
	}
	// A succeeded job's Error stays empty. A client that renders the
	// field as its name says would otherwise show every success as a
	// failure.
	if got.Error != "" {
		t.Errorf("a succeeded job carries Error %q; the gates belong in Verdict", got.Error)
	}
}

// A job that did NOT succeed carries its reason in Error as well, since
// that is the field every client already shows.
//
// @decision(DL-050)
func TestAFailedJobStillExplainsItselfInError(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobDelivering)

	sup := &Supervisor{Store: st, Warn: func(string) {}}
	if _, err := sup.recordVerdict(ctx, job, Verdict{
		State: store.JobFailed, Summary: "the check failed (exit 1)",
		Gates: []GateResult{{Gate: GateMechanical, Outcome: GateFailed, Detail: "the check failed"}},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.Jobs().Get(ctx, job.OrgID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Error, "check failed") {
		t.Errorf("Error = %q, want the reason the job failed", got.Error)
	}
	if !strings.Contains(got.Verdict, GateMechanical) {
		t.Errorf("Verdict = %q, want the gates", got.Verdict)
	}
}

// A cancel that lands while the gates are being read wins.
//
// @decision(DL-050) @decision(DL-045)
func TestACancelBeatsTheVerdict(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobDelivering)

	// Cancelled under the pass.
	cancelled := job
	cancelled.State = store.JobCancelled
	if err := st.Jobs().Update(ctx, cancelled); err != nil {
		t.Fatal(err)
	}

	sup := &Supervisor{Store: st, Warn: func(string) {}}
	if _, err := sup.recordVerdict(ctx, job, Verdict{State: store.JobSucceeded, Summary: "all good"}); err != nil {
		t.Fatal(err)
	}
	got, err := st.Jobs().Get(ctx, job.OrgID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.JobCancelled {
		t.Errorf("state = %v, want cancelled: a verdict resurrected a cancelled job", got.State)
	}
}

// Delivery terminates the job's sandbox; nothing else does.
//
// @decision(DL-050) @decision(DL-009)
func TestAFinishedJobsSandboxIsStopped(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobDelivering)

	stopped := make(chan string, 1)
	sup := &Supervisor{
		Store: st, Warn: func(string) {},
		Terminate: func(_ context.Context, j store.Job) error {
			stopped <- j.ID
			return nil
		},
	}
	if err := sup.deliver(ctx, job); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-stopped:
		if id != job.ID {
			t.Errorf("stopped job %s, want %s", id, job.ID)
		}
	default:
		t.Fatal("the job finished and its sandbox was left running: nothing else stops it")
	}
}

// A terminate that fails does not undo the verdict. The job is already
// terminal, and retrying the pass would recompute the same conclusion.
//
// @decision(DL-050)
func TestAFailedTerminateStillLeavesTheJobJudged(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobDelivering)

	var warned []string
	sup := &Supervisor{
		Store: st,
		Warn:  func(s string) { warned = append(warned, s) },
		Terminate: func(context.Context, store.Job) error {
			return context.DeadlineExceeded
		},
	}
	if err := sup.deliver(ctx, job); err != nil {
		t.Fatalf("deliver returned %v; a sandbox that would not stop is not the job's verdict", err)
	}
	got, err := st.Jobs().Get(ctx, job.OrgID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !isTerminalJob(got.State) {
		t.Errorf("state = %v, want terminal: a failed terminate stranded the job", got.State)
	}
	if len(warned) == 0 {
		t.Error("a sandbox that would not stop was not warned about")
	}
}

// The whole run is judged, not the part after the supervisor's cursor,
// which sits at the end of the run by delivery.
//
// @decision(DL-050) @decision(DL-046)
func TestTheVerdictReadsTheWholeRunNotFromTheCursor(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobDelivering)

	// Gate 1's evidence is an event, so this exercises the event read;
	// ListMessages ignores the cursor.
	conv, err := st.Conversations().UpsertConversation(ctx, store.Conversation{
		ID: store.NewID(time.Now()), OrgID: job.OrgID, SessionID: job.SessionID,
		OpenCodeID: "ses_cursor",
	})
	if err != nil {
		t.Fatal(err)
	}
	written, err := st.Conversations().PutEvents(ctx, []store.Event{{
		ID: store.NewID(time.Now()), OrgID: job.OrgID, SessionID: job.SessionID,
		ConversationID: conv.ID, Source: store.SourceSpov, Name: MarkerEvidence,
		Text: checkLine(map[string]any{"exit": 0}), At: time.Now(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 1 {
		t.Fatalf("wrote %d events, want 1", len(written))
	}

	// The cursor is past the evidence, as it is by delivery.
	job.Cursor = written[0].Pos
	if err := st.Jobs().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	// Gate 1 is asserted directly below.
	job.CheckCommand = "./scripts/check.sh"
	if err := st.Jobs().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	seedMessages(t, st, job, []store.Message{toolMessage(partRan)})

	sup := &Supervisor{Store: st, Warn: func(string) {}}
	v, err := sup.Judge(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if got := gateNamed(v, GateMechanical); got.Outcome != GatePassed {
		t.Fatalf("gate 1 = %s (%s), want passed: the verdict read from the cursor and missed evidence before it",
			got.Outcome, got.Detail)
	}
}

// gateNamed pulls one gate's result out of a verdict.
func gateNamed(v Verdict, name string) GateResult {
	for _, g := range v.Gates {
		if g.Gate == name {
			return g
		}
	}
	return GateResult{Gate: name, Outcome: "missing"}
}

// TestTheCheckEventMatchesThePlugin pins the check event name, which is
// written in Go and in the plugin. Drift leaves gate 1 silently unverified.
//
// @decision(DL-050) @decision(DL-039)
func TestTheCheckEventMatchesThePlugin(t *testing.T) {
	src, err := opencode.Plugin{Kind: "pr", Goal: "g", CheckCommand: "true"}.Source()
	if err != nil {
		t.Fatal(err)
	}
	// The Go side's constant and the server's own reader must agree...
	if checkEventName != opencode.CheckEvent {
		t.Fatalf("the server reads %q but opencode exports %q", checkEventName, opencode.CheckEvent)
	}
	// ...and the rendered plugin must actually carry it, since that is
	// what writes the line.
	if !strings.Contains(src, `"checkEvent":`+strconvQuote(opencode.CheckEvent)) {
		t.Fatalf("the rendered plugin does not carry checkEvent %q; gate 1 would read a name nothing writes",
			opencode.CheckEvent)
	}
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// The error and boundary paths of the verdict's readers: a malformed row
// must neither panic nor pass.
//
// @decision(DL-050)

// An unreadable or absent job kind cannot pass gate 2.
//
// @decision(DL-050)
func TestAnUnknownJobKindHasNoDeliverable(t *testing.T) {
	got := judgeDeliverable(store.Job{Kind: "sculpture"}, nil, aReport())
	if got.Outcome != GateUnverified {
		t.Errorf("outcome = %s, want unverified for an unknown kind", got.Outcome)
	}
	// A report job that left no report produced nothing.
	blank := judgeDeliverable(store.Job{Kind: store.JobKindReport}, nil, nil)
	if blank.Outcome != GateFailed {
		t.Errorf("outcome = %s, want failed: no report is not a report", blank.Outcome)
	}
}

// Gate 3 reports what it found in a way a human can read, including
// when there is nothing to report.
//
// @decision(DL-050)
func TestGateThreeWordsItsFindings(t *testing.T) {
	t.Run("a refusal names the tool", func(t *testing.T) {
		got := judgeConduct(nil, []store.Message{toolMessage(partRefusedByPolicy)}, nil)
		if !strings.Contains(got.Detail, "shell") {
			t.Errorf("detail = %q, want it to name the refused call", got.Detail)
		}
		if !strings.Contains(got.Detail, "policy") {
			t.Errorf("detail = %q, want it to say which path refused", got.Detail)
		}
	})
	t.Run("a refusal with no tool name still reads", func(t *testing.T) {
		got := judgeConduct(nil, []store.Message{
			toolMessage(`{"type":"tool","executed":false,` +
				`"state":{"status":"error","error":{"type":"permission.rejected"}}}`)}, nil)
		if got.Outcome != GateFailed || got.Detail == "" {
			t.Errorf("outcome = %s, detail = %q", got.Outcome, got.Detail)
		}
	})
	t.Run("an untyped error is not a refusal", func(t *testing.T) {
		// A part with no error type is a call that failed, not one that
		// was forbidden. `executed` is false on every local part
		// (DL-050), so reading this as a refusal would block working jobs
		// on every failed shell command.
		got := judgeConduct(nil, []store.Message{
			toolMessage(`{"type":"tool","name":"shell","executed":false,"state":{"status":"error"}}`)}, nil)
		if got.Outcome == GateFailed {
			t.Error("an untyped tool error was read as a refusal; an ordinary failing command would block the job")
		}
	})
	t.Run("one unanswered permission reads as one", func(t *testing.T) {
		ask := store.Event{Source: store.SourceEphemeral, Name: eventPermissionAsked,
			Raw: []byte(`{"data":{"requestID":"p1"}}`)}
		got := judgeConduct([]store.Event{ask}, []store.Message{toolMessage(partRan)}, nil)
		if !strings.Contains(got.Detail, "1 permission request") {
			t.Errorf("detail = %q, want it to count one request without pluralising", got.Detail)
		}
	})
	t.Run("several read as several", func(t *testing.T) {
		asks := []store.Event{
			{Source: store.SourceEphemeral, Name: eventPermissionAsked, Raw: []byte(`{"data":{"requestID":"p1"}}`)},
			{Source: store.SourceEphemeral, Name: eventPermissionAsked, Raw: []byte(`{"data":{"requestID":"p2"}}`)},
		}
		got := judgeConduct(asks, []store.Message{toolMessage(partRan)}, nil)
		if !strings.Contains(got.Detail, "2 permission requests") {
			t.Errorf("detail = %q, want a plural count", got.Detail)
		}
	})
}

// Permission frames the supervisor cannot parse count as ASKED, never
// as answered: the safe direction is to report a job as parked rather
// than to let an unreadable frame make a park invisible.
//
// @decision(DL-050) @decision(DL-046)
func TestAnUnreadablePermissionFrameCountsAsParked(t *testing.T) {
	for name, raw := range map[string][]byte{
		"no raw at all":  nil,
		"not json":       []byte("<html>"),
		"no data object": []byte(`{"type":"permission.asked"}`),
		"no request id":  []byte(`{"data":{"sessionID":"ses_1"}}`),
	} {
		t.Run(name, func(t *testing.T) {
			ask := store.Event{Source: store.SourceEphemeral, Name: eventPermissionAsked, Raw: raw}
			if n := unansweredPermissions([]store.Event{ask}); n != 1 {
				t.Errorf("outstanding = %d, want 1: an unreadable ask made a park invisible", n)
			}
		})
	}

	// A spov marker is not a permission frame, whatever it is named.
	spoof := store.Event{Source: store.SourceSpov, Name: eventPermissionAsked,
		Raw: []byte(`{"data":{"requestID":"p1"}}`)}
	if n := unansweredPermissions([]store.Event{spoof}); n != 0 {
		t.Errorf("outstanding = %d, want 0: a spov marker was read as the session's own frame", n)
	}
}

// The alternative request-id spellings are accepted, since the frame's
// shape is measured on one build rather than specified.
//
// @decision(DL-050)
func TestTheRequestIDIsReadUnderItsSpellings(t *testing.T) {
	for _, key := range []string{"requestID", "requestId", "request_id"} {
		if got := permissionRequestID([]byte(`{"data":{"` + key + `":"p1"}}`)); got != "p1" {
			t.Errorf("%s: got %q, want p1", key, got)
		}
	}
	// The frame's own top-level id is the EVENT's, not the request's,
	// and must not be mistaken for it.
	if got := permissionRequestID([]byte(`{"id":"evt_1","data":{"sessionID":"s"}}`)); got != "" {
		t.Errorf("got %q from a frame with no request id; the event's own id was read instead", got)
	}
}

// Only a shutdown is a retry, and only from the sandbox's own voice.
//
// @decision(DL-050)
func TestOnlyAShutdownFromTheSandboxIsARetry(t *testing.T) {
	interrupted := func(source store.EventSource, raw string) []store.Event {
		return []store.Event{{Source: source, Name: "session.execution.interrupted", Raw: []byte(raw)}}
	}
	cases := map[string]struct {
		events []store.Event
		want   bool
	}{
		"a shutdown": {
			interrupted(store.SourceOpenCode, `{"data":{"reason":"shutdown"}}`), true,
		},
		"another reason": {
			interrupted(store.SourceOpenCode, `{"data":{"reason":"aborted"}}`), false,
		},
		"no reason": {
			interrupted(store.SourceOpenCode, `{"data":{}}`), false,
		},
		"an unreadable frame": {
			interrupted(store.SourceOpenCode, `<html>`), false,
		},
		"a spov marker claiming a shutdown": {
			interrupted(store.SourceSpov, `{"data":{"reason":"shutdown"}}`), false,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := wasShutDown(tc.events, nil); got != tc.want {
				t.Errorf("wasShutDown = %v, want %v", got, tc.want)
			}
		})
	}
}

// A check's output is trimmed to something a job row can carry, without
// losing the fact that there was output.
//
// @decision(DL-050)
func TestTheCheckOutputIsTrimmedForTheRow(t *testing.T) {
	if got := firstLine(""); got != "no output" {
		t.Errorf("empty output = %q, want a phrase rather than an empty row", got)
	}
	if got := firstLine("first\nsecond\nthird"); got != "first" {
		t.Errorf("got %q, want only the first line", got)
	}
	long := strings.Repeat("x", 400)
	got := firstLine(long)
	if len(got) > 220 {
		t.Errorf("a %d-char line reached the row; it should be capped", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Error("a truncated line does not say it was truncated")
	}
}

// Evidence that cannot be parsed leaves gate 1 unverified rather than
// crashing or passing. A truncated file ends mid-line, which is the
// ordinary case for a large one.
//
// @decision(DL-050)
func TestUnparseableEvidenceLeavesGateOneUnverified(t *testing.T) {
	for name, text := range map[string]string{
		"empty":            "",
		"not json":         "the plugin crashed",
		"a truncated line": `{"event":"check.finished","exi`,
		"blank lines":      "\n\n   \n",
	} {
		t.Run(name, func(t *testing.T) {
			got := judgeMechanical(checkedJob(), []store.Event{evidenceEvent(text)})
			if got.Outcome != GateUnverified {
				t.Errorf("outcome = %s (%s), want unverified", got.Outcome, got.Detail)
			}
		})
	}
}

// A session with nothing recorded produces a verdict rather than an
// error, and that verdict is not a success. ListMessages returns an empty
// list for a missing session too, so the conclusion is the only safeguard.
//
// @decision(DL-050)
func TestAJobWithNothingRecordedIsNotASuccess(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobDelivering)

	sup := &Supervisor{Store: st, Warn: func(string) {}}
	v, err := sup.Judge(ctx, job)
	if err != nil {
		t.Fatalf("Judge returned %v for an empty session; an empty run is a verdict, not a failure to compute one", err)
	}
	if v.State == store.JobSucceeded {
		t.Errorf("a run that left no trace at all was called succeeded (%q)", v.Summary)
	}
	for _, g := range v.Gates {
		if g.Outcome == GatePassed {
			t.Errorf("gate %s passed on an empty session: %s", g.Gate, g.Detail)
		}
	}
}

// A job started with no check command cannot pass gate 1 on a
// check.finished line the agent appended to the evidence file itself.
// Gate 1 stays forgeable for a job that did ask for a check; see DL-050.
//
// @decision(DL-050)
func TestAForgedCheckCannotCarryAJobThatAskedForNone(t *testing.T) {
	// Exactly what `echo '{"event":"check.finished","exit":0}' >>
	// /tmp/spov-goal.jsonl` would leave, collected and marked by the
	// supervisor in the ordinary way.
	forged := evidenceEvent(checkLine(map[string]any{"exit": 0, "output": "all good"}))

	t.Run("gate 1 does not read it", func(t *testing.T) {
		got := judgeMechanical(store.Job{ID: "j", OrgID: "o", Kind: store.JobKindPR}, []store.Event{forged})
		if got.Outcome == GatePassed {
			t.Fatal("a check the job never asked for passed gate 1: the agent graded its own work")
		}
	})

	t.Run("and it cannot carry the verdict", func(t *testing.T) {
		// A pr job: gate 2 is unverified, so gate 1 is the only thing
		// that could produce a success.
		job := store.Job{ID: "j", OrgID: "o", Kind: store.JobKindPR}
		v := judge(job, []store.Event{forged}, []store.Message{toolMessage(partRan)}, nil)
		if v.State == store.JobSucceeded {
			t.Fatalf("a forged check carried the job to succeeded (%q)", v.Summary)
		}
	})

	t.Run("a job that did ask for one still reads it", func(t *testing.T) {
		// The guard must not break the ordinary case.
		got := judgeMechanical(checkedJob(), []store.Event{forged})
		if got.Outcome != GatePassed {
			t.Errorf("outcome = %s (%s), want passed", got.Outcome, got.Detail)
		}
	})
}

// "The job produced no report" fails gate 2; "spov could not read the
// report" leaves it unverified, since delivery is idempotent and would
// never retry the read.
//
// @decision(DL-050)
func TestABrokenReadIsNotAMissingReport(t *testing.T) {
	job := store.Job{ID: "j", OrgID: "o", Kind: store.JobKindReport}

	t.Run("no report fails the job", func(t *testing.T) {
		got := judgeDeliverable(job, nil, nil)
		if got.Outcome != GateFailed {
			t.Errorf("outcome = %s (%s), want failed", got.Outcome, got.Detail)
		}
	})

	t.Run("a read that broke does not", func(t *testing.T) {
		broke := []store.Event{{
			Source: store.SourceSpov, Name: MarkerCollectFailed,
			Text: "connection reset",
		}}
		got := judgeDeliverable(job, broke, nil)
		if got.Outcome == GateFailed {
			t.Fatal("a failed read was recorded as the job producing nothing: " +
				"delivery is idempotent, so this failure is permanent")
		}
		if got.Outcome != GateUnverified {
			t.Errorf("outcome = %s, want unverified", got.Outcome)
		}
		if !strings.Contains(got.Detail, "connection reset") {
			t.Errorf("detail = %q, want it to carry why the read failed", got.Detail)
		}
	})

	t.Run("only spov may say the read broke", func(t *testing.T) {
		// Otherwise a sandbox turns its own empty output into
		// "unverified" and dodges gate 2 entirely.
		forged := []store.Event{{
			Source: store.SourceOpenCode, Name: MarkerCollectFailed,
			Text: "trust me, the read broke",
		}}
		if got := judgeDeliverable(job, forged, nil); got.Outcome != GateFailed {
			t.Errorf("outcome = %s: a sandbox talked its way out of gate 2", got.Outcome)
		}
	})
}

// A judged job is stamped as stopped. The store does the stamping, so
// this asserts the result rather than the mechanism.
//
// @decision(DL-050) @decision(DL-045)
func TestAJudgedJobIsStampedStopped(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobDelivering)

	sup := &Supervisor{Store: st, Warn: func(string) {}}
	if _, err := sup.recordVerdict(ctx, job, Verdict{State: store.JobSucceeded, Summary: "done"}); err != nil {
		t.Fatal(err)
	}
	got, err := st.Jobs().Get(ctx, job.OrgID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.StoppedAt == nil {
		t.Error("a finished job has no StoppedAt; nothing can say how long it ran")
	}
}

// TestACheckThatCouldNotRunVetoesSuccess: a check command the caller set
// that exits 126/127 keeps a job from succeeding, even with gate 2 passed.
//
// @decision(DL-050)
func TestACheckThatCouldNotRunVetoesSuccess(t *testing.T) {
	// A report job holding its report: gate 2 passes, so without the
	// veto this job succeeds.
	job := checkedJob()
	unrunnable := []store.Event{evidenceEvent(checkLine(map[string]any{
		"exit": 127, "output": "sh: ./scripts/check.sh: not found",
	}))}

	// The premise. If gate 2 stops passing this test proves nothing.
	if got := judgeDeliverable(job, unrunnable, aReport()); got.Outcome != GatePassed {
		t.Fatalf("gate 2 = %s, want passed: this test needs a job that would otherwise succeed",
			got.Outcome)
	}

	v := judge(job, unrunnable, nil, aReport())
	if v.State == store.JobSucceeded {
		t.Fatal("a job whose check could not be run was recorded succeeded;" +
			" the gate the caller asked for never ran and nothing says so in the state")
	}
	if v.State != store.JobFailed {
		t.Errorf("state = %s, want failed", v.State)
	}
	if !strings.Contains(v.Summary, "could not be run") {
		t.Errorf("summary = %q, want the reason the check did not run", v.Summary)
	}
	// The wording must still not say the check failed.
	if strings.Contains(v.Summary, "the check failed") {
		t.Errorf("summary = %q: the veto must not reword an unrunnable command"+
			" as a failing suite", v.Summary)
	}
}

// A check spov proposed that could not run does not veto success.
//
// @decision(DL-050) @decision(DL-047)
func TestAProposedCheckThatCouldNotRunDoesNotVeto(t *testing.T) {
	job := checkedJob()
	job.CheckProposed = true
	unrunnable := []store.Event{evidenceEvent(checkLine(map[string]any{
		"exit": 127, "output": "sh: ./scripts/check.sh: not found",
	}))}

	v := judge(job, unrunnable, nil, aReport())
	if v.State != store.JobSucceeded {
		t.Errorf("state = %s, want succeeded: spov's own guessed check command"+
			" failed a job that delivered its report", v.State)
	}
}

// A job given no check at all is not vetoed.
//
// @decision(DL-050)
func TestAJobWithNoCheckIsNotVetoed(t *testing.T) {
	job := store.Job{ID: "j", OrgID: "o", Kind: store.JobKindReport}
	v := judge(job, nil, nil, aReport())
	if v.State != store.JobSucceeded {
		t.Errorf("state = %s, want succeeded: a job that asked for no check"+
			" must not be failed for not having one", v.State)
	}
}

// A check command the shell could not run (126 not executable, 127 not
// found) is not a failing check, and its detail says "could not be run".
//
// @decision(DL-050)
func TestACheckTheShellCouldNotRunIsNotAFailingCheck(t *testing.T) {
	for name, exit := range map[string]int{
		"not found":      127,
		"not executable": 126,
	} {
		t.Run(name, func(t *testing.T) {
			got := judgeMechanical(checkedJob(), []store.Event{
				evidenceEvent(checkLine(map[string]any{
					"exit": exit, "output": "sh: ./scripts/check.sh: not found",
				}))})
			if got.Outcome == GateFailed {
				t.Fatalf("exit %d read as a failing check: a wrong path would fail every run "+
					"with the code blamed for it", exit)
			}
			if !strings.Contains(got.Detail, "could not be run") {
				t.Errorf("detail = %q, want it to say the command could not be run", got.Detail)
			}
		})
	}

	// An ordinary non-zero status still fails.
	got := judgeMechanical(checkedJob(), []store.Event{
		evidenceEvent(checkLine(map[string]any{"exit": 1, "output": "FAIL"}))})
	if got.Outcome != GateFailed {
		t.Errorf("outcome = %s, want failed for an ordinary non-zero exit", got.Outcome)
	}
}

// The recorder catches up before the gates are read, so a refusal in the
// run's last turn is seen.
//
// @decision(DL-050) @decision(DL-017)
func TestTheVerdictWaitsForTheFinalRead(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobDelivering)
	// A collected report, so the job would succeed if the refusal in the
	// last turn were missed.
	seedReport(t, st, job)

	var caughtUp bool
	sup := &Supervisor{
		Store: st, Warn: func(string) {},
		CatchUp: func(_ context.Context, j store.Job) error {
			caughtUp = true
			// What the recorder's final read would store: the last
			// turn, carrying a refusal nothing had recorded yet.
			seedMessages(t, st, j, []store.Message{toolMessage(partRefusedByPolicy)})
			return nil
		},
	}
	if err := sup.deliver(ctx, job); err != nil {
		t.Fatal(err)
	}
	if !caughtUp {
		t.Fatal("the verdict was computed without a final read")
	}
	got, err := st.Jobs().Get(ctx, job.OrgID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State == store.JobSucceeded {
		t.Fatalf("a refusal in the run's last turn was missed (%q): "+
			"the verdict judged rows written before the final read", got.Verdict)
	}
}

// A catch-up that fails does not stop the verdict. A job whose sandbox
// is already gone is the ordinary case for the abandoned backstop, and
// judging on what was stored beats not judging at all.
//
// @decision(DL-050)
func TestAFailedFinalReadStillProducesAVerdict(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobDelivering)
	seedMessages(t, st, job, []store.Message{toolMessage(partRan)})

	var warned []string
	sup := &Supervisor{
		Store: st,
		Warn:  func(s string) { warned = append(warned, s) },
		CatchUp: func(context.Context, store.Job) error {
			return context.DeadlineExceeded
		},
	}
	if err := sup.deliver(ctx, job); err != nil {
		t.Fatalf("deliver returned %v; a sandbox that could not be read is not a reason to leave the job unjudged", err)
	}
	got, err := st.Jobs().Get(ctx, job.OrgID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !isTerminalJob(got.State) {
		t.Errorf("state = %v, want terminal", got.State)
	}
	if len(warned) == 0 {
		t.Error("a failed final read was not warned about; the verdict may have missed the last turn")
	}
}

// A run longer than the verdict's event bound drops its head, not its
// tail, because gate 1's evidence marker is the last event a job has.
//
// @decision(DL-050)
func TestALongRunKeepsTheEndWhereTheEvidenceIs(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobDelivering)
	job.CheckCommand = "./scripts/check.sh"
	if err := st.Jobs().Update(ctx, job); err != nil {
		t.Fatal(err)
	}

	conv, err := st.Conversations().UpsertConversation(ctx, store.Conversation{
		ID: store.NewID(time.Now()), OrgID: job.OrgID, SessionID: job.SessionID,
		OpenCodeID: "ses_long",
	})
	if err != nil {
		t.Fatal(err)
	}
	// A run longer than the bound, with the bound lowered to keep it small.
	const bound = 8
	evs := make([]store.Event, 0, bound*2)
	for i := range bound * 2 {
		evs = append(evs, store.Event{
			ID: store.NewID(time.Now()), OrgID: job.OrgID, SessionID: job.SessionID,
			ConversationID: conv.ID, Source: store.SourceOpenCode,
			Seq: int64(i + 1), Name: "session.execution.succeeded", At: time.Now(),
		})
	}
	if _, err := st.Conversations().PutEvents(ctx, evs); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Conversations().PutEvents(ctx, []store.Event{{
		ID: store.NewID(time.Now()), OrgID: job.OrgID, SessionID: job.SessionID,
		Source: store.SourceSpov, Name: MarkerEvidence, At: time.Now(),
		Text: checkLine(map[string]any{"exit": 0}),
	}}); err != nil {
		t.Fatal(err)
	}

	sup := &Supervisor{Store: st, Warn: func(string) {}, MaxVerdictEvents: bound}
	v, err := sup.Judge(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if got := gateNamed(v, GateMechanical); got.Outcome != GatePassed {
		t.Fatalf("gate 1 = %s (%s), want passed: a long run truncated away the evidence it is judged on",
			got.Outcome, got.Detail)
	}
}

// The verdict holds a bounded size of a run's events as well as a
// bounded count, and still keeps the end, where the evidence is.
//
// @decision(DL-131)
func TestTheVerdictHoldsABoundedSizeOfARun(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobDelivering)
	job.CheckCommand = "./scripts/check.sh"
	if err := st.Jobs().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	conv, err := st.Conversations().UpsertConversation(ctx, store.Conversation{
		ID: store.NewID(time.Now()), OrgID: job.OrgID, SessionID: job.SessionID, OpenCodeID: "ses_fat",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Far fewer events than the count bound, each a kilobyte.
	pad := []byte(`{"pad":"` + strings.Repeat("x", 1000) + `"}`)
	for i := range 40 {
		if _, err := st.Conversations().PutEvents(ctx, []store.Event{{
			ID: store.NewID(time.Now()), OrgID: job.OrgID, SessionID: job.SessionID,
			ConversationID: conv.ID, Source: store.SourceOpenCode,
			Seq: int64(i + 1), Name: "session.next.tool", At: time.Now(), Raw: pad,
		}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.Conversations().PutEvents(ctx, []store.Event{{
		ID: store.NewID(time.Now()), OrgID: job.OrgID, SessionID: job.SessionID,
		Source: store.SourceSpov, Name: MarkerEvidence, At: time.Now(),
		Text: checkLine(map[string]any{"exit": 0}),
	}}); err != nil {
		t.Fatal(err)
	}

	const bound = 8 << 10
	sup := &Supervisor{Store: st, Warn: func(string) {}, MaxVerdictBytes: bound}
	evs, err := sup.jobEvents(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	var held int64
	for _, e := range evs {
		held += eventBytes(e)
	}
	if held > bound || len(evs) == 0 || len(evs) >= 41 {
		t.Errorf("the verdict holds %d events of %d bytes against a bound of %d", len(evs), held, bound)
	}
	if last := evs[len(evs)-1]; last.Name != MarkerEvidence {
		t.Errorf("the last event held is %s, want the evidence marker", last.Name)
	}
	v, err := sup.Judge(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if got := gateNamed(v, GateMechanical); got.Outcome != GatePassed {
		t.Errorf("gate 1 = %s (%s), want passed", got.Outcome, got.Detail)
	}

	// One event larger than the whole bound is still held: the newest
	// is never dropped.
	sup.MaxVerdictBytes = 16
	if evs, err := sup.jobEvents(ctx, job); err != nil || len(evs) != 1 || evs[0].Name != MarkerEvidence {
		t.Errorf("under a bound smaller than one event: %d events, %v", len(evs), err)
	}
}

// A succeeded job carries no Error, including the GapNote a follow pass
// stamped while it ran.
//
// @decision(DL-050)
func TestASucceededJobCarriesNoStaleError(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobDelivering)

	// The job arrives at the verdict already carrying a note from the
	// follow pass that moved it.
	job.Error = GapNote
	if err := st.Jobs().Update(ctx, job); err != nil {
		t.Fatal(err)
	}

	sup := &Supervisor{Store: st, Warn: func(string) {}}
	if _, err := sup.recordVerdict(ctx, job, Verdict{
		State: store.JobSucceeded, Summary: "the deliverable is there",
		Gates: []GateResult{{Gate: GateDeliverable, Outcome: GatePassed, Detail: "collected"}},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.Jobs().Get(ctx, job.OrgID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.JobSucceeded {
		t.Fatalf("state = %v, want succeeded", got.State)
	}
	if got.Error != "" {
		t.Errorf("a succeeded job kept Error = %q: a client rendering that field as a "+
			"failure shows this success as one", got.Error)
	}
	// The verdict itself must still be there — clearing Error must not
	// clear the reasoning.
	if got.Verdict == "" {
		t.Error("clearing the error also cleared the verdict")
	}
}

// ParseGatesNote is the inverse of gatesNote. These tests hold the pair.
//
// @decision(DL-057) @decision(DL-050)

// TestTheGatesSurviveTheRoundTrip renders a verdict through gatesNote and
// reads it back, pinning the two to each other rather than to a literal.
func TestTheGatesSurviveTheRoundTrip(t *testing.T) {
	v := Verdict{
		State:   store.JobFailed,
		Summary: "conduct: the run was refused",
		Gates: []GateResult{
			{Gate: GateMechanical, Outcome: GatePassed, Detail: "./scripts/check.sh exited 0"},
			{Gate: GateDeliverable, Outcome: GateUnverified, Detail: "a pr job's deliverable cannot be verified yet"},
			{Gate: GateClassification, Outcome: GateFailed, Detail: "an edit was refused (permission.rejected)"},
		},
	}
	// The stored column is the summary and then the gates, which is
	// what jobViewOf is handed.
	stored := v.Summary + "\n" + gatesNote(v)

	got := ParseGatesNote(stored)
	if len(got) != len(v.Gates) {
		t.Fatalf("round trip lost gates: wrote %d, read %d back from:\n%s", len(v.Gates), len(got), stored)
	}
	for i, want := range v.Gates {
		if got[i] != want {
			t.Errorf("gate %d round-tripped as %+v, want %+v", i, got[i], want)
		}
	}
	// The third gate's detail carries a parenthesis of its own. If the
	// parser cut at the first `(` rather than the last `)`, this is
	// where it shows.
	if got[2].Detail != "an edit was refused (permission.rejected)" {
		t.Errorf("a detail containing parentheses was mangled: %q", got[2].Detail)
	}
}

// TestTheSummaryIsNotReadAsAGate holds the first line out. The summary
// has the same `name: detail` shape as a gate — "conduct: the run was
// refused" — so a parser that considered every line would invent a
// fourth gate out of the conclusion.
func TestTheSummaryIsNotReadAsAGate(t *testing.T) {
	// The summary is a gate's Detail verbatim (see judge), so it can
	// carry both halves of the shape a gate line has: something before
	// a colon, an enum word after it, and a parenthesis at the end.
	// Only the missing "- " tells the two apart.
	stored := "deliverable: unverified (a pr job's deliverable cannot be verified yet)\n" +
		"- check: passed (exited 0)"
	got := ParseGatesNote(stored)
	if len(got) != 1 {
		t.Fatalf("read %d gates from a verdict with one, %+v", len(got), got)
	}
	if got[0].Gate != GateMechanical {
		t.Errorf("read the summary as a gate: %+v", got[0])
	}
}

// TestTheRetryNoteIsNotAGate holds the other non-gate line. gatesNote
// appends the sandbox-shutdown note as a `- ` line with no gate shape,
// so a parser that assumed every bullet was a gate would name one after
// the first words of a sentence and show it to a reader as a finding.
func TestTheRetryNoteIsNotAGate(t *testing.T) {
	v := Verdict{
		Summary: "the sandbox shut down",
		Gates:   []GateResult{{Gate: GateMechanical, Outcome: GateUnverified, Detail: "no evidence"}},
		Retry:   true,
	}
	stored := v.Summary + "\n" + gatesNote(v)
	if !strings.Contains(stored, "- the sandbox shut down mid-run") {
		t.Fatalf("the fixture no longer contains the retry note, so this test proves nothing:\n%s", stored)
	}
	got := ParseGatesNote(stored)
	if len(got) != 1 {
		t.Fatalf("read %d gates past the retry note, %+v", len(got), got)
	}
	if got[0].Gate != GateMechanical {
		t.Errorf("read the retry note as a gate: %+v", got[0])
	}
}

// TestAnUnknownOutcomeIsDroppedRatherThanPassedOn keeps the API's enum
// honest. A client switches on the outcome to colour the line; a value
// outside the three would fall through to nothing, which is
// indistinguishable from a gate that did not report.
func TestAnUnknownOutcomeIsDroppedRatherThanPassedOn(t *testing.T) {
	got := ParseGatesNote("summary\n- check: sideways (who knows)\n- conduct: passed (nothing was refused)")
	if len(got) != 1 {
		t.Fatalf("read %d gates, want only the one with a known outcome: %+v", len(got), got)
	}
	if got[0].Outcome != GatePassed {
		t.Errorf("kept an outcome outside the enum: %+v", got[0])
	}
}

// TestAJobWithNoVerdictHasNoGates covers the ordinary running job: the
// column is empty until delivery, and an empty column must produce no
// gates rather than one empty one.
func TestAJobWithNoVerdictHasNoGates(t *testing.T) {
	if got := ParseGatesNote(""); len(got) != 0 {
		t.Errorf("an empty verdict produced %d gates: %+v", len(got), got)
	}
}

// TestAGateDetailFromTheSandboxCannotBreakTheLine: gate 3's detail embeds
// a sandbox-chosen tool name, and a newline in it must not split the gate
// line and drop the gate from ParseGatesNote's result.
//
// @decision(DL-057) @decision(DL-050)
func TestAGateDetailFromTheSandboxCannotBreakTheLine(t *testing.T) {
	v := Verdict{
		Summary: "conduct: the run was refused",
		Gates: []GateResult{
			{Gate: GateMechanical, Outcome: GatePassed, Detail: "exited 0"},
			{
				Gate:    GateClassification,
				Outcome: GateFailed,
				// A tool name a sandbox chose, with a newline in it.
				Detail: "a call to\nevil\ttool was refused (permission.rejected)",
			},
		},
	}
	stored := v.Summary + "\n" + gatesNote(v)

	got := ParseGatesNote(stored)
	if len(got) != 2 {
		t.Fatalf("a newline in a detail cost a gate: read %d of 2 from\n%+v", len(got), got)
	}
	if got[1].Gate != GateClassification || got[1].Outcome != GateFailed {
		t.Fatalf("the deciding gate did not survive: %+v", got[1])
	}
	if strings.Contains(got[1].Detail, "\n") || strings.Contains(got[1].Detail, "\t") {
		t.Errorf("the detail kept the whitespace that breaks the format: %q", got[1].Detail)
	}
	// The text is still readable, not merely parseable.
	if !strings.Contains(got[1].Detail, "evil") {
		t.Errorf("collapsing the whitespace lost the detail's words: %q", got[1].Detail)
	}
}

// Gate 2 over a job's inventory.
//
// @decision(DL-050)

func prJob() store.Job {
	return store.Job{ID: "j1", OrgID: "o", Kind: store.JobKindPR}
}

func pr(branch string, status store.DeliverableStatus) store.Deliverable {
	d := store.Deliverable{Kind: store.DeliverablePR, Status: status,
		Platform: "codefloe", Owner: "acme", Name: "widgets", Branch: branch}
	d.Key = store.PullRequestKey(d.Platform, d.Owner, d.Name, d.Branch)
	if status == store.DeliverableVerified {
		d.Number, d.URL = 74, "https://codefloe.com/acme/widgets/pulls/74"
	}
	return d
}

// TestTheDeliverableGateOverTheSet checks gate 2's four outcomes, and
// that each qualifying fact turns a pass into a partial one.
func TestTheDeliverableGateOverTheSet(t *testing.T) {
	ok := pr("a", store.DeliverableVerified)
	bounded := []store.Event{{Source: store.SourceSpov, Name: MarkerBounded, Text: "the job reached its wall clock"}}
	broke := []store.Event{{Source: store.SourceSpov, Name: MarkerCollectFailed, Text: "connection reset"}}
	report := store.Job{ID: "j", OrgID: "o", Kind: store.JobKindReport}
	unfinished := prJob()
	unfinished.Unfinished = "the docs"
	for name, c := range map[string]struct {
		job     store.Job
		events  []store.Event
		members []store.Deliverable
		want    GateOutcome
		says    string
	}{
		"one verified":                 {prJob(), nil, []store.Deliverable{ok}, GatePassed, "1 of 1 pull request verified"},
		"two verified":                 {prJob(), nil, []store.Deliverable{ok, pr("b", store.DeliverableVerified)}, GatePassed, "2 of 2"},
		"one missing":                  {prJob(), nil, []store.Deliverable{ok, pr("b", store.DeliverableMissing)}, GatePartial, "1 not found on the forge"},
		"one unchecked":                {prJob(), nil, []store.Deliverable{ok, pr("b", store.DeliverableUnverified)}, GatePartial, "1 could not be checked"},
		"unfinished declared":          {unfinished, nil, []store.Deliverable{ok}, GatePartial, "the agent left unfinished: the docs"},
		"ended at the wall clock":      {prJob(), bounded, []store.Deliverable{ok}, GatePartial, "wall clock"},
		"report listing broke":         {report, broke, aReport(), GatePartial, "connection reset"},
		"nothing recorded":             {prJob(), nil, nil, GateFailed, "recorded no pull request"},
		"no report":                    {report, nil, nil, GateFailed, "left no report"},
		"every one missing":            {prJob(), nil, []store.Deliverable{pr("b", store.DeliverableMissing)}, GateFailed, "1 not found"},
		"none verified, one unchecked": {prJob(), nil, []store.Deliverable{pr("b", store.DeliverableMissing), pr("c", store.DeliverableUnverified)}, GateUnverified, "could not be checked"},
		"the listing broke":            {report, broke, nil, GateUnverified, "connection reset"},
		"only over the limit":          {report, nil, []store.Deliverable{{Kind: store.DeliverableReport, Status: store.DeliverableOverLimit}}, GateFailed, "over the size limit"},
		// @decision(DL-051)
		"a report lost images":   {report, nil, []store.Deliverable{{Kind: store.DeliverableReport, Status: store.DeliverableVerified, ImagesLost: true}}, GatePartial, "1 report lost some linked images"},
		"images lost unverified": {report, nil, []store.Deliverable{{Kind: store.DeliverableReport, Status: store.DeliverableUnverified, ImagesLost: true}}, GateUnverified, "could not be checked"},
	} {
		got := judgeDeliverable(c.job, c.events, c.members)
		if got.Outcome != c.want || !strings.Contains(got.Detail, c.says) {
			t.Errorf("%s: %s (%q), want %s saying %q", name, got.Outcome, got.Detail, c.want, c.says)
		}
	}
}

// TestAPRJobSucceedsOnItsPullRequest: a pr job with no check succeeds on
// an open pull request alone.
func TestAPRJobSucceedsOnItsPullRequest(t *testing.T) {
	v := judge(prJob(), nil, []store.Message{toolMessage(partRan)}, []store.Deliverable{pr("a", store.DeliverableVerified)})
	if v.State != store.JobSucceeded {
		t.Fatalf("state = %v (%q), want succeeded on the pull request alone", v.State, v.Summary)
	}
	if deliv := gateNamed(v, GateDeliverable); deliv.Outcome != GatePassed {
		t.Errorf("gate 2 = %s (%q), want passed", deliv.Outcome, deliv.Detail)
	}
}

// TestAPartialDeliverySucceedsAndSaysSo: a verified pull request beside
// a missing one is a success, recorded as partial.
func TestAPartialDeliverySucceedsAndSaysSo(t *testing.T) {
	v := judge(prJob(), nil, []store.Message{toolMessage(partRan)},
		[]store.Deliverable{pr("a", store.DeliverableVerified), pr("b", store.DeliverableMissing)})
	if v.State != store.JobSucceeded {
		t.Fatalf("state = %v (%q), want succeeded", v.State, v.Summary)
	}
	if !strings.Contains(v.Summary, "part of the deliverables") {
		t.Errorf("summary = %q, want it to say the delivery was partial", v.Summary)
	}
	if g := ParseGatesNote("x\n" + gatesNote(v)); gateNamed(Verdict{Gates: g}, GateDeliverable).Outcome != GatePartial {
		t.Errorf("partial did not survive the verdict text: %+v", g)
	}
}

// TestAPassingCheckWithNothingVerifiedIsNotASuccess checks that a
// check that passed does not carry a job that delivered nothing spov
// could verify.
func TestAPassingCheckWithNothingVerifiedIsNotASuccess(t *testing.T) {
	job := prJob()
	job.CheckCommand = "./scripts/check.sh"
	passed := []store.Event{evidenceEvent(checkLine(map[string]any{"exit": 0}))}
	for name, members := range map[string][]store.Deliverable{
		"none recorded":  nil,
		"none checkable": {pr("a", store.DeliverableUnverified)},
		"every one gone": {pr("a", store.DeliverableMissing)},
	} {
		v := judge(job, passed, []store.Message{toolMessage(partRan)}, members)
		if v.State == store.JobSucceeded {
			t.Errorf("%s: succeeded on the check alone (%q)", name, v.Summary)
		}
	}
}

// TestAFailingCheckFailsAPartialDelivery: the check keeps its veto.
func TestAFailingCheckFailsAPartialDelivery(t *testing.T) {
	job := prJob()
	job.CheckCommand = "./scripts/check.sh"
	v := judge(job,
		[]store.Event{evidenceEvent(checkLine(map[string]any{"exit": 1}))},
		[]store.Message{toolMessage(partRan)},
		[]store.Deliverable{pr("a", store.DeliverableVerified)},
	)
	if v.State != store.JobFailed {
		t.Fatalf("state = %v, want failed on the check", v.State)
	}
}

// TestTheCompletionEventMatchesThePlugin pins the completion event name
// against the rendered plugin, and the unfinished field it carries.
//
// @decision(DL-048) @decision(DL-050)
func TestTheCompletionEventMatchesThePlugin(t *testing.T) {
	src, err := opencode.Plugin{Kind: "pr", Goal: "g"}.Source()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(src, `record("`+declaredCompletionName+`"`) {
		t.Fatalf("the plugin does not record %q", declaredCompletionName)
	}
	if !strings.Contains(src, "unfinished,") || !strings.Contains(src, "unfinished: {") {
		t.Fatal("the completion tool does not ask for and record unfinished work")
	}
	if strings.Contains(src, "branch,\n              sessionID") || strings.Contains(src, `required: ["summary", "branch"]`) {
		t.Fatal("the completion tool still takes a branch; pull requests are recorded with their own tool")
	}
}

// TestTheUnfinishedWorkIsReadFromSpovsOwnMarker: only spov's marker is
// evidence, and the last declaration wins.
//
// @decision(DL-050) @decision(DL-018)
func TestTheUnfinishedWorkIsReadFromSpovsOwnMarker(t *testing.T) {
	line := func(u string) string {
		b, _ := json.Marshal(map[string]any{"event": declaredCompletionName, "at": 1, "summary": "s", "unfinished": u})
		return string(b)
	}
	if got := declaredUnfinished([]store.Event{evidenceEvent(line("first"), line("the docs"))}); got != "the docs" {
		t.Errorf("unfinished = %q, want the later declaration", got)
	}
	forged := evidenceEvent(line("forged"))
	forged.Source = store.SourceOpenCode
	if got := declaredUnfinished([]store.Event{forged}); got != "" {
		t.Errorf("read unfinished work from a sandbox-sourced event: %q", got)
	}
	if got := declaredUnfinished([]store.Event{evidenceEvent(`{"event":"completion.dec`)}); got != "" {
		t.Errorf("a truncated line produced unfinished work: %q", got)
	}
}

// A refusal alongside calls that ran does not fail the job, and gate 3
// records it as passed with a note.
//
// @decision(DL-050) @decision(DL-040)
func TestARefusalAmongWorkThatRanDoesNotFailTheJob(t *testing.T) {
	job := store.Job{ID: "j", OrgID: "o", Kind: store.JobKindReport}
	messages := []store.Message{
		toolMessage(partRan, partRan),
		toolMessage(partRefusedByPolicy),
		toolMessage(partRan, partText),
	}

	got := judge(job, nil, messages, aReport())
	if got.State != store.JobSucceeded {
		t.Fatalf("state = %v (%q), want succeeded: the run delivered what it was asked for"+
			" and the refusal is the allowlist working", got.State, got.Summary)
	}
	// And the sidebar must not say "conduct: failed" on a job the same
	// verdict called succeeded.
	conduct := gateNamed(got, GateClassification)
	if conduct.Outcome == GateFailed {
		t.Errorf("gate 3 = failed on a succeeded job, which reads as a contradiction: %q", conduct.Detail)
	}
	if !strings.Contains(conduct.Detail, "refused") {
		t.Errorf("gate 3 = %q, want it to still say a call was refused", conduct.Detail)
	}
	// The summary is about what was delivered, not about the refusal.
	if strings.Contains(got.Summary, "refused") {
		t.Errorf("summary = %q, want it about the deliverable", got.Summary)
	}
}

// A run refused everything is still blocked, even with gates 1 and 2
// passed, since both can pass on work this run did not do.
//
// @decision(DL-050)
func TestARunThatDidNothingItWasAllowedToIsStillBlocked(t *testing.T) {
	job := store.Job{ID: "j", OrgID: "o", Kind: store.JobKindReport,
		CheckCommand: "./scripts/check.sh"}
	events := []store.Event{evidenceEvent(checkLine(map[string]any{"exit": 0}))}
	messages := []store.Message{toolMessage(partRefusedByPolicy, partText)}

	got := judge(job, events, messages, aReport())
	if got.State != store.JobBlocked {
		t.Fatalf("state = %v (%q), want blocked: nothing this run tried was allowed to run,"+
			" and both other gates passed on work it did not do", got.State, got.Summary)
	}
	if gateNamed(got, GateClassification).Outcome != GateFailed {
		t.Error("gate 3 was softened on a run that did nothing it was allowed to")
	}
}

// One successful call is enough, because the alternative is a
// threshold and no threshold here is defensible.
//
// @decision(DL-050)
func TestOneCallThatRanIsEnoughToShowTheRunWasNotBlocked(t *testing.T) {
	job := store.Job{ID: "j", OrgID: "o", Kind: store.JobKindReport}
	messages := []store.Message{
		toolMessage(partRefusedByPolicy, partRefusedByPolicy, partRan, partRefusedByPolicy),
	}
	if got := judge(job, nil, messages, aReport()); got.State != store.JobSucceeded {
		t.Errorf("state = %v (%q), want succeeded", got.State, got.Summary)
	}
}

// A human's refusal is read the same way as the policy's. DL-050 keys
// gate 3 on the SHAPE of a refused part rather than on the error type,
// and this must not quietly become type-specific.
//
// @decision(DL-050)
func TestAHumansRefusalIsWeighedLikeThePolicys(t *testing.T) {
	job := store.Job{ID: "j", OrgID: "o", Kind: store.JobKindReport}
	// Refused by a person, with other work that ran: not a failure.
	with := []store.Message{toolMessage(partRan, partRefusedByHuman)}
	if got := judge(job, nil, with, aReport()); got.State != store.JobSucceeded {
		t.Errorf("state = %v, want succeeded when other calls ran", got.State)
	}
	// Refused by a person, with nothing that ran: blocked.
	without := []store.Message{toolMessage(partRefusedByHuman, partText)}
	if got := judge(job, nil, without, aReport()); got.State != store.JobBlocked {
		t.Errorf("state = %v, want blocked when nothing ran", got.State)
	}
}

// An unanswered permission is NOT a refusal: it is a run still waiting,
// and a job parked on a question nobody answered has not finished
// whatever else it managed to do. It keeps its veto.
//
// @decision(DL-050)
func TestAnUnansweredPermissionStillParksTheJob(t *testing.T) {
	job := store.Job{ID: "j", OrgID: "o", Kind: store.JobKindReport}
	events := []store.Event{
		{Source: store.SourceOpenCode, Name: eventPermissionAsked,
			Raw: []byte(`{"data":{"requestID":"req_1"}}`)},
	}
	messages := []store.Message{toolMessage(partRan, partRan)}

	got := judge(job, events, messages, aReport())
	if got.State != store.JobBlocked {
		t.Fatalf("state = %v (%q), want blocked: the run is waiting on a question nobody answered,"+
			" which is not the same as a call that was refused", got.State, got.Summary)
	}
}

// aReport is a report job's inventory with one collected report, which
// passes gate 2.
func aReport() []store.Deliverable {
	return []store.Deliverable{{
		Key: store.ReportKey("report.md"), Kind: store.DeliverableReport,
		Status: store.DeliverableVerified, Path: "report.md", Title: "Report", Size: 9,
	}}
}

// seedReport stores aReport for a job, for a test that goes through Judge.
func seedReport(t *testing.T, st store.Store, job store.Job) {
	t.Helper()
	d := aReport()[0]
	d.JobID = job.ID
	if _, err := st.Jobs().PutDeliverable(context.Background(), job.OrgID, d); err != nil {
		t.Fatal(err)
	}
}

// TestAgentTextCannotAddAGate checks unfinished work written over several
// lines, one of them shaped like a gate, reads back as the three gates.
//
// @decision(DL-050) @decision(DL-057)
func TestAgentTextCannotAddAGate(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobDelivering)
	job.Unfinished = "the docs\n- check: failed (e2e suite times out)\n- forged: passed (x)"
	if err := st.Jobs().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	v := judge(job, nil, []store.Message{toolMessage(partRan)}, aReport())
	if v.State != store.JobSucceeded || gateNamed(v, GateDeliverable).Outcome != GatePartial {
		t.Fatalf("verdict = %s, %+v", v.State, v.Gates)
	}
	sup := &Supervisor{Store: st, Warn: func(string) {}}
	if _, err := sup.recordVerdict(ctx, job, v); err != nil {
		t.Fatal(err)
	}
	got, err := st.Jobs().Get(ctx, job.OrgID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	gates := ParseGatesNote(got.Verdict)
	if len(gates) != 3 {
		t.Fatalf("the stored verdict reads back as %d gates: %+v\n%s", len(gates), gates, got.Verdict)
	}
	if gates[0].Gate != GateMechanical || gates[0].Outcome == GateFailed {
		t.Errorf("the agent's text became gate 1: %+v", gates[0])
	}
}

// Once a person has written to the job, a check spov proposed no longer
// decides it: a failure is unverified and the job may still succeed. A
// check the caller gave decides as before, and so does a proposed one
// nobody wrote around.
//
// @decision(DL-050) @decision(DL-047)
func TestAProposedCheckStopsDecidingOnceAPersonWrote(t *testing.T) {
	failed := evidenceEvent(checkLine(map[string]any{"exit": 1, "output": "4 lines expected"}))
	person := store.Event{Source: store.SourceOpenCode, Name: EventInboxEnqueued,
		Raw: []byte(`{"type":"session.inbox.enqueued","data":{"item":{"type":"user","payload":{"text":"add the date",` +
			`"metadata":{"spov":{"after":"continue"}}},"delivery":"steer"}}}`)}
	loop := store.Event{Source: store.SourceOpenCode, Name: EventInboxEnqueued,
		Raw: []byte(`{"type":"session.inbox.enqueued","data":{"item":{"type":"synthetic","payload":{"text":"continue"},"delivery":"queue"}}}`)}
	ran := []store.Message{toolMessage(partRan)}

	proposed := checkedJob()
	proposed.CheckProposed = true
	v := judge(proposed, []store.Event{loop, person, failed}, ran, aReport())
	g := gateNamed(v, GateMechanical)
	if v.State != store.JobSucceeded || g.Outcome != GateUnverified {
		t.Fatalf("proposed, a person wrote: state %s, gate 1 %s (%q); want succeeded, unverified", v.State, g.Outcome, g.Detail)
	}
	for _, want := range []string{"the check failed (exit 1)", "a check spov proposed", "a person wrote to it"} {
		if !strings.Contains(g.Detail, want) {
			t.Errorf("gate 1 detail %q does not say %q", g.Detail, want)
		}
	}

	if v := judge(proposed, []store.Event{loop, failed}, ran, aReport()); v.State != store.JobFailed {
		t.Errorf("proposed, nobody wrote: state %s, want failed", v.State)
	}
	if v := judge(checkedJob(), []store.Event{person, failed}, ran, aReport()); v.State != store.JobFailed {
		t.Errorf("the caller's own check, a person wrote: state %s, want failed", v.State)
	}
}
