package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/opencode/contract"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/session"
	"github.com/jimutt/spov/internal/store"
)

// seedJobSession puts a job and its session in a service's store.
func seedJobSession(t *testing.T, s *Service, sessState store.SessionState, sandboxID string) store.Job {
	t.Helper()
	return seedJobSessionWith(t, s, sessState, sandboxID, nil)
}

// seedJobSessionWith is seedJobSession with a chance to set the job's
// create-time fields before it is stored.
func seedJobSessionWith(t *testing.T, s *Service, sessState store.SessionState, sandboxID string, set func(*store.Job)) store.Job {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	sess := store.Session{
		ID: store.NewID(now), OrgID: "org_1", OwnerID: "user_1",
		Name: "job-" + store.NewID(now), State: sessState, SandboxID: sandboxID,
		// A real job's session carries a validated request; without an
		// egress preset the launch fails before reaching what these
		// tests are about.
		Request: store.SessionRequest{Egress: "agent"},
	}
	if err := s.deps.Store.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	job := store.Job{
		ID: store.NewID(now), OrgID: sess.OrgID, OwnerID: sess.OwnerID,
		SessionID: sess.ID, Kind: store.JobKindReport,
		State: store.JobQueued, Goal: "write the report",
	}
	if set != nil {
		set(&job)
	}
	if err := s.deps.Store.Jobs().Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	return job
}

// A session that already has a sandbox was launched by an earlier
// attempt, so provisioning must be a no-op rather than a second launch.
//
// @decision(DL-045) @decision(DL-046)
func TestProvisionJobDoesNotRelaunchARunningSession(t *testing.T) {
	var launches int
	s := testService(t, func(d *Deps) {
		d.Launch = func(context.Context, session.Options, session.StepReporter) (*session.Session, error) {
			launches++
			return nil, errors.New("must not be called")
		}
	})
	job := seedJobSession(t, s, store.StateRunning, "sb-1")
	if err := s.ProvisionJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if launches != 0 {
		t.Fatalf("launched %d times for a session that already has a sandbox", launches)
	}
}

// A launch that failed reports the session's own reason, which says
// which step failed, rather than restating it vaguely.
//
// @decision(DL-045)
func TestProvisionJobReportsWhyTheLaunchFailed(t *testing.T) {
	s := testService(t, func(d *Deps) {
		d.Launch = func(context.Context, session.Options, session.StepReporter) (*session.Session, error) {
			return nil, errors.New("the image could not be resolved")
		}
		d.Forge = &fakeForge{platform: git.NewFake()}
	})
	job := seedJobSession(t, s, store.StateStarting, "")

	err := s.ProvisionJob(context.Background(), job)
	if err == nil {
		t.Fatal("ProvisionJob returned nil for a launch that failed")
	}
	if !strings.Contains(err.Error(), "image could not be resolved") {
		t.Errorf("error = %q, want the session's own reason", err)
	}
}

// A job's launch carries its JobOptions, which is what tags the sandbox
// and writes the plugin. Without them the sandbox is an ordinary
// interactive session with no loop and no permission policy.
//
// @decision(DL-045) @decision(DL-046) @decision(DL-040)
func TestAJobsLaunchCarriesItsJobOptions(t *testing.T) {
	var got *session.JobOptions
	s := testService(t, func(d *Deps) {
		d.Launch = func(_ context.Context, opts session.Options, _ session.StepReporter) (*session.Session, error) {
			got = opts.Job
			return nil, errors.New("stop here; the options are what this asserts")
		}
		d.Forge = &fakeForge{platform: git.NewFake()}
	})
	job := seedJobSession(t, s, store.StateStarting, "")
	_ = s.ProvisionJob(context.Background(), job)

	if got == nil {
		t.Fatal("the launch carried no JobOptions; the sandbox would have no plugin and no job tag")
	}
	if got.ID != job.ID {
		t.Errorf("JobOptions.ID = %q, want %q", got.ID, job.ID)
	}
	if got.Goal != job.Goal {
		t.Errorf("JobOptions.Goal = %q, want the job's goal", got.Goal)
	}
}

