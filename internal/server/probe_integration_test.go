//go:build integration

package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/opencode/contract"
	"github.com/jimutt/spov/internal/store"
)

// probeComplete in a user or synthetic message makes the fake model call
// the completion tool.
const probeComplete = "SPOV_PROBE_COMPLETE"

// probeProviderPackage is the pinned build's OpenAI-compatible provider.
const probeProviderPackage = "@ai-sdk/openai-compatible"

// TestIntegrationLocalOpenCodeProbe runs the pinned OpenCode build on
// this machine with the real spov-goal plugin and a fake model, and
// drives it with spov's own request and projection code. It re-checks
// the shapes the OpenAPI document does not describe: event frames,
// stored messages, inbox items and the plugin's evidence. It creates no
// sandbox and needs no provider key. The server runs isolated: a
// throwaway HOME and XDG directories, bound to loopback, a random
// password.
//
// It runs when asked, with the binary `spov fly` would use or the one
// SPOV_PROBE_OPENCODE names:
//
//	SPOV_PROBE=1 go test -tags integration ./internal/server/ -run LocalOpenCodeProbe -v
//
// @decision(DL-035)
func TestIntegrationLocalOpenCodeProbe(t *testing.T) {
	if os.Getenv("SPOV_PROBE") == "" && os.Getenv("SPOV_PROBE_OPENCODE") == "" {
		t.Skip("set SPOV_PROBE=1 to run the local OpenCode probe")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	bin := probeBinary(ctx, t)
	model := newFakeModel(t)
	work := t.TempDir()
	// The plugin's evidence path is fixed, so the run starts it empty and
	// refuses to read a file it could not clear.
	for _, f := range []string{opencode.PluginEvidencePath, opencode.PluginOutcomePath} {
		if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("clearing the plugin's %s: %v", f, err)
		}
		t.Cleanup(func() { _ = os.Remove(f) })
	}
	conn := startProbeServer(ctx, t, bin, model.URL, work)

	health, err := opencode.NewClient(conn.URL, conn.Password).WaitHealthy(ctx, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("the server never answered as healthy: %v", err)
	}
	if health.Version != contract.Version() {
		t.Errorf("the server reports %s, and the pin is %s", health.Version, contract.Version())
	}

	events := followProbeEvents(ctx, t, conn)

	id, err := createConversation(ctx, conn, work)
	if err != nil {
		t.Fatalf("creating the conversation: %v", err)
	}
	if err := setConversationModel(ctx, conn, id, "probe/m"); err != nil {
		t.Fatalf("setting the model: %v", err)
	}
	if err := openJob(ctx, conn, id, store.Job{Goal: "Say ok, then finish. " + probeComplete}); err != nil {
		t.Fatalf("opening the job: %v", err)
	}

	// The completion tool ends the run, and the stop marker reaches the
	// transcript the way the supervisor reads it.
	var msgs []store.Message
	if !probeWait(ctx, func() bool {
		msgs = probeMessages(ctx, t, conn, id)
		return jobs.StoppedIn(msgs)
	}) {
		t.Fatalf("no stop marker reached the transcript; messages: %s", probeRoles(msgs))
	}
	if !probeWait(ctx, func() bool { return events.has(func(e probeEvent) bool { return jobs.TurnEnded(e.Name) }) }) {
		t.Error("no event the recorder reads as a turn's end arrived")
	}
	if !events.has(func(e probeEvent) bool {
		return e.Name == jobs.EventInboxEnqueued && jobs.InboxItemType(e.Raw) == jobs.SyntheticRole
	}) {
		t.Errorf("no synthetic inbox item was seen; events: %s", events.names())
	}
	if !events.has(func(e probeEvent) bool { return e.Seq > 0 }) {
		t.Error("no event carried a durable sequence")
	}

	evidence, err := readEvidence(ctx, conn, opencode.PluginEvidencePath)
	if err != nil {
		t.Fatalf("reading the plugin's evidence: %v", err)
	}
	probeFlatEvidence(t, evidence)
	// Both tools ran through the real dispatcher: the pull request was
	// recorded for a selected repository, then completion was declared.
	//
	// @decision(DL-051) @decision(DL-050)
	if !strings.Contains(string(evidence), `"event":"`+opencode.DeliverableEvent+`"`) ||
		!strings.Contains(string(evidence), `"platform":"codefloe"`) {
		t.Errorf("no pull request was recorded; evidence:\n%s", evidence)
	}
	if !strings.Contains(string(evidence), `"unfinished":"nothing much"`) {
		t.Errorf("the completion did not carry unfinished work; evidence:\n%s", evidence)
	}
	outcome, err := readEvidence(ctx, conn, OutcomePath)
	if err != nil {
		t.Fatalf("reading the plugin's outcome file: %v", err)
	}
	for _, want := range []string{opencode.DeliverableEvent, "completion.declared", "loop.stopped"} {
		if !strings.Contains(string(outcome), `"event":"`+want+`"`) {
			t.Errorf("the outcome file has no %s line:\n%s", want, outcome)
		}
	}
	if strings.Contains(string(outcome), "permission.") {
		t.Errorf("the outcome file holds permission lines:\n%s", outcome)
	}

	// The loop command the supervisor sends to pause and resume a job.
	for _, arg := range []string{LoopPause, LoopResume} {
		if err := sendLoopCommand(ctx, conn, id, arg); err != nil {
			t.Errorf("the %s command: %v", arg, err)
		}
	}

	// A human's message arrives as a user inbox item, which is what holds
	// a job's loop.
	minted := MessageIDPrefix + "probe1"
	body, err := json.Marshal(steerBody(minted, "a human says hello", "steer", ""))
	if err != nil {
		t.Fatal(err)
	}
	answer, err := sandboxPostJSON(ctx, conn, promptURL(conn, id), body)
	if err != nil {
		t.Fatalf("steering: %v", err)
	}
	// With no fallback, only an answer that carries the id returns it.
	if got := promptedID(answer, ""); got != minted {
		t.Errorf("the steer's answer names message %q, want %q: %s", got, minted, answer)
	}
	if !probeWait(ctx, func() bool {
		return events.has(func(e probeEvent) bool {
			return e.Name == jobs.EventInboxEnqueued && jobs.InboxItemType(e.Raw) == "user"
		})
	}) {
		t.Errorf("the human's message never arrived as a user inbox item; events: %s", events.names())
	}

	answer, err = sandboxPostJSON(ctx, conn, interruptURL(conn, id, ResumeOnInterrupt), nil)
	if err != nil {
		t.Fatalf("the interrupt: %v", err)
	}
	var interrupted map[string]any
	if err := json.Unmarshal(answer, &interrupted); err != nil || interrupted["interrupted"] == nil {
		t.Errorf("the interrupt's answer has no interrupted field for wasInterrupted to read: %s", answer)
	}
}

