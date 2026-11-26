package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/opencode/contract"
	"github.com/jimutt/spov/internal/store"
)

// Frames and stored messages measured by spike S-OI1 at 2.3.0, with the
// probe's temporary paths rewritten to the workspace.
const (
	backgroundShellFrame = `{"id":"evt_20393ab1d001PxJQEo1O7FzKAJ","created":1795356338973,"type":"session.inbox.enqueued","data":{"inboxID":"msg_20393a730001n1YwKhLrKRzQGi","sessionID":"ses_dfc6c5ec6ffeMYEJLJ7hAmFL76","item":{"type":"synthetic","payload":{"text":"<shell id=\"sh_20393a6dc001QqPWQLrGruFs0m\" state=\"completed\" command=\"sleep 1; printf 'built\\n'; exit 3\">\nbuilt\n\n\nExit code: 3\n</shell>","description":"sleep 1; printf 'built\\n'; exit 3","metadata":{"source":"shell","shellID":"sh_20393a6dc001QqPWQLrGruFs0m","jobID":"sh_20393a6dc001QqPWQLrGruFs0m","state":"completed","truncated":false,"exit":3}},"delivery":"steer"}},"durable":{"aggregateID":"ses_dfc6c5ec6ffeMYEJLJ7hAmFL76","seq":32,"version":1}}`
	personShellFrame     = `{"id":"evt_20393abdb002NJt4ytLW0xkwus","created":1795356339163,"type":"session.inbox.enqueued","location":{"directory":"/workspace"},"data":{"inboxID":"msg_20393abdb001Qna9MO4308VFnd","sessionID":"ses_dfc6c5ec6ffeMYEJLJ7hAmFL76","item":{"type":"synthetic","payload":{"text":"The following shell command was executed by the user:\n<shell id=\"sh_20393abce001ZQNsRdmdTHQiTj\" state=\"completed\" command=\"echo by-hand; exit 2\">\nby-hand\n\n\nExit code: 2\n</shell>","metadata":{"source":"shell","shellID":"sh_20393abce001ZQNsRdmdTHQiTj","state":"completed","truncated":false,"exit":2}},"delivery":"steer"}},"durable":{"aggregateID":"ses_dfc6c5ec6ffeMYEJLJ7hAmFL76","seq":44,"version":1}}`
	instructionsFrame    = `{"id":"evt_20393a77b001pMsYwnYX0cv7l5","created":1795356338043,"type":"session.synthetic","location":{"directory":"/workspace"},"data":{"sessionID":"ses_dfc6c5ec6ffeMYEJLJ7hAmFL76","text":"Instructions from: /workspace/app/AGENTS.md\n# Probe repository\n\nRun ` + "`./check.sh`" + ` before you finish.\n","description":"Loaded app/AGENTS.md","metadata":{"instruction":{"paths":["/workspace/app/AGENTS.md"]}}},"durable":{"aggregateID":"ses_dfc6c5ec6ffeMYEJLJ7hAmFL76","seq":22,"version":1}}`
	spovsOwnFrame        = `{"id":"evt_20393abde001909bXfJhabRh2j","created":1795356339166,"type":"session.inbox.enqueued","data":{"inboxID":"msg_01a20393abdcspike0002","sessionID":"ses_dfc6c5ec6ffeMYEJLJ7hAmFL76","item":{"type":"synthetic","payload":{"text":"Continue working toward the goal."},"delivery":"queue"}},"durable":{"aggregateID":"ses_dfc6c5ec6ffeMYEJLJ7hAmFL76","seq":45,"version":1}}`

	backgroundShellMessage = `{"id":"msg_20393a730001n1YwKhLrKRzQGi","metadata":{"source":"shell","shellID":"sh_20393a6dc001QqPWQLrGruFs0m","jobID":"sh_20393a6dc001QqPWQLrGruFs0m","state":"completed","truncated":false,"exit":3},"time":{"created":1795356338983},"text":"<shell id=\"sh_20393a6dc001QqPWQLrGruFs0m\" state=\"completed\" command=\"sleep 1; printf 'built\\n'; exit 3\">\nbuilt\n\n\nExit code: 3\n</shell>","description":"sleep 1; printf 'built\\n'; exit 3","type":"synthetic"}`
	instructionsMessage    = `{"id":"msg_20393a77b001pMsYwnYX0cv7l5","metadata":{"instruction":{"paths":["/workspace/app/AGENTS.md"]}},"time":{"created":1795356338043},"text":"Instructions from: /workspace/app/AGENTS.md\n# Probe repository\n","description":"Loaded app/AGENTS.md","type":"synthetic"}`
	spovsOwnMessage        = `{"id":"msg_01a20393abdcspike0002","time":{"created":1795356339223},"text":"Continue working toward the goal.","type":"synthetic"}`
)

