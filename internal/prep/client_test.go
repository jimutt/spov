package prep

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The client's own half, driven against a local server rather than a
// provider: the wire format IS the interface here, so what this asserts
// is the request spov actually puts on it.
//
// @decision(DL-047)

// captured is what the fake endpoint saw.
type captured struct {
	path  string
	auth  string
	body  map[string]any
	calls int
}

// endpoint serves one chat completion and records the request.
func endpoint(t *testing.T, reply string, status int) (*httptest.Server, *captured) {
	t.Helper()
	got := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.calls++
		got.path = r.URL.Path
		got.auth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got.body)
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"message":"nope"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":` +
			mustJSON(reply) + `}}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func mustJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func clientFor(t *testing.T, url string) Completer {
	t.Helper()
	c, err := NewClient(Selection{BaseURL: url, Model: "test-model", Key: "sk-test"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The request spov sends: chat completions, the configured model, both
// messages, and a bound on what it may cost.
//
// @decision(DL-047)
func TestTheRequestIsAnOrdinaryChatCompletion(t *testing.T) {
	srv, got := endpoint(t, "the answer", http.StatusOK)
	out, err := clientFor(t, srv.URL).Complete(context.Background(), "sys", "usr")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out != "the answer" {
		t.Errorf("Complete = %q", out)
	}
	if !strings.HasSuffix(got.path, "/chat/completions") {
		t.Errorf("path = %q, want the chat completions route", got.path)
	}
	if got.auth != "Bearer sk-test" {
		t.Errorf("Authorization = %q", got.auth)
	}
	if got.body["model"] != "test-model" {
		t.Errorf("model = %v, want the configured one", got.body["model"])
	}
	// The cost bound must actually be sent. Without it a model that
	// misunderstands the task is paid for by the token.
	if got.body["max_completion_tokens"] == nil {
		t.Error("no output bound was sent; the pass would be unbounded in cost")
	}
	msgs, _ := got.body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("sent %d messages, want the system and the user one", len(msgs))
	}
}

// A base URL is honoured, which is what reaches OpenRouter, Gemini's
// compatibility endpoint and a LiteLLM gateway with one client.
//
// @decision(DL-047)
func TestTheBaseURLIsWhereTheRequestGoes(t *testing.T) {
	srv, got := endpoint(t, "x", http.StatusOK)
	if _, err := clientFor(t, srv.URL).Complete(context.Background(), "s", "u"); err != nil {
		t.Fatal(err)
	}
	if got.calls != 1 {
		t.Errorf("the endpoint saw %d calls, want 1", got.calls)
	}
}

// The SDK retries by default, which would multiply a slow provider into
// a job waiting several times the timeout before it opens. One attempt,
// then the job opens on its goal.
//
// @decision(DL-047)
func TestAFailedCallIsNotRetried(t *testing.T) {
	srv, got := endpoint(t, "", http.StatusInternalServerError)
	if _, err := clientFor(t, srv.URL).Complete(context.Background(), "s", "u"); err == nil {
		t.Fatal("Complete returned no error for a 500")
	}
	if got.calls != 1 {
		t.Errorf("the endpoint saw %d calls; the pass must not retry and delay the job", got.calls)
	}
}

// A 200 carrying no choices is a provider that answered without
// answering. It must be an error rather than an empty assessment, which
// would be indistinguishable from a model with nothing to say.
//
// @decision(DL-047)
func TestAnAnswerWithNoChoicesIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	t.Cleanup(srv.Close)
	if _, err := clientFor(t, srv.URL).Complete(context.Background(), "s", "u"); err == nil {
		t.Error("Complete accepted an answer with no choices")
	}
}

// The whole pass over a live endpoint, so the request and the parsing
// are exercised together rather than only against a fake Completer.
//
// @decision(DL-047)
func TestAPassAgainstAServerReadsTheAssessment(t *testing.T) {
	srv, got := endpoint(t, "```json\n{\"summary\":\"Fix a test.\",\"check\":\"go test ./...\"}\n```", http.StatusOK)
	a, err := Pass(context.Background(), clientFor(t, srv.URL), "fix the flaky test")
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if a.Summary != "Fix a test." || a.Check != "go test ./..." {
		t.Errorf("Assessment = %+v", a)
	}
	// The goal reaches the model inside the request, not the system
	// prompt.
	msgs, _ := got.body["messages"].([]any)
	last, _ := msgs[len(msgs)-1].(map[string]any)
	if content, _ := last["content"].(string); !strings.Contains(content, "fix the flaky test") {
		t.Errorf("the user message does not carry the goal: %q", content)
	}
}

// TestAReplyCutOffByTheBoundSaysSo pins that a reply truncated by the
// output bound is named as such, rather than reported as unparseable.
//
// @decision(DL-047)
func TestAReplyCutOffByTheBoundSaysSo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"length","message":{"role":"assistant","content":""}}]}`))
	}))
	t.Cleanup(srv.Close)

	_, err := clientFor(t, srv.URL).Complete(context.Background(), "s", "u")
	if err == nil {
		t.Fatal("Complete accepted an empty truncated answer")
	}
	if !strings.Contains(err.Error(), "output bound") {
		t.Errorf("error = %q, which does not name the bound as the cause", err)
	}
}

// A truncated reply that nonetheless carries usable content is used:
// the parser cuts to the outer braces, so a trailing cut-off sentence
// costs nothing.
//
// @decision(DL-047)
func TestATruncatedReplyWithContentIsStillRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"length","message":{"role":"assistant",` +
			`"content":"{\"summary\":\"S\",\"check\":\"\"} and then it was cut"}}]}`))
	}))
	t.Cleanup(srv.Close)

	a, err := Pass(context.Background(), clientFor(t, srv.URL), "g")
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if a.Summary != "S" {
		t.Errorf("Assessment = %+v", a)
	}
}

// A redirect is the one way this credential could be carried to a host
// it was not minted for, so it is refused rather than followed — the
// same rule the sandbox requests follow.
//
// @decision(DL-047)
func TestARedirectIsNotFollowed(t *testing.T) {
	reached := false
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		if r.Header.Get("Authorization") != "" {
			t.Error("the key was carried to a host it was not minted for")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"x"}}]}`))
	}))
	t.Cleanup(elsewhere.Close)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, elsewhere.URL+"/chat/completions", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)

	if _, err := clientFor(t, srv.URL).Complete(context.Background(), "s", "u"); err == nil {
		t.Error("Complete followed a redirect")
	}
	if reached {
		t.Error("the request reached the redirect target")
	}
}

// A failing provider's error must not carry the key into a log. The
// supervisor redacts, but the error should not need redacting.
//
// @decision(DL-047)
func TestAnErrorDoesNotCarryTheKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A provider that echoes the request back in its error, which
		// is the realistic way a key reaches an error string.
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key: ` + r.Header.Get("Authorization") + `"}}`))
	}))
	t.Cleanup(srv.Close)

	_, err := clientFor(t, srv.URL).Complete(context.Background(), "s", "u")
	if err == nil {
		t.Fatal("Complete accepted a 401")
	}
	t.Logf("error text: %s", err)
	if strings.Contains(err.Error(), "sk-test") {
		t.Error("the error carries the key; anything logging it would leak the credential")
	}
}