// probeBinary finds the pinned build: SPOV_PROBE_OPENCODE, else where
// `spov fly` would look.
func probeBinary(ctx context.Context, t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("SPOV_PROBE_OPENCODE"); bin != "" {
		// The server runs in another directory, so a relative path is
		// made absolute here.
		abs, err := filepath.Abs(bin)
		if err != nil {
			t.Fatal(err)
		}
		return abs
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(home, ".local", "share", "spov")
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		data = filepath.Join(dir, "spov")
	}
	// The probe checks the pinned build itself, so a nearby patch the
	// locator would accept is not enough.
	found, err := opencode.Locator{DataDir: data, HomeDir: home}.Locate(ctx, contract.Version())
	if err == nil && found.Drift != opencode.Exact {
		err = fmt.Errorf("the closest is %s at %s", found.Version, found.Path)
	}
	if err != nil {
		t.Fatalf("no local OpenCode %s (set SPOV_PROBE_OPENCODE to one): %v", contract.Version(), err)
	}
	return found.Path
}

// startProbeServer writes an isolated home with the fake model's
// provider and the spov-goal plugin, and starts the server in it.
func startProbeServer(ctx context.Context, t *testing.T, bin, modelURL, work string) Connection {
	t.Helper()
	// A pr job, so both of the plugin's tools are registered (DL-051).
	source, err := opencode.Plugin{
		Kind: "pr", Goal: "Say ok, then finish. " + probeComplete,
		Dir:       work,
		Workspace: []opencode.WorkspaceRepo{{Slug: "acme/app", Dir: "app", Platform: "codefloe", Host: "codefloe.com"}},
	}.Source()
	if err != nil {
		t.Fatal(err)
	}
	return startProbeServerWith(ctx, t, bin, modelURL, work, source)
}

