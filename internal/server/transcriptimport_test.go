package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/opencode/contract"
	"github.com/jimutt/spov/internal/session"
	"github.com/jimutt/spov/internal/store"
)

// An import sends the stored info and messages whole, then reads the
// conversation's log head to know where the import's own frames end.
//
// @decision(DL-089)
func TestAnImportSendsTheStoredConversationAndReadsItsHead(t *testing.T) {
	var got struct {
		Info     json.RawMessage   `json:"info"`
		Messages []json.RawMessage `json:"messages"`
	}
	sb := httptest.NewServer(contract.Checked(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/experimental/session/import":
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &got); err != nil {
				t.Errorf("the import body: %v", err)
			}
			_, _ = io.WriteString(w, `{"data":{"id":"ses_1"}}`)
		case r.URL.Path == "/api/experimental/session/ses_1/log":
			if r.URL.Query().Get("follow") != "false" {
				t.Errorf("the head read follows: %s", r.URL.RawQuery)
			}
			_, _ = io.WriteString(w, "data: {\"type\":\"log.synced\",\"aggregateID\":\"ses_1\",\"seq\":4}\n\n")
		default:
			http.NotFound(w, r)
		}
	})))
	t.Cleanup(sb.Close)
	conn := Connection{URL: sb.URL, User: opencode.BasicAuthUser, Password: "pw"}
	at := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	info := builtInfo(store.Conversation{OpenCodeID: "ses_1", Title: "t", CreatedAt: at, UpdatedAt: at}, "")
	head, err := importConversation(context.Background(), conn, info,
		[]json.RawMessage{json.RawMessage(`{"id":"msg_1","time":{"created":1788266065984},"text":"hi","type":"user"}`),
			json.RawMessage(`{"id":"msg_2","time":{"created":1788266066749},"text":"and then","type":"user"}`)})
	if err != nil || head != 4 {
		t.Fatalf("head %d, %v", head, err)
	}
	if string(got.Info) != string(info) || len(got.Messages) != 2 || !strings.Contains(string(got.Messages[1]), `"msg_2"`) {
		t.Errorf("sent %s %s", got.Info, got.Messages)
	}
	if _, err := importConversation(context.Background(), conn, []byte(`{}`), nil); err == nil {
		t.Error("an info with no id was imported")
	}
}

// The head is read from any of the log's shapes, and a built info
// carries what an import requires.
//
// @decision(DL-089)
func TestLogFramesAndBuiltInfo(t *testing.T) {
	for _, body := range []string{
		`[{"durable":{"seq":2}},{"durable":{"seq":7}}]`,
		`{"data":[{"durable":{"seq":7}}]}`,
		"data: {\"durable\":{\"seq\":7}}\n\n",
	} {
		var head int64
		for _, f := range logFrames([]byte(body)) {
			head = max(head, durableSeq(f))
		}
		if head != 7 {
			t.Errorf("%s: head %d", body, head)
		}
	}
	at := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	var info map[string]any
	if err := json.Unmarshal(builtInfo(store.Conversation{OpenCodeID: "ses_9", Title: "x", CreatedAt: at, UpdatedAt: at}, ""), &info); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"id", "projectID", "cost", "tokens", "time", "location"} {
		if _, ok := info[k]; !ok {
			t.Errorf("the built info lacks %s: %v", k, info)
		}
	}
}

