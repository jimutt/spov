package server

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/store"
)

// What a message means on a job, state by state, per DL-052's criterion.
//
// @decision(DL-052) @decision(DL-045)
func TestWhatAMessageMeansOnAJob(t *testing.T) {
	for _, tc := range []struct {
		state   store.JobState
		allowed bool
		why     string
	}{
		{store.JobRunning, true, "the ordinary case"},
		{store.JobSteered, true, "talking to a job you have taken over is the primary use"},
		{store.JobBlocked, false, "the agent is parked on a question and is not reading its inbox"},
		{store.JobQueued, false, "there is no sandbox"},
		{store.JobProvisioning, false, "the agent has not been given its goal"},
		{store.JobDelivering, false, "the run is over"},
		{store.JobSucceeded, false, "it has finished"},
		{store.JobFailed, false, "it has finished"},
		{store.JobCancelled, false, "it has finished"},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			f := newSteerFixtureAt(t, tc.state, "ses_1")
			_, err := f.svc.Steer(context.Background(), f.p, f.sess.ID, SteerRequest{Text: "hello"})
			switch {
			case tc.allowed && err != nil:
				t.Fatalf("a message to a %s job was refused (%s): %v", tc.state, tc.why, err)
			case !tc.allowed && !errors.Is(err, ErrJobNotSteerable):
				t.Fatalf("a message to a %s job was not refused (%s): %v", tc.state, tc.why, err)
			}
			if tc.allowed {
				return
			}
			// The route is about the SESSION and the refusal is about
			// the JOB, so the message has to name the job's state or a
			// reader cannot tell which of their two things refused.
			if !strings.Contains(err.Error(), string(tc.state)) {
				t.Errorf("the refusal says %q and never names the job's state", err)
			}
			if n := len(f.calls()); n != 0 {
				t.Errorf("the sandbox saw %d prompts from a %s job", n, tc.state)
			}
		})
	}
}

// Every state the machine has is decided here, one way or the other. A
// state added and not named in jobRefusesAMessage silently becomes
// steerable, which is the direction that matters.
//
// @decision(DL-052) @decision(DL-045)
func TestEveryJobStateIsDecided(t *testing.T) {
	decided := map[store.JobState]bool{
		store.JobRunning: true, store.JobSteered: true,
	}
	all := store.JobStates()
	if len(all) == 0 {
		t.Fatal("the machine reports no states at all, so this test is watching nothing")
	}
	for _, s := range all {
		if _, refused := jobRefusesAMessage(s); refused {
			decided[s] = true
		}
	}
	for _, s := range all {
		if !decided[s] {
			t.Errorf("job state %q is neither refused nor named as steerable, so it is"+
				" steerable by omission", s)
		}
	}
}

// The refusals and the machine are the same fact in two places, cross-
// checked here rather than derived — deriving jobRefusesAMessage from
// CanTransition would make the semantic rule follow a structural table,
// the shortcut DL-045 declined for abandonedState.
//
// @decision(DL-052) @decision(DL-045)
func TestTheRefusalsAreTheEdgesTheMachineHas(t *testing.T) {
	all := store.JobStates()
	if len(all) == 0 {
		t.Fatal("the machine reports no states at all, so this test is watching nothing")
	}
	for _, s := range all {
		_, refused := jobRefusesAMessage(s)
		// A message is admitted exactly where the takeover it causes is
		// a move the machine has.
		steerable := store.CanTransition(s, store.JobSteered)
		if refused == steerable {
			t.Errorf("%s: the refusal says steerable=%t and the machine says %t;"+
				" these are the same fact and one of them has moved",
				s, !refused, steerable)
		}
	}
}

