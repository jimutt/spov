package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/store"
)

// The verdict: whether a job achieved anything, computed from what the
// run left in the store rather than from anything the run says about
// itself. Three gates:
//
//   - gate 1, mechanical: the repo's own check command, run in the
//     sandbox and reported as the plugin's evidence.
//   - gate 2, the deliverables: for `report`, the reports collected from
//     the out directory; for `pr`, each recorded pull request found open
//     through spov's own forge client.
//   - gate 3, conduct: what the agent was allowed to do, read from tool
//     parts and permission events.
//
// It reads the store and never the sandbox.
//
// @decision(DL-050) @decision(DL-018)

// Verdict is what the gates concluded about one job.
//
// @decision(DL-050)
type Verdict struct {
	// State is where the job should go: succeeded, failed or blocked.
	State store.JobState
	// Summary is the one-line reason, for the job row a human reads.
	Summary string
	// Gates records what each gate saw, so a verdict can be argued
	// with rather than merely believed.
	Gates []GateResult
	// Retry reports that the run was cut off by infrastructure rather
	// than concluded — a sandbox shutdown — so the outcome is not the
	// job's fault and re-running it is meaningful.
	Retry bool
	// Deliverables is the inventory gate 2 judged, for the metrics.
	//
	// @decision(DL-050)
	Deliverables []store.Deliverable
}

// GateOutcome is what one gate concluded.
//
// @decision(DL-050)
type GateOutcome string

const (
	// GatePassed is the gate's condition met.
	GatePassed GateOutcome = "passed"
	// GateFailed is the gate's condition definitively not met.
	GateFailed GateOutcome = "failed"
	// GateUnverified is the gate having nothing to judge on. It is not a
	// failure: the job is classified on the gates that did report.
	GateUnverified GateOutcome = "unverified"
	// GatePartial is gate 2 with at least one deliverable verified and
	// something qualifying it: a member missing, unverified or over the
	// limit, unfinished work declared, or an ending at the wall clock.
	// It carries a success, which is then a partial one.
	//
	// @decision(DL-050)
	GatePartial GateOutcome = "partial"
)

// GateResult is one gate's finding.
//
// @decision(DL-050)
type GateResult struct {
	Gate string `json:"gate"`
	// Outcome is declared as an enum so the generated client narrows it
	// rather than typing it `string`.
	//
	// @decision(DL-057)
	Outcome GateOutcome `json:"outcome" enum:"passed,partial,failed,unverified"`
	Detail  string      `json:"detail"`
	// Parked marks a failed conduct gate as a run waiting on an
	// unanswered permission rather than one that was refused. A park
	// vetoes even when other calls ran. Internal only: clients rebuild
	// gates from the verdict text.
	//
	// @decision(DL-050)
	Parked bool `json:"-"`
	// Unrunnable says the caller's check command could not be run at
	// all (the shell answered 126 or 127). It vetoes success. Internal
	// only, like Parked.
	//
	// @decision(DL-050)
	Unrunnable bool `json:"-"`
}

// The gate names, as they appear in a job's recorded verdict. Exported
// because the verdict text is public and tests on both sides of the
// package boundary name them.
//
// @decision(DL-050) @decision(DL-046)
const (
	GateMechanical     = "check"
	GateDeliverable    = "deliverable"
	GateClassification = "conduct"
)

// checkEvidence is what the plugin records about the repo's own check.
// The line is flat, `{event, at, ...data}`, with no nested `data`
// object; TestGateOneReadsTheLineShapeThePluginWrites pins the shape.
//
// @decision(DL-050) @decision(DL-039)
type checkEvidence struct {
	Event string `json:"event"`
	// Exit is the check command's exit status. Zero is the pass.
	Exit *int `json:"exit"`
	// Timeout and Infra mean the check never got to run, as opposed to
	// running and failing.
	Timeout bool   `json:"timeout"`
	Infra   bool   `json:"infra"`
	Command string `json:"command"`
	Output  string `json:"output"`
}

// checkEventName is the evidence event the plugin writes when it has
// run the repo's check command. It must match CONFIG.checkEvent in
// spov-goal.js, or gate 1 is silently unverified on every job;
// TestTheCheckEventMatchesThePlugin guards it.
//
// @decision(DL-050) @decision(DL-039)
const checkEventName = "check.finished"

// The shell's own "I could not run that" statuses. POSIX: 127 is not
// found, 126 is found but not executable. Neither is the code failing.
//
// @decision(DL-050)
const (
	shellNotFound      = 127
	shellNotExecutable = 126
)

// Judge computes a job's verdict from what it left in the store. It
// writes nothing and reads no sandbox; the gates themselves are the
// pure judge.
//
// @decision(DL-050) @decision(DL-018) @decision(DL-114)
func (s *Supervisor) Judge(ctx context.Context, job store.Job) (Verdict, error) {
	events, err := s.jobEvents(ctx, job)
	if err != nil {
		return Verdict{}, err
	}
	own, err := s.ownConversation(ctx, job)
	if err != nil {
		return Verdict{}, err
	}
	events = withoutOthersInbox(events, own)
	messages, err := s.jobMessages(ctx, job)
	if err != nil {
		return Verdict{}, err
	}
	have, err := s.Store.Jobs().Deliverables(ctx, job.OrgID, []string{job.ID})
	if err != nil {
		return Verdict{}, fmt.Errorf("supervisor: job %s: reading deliverables for the verdict: %w", job.ID, err)
	}
	v := judge(job, events, messages, have[job.ID])
	v.Deliverables = have[job.ID]
	return v, nil
}