// The goal is opened in the conversation the sandbox opened with, which
// is the oldest. A job's session is fresh and never joined, so a second
// conversation is one the agent made.
//
// @decision(DL-045)
func TestTheGoalOpensInTheOldestConversation(t *testing.T) {
	ctx := context.Background()
	s := testService(t, nil)
	job := seedJobSession(t, s, store.StateRunning, "sb-1")

	older := time.Now().Add(-time.Hour)
	for _, c := range []store.Conversation{
		{ID: store.NewID(time.Now()), OrgID: job.OrgID, SessionID: job.SessionID,
			OpenCodeID: "ses_second", CreatedAt: time.Now()},
		{ID: store.NewID(older), OrgID: job.OrgID, SessionID: job.SessionID,
			OpenCodeID: "ses_first", CreatedAt: older},
	} {
		if _, err := s.deps.Store.Conversations().UpsertConversation(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	sess, err := s.deps.Store.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.jobConversation(ctx, Connection{}, sess)
	if err != nil {
		t.Fatal(err)
	}
	if got != "ses_first" {
		t.Errorf("conversation = %q, want the one the sandbox opened with", got)
	}
}

// A sandbox with no conversation gets one created, not waited for.
//
// @decision(DL-045) @decision(DL-017)
func TestAJobCreatesItsOwnConversation(t *testing.T) {
	ctx := context.Background()
	var created bool
	var gotDir string
	srv := httptest.NewServer(contract.Checked(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		created = true
		var body struct {
			Location struct {
				Directory string `json:"directory"`
			} `json:"location"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotDir = body.Location.Directory
		_, _ = w.Write([]byte(`{"data":{"id":"ses_new"}}`))
	})))
	defer srv.Close()

	s := testService(t, nil)
	job := seedJobSession(t, s, store.StateRunning, "sb-1")
	sess, err := s.deps.Store.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	// Landing, not Sandbox.Workdir: the entrypoint cd's out of the
	// sandbox workdir before exec'ing the server, so Landing is the only
	// field that says where the agent ends up.
	//
	// @decision(DL-037)
	sess.Landing = "/workspace/spov"

	got, err := s.jobConversation(ctx, Connection{URL: srv.URL}, sess)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("no conversation was created; the job would wait for one that never appears")
	}
	if got != "ses_new" {
		t.Errorf("conversation = %q, want the created one", got)
	}
	if gotDir != "/workspace/spov" {
		t.Errorf("location[directory] = %q, want the directory the server started in", gotDir)
	}
	// Recorded, so a retry of this pass does not create a second one.
	convs, err := s.deps.Store.Conversations().ListConversations(ctx, sess.OrgID, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 1 || convs[0].OpenCodeID != "ses_new" {
		t.Errorf("conversations = %v, want the created one recorded", convs)
	}
}

// TestAJobsConversationDefaultsToTheWorkspaceAndNotToHome is the job
// half of DL-037: `Sandbox.Workdir` is never defaulted anywhere, so the
// empty case must land at the workspace root rather than /root.
//
// @decision(DL-037)
func TestAJobsConversationDefaultsToTheWorkspaceAndNotToHome(t *testing.T) {
	ctx := context.Background()
	var gotDir string
	srv := httptest.NewServer(contract.Checked(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Location struct {
				Directory string `json:"directory"`
			} `json:"location"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotDir = body.Location.Directory
		_, _ = w.Write([]byte(`{"data":{"id":"ses_new"}}`))
	})))
	defer srv.Close()

	s := testService(t, nil)
	job := seedJobSession(t, s, store.StateRunning, "sb-1")
	sess, err := s.deps.Store.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Landing != "" {
		t.Fatalf("the fixture recorded a landing (%q); this test is about the empty case",
			sess.Landing)
	}

	if _, err := s.jobConversation(ctx, Connection{URL: srv.URL}, sess); err != nil {
		t.Fatal(err)
	}
	if gotDir == opencode.SandboxHome {
		t.Errorf("location[directory] = %q: the conversation still lands in the sandbox's home",
			gotDir)
	}
	if gotDir != opencode.WorkspaceRoot {
		t.Errorf("location[directory] = %q, want %q", gotDir, opencode.WorkspaceRoot)
	}
}

// TestTheReportsAreCollectedFromTheWorkspaceNotTheLanding drives the
// real collection path and pins where it reads: the workspace's out
// directory, never a session's landing, with each path relative and
// escaped and each read bounded.
//
// @decision(DL-037) @decision(DL-045) @decision(DL-051)
func TestTheReportsAreCollectedFromTheWorkspaceNotTheLanding(t *testing.T) {
	ctx := context.Background()
	var dirs, paths []string
	srv := httptest.NewServer(contract.Checked(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dirs = append(dirs, r.URL.Query().Get("location[directory]"))
		paths = append(paths, r.URL.EscapedPath())
		if strings.HasSuffix(r.URL.Path, "/api/fs/list") {
			_, _ = w.Write([]byte(`{"location":{"directory":"/workspace/out"},"data":[` +
				`{"path":"images/","type":"directory"},{"path":"my report.md","type":"file"}]}`))
			return
		}
		_, _ = w.Write([]byte("a report that runs long"))
	})))
	defer srv.Close()

	// Driven through OpenJobOut rather than the helpers directly: the
	// directory is chosen there, so a test that passed its own would pin
	// nothing about the shipped code.
	fake := sandbox.NewFake()
	fake.ConnectBaseURL = srv.URL
	s := testService(t, func(d *Deps) { d.Provider = fake })

	h, err := fake.Create(ctx, sandbox.Spec{Name: "job", Image: "i", Command: []string{"x"},
		CPU: 0.5, MemoryMiB: 1024, Timeout: time.Hour, IdleTimeout: 10 * time.Minute,
		Tags:   sandbox.BuildTags(sandbox.TagSet{Name: "job", User: "u", CreatedAt: time.Now()}),
		Egress: sandbox.EgressOpen()})
	if err != nil {
		t.Fatal(err)
	}
	info, _ := h.Info(ctx)
	fake.ExecResults = map[string]sandbox.ExecResult{
		"printenv " + opencode.PasswordEnvVar: {Stdout: "pw\n"},
	}
	job := seedJobSession(t, s, store.StateRunning, info.ID)
	sess, err := s.deps.Store.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	// An explicit landing: the agent works in the repo, the reports
	// still do not.
	sess.Landing = "/workspace/spov"
	if err := s.deps.Store.Sessions().Update(ctx, sess); err != nil {
		t.Fatal(err)
	}

	out, err := s.OpenJobOut(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := out.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || !entries[0].Dir || entries[1].Path != "my report.md" || entries[1].Dir {
		t.Errorf("entries = %+v", entries)
	}
	got, err := out.Read(ctx, "my report.md", 1<<20)
	if err != nil || string(got) != "a report that runs long" {
		t.Errorf("read = %q, %v", got, err)
	}
	cut, err := out.Read(ctx, "my report.md", 8)
	if !errors.Is(err, jobs.ErrFileTruncated) || string(cut) != "a report" {
		t.Errorf("a read over its bound = %q, %v; want the first bytes and ErrFileTruncated", cut, err)
	}
	if _, err := out.Read(ctx, "../secret", 1<<20); !errors.Is(err, jobs.ErrNoFile) {
		t.Errorf("a path climbing out was read: %v", err)
	}
	for i, dir := range dirs {
		if dir != opencode.OutDir {
			t.Errorf("request %d (%s) sent location %q, want %q", i, paths[i], dir, opencode.OutDir)
		}
	}
	if len(paths) != 3 || !strings.HasSuffix(paths[1], "/api/fs/read/my%20report.md") {
		t.Errorf("paths = %v, want the read relative and escaped", paths)
	}
}

