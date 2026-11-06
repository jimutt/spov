package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/server/client"
	"github.com/jimutt/spov/internal/store"
)

// TestAttachConnectsThroughTheServer is the normal path: the server
// recovers the password and mints the token; the CLI holds neither.
//
// @decision(DL-025) @decision(DL-030)
func TestAttachConnectsThroughTheServer(t *testing.T) {
	f := newAPIFixture(t)
	h := f.seedSession("spov-running", 20*time.Minute)
	// The age printed is the CLI's clock against a time that crossed
	// JSON, which drops the monotonic reading, so a wall-clock step
	// would move it. Pinned to the stored creation time instead.
	at := f.session("spov-running").CreatedAt.Add(20 * time.Minute)
	f.env.Now = func() time.Time { return at }

	_, stderr, err := runWith(t, f.env, "attach", "spov-running")
	if err != nil {
		t.Fatalf("attach: %v\n%s", err, stderr)
	}
	if f.tuiRuns != 1 {
		t.Errorf("the TUI ran %d times, want 1", f.tuiRuns)
	}
	if !strings.HasPrefix(f.tuiSeen.ServerURL, "http://127.0.0.1:") {
		t.Errorf("the TUI server URL = %q, want a loopback address", f.tuiSeen.ServerURL)
	}
	// The password was recovered from the sandbox by the server, once,
	// and never printed.
	//
	// @decision(DL-030)
	if execs := h.(*sandbox.FakeHandle).ExecLog(); len(execs) != 1 {
		t.Errorf("the sandbox was exec'd %d times, want once for the password", len(execs))
	}
	if strings.Contains(stderr, "seeded-password") {
		t.Error("the session password was printed")
	}
	for _, want := range []string{"Attached to spov-running", "running for 20m", "cloned acme/app"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
}

// TestAttachNeverTerminates: attach is the way back into a session, and
// `stop` is the way out of one. Exiting the TUI must leave the sandbox up.
//
// @decision(DL-025)
func TestAttachNeverTerminates(t *testing.T) {
	f := newAPIFixture(t)
	f.seedSession("spov-running", time.Minute)

	_, stderr, err := runWith(t, f.env, "attach", "spov-running")
	if err != nil {
		t.Fatalf("attach: %v\n%s", err, stderr)
	}
	if f.provider.Running() != 1 {
		t.Error("attach terminated the sandbox")
	}
	if !strings.Contains(stderr, "spov stop spov-running") {
		t.Errorf("no hint how to stop it:\n%s", stderr)
	}
	if strings.Contains(stderr, "spov attach") {
		t.Errorf("the hint repeats the command just run:\n%s", stderr)
	}
}

// TestAttachRejectsStoppedSession: attaching to a stopped session can
// only fail, and it fails clearly before any connection is attempted.
func TestAttachRejectsStoppedSession(t *testing.T) {
	f := newAPIFixture(t)
	f.seedSession("old", time.Minute)
	if _, _, err := runWith(t, f.env, "stop", "old"); err != nil {
		t.Fatal(err)
	}
	// A name no live session holds finds the caller's stopped session of
	// that name (DL-087), which is refused for what it is, as by ID.
	_, _, err := runWith(t, f.env, "attach", "old")
	if err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Errorf("by name: err = %v", err)
	}
	_, _, err = runWith(t, f.env, "attach", f.session("old").ID)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "stopped") || !strings.Contains(err.Error(), "spov fly") {
		t.Errorf("by id: err = %v", err)
	}
	if f.proxyStarts != 0 {
		t.Error("a proxy was started for a stopped session")
	}
}

func TestAttachUnknownSession(t *testing.T) {
	f := newAPIFixture(t)
	_, _, err := runWith(t, f.env, "attach", "nope")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "spov ls") {
		t.Errorf("err = %v", err)
	}
}

// TestAttachReportsUnrecoverablePassword: the server cannot mint without
// the password, and the CLI says the sandbox is still billing.
//
// @decision(DL-025) @decision(DL-029)
func TestAttachReportsUnrecoverablePassword(t *testing.T) {
	f := newAPIFixture(t)
	f.seedSession("mute", time.Minute)
	f.provider.ExecErr = errors.New("exec failed")

	_, _, err := runWith(t, f.env, "attach", "mute")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "spov stop mute") {
		t.Errorf("error %q does not say how to stop the billing sandbox", err)
	}
	if f.provider.Running() != 1 {
		t.Error("the sandbox was terminated")
	}
}

