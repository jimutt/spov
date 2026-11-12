package store

import (
	"errors"
	"slices"
	"testing"
	"time"
)

// The state machine is the one thing the supervisor and the job surface
// all read: DL-046 writes steered and blocked, DL-050 writes
// the verdict, DL-049 delivers at a wall clock. So it is asserted against
// directly, not only through the store contract.
//
// @decision(DL-045)
func TestJobMachineReachesEveryStateAndStopsAtTerminalOnes(t *testing.T) {
	// Every state must be reachable from queued, or it is a state the
	// machine cannot enter and the batch has a dead branch in it.
	seen := map[JobState]bool{JobQueued: true}
	frontier := []JobState{JobQueued}
	for len(frontier) > 0 {
		from := frontier[0]
		frontier = frontier[1:]
		for _, to := range jobTransitions[from] {
			if !seen[to] {
				seen[to] = true
				frontier = append(frontier, to)
			}
		}
	}
	for st := range jobStates {
		if !seen[st] {
			t.Errorf("%s is unreachable from queued", st)
		}
	}

	// A terminal state has no way out. If one grows an exit it has
	// stopped being terminal, and Terminal() would then be lying to
	// ListLive and to the supervisor's restart query.
	for st := range jobStates {
		if !st.Terminal() {
			continue
		}
		if len(jobTransitions[st]) != 0 {
			t.Errorf("%s is terminal but leads to %v", st, jobTransitions[st])
		}
		if st.Live() {
			t.Errorf("%s is terminal and live", st)
		}
	}

	// Every state named in the table must be one Validate accepts, or a
	// legal move writes a row the store then refuses.
	for from, tos := range jobTransitions {
		if _, ok := jobStates[from]; !ok {
			t.Errorf("the table moves from %q, which is not a state", from)
		}
		for _, to := range tos {
			if _, ok := jobStates[to]; !ok {
				t.Errorf("%s -> %q, which is not a state", from, to)
			}
		}
	}
}

// @decision(DL-045)
func TestJobSuspensionsAreNotTerminalAndReturnToRunning(t *testing.T) {
	// This is the distinction DL-045 draws three words around:
	// suspended (steered), parked (blocked), and neither is finished.
	for _, st := range []JobState{JobBlocked, JobSteered} {
		if st.Terminal() {
			t.Errorf("%s is terminal: a job waiting on a human has been thrown away", st)
		}
		if !st.Live() {
			t.Errorf("%s is not live: its sandbox is still up and still costing money", st)
		}
		if !CanTransition(st, JobRunning) {
			t.Errorf("%s cannot return to running, so releasing it is impossible", st)
		}
		if !CanTransition(st, JobCancelled) {
			t.Errorf("%s cannot be cancelled, so giving up on it is impossible", st)
		}
	}
	// Queued is not live: nothing has been provisioned for it yet, so
	// it costs nothing and holds nothing.
	if JobQueued.Live() {
		t.Error("a queued job is live, so a restart would go looking for its sandbox")
	}
}

// @decision(DL-045)
func TestCanTransitionAllowsTheSameStateTwice(t *testing.T) {
	// The supervisor writes progress as well as state; re-recording
	// running while still running is not a transition.
	if !CanTransition(JobRunning, JobRunning) {
		t.Error("a job cannot be updated without changing state")
	}
	// But a terminal state repeating itself is still terminal, not a
	// way back in.
	if CanTransition(JobSucceeded, JobRunning) {
		t.Error("a succeeded job can be restarted")
	}
}