// startProbeServerWith is startProbeServer with the plugin's source
// given, or none when it is empty, for a spike that measures OpenCode
// alone or another plugin.
func startProbeServerWith(ctx context.Context, t *testing.T, bin, modelURL, work, plugin string, env ...string) Connection {
	t.Helper()
	root := t.TempDir()
	dirs := map[string]string{}
	for _, d := range []string{"home", "config", "data", "state", "cache"} {
		dirs[d] = filepath.Join(root, d)
		if err := os.MkdirAll(dirs[d], 0o700); err != nil {
			t.Fatal(err)
		}
	}
	pluginDir := filepath.Join(dirs["config"], "opencode", "plugins")
	if err := os.MkdirAll(pluginDir, 0o700); err != nil {
		t.Fatal(err)
	}
	config, err := json.Marshal(map[string]any{
		"model": "probe/m",
		"providers": map[string]any{"probe": map[string]any{
			"name":     "Probe",
			"package":  probeProviderPackage,
			"settings": map[string]any{"baseURL": modelURL + "/v1", "apiKey": "probe"},
			"models":   map[string]any{"m": map[string]any{"name": "m"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirs["config"], "opencode", "opencode.json"), config, 0o600); err != nil {
		t.Fatal(err)
	}
	if plugin != "" {
		if err := os.WriteFile(filepath.Join(pluginDir, "spov-goal.js"), []byte(plugin), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	secret := make([]byte, 18)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	password := hex.EncodeToString(secret)

	cmd := exec.Command(bin, "serve", "--hostname", "127.0.0.1", "--port", fmt.Sprint(port))
	cmd.Dir = work
	// Its own process group, so whatever it starts is stopped with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = []string{
		"PATH=/usr/bin:/bin", "HOME=" + dirs["home"],
		"XDG_CONFIG_HOME=" + dirs["config"], "XDG_DATA_HOME=" + dirs["data"],
		"XDG_STATE_HOME=" + dirs["state"], "XDG_CACHE_HOME=" + dirs["cache"],
		opencode.PasswordEnvVar + "=" + password, "OPENCODE_DISABLE_AUTOUPDATE=1",
	}
	cmd.Env = append(cmd.Env, env...)
	logs := filepath.Join(root, "server.log")
	out, err := os.Create(logs)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		_ = out.Close()
		if t.Failed() {
			if b, err := os.ReadFile(logs); err == nil {
				t.Logf("server log:\n%s", b)
			}
		}
	})
	return Connection{URL: fmt.Sprintf("http://127.0.0.1:%d", port), User: opencode.BasicAuthUser, Password: password}
}

// probeEvent is one event frame as the recorder projects it.
type probeEvent struct {
	LogEvent
	Raw []byte
	Seq int64
}

type probeEvents struct {
	mu  sync.Mutex
	all []probeEvent
}

func (p *probeEvents) has(match func(probeEvent) bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.all {
		if match(e) {
			return true
		}
	}
	return false
}

func (p *probeEvents) names() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, e := range p.all {
		if !seen[e.Name] {
			seen[e.Name] = true
			out = append(out, e.Name)
		}
	}
	return strings.Join(out, ", ")
}

// followProbeEvents reads the event stream the way the recorder does.
func followProbeEvents(ctx context.Context, t *testing.T, conn Connection) *probeEvents {
	t.Helper()
	body, err := openSandboxEvents(ctx, conn)
	if err != nil {
		t.Fatalf("opening the event stream: %v", err)
	}
	t.Cleanup(func() { _ = body.Close() })
	p := &probeEvents{}
	go func() {
		_ = readSSEFrames(ctx, body, func(raw string) bool {
			var frame map[string]any
			if json.Unmarshal([]byte(raw), &frame) != nil {
				return true
			}
			e, ok := projectFrame(frame, time.Now())
			if !ok {
				return true
			}
			p.mu.Lock()
			p.all = append(p.all, probeEvent{LogEvent: e, Raw: []byte(raw), Seq: durableSeq(frame)})
			p.mu.Unlock()
			return true
		})
	}()
	return p
}

// probeMessages reads a conversation's messages the way the recorder
// stores them.
func probeMessages(ctx context.Context, t *testing.T, conn Connection, id string) []store.Message {
	t.Helper()
	body, err := sandboxGet(ctx, conn.URL+"/api/session/"+url.PathEscape(id)+"/message", conn.Token, conn.User, conn.Password)
	if err != nil {
		t.Fatalf("reading the messages: %v", err)
	}
	var out []store.Message
	for _, m := range parseMessages(body) {
		out = append(out, store.Message{OpenCodeID: m.ID, Role: m.Role, Text: m.Text, Raw: m.Raw})
	}
	return out
}

func probeRoles(msgs []store.Message) string {
	var out []string
	for _, m := range msgs {
		text := m.Text
		if len(text) > 60 {
			text = text[:60] + "…"
		}
		out = append(out, fmt.Sprintf("%s %q", m.Role, text))
	}
	return strings.Join(out, "; ")
}

// probeFlatEvidence checks the line shape gate 1 reads: flat, with the
// event's name and time at the top level beside its data (DL-050).
func probeFlatEvidence(t *testing.T, evidence []byte) {
	t.Helper()
	sc := bufio.NewScanner(strings.NewReader(string(evidence)))
	var names []string
	for sc.Scan() {
		var line map[string]any
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Errorf("an evidence line is not JSON: %q", sc.Text())
			continue
		}
		name, _ := line["event"].(string)
		if name == "" || line["at"] == nil {
			t.Errorf("an evidence line has no top-level event and at: %q", sc.Text())
		}
		if _, nested := line["data"].(map[string]any); nested {
			t.Errorf("an evidence line nests its data: %q", sc.Text())
		}
		names = append(names, name)
	}
	if err := sc.Err(); err != nil {
		t.Errorf("reading the evidence: %v", err)
	}
	if !strings.Contains(strings.Join(names, " "), "completion.tool.registered") {
		t.Errorf("the plugin never recorded its completion tool; evidence: %v", names)
	}
	if hooked := strings.Count(strings.Join(names, " "), "goal.hooked"); hooked != 2 {
		t.Errorf("the goal handler attached %d times, want the context and the compaction hook; evidence: %v", hooked, names)
	}
}

func probeWait(ctx context.Context, done func() bool) bool {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if done() {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return done()
}

// fakeModel is an OpenAI-compatible chat endpoint that streams a short
// reply, or a call to the completion tool when the newest user message
// asks for one and no tool has answered since.
type fakeModel struct{ *httptest.Server }

func newFakeModel(t *testing.T) fakeModel {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"data":[{"id":"m","object":"model"}]}`)
			return
		}
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		offered := map[string]bool{}
		for _, tool := range req.Tools {
			offered[tool.Function.Name] = true
		}
		// After the probe's goal: record a pull request, then declare
		// completion, then just answer.
		call, answered := false, 0
		for _, m := range req.Messages {
			switch m.Role {
			case "user":
				text, _ := json.Marshal(m.Content)
				if strings.Contains(string(text), probeComplete) {
					call, answered = true, 0
				}
			case "tool":
				answered++
			}
		}
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
		tool, args := "", ""
		switch {
		case call && answered == 0 && offered[opencode.PullRequestTool]:
			tool, args = opencode.PullRequestTool, `{"repository":"acme/app","branch":"probe/branch"}`
		case call && answered <= 1 && offered[opencode.CompletionTool]:
			tool, args = opencode.CompletionTool, `{"summary":"probe","unfinished":"nothing much"}`
		}
		if tool != "" {
			chunk(map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
				"index": 0, "id": fmt.Sprintf("call_%d", answered+1), "type": "function",
				"function": map[string]any{"name": tool, "arguments": args},
			}}}, nil)
			chunk(map[string]any{}, "tool_calls")
		} else {
			chunk(map[string]any{"role": "assistant", "content": "ok"}, nil)
			chunk(map[string]any{}, "stop")
		}
		send(map[string]any{"id": "c", "object": "chat.completion.chunk", "created": 0, "model": "m", "choices": []any{},
			"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 2, "total_tokens": 7}})
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return fakeModel{srv}
}
