package server

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/telemetry"
)

// ReconcileInterval is how often the store is compared with the provider.
//
// @decision(DL-004)
const ReconcileInterval = 30 * time.Second

// Reconciler keeps the store's view of sessions in step with what the
// provider actually runs, so listings never need a live provider call.
//
// @decision(DL-004)
type Reconciler struct {
	Store    store.Store
	Provider sandbox.Provider
	// Now supplies timestamps; nil means time.Now.
	Now func() time.Time
	// Warn receives one line per problem; nil discards.
	Warn func(string)
	// DefaultOrg and DefaultOwner are given to sandboxes the store does
	// not know. In local mode both are the local principal.
	DefaultOrg, DefaultOwner string
	// Forget is called with the id of every session the reconciler
	// stops, so the server can drop what it cached for it.
	//
	// @decision(DL-030)
	Forget func(sessionID string)
	// LaunchTimeout bounds a launch; a starting session older than
	// LaunchTimeout plus a minute belonged to a replica that died.
	// Zero means config.DefaultLaunchTimeout.
	//
	// @decision(DL-004)
	LaunchTimeout time.Duration
	// MaxSessionAge is the longest a session may legitimately run — the
	// org's timeout quota. A sandbox older than this plus reapGrace is
	// terminated whatever the store says. Zero disables the backstop.
	//
	// @decision(DL-004)
	MaxSessionAge time.Duration
	// Metrics records each pass and the sessions it stops; nil records
	// nothing.
	//
	// @decision(DL-011)
	Metrics *telemetry.Metrics
}

// orphanGrace is added to LaunchTimeout before a starting session is
// treated as abandoned, so a launch that is merely slow is left alone.
//
// @decision(DL-004)
const orphanGrace = time.Minute

func (r *Reconciler) launchTimeout() time.Duration {
	if r.LaunchTimeout > 0 {
		return r.LaunchTimeout
	}
	return config.DefaultLaunchTimeout
}

func (r *Reconciler) forget(id string) {
	if r.Forget != nil {
		r.Forget(id)
	}
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reconciler) warn(format string, args ...any) {
	if r.Warn != nil {
		r.Warn(fmt.Sprintf(format, args...))
	}
}

