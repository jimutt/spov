//go:build integration

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/opencode"
)

// Spike S-OI1: the synthetic messages the pinned OpenCode adds to a
// conversation by itself, which DL-180 tells apart from spov's own. A
// fake model runs a shell command in the background and reads a file in
// a directory holding its own AGENTS.md; then a person runs a command
// with the session's shell route and spov sends a synthetic of its own.
// The spike records every frame and the stored messages, and checks
// that each of OpenCode's carries metadata and spov's none. No Modal, no
// key.
//
//	SPOV_PROBE=1 go test -tags integration ./internal/server/ -run SOI1 -v
//
// SPOV_OI1_OUT=<dir> writes the frames and reads there, in S-SA1's
// layout, so TestWriteTheSubagentFixture turns them into a fixture.
//
// @decision(DL-180)
func TestIntegrationSOI1InjectedMessages(t *testing.T) {
	if os.Getenv("SPOV_PROBE") == "" && os.Getenv("SPOV_PROBE_OPENCODE") == "" {
		t.Skip("set SPOV_PROBE=1 to run the spike")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	work := t.TempDir()
	repo := filepath.Join(work, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"note.txt":  "hello\n",
		"AGENTS.md": "# Probe repository\n\nRun `./check.sh` before you finish.\n",
	} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	model := newInjectingModel(t, filepath.Join(repo, "note.txt"))
	conn := startProbeServerWith(ctx, t, probeBinary(ctx, t), model.URL, work, "")
	if _, err := opencode.NewClient(conn.URL, conn.Password).WaitHealthy(ctx, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	frames := captureFrames(ctx, t, conn)

	id, err := createConversation(ctx, conn, work)
	if err != nil {
		t.Fatal(err)
	}
	if err := setConversationModel(ctx, conn, id, "probe/m"); err != nil {
		t.Fatal(err)
	}
	run := &spikeRun{t: t, ctx: ctx, conn: conn, id: id, start: time.Now()}
	run.prompt("INJECT run the build in the background, then read the note", "steer", true)
	if !probeWait(ctx, func() bool { return model.saw("final") }) {
		t.Fatalf("the turn never ended:\n%s", model.log())
	}
	if !probeWait(ctx, func() bool { return frames.syntheticWith(id, "source") >= 1 }) {
		t.Fatalf("no shell result reached the conversation:\n%s", frames.summary())
	}
	if code, out := run.do(http.MethodPost, "/shell", map[string]any{"command": "echo by-hand; exit 2"}); code >= 300 {
		t.Fatalf("the shell route: %d %s", code, clip(string(out), 300))
	}
	run.synthetic("SPOV-OWN a continuation as spov sends one", "queue", false)
	// Neither starts a turn; a person's next message delivers both.
	turns := frames.count(id, "session.execution.succeeded")
	run.prompt("INJECT once more", "steer", true)
	if !probeWait(ctx, func() bool { return frames.count(id, "session.execution.succeeded") > turns }) {
		t.Fatalf("the last turn never ended:\n%s", frames.summary())
	}
	time.Sleep(time.Second)

	get := func(path string) json.RawMessage {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, conn.URL+path, nil)
		req.SetBasicAuth(conn.User, conn.Password)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return b
	}
	messages := get("/api/session/" + id + "/message?order=asc&limit=200")

	var listed struct {
		Data []struct {
			Type        string         `json:"type"`
			Text        string         `json:"text"`
			Description string         `json:"description"`
			Metadata    map[string]any `json:"metadata"`
		} `json:"data"`
	}
	if err := json.Unmarshal(messages, &listed); err != nil {
		t.Fatalf("the messages: %v %s", err, clip(string(messages), 300))
	}
	kinds := map[string]int{}
	for _, m := range listed.Data {
		if m.Type != "synthetic" {
			continue
		}
		t.Logf("synthetic: description=%q metadata=%v text=%q", m.Description, m.Metadata, clip(m.Text, 160))
		switch {
		case strings.HasPrefix(m.Text, "SPOV-OWN"):
			kinds["spov"]++
			if len(m.Metadata) != 0 || m.Description != "" {
				t.Errorf("spov's own synthetic came back with metadata %v or a description %q", m.Metadata, m.Description)
			}
		case m.Metadata["source"] == "shell":
			kinds["shell"]++
		case m.Metadata["instruction"] != nil:
			kinds["instructions"]++
		default:
			kinds["other"]++
		}
		if !strings.HasPrefix(m.Text, "SPOV-OWN") && len(m.Metadata) == 0 {
			t.Errorf("a synthetic spov did not send carries no metadata: %q", clip(m.Text, 200))
		}
	}
	t.Logf("synthetics by kind: %v", kinds)
	if kinds["shell"] < 2 || kinds["instructions"] < 1 || kinds["spov"] != 1 {
		t.Errorf("want two shell results, an instruction load and spov's own, got %v", kinds)
	}
	t.Logf("the shell tool's parameters: %v", model.shellParams())
	t.Logf("model requests:\n%s", model.log())
	t.Logf("frames:\n%s", frames.summary())

	if dir := os.Getenv("SPOV_OI1_OUT"); dir != "" {
		out := map[string]json.RawMessage{"messages " + id: messages}
		b, _ := json.MarshalIndent(out, "", " ")
		_ = os.WriteFile(filepath.Join(dir, "reads.json"), b, 0o600)
		_ = os.WriteFile(filepath.Join(dir, "frames.jsonl"), []byte(frames.jsonl()), 0o600)
		t.Logf("root: %s", id)
	}
}

// syntheticWith counts the session's inbox frames for a synthetic whose
// metadata has this key.
func (c *capturedFrames) syntheticWith(session, key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, f := range c.all {
		if f.Session != session || f.Name != "session.inbox.enqueued" {
			continue
		}
		var frame struct {
			Data struct {
				Item struct {
					Type    string `json:"type"`
					Payload struct {
						Metadata map[string]any `json:"metadata"`
					} `json:"payload"`
				} `json:"item"`
			} `json:"data"`
		}
		if json.Unmarshal(f.Raw, &frame) == nil && frame.Data.Item.Type == "synthetic" && frame.Data.Item.Payload.Metadata[key] != nil {
			n++
		}
	}
	return n
}

// injectingModel runs a background shell command on its first step,
// reads a file on its second and answers "final" after that.
type injectingModel struct {
	*httptest.Server
	mu     sync.Mutex
	reqs   []string
	params []string
}

func newInjectingModel(t *testing.T, readPath string) *injectingModel {
	t.Helper()
	m := &injectingModel{}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"data":[{"id":"m","object":"model"}]}`)
			return
		}
		var req struct {
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
			Tools []json.RawMessage `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		readParam := "filePath"
		for _, raw := range req.Tools {
			var def struct {
				Function struct {
					Name       string `json:"name"`
					Parameters struct {
						Properties map[string]json.RawMessage `json:"properties"`
					} `json:"parameters"`
				} `json:"function"`
			}
			if json.Unmarshal(raw, &def) != nil {
				continue
			}
			keys := slices.Sorted(maps.Keys(def.Function.Parameters.Properties))
			switch def.Function.Name {
			case "shell":
				m.mu.Lock()
				if m.params == nil {
					m.params = keys
				}
				m.mu.Unlock()
			case "read":
				if slices.Contains(keys, "path") {
					readParam = "path"
				}
			}
		}
		tools := 0
		for _, msg := range req.Messages {
			if msg.Role == "tool" {
				tools++
			}
		}
		tool, args, answer := "", "", "final"
		switch {
		case len(req.Tools) == 0:
			answer = "title"
		case tools == 0:
			tool, args = "shell", `{"command":"sleep 1; printf 'built\\n'; exit 3","background":true}`
		case tools == 1:
			b, _ := json.Marshal(map[string]string{readParam: readPath})
			tool, args = "read", string(b)
		}
		if tool != "" {
			answer = tool
		}
		m.mu.Lock()
		m.reqs = append(m.reqs, fmt.Sprintf("tools=%d -> %s", tools, answer))
		m.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		send := func(v any) {
			b, _ := json.Marshal(v)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
			if flusher != nil {
				flusher.Flush()
			}
		}
		chunk := func(delta map[string]any, finish any) {
			send(map[string]any{"id": "c", "object": "chat.completion.chunk", "created": 0, "model": "m",
				"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
		}
		if tool != "" {
			chunk(map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
				"index": 0, "id": fmt.Sprintf("call_%d", time.Now().UnixNano()), "type": "function",
				"function": map[string]any{"name": tool, "arguments": args},
			}}}, nil)
			chunk(map[string]any{}, "tool_calls")
		} else {
			chunk(map[string]any{"role": "assistant", "content": answer}, nil)
			chunk(map[string]any{}, "stop")
		}
		send(map[string]any{"id": "c", "object": "chat.completion.chunk", "created": 0, "model": "m", "choices": []any{},
			"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 2, "total_tokens": 7}})
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(m.Server.Close)
	return m
}

func (m *injectingModel) saw(answer string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.reqs {
		if strings.HasSuffix(r, "-> "+answer) {
			return true
		}
	}
	return false
}

func (m *injectingModel) shellParams() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.params
}

func (m *injectingModel) log() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.reqs, "\n")
}