// A job takes a steered or a queued message, and every message to it
// carries what follows it in the prompt's metadata, continue unless it
// says otherwise. An interactive session's carries none.
//
// @decision(DL-019) @decision(DL-052)
func TestAJobMessageSaysWhatFollowsIt(t *testing.T) {
	f := newSteerFixture(t, "ses_1")
	ctx := context.Background()
	after := func(c prompted) any {
		meta, _ := c.body["metadata"].(map[string]any)
		spov, _ := meta["spov"].(map[string]any)
		return spov["after"]
	}

	sent, err := f.svc.Steer(ctx, f.p, f.sess.ID, SteerRequest{Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if sent.Delivery != DeliverySteer || sent.After != AfterContinue {
		t.Errorf("sent = %+v, want a steered message that continues", sent)
	}
	calls := f.calls()
	if len(calls) != 1 || calls[0].body["delivery"] != DeliverySteer || after(calls[0]) != AfterContinue {
		t.Errorf("the sandbox was sent %+v", calls)
	}
	if calls[0].body["text"] != "hello" {
		t.Errorf("text = %#v; the choice goes in the metadata, never the text", calls[0].body["text"])
	}

	if _, err := f.svc.Steer(ctx, f.p, f.sess.ID, SteerRequest{Text: "over to me", After: AfterHandover, Delivery: DeliveryQueue}); err != nil {
		t.Fatal(err)
	}
	if calls := f.calls(); after(calls[1]) != AfterHandover || calls[1].body["delivery"] != DeliveryQueue {
		t.Errorf("a queued handover was sent as %+v", calls[1].body)
	}

	if _, err := f.svc.Steer(ctx, f.p, f.sess.ID, SteerRequest{Text: "x", After: "later"}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("an unknown after: err = %v", err)
	}

	inter := interactiveFixture(t, f, "ses_i")
	got, err := f.svc.Steer(ctx, f.p, inter.ID, SteerRequest{Text: "hello", After: AfterHandover})
	if err != nil {
		t.Fatalf("an interactive session was refused a message: %v", err)
	}
	calls = f.calls()
	if got.After != "" || calls[len(calls)-1].body["metadata"] != nil {
		t.Errorf("an interactive session's message carried %+v, %v", got, calls[len(calls)-1].body["metadata"])
	}
}

// enqueueOn stores the sandbox's enqueue of one item in a session's
// only conversation, as the recorder would.
func enqueueOn(t *testing.T, f *steerFix, id, itemType string) {
	t.Helper()
	ctx := context.Background()
	convs, err := f.svc.deps.Store.Conversations().ListConversations(ctx, f.sess.OrgID, f.sess.ID)
	if err != nil || len(convs) == 0 {
		t.Fatalf("no conversation: %v", err)
	}
	raw := `{"type":"session.inbox.enqueued","data":{"inboxID":"` + id + `","item":{"type":"` + itemType + `","payload":{"text":"x"},"delivery":"queue"}}}`
	if _, err := f.svc.deps.Store.Conversations().PutEvents(ctx, []store.Event{{
		ID: store.NewID(f.svc.deps.Now()), OrgID: f.sess.OrgID, SessionID: f.sess.ID, ConversationID: convs[0].ID,
		Source: store.SourceOpenCode, Seq: int64(len(id)) + int64(len(itemType)), Name: jobs.EventInboxEnqueued,
		At: f.svc.deps.Now(), Raw: []byte(raw),
	}}); err != nil {
		t.Fatal(err)
	}
}

// Cancelling relays OpenCode's inbox DELETE for the conversation and
// leaves an audit marker; the sandbox's answer says nothing more. Only a
// person's own stored message can be named.
//
// @decision(DL-019)
func TestCancellingAMessageRelaysTheInboxDelete(t *testing.T) {
	f := newSteerFixture(t, "ses_1")
	enqueueOn(t, f, "msg_queued", "user")
	enqueueOn(t, f, "msg_spovs", "synthetic")
	got, err := f.svc.CancelMessage(context.Background(), f.p, f.sess.ID, "msg_queued", "")
	if err != nil {
		t.Fatal(err)
	}
	calls := f.calls()
	if len(calls) != 1 || calls[0].method != http.MethodDelete || !strings.HasSuffix(calls[0].path, "/api/session/ses_1/inbox/msg_queued") {
		t.Fatalf("the sandbox saw %+v", calls)
	}
	if got.ID != "msg_queued" || got.Conversation != "ses_1" {
		t.Errorf("cancelled = %+v", got)
	}
	if marks := markersOn(t, f.svc, f.sess, MarkerMessageCancelled); len(marks) != 1 {
		t.Errorf("%d cancel markers, want 1", len(marks))
	}
	for _, id := range []string{"msg_spovs", "msg_unknown"} {
		if _, err := f.svc.CancelMessage(context.Background(), f.p, f.sess.ID, id, ""); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s: err = %v, want not found", id, err)
		}
	}
	if _, err := f.svc.CancelMessage(context.Background(), f.p, f.sess.ID, "evt_1", ""); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("a non-message id: err = %v", err)
	}
	if n := len(f.calls()); n != 1 {
		t.Errorf("a refused cancel reached the sandbox: %d requests", n)
	}
}