// Run reconciles once immediately, then every ReconcileInterval with
// jitter, until ctx ends.
func (r *Reconciler) Run(ctx context.Context) {
	for {
		start := r.now()
		err := r.Once(ctx)
		if err != nil && ctx.Err() == nil {
			r.warn("reconcile: %v", err)
		}
		// @decision(DL-011)
		if ctx.Err() == nil {
			r.Metrics.Reconciled(ctx, err, r.now().Sub(start))
		}
		wait := ReconcileInterval + rand.N(ReconcileInterval/5)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// Once performs one reconciliation:
//
//   - a live session whose sandbox the provider no longer reports is
//     marked stopped;
//   - a running sandbox the store does not know is adopted as a running
//     session owned by the defaults;
//   - a session left starting by a previous server is marked failed
//     unless its sandbox is running, in which case it becomes running.
func (r *Reconciler) Once(ctx context.Context) error {
	infos, err := r.Provider.List(ctx, map[string]string{sandbox.TagSpov: "1"})
	if err != nil {
		return err
	}
	seen := map[string]sandbox.Info{}
	for _, info := range infos {
		seen[info.ID] = info
	}
	now := r.now()

	live, err := r.Store.Sessions().ListLive(ctx)
	if err != nil {
		return err
	}
	jobs, err := r.liveJobs(ctx)
	if err != nil {
		return err
	}
	for _, sess := range live {
		// A queued job's session is starting with no sandbox while it
		// waits on the queue; do not sweep it as an orphan.
		//
		// @decision(DL-004)
		if jobs.queuedSessions[sess.ID] {
			continue
		}
		info, running := seen[sess.SandboxID]
		running = running && info.State == sandbox.StateRunning
		// A launch is in flight until LaunchTimeout plus orphanGrace has
		// passed; after that the launching replica is gone, whichever it
		// was, and the row is classified below.
		//
		// @decision(DL-004)
		starting := sess.State == store.StateStarting
		orphaned := starting && now.Sub(sess.CreatedAt) > r.launchTimeout()+orphanGrace
		// A stop goes through MarkStopped, so a session Delete stopped
		// in the meantime is neither overwritten nor counted twice.
		//
		// @decision(DL-011)
		var (
			to      store.SessionState
			errText string
			reason  telemetry.Termination
		)
		switch {
		case running && sess.State == store.StateStarting && sess.SandboxID != "":
			sess.State = store.StateRunning
			sess.Error = ""
			if err := r.Store.Sessions().Update(ctx, sess); err != nil {
				r.warn("reconcile: update %s: %v", sess.ID, err)
			}
			continue
		case running:
			continue
		case starting && !orphaned:
			// Still within its launch budget; nothing to conclude yet.
			continue
		case orphaned && sess.SandboxID != "":
			to, errText, reason = store.StateFailed, "launch orphaned", telemetry.TerminationFailed
		case orphaned:
			to, errText, reason = store.StateFailed, "the server stopped while the session was starting", telemetry.TerminationFailed
		default:
			to, errText, reason = store.StateStopped, sess.Error, telemetry.TerminationEnded
		}
		r.forget(sess.ID)
		took, err := r.Store.Sessions().MarkStopped(ctx, sess.OrgID, sess.ID, to, now, errText)
		if err != nil {
			r.warn("reconcile: update %s: %v", sess.ID, err)
		} else if took {
			recordStop(ctx, r.Metrics, r.Store, sess, now, reason)
		}
	}

	for id, info := range seen {
		if info.State != sandbox.StateRunning {
			continue
		}
		// The age backstop runs first and ignores the store, which is
		// what may have lost track of the sandbox.
		//
		// @decision(DL-004)
		if r.reap(ctx, info, now) {
			continue
		}
		known, err := r.Store.Sessions().BySandbox(ctx, id)
		switch {
		case err == nil && known.State.Live():
			continue
		case err == nil:
			// Marked stopped while the provider still runs it — a
			// transient listing miss. Bring it back rather than leave a
			// billing sandbox invisible.
			known.State = store.StateRunning
			known.Error = ""
			known.StoppedAt = nil
			if err := r.Store.Sessions().Update(ctx, known); err != nil {
				r.warn("reconcile: resurrect %s: %v", known.ID, err)
			}
			continue
		case !errors.Is(err, store.ErrNotFound):
			return err
		}
		// A sandbox tagged with a job that still counts belongs to that
		// job, whose session is invisible to the name check below. An
		// untagged sandbox reads as "", which is never a job id.
		//
		// @decision(DL-004)
		if job := info.Tags[sandbox.TagJob]; job != "" && jobs.counting[job] {
			continue
		}
		// A launch in flight has created its sandbox but not recorded its
		// ID yet: its session is starting under the sandbox's name. Leave
		// the sandbox to that launch.
		//
		// @decision(DL-004)
		if r.launchInFlight(ctx, r.orgOf(info), info.Name) {
			continue
		}
		if err := r.adopt(ctx, info, now); err != nil {
			r.warn("reconcile: adopt %s: %v", id, err)
		}
	}
	return nil
}

// reapGrace is added to the org's maximum session lifetime before a
// sandbox is terminated regardless of the store. It is generous on
// purpose: reaping a session still in use costs more than an extra hour.
//
// @decision(DL-004)
const reapGrace = time.Hour

// liveJobs is what the reconciler needs to know about jobs in flight,
// built from one Jobs().ListLive call per cycle.
//
// @decision(DL-004)
type liveJobs struct {
	// queuedSessions are the sessions of jobs not yet provisioned.
	queuedSessions map[string]bool
	// counting are the ids of jobs that still count against their
	// owner's quota; a sandbox tagged with one is not an orphan.
	counting map[string]bool
}

func (r *Reconciler) liveJobs(ctx context.Context) (liveJobs, error) {
	jobs, err := r.Store.Jobs().ListLive(ctx)
	if err != nil {
		return liveJobs{}, err
	}
	out := liveJobs{queuedSessions: map[string]bool{}, counting: map[string]bool{}}
	for _, j := range jobs {
		if j.State == store.JobQueued {
			out.queuedSessions[j.SessionID] = true
		}
		// Counts(), the quota's predicate, rather than Live().
		//
		// @decision(DL-004)
		if j.State.Counts() {
			out.counting[j.ID] = true
		}
	}
	return out, nil
}

// maxAge is the age past which a sandbox is reaped. Zero disables the
// backstop, which is what a reconciler built without a quota gets.
func (r *Reconciler) maxAge() time.Duration {
	if r.MaxSessionAge <= 0 {
		return 0
	}
	return r.MaxSessionAge + reapGrace
}

// reap terminates a sandbox that has outlived any session it could
// belong to, and reports whether it did. The store is not consulted
// before terminating: a sandbox that old is over-age whatever it says.
//
// @decision(DL-004) @decision(DL-029)
func (r *Reconciler) reap(ctx context.Context, info sandbox.Info, now time.Time) bool {
	max := r.maxAge()
	if max <= 0 || info.CreatedAt.IsZero() {
		return false
	}
	age := now.Sub(info.CreatedAt)
	if age < max {
		return false
	}
	h, err := r.Provider.Get(ctx, info.ID)
	if err != nil {
		if !errors.Is(err, sandbox.ErrNotFound) {
			r.warn("reconcile: reap %s: %v", info.ID, err)
		}
		return false
	}
	if err := h.Terminate(ctx); err != nil {
		r.warn("reconcile: reap %s: %v", info.ID, err)
		return false
	}
	r.warn("reconcile: reaped %s after %s, past the %s maximum",
		info.ID, age.Round(time.Minute), max)
	// A session that named it is now stopped, whatever it said before.
	if sess, err := r.Store.Sessions().BySandbox(ctx, info.ID); err == nil && sess.State.Live() {
		// @decision(DL-011)
		took, err := r.Store.Sessions().MarkStopped(ctx, sess.OrgID, sess.ID, store.StateStopped, now,
			"reaped: the sandbox outlived the maximum session age")
		if err != nil {
			r.warn("reconcile: reap %s: record stop: %v", info.ID, err)
		} else if took {
			recordStop(ctx, r.Metrics, r.Store, sess, now, telemetry.TerminationReaped)
		}
		r.forget(sess.ID)
	}
	return true
}

// ownerFromTag reports whether a tag value may be used as a user id: a
// ULID or the local principal. A sandbox tagged with an OS
// username is not.
//
// @decision(DL-004)
func ownerFromTag(v string) bool {
	return v == auth.LocalID || store.ValidID(v)
}

// orgFromTag reports whether a tag value may be used as an org id: one
// this reconciler serves. Org ids are not ULIDs ("default", "local"), so
// do not reuse ownerFromTag's check here; see DL-004.
//
// @decision(DL-004)
func (r *Reconciler) orgFromTag(v string) bool {
	return v != "" && (v == r.DefaultOrg || v == auth.LocalID || v == DefaultOrgID)
}

// launchInFlight reports whether a live session with this name is still
// starting in org, which is what a launch looks like before it records
// its sandbox. org is the sandbox's own (orgOf), the one a hosted
// launch's session lives in, not the reconciler's default.
//
// @decision(DL-004)
func (r *Reconciler) launchInFlight(ctx context.Context, org, name string) bool {
	if name == "" {
		return false
	}
	sess, err := r.Store.Sessions().Get(ctx, org, name)
	return err == nil && sess.State == store.StateStarting && sess.SandboxID == ""
}

// orgOf is the org a sandbox's session belongs to: its org tag when
// orgFromTag accepts it, else the reconciler's default.
//
// @decision(DL-004)
func (r *Reconciler) orgOf(info sandbox.Info) string {
	if tagged := info.Tags[sandbox.TagOrg]; r.orgFromTag(tagged) {
		return tagged
	}
	if r.DefaultOrg != "" {
		return r.DefaultOrg
	}
	return auth.LocalID
}

// adopt records a sandbox the store did not know. Its name, clones and
// ownership come from the tags; an owner or org tag that fails
// ownerFromTag or orgFromTag falls back to the reconciler's defaults.
//
// @decision(DL-004)
func (r *Reconciler) adopt(ctx context.Context, info sandbox.Info, now time.Time) error {
	owner := r.DefaultOwner
	if owner == "" {
		owner = auth.LocalID
	}
	org := r.orgOf(info)
	if tagged := info.Tags[sandbox.TagUser]; ownerFromTag(tagged) {
		owner = tagged
	}
	name := info.Name
	if name == "" {
		name = info.ID
	}
	sess := store.Session{
		ID:        store.NewID(now),
		OrgID:     org,
		OwnerID:   owner,
		Name:      name,
		State:     store.StateRunning,
		SandboxID: info.ID,
		Request:   store.SessionRequest{Name: name},
		Clones:    sandbox.ReposFromTags(info.Tags),
		CreatedAt: info.CreatedAt,
	}
	err := r.Store.Sessions().Create(ctx, sess)
	if errors.Is(err, store.ErrConflict) {
		// The name is held by a live session with another sandbox —
		// most likely one this server is starting right now. Adopt
		// under the sandbox id so nothing is lost, and let the name
		// belong to the newer session.
		sess.Name = info.ID
		err = r.Store.Sessions().Create(ctx, sess)
	}
	return err
}