// @decision(DL-045)
func TestJobValidate(t *testing.T) {
	ok := Job{ID: "j", OrgID: "o", SessionID: "s", Kind: JobKindPR, State: JobQueued, Goal: "do the thing"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("a valid job = %v", err)
	}
	bad := map[string]Job{
		"no id":         {OrgID: "o", SessionID: "s", Kind: JobKindPR, State: JobQueued, Goal: "g"},
		"no org":        {ID: "j", SessionID: "s", Kind: JobKindPR, State: JobQueued, Goal: "g"},
		"no session":    {ID: "j", OrgID: "o", Kind: JobKindPR, State: JobQueued, Goal: "g"},
		"blank goal":    {ID: "j", OrgID: "o", SessionID: "s", Kind: JobKindPR, State: JobQueued, Goal: " \n\t"},
		"unknown kind":  {ID: "j", OrgID: "o", SessionID: "s", Kind: "sculpture", State: JobQueued, Goal: "g"},
		"unknown state": {ID: "j", OrgID: "o", SessionID: "s", Kind: JobKindPR, State: "loitering", Goal: "g"},
		// @decision(DL-137)
		"unknown pr policy":  {ID: "j", OrgID: "o", SessionID: "s", Kind: JobKindPR, State: JobQueued, Goal: "g", PRPolicy: "theirs"},
		"report pr policy":   {ID: "j", OrgID: "o", SessionID: "s", Kind: JobKindReport, State: JobQueued, Goal: "g", PRPolicy: PRPolicyGoal},
		"sync job pr policy": {ID: "j", OrgID: "o", SessionID: "s", Kind: JobKindSync, State: JobQueued, Goal: "g", PRPolicy: PRPolicyGoal},
	}
	for _, p := range []PRPolicy{"", PRPolicySpov, PRPolicyGoal} {
		withPolicy := ok
		withPolicy.PRPolicy = p
		if err := withPolicy.Validate(); err != nil {
			t.Errorf("a pr job with policy %q = %v", p, err)
		}
	}
	for name, j := range bad {
		t.Run(name, func(t *testing.T) {
			if err := j.Validate(); !errors.Is(err, ErrInvalidJob) {
				t.Errorf("= %v, want ErrInvalidJob", err)
			}
		})
	}
}

// @decision(DL-045)
func TestStampJobTimesIsWriteOnce(t *testing.T) {
	first := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	later := first.Add(time.Hour)

	// Reaching running stamps StartedAt.
	next := Job{State: JobRunning}
	StampJobTimes(&next, Job{State: JobProvisioning}, first)
	if next.StartedAt == nil || !next.StartedAt.Equal(first) {
		t.Fatalf("StartedAt = %v", next.StartedAt)
	}
	// Coming back from a park keeps the original.
	again := Job{State: JobRunning}
	StampJobTimes(&again, next, later)
	if !again.StartedAt.Equal(first) {
		t.Errorf("StartedAt moved to %v on a re-entry to running", again.StartedAt)
	}
	if again.StoppedAt != nil {
		t.Errorf("a running job is stopped at %v", again.StoppedAt)
	}
	// A terminal state stamps StoppedAt.
	done := Job{State: JobSucceeded}
	StampJobTimes(&done, again, later)
	if done.StoppedAt == nil || !done.StoppedAt.Equal(later) {
		t.Fatalf("StoppedAt = %v", done.StoppedAt)
	}
	// A suspension does not.
	parked := Job{State: JobBlocked}
	StampJobTimes(&parked, again, later)
	if parked.StoppedAt != nil {
		t.Errorf("a blocked job is stopped at %v: it is parked, not finished", parked.StoppedAt)
	}
}

// JobCounts is Counts written out as a list, so a query can name the
// states. Two copies of the same rule drift; this is what stops them.
//
// The test is written against jobStates rather than against JobCounts,
// so a state added to the machine and left out of both is caught here
// rather than silently escaping the quota.
//
// @decision(DL-045)
func TestJobCountsAgreesWithTheCountsMethod(t *testing.T) {
	inList := map[JobState]bool{}
	for _, st := range JobCounts {
		if inList[st] {
			t.Errorf("JobCounts lists %s twice, which would double-count it", st)
		}
		inList[st] = true
	}
	for st := range jobStates {
		if st.Counts() != inList[st] {
			t.Errorf("%s: Counts() = %v but JobCounts membership = %v",
				st, st.Counts(), inList[st])
		}
	}
	// The difference from Live that the quota depends on: a queued job
	// holds no sandbox but still occupies a slot.
	if !JobQueued.Counts() {
		t.Error("a queued job must count against the quota, or a caller can queue past it")
	}
	if JobQueued.Live() {
		t.Error("this test no longer distinguishes Counts from Live")
	}
}