// judge is the pure half, so every gate is testable from fixtures. A job
// that ended at its wall clock has the verdict open with that.
//
// @decision(DL-050) @decision(DL-049)
func judge(job store.Job, events []store.Event, messages []store.Message, members []store.Deliverable) Verdict {
	v := judgeGates(job, events, messages, members)
	// @decision(DL-034)
	if note := spovNote(events, MarkerProxyFailed); note != "" {
		v.State = store.JobFailed
		v.Summary = note + "; " + v.Summary
	}
	for _, note := range []string{boundNote(events), deniedNote(events), failedTurnNote(events)} {
		if note != "" {
			v.Summary = note + "; " + v.Summary
		}
	}
	return v
}

// deniedNote names the call whose repeated denial ended the run, from
// the plugin's first stop marker, redacted, since a denied shell command
// can carry a credential. Like any marker it is the sandbox's word.
//
// @decision(DL-040)
func deniedNote(events []store.Event) string {
	for _, e := range events {
		if e.Source == store.SourceSpov || !isLoopStopped(e) {
			continue
		}
		p := stopPayloadOf(e)
		if p.Reason != opencode.DeniedReason {
			return ""
		}
		call := redact.Text(p.Call)
		if r := []rune(call); len(r) > maxDeniedCall {
			call = string(r[:maxDeniedCall]) + "…"
		}
		if call == "" {
			call = "(not recorded)"
		}
		return fmt.Sprintf("the run was ended after the same call was denied %d times in a row: %s",
			opencode.DenialStopAt, call)
	}
	return ""
}

// failedTurnNote names the error of the failed turns that ended the run,
// from the plugin's first stop marker, redacted, since a provider's error
// can echo what it was sent.
//
// @decision(DL-048)
func failedTurnNote(events []store.Event) string {
	for _, e := range events {
		if e.Source == store.SourceSpov || !isLoopStopped(e) {
			continue
		}
		p := stopPayloadOf(e)
		if p.Reason != opencode.TurnFailedReason {
			return ""
		}
		msg := redact.Text(p.Error)
		if r := []rune(msg); len(r) > maxDeniedCall {
			msg = string(r[:maxDeniedCall]) + "…"
		}
		if msg == "" {
			msg = "(no error given)"
		}
		return fmt.Sprintf("the run was ended after %d turns in a row failed: %s", len(opencode.TurnRetryDelays)+1, msg)
	}
	return ""
}

// maxDeniedCall bounds the call a verdict quotes.
const maxDeniedCall = 300

// boundNote is the wall clock's note when the job ended at it, read from
// spov's own marker and never from anything the sandbox wrote.
//
// @decision(DL-049)
func boundNote(events []store.Event) string {
	return spovNote(events, MarkerBounded)
}

// spovNote is the text of spov's own marker named name, never of
// anything the sandbox wrote.
func spovNote(events []store.Event, name string) string {
	for _, e := range events {
		if e.Source == store.SourceSpov && e.Name == name {
			return e.Text
		}
	}
	return ""
}

// judgeGates applies the three gates.
//
// @decision(DL-050)
func judgeGates(job store.Job, events []store.Event, messages []store.Message, members []store.Deliverable) Verdict {
	v := Verdict{}
	mech := judgeMechanical(job, events)
	deliv := judgeDeliverable(job, events, members)
	// The human stops, read once. interrupted.go uses them to tell a
	// stopped call from a refused one.
	//
	// @decision(DL-053)
	stops := humanStops(events)
	conduct := judgeConduct(events, messages, stops)
	v.Gates = []GateResult{mech, deliv, conduct}

	// A sandbox shutdown mid-run makes the outcome a retry note, not a
	// re-queue. Set whatever the gates concluded: a timed-out run has
	// usually made tool calls already.
	//
	// @decision(DL-050)
	v.Retry = wasShutDown(events, messages)

	// A refusal vetoes only a run in which no tool call ran; a park
	// always vetoes. Whether anything ran is asked of the tool parts,
	// never of gates 1 and 2, which can pass on work this run did not do.
	//
	// @decision(DL-050) @decision(DL-040)
	worked := anyToolRan(messages, stops)
	if conduct.Outcome == GateFailed && (conduct.Parked || !worked) {
		v.State = store.JobBlocked
		v.Summary = conduct.Detail
		return v
	}
	// A refusal that did not decide the outcome is recorded as passed,
	// with a detail saying a call was refused and the run continued.
	//
	// @decision(DL-050)
	if conduct.Outcome == GateFailed {
		conduct = GateResult{
			Gate:    GateClassification,
			Outcome: GatePassed,
			Detail:  "a tool call was refused; the run continued and delivered",
		}
		// Rebuilt rather than indexed: an index would be silently wrong
		// the day a fourth gate is added or the order changes.
		v.Gates = []GateResult{mech, deliv, conduct}
	}
	// A definitively failing check is a failure even if something was
	// delivered: a PR that does not build is not the deliverable.
	if mech.Outcome == GateFailed {
		v.State = store.JobFailed
		v.Summary = mech.Detail
		return v
	}
	if deliv.Outcome == GateFailed {
		v.State = store.JobFailed
		v.Summary = deliv.Detail
		return v
	}
	// A check the caller asked for and spov could not run vetoes
	// success. It sits after the two failure checks so a failing gate
	// reports its own reason, and before the success check.
	//
	// @decision(DL-050)
	if mech.Unrunnable {
		v.State = store.JobFailed
		v.Summary = mech.Detail
		return v
	}
	// Nothing failed. Success needs a verified deliverable: gate 2
	// passed or partial. A passing check carries no success on its own,
	// since a job that delivered nothing verifiable did not deliver.
	//
	// @decision(DL-050)
	if deliv.Outcome == GatePassed || deliv.Outcome == GatePartial {
		v.State = store.JobSucceeded
		// The summary does not mention a refusal that did not change
		// the outcome.
		//
		// @decision(DL-050)
		v.Summary = passSummary(mech, deliv)
		return v
	}
	v.State = store.JobFailed
	v.Summary = "the job finished without a verified deliverable: " + unverifiedWhy(mech, deliv)
	return v
}

