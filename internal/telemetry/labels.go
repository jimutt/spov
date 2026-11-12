package telemetry

import (
	"slices"

	"github.com/jimutt/spov/internal/store"
)

// The closed label sets. A value outside its set is recorded as
// "other", so no caller can widen a label by passing something new.
//
// @decision(DL-011)

// QuotaLimit is which of the caller's limits refused a request.
type QuotaLimit string

const (
	QuotaSessions QuotaLimit = "sessions"
	QuotaJobs     QuotaLimit = "jobs"
	QuotaSize     QuotaLimit = "size"
	QuotaDuration QuotaLimit = "duration"
	// QuotaImageBuilds is a user's new image variants in a day (DL-134).
	QuotaImageBuilds QuotaLimit = "image_builds"
)

// RequestLimit is which of the server's request limits refused a
// request.
//
// @decision(DL-133)
type RequestLimit string

const (
	LimitSignInRate    RequestLimit = "sign_in_rate"
	LimitSignInCap     RequestLimit = "sign_in_cap"
	LimitDeviceCodeCap RequestLimit = "device_code_cap"
	// LimitSignInShare and LimitDeviceCodeShare are one network
	// holding its most places in a cap.
	LimitSignInShare     RequestLimit = "sign_in_share"
	LimitDeviceCodeShare RequestLimit = "device_code_share"
	LimitDevicePollRate  RequestLimit = "device_poll_rate"
	LimitRequests        RequestLimit = "requests"
	LimitCreates         RequestLimit = "creates"
	LimitStreams         RequestLimit = "streams"
	LimitInFlight        RequestLimit = "in_flight"
)

// EndedBy is what ended a job's run.
type EndedBy string

const (
	EndedDeclared         EndedBy = "declared"
	EndedWallClock        EndedBy = "wall-clock"
	EndedStopped          EndedBy = "stopped"
	EndedDenied           EndedBy = "denied"
	EndedNoCompletionTool EndedBy = "no-completion-tool"
	EndedContinueFailed   EndedBy = "continue-failed"
	// @decision(DL-048)
	EndedTurnFailed   EndedBy = "turn-failed"
	EndedAbandoned    EndedBy = "abandoned"
	EndedCancelled    EndedBy = "cancelled"
	EndedLaunchFailed EndedBy = "launch-failed"
	EndedOther        EndedBy = "other"
)

// SessionKind is whether a session is a person's or a job's.
type SessionKind string

const (
	KindInteractive SessionKind = "interactive"
	KindJob         SessionKind = "job"
)

// Termination is why a session stopped: someone or something asked
// (stopped), the sandbox ended on its own, at its idle timeout or its
// lifetime (ended), the backstop terminated it (reaped), it broke
// (failed), or a held sync job's sandbox was applied or discarded
// (released).
type Termination string

const (
	TerminationStopped Termination = "stopped"
	TerminationEnded   Termination = "ended"
	TerminationReaped  Termination = "reaped"
	TerminationFailed  Termination = "failed"
	// @decision(DL-101)
	TerminationReleased Termination = "released"
)

// LaunchStep is where a launch failed: one of the launcher's steps, or
// the setup before the first of them.
type LaunchStep string

const (
	StepSetup   LaunchStep = "setup"
	StepTokens  LaunchStep = "tokens"
	StepSandbox LaunchStep = "sandbox"
	StepServer  LaunchStep = "server"
)

// Outcome is a renewal's or a recovery's result.
type Outcome string

const (
	OutcomeRenewed   Outcome = "renewed"
	OutcomeSkipped   Outcome = "skipped"
	OutcomeRecreated Outcome = "recreated"
	OutcomeFailed    Outcome = "failed"
)

var (
	quotaLimits      = names(QuotaSessions, QuotaJobs, QuotaSize, QuotaDuration, QuotaImageBuilds)
	requestLimits    = names(LimitSignInRate, LimitSignInCap, LimitDeviceCodeCap, LimitSignInShare, LimitDeviceCodeShare, LimitDevicePollRate, LimitRequests, LimitCreates, LimitStreams, LimitInFlight)
	jobKinds         = []string{string(store.JobKindPR), string(store.JobKindReport), string(store.JobKindSync)}
	jobOutcomes      = []string{string(store.JobSucceeded), string(store.JobFailed), string(store.JobCancelled)}
	endedBys         = names(EndedDeclared, EndedWallClock, EndedStopped, EndedDenied, EndedNoCompletionTool, EndedContinueFailed, EndedTurnFailed, EndedAbandoned, EndedCancelled, EndedLaunchFailed)
	gates            = []string{"check", "deliverable", "conduct"}
	gateResults      = []string{"passed", "partial", "failed", "unverified"}
	deliverableKinds = []string{string(store.DeliverablePR), string(store.DeliverableReport), string(store.DeliverableSync)}
	deliverableStats = func() []string {
		out := make([]string, 0, len(store.DeliverableStatuses))
		for _, st := range store.DeliverableStatuses {
			out = append(out, string(st))
		}
		return out
	}()
	sessionKindNames = names(KindInteractive, KindJob)
	launchSteps      = names(StepSetup, StepTokens, StepSandbox, StepServer)
	terminations     = names(TerminationStopped, TerminationEnded, TerminationReaped, TerminationFailed, TerminationReleased)
	renewalOutcomes  = names(OutcomeRenewed, OutcomeSkipped, OutcomeFailed)
	recoveryOutcomes = names(OutcomeRecreated, OutcomeFailed)

	// Every state the job quota counts is an unfinished one.
	unfinishedJobStates = store.JobCounts
	liveSessionStates   = []store.SessionState{store.StateStarting, store.StateRunning}
	sessionKinds        = []SessionKind{KindInteractive, KindJob}
)

// names is a closed set as the strings it is checked against.
func names[T ~string](values ...T) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = string(v)
	}
	return out
}

// oneOf is v when the set lists it, and "other" when it does not.
func oneOf(v string, set []string) string {
	if slices.Contains(set, v) {
		return v
	}
	return "other"
}

// statusClass is an HTTP status as 2xx, 3xx, 4xx or 5xx.
func statusClass(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "2xx"
	case status >= 300 && status < 400:
		return "3xx"
	case status >= 400 && status < 500:
		return "4xx"
	case status >= 500 && status < 600:
		return "5xx"
	default:
		return "other"
	}
}

func okOrError(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}
