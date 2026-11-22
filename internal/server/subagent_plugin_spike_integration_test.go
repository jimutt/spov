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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/store"
)

// Spike S-SA3: what a plugin sees of a subagent at the pin, and what an
// interrupt does to a plugin tool and to a running child (DL-143,
// DL-144, DL-146). A plugin that only observes records every event its
// own subscription delivers, and registers `probe_wait`, a tool that
// waits on its call's abort signal. A fake model makes four root
// conversations run:
//
//   - A: a subagent whose child runs one slow glob, to see the child's
//     frames on the plugin's subscription and its report reach the
//     parent;
//   - B: `probe_wait` in the root, interrupted while it waits;
//   - C: a subagent whose child waits in `probe_wait`, and an interrupt
//     of the parent once the parent's turn has ended, then of the child;
//   - D: the same with the parent's own turn still in `probe_wait`.
//
// A, B and ctx.session.get's answers are asserted; C and D logged.
//
//	SPOV_PROBE=1 go test -tags integration ./internal/server/ -run SSA3 -v
//
// @decision(DL-143) @decision(DL-146) @decision(DL-144)
func TestIntegrationSSA3WhatAPluginSeesOfASubagent(t *testing.T) {
	if os.Getenv("SPOV_PROBE") == "" && os.Getenv("SPOV_PROBE_OPENCODE") == "" {
		t.Skip("set SPOV_PROBE=1 to run the spike")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	seen := filepath.Join(t.TempDir(), "seen.jsonl")
	model := newTaggedModel(t, func(q taggedRequest) taggedAnswer {
		report := strings.Contains(q.latest, "subagent sessionID=")
		switch {
		case !q.offered:
			return taggedAnswer{text: "title"}
		case report:
			return taggedAnswer{text: "read the report"}
		case q.tag == "PARENT-A" && q.tools == 0:
			return taggedAnswer{tool: "subagent", args: `{"agent":"general","description":"probe child a","prompt":"CHILD-A run one glob"}`}
		case q.tag == "CHILD-A" && q.tools == 0:
			return taggedAnswer{tool: "glob", args: `{"pattern":"*.txt"}`, delay: 5 * time.Second}
		case q.tag == "PARENT-B" && q.tools == 0:
			return taggedAnswer{tool: "probe_wait", args: `{}`}
		case q.tag == "PARENT-C" && q.tools == 0:
			return taggedAnswer{tool: "subagent", args: `{"agent":"general","description":"probe child c","prompt":"CHILD-C wait"}`}
		case q.tag == "CHILD-C" && q.tools == 0, q.tag == "CHILD-D" && q.tools == 0:
			return taggedAnswer{tool: "probe_wait", args: `{}`}
		case q.tag == "PARENT-D" && q.tools == 0:
			return taggedAnswer{tool: "subagent", args: `{"agent":"general","description":"probe child d","prompt":"CHILD-D wait"}`}
		case q.tag == "PARENT-D" && q.tools == 1:
			return taggedAnswer{tool: "probe_wait", args: `{}`}
		}
		return taggedAnswer{text: "final"}
	})
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "note.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	conn := startProbeServerWith(ctx, t, probeBinary(ctx, t), model.URL, work, observingPlugin(seen))
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
	log := func() []pluginSaw { return readPluginSaw(t, seen) }

	// A: the child's frames, and the report that starts the parent's
	// next turn.
	a := open()
	a.prompt("PARENT-A start a child", "steer", true)
	if !probeWait(ctx, func() bool { return countSaw(log(), a.id, "session.execution.succeeded") >= 2 }) {
		t.Fatalf("the parent never ran a turn on its child's report; the plugin saw:\n%s", sawSummary(log()))
	}
	childA := ""
	for _, e := range log() {
		if e.Event == "session.created" && e.Parent == a.id {
			childA = e.Session
		}
	}
	if childA == "" {
		t.Errorf("the plugin saw no session.created naming the parent %s", a.id)
	}
	order := sawOrder(log(), a.id, childA)
	t.Logf("A: parent %s, child %s; in the plugin's order:\n%s", a.id, childA, strings.Join(order, "\n"))
	// The hold needs the child's start before the parent's turn ends,
	// and the child's end before its report starts the parent's next.
	at := func(who, event string) int { return slices.Index(order, "  "+who+" "+event) }
	if c, s := at("child", "session.execution.started"), at("parent", "session.execution.succeeded"); c < 0 || s < 0 || c > s {
		t.Errorf("A: the child's turn did not start before the parent's first turn ended")
	}
	if c, r := at("child", "session.execution.succeeded"), slices.Index(order[at("parent", "session.execution.succeeded")+1:], "  parent session.execution.started"); c < 0 || r < 0 {
		t.Errorf("A: the child's end and the parent's next turn are not both there")
	}
	report := false
	for _, e := range log() {
		if e.Event == "session.inbox.enqueued" && e.Session == a.id && strings.Contains(e.Raw, `"type":"synthetic"`) &&
			strings.Contains(e.Raw, `"source":"subagent"`) && strings.Contains(e.Raw, childA) {
			report = true
		}
	}
	if !report {
		t.Errorf("A: the child's report did not reach the parent's inbox as a synthetic item from the subagent")
	}

	// B: a plugin tool's call context, and an interrupt while it waits.
	b := open()
	b.prompt("PARENT-B wait", "steer", true)
	if !probeWait(ctx, func() bool { return sawTool(log(), b.id, "started") != nil }) {
		t.Fatalf("probe_wait never started; the plugin saw:\n%s", sawSummary(log()))
	}
	started := sawTool(log(), b.id, "started")
	t.Logf("B: the tool's context keys %v, signal %q, aborted %v, AbortSignal %v", started.Keys, started.Signal, started.Aborted, started.IsAbortSignal)
	if !started.IsAbortSignal || started.Aborted != false {
		t.Errorf("B: a plugin tool's call context carries no live AbortSignal")
	}
	b.interrupt(false)
	if !probeWait(ctx, func() bool { return sawTool(log(), b.id, "aborted") != nil || sawTool(log(), b.id, "timeout") != nil }) {
		t.Fatal("probe_wait never ended")
	}
	if ended := sawTool(log(), b.id, "aborted"); ended != nil {
		t.Logf("B: the interrupt aborted the tool's signal (reason %q)", ended.Reason)
	} else {
		t.Errorf("B: the interrupt did not abort the tool's signal; it ran to its timeout")
	}
	t.Logf("B: execution frames: %v", sawExecutions(log(), b.id))

	// C: an interrupt of the parent while its child waits in a tool.
	c := open()
	c.prompt("PARENT-C start a child", "steer", true)
	childC := ""
	if !probeWait(ctx, func() bool {
		for _, e := range log() {
			if e.Event == "session.created" && e.Parent == c.id {
				childC = e.Session
			}
		}
		return childC != "" && sawTool(log(), childC, "started") != nil && countSaw(log(), c.id, "session.execution.succeeded") >= 1
	}) {
		t.Fatalf("the child never waited with its parent idle; the plugin saw:\n%s", sawSummary(log()))
	}
	c.interrupt(false)
	time.Sleep(3 * time.Second)
	childEnded := sawTool(log(), childC, "aborted") != nil
	t.Logf("C: after an interrupt of the idle parent, the child's tool aborted: %v; child executions %v; parent executions %v",
		childEnded, sawExecutions(log(), childC), sawExecutions(log(), c.id))
	if !childEnded {
		(&spikeRun{t: t, ctx: ctx, conn: conn, id: childC, start: time.Now()}).interrupt(false)
		time.Sleep(3 * time.Second)
		t.Logf("C: after an interrupt of the child itself, its tool aborted: %v; child executions %v; parent executions %v",
			sawTool(log(), childC, "aborted") != nil, sawExecutions(log(), childC), sawExecutions(log(), c.id))
	}

	// D: an interrupt of the parent while its own turn is still in
	// flight (in probe_wait) and its child waits too.
	d := open()
	d.prompt("PARENT-D start a child and wait", "steer", true)
	childD := ""
	if !probeWait(ctx, func() bool {
		for _, e := range log() {
			if e.Event == "session.created" && e.Parent == d.id {
				childD = e.Session
			}
		}
		return childD != "" && sawTool(log(), childD, "started") != nil && sawTool(log(), d.id, "started") != nil
	}) {
		t.Fatalf("the parent and its child never both waited; the plugin saw:\n%s", sawSummary(log()))
	}
	d.interrupt(false)
	time.Sleep(3 * time.Second)
	t.Logf("D: after an interrupt of the busy parent, the parent's tool aborted: %v, the child's: %v; child executions %v; parent executions %v",
		sawTool(log(), d.id, "aborted") != nil, sawTool(log(), childD, "aborted") != nil, sawExecutions(log(), childD), sawExecutions(log(), d.id))
	t.Logf("B, later: execution frames %v", sawExecutions(log(), b.id))
	for _, e := range log() {
		if e.Event == "session.inbox.enqueued" && (e.Session == c.id || e.Session == a.id || e.Session == d.id) {
			t.Logf("inbox of %s: %s", e.Session, clip(e.Raw, 700))
		}
	}
	// The job plugin learns its session from ctx.session.get, which
	// answers Session.Info for {sessionID} and names a child's parent.
	lookups := map[string]pluginSaw{}
	for _, e := range log() {
		if e.Info == "object" {
			lookups[e.Session] = e
		}
	}
	if l, ok := lookups[childA]; !ok || l.Parent != a.id || l.ID != childA {
		t.Errorf("ctx.session.get did not name the child's parent: %+v", l)
	}
	if l, ok := lookups[a.id]; !ok || l.Parent != "" || l.ID != a.id || l.Error != "" {
		t.Errorf("ctx.session.get did not answer the root as one: %+v", l)
	}
	for _, e := range log() {
		if e.Info != "" || e.Hook != "" {
			t.Logf("lookup %s hook %s session %s parent %q id %v keys %v error %s", e.Info, e.Hook, e.Session, e.Parent, e.ID, e.Keys, e.Error)
		}
	}
	t.Logf("model requests:\n%s", model.log())
	if os.Getenv("SPOV_SA3_FRAMES") != "" {
		t.Logf("wire frames:\n%s", frames.summary())
	}
}

// observingPlugin records every event its subscription delivers to out,
// and registers probe_wait.
func observingPlugin(out string) string {
	path, _ := json.Marshal(out)
	return `const { appendFileSync } = await import("node:fs")
const OUT = ` + string(path) + `
function log(v) { try { appendFileSync(OUT, JSON.stringify({ at: Date.now(), ...v }) + "\n") } catch (e) {} }
export default {
  id: "spike.subagents",
  async setup(ctx) {
    ctx.tool.transform((draft) => {
      draft.add({
        name: "probe_wait",
        options: { codemode: false },
        description: "Wait until stopped.",
        input: { type: "object", properties: {}, additionalProperties: false },
        execute: async (input, context) => {
          const signal = context?.signal
          log({ tool: "started", session: context?.sessionID ?? null, keys: Object.keys(context ?? {}).sort(),
            signal: typeof signal, aborted: signal?.aborted ?? null,
            isAbortSignal: typeof AbortSignal !== "undefined" && signal instanceof AbortSignal })
          const how = await new Promise((resolve) => {
            const timer = setTimeout(() => resolve("timeout"), 20000)
            signal?.addEventListener?.("abort", () => { clearTimeout(timer); resolve("aborted") })
          })
          log({ tool: how, session: context?.sessionID ?? null, reason: String(signal?.reason ?? "") })
          return { content: "waited: " + how }
        },
      })
    })
    try {
      ctx.session.hook("context", (event) => {
        log({ hook: "context", session: event?.sessionID ?? null, keys: Object.keys(event ?? {}).sort() })
      })
    } catch (e) { log({ hook: "context", error: String(e) }) }
    const looked = new Set()
    const lookUp = async (id) => {
      if (!id || looked.has(id)) return
      looked.add(id)
      for (const [form, arg] of [["id", id], ["object", { sessionID: id }]]) {
        try {
          const info = await ctx.session.get(arg)
          log({ info: form, session: id, parent: info?.parentID ?? info?.data?.parentID ?? null,
            keys: Object.keys(info ?? {}).sort(), id: info?.id ?? info?.data?.id ?? null })
        } catch (e) { log({ info: form, session: id, error: String(e).slice(0, 300) }) }
      }
    }
    ;(async () => {
      for await (const ev of ctx.event.subscribe()) {
        const type = ev?.type ?? ev?.event
        const data = ev?.data ?? ev?.properties ?? {}
        await lookUp(data?.sessionID)
        const entry = { event: type, session: data?.sessionID ?? null, keys: Object.keys(ev ?? {}).sort() }
        if (type === "session.created") entry.parent = data?.parentID ?? data?.info?.parentID ?? null
        if (typeof type === "string" && type.startsWith("session.execution.")) entry.reason = data?.reason ?? null
        if (type === "session.created" || type === "session.inbox.enqueued") entry.raw = JSON.stringify(ev).slice(0, 1500)
        log(entry)
      }
    })()
  },
}
`
}

// pluginSaw is one line the observing plugin wrote.
type pluginSaw struct {
	Info          string   `json:"info"`
	Hook          string   `json:"hook"`
	Error         string   `json:"error"`
	ID            any      `json:"id"`
	At            int64    `json:"at"`
	Event         string   `json:"event"`
	Session       string   `json:"session"`
	Parent        string   `json:"parent"`
	Reason        any      `json:"reason"`
	Raw           string   `json:"raw"`
	Tool          string   `json:"tool"`
	Keys          []string `json:"keys"`
	Signal        string   `json:"signal"`
	Aborted       any      `json:"aborted"`
	IsAbortSignal bool     `json:"isAbortSignal"`
}

func readPluginSaw(t *testing.T, path string) []pluginSaw {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []pluginSaw
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var e pluginSaw
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

func countSaw(all []pluginSaw, session, event string) int {
	n := 0
	for _, e := range all {
		if e.Session == session && e.Event == event {
			n++
		}
	}
	return n
}

func sawTool(all []pluginSaw, session, what string) *pluginSaw {
	for i := range all {
		if all[i].Tool == what && all[i].Session == session {
			return &all[i]
		}
	}
	return nil
}

func sawExecutions(all []pluginSaw, session string) []string {
	var out []string
	for _, e := range all {
		if e.Session == session && strings.HasPrefix(e.Event, "session.execution.") {
			out = append(out, fmt.Sprintf("%s(%v)", strings.TrimPrefix(e.Event, "session.execution."), e.Reason))
		}
	}
	return out
}

// sawOrder is the lifecycle frames of a parent and its child, in the
// order the plugin received them.
func sawOrder(all []pluginSaw, parent, child string) []string {
	var out []string
	for _, e := range all {
		if e.Session != parent && e.Session != child {
			continue
		}
		if !strings.HasPrefix(e.Event, "session.execution.") && e.Event != "session.created" &&
			e.Event != "session.inbox.enqueued" && e.Event != "session.inbox.delivered" {
			continue
		}
		who := "parent"
		if e.Session == child {
			who = "child"
		}
		out = append(out, fmt.Sprintf("  %s %s", who, e.Event))
	}
	return out
}

func sawSummary(all []pluginSaw) string {
	var b strings.Builder
	for _, e := range all {
		fmt.Fprintf(&b, "%s %s %s %s\n", e.Event, e.Session, e.Tool, e.Parent)
	}
	return b.String()
}

// taggedRequest is a chat request as a taggedModel's rule reads it: the
// tag its first user message carries, the tool results since, whether
// tools were offered and the newest user text.
type taggedRequest struct {
	tag     string
	tools   int
	offered bool
	latest  string
	users   []string
}

// taggedAnswer is a reply: a call to tool with args, else text, after
// delay.
type taggedAnswer struct {
	tool, args, text string
	delay            time.Duration
}

// taggedModel is an OpenAI-compatible endpoint whose answers a rule
// decides. A request is tagged by the first word of the form WORD-WORD
// in upper case in its first user message.
type taggedModel struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []string
}

func (m *taggedModel) log() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.reqs, "\n")
}