// passSummary says which gates carried the pass, so a success that
// rested on one gate is not read as one that rested on all of them.
func passSummary(mech, deliv GateResult) string {
	var passed []string
	if mech.Outcome == GatePassed {
		passed = append(passed, "the repo's own check passed")
	}
	switch deliv.Outcome {
	case GatePassed:
		passed = append(passed, "the deliverables are there")
	case GatePartial:
		passed = append(passed, "part of the deliverables are there ("+deliv.Detail+")")
	}
	summary := strings.Join(passed, " and ")
	// An unverified gate is named in the summary rather than left out.
	// A success that rested on one gate because the other could not
	// report is a weaker claim than one that cleared both, and the row
	// should say so rather than presenting them alike.
	var missing []string
	if mech.Outcome == GateUnverified {
		missing = append(missing, "the check")
	}
	if len(missing) > 0 {
		summary += "; " + strings.Join(missing, " and ") + " could not be verified"
	}
	return summary
}

// unverifiedWhy explains a job that cleared no gate at all.
func unverifiedWhy(mech, deliv GateResult) string {
	return mech.Detail + "; " + deliv.Detail
}

// judgeMechanical is gate 1: the repo's own check command, read from
// the plugin evidence the supervisor stored as a spov marker at
// delivery. No evidence is GateUnverified, not GateFailed.
//
// @decision(DL-050) @decision(DL-039)
func judgeMechanical(job store.Job, events []store.Event) (res GateResult) {
	res = GateResult{Gate: GateMechanical, Outcome: GateUnverified,
		Detail: "the run left no record of a check command"}
	// A check spov proposed stops deciding once a person has written to
	// the job: what they asked is newer than the goal it was drawn from.
	// Registered first, so it runs after the provenance note.
	//
	// @decision(DL-050)
	defer func() {
		if job.CheckProposed && strings.TrimSpace(job.CheckCommand) != "" && personWrote(events) {
			res.Outcome = GateUnverified
			res.Unrunnable = false
			res.Detail += "; it no longer decides the job, since a person wrote to it after spov proposed it"
		}
	}()
	// Marks a check spov proposed, on every outcome. Deferred rather
	// than applied at the end so the early (unverified) returns get it too.
	//
	// @decision(DL-047)
	defer func() { res.Detail = withCheckProvenance(res.Detail, job) }()
	// A job with no check configured has no gate 1, whatever the
	// evidence file says. This closes the cheapest forgery (the agent
	// can write the file) and is not a short-circuit; do not remove it.
	// See DL-050.
	//
	// @decision(DL-050)
	if strings.TrimSpace(job.CheckCommand) == "" {
		res.Detail = "the job was started without a check command, so there is no mechanical gate"
		return res
	}
	check, ok := latestCheck(events)
	if !ok {
		return res
	}
	switch {
	case check.Timeout:
		// Not a failure: the check did not report.
		res.Detail = "the check timed out before it reported"
		return res
	case check.Infra:
		res.Detail = "the check could not run: " + firstLine(check.Output)
		return res
	case check.Exit == nil:
		res.Detail = "the check recorded no exit status"
		return res
	case *check.Exit == 0:
		res.Outcome = GatePassed
		res.Detail = "the check passed"
		return res
	case *check.Exit == shellNotFound || *check.Exit == shellNotExecutable:
		// The shell could not run the command. The outcome stays
		// unverified, not failed; Unrunnable adds the veto, but only
		// for a check the caller gave, never one spov proposed.
		//
		// @decision(DL-050) @decision(DL-047)
		res.Unrunnable = !job.CheckProposed
		res.Detail = fmt.Sprintf("the check command could not be run (exit %d): %s",
			*check.Exit, firstLine(check.Output))
		return res
	default:
		res.Outcome = GateFailed
		res.Detail = fmt.Sprintf("the check failed (exit %d): %s", *check.Exit, firstLine(check.Output))
		return res
	}
}

