//go:build integration

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/opencode"
)

// Spike S-SA1: how the pinned OpenCode shows a subagent, which is what
// DL-110 to DL-115 rely on. A fake model makes a root conversation call
// the `subagent` tool, in the foreground and in the background, and each
// child run one glob. The spike records every frame, then reads the
// session listing and each session's info and messages. No Modal, no key.
//
//	SPOV_PROBE=1 go test -tags integration ./internal/server/ -run SSA1 -v
//
// SPOV_SA1_OUT=<dir> writes the frames and reads there, as fixtures.
//
// @decision(DL-110)
func TestIntegrationSSA1Subagents(t *testing.T) {
	if os.Getenv("SPOV_PROBE") == "" && os.Getenv("SPOV_PROBE_OPENCODE") == "" {
		t.Skip("set SPOV_PROBE=1 to run the spike")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	model := newSubagentModel(t)
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "note.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	conn := startProbeServerWith(ctx, t, probeBinary(ctx, t), model.URL, work, "")
	if _, err := opencode.NewClient(conn.URL, conn.Password).WaitHealthy(ctx, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	frames := captureFrames(ctx, t, conn)

	open := func() *spikeRun {
		id, err := createConversation(ctx, conn, work)
		if err != nil {
			t.Fatal(err)
		}
		if err := setConversationModel(ctx, conn, id, "probe/m"); err != nil {
			t.Fatal(err)
		}
		return &spikeRun{t: t, ctx: ctx, conn: conn, id: id, start: time.Now()}
	}
	get := func(path string) json.RawMessage {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, conn.URL+path, nil)
		req.SetBasicAuth(conn.User, conn.Password)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: %d %s", path, resp.StatusCode, clip(string(b), 300))
			return json.RawMessage(fmt.Sprintf("%q", string(b)))
		}
		return b
	}

	fg := open()
	fg.prompt("PARENT-FG start a child", "steer", true)
	if !probeWait(ctx, func() bool { return model.saw("PARENT-FG", "final") }) {
		t.Fatal("the foreground parent never finished")
	}
	bg := open()
	bg.prompt("PARENT-BG start a child", "steer", true)
	if !probeWait(ctx, func() bool { return model.saw("CHILD-BG", "final") }) {
		t.Fatal("the background child never finished")
	}
	time.Sleep(5 * time.Second)

	out := map[string]json.RawMessage{
		"list_default": get("/api/session"),
		"list_asc":     get("/api/session?order=asc&limit=200"),
		"list_fg_kids": get("/api/session?parentID=" + url.QueryEscape(fg.id)),
	}
	for _, s := range frames.sessions() {
		out["info "+s] = get("/api/session/" + url.PathEscape(s))
		out["messages "+s] = get("/api/session/" + url.PathEscape(s) + "/message?order=asc&limit=200")
		out["inbox "+s] = get("/api/session/" + url.PathEscape(s) + "/inbox")
	}

	type listing struct {
		Data []struct {
			ID       string `json:"id"`
			ParentID string `json:"parentID"`
		} `json:"data"`
	}
	for _, k := range []string{"list_default", "list_asc", "list_fg_kids"} {
		var l listing
		_ = json.Unmarshal(out[k], &l)
		t.Logf("%s: %+v", k, l.Data)
	}
	t.Logf("roots: fg=%s bg=%s", fg.id, bg.id)
	t.Logf("sessions seen on the stream: %v", frames.sessions())
	t.Logf("model requests:\n%s", model.log())
	t.Logf("frames:\n%s", frames.summary())

	if dir := os.Getenv("SPOV_SA1_OUT"); dir != "" {
		b, _ := json.MarshalIndent(out, "", " ")
		_ = os.WriteFile(filepath.Join(dir, "reads.json"), b, 0o600)
		_ = os.WriteFile(filepath.Join(dir, "frames.jsonl"), []byte(frames.jsonl()), 0o600)
	}
}

// subagentModel scripts a parent that calls `subagent` once and a child
// that runs one glob. A request is the parent's or a child's by the tag
// in its first user message.
type subagentModel struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []string
}

