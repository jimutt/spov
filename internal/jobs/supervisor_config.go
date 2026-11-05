package jobs

import (
	"time"

	"github.com/jimutt/spov/internal/config"
)

// River's defaults are wrong for this and are set explicitly.
//
// @decision(DL-046)
const (
	// SupervisorJobTimeout bounds one attempt at supervising a job. An
	// attempt is one pass that snoozes to be redelivered, so this is the
	// launch budget plus a margin, not the job's whole life; a wedged
	// attempt frees its worker in minutes.
	//
	// @decision(DL-045)
	SupervisorJobTimeout = config.DefaultLaunchTimeout + 30*time.Minute

	// SupervisorRescueAfter is when River may reschedule an attempt it
	// believes died. River requires it to exceed JobTimeout — a smaller
	// value reschedules jobs that are merely still running — and the
	// margin here is the same one the timeout carries.
	SupervisorRescueAfter = SupervisorJobTimeout + time.Hour

	// SupervisorQueue is the queue jobs are worked on.
	SupervisorQueue = "spov_jobs"

	// SupervisorMaxAttempts is how many times a job's supervision is
	// retried after an infrastructure failure. Snoozes do not consume it.
	// River's default of 25 would keep re-provisioning sandboxes for a
	// job whose goal is what is broken.
	//
	// @decision(DL-045)
	SupervisorMaxAttempts = 3
)

// SupervisorMaxWorkers bounds how many jobs one replica supervises at
// once. Each holds a sandbox, so this is a spend ceiling as much as a
// concurrency one.
//
// @decision(DL-046)
const SupervisorMaxWorkers = 10

// DefaultWallClock is a job's wall clock when its request names none,
// clamped at create to the caller's quota and to what the sandbox will
// outlive. WallClockMargin is what the sandbox's timeout must cover
// beyond the clock: the launch, which runs before the clock starts, and
// the delivery after it. MinWallClock refuses a clock shorter than a few
// supervisor passes.
//
// @decision(DL-049)
const (
	DefaultWallClock = time.Hour
	WallClockMargin  = config.DefaultLaunchTimeout + 5*time.Minute
	MinWallClock     = time.Minute
)

// SteerHeadroom is how much longer than its clock and margin a job's
// sandbox lives, for the time people spend steering it, which the clock
// does not count.
//
// @decision(DL-049)
const SteerHeadroom = time.Hour