// personWrote reports whether a person put anything in the job's inbox:
// an enqueued item of type user. The opening goal and the plugin's
// continuations are synthetic. Once the recording has stopped, spov's
// own record of a send stands in for the item the log can no longer
// show.
//
// @decision(DL-050) @decision(DL-131)
func personWrote(events []store.Event) bool {
	stopped := false
	for _, e := range events {
		if e.Source == store.SourceOpenCode && e.Name == EventInboxEnqueued && InboxItemType(e.Raw) == "user" {
			return true
		}
		if isRecordingStopped(e) {
			stopped = true
		}
		if _, sent := steerRecordOf(e); sent && stopped {
			return true
		}
	}
	return false
}

// withCheckProvenance notes a check spov proposed rather than was given.
//
// @decision(DL-047)
func withCheckProvenance(detail string, job store.Job) string {
	if !job.CheckProposed {
		return detail
	}
	return detail + " (a check spov proposed, not one the caller gave)"
}

// latestCheck finds the last check the plugin recorded: a run may check
// more than once, and the verdict is about where it finished.
//
// @decision(DL-050)
func latestCheck(events []store.Event) (checkEvidence, bool) {
	var out checkEvidence
	found := false
	for _, e := range events {
		// The evidence is spov's own marker, and it is matched on
		// source as well as name: an event the sandbox named the same
		// thing must never be read as spov's record of the check.
		//
		// @decision(DL-018)
		if !isRecordMarker(e) {
			continue
		}
		for _, line := range strings.Split(e.Text, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var c checkEvidence
			if err := json.Unmarshal([]byte(line), &c); err != nil {
				// A truncated evidence file ends mid-line; not a fault.
				continue
			}
			if c.Event != checkEventName {
				continue
			}
			out, found = c, true
		}
	}
	return out, found
}

// declaredCompletionName is the evidence event the plugin writes when the
// agent declares completion. It must match the literal in spov-goal.js;
// TestTheCompletionEventMatchesThePlugin guards it.
//
// @decision(DL-048) @decision(DL-050)
const declaredCompletionName = "completion.declared"

// changeNoun names a pr job's members the way their forges do: merge
// requests when every one is GitLab's, pull requests otherwise.
//
// @decision(DL-051)
func changeNoun(members []store.Deliverable) string {
	noun := ""
	for _, m := range members {
		n := git.ChangeNoun(m.Forge)
		if noun != "" && n != noun {
			return "pull request"
		}
		noun = n
	}
	if noun == "" {
		return "pull request"
	}
	return noun
}

// judgeDeliverable is gate 2, over the job's whole inventory: the pull
// requests the agent recorded and the forge was asked about, or the
// reports collected from the out directory.
//
// @decision(DL-050)
func judgeDeliverable(job store.Job, events []store.Event, members []store.Deliverable) GateResult {
	// @decision(DL-101)
	if job.Kind == store.JobKindSync {
		return judgeSync(job, events, members)
	}
	res := GateResult{Gate: GateDeliverable}
	noun := changeNoun(members)
	if job.Kind == store.JobKindReport {
		noun = "report"
	} else if job.Kind != store.JobKindPR {
		res.Outcome = GateUnverified
		res.Detail = "no deliverable is defined for this kind of job"
		return res
	}
	count := map[store.DeliverableStatus]int{}
	lostImages := 0
	// A withdrawn member is named but not judged (DL-051).
	withdrawn := 0
	var judged []store.Deliverable
	for _, m := range members {
		if m.Status == store.DeliverableWithdrawn {
			withdrawn++
			continue
		}
		judged = append(judged, m)
	}
	members = judged
	closed := 0
	for _, m := range members {
		// A missing pull request the forge found closed has its number.
		//
		// @decision(DL-051)
		if m.Status == store.DeliverableMissing && m.Kind == store.DeliverablePR && m.Number > 0 {
			closed++
			continue
		}
		count[m.Status]++
		// @decision(DL-051)
		if m.Status == store.DeliverableVerified && m.ImagesLost {
			lostImages++
		}
	}
	why, broke := collectionFailed(events)
	bounded := boundNote(events) != ""
	verified := count[store.DeliverableVerified]

	var parts []string
	if len(members) > 0 {
		parts = append(parts, fmt.Sprintf("%d of %s verified", verified, plural(len(members), noun)))
	}
	for _, st := range []struct {
		status store.DeliverableStatus
		says   string
	}{
		{store.DeliverableMissing, "not found on the forge"},
		{store.DeliverableUnverified, "could not be checked"},
		{store.DeliverableOverLimit, "over the size limit"},
	} {
		if n := count[st.status]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, st.says))
		}
	}
	if closed > 0 {
		parts = append(parts, fmt.Sprintf("%d closed without merging", closed))
	}
	if lostImages > 0 {
		parts = append(parts, fmt.Sprintf("%s lost some linked images", plural(lostImages, noun)))
	}
	// @decision(DL-051)
	if withdrawn > 0 {
		parts = append(parts, fmt.Sprintf("%d withdrawn at the reader's request", withdrawn))
	}
	if n := unaskedWithdrawals(events, members); n > 0 {
		parts = append(parts, fmt.Sprintf("the agent withdrew %s nobody asked it to drop, judged as recorded", plural(n, noun)))
	}
	if broke {
		parts = append(parts, "the out directory could not be read: "+why)
	}
	if job.Unfinished != "" {
		parts = append(parts, "the agent left unfinished: "+truncateRunes(oneLine(job.Unfinished), 300))
	}
	if bounded {
		parts = append(parts, "the run ended at its wall clock before declaring itself done")
	}

	qualified := verified < len(members) || lostImages > 0 || broke || job.Unfinished != "" || bounded
	switch {
	case verified > 0 && !qualified:
		res.Outcome = GatePassed
	case verified > 0:
		res.Outcome = GatePartial
	case count[store.DeliverableUnverified] > 0 || broke:
		res.Outcome = GateUnverified
	default:
		res.Outcome = GateFailed
		if len(members) == 0 && withdrawn > 0 {
			parts = append([]string{"every " + noun + " the job recorded was withdrawn"}, parts...)
		} else if len(members) == 0 {
			if job.Kind == store.JobKindPR {
				parts = append([]string{"the job recorded no pull request or merge request"}, parts...)
			} else {
				parts = append([]string{"the job left no report in " + opencode.OutDir}, parts...)
			}
		}
	}
	res.Detail = strings.Join(parts, "; ")
	return res
}