// What OpenCode adds to a conversation by itself is named on the wire,
// in the log and in the transcript alike, and a shell result is served
// as its output without OpenCode's wrapper; what spov sends is not.
//
// @decision(DL-180)
func TestWhatOpenCodeAddedIsNamedOnTheWire(t *testing.T) {
	s := &Server{deps: Deps{Store: store.NewMemory()}}
	names := &conversationNames{byID: map[string]string{}, list: func(context.Context) ([]store.Conversation, error) { return nil, nil }}
	logOf := func(name, raw string) LogEvent {
		t.Helper()
		ev := store.Event{Source: store.SourceOpenCode, Name: name, Raw: contract.Frame(t, raw)}
		var frame map[string]any
		if err := json.Unmarshal(ev.Raw, &frame); err != nil {
			t.Fatal(err)
		}
		projected, _ := projectFrame(frame, time.Now())
		ev.Text = projected.Text
		return s.logEvent(context.Background(), names, ev)
	}

	bg := logOf(jobs.EventInboxEnqueued, backgroundShellFrame)
	if bg.Role != "synthetic" || bg.Injected == nil || bg.Injected.Kind != "shell" || bg.Injected.Shell == nil {
		t.Fatalf("a background shell result read as %+v", bg)
	}
	if sh := bg.Injected.Shell; sh.Command != "sleep 1; printf 'built\\n'; exit 3" || sh.State != "completed" || sh.Exit == nil || *sh.Exit != 3 || sh.ByPerson {
		t.Errorf("the background result's parts: %+v", sh)
	}
	if bg.Text != "built\n\n\nExit code: 3" {
		t.Errorf("the background result's text: %q", bg.Text)
	}

	hand := logOf(jobs.EventInboxEnqueued, personShellFrame)
	if hand.Injected == nil || hand.Injected.Shell == nil || !hand.Injected.Shell.ByPerson || hand.Injected.Shell.Command != "echo by-hand; exit 2" {
		t.Fatalf("a person's shell result read as %+v", hand.Injected)
	}
	if hand.Text != "by-hand\n\n\nExit code: 2" || hand.Injected.Summary != "" {
		t.Errorf("a person's shell result: text %q, summary %q", hand.Text, hand.Injected.Summary)
	}

	in := logOf(syntheticEvent, instructionsFrame)
	if in.Role != "synthetic" || in.Injected == nil || in.Injected.Kind != "instructions" || in.Injected.Summary != "Loaded app/AGENTS.md" {
		t.Fatalf("an instructions frame read as %+v", in)
	}
	if !strings.HasPrefix(in.Text, "Instructions from: /workspace/app/AGENTS.md\n") {
		t.Errorf("an instructions frame's text: %q", in.Text)
	}

	own := logOf(jobs.EventInboxEnqueued, spovsOwnFrame)
	if own.Role != "synthetic" || own.Injected != nil || own.Text != "Continue working toward the goal." {
		t.Errorf("spov's own synthetic read as %+v", own)
	}

	for name, raw := range map[string]string{
		"background shell": backgroundShellMessage,
		"instructions":     instructionsMessage,
	} {
		m := messageViewOf(store.Message{Role: "synthetic", Text: "raw", Raw: []byte(raw)})
		if m.Injected == nil {
			t.Errorf("the stored %s message is not named injected", name)
		}
	}
	m := messageViewOf(store.Message{Role: "synthetic", Text: projectedText(t, backgroundShellMessage), Raw: []byte(backgroundShellMessage)})
	if m.Text != bg.Text {
		t.Errorf("the transcript serves %q for the shell result and the log %q; the page joins them by text", m.Text, bg.Text)
	}
	if m := messageViewOf(store.Message{Role: "synthetic", Text: "Continue working toward the goal.", Raw: []byte(spovsOwnMessage)}); m.Injected != nil {
		t.Errorf("spov's own stored synthetic read as injected: %+v", m.Injected)
	}
	if m := messageViewOf(store.Message{Role: "synthetic", Raw: []byte(reportMessage)}); m.Injected != nil || m.Report == nil {
		t.Errorf("a subagent's report read as injected %+v, report %+v", m.Injected, m.Report)
	}
	if m := messageViewOf(store.Message{Role: "user", Text: "hi", Raw: []byte(`{"type":"user","text":"hi","metadata":{"source":"shell"}}`)}); m.Injected != nil {
		t.Errorf("a person's message read as injected: %+v", m.Injected)
	}
	if got, _ := storedInjected(store.Event{Source: store.SourceSpov, Name: syntheticEvent, Raw: []byte(instructionsFrame)}, "synthetic"); got != nil {
		t.Errorf("spov's own row read as injected: %+v", got)
	}
}

