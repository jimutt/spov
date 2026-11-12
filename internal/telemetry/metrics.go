// Package telemetry is spov's control-plane metrics: the OpenTelemetry
// instruments, their closed label sets, and the exporters that publish
// them. It is the only package that imports the OpenTelemetry SDK and
// exporters; everything else records through *Metrics.
//
// @decision(DL-011)
package telemetry

import (
	"context"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/jimutt/spov/internal/store"
)

// Metrics records spov's metrics. A nil *Metrics records nothing, which
// is what a server with no exporter configured, and every test that does
// not ask for metrics, gets.
//
// Every method takes its labels as closed sets: a typed value from this
// package or from store, normalised to "other" when it is not one the
// catalogue lists. No method takes a free string label.
//
// @decision(DL-011)
type Metrics struct {
	apiRequests   metric.Int64Counter
	apiDuration   metric.Float64Histogram
	quotaRefusals metric.Int64Counter
	// @decision(DL-133)
	limited          metric.Int64Counter
	jobEndings       metric.Int64Counter
	jobQueueDelay    metric.Float64Histogram
	jobClockUsed     metric.Float64Histogram
	jobDelivery      metric.Float64Histogram
	jobGates         metric.Int64Counter
	deliverables     metric.Int64Counter
	launches         metric.Int64Counter
	launchDuration   metric.Float64Histogram
	sessionLifetime  metric.Float64Histogram
	terminations     metric.Int64Counter
	reconcileRuns    metric.Int64Counter
	reconcileTime    metric.Float64Histogram
	supervisorPasses metric.Int64Counter
	recorderGaps     metric.Int64Counter
	recorderRetries  metric.Int64Counter
	renewals         metric.Int64Counter
	proxyRecoveries  metric.Int64Counter
	// @decision(DL-085) @decision(DL-086)
	snapshotCollections metric.Int64Counter
	snapshotPrunes      metric.Int64Counter
	imageBuilds         metric.Int64Counter

	// shared is the leader's latest snapshot of shared state, nil when
	// this process does not lead (DL-011).
	shared atomic.Pointer[Snapshot]
}

// Snapshot is shared state as the replica holding the reconciler lock
// last read it. Only unfinished jobs and live sessions are counted.
//
// @decision(DL-011)
type Snapshot struct {
	Jobs     map[store.JobState]int64
	Sessions map[SessionKey]int64
	// OldestQueued is how long the oldest queued job has waited; zero
	// when none is queued.
	OldestQueued time.Duration
}

// SessionKey is a session gauge's labels.
type SessionKey struct {
	State store.SessionState
	Kind  SessionKind
}

// Bucket boundaries, in seconds, for each histogram's range.
var (
	requestBuckets  = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}
	launchBuckets   = []float64{5, 10, 20, 30, 45, 60, 90, 120, 180, 300, 600}
	waitBuckets     = []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600, 7200}
	clockBuckets    = []float64{60, 300, 600, 1200, 1800, 3600, 5400, 7200, 10800, 14400}
	deliverBuckets  = []float64{1, 5, 10, 30, 60, 120, 300, 600}
	lifetimeBuckets = []float64{60, 300, 900, 1800, 3600, 7200, 14400, 28800, 43200}
	passBuckets     = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}
)