// judgeConduct is gate 3: what the agent was allowed to do. It is read
// on every job and is the only gate that can veto a success. It fails on
// a refused tool part, or on a permission asked and never answered
// (parked).
//
// @decision(DL-050) @decision(DL-046)
func judgeConduct(events []store.Event, messages []store.Message, stops []int64) GateResult {
	res := GateResult{Gate: GateClassification}
	if part, ok := firstRefusal(messages, stops); ok {
		res.Outcome = GateFailed
		res.Detail = refusalDetail(part)
		return res
	}
	if n := unansweredPermissions(events); n > 0 {
		res.Outcome = GateFailed
		res.Detail = fmt.Sprintf("the run is waiting on %s nobody answered", plural(n, "permission request"))
		// A park is not a refusal and is never softened in judge.
		//
		// @decision(DL-050)
		res.Parked = true
		return res
	}
	// Nothing refused is a pass only if some tool call actually ran,
	// not merely was called: a call stopped before it started does not count.
	//
	// @decision(DL-053) @decision(DL-050)
	if !anyToolRan(messages, stops) {
		res.Outcome = GateUnverified
		res.Detail = "the run made no tool calls, so there is nothing to classify"
		if anyToolCall(messages) {
			res.Detail = "the run's tool calls were stopped before they ran, so there is nothing to classify"
		}
		return res
	}
	res.Outcome = GatePassed
	res.Detail = "every tool call was allowed to run"
	return res
}

// refusalDetail says what was refused and, when the marker is one of
// the measured ones, by which path. It never uses the agent's own
// narration of the denial.
//
// @decision(DL-050)
func refusalDetail(p toolPart) string {
	what := "a tool call"
	if p.Name != "" {
		what = "the " + p.Name + " call"
	}
	return "the run was refused: " + what + " did not run because " + refusalReason(p)
}

// firstRefusal finds the earliest refused tool call not stopped by a
// human; an early refusal is often the cause of what follows.
//
// @decision(DL-050)
func firstRefusal(messages []store.Message, stops []int64) (toolPart, bool) {
	for _, m := range messages {
		for _, p := range toolParts(m.Raw) {
			if p.Refused() && !stoppedByAHuman(p, stops) {
				return p, true
			}
		}
	}
	return toolPart{}, false
}

// anyToolRan reports whether any tool call actually executed: a part
// that is not a refusal, or one a human stopped after it had started
// (RanAt set). Do not drop the RanAt check; see DL-053. `executed` is
// not the test; see DL-050.
//
// @decision(DL-050) @decision(DL-053)
func anyToolRan(messages []store.Message, stops []int64) bool {
	for _, m := range messages {
		for _, p := range toolParts(m.Raw) {
			if !p.Refused() || (p.RanAt > 0 && stoppedByAHuman(p, stops)) {
				return true
			}
		}
	}
	return false
}

// anyToolCall reports whether the run made any tool call at all.
func anyToolCall(messages []store.Message) bool {
	for _, m := range messages {
		if len(toolParts(m.Raw)) > 0 {
			return true
		}
	}
	return false
}

