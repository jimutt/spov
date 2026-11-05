package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/store"
)

// launchedElsewhere records a running session whose sandbox exists but
// whose launch this server never ran: exactly what another replica's
// session looks like from here.
func launchedElsewhere(t *testing.T, f *apiFixture, name string) store.Session {
	t.Helper()
	ctx := context.Background()
	h, err := f.provider.Create(ctx, sandbox.Spec{Name: name, Image: "i", Command: []string{"x"},
		CPU: 0.5, MemoryMiB: 1024, Timeout: time.Hour, IdleTimeout: 10 * time.Minute,
		Tags: sandbox.BuildTags(sandbox.TagSet{Name: name, User: "u", CreatedAt: time.Now()}), Egress: sandbox.EgressOpen()})
	if err != nil {
		t.Fatal(err)
	}
	info, _ := h.Info(ctx)
	sess := store.Session{ID: store.NewID(time.Now()), OrgID: auth.LocalID, OwnerID: auth.LocalID,
		Name: name, State: store.StateRunning, SandboxID: info.ID}
	if err := f.store.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	return sess
}

// TestConnectWorksFromAReplicaThatDidNotLaunch is the invariant: no
// request path depends on the launching process. The handle comes from
// the sandbox ID and the password from the sandbox itself.
//
// @decision(DL-003)
func TestConnectWorksFromAReplicaThatDidNotLaunch(t *testing.T) {
	f := newAPIFixture(t)
	f.provider.ExecResults = map[string]sandbox.ExecResult{
		"printenv " + opencode.PasswordEnvVar: {Stdout: "from-the-sandbox\n"},
	}
	sess := launchedElsewhere(t, f, "elsewhere")

	resp, body := f.do(http.MethodPost, "/api/v1/sessions/"+sess.ID+"/connect", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	got := connectBody(t, body)
	if got.Password != "from-the-sandbox" {
		t.Errorf("password = %q", got.Password)
	}
	if got.Token == "" || got.URL == "" {
		t.Errorf("connection = %+v", got)
	}
}

// TestConnectReportsAnUnrecoverablePasswordAsBadGateway: a fabricated
// password would authenticate nowhere and fail at the proxy with a far
// less obvious message, so the failure surfaces here instead.
//
// @decision(DL-003)
func TestConnectReportsAnUnrecoverablePasswordAsBadGateway(t *testing.T) {
	f := newAPIFixture(t)
	// No ExecResults, so the recovery finds nothing.
	sess := launchedElsewhere(t, f, "no-password")

	resp, body := f.do(http.MethodPost, "/api/v1/sessions/"+sess.ID+"/connect", nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if f.srv.Service().hasCachedPassword(sess.ID) {
		t.Error("a failed recovery cached something")
	}
}

// TestDeleteWorksFromAReplicaThatDidNotLaunch: terminate goes through
// the sandbox ID, so any replica can end any session.
//
// @decision(DL-003)
func TestDeleteWorksFromAReplicaThatDidNotLaunch(t *testing.T) {
	f := newAPIFixture(t)
	sess := launchedElsewhere(t, f, "ending")
	if f.provider.Running() != 1 {
		t.Fatalf("%d sandboxes running before the delete", f.provider.Running())
	}

	resp, body := f.do(http.MethodDelete, "/api/v1/sessions/"+sess.ID, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if f.provider.Running() != 0 {
		t.Error("the sandbox is still running")
	}
	got, err := f.store.Sessions().Get(context.Background(), auth.LocalID, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.StateStopped {
		t.Errorf("state = %s", got.State)
	}
}

// TestStoppingASessionDropsItsCachedPassword: a stopped sandbox's
// password must not linger in a replica's memory.
//
// @decision(DL-003)
func TestStoppingASessionDropsItsCachedPassword(t *testing.T) {
	f := newAPIFixture(t)
	f.provider.ExecResults = map[string]sandbox.ExecResult{
		"printenv " + opencode.PasswordEnvVar: {Stdout: "cached\n"},
	}
	sess := launchedElsewhere(t, f, "cached")
	if resp, body := f.do(http.MethodPost, "/api/v1/sessions/"+sess.ID+"/connect", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if !f.srv.Service().hasCachedPassword(sess.ID) {
		t.Fatal("the password was not cached")
	}
	if resp, body := f.do(http.MethodDelete, "/api/v1/sessions/"+sess.ID, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if f.srv.Service().hasCachedPassword(sess.ID) {
		t.Error("a stopped session's password is still held")
	}
}

// TestEventsFromAReplicaThatDidNotLaunchEndWithTheDerivedOutcome: a
// finished session yields its one terminal event rather than an error
// about an unknown launch, so Follow sees a valid stream from anywhere.
//
// @decision(DL-003)
func TestEventsFromAReplicaThatDidNotLaunchEndWithTheDerivedOutcome(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		state store.SessionState
		error string
		want  string
	}{
		{"running", store.StateRunning, "", "ready"},
		{"stopped", store.StateStopped, "", "error"},
		{"failed", store.StateFailed, "it went wrong", "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAPIFixture(t)
			sess := store.Session{ID: store.NewID(time.Now()), OrgID: auth.LocalID, OwnerID: auth.LocalID,
				Name: "s-" + tc.name, State: tc.state, Error: tc.error}
			if err := f.store.Sessions().Create(ctx, sess); err != nil {
				t.Fatal(err)
			}
			evs := f.events(sess.ID)
			if len(evs) != 1 || evs[0].Name != tc.want {
				t.Fatalf("events = %+v, want one %s", evs, tc.want)
			}
			if tc.want == "error" && !strings.Contains(string(evs[0].Data), tc.error) {
				t.Errorf("the error event lost the reason: %s", evs[0].Data)
			}
		})
	}
}

// TestEventsPollTheStoreForALaunchRunningElsewhere: the stream opens
// with a step so the client knows the launch is healthy, then follows
// the store until the launching replica records an outcome.
//
// @decision(DL-003)
func TestEventsPollTheStoreForALaunchRunningElsewhere(t *testing.T) {
	f := newAPIFixture(t)
	ctx := context.Background()
	sess := store.Session{ID: store.NewID(time.Now()), OrgID: auth.LocalID, OwnerID: auth.LocalID,
		Name: "in-flight", State: store.StateStarting}
	if err := f.store.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	// Another replica finishes the launch once this one has sent its
	// step: read up to that event, then update the store, then read on.
	req, _ := http.NewRequest(http.MethodGet, f.http.URL+"/api/v1/sessions/"+sess.ID+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	rd := bufio.NewReader(resp.Body)
	var head bytes.Buffer
	for !strings.Contains(head.String(), "event: step") || !strings.HasSuffix(head.String(), "\n\n") {
		line, err := rd.ReadString('\n')
		head.WriteString(line)
		if err != nil {
			t.Fatalf("the stream ended before its step: %v\n%s", err, head.String())
		}
	}
	sess.State = store.StateRunning
	sess.SandboxID = "sb-elsewhere"
	if err := f.store.Sessions().Update(ctx, sess); err != nil {
		t.Fatal(err)
	}
	rest, _ := io.ReadAll(rd)
	evs := parseEvents(append(head.Bytes(), rest...))
	if len(evs) < 2 {
		t.Fatalf("events = %+v, want a step then a terminal event", evs)
	}
	if evs[0].Name != "step" {
		t.Errorf("first event is %q, want step", evs[0].Name)
	}
	var step StepEvent
	if err := json.Unmarshal(evs[0].Data, &step); err != nil {
		t.Fatalf("decode step: %v", err)
	}
	if step.State != "started" || step.Step == "" {
		t.Errorf("step = %+v", step)
	}
	if last := evs[len(evs)-1]; last.Name != "ready" {
		t.Errorf("last event is %q, want ready", last.Name)
	}
}

// TestEventsForALaunchThatNeverFinishesEndWithTheStream: the handler's
// own deadline bounds it, so a client is not held forever by a launch
// nothing is running.
//
// @decision(DL-003)
func TestEventsForALaunchThatNeverFinishesEndWithTheStream(t *testing.T) {
	f := newAPIFixture(t)
	ctx := context.Background()
	sess := store.Session{ID: store.NewID(time.Now()), OrgID: auth.LocalID, OwnerID: auth.LocalID,
		Name: "stuck", State: store.StateStarting}
	if err := f.store.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	// The session vanishes, which is how the poll learns to stop.
	go func() {
		time.Sleep(testStorePoll / 2)
		sess.State = store.StateFailed
		sess.Error = "launch orphaned"
		if err := f.store.Sessions().Update(ctx, sess); err != nil {
			t.Errorf("update: %v", err)
		}
	}()

	done := make(chan []event, 1)
	go func() { done <- f.events(sess.ID) }()
	select {
	case evs := <-done:
		if len(evs) == 0 {
			t.Fatal("no events")
		}
		last := evs[len(evs)-1]
		if last.Name != "error" || !strings.Contains(string(last.Data), "launch orphaned") {
			t.Errorf("last event = %+v, want the orphan failure", last)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the stream never ended")
	}
}

// TestDeleteAfterALaunchCommitsTerminatesTheSandbox: between committing
// its handle and the goroutine's cleanup, a launch is finished but its
// cancel is still registered. A delete arriving then must terminate the
// sandbox, not cancel a launch that has already succeeded.
//
// @decision(DL-003)
func TestDeleteAfterALaunchCommitsTerminatesTheSandbox(t *testing.T) {
	f := newAPIFixture(t)
	svc := f.srv.Service()
	ctx := context.Background()
	sess := launchedElsewhere(t, f, "committed")

	// The window: the launch has recorded its sandbox, its cancel is
	// still in the map.
	svc.mu.Lock()
	svc.launches[sess.ID] = func() {}
	svc.mu.Unlock()

	p := auth.Principal{OrgID: auth.LocalID, UserID: auth.LocalID, Admin: true}
	if err := svc.Delete(ctx, p, sess.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, err := f.store.Sessions().Get(ctx, auth.LocalID, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.StateStopped {
		t.Errorf("state = %s; delete reported success without stopping it", got.State)
	}
	if f.provider.Running() != 0 {
		t.Error("the sandbox is still running after a successful delete")
	}
}