func (m *subagentModel) saw(tag, answer string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.reqs {
		if strings.HasPrefix(r, tag+" ") && strings.HasSuffix(r, "-> "+answer) {
			return true
		}
	}
	return false
}

func (m *subagentModel) log() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.reqs, "\n")
}

var subagentTags = []string{"CHILD-FG", "CHILD-BG", "PARENT-FG", "PARENT-BG"}

func newSubagentModel(t *testing.T) *subagentModel {
	t.Helper()
	m := &subagentModel{}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"data":[{"id":"m","object":"model"}]}`)
			return
		}
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
			Tools []json.RawMessage `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		first, users, tools := "", 0, 0
		for _, msg := range req.Messages {
			switch msg.Role {
			case "user":
				text, _ := json.Marshal(msg.Content)
				if first == "" {
					for _, tag := range subagentTags {
						if strings.Contains(string(text), tag) {
							first = tag
							break
						}
					}
				}
				users++
			case "tool":
				tools++
			}
		}
		tool, args, answer := "", "", "final"
		switch {
		case len(req.Tools) == 0:
			answer = "title"
		case first == "PARENT-FG" && tools == 0:
			tool, args = "subagent", `{"agent":"general","description":"probe child fg","prompt":"CHILD-FG run one glob"}`
		case first == "PARENT-BG" && tools == 0:
			tool, args = "subagent", `{"agent":"general","description":"probe child bg","prompt":"CHILD-BG run one glob","background":true}`
		case strings.HasPrefix(first, "CHILD") && tools == 0:
			if first == "CHILD-BG" {
				time.Sleep(4 * time.Second)
			}
			tool, args = "glob", `{"pattern":"*.txt"}`
		}
		if tool != "" {
			answer = tool
		}
		m.mu.Lock()
		m.reqs = append(m.reqs, fmt.Sprintf("%s users=%d tools=%d -> %s", first, users, tools, answer))
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

// capturedFrames keeps every frame whole, in arrival order.
type capturedFrames struct {
	mu    sync.Mutex
	start time.Time
	all   []capturedFrame
}

type capturedFrame struct {
	At      float64         `json:"at"`
	Name    string          `json:"name"`
	Session string          `json:"session"`
	Raw     json.RawMessage `json:"raw"`
}

func captureFrames(ctx context.Context, t *testing.T, conn Connection) *capturedFrames {
	t.Helper()
	body, err := openSandboxEvents(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = body.Close() })
	c := &capturedFrames{start: time.Now()}
	go func() {
		_ = readSSEFrames(ctx, body, func(raw string) bool {
			var frame map[string]any
			if json.Unmarshal([]byte(raw), &frame) != nil {
				return true
			}
			name := find(frame, nameKeys)
			if boringFrames.MatchString(name) {
				return true
			}
			c.mu.Lock()
			c.all = append(c.all, capturedFrame{At: time.Since(c.start).Seconds(), Name: name, Session: find(frame, sessionKeys), Raw: json.RawMessage(raw)})
			c.mu.Unlock()
			return true
		})
	}()
	return c
}

func (c *capturedFrames) sessions() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, f := range c.all {
		if f.Session != "" && !seen[f.Session] {
			seen[f.Session] = true
			out = append(out, f.Session)
		}
	}
	return out
}

func (c *capturedFrames) summary() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b strings.Builder
	for _, f := range c.all {
		detail := ""
		if strings.Contains(f.Name, "inbox") || strings.Contains(f.Name, "created") || strings.Contains(f.Name, "tool") ||
			strings.Contains(f.Name, "subagent") || strings.Contains(f.Name, "execution") {
			detail = clip(string(f.Raw), 600)
		}
		fmt.Fprintf(&b, "%7.2fs %-40s %s %s\n", f.At, f.Name, f.Session, detail)
	}
	return b.String()
}

func (c *capturedFrames) jsonl() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b strings.Builder
	for _, f := range c.all {
		line, _ := json.Marshal(f)
		b.Write(line)
		b.WriteString("\n")
	}
	return b.String()
}