// JobStates is every state the machine has.
//
// It exists so a rule that must decide something for EVERY state can be
// checked against the machine rather than against a list someone has to
// remember to extend — DL-052's refusals are the first. A JobStates
// that fell behind would make such a rule vacuous rather than wrong,
// which is the failure that does not announce itself.
//
// @decision(DL-045) @decision(DL-052)
func TestJobStatesIsEveryStateTheMachineHas(t *testing.T) {
	all := JobStates()
	if len(all) == 0 {
		t.Fatal("no states at all: every exhaustiveness check built on this is vacuous")
	}
	for _, s := range all {
		if !ValidJobState(s) {
			t.Errorf("JobStates lists %q, which the machine does not have", s)
		}
	}
	// The other direction, against jobStates itself: the two are the
	// same fact, and this is what says the accessor did not lose one.
	if len(all) != len(jobStates) {
		t.Errorf("JobStates has %d entries and the machine has %d", len(all), len(jobStates))
	}
	for s := range jobStates {
		if !slices.Contains(all, s) {
			t.Errorf("JobStates omits %q, so a rule checked against it never sees that state", s)
		}
	}
	// Sorted, so a caller iterating it behaves the same way twice. A
	// map's order is not stable and a test that walked it would be
	// flaky rather than wrong.
	if !slices.IsSorted(all) {
		t.Errorf("JobStates is not sorted: %v", all)
	}
}

// The wall clock is one ledger across the job: running and blocked
// count, steering pauses it, a resume continues with what is left, and a
// caller cannot set either field.
//
// @decision(DL-049)
func TestTheWallClockIsOneAllowanceAcrossTheJob(t *testing.T) {
	t1 := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t1.Add(time.Duration(m) * time.Minute) }
	cur := Job{State: JobProvisioning}
	step := func(to JobState, minute int) {
		t.Helper()
		next := Job{State: to}
		StampJobTimes(&next, cur, at(minute))
		cur = next
	}

	step(JobRunning, 0)
	if cur.ClockStartedAt == nil || !cur.ClockStartedAt.Equal(t1) || cur.ClockUsed != 0 {
		t.Fatalf("first running: clock %v, used %s", cur.ClockStartedAt, cur.ClockUsed)
	}
	step(JobBlocked, 20)
	step(JobRunning, 30)
	if !cur.ClockStartedAt.Equal(t1) || cur.ClockUsed != 0 {
		t.Errorf("blocked and back moved the clock: %v, used %s", cur.ClockStartedAt, cur.ClockUsed)
	}
	if got := cur.ClockElapsed(at(40)); got != 40*time.Minute {
		t.Errorf("elapsed after 40m of running and blocked = %s", got)
	}
	step(JobSteered, 40)
	if cur.ClockStartedAt != nil || cur.ClockUsed != 40*time.Minute {
		t.Errorf("steering: clock %v, used %s; want stopped at 40m", cur.ClockStartedAt, cur.ClockUsed)
	}
	if got := cur.ClockElapsed(at(100)); got != 40*time.Minute {
		t.Errorf("an hour of steering counted: elapsed %s", got)
	}
	step(JobRunning, 100)
	if got := cur.ClockElapsed(at(110)); got != 50*time.Minute {
		t.Errorf("a resume did not continue the clock: elapsed %s, want 50m", got)
	}
	if !cur.StartedAt.Equal(t1) {
		t.Errorf("StartedAt moved to %v on a resume", cur.StartedAt)
	}
	step(JobDelivering, 110)
	if cur.ClockStartedAt != nil || cur.ClockUsed != 50*time.Minute {
		t.Errorf("delivering: clock %v, used %s; want stopped at 50m", cur.ClockStartedAt, cur.ClockUsed)
	}

	// A caller cannot set the clock: the stored values win.
	forged := Job{State: JobDelivering, ClockStartedAt: &t1, ClockUsed: time.Minute}
	StampJobTimes(&forged, cur, at(111))
	if forged.ClockStartedAt != nil || forged.ClockUsed != 50*time.Minute {
		t.Errorf("a caller's clock was kept: %v, %s", forged.ClockStartedAt, forged.ClockUsed)
	}
}

// LiveStates is SessionState.Live as a list.
//
// @decision(DL-012)
func TestLiveStatesAgreesWithTheLiveMethod(t *testing.T) {
	for _, st := range []SessionState{StateStarting, StateRunning, StateStopped, StateFailed} {
		if st.Live() != slices.Contains(LiveStates, st) {
			t.Errorf("%s: Live() = %v but LiveStates membership = %v", st, st.Live(), slices.Contains(LiveStates, st))
		}
	}
}