// NewMetrics creates every instrument on meter. version is the build,
// the one label of spov_build_info. Start calls it; a test reads the
// instruments back through telemetrytest.
//
// @decision(DL-011)
func NewMetrics(meter metric.Meter, version string) (*Metrics, error) {
	m := &Metrics{}
	b := builder{meter: meter}
	m.apiRequests = b.counter("spov_api_requests", "{request}", "API requests served, by operation and status class")
	m.apiDuration = b.histogram("spov_api_request_duration", "API request duration, by operation", requestBuckets)
	m.quotaRefusals = b.counter("spov_quota_refusals", "{refusal}", "Requests refused by the caller's own limits")
	m.limited = b.counter("spov_requests_limited", "{request}", "Requests refused by the server's rate limits and caps, by limit")
	m.jobEndings = b.counter("spov_job_endings", "{job}", "Jobs that reached a terminal state, by kind, outcome and what ended them")
	m.jobQueueDelay = b.histogram("spov_job_queue_delay", "Time from a job's creation to its provisioning", waitBuckets)
	m.jobClockUsed = b.histogram("spov_job_clock_used", "Wall clock a job used, recorded at its ending", clockBuckets)
	m.jobDelivery = b.histogram("spov_job_delivery_duration", "Time a job spent delivering", deliverBuckets)
	m.jobGates = b.counter("spov_job_gates", "{gate}", "Verdict gate results")
	m.deliverables = b.counter("spov_job_deliverables", "{deliverable}", "A judged job's deliverables, by kind and status")
	m.launches = b.counter("spov_session_launches", "{launch}", "Session launches, by kind, outcome and failed step")
	m.launchDuration = b.histogram("spov_session_launch_duration", "Session launch duration, by kind and outcome", launchBuckets)
	m.sessionLifetime = b.histogram("spov_session_lifetime", "A session's lifetime, recorded when it stops", lifetimeBuckets)
	m.terminations = b.counter("spov_session_terminations", "{session}", "Sessions that stopped, by reason")
	m.reconcileRuns = b.counter("spov_reconcile_runs", "{run}", "Reconciler passes, by outcome")
	m.reconcileTime = b.histogram("spov_reconcile_duration", "Reconciler pass duration", passBuckets)
	m.supervisorPasses = b.counter("spov_supervisor_passes", "{pass}", "Supervisor passes over a job, by outcome")
	m.recorderGaps = b.counter("spov_recorder_gaps", "{gap}", "Gaps the recorder marked in a session's log")
	m.recorderRetries = b.counter("spov_recorder_reconnects", "{reconnect}", "Times the recorder reattached to a session's stream")
	m.renewals = b.counter("spov_git_token_renewals", "{renewal}", "Git token re-mints, by outcome")
	m.proxyRecoveries = b.counter("spov_proxy_recoveries", "{recovery}", "Credential proxy sidecar recoveries, by outcome")
	m.snapshotCollections = b.counter("spov_snapshot_collections", "{snapshot}", "Exit snapshots collected after a run ended, by outcome")
	m.snapshotPrunes = b.counter("spov_snapshot_prunes", "{snapshot}", "Exit snapshots pruned, by reason")
	m.imageBuilds = b.counter("spov_image_builds", "{build}", "Sandbox image builds, by outcome")
	b.gauge("spov_build_info", "{build}", "The running build, always 1", func(_ context.Context, o metric.Int64Observer) error {
		o.Observe(1, metric.WithAttributes(attribute.String("version", version)))
		return nil
	})
	b.gauge("spov_jobs", "{job}", "Unfinished jobs by state; reported only by the reconciler lock holder",
		func(_ context.Context, o metric.Int64Observer) error {
			if s := m.shared.Load(); s != nil {
				for _, st := range unfinishedJobStates {
					o.Observe(s.Jobs[st], metric.WithAttributes(attribute.String("state", string(st))))
				}
			}
			return nil
		})
	b.gauge("spov_sessions", "{session}", "Live sessions by state and kind; reported only by the reconciler lock holder",
		func(_ context.Context, o metric.Int64Observer) error {
			if s := m.shared.Load(); s != nil {
				for _, st := range liveSessionStates {
					for _, k := range sessionKinds {
						o.Observe(s.Sessions[SessionKey{State: st, Kind: k}],
							metric.WithAttributes(attribute.String("state", string(st)), attribute.String("kind", string(k))))
					}
				}
			}
			return nil
		})
	b.floatGauge("spov_job_oldest_queued_age", "s", "How long the oldest queued job has waited; reported only by the reconciler lock holder",
		func(_ context.Context, o metric.Float64Observer) error {
			if s := m.shared.Load(); s != nil {
				o.Observe(s.OldestQueued.Seconds())
			}
			return nil
		})
	return m, b.err
}

// builder creates instruments and keeps the first error.
type builder struct {
	meter metric.Meter
	err   error
}

func (b *builder) keep(err error) {
	if b.err == nil {
		b.err = err
	}
}

func (b *builder) counter(name, unit, desc string) metric.Int64Counter {
	c, err := b.meter.Int64Counter(name, metric.WithUnit(unit), metric.WithDescription(desc))
	b.keep(err)
	return c
}