// TestAttachVersionGate: the same gate as fly.
//
// @decision(DL-025) @decision(DL-035)
func TestAttachVersionGate(t *testing.T) {
	f := newAPIFixture(t)
	f.seedSession("s", time.Minute)
	f.health.Version = "9.9.9"

	_, stderr, err := runWith(t, f.env, "attach", "s")
	if err == nil || !strings.Contains(err.Error(), "--allow-version-mismatch") {
		t.Fatalf("err = %v", err)
	}
	if f.tuiRuns != 0 {
		t.Error("the TUI ran despite the mismatch")
	}
	if !strings.Contains(stderr, "spov stop s") {
		t.Errorf("no hint:\n%s", stderr)
	}

	_, _, err = runWith(t, f.env, "attach", "s", "--allow-version-mismatch")
	if err != nil {
		t.Fatalf("attach with override: %v", err)
	}
	if f.tuiRuns != 1 {
		t.Error("the TUI did not run with the override")
	}
}

func TestAttachNoTUI(t *testing.T) {
	f := newAPIFixture(t)
	f.seedSession("s", time.Minute)

	_, stderr, err := runWith(t, f.env, "attach", "s", "--no-tui")
	if err != nil {
		t.Fatalf("attach: %v\n%s", err, stderr)
	}
	if f.tuiRuns != 0 {
		t.Error("the TUI ran despite --no-tui")
	}
	// No URL is printed: the loopback one dies with this process.
	//
	// @decision(DL-030)
	if strings.Contains(stderr, "http://") {
		t.Errorf("a URL was printed:\n%s", stderr)
	}
	if !strings.Contains(stderr, "--web") {
		t.Errorf("no hint how to get a browser onto it:\n%s", stderr)
	}
}

func TestAttachKeepsSandboxOnTUIFailure(t *testing.T) {
	f := newAPIFixture(t)
	f.seedSession("s", time.Minute)
	f.tuiErr = errors.New("boom")

	_, stderr, err := runWith(t, f.env, "attach", "s")
	if err == nil {
		t.Fatal("expected the TUI failure")
	}
	if f.provider.Running() != 1 || !strings.Contains(stderr, "spov stop s") {
		t.Errorf("the sandbox must be left running with a hint:\n%s", stderr)
	}
}

func TestAttachByID(t *testing.T) {
	f := newAPIFixture(t)
	f.seedSession("named", time.Minute)
	id := f.session("named").ID

	_, stderr, err := runWith(t, f.env, "attach", id)
	if err != nil {
		t.Fatalf("attach: %v\n%s", err, stderr)
	}
	// Hints use the name even when the user gave the ID.
	if !strings.Contains(stderr, "spov stop named") {
		t.Errorf("hint does not use the session name:\n%s", stderr)
	}
}

// TestAttachReportsAnUnreachableSession: the proxy path failing is a
// local problem; the sandbox stays and the user is told how to stop it.
//
// @decision(DL-030)
func TestAttachReportsAnUnreachableSession(t *testing.T) {
	f := newAPIFixture(t)
	f.seedSession("s", time.Minute)
	f.proxyHealthErr = errors.New("refused")

	_, _, err := runWith(t, f.env, "attach", "s")
	if err == nil || !strings.Contains(err.Error(), "spov stop s") {
		t.Fatalf("err = %v", err)
	}
	if f.provider.Running() != 1 {
		t.Error("the sandbox was terminated")
	}
}

func TestAttachRequiresAnArgument(t *testing.T) {
	if _, _, err := runWith(t, Env{}, "attach"); err == nil {
		t.Fatal("expected an error")
	}
}

// TestAttachMintsThroughTheServer: the client's proxy has no provider
// credential; every token it holds was minted by the server for it.
//
// @decision(DL-030)
func TestAttachMintsThroughTheServer(t *testing.T) {
	f := newAPIFixture(t)
	f.seedSession("s", time.Minute)
	if _, stderr, err := runWith(t, f.env, "attach", "s", "--no-tui"); err != nil {
		t.Fatalf("attach: %v\n%s", err, stderr)
	}
	mints := f.provider.ConnectTokens
	if len(mints) != 1 {
		t.Fatalf("%d tokens minted, want 1", len(mints))
	}
	if !strings.Contains(mints[0].Metadata, "local") {
		t.Errorf("the mint carries no principal: %q", mints[0].Metadata)
	}
	// Recorded as an audit row too.
	n, err := f.deps.Store.Mints().CountSince(context.Background(), "local", f.session("s").ID, time.Time{})
	if err != nil || n != 1 {
		t.Errorf("mint rows = %d (%v), want 1", n, err)
	}
}