func newTaggedModel(t *testing.T, rule func(taggedRequest) taggedAnswer) *taggedModel {
	t.Helper()
	m := &taggedModel{}
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
		q := taggedRequest{offered: len(req.Tools) > 0}
		for _, msg := range req.Messages {
			switch msg.Role {
			case "user":
				text, _ := json.Marshal(msg.Content)
				if q.tag == "" {
					q.tag = probeTag(string(text))
				}
				q.latest, q.tools = string(text), 0
				q.users = append(q.users, string(text))
			case "tool":
				q.tools++
			}
		}
		a := rule(q)
		if a.delay > 0 {
			select {
			case <-time.After(a.delay):
			case <-r.Context().Done():
				m.mu.Lock()
				m.reqs = append(m.reqs, fmt.Sprintf("%s tools=%d -> cancelled while waiting", q.tag, q.tools))
				m.mu.Unlock()
				return
			}
		}
		answer := a.text
		if a.tool != "" {
			answer = a.tool
		}
		m.mu.Lock()
		m.reqs = append(m.reqs, fmt.Sprintf("%s tools=%d -> %s", q.tag, q.tools, answer))
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
		if a.tool != "" {
			chunk(map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
				"index": 0, "id": fmt.Sprintf("call_%d", time.Now().UnixNano()), "type": "function",
				"function": map[string]any{"name": a.tool, "arguments": a.args},
			}}}, nil)
			chunk(map[string]any{}, "tool_calls")
		} else {
			chunk(map[string]any{"role": "assistant", "content": a.text}, nil)
			chunk(map[string]any{}, "stop")
		}
		send(map[string]any{"id": "c", "object": "chat.completion.chunk", "created": 0, "model": "m", "choices": []any{},
			"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 2, "total_tokens": 7}})
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(m.Server.Close)
	return m
}

// probeTag is the first PARENT-x, CHILD-x or JOB-x word in text.
func probeTag(text string) string {
	for _, f := range strings.FieldsFunc(text, func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r == '-' || r >= '0' && r <= '9')
	}) {
		for _, p := range []string{"PARENT-", "CHILD-", "JOB-"} {
			if strings.HasPrefix(f, p) && len(f) > len(p) {
				return f
			}
		}
	}
	return ""
}