// Cancels take from the same per-session ceiling as sends.
//
// @decision(DL-019)
func TestCancellingIsRateLimitedLikeSending(t *testing.T) {
	f := newSteerFixture(t, "ses_1")
	enqueueOn(t, f, "msg_queued", "user")
	var err error
	for i := 0; i <= MessagesPerMinute && err == nil; i++ {
		_, err = f.svc.CancelMessage(context.Background(), f.p, f.sess.ID, "msg_queued", "")
	}
	if !errors.Is(err, ErrTooManyMessages) {
		t.Errorf("err = %v, want the message ceiling", err)
	}
}

// A refusal is a state conflict over the wire, not a bad request: the
// job exists, the caller may see it, and nothing about what they sent
// is wrong. It is the reading a resume of an unsuspended job gets.
//
// @decision(DL-052) @decision(DL-054)
func TestAJobRefusalIsAConflict(t *testing.T) {
	f := newAPIFixture(t)
	sess := runningSession(t, f)
	seedConversation(t, f, sess, "ses_1", "one")
	job := store.Job{
		ID: store.NewID(f.srv.deps.Now()), OrgID: sess.OrgID, OwnerID: sess.OwnerID,
		SessionID: sess.ID, Kind: store.JobKindReport, State: store.JobQueued, Goal: "g",
	}
	if err := f.store.Jobs().Create(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	res, body := f.do(http.MethodPost, "/api/v1/sessions/"+sess.ID+"/messages",
		SteerRequest{Text: "hello"})
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("a message to a queued job = %d %s, want 409", res.StatusCode, body)
	}
	if !strings.Contains(string(body), string(store.JobQueued)) {
		t.Errorf("the refusal does not name the job's state: %s", body)
	}
}

// A job refusal costs nothing: not a message from the session's
// ceiling, not a mint, and not a request to the sandbox. The check sits
// ahead of both bounds so a refusal reads as the job's state rather than
// as a 429 about rate limits.
//
// @decision(DL-052) @decision(DL-019)
func TestAJobRefusalCostsNothing(t *testing.T) {
	f := newSteerFixtureAt(t, store.JobBlocked, "ses_1")
	ctx := context.Background()
	for range MessagesPerMinute + 2 {
		if _, err := f.svc.Steer(ctx, f.p, f.sess.ID, SteerRequest{Text: "hello"}); !errors.Is(err, ErrJobNotSteerable) {
			t.Fatalf("err = %v, want the job's own refusal", err)
		}
	}
	if n := mintsOf(f.fake); n != 0 {
		t.Errorf("refusals spent %d mints", n)
	}
	if n := len(f.calls()); n != 0 {
		t.Errorf("refusals reached the sandbox %d times", n)
	}
	// Released, the session still has its whole ceiling.
	setJobState(t, f.svc, f.job, store.JobRunning)
	for i := range MessagesPerMinute {
		if _, err := f.svc.Steer(ctx, f.p, f.sess.ID, SteerRequest{Text: "hello"}); err != nil {
			t.Fatalf("message %d of %d was refused, so the refusals above spent the ceiling: %v",
				i+1, MessagesPerMinute, err)
		}
	}
}