func (b *builder) histogram(name, desc string, buckets []float64) metric.Float64Histogram {
	h, err := b.meter.Float64Histogram(name, metric.WithUnit("s"), metric.WithDescription(desc),
		metric.WithExplicitBucketBoundaries(buckets...))
	b.keep(err)
	return h
}

func (b *builder) gauge(name, unit, desc string, fn metric.Int64Callback) {
	_, err := b.meter.Int64ObservableGauge(name, metric.WithUnit(unit), metric.WithDescription(desc), metric.WithInt64Callback(fn))
	b.keep(err)
}

func (b *builder) floatGauge(name, unit, desc string, fn metric.Float64Callback) {
	_, err := b.meter.Float64ObservableGauge(name, metric.WithUnit(unit), metric.WithDescription(desc), metric.WithFloat64Callback(fn))
	b.keep(err)
}

// SetShared publishes the leader's snapshot; ClearShared withdraws it
// when this process stops leading.
//
// @decision(DL-011)
func (m *Metrics) SetShared(s Snapshot) {
	if m != nil {
		m.shared.Store(&s)
	}
}

// ClearShared stops this process reporting shared state.
//
// @decision(DL-011)
func (m *Metrics) ClearShared() {
	if m != nil {
		m.shared.Store(nil)
	}
}

// APIRequest records one served API operation. operation is the huma
// operation ID, a closed set the server's own registrations define. A
// negative took counts the request without timing it.
//
// @decision(DL-011)
func (m *Metrics) APIRequest(ctx context.Context, operation string, status int, took time.Duration) {
	if m == nil {
		return
	}
	op := attribute.String("operation", operation)
	m.apiRequests.Add(ctx, 1, metric.WithAttributes(op, attribute.String("status_class", statusClass(status))))
	// A stream passes a negative duration: it is counted, not timed.
	if took >= 0 {
		m.apiDuration.Record(ctx, took.Seconds(), metric.WithAttributes(op))
	}
}

// QuotaRefused records a request refused by the caller's limits.
func (m *Metrics) QuotaRefused(ctx context.Context, limit QuotaLimit) {
	if m == nil {
		return
	}
	m.quotaRefusals.Add(ctx, 1, metric.WithAttributes(attribute.String("limit", oneOf(string(limit), quotaLimits))))
}

// RequestLimited records a request one of the server's request limits
// refused.
//
// @decision(DL-133)
func (m *Metrics) RequestLimited(ctx context.Context, limit RequestLimit) {
	if m == nil {
		return
	}
	m.limited.Add(ctx, 1, metric.WithAttributes(attribute.String("limit", oneOf(string(limit), requestLimits))))
}

// JobEnded records a job's terminal state, once, where the store took
// the move. clockUsed is the wall clock the job used.
//
// @decision(DL-011)
func (m *Metrics) JobEnded(ctx context.Context, kind store.JobKind, outcome store.JobState, endedBy EndedBy, clockUsed time.Duration) {
	if m == nil {
		return
	}
	m.jobEndings.Add(ctx, 1, metric.WithAttributes(
		attribute.String("kind", oneOf(string(kind), jobKinds)),
		attribute.String("outcome", oneOf(string(outcome), jobOutcomes)),
		attribute.String("ended_by", oneOf(string(endedBy), endedBys)),
	))
	if clockUsed > 0 {
		m.jobClockUsed.Record(ctx, clockUsed.Seconds(), metric.WithAttributes(attribute.String("kind", oneOf(string(kind), jobKinds))))
	}
}

// JobQueued records how long a job waited before provisioning.
func (m *Metrics) JobQueued(ctx context.Context, waited time.Duration) {
	if m == nil || waited < 0 {
		return
	}
	m.jobQueueDelay.Record(ctx, waited.Seconds())
}

// JobDelivered records how long a job spent delivering.
func (m *Metrics) JobDelivered(ctx context.Context, took time.Duration) {
	if m == nil || took < 0 {
		return
	}
	m.jobDelivery.Record(ctx, took.Seconds())
}

// JobGate records one verdict gate's result.
func (m *Metrics) JobGate(ctx context.Context, gate, result string) {
	if m == nil {
		return
	}
	m.jobGates.Add(ctx, 1, metric.WithAttributes(
		attribute.String("gate", oneOf(gate, gates)),
		attribute.String("result", oneOf(result, gateResults)),
	))
}