// A sandbox that answers without an id is an unverified shape, not a
// job to fail silently.
//
// @decision(DL-045)
func TestACreatedConversationWithoutAnIDIsAnError(t *testing.T) {
	srv := httptest.NewServer(contract.Checked(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{}}`))
	})))
	defer srv.Close()

	if _, err := createConversation(context.Background(), Connection{URL: srv.URL}, "/root"); err == nil {
		t.Fatal("createConversation returned nil for a body carrying no id")
	}
}

// Collecting from a sandbox that is already gone says so, with its own
// error: the job's transcript is still there, and gate 2 reads the gone
// sandbox as unverified rather than as a job that delivered nothing.
//
// @decision(DL-045) @decision(DL-051)
func TestCollectingFromAGoneSandboxIsNotAFailure(t *testing.T) {
	ctx := context.Background()
	s := testService(t, nil)
	job := seedJobSession(t, s, store.StateStopped, "")

	if _, err := s.OpenJobOut(ctx, job); !errors.Is(err, jobs.ErrSandboxGone) {
		t.Errorf("out directory error = %v, want jobs.ErrSandboxGone", err)
	}
	if _, err := s.CollectJobEvidence(ctx, job); !errors.Is(err, jobs.ErrSandboxGone) {
		t.Errorf("evidence error = %v, want jobs.ErrSandboxGone", err)
	}
	if _, err := s.CollectJobOutcome(ctx, job); !errors.Is(err, jobs.ErrSandboxGone) {
		t.Errorf("outcome error = %v, want jobs.ErrSandboxGone", err)
	}
}

// A marker is written in spov's own voice. Its source is what makes it
// spov's, never its name, and the store refuses a spov-prefixed name
// from any other source.
//
// @decision(DL-045) @decision(DL-018)
func TestAJobMarkerIsWrittenInSpovsVoice(t *testing.T) {
	ctx := context.Background()
	s := testService(t, nil)
	job := seedJobSession(t, s, store.StateRunning, "sb-1")

	if err := s.MarkJobEvent(ctx, job, jobs.MarkerEvidence, "what the plugin saw"); err != nil {
		t.Fatal(err)
	}
	events, err := s.deps.Store.Conversations().ListEvents(ctx, job.OrgID, job.SessionID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("stored %d events, want 1", len(events))
	}
	if events[0].Source != store.SourceSpov {
		t.Errorf("source = %v, want %v; a sandbox must not be able to forge one",
			events[0].Source, store.SourceSpov)
	}
	if events[0].Name != jobs.MarkerEvidence {
		t.Errorf("name = %q, want %q", events[0].Name, jobs.MarkerEvidence)
	}
}

// A job pointed at a session that does not exist reports that rather
// than panicking on a zero-valued row.
//
// @decision(DL-045)
func TestJobPathsReportAMissingSession(t *testing.T) {
	ctx := context.Background()
	s := testService(t, nil)
	job := store.Job{
		ID: store.NewID(time.Now()), OrgID: "org_1", OwnerID: "user_1",
		SessionID: store.NewID(time.Now()), Kind: store.JobKindReport,
		State: store.JobQueued, Goal: "g",
	}
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"ProvisionJob", func() error { return s.ProvisionJob(ctx, job) }},
		{"OpenJobGoal", func() error { return s.OpenJobGoal(ctx, job) }},
		{"OpenJobOut", func() error { _, err := s.OpenJobOut(ctx, job); return err }},
		{"CollectJobEvidence", func() error { _, err := s.CollectJobEvidence(ctx, job); return err }},
		{"CollectJobOutcome", func() error { _, err := s.CollectJobOutcome(ctx, job); return err }},
		// @decision(DL-054)
		{"SetJobLoopPaused", func() error { return s.SetJobLoopPaused(ctx, auth.Principal{}, job, true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); err == nil {
				t.Error("returned nil for a job whose session does not exist")
			}
		})
	}
}

// Opening the goal needs a running sandbox; a session without one
// reports rather than dialling nothing.
//
// @decision(DL-045)
func TestOpeningTheGoalNeedsARunningSandbox(t *testing.T) {
	ctx := context.Background()
	s := testService(t, nil)
	job := seedJobSession(t, s, store.StateStarting, "")
	if err := s.OpenJobGoal(ctx, job); err == nil {
		t.Error("OpenJobGoal returned nil against a session with no sandbox")
	}
}

// Holding or releasing the loop needs a running sandbox too, and the
// refusal names ErrSessionNotRunning rather than failing at the dial —
// a pause tolerates the sandbox being gone (pauseLoop warns and the
// row stays steered), but a resume is a person asking for something
// that cannot happen and needs a 409 they can act on.
//
// @decision(DL-054) @decision(DL-045)
func TestHoldingTheLoopNeedsARunningSandbox(t *testing.T) {
	ctx := context.Background()
	s := testService(t, nil)
	job := seedJobSession(t, s, store.StateStarting, "")
	for _, paused := range []bool{true, false} {
		if err := s.SetJobLoopPaused(ctx, auth.Principal{}, job, paused); !errors.Is(err, ErrSessionNotRunning) {
			t.Errorf("SetJobLoopPaused(%v) = %v, want ErrSessionNotRunning", paused, err)
		}
	}
}

// The adopted sandbox is recorded onto the job's session, so one
// sandbox is not counted by both quotas.
//
// @decision(DL-045) @decision(DL-009)
func TestAdoptingASandboxRecordsItOnTheSession(t *testing.T) {
	ctx := context.Background()
	s := testService(t, nil)
	job := seedJobSession(t, s, store.StateStarting, "")

	if err := s.AdoptJobSandbox(ctx, job, "sb-adopted"); err != nil {
		t.Fatal(err)
	}
	sess, err := s.deps.Store.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.SandboxID != "sb-adopted" {
		t.Errorf("SandboxID = %q, want the adopted sandbox", sess.SandboxID)
	}
	if sess.State != store.StateRunning {
		t.Errorf("state = %v, want running", sess.State)
	}
	// Idempotent: a second pass changes nothing.
	if err := s.AdoptJobSandbox(ctx, job, "sb-adopted"); err != nil {
		t.Fatal(err)
	}
}

// Adopting onto a session someone has already stopped must not
// resurrect it: the delete path has concluded that row.
//
// @decision(DL-045) @decision(DL-003)
func TestAdoptingDoesNotResurrectAStoppedSession(t *testing.T) {
	ctx := context.Background()
	s := testService(t, nil)
	job := seedJobSession(t, s, store.StateRunning, "sb-1")
	sess, err := s.deps.Store.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	sess.State = store.StateStopped
	if err := s.deps.Store.Sessions().Update(ctx, sess); err != nil {
		t.Fatal(err)
	}

	if err := s.AdoptJobSandbox(ctx, job, "sb-other"); err != nil {
		t.Fatal(err)
	}
	got, err := s.deps.Store.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.StateStopped {
		t.Errorf("state = %v, want the stop to stand", got.State)
	}
}

// Collecting needs a live sandbox; the failure paths report rather than
// dial nothing.
//
// @decision(DL-045)
func TestCollectingReportsAConnectFailure(t *testing.T) {
	ctx := context.Background()
	// A provider that knows nothing of this sandbox, which is what a
	// sandbox that has already gone looks like.
	s := testService(t, func(d *Deps) { d.Provider = sandbox.NewFake() })
	job := seedJobSession(t, s, store.StateRunning, "sb-gone")
	out, err := s.OpenJobOut(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := out.List(ctx, ""); err == nil {
		t.Error("listing the out directory returned nil with no way to connect")
	}
	if _, err := s.CollectJobEvidence(ctx, job); err == nil {
		t.Error("CollectJobEvidence returned nil with no way to connect")
	}
}

// A finished job's sandbox is stopped through the ordinary delete path,
// so it ends the way an interactive session's does.
//
// @decision(DL-050) @decision(DL-003)
func TestTerminateJobSandboxStopsARunningSession(t *testing.T) {
	ctx := context.Background()
	fake := sandbox.NewFake()
	s := testService(t, func(d *Deps) { d.Provider = fake })

	// A real sandbox to stop, so the terminate has something to act on.
	h, err := fake.Create(ctx, sandbox.Spec{
		Name: "job-sb", Image: "i", Command: []string{"x"},
		CPU: 0.5, MemoryMiB: 1024, Timeout: time.Hour, IdleTimeout: 10 * time.Minute,
		Tags:   sandbox.BuildTags(sandbox.TagSet{Name: "job-sb", User: "user_1", CreatedAt: time.Now()}),
		Egress: sandbox.EgressOpen(),
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := h.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	job := seedJobSession(t, s, store.StateRunning, info.ID)

	if err := s.TerminateJobSandbox(ctx, job); err != nil {
		t.Fatalf("terminating: %v", err)
	}
	if fake.Running() != 0 {
		t.Errorf("%d sandboxes still running: the job's sandbox was left billing", fake.Running())
	}
	sess, err := s.deps.Store.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.State.Live() {
		t.Errorf("session state = %v, want it recorded stopped", sess.State)
	}
}

// A job whose sandbox is already gone is the ordinary case for the
// backstop path — the sandbox stopping is how the job got to delivery —
// so it must not be an error.
//
// @decision(DL-050)
func TestTerminateJobSandboxAcceptsAnAlreadyStoppedSession(t *testing.T) {
	s := testService(t, func(d *Deps) { d.Provider = sandbox.NewFake() })
	job := seedJobSession(t, s, store.StateStopped, "sb-gone")

	if err := s.TerminateJobSandbox(context.Background(), job); err != nil {
		t.Errorf("terminating an already-stopped session returned %v; that is the backstop's ordinary case", err)
	}
}

// A job whose session row is gone has nothing to stop, and says so
// quietly rather than failing a verdict that has already been written.
//
// @decision(DL-050)
func TestTerminateJobSandboxAcceptsAMissingSession(t *testing.T) {
	s := testService(t, func(d *Deps) { d.Provider = sandbox.NewFake() })
	job := seedJobSession(t, s, store.StateRunning, "sb-1")
	job.SessionID = store.NewID(time.Now())

	if err := s.TerminateJobSandbox(context.Background(), job); err != nil {
		t.Errorf("a job with no session row returned %v, want nil", err)
	}
}

// An adopted sandbox's launch is gone, so its credentials are worked out
// from the owner's forge and a renewable token is marked due, for the
// next renewal pass to re-mint while the job is unfinished.
//
// @decision(DL-021) @decision(DL-043)
func TestAdoptingRecordsTheCredentialsAndADueToken(t *testing.T) {
	ctx := context.Background()
	forge := &fakeForge{platform: git.NewFake()}
	forge.platform.Renews = true
	s := testService(t, func(d *Deps) { d.Forge = forge })
	job := seedJobSession(t, s, store.StateStarting, "")
	sess, err := s.deps.Store.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	sess.Request.Repos = []store.RepoRef{{Platform: "fake", Owner: "acme", Name: "app"}}
	if err := s.deps.Store.Sessions().Update(ctx, sess); err != nil {
		t.Fatal(err)
	}

	if err := s.AdoptJobSandbox(ctx, job, "sb-adopted"); err != nil {
		t.Fatal(err)
	}
	sess, err = s.deps.Store.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.Credentials) != 1 || sess.Credentials[0].Kind != store.CredentialRenewed {
		t.Errorf("credentials = %+v", sess.Credentials)
	}
	if sess.GitTokenExpiresAt == nil || !renewalDue(sess, s.deps.Now()) {
		t.Errorf("the adopted session's token is not due: %v", sess.GitTokenExpiresAt)
	}
}
