package server

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/store"
)

// TestWriteTheSubagentFixture writes the conversations and log the API
// would serve for one root of a spike S-SA1 capture and its child, by
// passing the captured frames through the real recorder into a memory
// store and reading them back with the page's own views. It is a tool,
// not a test: web/src/lib/subagent.fixture.ts is made from its output,
// with only the fake model's words rewritten.
//
//	SPOV_SA1_OUT=<dir> SPOV_PROBE=1 go test -tags integration ./internal/server/ -run SSA1 -v
//	SPOV_SA1_FIXTURE=<dir> SPOV_SA1_ROOT=<ses_…> go test ./internal/server/ -run TestWriteTheSubagentFixture -v
//
// It writes <dir>/fixture.json.
//
// @decision(DL-112) @decision(DL-144)
func TestWriteTheSubagentFixture(t *testing.T) {
	dir, root := os.Getenv("SPOV_SA1_FIXTURE"), os.Getenv("SPOV_SA1_ROOT")
	if dir == "" || root == "" {
		t.Skip("set SPOV_SA1_FIXTURE to an S-SA1 capture and SPOV_SA1_ROOT to one of its roots")
	}
	var reads map[string]json.RawMessage
	b, err := os.ReadFile(filepath.Join(dir, "reads.json"))
	if err != nil || json.Unmarshal(b, &reads) != nil {
		t.Fatalf("reading reads.json: %v", err)
	}
	type captured struct {
		Session string          `json:"session"`
		Raw     json.RawMessage `json:"raw"`
	}
	var frames []captured
	created := map[string]bool{root: true}
	f, err := os.Open(filepath.Join(dir, "frames.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var c captured
		if json.Unmarshal(sc.Bytes(), &c) != nil {
			continue
		}
		var frame map[string]any
		_ = json.Unmarshal(c.Raw, &frame)
		if parent, ok := createdParent(frame); ok && created[parent] {
			created[c.Session] = true
		}
		if created[c.Session] {
			frames = append(frames, c)
		}
	}

	rf := newRecorderFixture(t)
	for id := range created {
		rf.sb.messages[id] = string(reads["messages "+id])
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rf.rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "attach", func() bool { _, _, s, _ := rf.sb.counts(); return s == 1 })
	for _, c := range frames {
		rf.sb.send(string(c.Raw))
	}
	stored := 0
	waitFor(t, "the frames to settle", func() bool {
		n := len(rf.events())
		settled := n == stored && n > 0
		stored = n
		time.Sleep(50 * time.Millisecond)
		return settled
	})

	s := &Server{deps: Deps{Store: rf.st}}
	convs, _, _, err := s.transcript(ctx, rf.sess)
	if err != nil {
		t.Fatal(err)
	}
	names := &conversationNames{byID: map[string]string{}, list: func(ctx context.Context) ([]store.Conversation, error) {
		return rf.st.Conversations().ListConversations(ctx, rf.sess.OrgID, rf.sess.ID)
	}}
	var log []LogEvent
	for _, ev := range rf.events() {
		if ev.Source != store.SourceOpenCode && ev.Source != store.SourceEphemeral {
			continue
		}
		e := s.logEvent(ctx, names, ev)
		// The recorder stamps an event when it reads it; the capture's
		// own time is the one a live recording would have had.
		var at struct {
			Created int64 `json:"created"`
		}
		if json.Unmarshal(ev.Raw, &at) == nil && at.Created > 0 {
			e.At = time.UnixMilli(at.Created).UTC()
		}
		log = append(log, e)
	}
	out, _ := json.MarshalIndent(map[string]any{"conversations": convs, "log": log}, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "fixture.json"), out, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d conversations, %d events", len(convs), len(log))
}
