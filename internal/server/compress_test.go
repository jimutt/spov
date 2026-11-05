package server

import (
	"bufio"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/store"
)

func acceptGzip(r *http.Request) { r.Header.Set("Accept-Encoding", "gzip") }

// The transcript is gzipped for a client that asks, and only then.
//
// @decision(DL-071)
func TestTheTranscriptIsGzippedForAClientThatAsks(t *testing.T) {
	f := newAPIFixture(t)
	sess := seedSession(t, f, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "mine", store.StateRunning)
	conv := seedConversation(t, f, sess, "ses_1", "one")
	seedMessage(t, f, conv, "msg_a", "assistant", strings.Repeat("what I said ", 200), recordedAt)

	resp, body := f.do(http.MethodGet, "/api/v1/sessions/mine/messages", nil, acceptGzip)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("GET = %d, encoding %q", resp.StatusCode, resp.Header.Get("Content-Encoding"))
	}
	if !strings.Contains(resp.Header.Get("Vary"), "Accept-Encoding") {
		t.Errorf("Vary = %q", resp.Header.Get("Vary"))
	}
	zr, err := gzip.NewReader(strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(zr)
	if err != nil || !strings.Contains(string(plain), "what I said") {
		t.Fatalf("decompressed %d bytes, %v", len(plain), err)
	}
	if len(body) >= len(plain)/4 {
		t.Errorf("%d bytes compressed from %d", len(body), len(plain))
	}

	for _, accept := range []string{"", "gzip;q=0", "br"} {
		resp, _ := f.do(http.MethodGet, "/api/v1/sessions/mine/messages", nil, func(r *http.Request) { r.Header.Set("Accept-Encoding", accept) })
		if got := resp.Header.Get("Content-Encoding"); got != "" {
			t.Errorf("Accept-Encoding %q got %q", accept, got)
		}
	}
	// A read outside the two is never compressed.
	resp, _ = f.do(http.MethodGet, "/api/v1/sessions", nil, acceptGzip)
	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Errorf("the session list is %q", got)
	}
}

// The log streams gzipped, each event flushed as it is sent rather than
// held in the compressor.
//
// @decision(DL-071)
func TestTheLogStreamsGzippedEventByEvent(t *testing.T) {
	f := newAPIFixture(t)
	f.srv.streamLifetime = time.Hour
	f.srv.heartbeat = time.Hour
	sess := seedSession(t, f, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "mine", store.StateRunning)
	conv := seedConversation(t, f, sess, "ses_1", "one")
	seedEvent(t, f, store.Event{OrgID: sess.OrgID, SessionID: sess.ID, ConversationID: conv.ID, Source: store.SourceOpenCode,
		Seq: 1, Name: "session.text.ended", Text: "first words", At: recordedAt})

	req, _ := http.NewRequest(http.MethodGet, f.http.URL+"/api/v1/sessions/mine/log", nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	acceptGzip(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("encoding %q", resp.Header.Get("Content-Encoding"))
	}
	got := make(chan string, 1)
	go func() {
		zr, err := gzip.NewReader(resp.Body)
		if err != nil {
			got <- err.Error()
			return
		}
		sc := bufio.NewScanner(zr)
		for sc.Scan() {
			if strings.Contains(sc.Text(), "first words") {
				got <- "ok"
				return
			}
		}
		got <- "the stream ended without the event"
	}()
	select {
	case r := <-got:
		if r != "ok" {
			t.Fatal(r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the event never left the compressor while the stream stayed open")
	}
}

func TestAcceptsGzip(t *testing.T) {
	for header, want := range map[string]bool{
		"gzip": true, "deflate, gzip;q=0.5": true, "GZIP": true, "gzip; q=1.0": true,
		"": false, "br": false, "gzip;q=0": false, "gzip;q=0.000": false, "gzip;q=x": false,
	} {
		if got := acceptsGzip([]string{header}); got != want {
			t.Errorf("%q: %v, want %v", header, got, want)
		}
	}
}

// A handler that flushes before it writes still gets a response whose
// header says what its body is.
//
// @decision(DL-071)
func TestAFlushBeforeAnyWriteDecidesTheEncoding(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &gzipWriter{ResponseWriter: rec}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Flush()
	_, _ = io.WriteString(w, "data: hello\n\n")
	w.close()
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("encoding %q", rec.Header().Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(zr); string(got) != "data: hello\n\n" {
		t.Errorf("body %q", got)
	}
}