// projectedText is the text the recorder stores for a message.
func projectedText(t *testing.T, raw string) string {
	t.Helper()
	ms := parseMessages([]byte(`{"data":[` + raw + `]}`))
	if len(ms) != 1 {
		t.Fatalf("parsing %s: %d messages", raw, len(ms))
	}
	return ms[0].Text
}

// A shell result's parts come from the metadata and the description, so
// a command that holds the wrapper's own quotes is not cut short, and
// text that is not in the wrapper's shape is kept whole.
//
// @decision(DL-180)
func TestAShellResultIsReadFromItsMetadataAndItsWrapper(t *testing.T) {
	shell := map[string]any{"source": "shell", "state": "completed", "exit": float64(0)}
	for _, tc := range []struct {
		name       string
		parts      syntheticParts
		command    string
		text       string
		exit       *int
		byPerson   bool
		unwrapped  bool
		wantSource string
	}{
		{
			name:    "a command holding the wrapper's closing quote",
			parts:   syntheticParts{Description: `echo '">'`, Metadata: shell, Text: "<shell id=\"sh_1\" state=\"completed\" command=\"echo '\">'\">\n\">\n</shell>"},
			command: `echo '">'`, text: `">`, exit: intp(0), unwrapped: true,
		},
		{
			name:    "a background command in 2.4's wrapper",
			parts:   syntheticParts{Description: "sleep 1; exit 3", Metadata: shell, Text: "<background-result kind=\"shell\" id=\"sh_7\" state=\"completed\" command=\"sleep 1; exit 3\">\nbuilt\n\n\nExit code: 3\n</background-result>"},
			command: "sleep 1; exit 3", text: "built\n\n\nExit code: 3", exit: intp(0), unwrapped: true,
		},
		{
			name:    "a person's command in 2.4's wrapper",
			parts:   syntheticParts{Metadata: shell, Text: shellPreamble + "<background-result kind=\"shell\" id=\"sh_8\" state=\"completed\" command=\"true\">\n(no output)\n</background-result>"},
			command: "true", text: "(no output)", exit: intp(0), byPerson: true, unwrapped: true,
		},
		{
			name:    "a person's command, read from the wrapper",
			parts:   syntheticParts{Metadata: shell, Text: shellPreamble + "<shell id=\"sh_2\" state=\"completed\" command=\"ls\">\na\nb\n</shell>"},
			command: "ls", text: "a\nb", exit: intp(0), byPerson: true, unwrapped: true,
		},
		{
			name:    "a person's command that failed to start",
			parts:   syntheticParts{Metadata: map[string]any{"source": "shell", "state": "error"}, Text: shellNotStarted + "nope\n\nENOENT"},
			command: "nope", text: "ENOENT", byPerson: true,
		},
		{
			name:    "a command with no output, its wrapper's lines apart",
			parts:   syntheticParts{Description: "true", Metadata: shell, Text: "<shell id=\"sh_4\" state=\"completed\" command=\"true\">\n\n</shell>"},
			command: "true", text: "", exit: intp(0),
		},
		{
			name:    "a command with no output, its wrapper's lines together",
			parts:   syntheticParts{Description: "true", Metadata: shell, Text: "<shell id=\"sh_5\" state=\"completed\" command=\"true\">\n</shell>"},
			command: "true", text: "", exit: intp(0),
		},
		{
			name:    "a description that differs from the wrapper's attribute",
			parts:   syntheticParts{Description: `echo "hi"`, Metadata: shell, Text: "<shell id=\"sh_6\" state=\"completed\" command=\"echo &quot;hi&quot;\">\nhi\n</shell>"},
			command: `echo "hi"`, text: "hi", exit: intp(0),
		},
		{
			name:    "a wrapper with no end",
			parts:   syntheticParts{Description: "make", Metadata: shell, Text: "<shell id=\"sh_3\" state=\"completed\" command=\"make\">\nhalf"},
			command: "make", text: "<shell id=\"sh_3\" state=\"completed\" command=\"make\">\nhalf", exit: intp(0),
		},
		{
			name:    "a fractional exit",
			parts:   syntheticParts{Description: "x", Metadata: map[string]any{"source": "shell", "exit": 1.5}, Text: "out"},
			command: "x", text: "out",
		},
	} {
		in, text := injectedOf(tc.parts, tc.parts.Text)
		if in == nil || in.Kind != "shell" || in.Shell == nil {
			t.Errorf("%s: read as %+v", tc.name, in)
			continue
		}
		if in.Shell.Command != tc.command || text != tc.text || in.Shell.ByPerson != tc.byPerson {
			t.Errorf("%s: command %q text %q by person %v", tc.name, in.Shell.Command, text, in.Shell.ByPerson)
		}
		if (in.Shell.Exit == nil) != (tc.exit == nil) || (tc.exit != nil && *in.Shell.Exit != *tc.exit) {
			t.Errorf("%s: exit %v, want %v", tc.name, in.Shell.Exit, tc.exit)
		}
	}
}