// seedConversations records conversations against a session, oldest
// first and a minute apart, since rows in the same millisecond have no
// creation order. Ids ascend with the times, so these tests cover the
// CLI's plumbing; the ordering rule itself is a store contract case.
func (f *apiFixture) seedConversations(name string, opencodeIDs ...string) {
	f.t.Helper()
	sess := f.session(name)
	at := time.Now().Add(-time.Duration(len(opencodeIDs)) * time.Minute)
	for _, id := range opencodeIDs {
		if _, err := f.deps.Store.Conversations().UpsertConversation(context.Background(), store.Conversation{
			ID:         store.NewID(at),
			OrgID:      sess.OrgID,
			SessionID:  sess.ID,
			OpenCodeID: id,
			CreatedAt:  at,
		}); err != nil {
			f.t.Fatal(err)
		}
		at = at.Add(time.Minute)
	}
}

// TestAttachOpensTheSessionsConversation: attach hands the client the
// conversation already running, so a message typed into it reaches the
// agent rather than opening a second conversation beside it.
//
// @decision(DL-025)
func TestAttachOpensTheSessionsConversation(t *testing.T) {
	f := newAPIFixture(t)
	f.seedSession("spov-running", 20*time.Minute)
	f.seedConversations("spov-running", "ses_theJobsOwn")

	if _, stderr, err := runWith(t, f.env, "attach", "spov-running"); err != nil {
		t.Fatalf("attach: %v\n%s", err, stderr)
	}
	if f.tuiSeen.Session != "ses_theJobsOwn" {
		t.Errorf("the TUI was given session %q, want the session's own conversation", f.tuiSeen.Session)
	}
}

// TestAttachOpensTheOldestConversation: of a job's own conversation, a
// subagent's and a stray one from an earlier attach, attach opens the
// oldest.
//
// @decision(DL-025) @decision(DL-045)
func TestAttachOpensTheOldestConversation(t *testing.T) {
	f := newAPIFixture(t)
	f.seedSession("spov-running", 20*time.Minute)
	f.seedConversations("spov-running", "ses_oldestIsTheJobs", "ses_aSubagents", "ses_strayFromAnAttach")

	if _, stderr, err := runWith(t, f.env, "attach", "spov-running"); err != nil {
		t.Fatalf("attach: %v\n%s", err, stderr)
	}
	if f.tuiSeen.Session != "ses_oldestIsTheJobs" {
		t.Errorf("the TUI was given session %q, want the oldest", f.tuiSeen.Session)
	}
}

// TestAttachWithNoConversationStillAttaches: a session the recorder has
// not reached yet has no conversation row, and attach opens the client on
// its home screen.
//
// @decision(DL-025) @decision(DL-017)
func TestAttachWithNoConversationStillAttaches(t *testing.T) {
	f := newAPIFixture(t)
	f.seedSession("spov-running", 20*time.Minute)

	_, stderr, err := runWith(t, f.env, "attach", "spov-running")
	if err != nil {
		t.Fatalf("attach: %v\n%s", err, stderr)
	}
	if f.tuiRuns != 1 {
		t.Errorf("the TUI ran %d times, want 1: a session with no conversation must still attach", f.tuiRuns)
	}
	if f.tuiSeen.Session != "" {
		t.Errorf("the TUI was given session %q, want none", f.tuiSeen.Session)
	}
}

// TestAttachSurvivesAnUnreadableConversation: a failed conversation read
// costs the home screen and a warning, never the attach.
//
// @decision(DL-025)
func TestAttachSurvivesAnUnreadableConversation(t *testing.T) {
	f := newAPIFixture(t)
	f.seedSession("spov-running", 20*time.Minute)

	// A client pointed at nothing: the read fails, the selection does
	// not.
	dead := client.New("http://127.0.0.1:1", f.token)
	var errOut strings.Builder
	env := f.env
	env.Err = &errOut

	if got := openingConversation(context.Background(), &env, dead, "any"); got != "" {
		t.Errorf("openingConversation = %q, want none when the read fails", got)
	}
	if !strings.Contains(errOut.String(), "could not read") {
		t.Errorf("the failure was silent:\n%s", errOut.String())
	}
}
