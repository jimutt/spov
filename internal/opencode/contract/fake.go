package contract

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Checked wraps a fake OpenCode server's handler so that every request
// spov sends it is checked against the committed document. A request
// that fails is reported on t and still reaches the handler, so the
// test's own assertions run as well. A sandbox.Fake connect prefix
// (/connect/<sandbox>/<port>) is dropped before the path is checked.
//
// @decision(DL-035)
func Checked(t testing.TB, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("contract: reading %s %s: %v", r.Method, r.URL, err)
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		check(t, r.Method, fromAPI(r.URL), body)
		h.ServeHTTP(w, r)
	})
}

// CheckRequest checks one request given as a method, a path with its
// query, and a body, for a fake that is not an HTTP server.
//
// @decision(DL-035)
func CheckRequest(t testing.TB, method, target string, body []byte) {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Errorf("contract: %v", err)
		return
	}
	check(t, method, u, body)
}

// connectPrefix is the path sandbox.Fake puts before a sandbox's own.
var connectPrefix = regexp.MustCompile(`^/connect/[^/]+/[^/]+/`)

// fromAPI drops a fake connect URL's prefix.
func fromAPI(u *url.URL) *url.URL {
	path := u.EscapedPath()
	loc := connectPrefix.FindStringIndex(path)
	if loc == nil {
		return u
	}
	out := *u
	out.RawPath = path[loc[1]-1:]
	out.Path, _ = url.PathUnescape(out.RawPath)
	return &out
}

func check(t testing.TB, method string, u *url.URL, body []byte) {
	t.Helper()
	d, err := Load()
	if err != nil {
		t.Errorf("contract: %v", err)
		return
	}
	if _, err := d.Check(method, u, body); err != nil {
		t.Errorf("contract: %v", err)
	}
}

// CheckEventFrame reports on t an event-stream frame the committed
// document refuses.
//
// @decision(DL-145)
func CheckEventFrame(t testing.TB, frame []byte) {
	t.Helper()
	if err := CheckEvent(frame); err != nil {
		t.Errorf("contract: %v\n\tframe: %s", err, clip(frame))
	}
}

// CheckLogFrame reports on t a session-log item the committed document
// refuses.
//
// @decision(DL-145)
func CheckLogFrame(t testing.TB, frame []byte) {
	t.Helper()
	if err := CheckLogItem(frame); err != nil {
		t.Errorf("contract: %v\n\tframe: %s", err, clip(frame))
	}
}

func clip(b []byte) string {
	const max = 600
	if len(b) > max {
		return string(b[:max]) + "…"
	}
	return string(b)
}

// Frame returns an event frame for a test's fixture after checking it
// against the document's schema for its type.
//
// @decision(DL-145)
func Frame(t testing.TB, raw string) []byte {
	t.Helper()
	CheckEventFrame(t, []byte(raw))
	return []byte(raw)
}

// Unchecked returns a fixture's frame that no build sends, for a test of
// what a reader does with one. why says what is wrong with it.
//
// @decision(DL-145)
func Unchecked(raw, why string) []byte {
	if strings.TrimSpace(why) == "" {
		panic("contract.Unchecked needs the reason the frame is malformed")
	}
	return []byte(raw)
}

// Event builds a fixture's event frame of type typ around data, a JSON
// object kept byte for byte, in the envelope the document gives that
// type: an id, a creation time a second after FixtureEpoch per seq and,
// for a durable type, a place at seq in the log of data's sessionID. The
// frame is checked as Frame checks one.
//
// @decision(DL-145)
func Event(t testing.TB, typ string, seq int64, data string) []byte {
	t.Helper()
	return EventAt(t, typ, seq, FixtureEpoch.Add(time.Duration(seq)*time.Second), data)
}

// FixtureEpoch is the creation time Event counts from.
var FixtureEpoch = time.UnixMilli(1794165467000).UTC()

// EventAt is Event created at a time the caller gives.
//
// @decision(DL-145)
func EventAt(t testing.TB, typ string, seq int64, created time.Time, data string) []byte {
	t.Helper()
	frame, err := envelope(typ, seq, created, data)
	if err != nil {
		t.Fatalf("contract: %v", err)
	}
	CheckEventFrame(t, frame)
	return frame
}