// Anything OpenCode marks is injected, a kind it has not named included,
// and its description is served as one bounded line.
//
// @decision(DL-180)
func TestAnythingOpenCodeMarksIsInjected(t *testing.T) {
	restart, _ := injectedOf(syntheticParts{Description: "Continuing after restart", Metadata: map[string]any{"notice": "restart"}}, "x")
	if restart == nil || restart.Kind != "other" || restart.Summary != "Continuing after restart" {
		t.Errorf("a restart notice read as %+v", restart)
	}
	described, _ := injectedOf(syntheticParts{Description: "a plugin's\nsecond line"}, "x")
	if described == nil || described.Summary != "a plugin's" {
		t.Errorf("a described synthetic read as %+v", described)
	}
	long, _ := injectedOf(syntheticParts{Description: strings.Repeat("é", 300)}, "x")
	if n := len([]rune(long.Summary)); n != maxInjectedSummary {
		t.Errorf("a long summary kept %d characters", n)
	}
	if bare, text := injectedOf(syntheticParts{Metadata: map[string]any{}}, "x"); bare != nil || text != "x" {
		t.Errorf("a synthetic with empty metadata read as %+v", bare)
	}
}

// spov's own synthetics carry neither metadata nor a description, which
// is what keeps them from reading as OpenCode's.
//
// @decision(DL-180)
func TestTheOpeningGoalIsABareSynthetic(t *testing.T) {
	var c openCapture
	srv := newOpenServer(t, &c)
	defer srv.Close()
	if err := openJob(context.Background(), testConn(srv.URL), "conv_1", store.Job{ID: "job_1", Goal: "g"}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"metadata", "description"} {
		if _, ok := c.body[k]; ok {
			t.Errorf("the opening goal sets %s: %v", k, c.body)
		}
	}
}