// A jobs store that cannot be read is neither a refusal nor a pass;
// see DL-013's posture on failing open versus blaming the caller.
//
// @decision(DL-052) @decision(DL-013)
func TestAnUnreadableJobIsNeitherRefusedNorAdmitted(t *testing.T) {
	f := newSteerFixture(t, "ses_1")
	real := f.svc.deps.Store
	f.svc.deps.Store = jobsFail{Store: real}
	_, err := f.svc.Steer(context.Background(), f.p, f.sess.ID, SteerRequest{Text: "hello"})
	switch {
	case err == nil:
		t.Fatal("a message went through while the job could not be read")
	case errors.Is(err, ErrJobNotSteerable), errors.Is(err, ErrInvalidRequest):
		t.Fatalf("spov's own outage was reported as the caller's fault: %v", err)
	}
	if n := len(f.calls()); n != 0 {
		t.Errorf("the sandbox saw %d prompts while the job could not be read", n)
	}
}

type jobsFail struct{ store.Store }

func (s jobsFail) Jobs() store.JobStore { return failingJobs{s.Store.Jobs()} }

type failingJobs struct{ store.JobStore }

func (failingJobs) BySession(context.Context, string, string) (store.Job, error) {
	return store.Job{}, errors.New("the jobs table is unreachable")
}

// DL-045's invariant, narrowed by DL-052: nothing spov generates is ever
// a `user` inbox item. This is the origin test — it reads the package's
// own source and checks that the prompt endpoint is addressed from
// exactly one place, and that the text sent there is the caller's own.
//
// @decision(DL-052) @decision(DL-045)
func TestOnlyAHumansOwnWordsReachThePromptEndpoint(t *testing.T) {
	const promptRoute = "/prompt"
	// The WHOLE tree, not this package: what the rule forbids is a path
	// nobody has written yet, and a walk of internal/server could not
	// see one in internal/jobs or internal/cli. Files are read directly
	// rather than through parser.ParseDir, which is deprecated for
	// ignoring build tags — and here ignoring them is what is wanted,
	// since a tagged file is still a file that could address the route.
	fset := token.NewFileSet()
	var sites []string
	err := filepath.WalkDir("../..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", "web", "dist":
				return fs.SkipDir
			}
			// .git, and .claude, which can hold whole worktrees of
			// this repository.
			if name := d.Name(); strings.HasPrefix(name, ".") && name != "." && name != ".." {
				return fs.SkipDir
			}
			// The contract package lists the route among those spov
			// sends, and sends nothing itself (DL-035).
			if filepath.ToSlash(path) == "../../internal/opencode/contract" {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		// Test files are excluded: the integration spikes address this
		// route deliberately, which is how it was measured.
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parsing %s: %w", path, perr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, uerr := strconv.Unquote(lit.Value)
			if uerr != nil || !strings.Contains(v, promptRoute) {
				return true
			}
			sites = append(sites, filepath.Base(path))
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) == 0 {
		t.Fatal("no file addresses the prompt route at all, so this test is watching nothing")
	}
	if len(sites) != 1 || sites[0] != "steer.go" {
		t.Errorf("the prompt endpoint is addressed from %v, want steer.go alone."+
			" It is the route that produces a `user` inbox item, and DL-045's"+
			" invariant is that nothing spov generates is ever one — a second"+
			" caller is spov speaking as the person, which steers every job on"+
			" its own opening turn.", sites)
	}

	// And what goes there is the caller's text, unadorned. An opening
	// goal is composed by openingText and posted to /synthetic; a
	// preamble added here would be spov's words inside a `user` item.
	f := newSteerFixture(t, "ses_1")
	const typed = "stop and summarise what you have"
	if _, err := f.svc.Steer(context.Background(), f.p, f.sess.ID, SteerRequest{Text: typed}); err != nil {
		t.Fatal(err)
	}
	calls := f.calls()
	if len(calls) != 1 || calls[0].body["text"] != typed {
		t.Errorf("the sandbox was sent %#v, want the caller's own words and nothing else",
			calls[0].body["text"])
	}
}
