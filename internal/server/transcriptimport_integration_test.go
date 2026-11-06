//go:build integration

package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/store"
)

// Spike S-TR1 asks whether transcript resume needs the old sandbox at
// all: can a conversation be rebuilt in a fresh OpenCode from what the
// recorder stores (each message's Raw, as GET /message returns it), and
// does the model then see that history on the next turn? It runs two
// servers of the pinned build on this machine with a recording fake
// model: no Modal, no key.
//
//	SPOV_PROBE=1 go test -tags integration ./internal/server/ -run STR1 -v
//
// SPOV_PROBE_OPENCODE names the binary when it is not where spov looks.
func TestIntegrationSTR1TranscriptImport(t *testing.T) {
	if os.Getenv("SPOV_PROBE") == "" && os.Getenv("SPOV_PROBE_OPENCODE") == "" {
		t.Skip("set SPOV_PROBE=1 to run the local OpenCode probe")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	bin := probeBinary(ctx, t)
	model := newRecordingModel(t)

	// The first server: one turn that says a word only it knows.
	workA := t.TempDir()
	a := startProbeServerWith(ctx, t, bin, model.URL, workA, "")
	if _, err := opencode.NewClient(a.URL, a.Password).WaitHealthy(ctx, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	id, err := createConversation(ctx, a, workA)
	if err != nil {
		t.Fatal(err)
	}
	if err := setConversationModel(ctx, a, id, "probe/m"); err != nil {
		t.Fatal(err)
	}
	seqA := watchSeqs(ctx, t, a, id)
	body, _ := json.Marshal(steerBody(MessageIDPrefix+"str1a", "Remember the word PINEAPPLE.", "steer", ""))
	if _, err := sandboxPostJSON(ctx, a, promptURL(a, id), body); err != nil {
		t.Fatalf("prompting: %v", err)
	}
	if !probeWait(ctx, func() bool { return countRole(probeMessages(ctx, t, a, id), "assistant") > 0 }) {
		t.Fatalf("no answer arrived: %s", probeRoles(probeMessages(ctx, t, a, id)))
	}
	stored := probeMessages(ctx, t, a, id)
	info, err := sandboxGet(ctx, a.URL+"/api/session/"+url.PathEscape(id), a.Token, a.User, a.Password)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the first server holds %d messages: %s", len(stored), probeRoles(stored))

	// The second server, a fresh home: the conversation is imported from
	// the stored messages alone, as the store keeps them.
	workB := t.TempDir()
	b := startProbeServerWith(ctx, t, bin, model.URL, workB, "")
	if _, err := opencode.NewClient(b.URL, b.Password).WaitHealthy(ctx, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	var sess map[string]any
	if err := json.Unmarshal(info, &sess); err != nil {
		t.Fatal(err)
	}
	if data, ok := sess["data"].(map[string]any); ok {
		sess = data
	}
	messages := make([]json.RawMessage, 0, len(stored))
	for i := len(stored) - 1; i >= 0; i-- { // the listing is newest first
		messages = append(messages, stored[i].Raw)
	}
	imp, _ := json.Marshal(map[string]any{"info": sess, "messages": messages})
	answer, err := sandboxPostJSON(ctx, b, b.URL+"/api/experimental/session/import", imp)
	t.Logf("import answered %s, %v", trimTo(string(answer), 400), err)
	if err != nil {
		t.Fatalf("the import was refused: %v", err)
	}
	var imported struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(answer, &imported)
	newID := firstNonEmptyString(imported.Data.ID, id)
	again := probeMessages(ctx, t, b, newID)
	t.Logf("the second server holds %d messages under %s: %s", len(again), newID, probeRoles(again))
	if len(again) != len(stored) {
		t.Errorf("the import kept %d of %d messages", len(again), len(stored))
	}

	// A turn on the second server: the model must be sent the first
	// server's history.
	model.reset()
	if err := setConversationModel(ctx, b, newID, "probe/m"); err != nil {
		t.Fatal(err)
	}
	seqB := watchSeqs(ctx, t, b, newID)
	body, _ = json.Marshal(steerBody(MessageIDPrefix+"str1b", "Which word did I ask you to remember?", "steer", ""))
	if _, err := sandboxPostJSON(ctx, b, promptURL(b, newID), body); err != nil {
		t.Fatalf("prompting the imported conversation: %v", err)
	}
	if !probeWait(ctx, func() bool { return model.calls() > 0 }) {
		t.Fatal("the imported conversation never called the model")
	}
	if !strings.Contains(model.last(), "PINEAPPLE") {
		t.Errorf("the model was not sent the imported history: %s", trimTo(model.last(), 600))
	} else {
		t.Log("the model was sent the imported history on the next turn")
	}
	// The recorder keys a conversation's durable frames by seq and
	// marks a gap against the last one stored (DL-017): does the
	// imported conversation's log continue the first server's, or
	// start again?
	probeWait(ctx, func() bool { return countRole(probeMessages(ctx, t, b, newID), "assistant") > 1 })
	time.Sleep(time.Second)
	loA, hiA := seqA()
	loB, hiB := seqB()
	t.Logf("durable seq: the first server's turn %d..%d; the imported conversation's next turn %d..%d", loA, hiA, loB, hiB)
	if loB <= hiA {
		t.Logf("the imported conversation's durable log starts again: seq %d after %d, so stored frames would collide (DL-017)", loB, hiA)
	}
}

// watchSeqs follows conn's event stream and reports the lowest and
// highest durable seq seen on conversation id.
func watchSeqs(ctx context.Context, t *testing.T, conn Connection, id string) func() (int64, int64) {
	t.Helper()
	body, err := openSandboxEvents(ctx, conn)
	if err != nil {
		t.Fatalf("event stream: %v", err)
	}
	t.Cleanup(func() { _ = body.Close() })
	var mu sync.Mutex
	var lo, hi int64
	go func() {
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			line, ok := strings.CutPrefix(sc.Text(), "data: ")
			if !ok || !strings.Contains(line, id) {
				continue
			}
			var frame map[string]any
			if json.Unmarshal([]byte(line), &frame) != nil {
				continue
			}
			if seq := durableSeq(frame); seq > 0 {
				mu.Lock()
				if lo == 0 || seq < lo {
					lo = seq
				}
				hi = max(hi, seq)
				mu.Unlock()
			}
		}
	}()
	return func() (int64, int64) { mu.Lock(); defer mu.Unlock(); return lo, hi }
}

func countRole(msgs []store.Message, role string) int {
	n := 0
	for _, m := range msgs {
		if m.Role == role {
			n++
		}
	}
	return n
}

func trimTo(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func firstNonEmptyString(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// recordingModel answers every chat request "ok" and keeps the last
// request's messages.
type recordingModel struct {
	*httptest.Server
	mu    sync.Mutex
	n     int
	saved string
}

func newRecordingModel(t *testing.T) *recordingModel {
	t.Helper()
	m := &recordingModel{}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"data":[{"id":"m","object":"model"}]}`)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.n++
		m.saved = string(raw)
		m.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range []string{
			`{"id":"c","object":"chat.completion.chunk","created":0,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`,
			`{"id":"c","object":"chat.completion.chunk","created":0,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`{"id":"c","object":"chat.completion.chunk","created":0,"model":"m","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
		} {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(m.Close)
	return m
}

func (m *recordingModel) reset() { m.mu.Lock(); m.n, m.saved = 0, ""; m.mu.Unlock() }
func (m *recordingModel) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.n
}
func (m *recordingModel) last() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saved
}
