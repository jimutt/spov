package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/opencode/contract"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/store"
)

func sessionPrincipal(sess store.Session) auth.Principal {
	return auth.Principal{OrgID: sess.OrgID, UserID: sess.OwnerID}
}

// One mint serves many of the server's own acts.
//
// @decision(DL-030)
func TestTheServersOwnActsShareOneMint(t *testing.T) {
	f := newAPIFixture(t)
	sess := runningSession(t, f)
	svc := f.srv.Service()
	p := sessionPrincipal(sess)

	first, err := svc.connectReusing(context.Background(), p, sess)
	if err != nil {
		t.Fatal(err)
	}
	after := len(serverMints(f))
	if after != 1 {
		t.Fatalf("mints after the first act = %d, want 1", after)
	}
	for i := range 4 {
		got, err := svc.connectReusing(context.Background(), p, sess)
		if err != nil {
			t.Fatal(err)
		}
		if got.Token != first.Token {
			t.Fatalf("act %d was served a different token, so the cache is not being read", i+2)
		}
	}
	if n := len(serverMints(f)); n != after {
		t.Errorf("mints after five acts = %d, want %d: a hit must not mint", n, after)
	}
}

// A miss mints.
//
// @decision(DL-030)
func TestAMissMints(t *testing.T) {
	f := newAPIFixture(t)
	sess := runningSession(t, f)
	svc := f.srv.Service()
	p := sessionPrincipal(sess)

	if _, err := svc.connectReusing(context.Background(), p, sess); err != nil {
		t.Fatal(err)
	}
	svc.conns.forget(sess.ID)
	if _, err := svc.connectReusing(context.Background(), p, sess); err != nil {
		t.Fatal(err)
	}
	if n := len(serverMints(f)); n != 2 {
		t.Errorf("mints = %d, want 2: a forgotten entry must mint again", n)
	}
}

// The /connect endpoint neither reads the cache nor fills it.
//
// @decision(DL-030)
func TestTheConnectEndpointIsOutsideTheCache(t *testing.T) {
	f := newAPIFixture(t)
	sess := runningSession(t, f)
	svc := f.srv.Service()

	for i := range 2 {
		res, body := f.do(http.MethodPost, "/api/v1/sessions/alpha/connect", nil)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("connect %d: %d %s", i+1, res.StatusCode, body)
		}
	}
	if n := len(serverMints(f)); n != 2 {
		t.Errorf("mints = %d, want 2: the client path mints per request", n)
	}
	if svc.hasCachedConnection(sess.ID) {
		t.Error("the endpoint filled the cache; a token a CLI holds must not be handed to the server's own acts")
	}

	// And a cached entry does not divert the endpoint.
	if _, err := svc.connectReusing(context.Background(), sessionPrincipal(sess), sess); err != nil {
		t.Fatal(err)
	}
	before := len(serverMints(f))
	res, body := f.do(http.MethodPost, "/api/v1/sessions/alpha/connect", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("connect: %d %s", res.StatusCode, body)
	}
	if n := len(serverMints(f)); n != before+1 {
		t.Errorf("mints = %d, want %d: the endpoint must mint even with an entry cached", n, before+1)
	}
}

// An entry within ConnectionExpiryMargin of expiry is not served, and is
// dropped rather than kept.
//
// @decision(DL-030)
func TestAnExpiringConnectionIsNotServed(t *testing.T) {
	now := time.Now()
	c := newConnCache(4)
	c.put("s1", Connection{Token: "t", ExpiresAt: now.Add(ConnectionExpiryMargin * 2)})
	c.put("s2", Connection{Token: "t", ExpiresAt: now.Add(ConnectionExpiryMargin / 2)})
	c.put("s3", Connection{Token: "t", ExpiresAt: now.Add(-time.Minute)})

	if _, ok := c.get("s1", now); !ok {
		t.Error("a connection well inside its life was not served")
	}
	for _, id := range []string{"s2", "s3"} {
		if _, ok := c.get(id, now); ok {
			t.Errorf("%s was served within %s of expiry", id, ConnectionExpiryMargin)
		}
		if c.has(id) {
			t.Errorf("%s was skipped but kept; an unusable credential must not sit in memory", id)
		}
	}
}

