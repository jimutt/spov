package server

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/credproxy"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/session"
	"github.com/jimutt/spov/internal/store"
)

// renewFixture is a running session on a renewable platform.
type renewFixture struct {
	*apiFixture
	id  string
	seq int64
}

func newRenewFixture(t *testing.T) *renewFixture {
	t.Helper()
	f := newAPIFixture(t)
	f.forge.platform.Renews = true
	id := f.create(repoReq("alpha"))
	f.events(id)
	return &renewFixture{apiFixture: f, id: id}
}

func (f *renewFixture) row() store.Session {
	f.t.Helper()
	sess, err := f.store.Sessions().Get(context.Background(), auth.LocalID, f.id)
	if err != nil {
		f.t.Fatal(err)
	}
	return sess
}

// due makes the session's token due, minted `ago`.
func (f *renewFixture) due(ago time.Duration) {
	f.t.Helper()
	now := time.Now()
	if err := f.store.Sessions().SetGitToken(context.Background(), auth.LocalID, f.id, now.Add(-ago), now.Add(5*time.Minute)); err != nil {
		f.t.Fatal(err)
	}
}

func (f *renewFixture) handle() *sandbox.FakeHandle {
	f.t.Helper()
	for _, h := range f.provider.Handles() {
		return h
	}
	f.t.Fatal("no sandbox")
	return nil
}

// refreshes are the execs that were credential refreshes.
func (f *renewFixture) refreshes() [][]string {
	var out [][]string
	for _, c := range f.handle().ExecLog() {
		if len(c) > 3 && c[3] == "spov-refresh" {
			out = append(out, c)
		}
	}
	return out
}

// act stores an event as the recorder would: an OpenCode frame in a
// conversation with a sequence, or a spov marker on the session.
func (f *renewFixture) act(name string, source store.EventSource) {
	f.t.Helper()
	ctx := context.Background()
	ev := store.Event{
		ID: store.NewID(time.Now()), OrgID: auth.LocalID, SessionID: f.id,
		Source: source, Name: name, At: time.Now(),
	}
	if source == store.SourceOpenCode {
		conv, err := f.store.Conversations().UpsertConversation(ctx, store.Conversation{
			ID: store.NewID(time.Now()), OrgID: auth.LocalID, SessionID: f.id, OpenCodeID: "ses_1",
		})
		if err != nil {
			f.t.Fatal(err)
		}
		f.seq++
		ev.ConversationID, ev.Seq = conv.ID, f.seq
	}
	if _, err := f.store.Conversations().PutEvents(ctx, []store.Event{ev}); err != nil {
		f.t.Fatal(err)
	}
}