// unansweredPermissions counts permissions asked and never answered,
// paired by request: an asked frame names its request at data.id, a
// replied one at requestID, and spov's MarkerAnswered names the one a
// person answered through it. An ask whose id cannot be read counts as
// unanswered, so an unreadable frame cannot hide a park. A turn ending
// clears nothing here: delivery's own interrupt ends the turn of a job
// abandoned while blocked.
//
// @decision(DL-050) @decision(DL-046)
func unansweredPermissions(events []store.Event) int {
	asked := map[string]bool{}
	answered := map[string]bool{}
	unreadable := 0
	for _, e := range events {
		if e.Source == store.SourceSpov && e.Name == MarkerAnswered {
			if id := AnsweredRequest(e.Raw); id != "" {
				answered[id] = true
			}
			continue
		}
		if e.Source != store.SourceOpenCode && e.Source != store.SourceEphemeral {
			continue
		}
		// A human stop ends the turn, so it clears every question asked
		// before it. Events are in position order, so a later ask is a
		// real park.
		//
		// @decision(DL-053) @decision(DL-050)
		if isHumanStop(e) {
			asked = map[string]bool{}
			unreadable = 0
			continue
		}
		switch e.Name {
		case eventPermissionAsked:
			id := askedRequestID(e.Raw)
			if id == "" {
				unreadable++
				continue
			}
			asked[id] = true
		case eventPermissionReplied:
			if id := permissionRequestID(e.Raw); id != "" {
				// An unpairable reply clears nothing, so it cannot hide a park.
				answered[id] = true
			}
		}
	}
	open := 0
	for id := range asked {
		if !answered[id] {
			open++
		}
	}
	return open + unreadable
}

// askedRequestID is the request a permission.asked frame raises: data.id
// in the measured shape (PermissionID), else a requestID spelling.
//
// @decision(DL-050) @decision(DL-020)
func askedRequestID(raw []byte) string {
	if id := PermissionID(raw); id != "" {
		return id
	}
	return permissionRequestID(raw)
}

