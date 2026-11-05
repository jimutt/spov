//go:build integration

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/store"
)

// Spike S-Q1: how the pinned OpenCode build orders and consumes
// inbox items. A queued user prompt against a steered one, mid-turn and
// idle; a queued item against a synthetic continuation; an interrupt
// with queued items pending; cancelling and re-moding a pending item;
// and which events say an item was consumed. Local, like the probe: the
// pinned binary, a scripted fake model, no plugin, no sandbox, no key.
//
//	SPOV_PROBE=1 go test -tags integration ./internal/server/ -run SQ1 -v
func TestIntegrationSQ1InboxOrdering(t *testing.T) {
	if os.Getenv("SPOV_PROBE") == "" && os.Getenv("SPOV_PROBE_OPENCODE") == "" {
		t.Skip("set SPOV_PROBE=1 to run the inbox spike")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	bin := probeBinary(ctx, t)
	model := newScriptedModel(t)
	work := t.TempDir()
	if err := os.WriteFile(work+"/note.txt", []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	conn := startProbeServerWith(ctx, t, bin, model.URL, work, "")
	if _, err := opencode.NewClient(conn.URL, conn.Password).WaitHealthy(ctx, 100*time.Millisecond); err != nil {
		t.Fatalf("the server never answered as healthy: %v", err)
	}
	frames := followRawFrames(ctx, t, conn)

	fresh := func(t *testing.T, letter string) (string, *spikeRun) {
		t.Helper()
		id, err := createConversation(ctx, conn, work)
		if err != nil {
			t.Fatalf("creating the conversation: %v", err)
		}
		if err := setConversationModel(ctx, conn, id, "probe/m"); err != nil {
			t.Fatalf("setting the model: %v", err)
		}
		spikeSessions.Store(letter, id)
		return id, &spikeRun{t: t, ctx: ctx, conn: conn, id: id, frames: frames, model: model, start: time.Now()}
	}

	// A: a queued prompt sent mid-turn. When does the model first see it,
	// and does a turn start for it by itself once the running one ends?
	t.Run("QueueMidTurn", func(t *testing.T) {
		_, r := fresh(t, "A")
		r.prompt("A0 WORK4", "steer", true)
		r.waitModelSees("A0", 20*time.Second)
		time.Sleep(1200 * time.Millisecond)
		r.prompt("A1 queued", "queue", true)
		r.settle(25 * time.Second)
		r.report()
	})

	// B: the same with steer.
	t.Run("SteerMidTurn", func(t *testing.T) {
		_, r := fresh(t, "B")
		r.prompt("B0 WORK4", "steer", true)
		r.waitModelSees("B0", 20*time.Second)
		time.Sleep(1200 * time.Millisecond)
		r.prompt("B1 steered", "steer", true)
		r.settle(25 * time.Second)
		r.report()
	})

	// C: a queued prompt, then a synthetic continuation (the plugin's
	// shape: resume true, no delivery), both mid-turn. Which goes first?
	t.Run("QueueThenSyntheticMidTurn", func(t *testing.T) {
		_, r := fresh(t, "C")
		r.prompt("C0 WORK4", "steer", true)
		r.waitModelSees("C0", 20*time.Second)
		time.Sleep(1200 * time.Millisecond)
		r.prompt("C1 queued", "queue", true)
		time.Sleep(300 * time.Millisecond)
		r.synthetic("C2 synthetic", "", true)
		r.settle(30 * time.Second)
		r.report()
	})

	// C': the plugin's real moment: the continuation is sent on
	// execution.succeeded, while a queued prompt is pending.
	t.Run("SyntheticAtTurnEndWithQueuePending", func(t *testing.T) {
		_, r := fresh(t, "D")
		r.prompt("D0 WORK3", "steer", true)
		r.waitModelSees("D0", 20*time.Second)
		time.Sleep(1200 * time.Millisecond)
		r.prompt("D1 queued", "queue", true)
		r.waitFrame("session.execution.succeeded", 30*time.Second)
		r.synthetic("D2 continuation", "", true)
		r.settle(30 * time.Second)
		r.report()
	})

	// E: two queued prompts mid-turn: one turn each, or one turn for both?
	t.Run("TwoQueuedMidTurn", func(t *testing.T) {
		_, r := fresh(t, "E")
		r.prompt("E0 WORK3", "steer", true)
		r.waitModelSees("E0", 20*time.Second)
		time.Sleep(1200 * time.Millisecond)
		r.prompt("E1 queued first", "queue", true)
		r.prompt("E2 queued second", "queue", true)
		r.settle(30 * time.Second)
		r.report()
	})

	// F: interrupt with resume=true while a queued prompt is pending.
	// The document says queued prompts stay parked: what wakes them?
	t.Run("InterruptResumeWithQueuePending", func(t *testing.T) {
		_, r := fresh(t, "F")
		r.prompt("F0 WORK6", "steer", true)
		r.waitModelSees("F0", 20*time.Second)
		time.Sleep(1200 * time.Millisecond)
		r.prompt("F1 queued", "queue", true)
		time.Sleep(300 * time.Millisecond)
		r.interrupt(true)
		r.settle(8 * time.Second)
		r.inbox("after the interrupt")
		r.prompt("F2 steered after", "steer", true)
		r.settle(15 * time.Second)
		r.inbox("after a later steer")
		r.report()
	})

	// G: interrupt with resume=false and a queued prompt pending, then a
	// PATCH of the pending item to steer.
	t.Run("InterruptHoldThenPatchToSteer", func(t *testing.T) {
		_, r := fresh(t, "G")
		r.prompt("G0 WORK6", "steer", true)
		r.waitModelSees("G0", 20*time.Second)
		time.Sleep(1200 * time.Millisecond)
		qid := r.prompt("G1 queued", "queue", true)
		time.Sleep(300 * time.Millisecond)
		r.interrupt(false)
		r.settle(8 * time.Second)
		r.inbox("after the interrupt")
		r.patch(qid, "steer")
		r.settle(15 * time.Second)
		r.inbox("after the patch")
		r.report()
	})

	// H: cancel a pending queued prompt mid-turn. Which event says so?
	t.Run("CancelQueued", func(t *testing.T) {
		_, r := fresh(t, "H")
		r.prompt("H0 WORK4", "steer", true)
		r.waitModelSees("H0", 20*time.Second)
		time.Sleep(1200 * time.Millisecond)
		qid := r.prompt("H1 queued then cancelled", "queue", true)
		time.Sleep(300 * time.Millisecond)
		r.inbox("before the cancel")
		r.cancel(qid)
		r.inbox("after the cancel")
		r.cancel(qid)
		r.settle(25 * time.Second)
		r.report()
	})

	// I: a queued prompt with resume=false on an idle session, then one
	// with resume=true. Does a queued item on an idle session start a
	// turn, and does resume=false hold it?
	t.Run("QueueOnIdle", func(t *testing.T) {
		_, r := fresh(t, "I")
		r.prompt("I0 queued resume false", "queue", false)
		r.settle(5 * time.Second)
		r.inbox("after resume=false")
		r.prompt("I1 queued resume true", "queue", true)
		r.settle(10 * time.Second)
		r.inbox("after resume=true")
		r.report()
	})

	// J: a steered prompt during one long tool call: does it wait for the
	// tool to finish, and does it end the turn?
	t.Run("SteerDuringALongStep", func(t *testing.T) {
		_, r := fresh(t, "J")
		r.prompt("J0 WORK2 SLOW", "steer", true)
		r.waitModelSees("J0", 20*time.Second)
		time.Sleep(500 * time.Millisecond)
		r.prompt("J1 steered", "steer", true)
		r.settle(30 * time.Second)
		r.report()
	})

	// L: metadata on a queued prompt: does the enqueued event carry it,
	// and is it kept on the delivered message?
	t.Run("QueuedMetadata", func(t *testing.T) {
		_, r := fresh(t, "L")
		r.prompt("L0 WORK3", "steer", true)
		r.waitModelSees("L0", 20*time.Second)
		time.Sleep(1200 * time.Millisecond)
		r.promptWith("L1 queued with metadata", "queue", true, map[string]any{"spov": map[string]any{"after": "handover"}})
		r.settle(30 * time.Second)
		r.messages()
		r.report()
	})

	// M: a queued prompt pending when the turn is interrupted with
	// resume=true, then the session left alone: does it ever run?
	t.Run("InterruptResumeThenIdle", func(t *testing.T) {
		_, r := fresh(t, "M")
		r.prompt("M0 WORK6", "steer", true)
		r.waitModelSees("M0", 20*time.Second)
		time.Sleep(1200 * time.Millisecond)
		r.prompt("M1 queued", "queue", true)
		time.Sleep(300 * time.Millisecond)
		r.interrupt(true)
		time.Sleep(6 * time.Second)
		r.inbox("six seconds after the interrupt")
		r.synthetic("M2 synthetic continuation", "", true)
		r.settle(15 * time.Second)
		r.inbox("after a synthetic")
		r.report()
	})

	// N: a steered message waiting while a long step runs, then an
	// interrupt with resume=true: is it read at once, in a new execution?
	t.Run("SteerPendingThenInterruptResume", func(t *testing.T) {
		_, r := fresh(t, "N")
		r.prompt("N0 WORK2 SLOW", "steer", true)
		time.Sleep(1500 * time.Millisecond)
		r.prompt("N1 steered while busy", "steer", true)
		time.Sleep(500 * time.Millisecond)
		r.inbox("before the interrupt")
		r.interrupt(true)
		r.settle(15 * time.Second)
		r.inbox("after the interrupt")
		r.report()
		if !r.model.saw(r.id, "N1") {
			t.Error("the steered message was not read after the interrupt")
		}
	})

	// K: a queued synthetic and a queued user prompt, both mid-turn: is a
	// queued synthetic ordered with users by arrival?
	t.Run("QueuedSyntheticAndQueuedUser", func(t *testing.T) {
		_, r := fresh(t, "K")
		r.prompt("K0 WORK3", "steer", true)
		r.waitModelSees("K0", 20*time.Second)
		time.Sleep(1200 * time.Millisecond)
		r.prompt("K1 queued user", "queue", true)
		r.synthetic("K2 queued synthetic", "queue", true)
		r.settle(30 * time.Second)
		r.report()
	})
}

// spikeRun is one conversation of the spike and what it saw.
type spikeRun struct {
	t      *testing.T
	ctx    context.Context
	conn   Connection
	id     string
	frames *rawFrames
	model  *scriptedModel
	start  time.Time
	notes  []string
	mu     sync.Mutex
}

func (r *spikeRun) note(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notes = append(r.notes, fmt.Sprintf("%7.2fs ACT   %s", time.Since(r.start).Seconds(), fmt.Sprintf(format, args...)))
}

func (r *spikeRun) do(method, path string, body any) (int, []byte) {
	r.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(r.ctx, method, r.conn.URL+"/api/session/"+url.PathEscape(r.id)+path, rd)
	if err != nil {
		r.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(r.conn.User, r.conn.Password)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

var spikeMsgID = func() func() string {
	var mu sync.Mutex
	n := 0
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		n++
		// The browser's shape (newMessageID): milliseconds in hex, so
		// OpenCode's own reading of the id's time is right.
		return fmt.Sprintf("%s%012x%s%04d", MessageIDPrefix, time.Now().UnixMilli(), "spike", n)
	}
}()

func (r *spikeRun) prompt(text, delivery string, resume bool) string {
	return r.promptWith(text, delivery, resume, nil)
}

func (r *spikeRun) promptWith(text, delivery string, resume bool, metadata map[string]any) string {
	id := spikeMsgID()
	body := map[string]any{"id": id, "text": text, "resume": resume}
	if metadata != nil {
		body["metadata"] = metadata
	}
	if delivery != "" {
		body["delivery"] = delivery
	}
	code, out := r.do(http.MethodPost, "/prompt", body)
	r.note("prompt %q delivery=%s resume=%v -> %d %s", text, delivery, resume, code, clip(string(out), 200))
	return id
}

func (r *spikeRun) synthetic(text, delivery string, resume bool) string {
	id := spikeMsgID()
	body := map[string]any{"id": id, "text": text, "resume": resume}
	if delivery != "" {
		body["delivery"] = delivery
	}
	code, out := r.do(http.MethodPost, "/synthetic", body)
	r.note("synthetic %q delivery=%q resume=%v -> %d %s", text, delivery, resume, code, clip(string(out), 200))
	return id
}

func (r *spikeRun) interrupt(resume bool) {
	code, out := r.do(http.MethodPost, fmt.Sprintf("/interrupt?resume=%v", resume), nil)
	r.note("interrupt resume=%v -> %d %s", resume, code, clip(string(out), 200))
}

func (r *spikeRun) inbox(when string) {
	code, out := r.do(http.MethodGet, "/inbox", nil)
	r.note("inbox %s -> %d %s", when, code, clip(string(out), 600))
}

func (r *spikeRun) patch(id, delivery string) {
	code, out := r.do(http.MethodPatch, "/inbox/"+url.PathEscape(id), map[string]any{"delivery": delivery})
	r.note("patch %s delivery=%s -> %d %s", id, delivery, code, clip(string(out), 200))
}

func (r *spikeRun) cancel(id string) {
	code, out := r.do(http.MethodDelete, "/inbox/"+url.PathEscape(id), nil)
	r.note("cancel %s -> %d %s", id, code, clip(string(out), 200))
}

func (r *spikeRun) messages() {
	code, out := r.do(http.MethodGet, "/message", nil)
	r.note("messages -> %d %s", code, clip(string(out), 3000))
}

func (r *spikeRun) waitModelSees(tag string, within time.Duration) {
	r.t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if r.model.saw(r.id, tag) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	r.t.Fatalf("the model never saw %s", tag)
}

func (r *spikeRun) waitFrame(name string, within time.Duration) {
	r.t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if r.frames.has(r.id, name, r.start) {
			r.note("saw %s", name)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.t.Fatalf("no %s arrived", name)
}

// settle waits until nothing has happened for two seconds, or within.
func (r *spikeRun) settle(within time.Duration) {
	time.Sleep(time.Second)
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		last := r.frames.last(r.id)
		if m := r.model.last(r.id); m.After(last) {
			last = m
		}
		if !last.IsZero() && time.Since(last) > 2500*time.Millisecond {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// report logs the acts, the frames and the model's requests on one
// clock.
func (r *spikeRun) report() {
	type line struct {
		at   time.Time
		text string
	}
	var lines []line
	r.mu.Lock()
	for _, n := range r.notes {
		var s float64
		_, _ = fmt.Sscanf(strings.TrimSpace(n), "%fs", &s)
		lines = append(lines, line{r.start.Add(time.Duration(s * float64(time.Second))), n})
	}
	r.mu.Unlock()
	for _, f := range r.frames.of(r.id) {
		if f.at.Before(r.start) {
			continue
		}
		lines = append(lines, line{f.at, fmt.Sprintf("%7.2fs EVENT %s%s %s", f.at.Sub(r.start).Seconds(), f.name, map[bool]string{true: "", false: " (ephemeral)"}[f.durable], f.detail)})
	}
	for _, q := range r.model.requests(r.id) {
		lines = append(lines, line{q.at, fmt.Sprintf("%7.2fs MODEL step sees users=%v tools-since-last-user=%d -> %s", q.at.Sub(r.start).Seconds(), q.users, q.tools, q.answer)})
	}
	for i := 1; i < len(lines); i++ {
		for j := i; j > 0 && lines[j].at.Before(lines[j-1].at); j-- {
			lines[j], lines[j-1] = lines[j-1], lines[j]
		}
	}
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.text)
		b.WriteString("\n")
	}
	r.t.Logf("timeline of %s:\n%s", r.id, b.String())
}

func clip(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// rawFrames keeps every event frame with its arrival time.
type rawFrame struct {
	at      time.Time
	name    string
	session string
	durable bool
	detail  string
}

type rawFrames struct {
	mu  sync.Mutex
	all []rawFrame
}

func (p *rawFrames) of(session string) []rawFrame {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []rawFrame
	for _, f := range p.all {
		if f.session == session {
			out = append(out, f)
		}
	}
	return out
}

func (p *rawFrames) has(session, name string, after time.Time) bool {
	for _, f := range p.of(session) {
		if f.name == name && !f.at.Before(after) {
			return true
		}
	}
	return false
}

func (p *rawFrames) last(session string) time.Time {
	fs := p.of(session)
	if len(fs) == 0 {
		return time.Time{}
	}
	return fs[len(fs)-1].at
}

// boringFrames are streamed parts whose detail adds nothing here.
var boringFrames = regexp.MustCompile(`\.(delta|streamed)$`)

func followRawFrames(ctx context.Context, t *testing.T, conn Connection) *rawFrames {
	t.Helper()
	body, err := openSandboxEvents(ctx, conn)
	if err != nil {
		t.Fatalf("opening the event stream: %v", err)
	}
	t.Cleanup(func() { _ = body.Close() })
	p := &rawFrames{}
	go func() {
		_ = readSSEFrames(ctx, body, func(raw string) bool {
			var frame map[string]any
			if json.Unmarshal([]byte(raw), &frame) != nil {
				return true
			}
			name := find(frame, nameKeys)
			session := find(frame, sessionKeys)
			if name == "" || session == "" || boringFrames.MatchString(name) {
				return true
			}
			detail := ""
			if strings.HasPrefix(name, "session.inbox.") || strings.HasPrefix(name, "session.execution.") ||
				strings.HasPrefix(name, "session.input") || strings.Contains(name, "prompt") {
				detail = clip(raw, 700)
			}
			p.mu.Lock()
			p.all = append(p.all, rawFrame{at: time.Now(), name: name, session: session, durable: isDurable(frame), detail: detail})
			p.mu.Unlock()
			return true
		})
	}()
	return p
}

// scriptedModel is an OpenAI-compatible endpoint whose behaviour is set
// by the newest user-role message: "WORKn" makes n steps, each a glob
// call answered after a pause ("SLOW" makes the pause long), then a
// text reply; anything else is answered at once with text. It records
// which tagged messages each request could see.
type modelRequest struct {
	at      time.Time
	session string
	users   []string
	tools   int
	answer  string
}

type scriptedModel struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []modelRequest
}

var spikeTag = regexp.MustCompile(`\b([A-Z][0-9])\b`)

func (m *scriptedModel) saw(session, tag string) bool {
	for _, q := range m.requests(session) {
		for _, u := range q.users {
			if u == tag {
				return true
			}
		}
	}
	return false
}

func (m *scriptedModel) requests(session string) []modelRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []modelRequest
	for _, q := range m.reqs {
		if q.session == session {
			out = append(out, q)
		}
	}
	return out
}

func (m *scriptedModel) last(session string) time.Time {
	qs := m.requests(session)
	if len(qs) == 0 {
		return time.Time{}
	}
	return qs[len(qs)-1].at
}

func newScriptedModel(t *testing.T) *scriptedModel {
	t.Helper()
	m := &scriptedModel{}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"data":[{"id":"m","object":"model"}]}`)
			return
		}
		raw, _ := io.ReadAll(r.Body)
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
		_ = json.Unmarshal(raw, &req)
		// Which conversation: the session id is in the system prompt or
		// nowhere, so the tags are prefixed by scenario letter instead and
		// the session is matched by the first tag's letter.
		var users []string
		latest, tools := "", 0
		for _, msg := range req.Messages {
			switch msg.Role {
			case "user":
				text, _ := json.Marshal(msg.Content)
				for _, tag := range spikeTag.FindAllStringSubmatch(string(text), -1) {
					users = append(users, tag[1])
				}
				latest, tools = string(text), 0
			case "tool":
				tools++
			}
		}
		want, slow := 0, strings.Contains(latest, "SLOW")
		if i := strings.Index(latest, "WORK"); i >= 0 && i+4 < len(latest) {
			want = int(latest[i+4] - '0')
		}
		answer := "text"
		if len(req.Tools) == 0 {
			// The title request: answered at once and not recorded.
			want, answer = 0, "title"
		}
		if tools < want {
			answer = "glob"
			pause := 1500 * time.Millisecond
			if slow {
				pause = 6 * time.Second
			}
			time.Sleep(pause)
		}
		m.mu.Lock()
		if answer != "title" {
			m.reqs = append(m.reqs, modelRequest{at: time.Now(), session: spikeSessionOf(users), users: users, tools: tools, answer: answer})
		}
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
		if answer == "title" {
			chunk(map[string]any{"role": "assistant", "content": "title"}, nil)
			chunk(map[string]any{}, "stop")
		} else if answer == "glob" {
			chunk(map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
				"index": 0, "id": fmt.Sprintf("call_%d", time.Now().UnixNano()), "type": "function",
				"function": map[string]any{"name": "glob", "arguments": `{"pattern":"*.txt"}`},
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
	t.Cleanup(m.Server.Close)
	return m
}

// spikeSessions maps a scenario letter to its conversation, filled as
// each scenario starts; the model cannot see session ids.
var spikeSessions sync.Map

func spikeSessionOf(users []string) string {
	if len(users) == 0 {
		return ""
	}
	if v, ok := spikeSessions.Load(users[0][:1]); ok {
		return v.(string)
	}
	return ""
}

// Spike S-Q1, second part: what the plugin API offers a plugin, two
// levels deep, and whether it can read or change the inbox or interrupt.
//
//	SPOV_PROBE=1 go test -tags integration ./internal/server/ -run SQ1PluginSurface -v
func TestIntegrationSQ1PluginSurface(t *testing.T) {
	if os.Getenv("SPOV_PROBE") == "" && os.Getenv("SPOV_PROBE_OPENCODE") == "" {
		t.Skip("set SPOV_PROBE=1 to run the inbox spike")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	bin := probeBinary(ctx, t)
	model := newScriptedModel(t)
	work := t.TempDir()
	out := t.TempDir() + "/surface.json"
	source := `const { writeFileSync } = await import("node:fs")
function shape(v, depth) {
  if (v === null || typeof v !== "object") return typeof v
  if (depth >= 3) return "object"
  const out = {}
  for (const k of Object.keys(v).sort()) out[k] = shape(v[k], depth + 1)
  let proto = Object.getPrototypeOf(v)
  while (proto && proto !== Object.prototype) {
    for (const k of Object.getOwnPropertyNames(proto)) if (k !== "constructor" && !(k in out)) out[k] = "proto:" + typeof v[k]
    proto = Object.getPrototypeOf(proto)
  }
  return out
}
export default {
  id: "spike.surface",
  async setup(ctx) {
    writeFileSync(` + "`" + `${JSON.stringify(out)}` + "`" + `, JSON.stringify(shape(ctx, 0), null, 1))
  },
}
`
	source = strings.Replace(source, "${JSON.stringify(out)}", strings.ReplaceAll(out, `"`, ""), 1)
	conn := startProbeServerWith(ctx, t, bin, model.URL, work, source)
	if _, err := opencode.NewClient(conn.URL, conn.Password).WaitHealthy(ctx, 100*time.Millisecond); err != nil {
		t.Fatalf("the server never answered as healthy: %v", err)
	}
	// A plugin is loaded with the first instance, which a session makes.
	id, err := createConversation(ctx, conn, work)
	if err != nil {
		t.Fatal(err)
	}
	if err := setConversationModel(ctx, conn, id, "probe/m"); err != nil {
		t.Fatal(err)
	}
	spikeSessions.Store("Z", id)
	(&spikeRun{t: t, ctx: ctx, conn: conn, id: id}).prompt("Z0 hello", "steer", true)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(out); err == nil && len(b) > 0 {
			t.Logf("plugin context:\n%s", b)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("the plugin never wrote its context")
}

// The real plugin at the pin, against DL-052: a message that
// continues leaves the loop running, a handover holds it once delivered,
// and every continuation goes out queued.
//
//	SPOV_PROBE=1 go test -tags integration ./internal/server/ -run SQ1PluginHold -v
//
// @decision(DL-052)
func TestIntegrationSQ1PluginHoldsOnADeliveredHandover(t *testing.T) {
	if os.Getenv("SPOV_PROBE") == "" && os.Getenv("SPOV_PROBE_OPENCODE") == "" {
		t.Skip("set SPOV_PROBE=1 to run the inbox spike")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, f := range []string{opencode.PluginEvidencePath, opencode.PluginOutcomePath} {
		_ = os.Remove(f)
		t.Cleanup(func() { _ = os.Remove(f) })
	}
	bin := probeBinary(ctx, t)
	model := newScriptedModel(t)
	work := t.TempDir()
	source, err := opencode.Plugin{Kind: "report", Goal: "P0 keep going", Dir: work}.Source()
	if err != nil {
		t.Fatal(err)
	}
	conn := startProbeServerWith(ctx, t, bin, model.URL, work, source)
	if _, err := opencode.NewClient(conn.URL, conn.Password).WaitHealthy(ctx, 100*time.Millisecond); err != nil {
		t.Fatalf("the server never answered as healthy: %v", err)
	}
	frames := followRawFrames(ctx, t, conn)
	id, err := createConversation(ctx, conn, work)
	if err != nil {
		t.Fatal(err)
	}
	if err := setConversationModel(ctx, conn, id, "probe/m"); err != nil {
		t.Fatal(err)
	}
	spikeSessions.Store("P", id)
	r := &spikeRun{t: t, ctx: ctx, conn: conn, id: id, frames: frames, model: model, start: time.Now()}
	if err := openJob(ctx, conn, id, store.Job{Goal: "P0 keep going"}); err != nil {
		t.Fatal(err)
	}

	count := func(event string) int {
		b, _ := os.ReadFile(opencode.PluginEvidencePath)
		return strings.Count(string(b), `"event":"`+event+`"`)
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				b, _ := os.ReadFile(opencode.PluginEvidencePath)
				t.Fatalf("timed out waiting for %s; evidence:\n%s", what, b)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	waitFor("two continuations", func() bool { return count("continue.sent") >= 2 })

	r.promptWith("P1 carry on", DeliveryQueue, true, map[string]any{"spov": map[string]any{"after": AfterContinue}})
	before := count("continue.sent")
	waitFor("continuations after a continue message", func() bool { return count("continue.sent") >= before+3 })
	if n := count("loop.held"); n != 0 {
		t.Fatalf("a continue message held the loop")
	}

	r.promptWith("P2 over to me", DeliveryQueue, true, map[string]any{"spov": map[string]any{"after": AfterHandover}})
	waitFor("the hold", func() bool { return count("loop.held") == 1 })
	time.Sleep(time.Second)
	held := count("continue.sent")
	time.Sleep(2 * time.Second)
	if n := count("continue.sent"); n != held {
		t.Errorf("%d continuations went out after the hold", n-held)
	}
	if !model.saw(id, "P2") {
		t.Error("the handover message never reached the model")
	}
	// Every continuation the plugin sent was queued.
	for _, f := range frames.of(id) {
		if f.name == "session.inbox.enqueued" && strings.Contains(f.detail, `"type":"synthetic"`) &&
			strings.Contains(f.detail, "Continue working") && !strings.Contains(f.detail, `"delivery":"queue"`) {
			t.Errorf("a continuation was not queued: %s", f.detail)
		}
	}
}

// Spike S-F1, first part: which tools the pinned build offers the
// model, to find the one that asks the person a question.
//
//	SPOV_PROBE=1 go test -tags integration ./internal/server/ -run SF1Tools -v
func TestIntegrationSF1Tools(t *testing.T) {
	if os.Getenv("SPOV_PROBE") == "" && os.Getenv("SPOV_PROBE_OPENCODE") == "" {
		t.Skip("set SPOV_PROBE=1 to run the spike")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var mu sync.Mutex
	var tools []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"data":[{"id":"m","object":"model"}]}`)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Tools json.RawMessage `json:"tools"`
		}
		_ = json.Unmarshal(raw, &req)
		mu.Lock()
		if len(req.Tools) > len(tools) {
			tools = req.Tools
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"c","object":"chat.completion.chunk","created":0,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	work := t.TempDir()
	conn := startProbeServerWith(ctx, t, probeBinary(ctx, t), srv.URL, work, "")
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
	(&spikeRun{t: t, ctx: ctx, conn: conn, id: id}).prompt("hello", "steer", true)
	time.Sleep(3 * time.Second)
	mu.Lock()
	defer mu.Unlock()
	var list []struct {
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	}
	_ = json.Unmarshal(tools, &list)
	for _, tool := range list {
		t.Logf("%s: %s\n    params: %s", tool.Function.Name, clip(tool.Function.Description, 160), clip(string(tool.Function.Parameters), 600))
	}
}

// Spike S-F1: a permission ask and a question form at the pin, and
// what replying to each does. A plugin makes glob ask; the scripted
// model calls glob on "ASKGLOB" and the question tool on "ASKQ", then
// answers in text once the tool has answered.
//
//	SPOV_PROBE=1 go test -tags integration ./internal/server/ -run SF1Asks -v
func TestIntegrationSF1Asks(t *testing.T) {
	if os.Getenv("SPOV_PROBE") == "" && os.Getenv("SPOV_PROBE_OPENCODE") == "" {
		t.Skip("set SPOV_PROBE=1 to run the spike")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"data":[{"id":"m","object":"model"}]}`)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
			Tools []json.RawMessage `json:"tools"`
		}
		_ = json.Unmarshal(raw, &req)
		latest, toolText := "", ""
		for _, m := range req.Messages {
			b, _ := json.Marshal(m.Content)
			switch m.Role {
			case "user":
				latest, toolText = string(b), ""
			case "tool":
				toolText = string(b)
			}
		}
		mu.Lock()
		if toolText != "" {
			seen = append(seen, "tool result: "+clip(toolText, 300))
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(v any) {
			b, _ := json.Marshal(v)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		}
		chunk := func(delta map[string]any, finish any) {
			send(map[string]any{"id": "c", "object": "chat.completion.chunk", "created": 0, "model": "m",
				"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
		}
		call := func(name, args string) {
			chunk(map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
				"index": 0, "id": fmt.Sprintf("call_%d", time.Now().UnixNano()), "type": "function",
				"function": map[string]any{"name": name, "arguments": args},
			}}}, nil)
			chunk(map[string]any{}, "tool_calls")
		}
		switch {
		case len(req.Tools) == 0 || toolText != "":
			chunk(map[string]any{"role": "assistant", "content": "done"}, nil)
			chunk(map[string]any{}, "stop")
		case strings.Contains(latest, "ASKGLOB"):
			call("glob", `{"pattern":"*.txt"}`)
		case strings.Contains(latest, "ASKQ"):
			call("question", `{"questions":[{"question":"Which colour?","header":"Colour","options":[{"label":"Red","description":"warm"},{"label":"Blue","description":"cool"}],"multiple":false},{"question":"Anything else?","header":"Notes","options":[{"label":"No","description":"nothing"}],"multiple":true}]}`)
		default:
			chunk(map[string]any{"role": "assistant", "content": "ok"}, nil)
			chunk(map[string]any{}, "stop")
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	plugin := `export default {
  id: "spike.ask",
  async setup(ctx) {
    ctx.permission.hook("evaluate", (evaluation) => {
      if (String(evaluation?.action) === "glob") { try { evaluation.effect = "ask" } catch (e) {} }
    })
  },
}
`
	work := t.TempDir()
	_ = os.WriteFile(work+"/a.txt", []byte("x"), 0o600)
	conn := startProbeServerWith(ctx, t, probeBinary(ctx, t), srv.URL, work, plugin)
	if _, err := opencode.NewClient(conn.URL, conn.Password).WaitHealthy(ctx, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	frames := followRawFramesAll(ctx, t, conn)
	newRun := func() *spikeRun {
		id, err := createConversation(ctx, conn, work)
		if err != nil {
			t.Fatal(err)
		}
		if err := setConversationModel(ctx, conn, id, "probe/m"); err != nil {
			t.Fatal(err)
		}
		return &spikeRun{t: t, ctx: ctx, conn: conn, id: id, start: time.Now()}
	}
	dump := func(r *spikeRun, label string) {
		var b strings.Builder
		for _, f := range frames.of(r.id) {
			if strings.Contains(f.name, "delta") || strings.Contains(f.name, "usage") {
				continue
			}
			fmt.Fprintf(&b, "  %s%s %s\n", f.name, map[bool]string{true: "", false: " (ephemeral)"}[f.durable], clip(f.detail, 700))
		}
		r.mu.Lock()
		notes := strings.Join(r.notes, "\n")
		r.mu.Unlock()
		mu.Lock()
		results := strings.Join(seen, "\n  ")
		seen = nil
		mu.Unlock()
		t.Logf("== %s\nacts:\n%s\nframes:\n%s  model saw:\n  %s", label, notes, b.String(), results)
	}
	get := func(r *spikeRun, path string) string {
		code, out := r.do(http.MethodGet, path, nil)
		r.note("GET %s -> %d %s", path, code, clip(string(out), 900))
		return string(out)
	}
	post := func(r *spikeRun, path string, body any) {
		code, out := r.do(http.MethodPost, path, body)
		r.note("POST %s %v -> %d %s", path, body, code, clip(string(out), 400))
	}
	idIn := func(listing, prefix string) string {
		i := strings.Index(listing, `"id":"`+prefix)
		if i < 0 {
			return ""
		}
		rest := listing[i+6:]
		return rest[:strings.Index(rest, `"`)]
	}
	wait := func() { time.Sleep(2 * time.Second) }

	// The listing fails at this build (a 400 on its own response), so the
	// request id is taken from the permission.asked frame, as spov would.
	askedID := func(r *spikeRun) string {
		for _, f := range frames.of(r.id) {
			if f.name == "permission.asked" {
				return idIn(f.detail, "per")
			}
		}
		return ""
	}
	t.Run("PermissionOnce", func(t *testing.T) {
		r := newRun()
		r.prompt("ASKGLOB please", "steer", true)
		wait()
		get(r, "/permission")
		get(r, "/permission/"+askedID(r))
		post(r, "/permission/"+askedID(r)+"/reply", map[string]any{"decision": "once"})
		wait()
		get(r, "/permission")
		dump(r, "permission once")
	})
	t.Run("PermissionRejectWithMessage", func(t *testing.T) {
		r := newRun()
		r.prompt("ASKGLOB please", "steer", true)
		wait()
		post(r, "/permission/"+askedID(r)+"/reply", map[string]any{"decision": "reject", "message": "not that, use read"})
		wait()
		dump(r, "permission reject")
	})
	// What settles a permission nobody answered when the turn is
	// interrupted (DL-020): the page and the supervisor read an ask as
	// pending until something does.
	t.Run("PermissionInterrupted", func(t *testing.T) {
		r := newRun()
		r.prompt("ASKGLOB please", "steer", true)
		wait()
		code, out := r.do(http.MethodPost, "/interrupt?resume=true", nil)
		r.note("POST /interrupt -> %d %s", code, clip(string(out), 200))
		wait()
		post(r, "/permission/"+askedID(r)+"/reply", map[string]any{"decision": "once"})
		wait()
		dump(r, "permission interrupted")
	})
	t.Run("QuestionInterrupted", func(t *testing.T) {
		r := newRun()
		r.prompt("ASKQ please", "steer", true)
		wait()
		code, out := r.do(http.MethodPost, "/interrupt?resume=true", nil)
		r.note("POST /interrupt -> %d %s", code, clip(string(out), 200))
		wait()
		dump(r, "question interrupted")
	})
	t.Run("QuestionAnswered", func(t *testing.T) {
		r := newRun()
		r.prompt("ASKQ please", "steer", true)
		wait()
		list := get(r, "/form")
		fid := idIn(list, "frm")
		if fid != "" {
			get(r, "/form/"+fid)
		}
		post(r, "/form/"+fid+"/reply", map[string]any{"answer": map[string]any{"q0": "Blue", "q1": []string{"No"}}})
		wait()
		get(r, "/form/"+fid)
		dump(r, "question answered")
	})
	t.Run("QuestionCancelled", func(t *testing.T) {
		r := newRun()
		r.prompt("ASKQ please", "steer", true)
		wait()
		list := get(r, "/form")
		fid := idIn(list, "frm")
		code, out := r.do(http.MethodDelete, "/form/"+fid, map[string]any{"message": "not now"})
		r.note("DELETE /form/%s -> %d %s", fid, code, clip(string(out), 300))
		wait()
		dump(r, "question cancelled")
	})
}

// followRawFramesAll is followRawFrames keeping the detail of every
// frame, for the ask spike.
func followRawFramesAll(ctx context.Context, t *testing.T, conn Connection) *rawFrames {
	t.Helper()
	body, err := openSandboxEvents(ctx, conn)
	if err != nil {
		t.Fatalf("opening the event stream: %v", err)
	}
	t.Cleanup(func() { _ = body.Close() })
	p := &rawFrames{}
	go func() {
		_ = readSSEFrames(ctx, body, func(raw string) bool {
			var frame map[string]any
			if json.Unmarshal([]byte(raw), &frame) != nil {
				return true
			}
			name, session := find(frame, nameKeys), find(frame, sessionKeys)
			if name == "" || session == "" {
				return true
			}
			detail := ""
			if strings.HasPrefix(name, "permission") || strings.HasPrefix(name, "form") || strings.Contains(name, "question") ||
				strings.HasPrefix(name, "session.tool") || strings.HasPrefix(name, "session.execution") || strings.HasPrefix(name, "session.model") {
				detail = raw
			}
			p.mu.Lock()
			p.all = append(p.all, rawFrame{at: time.Now(), name: name, session: session, durable: isDurable(frame), detail: detail})
			p.mu.Unlock()
			return true
		})
	}()
	return p
}
