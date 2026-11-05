package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/opencode/contract"
	"github.com/jimutt/spov/internal/proxy"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/store"
)

// Both sandbox readers carry the DL-030 credential pair, and neither may
// carry it anywhere else. A redirect is the one way that could happen.
//
// @decision(DL-018) @decision(DL-030)
func TestSandboxGetCarriesTheCredentialPairAndRefusesARedirect(t *testing.T) {
	var gotCookie, gotUser, gotPass string
	var ok bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the credential followed a redirect to %s", r.URL)
	}))
	defer elsewhere.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session":
			if c, err := r.Cookie(proxy.TokenCookie); err == nil {
				gotCookie = c.Value
			}
			gotUser, gotPass, ok = r.BasicAuth()
			fmt.Fprint(w, `{"data":[]}`)
		case "/away":
			http.Redirect(w, r, elsewhere.URL+"/steal", http.StatusFound)
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	defer srv.Close()

	body, err := sandboxGet(context.Background(), srv.URL+"/api/session", "tok-1", opencode.BasicAuthUser, "pw")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"data":[]}` {
		t.Errorf("body = %s", body)
	}
	if gotCookie != "tok-1" {
		t.Errorf("connect token cookie = %q", gotCookie)
	}
	if !ok || gotUser != opencode.BasicAuthUser || gotPass != "pw" {
		t.Errorf("basic auth = %q/%q (present=%v)", gotUser, gotPass, ok)
	}

	// A redirect is not followed, so the credential stays where it was
	// minted for. The 302 surfaces as a refusal.
	if _, err := sandboxGet(context.Background(), srv.URL+"/away", "tok-1", opencode.BasicAuthUser, "pw"); err == nil {
		t.Error("a redirect was followed")
	}

	// A non-200 is an error, not an empty transcript.
	if _, err := sandboxGet(context.Background(), srv.URL+"/nope", "tok-1", opencode.BasicAuthUser, "pw"); err == nil {
		t.Error("a 418 was read as a body")
	}
}

// Opening the event stream carries the same pair, and hands back a body
// the recorder can read.
//
// @decision(DL-018) @decision(DL-017)
func TestDialingTheSandboxEventStream(t *testing.T) {
	var path, cookie string
	srv := httptest.NewServer(contract.Checked(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if c, err := r.Cookie(proxy.TokenCookie); err == nil {
			cookie = c.Value
		}
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("Accept = %q", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, frame(durableFrame))
	})))
	defer srv.Close()

	f := newAPIFixtureWith(t, func(d *Deps) {
		d.Provider.(*sandbox.Fake).ConnectBaseURL = srv.URL
	})
	sess := runningSession(t, f)
	p := auth.Principal{OrgID: auth.LocalID, UserID: auth.LocalID}

	conn, err := f.srv.svc.Connect(context.Background(), p, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	body, err := openSandboxEvents(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = body.Close() }()
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "session.text.ended") {
		t.Errorf("stream = %s", got)
	}
	// conn.URL already carries the edge's own prefix, so the event path
	// is appended to it rather than replacing it.
	if !strings.HasSuffix(path, "/api/event") {
		t.Errorf("path = %q, want it to end at the event stream", path)
	}
	if cookie == "" {
		t.Error("the stream carried no connect token")
	}
}

// A sandbox that accepts the connection and never answers must not hold
// a browser on an empty stream for the whole lifetime bound.
//
// @decision(DL-018)
func TestDialingGivesUpOnASandboxThatNeverAnswers(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-block }))
	defer srv.Close()
	defer close(block)

	f := newAPIFixtureWith(t, func(d *Deps) {
		d.Provider.(*sandbox.Fake).ConnectBaseURL = srv.URL
	})
	sess := runningSession(t, f)
	p := auth.Principal{OrgID: auth.LocalID, UserID: auth.LocalID}

	// The deadline is the test's own, and short: what is under test is
	// that the dial GIVES UP rather than holding the stream open, and
	// the caller's context is what bounds it. Two seconds proved the
	// same thing and cost two seconds of every run.
	const bound = 150 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	conn, err := f.srv.svc.Connect(ctx, p, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := openSandboxEvents(ctx, conn); err == nil {
		t.Fatal("a silent sandbox produced a usable stream")
	}
	// Generously above the bound: the point is that it returned when
	// the context said so rather than hanging on the sandbox, not a
	// precise figure on a shared CI machine.
	if elapsed := time.Since(start); elapsed > 20*bound {
		t.Errorf("gave up after %s, which is not the caller's deadline doing it", elapsed)
	}
}

// The SSE parser has to survive what a real stream contains: keepalive
// comments, multi-line data, and a frame that is not one the recorder
// stores.
//
// @decision(DL-018) @decision(DL-017)
func TestTheFrameReaderParsesARealisticStream(t *testing.T) {
	stream := ":keepalive\n\n" +
		"event: message\ndata: {\"event\":\"session.text.ended\",\n" +
		"data: \"durability\":\"durable\",\"sessionID\":\"ses_1\",\"text\":\"multi\"}\n\n" +
		frame(`{"event":"session.status","durability":"ephemeral","sessionID":"ses_1"}`) +
		frame(durableFrame)

	var texts []string
	err := readSSEFrames(context.Background(), strings.NewReader(stream), func(raw string) bool {
		if ev, ok := projectString(t, raw); ok {
			texts = append(texts, ev.Text)
		}
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(texts) != 2 || texts[0] != "multi" || texts[1] != "hello" {
		t.Errorf("read %q, want the two durable events in order", texts)
	}
}

// A caller that stops reading stops the parser, and a frame past the
// limit is an error rather than an unbounded buffer.
//
// @decision(DL-018)
func TestTheFrameReaderStopsAndIsBounded(t *testing.T) {
	stream := frame(`{"n":1}`) + frame(`{"n":2}`)
	seen := 0
	if err := readSSEFrames(context.Background(), strings.NewReader(stream), func(string) bool {
		seen++
		return false
	}); err != nil {
		t.Fatal(err)
	}
	if seen != 1 {
		t.Errorf("read %d frames after asking to stop at one", seen)
	}

	huge := "data: " + strings.Repeat("x", MaxLogFrameBytes+1) + "\n\n"
	if err := readSSEFrames(context.Background(), strings.NewReader(huge), func(string) bool { return true }); err == nil {
		t.Error("an oversized frame was accepted")
	}
}

// runningSession drives a launch to running and returns the stored row,
// which is what Connect needs to reach a sandbox.
func runningSession(t *testing.T, f *apiFixture) store.Session {
	t.Helper()
	id := f.create(repoReq("alpha"))
	f.events(id)
	sess, err := f.store.Sessions().Get(context.Background(), auth.LocalID, id)
	if err != nil {
		t.Fatal(err)
	}
	if sess.State != store.StateRunning {
		t.Fatalf("session is %s, want running", sess.State)
	}
	return sess
}

// A sandbox refusing the credential is reported as its own error, on
// every path that reaches one. This is what the connection cache reads
// to know an entry has gone stale.
//
// @decision(DL-030)
func TestASandboxRefusalIsItsOwnError(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
			}))
			defer srv.Close()

			_, err := sandboxGet(context.Background(), srv.URL, "tok", "u", "pw")
			if !errors.Is(err, ErrSandboxUnauthorized) {
				t.Errorf("sandboxGet: %v, want a refusal", err)
			}
			_, err = openSandboxEvents(context.Background(), Connection{URL: srv.URL})
			if !errors.Is(err, ErrSandboxUnauthorized) {
				t.Errorf("openSandboxEvents: %v, want a refusal", err)
			}
			_, err = sandboxPostJSON(context.Background(), Connection{URL: srv.URL}, srv.URL, []byte(`{}`))
			if !errors.Is(err, ErrSandboxUnauthorized) {
				t.Errorf("sandboxPostJSON: %v, want a refusal", err)
			}
		})
	}
}

// Every other status stays an ordinary failure. Reading one as a
// refusal would drop a good connection and mint again on each of them,
// and withConnection would retry an act that may already have taken
// effect.
//
// @decision(DL-030)
func TestAnOrdinaryFailureIsNotARefusal(t *testing.T) {
	for _, code := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusNotFound} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
			}))
			defer srv.Close()
			_, err := sandboxGet(context.Background(), srv.URL, "tok", "u", "pw")
			if err == nil {
				t.Fatal("the status was accepted")
			}
			if errors.Is(err, ErrSandboxUnauthorized) {
				t.Errorf("%v was read as a refusal", err)
			}
		})
	}
}