// jobProbe is the real plugin of a report job at the pin, on a local
// OpenCode, opened as the supervisor opens a job.
type jobProbe struct {
	*spikeRun
	work string
}

// startJobProbe starts a local OpenCode with plugin and opens a job on
// goal in it. The evidence files are this machine's, as the plugin
// writes them, and are removed before and after.
func startJobProbe(ctx context.Context, t *testing.T, model *taggedModel, plugin opencode.Plugin) *jobProbe {
	t.Helper()
	for _, f := range []string{opencode.PluginEvidencePath, opencode.PluginOutcomePath} {
		_ = os.Remove(f)
		t.Cleanup(func() { _ = os.Remove(f) })
	}
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "note.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plugin.Dir = work
	source, err := plugin.Source()
	if err != nil {
		t.Fatal(err)
	}
	conn := startProbeServerWith(ctx, t, probeBinary(ctx, t), model.URL, work, source)
	if _, err := opencode.NewClient(conn.URL, conn.Password).WaitHealthy(ctx, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	id, err := createConversation(ctx, conn, work)
	if err != nil {
		t.Fatal(err)
	}
	if err := setConversationModel(ctx, conn, id, "probe/m"); err != nil {
		t.Fatal(err)
	}
	if err := openJob(ctx, conn, id, store.Job{Goal: plugin.Goal}); err != nil {
		t.Fatal(err)
	}
	return &jobProbe{spikeRun: &spikeRun{t: t, ctx: ctx, conn: conn, id: id, start: time.Now()}, work: work}
}

// evidence is the plugin's evidence lines of one event.
func (j *jobProbe) evidence(event string) []map[string]any {
	b, _ := os.ReadFile(opencode.PluginEvidencePath)
	var out []map[string]any
	for _, line := range strings.Split(string(b), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["event"] == event {
			out = append(out, m)
		}
	}
	return out
}

func (j *jobProbe) evidenceLog() string {
	b, _ := os.ReadFile(opencode.PluginEvidencePath)
	return string(b)
}

// The real plugin at the pin, against DL-146: an interrupt while the
// completion's check runs withdraws the declaration and kills the check.
//
//	SPOV_PROBE=1 go test -tags integration ./internal/server/ -run SSA3 -v
//
// @decision(DL-146)
func TestIntegrationSSA3AnInterruptWithdrawsADeclarationDuringItsCheck(t *testing.T) {
	if os.Getenv("SPOV_PROBE") == "" && os.Getenv("SPOV_PROBE_OPENCODE") == "" {
		t.Skip("set SPOV_PROBE=1 to run the spike")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	model := newTaggedModel(t, func(q taggedRequest) taggedAnswer {
		switch {
		case !q.offered:
			return taggedAnswer{text: "title"}
		case q.tag == "JOB-W" && q.tools == 0 && !strings.Contains(q.latest, "Continue working"):
			return taggedAnswer{tool: opencode.CompletionTool, args: `{"summary":"done"}`}
		}
		return taggedAnswer{text: "ok"}
	})
	j := startJobProbe(ctx, t, model, opencode.Plugin{Kind: "report", Goal: "JOB-W finish at once", CheckCommand: "sleep 15; touch checked"})
	if !probeWait(ctx, func() bool { return strings.Contains(model.log(), "JOB-W tools=0 -> "+opencode.CompletionTool) }) {
		t.Fatalf("the model never called the completion tool:\n%s", model.log())
	}
	time.Sleep(2 * time.Second)
	j.interrupt(true)
	if !probeWait(ctx, func() bool { return len(j.evidence("completion.interrupted")) == 1 }) {
		t.Fatalf("the interrupt did not withdraw the declaration; evidence:\n%s", j.evidenceLog())
	}
	time.Sleep(time.Second)
	if n := len(j.evidence("completion.declared")) + len(j.evidence("loop.stopped")) + len(j.evidence(opencode.CheckEvent)); n != 0 {
		t.Errorf("a withdrawn declaration left %d declaration, stop or check lines; evidence:\n%s", n, j.evidenceLog())
	}
	if len(j.evidence("check.interrupted")) != 1 {
		t.Errorf("the check was not stopped; evidence:\n%s", j.evidenceLog())
	}
	if len(j.evidence("loop.held")) != 1 {
		t.Errorf("the person's interrupt did not hold the loop; evidence:\n%s", j.evidenceLog())
	}
	time.Sleep(15 * time.Second)
	if _, err := os.Stat(filepath.Join(j.work, "checked")); err == nil {
		t.Error("the check ran on after the interrupt")
	}
	t.Logf("model requests:\n%s", model.log())
}

// The real plugin at the pin, against DL-143: the job's turn ends while
// its subagent works, no continuation is sent, the child's report starts
// the next turn, and the loop then continues as before.
//
//	SPOV_PROBE=1 go test -tags integration ./internal/server/ -run SSA3 -v
//
// @decision(DL-143)
func TestIntegrationSSA3TheLoopWaitsForItsSubagent(t *testing.T) {
	if os.Getenv("SPOV_PROBE") == "" && os.Getenv("SPOV_PROBE_OPENCODE") == "" {
		t.Skip("set SPOV_PROBE=1 to run the spike")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	model := newTaggedModel(t, func(q taggedRequest) taggedAnswer {
		switch {
		case !q.offered:
			return taggedAnswer{text: "title"}
		case q.tag == "CHILD-H" && q.tools == 0:
			return taggedAnswer{tool: "glob", args: `{"pattern":"*.txt"}`, delay: 8 * time.Second}
		case q.tag == "CHILD-H":
			return taggedAnswer{text: "child report"}
		case q.tools > 0, strings.Contains(q.latest, opencode.LoopStoppedMarker):
			return taggedAnswer{text: "called"}
		case strings.Contains(q.latest, "subagent sessionID="):
			return taggedAnswer{text: "read the report"}
		case strings.Contains(q.latest, "Continue working"):
			return taggedAnswer{tool: opencode.CompletionTool, args: `{"summary":"done"}`}
		case q.tag == "JOB-H":
			return taggedAnswer{tool: "subagent", args: `{"agent":"general","description":"probe child h","prompt":"CHILD-H run one glob"}`}
		}
		return taggedAnswer{text: "ok"}
	})
	j := startJobProbe(ctx, t, model, opencode.Plugin{Kind: "report", Goal: "JOB-H delegate to a subagent"})
	if !probeWait(ctx, func() bool { return len(j.evidence("loop.stopped")) == 1 }) {
		t.Fatalf("the job never ended; model requests:\n%s\nevidence:\n%s", model.log(), j.evidenceLog())
	}
	answers := []string{}
	for _, line := range strings.Split(model.log(), "\n") {
		if strings.HasPrefix(line, "JOB-H") {
			_, a, _ := strings.Cut(line, "-> ")
			answers = append(answers, a)
		}
	}
	t.Logf("the job's model requests, in order: %v", answers)
	// The answer to the completion's own result may come after the stop.
	want := []string{"subagent", "called", "read the report", opencode.CompletionTool}
	answers = slices.DeleteFunc(answers, func(a string) bool { return a == "title" })
	if len(answers) < len(want) || !slices.Equal(answers[:len(want)], want) {
		t.Errorf("the job's requests were %v, want %v: a continuation went out while the subagent ran, or none after its report", answers, want)
	}
	waiting := j.evidence("continue.waiting")
	if len(waiting) != 1 || waiting[0]["running"] != float64(1) {
		t.Errorf("continue.waiting = %v, want one with one subagent running; evidence:\n%s", waiting, j.evidenceLog())
	}
	if got := j.evidence("loop.stopped"); got[0]["reason"] != opencode.CompletionReason {
		t.Errorf("the run stopped as %v", got)
	}
}

// The real plugin at the pin, against DL-182: the job's turn ends while
// a shell command it ran in the background works, no continuation is
// sent, the command's result starts the next turn, and the job ends on
// it.
//
//	SPOV_PROBE=1 go test -tags integration ./internal/server/ -run SSA3 -v
//
// @decision(DL-182)
func TestIntegrationSSA3TheLoopWaitsForItsBackgroundCommand(t *testing.T) {
	if os.Getenv("SPOV_PROBE") == "" && os.Getenv("SPOV_PROBE_OPENCODE") == "" {
		t.Skip("set SPOV_PROBE=1 to run the spike")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	model := newTaggedModel(t, func(q taggedRequest) taggedAnswer {
		switch {
		case !q.offered:
			return taggedAnswer{text: "title"}
		case strings.Contains(q.latest, opencode.LoopStoppedMarker):
			return taggedAnswer{text: "stopped"}
		// The command's result, whose output only it carries (the
		// request is JSON, where "<" is escaped).
		case strings.Contains(q.latest, "built-ok"):
			return taggedAnswer{tool: opencode.CompletionTool, args: `{"summary":"built"}`}
		case strings.Contains(q.latest, "Continue working"):
			return taggedAnswer{text: "continued"}
		case q.tools > 0:
			return taggedAnswer{text: "waiting"}
		case q.tag == "JOB-B":
			return taggedAnswer{tool: "shell", args: `{"command":"sleep 6; echo built-ok","background":true}`}
		}
		return taggedAnswer{text: "ok"}
	})
	j := startJobProbe(ctx, t, model, opencode.Plugin{Kind: "report", Goal: "JOB-B build in the background"})
	if !probeWait(ctx, func() bool { return len(j.evidence("loop.stopped")) == 1 }) {
		t.Fatalf("the job never ended; model requests:\n%s\nevidence:\n%s", model.log(), j.evidenceLog())
	}
	answers := []string{}
	for _, line := range strings.Split(model.log(), "\n") {
		if strings.HasPrefix(line, "JOB-B") {
			_, a, _ := strings.Cut(line, "-> ")
			answers = append(answers, a)
		}
	}
	answers = slices.DeleteFunc(answers, func(a string) bool { return a == "title" })
	t.Logf("the job's model requests, in order: %v", answers)
	want := []string{"shell", "waiting", opencode.CompletionTool}
	if len(answers) < len(want) || !slices.Equal(answers[:len(want)], want) {
		t.Errorf("the job's requests were %v, want %v: a continuation went out while the command ran", answers, want)
	}
	waiting := j.evidence("continue.waiting")
	if len(waiting) != 1 || waiting[0]["shells"] != float64(1) {
		t.Errorf("continue.waiting = %v, want one with one background command; evidence:\n%s", waiting, j.evidenceLog())
	}
	if len(j.evidence("shell.backgrounded")) != 1 || len(j.evidence("shell.reported"))+len(j.evidence("shell.exited")) == 0 {
		t.Errorf("the command was not followed from its call to its end; evidence:\n%s", j.evidenceLog())
	}
	if got := j.evidence("loop.stopped"); got[0]["reason"] != opencode.CompletionReason {
		t.Errorf("the run stopped as %v", got)
	}
}