// Spike S-SA2: does a transcript import keep a child's link to its
// parent? A foreground subagent runs on one local server; its parent and
// child are imported into a second, fresh one, parent first as the
// stored order puts them, then the child alone into a third, whose
// parent is missing. No Modal, no key.
//
//	SPOV_PROBE=1 go test -tags integration ./internal/server/ -run SSA2 -v
//
// @decision(DL-110)
func TestIntegrationSSA2ImportKeepsTheParent(t *testing.T) {
	if os.Getenv("SPOV_PROBE") == "" && os.Getenv("SPOV_PROBE_OPENCODE") == "" {
		t.Skip("set SPOV_PROBE=1 to run the spike")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	bin := probeBinary(ctx, t)
	model := newSubagentModel(t)
	start := func() (Connection, string) {
		work := t.TempDir()
		conn := startProbeServerWith(ctx, t, bin, model.URL, work, "")
		if _, err := opencode.NewClient(conn.URL, conn.Password).WaitHealthy(ctx, 100*time.Millisecond); err != nil {
			t.Fatal(err)
		}
		return conn, work
	}
	info := func(conn Connection, id string) map[string]any {
		body, err := sandboxGet(ctx, conn.URL+"/api/session/"+url.PathEscape(id), conn.Token, conn.User, conn.Password)
		if err != nil {
			t.Fatalf("reading %s: %v", id, err)
		}
		var v struct {
			Data map[string]any `json:"data"`
		}
		_ = json.Unmarshal(body, &v)
		return v.Data
	}
	transcript := func(conn Connection, id string) []json.RawMessage {
		stored := probeMessages(ctx, t, conn, id)
		out := make([]json.RawMessage, 0, len(stored))
		for i := len(stored) - 1; i >= 0; i-- {
			out = append(out, stored[i].Raw)
		}
		return out
	}

	a, work := start()
	parent, err := createConversation(ctx, a, work)
	if err != nil {
		t.Fatal(err)
	}
	if err := setConversationModel(ctx, a, parent, "probe/m"); err != nil {
		t.Fatal(err)
	}
	(&spikeRun{t: t, ctx: ctx, conn: a, id: parent, start: time.Now()}).prompt("PARENT-FG start a child", "steer", true)
	if !probeWait(ctx, func() bool { return model.saw("PARENT-FG", "final") }) {
		t.Fatal("the parent never finished")
	}
	var listed struct {
		Data []struct {
			ID       string `json:"id"`
			ParentID string `json:"parentID"`
		} `json:"data"`
	}
	body, err := sandboxGet(ctx, a.URL+"/api/session?parentID="+url.QueryEscape(parent), a.Token, a.User, a.Password)
	if err != nil || json.Unmarshal(body, &listed) != nil || len(listed.Data) != 1 {
		t.Fatalf("the child: %v %s", err, clip(string(body), 300))
	}
	child := listed.Data[0].ID

	imp := func(conn Connection, id string, raws []json.RawMessage, inf map[string]any) error {
		b, _ := json.Marshal(map[string]any{"info": inf, "messages": raws})
		answer, err := sandboxPostJSON(ctx, conn, conn.URL+"/api/experimental/session/import", b)
		t.Logf("import %s: %s %v", id, clip(string(answer), 300), err)
		return err
	}
	pInfo, cInfo := info(a, parent), info(a, child)
	pMsgs, cMsgs := transcript(a, parent), transcript(a, child)
	t.Logf("the child's stored info names parentID %v", cInfo["parentID"])

	b, _ := start()
	if err := imp(b, parent, pMsgs, pInfo); err != nil {
		t.Fatalf("the parent's import: %v", err)
	}
	if err := imp(b, child, cMsgs, cInfo); err != nil {
		t.Fatalf("the child's import after its parent: %v", err)
	}
	if got := info(b, child)["parentID"]; got != parent {
		t.Errorf("after the import the child's parentID is %v, want %s", got, parent)
	}
	if n := len(probeMessages(ctx, t, b, child)); n != len(cMsgs) {
		t.Errorf("the child's import kept %d of %d messages", n, len(cMsgs))
	}

	c, _ := start()
	err = imp(c, child, cMsgs, cInfo)
	t.Logf("the child imported with no parent present: %v", err)
	if err == nil {
		t.Logf("  and its parentID there is %v", info(c, child)["parentID"])
	}
}
