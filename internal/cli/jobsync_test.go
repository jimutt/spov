package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/jobs/jobstest"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
	"github.com/jimutt/spov/internal/store"
)

// syncJobFixture is an in-process server with a supervisor whose queue
// only records, and a checkout of alice/spov in the working directory.
type syncJobFixture struct {
	*apiFixture
	env  Env
	srv  *server.Server
	base string
}

func newSyncJobFixture(t *testing.T) *syncJobFixture {
	t.Helper()
	f := newAPIFixtureWithConfig(t, platformOnlyConfig)
	f.deps.Jobs = &jobs.Supervisor{
		Store: f.deps.Store, Provider: f.provider,
		Queue: func(context.Context, jobs.JobArgs) error { return nil },
		Warn:  func(string) {},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(f.deps, ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	hs := httptest.NewUnstartedServer(srv.Handler())
	hs.Listener = ln
	hs.Start()
	t.Cleanup(hs.Close)
	env := f.env
	env.Client = client.New(hs.URL, f.token)
	base := initCLIGitRepo(t, f.wd, "https://codefloe.com/alice/spov.git")
	return &syncJobFixture{apiFixture: f, env: env, srv: srv, base: base}
}

func (s *syncJobFixture) onlyJob(t *testing.T) store.Job {
	t.Helper()
	list, err := s.deps.Store.Jobs().List(context.Background(), auth.LocalID, store.JobFilter{})
	if err != nil || len(list) != 1 {
		t.Fatalf("jobs = %v, %v; want one", list, err)
	}
	return list[0]
}

// launched creates a sync job, launches its sandbox, moves it to state
// and cans the sandbox's export with a commit and an uncommitted file.
func (s *syncJobFixture) launched(t *testing.T, state store.JobState) (store.Job, string) {
	t.Helper()
	ctx := context.Background()
	if _, stderr, err := runWith(t, s.env, "jobs", "create", "add the agent file", "--kind", "sync"); err != nil {
		t.Fatalf("create: %v\n%s", err, stderr)
	}
	job := s.onlyJob(t)
	if err := s.srv.Service().ProvisionJob(ctx, job); err != nil {
		t.Fatalf("provision: %v", err)
	}
	for _, st := range jobstest.PathTo(state) {
		job.State = st
		if err := s.deps.Store.Jobs().Update(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	clone := filepath.Join(t.TempDir(), "sandbox")
	runCLIGit(t, s.wd, "clone", s.wd, clone)
	runCLIGit(t, clone, "config", "user.name", "Sandbox Agent")
	runCLIGit(t, clone, "config", "user.email", "agent@example.com")
	if err := os.WriteFile(filepath.Join(clone, "agent.txt"), []byte("committed by agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCLIGit(t, clone, "add", "agent.txt")
	runCLIGit(t, clone, "commit", "-m", "agent commit")
	head := strings.TrimSpace(runCLIGit(t, clone, "rev-parse", "HEAD"))
	bundlePath := filepath.Join(t.TempDir(), "agent.bundle")
	runCLIGit(t, clone, "bundle", "create", bundlePath, s.base+"..HEAD")
	bundle, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(map[string]any{
		"branch": "main", "base_commit": s.base, "head_commit": head,
		"bundle":       base64.StdEncoding.EncodeToString(bundle),
		"worktree_tar": cliWorktreeTar(t, map[string]string{"wip.txt": "uncommitted edit\n"}),
	})
	sess, err := s.deps.Store.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	h := s.provider.Handles()[sess.SandboxID]
	if h == nil {
		t.Fatalf("the job's session %s has no sandbox", sess.State)
	}
	if _, err := sessionExportProbe(h, "spov", s.base, string(out), s.provider); err != nil {
		t.Fatal(err)
	}
	return job, head
}

func (s *syncJobFixture) session(t *testing.T, job store.Job) store.Session {
	t.Helper()
	sess, err := s.deps.Store.Sessions().Get(context.Background(), job.OrgID, job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

// A sync job is seeded from the checkout it is created in, after the
// checks fly's sync makes, and lands in the repository.
//
// @decision(DL-100) @decision(DL-101)
func TestASyncJobIsSeededFromTheCheckout(t *testing.T) {
	s := newSyncJobFixture(t)
	if err := os.WriteFile(filepath.Join(s.wd, "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runWith(t, s.env, "jobs", "create", "work", "--kind", "sync"); err == nil {
		t.Fatal("a dirty checkout was accepted")
	}
	if list, _ := s.deps.Store.Jobs().List(context.Background(), auth.LocalID, store.JobFilter{}); len(list) != 0 {
		t.Fatalf("a refused create reached the server: %v", list)
	}
	if err := os.Remove(filepath.Join(s.wd, "dirty.txt")); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := runWith(t, s.env, "jobs", "create", "work", "--kind", "sync", "--hold", "45m"); err != nil {
		t.Fatalf("create: %v\n%s", err, stderr)
	}
	job := s.onlyJob(t)
	sess := s.session(t, job)
	if job.Kind != store.JobKindSync || sess.Request.Landing != "spov" || sess.Request.Sync == nil ||
		sess.Request.Sync.Branch != "main" || sess.Request.Sync.BaseCommit != s.base {
		t.Errorf("job %s, landing %q, seed %+v", job.Kind, sess.Request.Landing, sess.Request.Sync)
	}
	if _, _, err := runWith(t, s.env, "jobs", "create", "work", "--hold", "45m"); err == nil {
		t.Error("--hold was accepted on a pr job")
	}
	// Another repository than the checkout's is refused in a job's own
	// words: --sync and --no-sync are fly's flags.
	_, _, err := runWith(t, s.env, "jobs", "create", "work", "--kind", "sync", "--repo", "alice/other")
	if err == nil || !strings.Contains(err.Error(), "does not match --repo alice/other") || strings.Contains(err.Error(), "-sync") {
		t.Errorf("a --repo that is not the checkout's: %v", err)
	}
}

// Apply lands the held state in the checkout and releases the sandbox.
//
// @decision(DL-102)
func TestJobsApplyLandsTheStateAndReleases(t *testing.T) {
	s := newSyncJobFixture(t)
	job, head := s.launched(t, store.JobSucceeded)
	_, stderr, err := runWith(t, s.env, "jobs", "apply", job.ID)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, stderr)
	}
	if got := strings.TrimSpace(runCLIGit(t, s.wd, "rev-parse", "HEAD")); got != head {
		t.Errorf("local HEAD = %s, want the sandbox's %s", got, head)
	}
	if b, err := os.ReadFile(filepath.Join(s.wd, "wip.txt")); err != nil || string(b) != "uncommitted edit\n" {
		t.Errorf("wip.txt = %q, %v", b, err)
	}
	if sess := s.session(t, job); sess.State.Live() {
		t.Errorf("the session is %s after apply; the hold should be released", sess.State)
	}
	got, _ := s.deps.Store.Jobs().Get(context.Background(), job.OrgID, job.ID)
	if got.SyncAppliedAt == nil {
		t.Error("the apply was not recorded")
	}
}

// A landing that fails releases nothing and says how to retry.
//
// @decision(DL-102)
func TestJobsApplyKeepsTheSandboxWhenItFails(t *testing.T) {
	s := newSyncJobFixture(t)
	job, _ := s.launched(t, store.JobSucceeded)
	s.provider.ExecErr = errors.New("sandbox exec failed")
	_, stderr, err := runWith(t, s.env, "jobs", "apply", job.ID)
	if err == nil {
		t.Fatal("apply succeeded with a failing export")
	}
	if !s.session(t, job).State.Live() {
		t.Error("a failed apply ended the session")
	}
	if !strings.Contains(stderr, "spov jobs release "+job.ID) {
		t.Errorf("a failed apply did not say how to discard:\n%s", stderr)
	}
	s.provider.ExecErr = nil
	// --keep lands and keeps the hold.
	if _, stderr, err := runWith(t, s.env, "jobs", "apply", job.ID, "--keep"); err != nil {
		t.Fatalf("apply --keep: %v\n%s", err, stderr)
	} else if !strings.Contains(stderr, "spov jobs release "+job.ID) {
		t.Errorf("--keep did not say how to end the hold:\n%s", stderr)
	}
	if !s.session(t, job).State.Live() {
		t.Error("--keep released the sandbox")
	}
}

// Apply on a job still steered lands its work so far and releases
// nothing.
//
// @decision(DL-102)
func TestJobsApplyOnASteeredJobDoesNotRelease(t *testing.T) {
	s := newSyncJobFixture(t)
	job, head := s.launched(t, store.JobSteered)
	_, stderr, err := runWith(t, s.env, "jobs", "apply", job.ID)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "still steered") {
		t.Errorf("apply did not say the job is still running:\n%s", stderr)
	}
	if got := strings.TrimSpace(runCLIGit(t, s.wd, "rev-parse", "HEAD")); got != head {
		t.Errorf("local HEAD = %s, want %s", got, head)
	}
	if !s.session(t, job).State.Live() {
		t.Error("apply on an unfinished job ended its session")
	}
}

// A held job is not cancelled, stopped or swept by accident: each
// refusal names release, and stop --all leaves it.
//
// @decision(DL-102)
func TestAHeldJobIsNotStoppedByAccident(t *testing.T) {
	s := newSyncJobFixture(t)
	job, _ := s.launched(t, store.JobSucceeded)
	sess := s.session(t, job)
	if _, _, err := runWith(t, s.env, "jobs", "cancel", job.ID); err == nil || !strings.Contains(err.Error(), "spov jobs release "+job.ID) {
		t.Errorf("cancel of a held job = %v, want a hint naming release", err)
	}
	if _, _, err := runWith(t, s.env, "stop", sess.ID); err == nil || !strings.Contains(err.Error(), "spov jobs release") {
		t.Errorf("stop of a held session = %v, want a refusal naming release", err)
	}
	if _, stderr, err := runWith(t, s.env, "stop", "--all"); err != nil || !strings.Contains(stderr, "spov jobs release "+job.ID) {
		t.Errorf("stop --all = %v, stderr %q", err, stderr)
	}
	if !s.session(t, job).State.Live() {
		t.Fatal("the held session was stopped")
	}
	out, _, err := runWith(t, s.env, "jobs", "release", job.ID)
	if err != nil || !strings.Contains(out, "discarded") {
		t.Fatalf("release = %v, %q", err, out)
	}
	if s.session(t, job).State.Live() {
		t.Error("release left the session running")
	}
}