// A connection missing its token or expiry, or keyed by an empty session
// id, is not cached. Connect never produces one.
//
// @decision(DL-030)
func TestAConnectionWithNoExpiryIsNotCached(t *testing.T) {
	c := newConnCache(4)
	c.put("s1", Connection{Token: "t"})
	c.put("s2", Connection{ExpiresAt: time.Now().Add(time.Hour)})
	c.put("", Connection{Token: "t", ExpiresAt: time.Now().Add(time.Hour)})
	if n := c.len(); n != 0 {
		t.Errorf("cached %d connections, want 0", n)
	}
}

// The cache is a bounded LRU, like passwordCache.
//
// @decision(DL-030) @decision(DL-003)
func TestTheConnectionCacheEvictsTheLeastRecentlyUsed(t *testing.T) {
	now := time.Now()
	conn := Connection{Token: "t", ExpiresAt: now.Add(time.Hour)}
	c := newConnCache(2)
	c.put("s1", conn)
	c.put("s2", conn)
	if _, ok := c.get("s1", now); !ok {
		t.Fatal("s1 was not held")
	}
	c.put("s3", conn)
	if c.has("s2") {
		t.Error("s2 survived; s1 was used more recently and must outlive it")
	}
	if !c.has("s1") || !c.has("s3") {
		t.Error("the wrong entries were evicted")
	}
	if n := c.len(); n != 2 {
		t.Errorf("len = %d, want 2", n)
	}
}