// permissionRequestID reads the request id from the frame's `data`
// object. The frame's top-level `id` is the event's, not the request's.
//
// @decision(DL-050) @decision(DL-046)
func permissionRequestID(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var frame map[string]any
	if err := json.Unmarshal(raw, &frame); err != nil {
		return ""
	}
	data, ok := frame["data"].(map[string]any)
	if !ok {
		return ""
	}
	for _, k := range []string{"requestID", "requestId", "request_id"} {
		if s, ok := data[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// wasShutDown reports that a turn was interrupted by the sandbox
// stopping rather than by anything about the work. A human's tool-call
// reject raises the same `shutdown` interrupt, so a shutdown near a
// declined call is not counted. Without that correlation the shutdown
// counts.
//
// @decision(DL-050) @decision(DL-053)
func wasShutDown(events []store.Event, messages []store.Message) bool {
	shutdowns := interruptsBecause(events, stopReasonShutdown)
	if len(shutdowns) == 0 {
		return false
	}
	// Read only when needed: declineTimes parses every message.
	declines := declineTimes(messages)
	for _, at := range shutdowns {
		// A shutdown with no `created` correlates with nothing and counts.
		if !nearAny(at, declines) {
			return true
		}
	}
	return false
}

// interruptReason reads why a turn was interrupted.
func interruptReason(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var frame map[string]any
	if err := json.Unmarshal(raw, &frame); err != nil {
		return ""
	}
	data, ok := frame["data"].(map[string]any)
	if !ok {
		return ""
	}
	s, _ := data["reason"].(string)
	return s
}

// firstLine trims output to something a job row can carry.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "no output"
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const max = 200
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// plural renders a count with its noun.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// jobEvents reads the whole of a job's event stream, from the start
// rather than from the job's follow cursor.
//
// @decision(DL-050) @decision(DL-046)
func (s *Supervisor) jobEvents(ctx context.Context, job store.Job) ([]store.Event, error) {
	var out []store.Event
	var after int64
	var dropped int
	var held int64
	for {
		batch, err := s.Store.Conversations().ListEvents(ctx, job.OrgID, job.SessionID, after, FollowBatch)
		if err != nil {
			return nil, fmt.Errorf("supervisor: job %s: reading events for the verdict: %w", job.ID, err)
		}
		out = append(out, batch...)
		for _, e := range batch {
			held += eventBytes(e)
		}
		// Past the bound drop the head, never the tail: gate 1's
		// evidence marker is the last event a job has. Trim before the
		// short-batch return, since every run ends on a short batch.
		//
		// @decision(DL-050)
		if max := s.maxVerdictEvents(); len(out) > max {
			n := len(out) - max
			for _, e := range out[:n] {
				held -= eventBytes(e)
			}
			out = out[n:]
			dropped += n
		}
		// The same trim by size: a count bounds nothing when the
		// sandbox chooses how large a frame is. The newest event is
		// always kept.
		//
		// @decision(DL-131)
		if max := s.maxVerdictBytes(); held > max {
			n := 0
			for n < len(out)-1 && held > max {
				held -= eventBytes(out[n])
				n++
			}
			// A copy, so the dropped head is not kept alive by the
			// slice's backing array.
			out = append([]store.Event(nil), out[n:]...)
			dropped += n
		}
		if len(batch) < FollowBatch {
			if dropped > 0 {
				s.warn("supervisor: job %s: the run was longer than %d events or %d MiB; judging on the last %d, "+
					"so anything in the first %d is not seen",
					job.ID, s.maxVerdictEvents(), s.maxVerdictBytes()>>20, len(out), dropped)
			}
			return out, nil
		}
		last := batch[len(batch)-1].Pos
		// The same guard follow() carries: a full batch that did not
		// advance would page over the same rows forever.
		if last <= after {
			return nil, fmt.Errorf(
				"supervisor: job %s: a full batch did not advance past %d while judging", job.ID, after)
		}
		after = last
	}
}

// MaxVerdictEvents bounds how many events the verdict holds at once. It
// is a memory bound; past it the head of the run is dropped.
//
// @decision(DL-050)
const MaxVerdictEvents = 20000

// maxVerdictEvents is the bound in force. Supervisor.MaxVerdictEvents
// overrides it so a test need not write 20 000 rows.
//
// @decision(DL-050)
func (s *Supervisor) maxVerdictEvents() int {
	if s.MaxVerdictEvents > 0 {
		return s.MaxVerdictEvents
	}
	return MaxVerdictEvents
}

// MaxVerdictBytes bounds the size of the events the verdict holds at
// once, by their text and raw frames. Past it the head of the run is
// dropped, as at MaxVerdictEvents.
//
// @decision(DL-131)
const MaxVerdictBytes = 256 << 20

// maxVerdictBytes is the bound in force, which a test lowers.
//
// @decision(DL-131)
func (s *Supervisor) maxVerdictBytes() int64 {
	if s.MaxVerdictBytes > 0 {
		return s.MaxVerdictBytes
	}
	return MaxVerdictBytes
}

func eventBytes(e store.Event) int64 { return int64(len(e.Text) + len(e.Raw)) }

// MaxVerdictMessages bounds how many of a job's newest messages the
// verdict reads, beside MaxVerdictBytes of them.
//
// @decision(DL-131)
const MaxVerdictMessages = 20_000

// jobMessages reads the conversation the run produced: its newest
// messages, at most MaxVerdictMessages and MaxVerdictBytes of them, as
// jobEvents keeps the newest events.
//
// @decision(DL-050) @decision(DL-131)
func (s *Supervisor) jobMessages(ctx context.Context, job store.Job) ([]store.Message, error) {
	msgs, more, err := s.Store.Conversations().RecentMessages(ctx, job.OrgID, job.SessionID, "", MaxVerdictMessages, s.maxVerdictBytes())
	if err != nil {
		return nil, fmt.Errorf("supervisor: job %s: reading messages for the verdict: %w", job.ID, err)
	}
	if more {
		s.warn("supervisor: job %s: the verdict reads only the newest %d messages", job.ID, len(msgs))
	}
	return msgs, nil
}

// recordVerdict writes the job's end: the state the gates concluded,
// the reason, and the gates themselves. The row is re-read inside the
// transaction, so a cancel that landed while the gates were read wins.
// It returns the row it wrote, nil when the job was already decided.
//
// @decision(DL-050) @decision(DL-045)
func (s *Supervisor) recordVerdict(ctx context.Context, job store.Job, v Verdict) (*store.Job, error) {
	var wrote *store.Job
	err := s.Store.Tx(ctx, func(tx store.Store) error {
		current, err := tx.Jobs().Get(ctx, job.OrgID, job.ID)
		if err != nil {
			return err
		}
		if isTerminalJob(current.State) {
			// Decided while the gates were read; leave it.
			return nil
		}
		if !store.CanTransition(current.State, v.State) {
			// There is no delivering -> blocked edge, so a refused run
			// that reached delivery is recorded failed with the refusal
			// as its reason.
			s.warn("supervisor: job %s: %s is not reachable from %s; recording it as failed",
				job.ID, v.State, current.State)
			current.State = store.JobFailed
		} else {
			current.State = v.State
		}
		// The summary goes to Error only when the job did not succeed,
		// since clients render Error as a failure. The gates go to
		// Verdict either way.
		//
		// @decision(DL-050)
		if current.State != store.JobSucceeded {
			current.Error = v.Summary
		} else {
			// Cleared, not left unset: the row may still carry GapNote
			// from a follow pass (DL-048).
			current.Error = ""
		}
		// One line, since a summary can carry agent-written text, and a
		// `- ` line in it would read back as a gate.
		//
		// @decision(DL-057) @decision(DL-050)
		current.Verdict = oneLine(v.Summary)
		if detail := gatesNote(v); detail != "" {
			current.Verdict = current.Verdict + "\n" + detail
		}
		if err := tx.Jobs().Update(ctx, current); err != nil {
			return err
		}
		wrote = &current
		return nil
	})
	if err != nil {
		return nil, err
	}
	return wrote, nil
}

// gatesNote renders what each gate found, one `- name: outcome (detail)`
// line per gate, plus the retry note. ParseGatesNote is its inverse.
//
// @decision(DL-050)
func gatesNote(v Verdict) string {
	var b strings.Builder
	for _, g := range v.Gates {
		// oneLine, because a detail can carry sandbox-chosen text and a
		// newline would break the line format.
		//
		// @decision(DL-057) @decision(DL-050)
		fmt.Fprintf(&b, "- %s: %s (%s)\n", g.Gate, g.Outcome, oneLine(g.Detail))
	}
	if v.Retry {
		b.WriteString("- the sandbox shut down mid-run, so this outcome is about the clock rather than the work\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// oneLine collapses any run of whitespace to a single space, so a
// value that is not spov's own wording cannot break the line format
// gatesNote writes.
//
// @decision(DL-057)
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// ParseGatesNote reads gatesNote back into gates, so the API can send
// them as structure; TestTheGatesSurviveTheRoundTrip pins the pair. Only
// `- ` lines are read, so the summary line is skipped. A line without the
// gate shape, such as the retry note, is dropped rather than guessed at.
// The name is cut at the first `: ` and the detail runs to the final `)`,
// since a detail may contain both.
//
// @decision(DL-057) @decision(DL-050)
func ParseGatesNote(verdict string) []GateResult {
	var out []GateResult
	for _, line := range strings.Split(verdict, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "- ")
		if !ok {
			continue
		}
		// The detail is parenthesised and may contain parentheses of
		// its own, so the closing one is the line's last character.
		open := strings.Index(rest, " (")
		if open < 0 || !strings.HasSuffix(rest, ")") {
			continue
		}
		head, detail := rest[:open], rest[open+2:len(rest)-1]
		name, outcome, ok := strings.Cut(head, ": ")
		if !ok {
			continue
		}
		// An unknown outcome is dropped: the field is an enum in the API.
		switch GateOutcome(outcome) {
		case GatePassed, GatePartial, GateFailed, GateUnverified:
		default:
			continue
		}
		out = append(out, GateResult{
			Gate: name, Outcome: GateOutcome(outcome), Detail: detail,
		})
	}
	return out
}

// collectionFailed reports whether delivery recorded that it could not
// read what the job left, and why.
//
// @decision(DL-050) @decision(DL-018)
func collectionFailed(events []store.Event) (string, bool) {
	why, broke := "", false
	for _, e := range events {
		// spov's own voice only: a sandbox naming an event this way
		// must not be able to turn its own empty output into
		// "unverified" and dodge gate 2. A later collection that worked
		// clears an earlier failure.
		if e.Source != store.SourceSpov {
			continue
		}
		switch e.Name {
		case MarkerCollectFailed:
			why, broke = e.Text, true
		case MarkerCollected:
			why, broke = "", false
		}
	}
	return why, broke
}

// isRecordMarker is spov's stored copy of the plugin's evidence or
// outcome file, never anything the sandbox wrote.
//
// @decision(DL-050) @decision(DL-051) @decision(DL-018)
func isRecordMarker(e store.Event) bool {
	return e.Source == store.SourceSpov && (e.Name == MarkerEvidence || e.Name == MarkerOutcome)
}

// ForgeFor produces the forge to ask on behalf of one user. It is the
// server's own per-user credential resolution, narrowed to what the
// gates need and held as a function so DL-046's wall holds.
//
// @decision(DL-050) @decision(DL-046)
//
// It takes the job's session, whose recorded credential tiers it follows
// (DL-042).
type ForgeFor func(ctx context.Context, sess store.Session) (Forge, error)

// FindPullRequestVia builds the supervisor's FindPullRequest seam. It
// asks the forge of the job's owner, resolved server-side the way the
// launch resolves it, never with the sandbox's token, about one member's
// own repository on its own platform.
//
// @decision(DL-050) @decision(DL-018) @decision(DL-051)
func FindPullRequestVia(forgeFor ForgeFor, st store.Store) func(context.Context, store.Job, store.Deliverable) (*git.PullRequest, error) {
	return func(ctx context.Context, job store.Job, d store.Deliverable) (*git.PullRequest, error) {
		if forgeFor == nil {
			return nil, git.ErrUnsupported
		}
		sess, err := st.Sessions().Get(ctx, job.OrgID, job.SessionID)
		if err != nil {
			return nil, fmt.Errorf("reading the job's session: %w", err)
		}
		// The owner comes from the session row already read.
		//
		// @decision(DL-050)
		f, err := forgeFor(ctx, sess)
		if err != nil {
			return nil, fmt.Errorf("resolving the forge for the job's owner: %w", err)
		}
		if f == nil {
			return nil, git.ErrUnsupported
		}
		ref := store.RepoRef{Platform: d.Platform, Owner: d.Owner, Name: d.Name}
		sels, err := f.Selections(ctx, []store.RepoRef{ref})
		if err != nil {
			return nil, fmt.Errorf("resolving %s/%s: %w", ref.Owner, ref.Name, err)
		}
		for _, sel := range sels {
			if sel.Platform == nil || sel.Platform.Name() != d.Platform {
				continue
			}
			return sel.Platform.FindPullRequest(ctx,
				git.RepoRef{Owner: ref.Owner, Name: ref.Name}, d.Branch)
		}
		return nil, git.ErrUnsupported
	}
}