// An imported conversation's frames are mapped onto its stored
// sequence: the import's echo is skipped, and later frames continue
// where the store stopped, with no gap marked.
//
// @decision(DL-089) @decision(DL-017)
func TestTheRecorderContinuesAnImportedConversation(t *testing.T) {
	f := newRecorderFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conv, err := f.st.Conversations().UpsertConversation(ctx, store.Conversation{
		ID: store.NewID(time.Now()), OrgID: "org", SessionID: f.sess.ID, OpenCodeID: "ses_1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Conversations().PutEvents(ctx, []store.Event{{
		ID: store.NewID(time.Now()), OrgID: "org", SessionID: f.sess.ID, ConversationID: conv.ID,
		Source: store.SourceOpenCode, Seq: 13, Name: "session.step.started", At: time.Now(),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.Conversations().SetSeqOffset(ctx, "org", conv.ID, 13, 4); err != nil {
		t.Fatal(err)
	}
	f.sb.listing = `{"data":[{"id":"ses_1","title":"t"}]}`
	f.sb.messages["ses_1"] = twoMessages
	if err := f.rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the catch-up", func() bool { return len(f.messages()) == 2 })
	f.sb.stream <- durableFrameFor("session.step.started", "ses_1", 4, "")
	f.sb.stream <- durableFrameFor("session.step.started", "ses_1", 5, "")
	f.sb.stream <- durableFrameFor("session.step.started", "ses_1", 6, "")
	waitFor(t, "two new rows", func() bool { return len(f.events()) == 3 })
	var seqs []int64
	for _, ev := range f.events() {
		if ev.Name == MarkerGap {
			t.Errorf("a gap was marked: %+v", ev)
		}
		seqs = append(seqs, ev.Seq)
	}
	if len(seqs) != 3 || seqs[1] != 14 || seqs[2] != 15 {
		t.Errorf("stored seqs %v, want 13 14 15", seqs)
	}
}

// A transcript resume imports each stored conversation, building an
// info for one that has none, records where its log continues, and
// notes one that fails.
//
// @decision(DL-089)
func TestImportConversationsRecordsEachOffset(t *testing.T) {
	var imported []string
	sb := httptest.NewServer(contract.Checked(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var body struct {
				Info struct {
					ID string `json:"id"`
				} `json:"info"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			if body.Info.ID == "ses_bad" {
				http.Error(w, `{"title":"no"}`, http.StatusBadRequest)
				return
			}
			imported = append(imported, body.Info.ID)
			_, _ = io.WriteString(w, `{"data":{"id":"`+body.Info.ID+`"}}`)
		default:
			_, _ = io.WriteString(w, "data: {\"type\":\"log.synced\",\"seq\":3}\n\n")
		}
	})))
	t.Cleanup(sb.Close)
	f := newAPIFixture(t)
	sess := runningSession(t, f)
	ctx := context.Background()
	at := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	for _, id := range []string{"ses_kept", "ses_built", "ses_bad"} {
		c := store.Conversation{ID: store.NewID(time.Now()), OrgID: sess.OrgID, SessionID: sess.ID, OpenCodeID: id}
		if id == "ses_kept" {
			c.Info = builtInfo(store.Conversation{OpenCodeID: id, CreatedAt: at, UpdatedAt: at}, "")
		}
		conv, err := f.store.Conversations().UpsertConversation(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.Conversations().PutMessages(ctx, []store.Message{{
			ID: store.NewID(time.Now()), OrgID: sess.OrgID, SessionID: sess.ID, ConversationID: conv.ID,
			OpenCodeID: "msg_" + id, Role: "user", Text: "hi",
			Raw: []byte(`{"id":"msg_` + id + `","time":{"created":1788266065984},"text":"hi","type":"user"}`),
		}}); err != nil {
			t.Fatal(err)
		}
	}
	live := &session.Session{URL: sb.URL, Password: "pw"}
	f.srv.svc.importConversations(ctx, sess, live, newLaunchLog(time.Now))
	if strings.Join(imported, " ") != "ses_kept ses_built" {
		t.Errorf("imported %v", imported)
	}
	convs, _ := f.store.Conversations().ListConversations(ctx, sess.OrgID, sess.ID)
	for _, c := range convs {
		if want := int64(3); c.OpenCodeID != "ses_bad" && c.SeqAfter != want {
			t.Errorf("%s: after %d", c.OpenCodeID, c.SeqAfter)
		}
	}
	evs, _ := f.store.Conversations().ListEvents(ctx, sess.OrgID, sess.ID, 0, 0)
	failed := 0
	for _, ev := range evs {
		if ev.Name == MarkerImportFailed {
			failed++
		}
	}
	if failed != 1 {
		t.Errorf("%d import notes", failed)
	}
}