// A sandbox refusing the credential drops the entry and the act is run
// again over a fresh mint.
//
// @decision(DL-030)
func TestARefusedCredentialIsMintedAgainAndTheActRetried(t *testing.T) {
	f := newAPIFixture(t)
	sess := runningSession(t, f)
	svc := f.srv.Service()
	p := sessionPrincipal(sess)

	// Warm the cache, so the refusal lands on a cached entry rather
	// than on a mint this act made.
	warm, err := svc.connectReusing(context.Background(), p, sess)
	if err != nil {
		t.Fatal(err)
	}
	minted := len(serverMints(f))

	var seen []string
	err = svc.withConnection(context.Background(), p, sess, func(conn Connection) error {
		seen = append(seen, conn.Token)
		if len(seen) == 1 {
			return fmt.Errorf("read the sandbox: %w (401 Unauthorized)", ErrSandboxUnauthorized)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("the act was not recovered: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("the act ran %d times, want 2", len(seen))
	}
	if seen[0] != warm.Token {
		t.Error("the first attempt did not use the cached connection")
	}
	if seen[1] == warm.Token {
		t.Error("the retry presented the refused token again")
	}
	if n := len(serverMints(f)); n != minted+1 {
		t.Errorf("mints = %d, want %d: exactly one re-mint", n, minted+1)
	}
	if !svc.hasCachedConnection(sess.ID) {
		t.Error("the fresh connection was not cached, so the next act mints again")
	}
}

// Only a refusal is retried: any other failure, such as a 500, may have
// taken effect.
//
// @decision(DL-030)
func TestOnlyARefusalIsRetried(t *testing.T) {
	f := newAPIFixture(t)
	sess := runningSession(t, f)
	svc := f.srv.Service()
	p := sessionPrincipal(sess)

	boom := errors.New("the sandbox answered 500 Internal Server Error")
	calls := 0
	err := svc.withConnection(context.Background(), p, sess, func(Connection) error {
		calls++
		return boom
	})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the act's own error unchanged", err)
	}
	if calls != 1 {
		t.Errorf("the act ran %d times, want 1", calls)
	}
	if !svc.hasCachedConnection(sess.ID) {
		t.Error("a failure that was not a refusal dropped the connection")
	}
}

// A stopped session's connection goes with its password: both reach a
// sandbox that is no longer there.
//
// @decision(DL-030) @decision(DL-003)
func TestStoppingASessionDropsItsConnection(t *testing.T) {
	f := newAPIFixture(t)
	sess := runningSession(t, f)
	svc := f.srv.Service()

	if _, err := svc.connectReusing(context.Background(), sessionPrincipal(sess), sess); err != nil {
		t.Fatal(err)
	}
	if !svc.hasCachedConnection(sess.ID) {
		t.Fatal("nothing was cached to drop")
	}
	svc.ForgetSession(sess.ID)
	if svc.hasCachedConnection(sess.ID) {
		t.Error("the connection outlived the session")
	}
	if svc.hasCachedPassword(sess.ID) {
		t.Error("the password outlived the session")
	}
}

// The recorder drops its cached connection when the sandbox refuses
// the credential, so a redial obtains a new one rather than the refused
// token again.
//
// @decision(DL-030) @decision(DL-017)
func TestTheRecorderDropsARefusedConnection(t *testing.T) {
	f := newRecorderFixture(t)
	var dropped []string
	var dmu sync.Mutex
	f.rec.drop = func(id, _ string) {
		dmu.Lock()
		defer dmu.Unlock()
		dropped = append(dropped, id)
	}
	f.sb.dialErr = fmt.Errorf("open the sandbox event stream: %w (401 Unauthorized)", ErrSandboxUnauthorized)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the capture to give up", func() bool { return !f.rec.Recording(f.sess.ID) })

	dmu.Lock()
	got := len(dropped)
	dmu.Unlock()
	// One per failed span: the first dial and every redial.
	if want := 1 + RecorderRedials; got != want {
		t.Errorf("dropped %d times, want %d: every refused span must drop before the redial", got, want)
	}
	dmu.Lock()
	defer dmu.Unlock()
	for _, id := range dropped {
		if id != f.sess.ID {
			t.Fatalf("dropped %q, want the session under capture", id)
		}
	}
}

// Only a refusal drops it. An ordinary break keeps the connection, or
// every redial would spend a mint.
//
// @decision(DL-030) @decision(DL-017)
func TestAnOrdinaryBreakKeepsTheConnection(t *testing.T) {
	f := newRecorderFixture(t)
	var drops atomic.Int64
	f.rec.drop = func(string, string) { drops.Add(1) }
	f.sb.dialErr = errors.New("connection refused")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the capture to give up", func() bool { return !f.rec.Recording(f.sess.ID) })
	if n := drops.Load(); n != 0 {
		t.Errorf("dropped %d times for a transport break, want 0", n)
	}
}

// Shutdown drops every cached connection.
//
// @decision(DL-030)
func TestShutdownDropsEveryCachedConnection(t *testing.T) {
	f := newAPIFixture(t)
	sess := runningSession(t, f)
	svc := f.srv.Service()

	if _, err := svc.connectReusing(context.Background(), sessionPrincipal(sess), sess); err != nil {
		t.Fatal(err)
	}
	if !svc.hasCachedConnection(sess.ID) {
		t.Fatal("nothing was cached to drop")
	}
	svc.Close()
	if svc.hasCachedConnection(sess.ID) {
		t.Error("a connect token outlived the process that minted it")
	}
}

// jobSandbox stands a job's session up against a live HTTP endpoint, so
// the real acts can be driven end to end.
func jobSandbox(t *testing.T, handler http.HandlerFunc) (*Service, store.Job, *sandbox.Fake) {
	t.Helper()
	ctx := context.Background()
	srv := httptest.NewServer(contract.Checked(t, handler))
	t.Cleanup(srv.Close)

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
	return s, seedJobSession(t, s, store.StateRunning, info.ID), fake
}

// mintsOf counts the tokens minted through the server, which carry
// metadata; the launcher's own proxy mint does not.
func mintsOf(f *sandbox.Fake) int {
	n := 0
	for _, r := range f.ConnectTokens {
		if r.Metadata != "" {
			n++
		}
	}
	return n
}

// The job acts share one mint, driven through the real methods, so
// a call site reverted to Service.Connect fails here. The tests above
// cover only the mechanism.
//
// @decision(DL-030) @decision(DL-045)
func TestTheJobActsShareOneMint(t *testing.T) {
	ctx := context.Background()
	s, job, fake := jobSandbox(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/api/session") && r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"data":[{"id":"ses_x","title":"t"}]}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/api/fs/list") {
			_, _ = w.Write([]byte(`{"location":{"directory":"/workspace/out"},"data":[{"path":"a.md","type":"file"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"id":"ses_x"}}`))
	})

	for _, act := range []struct {
		name string
		run  func() error
	}{
		{"OpenJobGoal", func() error { return s.OpenJobGoal(ctx, job) }},
		{"SetJobLoopPaused", func() error {
			return s.SetJobLoopPaused(ctx, auth.Principal{OrgID: job.OrgID, UserID: "user_1"}, job, true)
		}},
		// @decision(DL-051)
		{"OpenJobOut", func() error {
			out, err := s.OpenJobOut(ctx, job)
			if err != nil {
				return err
			}
			if _, err := out.List(ctx, ""); err != nil {
				return err
			}
			_, err = out.Read(ctx, "a.md", 1<<20)
			return err
		}},
		{"CollectJobEvidence", func() error { _, err := s.CollectJobEvidence(ctx, job); return err }},
		{"CollectJobOutcome", func() error { _, err := s.CollectJobOutcome(ctx, job); return err }},
		// @decision(DL-049)
		{"EndJobRun", func() error { return s.EndJobRun(ctx, job) }},
	} {
		if err := act.run(); err != nil {
			t.Fatalf("%s: %v", act.name, err)
		}
	}
	if n := mintsOf(fake); n != 1 {
		t.Errorf("mints = %d, want 1: the server-side job acts must share one credential", n)
	}
}

// The connect and drop seams NewRecorder wires use the cache. Every other
// recorder test replaces f.rec.connect with a fake.
//
// @decision(DL-030) @decision(DL-017)
func TestNewRecorderWiresTheConnectionCache(t *testing.T) {
	ctx := context.Background()
	s, job, fake := jobSandbox(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	sess, err := s.deps.Store.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	rec := NewRecorder(s, s.deps.Store, time.Now, func(string) {})

	first, err := rec.connect(ctx, auth.Recorder(sess.OrgID), sess)
	if err != nil {
		t.Fatal(err)
	}
	again, err := rec.connect(ctx, auth.Recorder(sess.OrgID), sess)
	if err != nil {
		t.Fatal(err)
	}
	if again.Token != first.Token {
		t.Error("the recorder's seam minted again; a redial must not spend the budget it shares")
	}
	if n := mintsOf(fake); n != 1 {
		t.Errorf("mints = %d, want 1", n)
	}

	// And its drop seam reaches the cache it just filled.
	if !s.hasCachedConnection(sess.ID) {
		t.Fatal("nothing was cached")
	}
	rec.drop(sess.ID, first.Token)
	if s.hasCachedConnection(sess.ID) {
		t.Error("the recorder's drop seam is not wired to the cache")
	}
}

// Reading a file (the out directory and evidence acts) reports a refusal too.
// The helper lives in supervisor_open.go, outside sandboxio.go's refusal
// tests.
//
// @decision(DL-030) @decision(DL-045)
func TestReadingAFileReportsARefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	conn := Connection{URL: srv.URL}

	if _, err := readOutFile(context.Background(), conn, "report.md", 1<<20); !errors.Is(err, ErrSandboxUnauthorized) {
		t.Errorf("readOutFile: %v, want a refusal", err)
	}
	if _, err := listOutDir(context.Background(), conn, ""); !errors.Is(err, ErrSandboxUnauthorized) {
		t.Errorf("listOutDir: %v, want a refusal", err)
	}
	if _, err := readEvidence(context.Background(), conn, EvidencePath); !errors.Is(err, ErrSandboxUnauthorized) {
		t.Errorf("readEvidence: %v, want a refusal", err)
	}
}

// A cache hit still passes Connect's checks: the Service.Get ownership
// check and the refusal of a session that is not running.
//
// @decision(DL-030) @decision(DL-006)
func TestAHitIsStillChecked(t *testing.T) {
	ctx := context.Background()
	f := newAPIFixture(t)
	sess := runningSession(t, f)
	svc := f.srv.Service()

	if _, err := svc.connectReusing(ctx, sessionPrincipal(sess), sess); err != nil {
		t.Fatal(err)
	}
	if !svc.hasCachedConnection(sess.ID) {
		t.Fatal("nothing was cached, so this test would be vacuous")
	}

	t.Run("another org is refused", func(t *testing.T) {
		stranger := auth.Principal{OrgID: "org_other", UserID: "user_other"}
		if _, err := svc.connectReusing(ctx, stranger, sess); err == nil {
			t.Error("a cached connection was served to a principal that may not reach the session")
		}
	})

	t.Run("a session that is not running is refused", func(t *testing.T) {
		stopped, err := f.store.Sessions().Get(ctx, sess.OrgID, sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		stopped.State = store.StateStopped
		if err := f.store.Sessions().Update(ctx, stopped); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.connectReusing(ctx, sessionPrincipal(sess), sess); !errors.Is(err, ErrSessionNotRunning) {
			t.Errorf("err = %v, want ErrSessionNotRunning", err)
		}
	})
}

// A refusal drops only the credential it was refused on, so a peer that
// has already minted afresh keeps its entry.
//
// @decision(DL-030)
func TestARefusalDropsOnlyTheTokenItWasRefusedOn(t *testing.T) {
	now := time.Now()
	c := newConnCache(4)
	c.put("s1", Connection{Token: "fresh", ExpiresAt: now.Add(time.Hour)})

	c.forgetIf("s1", "stale")
	if !c.has("s1") {
		t.Error("a refusal on an older token discarded a peer's fresh mint")
	}
	c.forgetIf("s1", "fresh")
	if c.has("s1") {
		t.Error("the refused token stayed cached")
	}
}

// A refusal drops the session's password as well as its connection,
// since a 401 does not say which of the two was refused.
//
// @decision(DL-030) @decision(DL-003)
func TestARefusalDropsBothCredentials(t *testing.T) {
	ctx := context.Background()
	f := newAPIFixture(t)
	sess := runningSession(t, f)
	svc := f.srv.Service()
	p := sessionPrincipal(sess)

	if _, err := svc.connectReusing(ctx, p, sess); err != nil {
		t.Fatal(err)
	}
	// A value the sandbox would never hand back, so the assertion is
	// about this password rather than an empty cache.
	svc.passwords.put(sess.ID, "stale-password")

	refused := false
	err := svc.withConnection(ctx, p, sess, func(Connection) error {
		if refused {
			return nil
		}
		refused = true
		return fmt.Errorf("%s: %w (401)", actRead, ErrSandboxUnauthorized)
	})
	if err != nil {
		t.Fatal(err)
	}
	if pw, ok := svc.passwords.get(sess.ID); ok && pw == "stale-password" {
		t.Error("the refused password survived, so a retry could only fail the same way")
	}
}

// A second refusal is not retried, and the credential that earned it is
// not left cached.
//
// @decision(DL-030)
func TestASecondRefusalDropsTheFreshConnection(t *testing.T) {
	ctx := context.Background()
	f := newAPIFixture(t)
	sess := runningSession(t, f)
	svc := f.srv.Service()
	p := sessionPrincipal(sess)

	calls := 0
	err := svc.withConnection(ctx, p, sess, func(Connection) error {
		calls++
		return fmt.Errorf("%s: %w (401)", actRead, ErrSandboxUnauthorized)
	})
	if !errors.Is(err, ErrSandboxUnauthorized) {
		t.Errorf("err = %v, want the refusal", err)
	}
	if calls != 2 {
		t.Errorf("the act ran %d times, want 2: one retry and no more", calls)
	}
	if svc.hasCachedConnection(sess.ID) {
		t.Error("a known-refused credential stayed cached")
	}
}