// JobDeliverable records one member of a judged job's inventory.
//
// @decision(DL-050)
func (m *Metrics) JobDeliverable(ctx context.Context, kind store.DeliverableKind, status store.DeliverableStatus) {
	if m == nil {
		return
	}
	m.deliverables.Add(ctx, 1, metric.WithAttributes(
		attribute.String("kind", oneOf(string(kind), deliverableKinds)),
		attribute.String("status", oneOf(string(status), deliverableStats)),
	))
}

// SessionLaunched records a launch's end. failedStep is where it failed,
// ignored on success.
func (m *Metrics) SessionLaunched(ctx context.Context, kind SessionKind, ok bool, failedStep LaunchStep, took time.Duration) {
	if m == nil {
		return
	}
	outcome, step := "succeeded", "none"
	if !ok {
		outcome, step = "failed", oneOf(string(failedStep), launchSteps)
	}
	k := attribute.String("kind", oneOf(string(kind), sessionKindNames))
	o := attribute.String("outcome", outcome)
	m.launches.Add(ctx, 1, metric.WithAttributes(k, o, attribute.String("failed_step", step)))
	m.launchDuration.Record(ctx, took.Seconds(), metric.WithAttributes(k, o))
}

// SessionStopped records a session's end and its lifetime.
func (m *Metrics) SessionStopped(ctx context.Context, kind SessionKind, reason Termination, lifetime time.Duration) {
	if m == nil {
		return
	}
	m.terminations.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", oneOf(string(reason), terminations))))
	if lifetime > 0 {
		m.sessionLifetime.Record(ctx, lifetime.Seconds(), metric.WithAttributes(attribute.String("kind", oneOf(string(kind), sessionKindNames))))
	}
}

// Reconciled records one reconciler pass.
func (m *Metrics) Reconciled(ctx context.Context, err error, took time.Duration) {
	if m == nil {
		return
	}
	m.reconcileRuns.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", okOrError(err))))
	m.reconcileTime.Record(ctx, took.Seconds())
}

// SupervisorPass records one supervisor pass over a job.
func (m *Metrics) SupervisorPass(ctx context.Context, err error) {
	if m == nil {
		return
	}
	m.supervisorPasses.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", okOrError(err))))
}

// RecorderGap records a gap marked in a session's log.
func (m *Metrics) RecorderGap(ctx context.Context) {
	if m != nil {
		m.recorderGaps.Add(ctx, 1)
	}
}

// RecorderReconnect records the recorder reattaching to a stream.
func (m *Metrics) RecorderReconnect(ctx context.Context) {
	if m != nil {
		m.recorderRetries.Add(ctx, 1)
	}
}

// TokenRenewal records one git token re-mint attempt.
func (m *Metrics) TokenRenewal(ctx context.Context, outcome Outcome) {
	if m != nil {
		m.renewals.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", oneOf(string(outcome), renewalOutcomes))))
	}
}

// ProxyRecovery records a sidecar recovery's outcome.
func (m *Metrics) ProxyRecovery(ctx context.Context, outcome Outcome) {
	if m != nil {
		m.proxyRecoveries.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", oneOf(string(outcome), recoveryOutcomes))))
	}
}

// snapshotOutcomes and pruneReasons are the closed label sets of the
// snapshot counters.
//
// @decision(DL-085) @decision(DL-086)
var (
	snapshotOutcomes = []string{"kept", "failed", "none"}
	pruneReasons     = []string{"retention", "cap", "superseded", "unused"}
)

// SnapshotCollected records one run's exit snapshot settling.
func (m *Metrics) SnapshotCollected(ctx context.Context, outcome string) {
	if m != nil {
		m.snapshotCollections.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", oneOf(outcome, snapshotOutcomes))))
	}
}

// SnapshotPruned records one exit snapshot marked deleted.
func (m *Metrics) SnapshotPruned(ctx context.Context, reason string) {
	if m != nil {
		m.snapshotPrunes.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", oneOf(reason, pruneReasons))))
	}
}

// ImageBuilt records one image build's outcome.
func (m *Metrics) ImageBuilt(ctx context.Context, err error) {
	if m != nil {
		m.imageBuilds.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", okOrError(err))))
	}
}