// A launch on a renewable platform records the credential's kind, where
// it is held and when its token lapses; a stored token is named as stored.
//
// @decision(DL-021) @decision(DL-043) @decision(DL-034)
func TestALaunchRecordsItsCredentialsAndTheTokensExpiry(t *testing.T) {
	f := newRenewFixture(t)
	sess := f.row()
	if len(sess.Credentials) != 1 || sess.Credentials[0] != (store.SessionCredential{Platform: "fake", Kind: store.CredentialRenewed,
		Holder: store.CredentialInSandbox, Forge: "forgejo", Access: store.GitAccessRepos}) {
		t.Errorf("credentials = %+v", sess.Credentials)
	}
	if sess.GitTokenExpiresAt == nil || time.Until(*sess.GitTokenExpiresAt) < 50*time.Minute {
		t.Errorf("the token's expiry = %v, want about the sandbox timeout from now", sess.GitTokenExpiresAt)
	}

	stored := newAPIFixture(t)
	id := stored.create(repoReq("beta"))
	evs := stored.events(id)
	row, err := stored.store.Sessions().Get(context.Background(), auth.LocalID, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(row.Credentials) != 1 || row.Credentials[0].Kind != store.CredentialStored {
		t.Errorf("a stored token's credentials = %+v", row.Credentials)
	}
	if row.GitTokenExpiresAt != nil {
		t.Errorf("a stored token has a renewal expiry: %v", row.GitTokenExpiresAt)
	}
	noted := false
	for _, e := range evs {
		if e.Name == "note" && strings.Contains(string(e.Data), "stored fake token") {
			noted = true
		}
	}
	if !noted {
		t.Error("the launch log does not say the sandbox gets a stored token")
	}
}

// A token that is not near its expiry is left alone.
//
// @decision(DL-021)
func TestATokenThatIsNotDueIsNotReminted(t *testing.T) {
	f := newRenewFixture(t)
	f.act("session.next.prompted", store.SourceOpenCode)
	f.srv.svc.RenewGitTokens(context.Background())
	if got := f.refreshes(); len(got) != 0 {
		t.Errorf("a token with an hour left was re-minted: %v", got)
	}
}

// A due token is re-minted only when something happened since it was
// minted. A write into the sandbox resets its idle clock (spike S-R1),
// so a session nobody is using must not be written to.
//
// @decision(DL-021)
func TestAnIdleSessionIsNeverReminted(t *testing.T) {
	f := newRenewFixture(t)
	f.due(10 * time.Minute)

	// The recorder's own markers and its own connects are not activity.
	f.act(MarkerGap, store.SourceSpov)
	if err := f.store.Mints().Create(context.Background(), store.Mint{
		ID: store.NewID(time.Now()), OrgID: auth.LocalID, SessionID: f.id, UserID: auth.RecorderID,
		Port: 4096, ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	f.srv.svc.RenewGitTokens(context.Background())
	if got := f.refreshes(); len(got) != 0 {
		t.Fatalf("an idle session was written to: %v", got)
	}

	// Activity before the mint does not count either.
	f.due(-time.Minute)
	f.act("session.next.prompted", store.SourceOpenCode)
	f.due(0)
	f.srv.svc.RenewGitTokens(context.Background())
	if got := f.refreshes(); len(got) != 0 {
		t.Fatalf("activity from before the mint re-minted the token: %v", got)
	}
}

// A due token on a session in use is minted again and written in through
// the exec's standard input, and the row's expiry moves on.
//
// @decision(DL-021)
func TestADueTokenOnASessionInUseIsRemintedThroughStdin(t *testing.T) {
	for name, act := range map[string]func(f *renewFixture){
		"an agent event": func(f *renewFixture) { f.act("session.next.prompted", store.SourceOpenCode) },
		"a steer":        func(f *renewFixture) { f.act(MarkerSteered, store.SourceSpov) },
		"a person's connect": func(f *renewFixture) {
			if err := f.store.Mints().Create(context.Background(), store.Mint{
				ID: store.NewID(time.Now()), OrgID: auth.LocalID, SessionID: f.id, UserID: "someone",
				Port: 4096, ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now(),
			}); err != nil {
				f.t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRenewFixture(t)
			f.due(10 * time.Minute)
			act(f)
			before := len(f.forge.platform.Requested())

			f.srv.svc.RenewGitTokens(context.Background())

			got := f.refreshes()
			if len(got) != 1 {
				t.Fatalf("refreshes = %v, want one", got)
			}
			want := opencode.RefreshArgs("git.example.com", "fake-user", false)
			if !slices.Equal(got[0], want) {
				t.Errorf("argv = %v, want %v", got[0][3:], want[3:])
			}
			if n := len(f.forge.platform.Requested()); n != before+1 {
				t.Errorf("minted %d tokens, want one", n-before)
			}
			inputs := f.handle().ExecInputs()
			stdin := string(inputs[len(inputs)-1])
			if !strings.HasPrefix(stdin, "fake-secret-") || !strings.HasSuffix(stdin, "\n") {
				t.Errorf("stdin = %q, want the new token", stdin)
			}
			for _, c := range f.handle().ExecLog() {
				if strings.Contains(strings.Join(c, " "), strings.TrimSpace(stdin)) {
					t.Errorf("the token is in an argv: %v", c)
				}
			}
			if row := f.row(); row.GitTokenExpiresAt == nil || time.Until(*row.GitTokenExpiresAt) < 50*time.Minute {
				t.Errorf("the expiry did not move on: %v", row.GitTokenExpiresAt)
			}
		})
	}
}

// A job's session is in use while its job is unfinished, whatever else
// has happened in it.
//
// @decision(DL-021)
func TestAnUnfinishedJobsSessionIsInUse(t *testing.T) {
	f := newRenewFixture(t)
	f.due(10 * time.Minute)
	job := store.Job{ID: store.NewID(time.Now()), OrgID: auth.LocalID, OwnerID: "u", SessionID: f.id,
		Kind: store.JobKindReport, State: store.JobQueued, Goal: "g"}
	if err := f.store.Jobs().Create(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	f.srv.svc.RenewGitTokens(context.Background())
	if got := f.refreshes(); len(got) != 1 {
		t.Fatalf("an unfinished job's session was not re-minted: %v", got)
	}
}

// A refresh that fails leaves the row due, holds the token it may have
// written rather than revoking it, and is tried again only after new
// activity: a refresh that kept failing must not write into an idle
// sandbox every minute.
//
// @decision(DL-021)
func TestAFailedRefreshWaitsForNewActivityAndHoldsItsToken(t *testing.T) {
	f := newRenewFixture(t)
	f.due(10 * time.Minute)
	f.act("session.next.prompted", store.SourceOpenCode)
	f.provider.ExecErr = errors.New("sandbox unreachable")
	before := f.row().GitTokenExpiresAt
	revoked := f.forge.platform.Revoked()

	f.srv.svc.RenewGitTokens(context.Background())
	if after := f.row().GitTokenExpiresAt; !after.Equal(*before) {
		t.Errorf("a failed refresh moved the expiry to %v", after)
	}
	if f.forge.platform.Revoked() != revoked {
		t.Error("a token the sandbox may hold was revoked")
	}

	f.provider.ExecErr = nil
	f.srv.svc.RenewGitTokens(context.Background())
	if got := f.refreshes(); len(got) != 1 {
		t.Fatalf("a failed refresh was retried with no new activity: %d refreshes", len(got))
	}

	f.act("session.next.prompted", store.SourceOpenCode)
	f.srv.svc.RenewGitTokens(context.Background())
	if got := f.refreshes(); len(got) != 2 {
		t.Fatalf("new activity did not retry: %d refreshes", len(got))
	}
	if f.forge.platform.Revoked() != revoked+1 {
		t.Error("the held token was not revoked once a new one was written in")
	}
}

// A recorder marker written after real activity does not hide it.
//
// @decision(DL-021)
func TestAMarkerAfterActivityDoesNotHideIt(t *testing.T) {
	f := newRenewFixture(t)
	f.due(10 * time.Minute)
	f.act(MarkerSteered, store.SourceSpov)
	f.act(MarkerGap, store.SourceSpov)
	f.srv.svc.RenewGitTokens(context.Background())
	if got := f.refreshes(); len(got) != 1 {
		t.Fatalf("activity behind a gap marker was missed: %d refreshes", len(got))
	}
}

// With two renewable platforms, one that fails does not undo the other:
// the token written for the first is kept, and the row stays due.
//
// @decision(DL-021)
func TestOnePlatformFailingDoesNotUndoAnother(t *testing.T) {
	f := newAPIFixture(t)
	f.forge.platform.Renews = true
	f.forge.second = git.NewFake()
	f.forge.second.PlatformName, f.forge.second.HostName, f.forge.second.Renews = "other", "git.other.example", true
	id := f.create(repoReq("alpha"))
	f.events(id)
	r := &renewFixture{apiFixture: f, id: id}
	r.due(10 * time.Minute)
	r.act("session.next.prompted", store.SourceOpenCode)
	f.forge.second.IssueErr = errors.New("installation lost access")
	before := r.row().GitTokenExpiresAt
	revoked := f.forge.platform.Revoked()

	f.srv.svc.RenewGitTokens(context.Background())
	if got := r.refreshes(); len(got) != 1 || got[0][len(got[0])-3] != "git.example.com" {
		t.Fatalf("refreshes = %v, want the first platform's only", got)
	}
	if f.forge.platform.Revoked() != revoked {
		t.Error("the first platform's new token was revoked because the second failed")
	}
	if after := r.row().GitTokenExpiresAt; !after.Equal(*before) {
		t.Errorf("the expiry moved on although a platform failed: %v", after)
	}
}

// Each re-mint releases the one before it, and ending the session
// releases the last.
//
// @decision(DL-021)
func TestRemintedTokensAreReleased(t *testing.T) {
	f := newRenewFixture(t)
	for range 2 {
		f.due(10 * time.Minute)
		f.act("session.next.prompted", store.SourceOpenCode)
		f.srv.svc.RenewGitTokens(context.Background())
	}
	if got := f.refreshes(); len(got) != 2 {
		t.Fatalf("refreshes = %d, want 2", len(got))
	}
	revoked := f.forge.platform.Revoked()
	// Deleting revokes the launch token (the launcher's) and the last
	// re-minted one (the renewal's).
	if err := f.srv.svc.Delete(context.Background(), auth.Principal{UserID: "u", OrgID: auth.LocalID, Admin: true}, f.id); err != nil {
		t.Fatal(err)
	}
	if got := f.forge.platform.Revoked() - revoked; got != 2 {
		t.Errorf("ending the session revoked %d tokens, want the launch's and the last re-minted one", got)
	}
}

// With the proxy sidecar, a re-mint goes to the proxy on stdin, naming
// the platform, and nothing is written into the main container.
//
// @decision(DL-021) @decision(DL-034)
func TestARemintGoesToTheProxySidecar(t *testing.T) {
	f := newProxiedRenewFixture(t)
	f.due(10 * time.Minute)
	f.act("session.next.prompted", store.SourceOpenCode)

	f.srv.svc.RenewGitTokens(context.Background())

	if got := f.refreshes(); len(got) != 0 {
		t.Errorf("the main container was written to: %v", got)
	}
	execs := f.handle().SidecarExecs()
	if len(execs) != 1 {
		t.Fatalf("sidecar execs = %+v, want one", execs)
	}
	want := credproxy.SetTokenCommand(f.forge.platform.Name())
	if execs[0].Sidecar != credproxy.SidecarName || !slices.Equal(execs[0].Command, want) {
		t.Errorf("exec = %+v, want %v", execs[0], want)
	}
	if stdin := string(execs[0].Stdin); !strings.HasPrefix(stdin, "fake-secret-") || !strings.HasSuffix(stdin, "\n") {
		t.Errorf("stdin = %q, want the new token", stdin)
	}
	if row := f.row(); row.GitTokenExpiresAt == nil || time.Until(*row.GitTokenExpiresAt) < 50*time.Minute {
		t.Errorf("the expiry did not move on: %v", row.GitTokenExpiresAt)
	}
}

// newProxiedRenewFixture is newRenewFixture with the credential proxy.
func newProxiedRenewFixture(t *testing.T) *renewFixture {
	t.Helper()
	api := newAPIFixtureWith(t, func(d *Deps) {
		d.Launch = NewLaunch(&session.Launcher{
			Sandboxes:     d.Provider,
			NewClient:     func(string, string) session.HealthChecker { return stubHealth{} },
			HealthTimeout: 5 * time.Second,
			ProxyImage:    func(context.Context) (string, error) { return "spov-proxy-test", nil },
		})
	})
	api.forge.platform.Renews = true
	f := &renewFixture{apiFixture: api, id: api.create(repoReq("alpha"))}
	f.events(f.id)
	return f
}

// A proxied sandbox whose sidecar has died never gets its token written
// into the main container: the re-mint fails and its token is held.
//
// @decision(DL-034) @decision(DL-021)
func TestADeadProxyNeverSendsTheTokenToTheMainContainer(t *testing.T) {
	f := newProxiedRenewFixture(t)
	f.due(10 * time.Minute)
	f.act("session.next.prompted", store.SourceOpenCode)
	f.handle().KillSidecar()

	f.srv.svc.RenewGitTokens(context.Background())

	if got := f.refreshes(); len(got) != 0 {
		t.Errorf("the token was written into the main container: %v", got)
	}
	for _, in := range f.handle().ExecInputs() {
		if len(in) > 0 {
			t.Errorf("the main container was given %q on stdin", in)
		}
	}
	if row := f.row(); row.GitTokenExpiresAt != nil && time.Until(*row.GitTokenExpiresAt) > 50*time.Minute {
		t.Error("the expiry moved on although nothing was written")
	}
	if n := f.forge.platform.Revoked(); n != 0 {
		t.Errorf("revoked %d tokens; a failed write holds its token", n)
	}
}

// Where a token is held is recorded only when the launch knows it: an
// adopted sandbox's holder is left unsaid.
//
// @decision(DL-034)
func TestTheHolderIsRecordedOnlyWhenKnown(t *testing.T) {
	creds := []session.Credential{{Platform: "gh", Proxied: true, Forge: "github"}, {Platform: "fj", Forge: "forgejo"}}
	got := storedCredentials(creds, true)
	if got[0].Holder != store.CredentialInProxy || got[1].Holder != store.CredentialInSandbox || got[0].Forge != "github" {
		t.Errorf("launched: %+v", got)
	}
	for _, c := range storedCredentials(creds, false) {
		if c.Holder != "" {
			t.Errorf("adopted: %+v", c)
		}
	}
}

// The launch log says where each stored token went; a renewable one needs
// no line.
//
// @decision(DL-043) @decision(DL-034)
func TestTheLaunchLogSaysWhereAStoredTokenWent(t *testing.T) {
	log := newLaunchLog(time.Now)
	noteStoredTokens(log, []session.Credential{
		{Platform: "app", Renewable: true, Proxied: true},
		{Platform: "fj", Proxied: true},
		{Platform: "pat"},
		{Platform: "gh", Proxied: true, Forge: "github"},
	})
	var got []string
	for _, e := range log.events {
		got = append(got, e.(NoteEvent).Message)
	}
	if len(got) != 3 || got[0] != ProxiedTokenNote("fj", "", false) || got[1] != StoredTokenNote("pat") ||
		!strings.Contains(got[2], "GraphQL") || strings.Contains(got[0], "GraphQL") {
		t.Errorf("notes %q", got)
	}
}

// A failed renewal is noted in the session's log once per run of
// failures, the note is not activity, and a success starts a new run.
//
// @decision(DL-021)
func TestAFailedRenewalIsNotedOnceAndIsNotActivity(t *testing.T) {
	f := newRenewFixture(t)
	ctx := context.Background()
	notes := func() []store.Event {
		evs, err := f.store.Conversations().ListEvents(ctx, auth.LocalID, f.id, 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		var out []store.Event
		for _, e := range evs {
			if e.Name == MarkerRenewalFailed {
				out = append(out, e)
			}
		}
		return out
	}
	f.due(10 * time.Minute)
	f.act("session.next.prompted", store.SourceOpenCode)
	f.provider.ExecErr = errors.New("sandbox unreachable")
	f.srv.svc.RenewGitTokens(ctx)
	got := notes()
	if len(got) != 1 || got[0].Source != store.SourceSpov || !strings.Contains(got[0].Text, "Could not renew the git token") ||
		!strings.Contains(got[0].Text, "ends at") {
		t.Fatalf("notes after a failure: %+v", got)
	}

	// The note alone is no reason to try again.
	tried := len(f.refreshes())
	f.srv.svc.RenewGitTokens(ctx)
	if n := len(f.refreshes()); n != tried {
		t.Errorf("the note counted as activity: %d refreshes, then %d", tried, n)
	}
	f.act("session.next.prompted", store.SourceOpenCode)
	f.srv.svc.RenewGitTokens(ctx)
	if len(notes()) != 1 {
		t.Errorf("a second failure in the run was noted again: %d notes", len(notes()))
	}

	f.provider.ExecErr = nil
	f.act("session.next.prompted", store.SourceOpenCode)
	f.srv.svc.RenewGitTokens(ctx)
	f.due(10 * time.Minute)
	f.provider.ExecErr = errors.New("sandbox unreachable")
	f.act("session.next.prompted", store.SourceOpenCode)
	f.srv.svc.RenewGitTokens(ctx)
	if len(notes()) != 2 {
		t.Errorf("a failure after a success was not noted: %d notes", len(notes()))
	}
}
