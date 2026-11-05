//go:build integration

package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// These are the phase-3 spikes behind DL-018, kept as the standing proof of the facts it rests on. Both create
// billable sandboxes and terminate what they create.
//
// S-A: a held-open log?follow=true does not reset Modal's idle timeout,
// and a follow=false poll every 30 s does. S-B: the durable log answers
// but returns only its log.synced watermark, so it cannot be the
// transcript source. Both held again on 0.0.0-beta-18743 (2026-09-02).

// logPath is the durable per-session event log under test.
func logPath(sessionID string, after int, follow bool) string {
	return fmt.Sprintf("/api/experimental/session/%s/log?after=%d&follow=%t", sessionID, after, follow)
}

// sandboxHTTP is the DL-030 request pair: the connect token as a cookie
// (the Authorization header is OpenCode's, for Basic) plus Basic auth.
func sandboxHTTP(ctx context.Context, t *testing.T, method, url, token, password string) *http.Request {
	t.Helper()
	return sandboxHTTPBody(ctx, t, method, url, token, password, "")
}

func sandboxHTTPBody(ctx context.Context, t *testing.T, method, url, token, password, body string) *http.Request {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, r)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.AddCookie(&http.Cookie{Name: "_modal_connect_token", Value: token})
	req.SetBasicAuth("opencode", password)
	return req
}

// createSession makes one OpenCode session so the log has a subject.
func createSession(ctx context.Context, t *testing.T, c *http.Client, base, token, password string) string {
	t.Helper()
	const create = `{"location":{"directory":"/root"}}`
	req := sandboxHTTPBody(ctx, t, http.MethodPost, base+"/api/session", token, password, create)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create session: %s: %s", resp.Status, truncate(string(body), 400))
	}
	// The session is wrapped in a `data` envelope, as the listing is.
	var out struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("create session: decode %s: %v", truncate(string(body), 400), err)
	}
	if out.Data.ID == "" {
		t.Fatalf("create session: no id in %s", truncate(string(body), 400))
	}
	return out.Data.ID
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// sseFrame is one parsed server-sent event.
type sseFrame struct {
	Event string
	Data  string
}

// readFrames reads SSE frames from r until ctx expires, the stream ends,
// or max frames have been read. It returns what it got and whether the
// stream ended by itself.
func readFrames(ctx context.Context, r io.Reader, max int) (frames []sseFrame, ended bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var cur sseFrame
	for sc.Scan() {
		if ctx.Err() != nil {
			return frames, false
		}
		line := sc.Text()
		switch {
		case line == "":
			if cur.Event != "" || cur.Data != "" {
				frames = append(frames, cur)
				cur = sseFrame{}
				if max > 0 && len(frames) >= max {
					return frames, false
				}
			}
		case strings.HasPrefix(line, "event:"):
			cur.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			cur.Data += strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
	return frames, sc.Err() == nil
}

// TestIntegrationLogReadingAndIdleTimeout is spike S-A. Four sandboxes
// with IdleTimeout 2m run in parallel for 4½ minutes: a no-traffic
// control, a held-open log?follow=true stream, a log?follow=false poll
// every 30 s, and a plain /api/info request every 30 s as the
// calibration arm. It fails if any arm stops behaving as measured.
func TestIntegrationLogReadingAndIdleTimeout(t *testing.T) {
	const (
		idle    = 2 * time.Minute
		observe = idle*2 + 30*time.Second
	)
	p := newIntegrationProvider(t)

	cases := []struct {
		name    string
		traffic func(ctx context.Context, t *testing.T, c *http.Client, base, token, password, session string)
	}{
		{name: "no traffic"},
		{name: "health request every 30s", traffic: func(ctx context.Context, t *testing.T, c *http.Client, base, token, password, session string) {
			tick := time.NewTicker(30 * time.Second)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					resp, err := c.Do(sandboxHTTP(ctx, t, http.MethodGet, base+"/api/info", token, password))
					if err != nil {
						t.Logf("health: %v", err)
						continue
					}
					resp.Body.Close()
				}
			}
		}},
		{name: "log follow held open", traffic: func(ctx context.Context, t *testing.T, c *http.Client, base, token, password, session string) {
			req := sandboxHTTP(ctx, t, http.MethodGet, base+logPath(session, 0, true), token, password)
			resp, err := c.Do(req)
			if err != nil {
				t.Logf("log follow: %v", err)
				return
			}
			defer resp.Body.Close()
			buf := make([]byte, 4096)
			for {
				if _, err := resp.Body.Read(buf); err != nil {
					t.Logf("log follow ended: %v", err)
					return
				}
			}
		}},
		{name: "log poll every 30s", traffic: func(ctx context.Context, t *testing.T, c *http.Client, base, token, password, session string) {
			tick := time.NewTicker(30 * time.Second)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					resp, err := c.Do(sandboxHTTP(ctx, t, http.MethodGet, base+logPath(session, 0, false), token, password))
					if err != nil {
						t.Logf("log poll: %v", err)
						continue
					}
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), observe+5*time.Minute)
			defer cancel()

			sb, creds, password := startOpenCodeSandbox(t, ctx, p, "spov-logidle", idle)
			client := &http.Client{Timeout: 0}

			// A session must exist before the log can be read, and
			// creating it is itself a request — so every arm, including
			// the control, pays the same one-off activity cost before
			// the observation window opens.
			session := createSession(ctx, t, client, creds.URL, creds.Token, password)
			t.Logf("session %s", session)

			trafficCtx, stopTraffic := context.WithCancel(ctx)
			defer stopTraffic()
			if tc.traffic != nil {
				go tc.traffic(trafficCtx, t, client, creds.URL, creds.Token, password, session)
			}

			start := time.Now()
			died := time.Duration(0)
			for time.Since(start) < observe {
				time.Sleep(15 * time.Second)
				code, err := sb.Poll(ctx, nil)
				if err != nil {
					t.Fatalf("poll: %v", err)
				}
				if code != nil {
					died = time.Since(start).Round(time.Second)
					break
				}
			}
			stopTraffic()

			if died != 0 {
				t.Logf("RESULT %-26s died after %s: this shape does NOT reset the idle timeout", tc.name, died)
			} else {
				t.Logf("RESULT %-26s alive after %s: this shape DOES reset the idle timeout", tc.name, observe)
			}

			switch tc.name {
			case "no traffic":
				if died == 0 {
					t.Errorf("the control survived %s with IdleTimeout %s: idle detection has changed and this spike measures nothing", observe, idle)
				}
			case "health request every 30s":
				if died != 0 {
					t.Errorf("the calibration arm died after %s: phase 0 found plain requests count as activity, so the harness is no longer measuring what it did", died)
				}
			case "log follow held open":
				// Measured: a held-open follow does not count as
				// activity, so DL-018's watcher may hold one open.
				if died == 0 {
					t.Errorf("a held-open follow now keeps the sandbox alive past %s: DL-018's watcher can no longer hold one open, and the relay must move to a poll slower than IdleTimeout", observe)
				}
			case "log poll every 30s":
				// Measured: a poll is a plain request and resets the
				// timer, which is why DL-018 forbids polling at or below
				// IdleTimeout. Surviving is the expected outcome.
				if died != 0 {
					t.Errorf("a 30s poll died after %s: polling no longer counts as activity, so DL-018's cadence constraint is now unnecessary and should be revisited", died)
				}
			}
		})
	}
}

// TestIntegrationDurableSessionLog is spike S-B: does
// /api/experimental/session/:id/log behave as the research inferred?
// Failures are reported rather than fatal wherever the run can continue,
// so one run yields the whole picture.
func TestIntegrationDurableSessionLog(t *testing.T) {
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	_, creds, password := startOpenCodeSandbox(t, ctx, p, "spov-logapi", 10*time.Minute)
	client := &http.Client{Timeout: 0}
	base := creds.URL

	// Claim: a sandbox with no sessions at all is the interactive case
	// before the user types anything. What does the log say then?
	t.Run("no sessions yet", func(t *testing.T) {
		reqCtx, c := context.WithTimeout(ctx, 30*time.Second)
		defer c()
		resp, err := client.Do(sandboxHTTP(reqCtx, t, http.MethodGet, base+"/api/session", creds.Token, password))
		if err != nil {
			t.Fatalf("list sessions: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		t.Logf("GET /api/session -> %s: %s", resp.Status, truncate(string(body), 300))
	})

	session := createSession(ctx, t, client, base, creds.Token, password)
	t.Logf("session under test: %s", session)

	// Give the log real content. resume:true schedules the agent loop,
	// which emits the durable session.execution.* / session.step.* /
	// session.text.* events the relay wants; the image's default model
	// is a free one, so this needs no provider key.
	runAgent(ctx, t, client, base, creds.Token, password, session)

	// Claim: the endpoint exists, answers text/event-stream, and
	// follow=false terminates rather than hanging.
	var firstFrames []sseFrame
	t.Run("follow=false terminates and streams", func(t *testing.T) {
		reqCtx, c := context.WithTimeout(ctx, 60*time.Second)
		defer c()
		// nolint:bodyclose // closed on the next line; the linter loses
		// track of the response when the request is built inline.
		resp, err := client.Do(sandboxHTTP(reqCtx, t, http.MethodGet, base+logPath(session, 0, false), creds.Token, password))
		if err != nil {
			t.Fatalf("log follow=false: %v", err)
		}
		defer resp.Body.Close()
		ct := resp.Header.Get("Content-Type")
		t.Logf("status=%s content-type=%s", resp.Status, ct)
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("log follow=false: %s: %s", resp.Status, truncate(string(body), 400))
		}
		if !strings.Contains(ct, "text/event-stream") {
			t.Errorf("content-type = %q, want text/event-stream", ct)
		}
		done := make(chan struct{})
		var ended bool
		go func() {
			firstFrames, ended = readFrames(reqCtx, resp.Body, 0)
			close(done)
		}()
		select {
		case <-done:
			if !ended {
				t.Errorf("follow=false did not end by itself within 60s")
			} else {
				t.Logf("follow=false ended by itself after %d frames", len(firstFrames))
			}
		case <-time.After(70 * time.Second):
			t.Errorf("follow=false hung: a poll fallback cannot use it as written")
		}
		for i, f := range firstFrames {
			t.Logf("frame %d: event=%q data=%s", i, f.Event, truncate(f.Data, 300))
		}
	})

	// The durable log returns only the log.synced watermark. These
	// assertions fail if it starts returning events, which is the
	// trigger to revisit DL-018.
	t.Run("the log yields only a watermark", func(t *testing.T) {
		if len(firstFrames) == 0 {
			t.Fatal("no frames at all: not even the watermark arrived")
		}
		watermark := 0
		content := 0
		for _, f := range firstFrames {
			var probe map[string]any
			if err := json.Unmarshal([]byte(f.Data), &probe); err != nil {
				content++
				continue
			}
			if probe["type"] == "log.synced" {
				watermark++
				if seq, ok := probe["seq"].(float64); ok {
					t.Logf("watermark seq=%d", int(seq))
					if seq == 0 {
						t.Errorf("the watermark sat at 0 after an agent run: the sequence is not advancing, so even the watermark is not a usable progress signal")
					}
				} else {
					t.Errorf("log.synced carried no seq: %s", truncate(f.Data, 200))
				}
				continue
			}
			content++
			t.Logf("CONTENT FRAME: %s", truncate(f.Data, 400))
		}
		if watermark != 1 {
			t.Errorf("log.synced appeared %d times, want exactly 1", watermark)
		}
		if content > 0 {
			t.Errorf("the log returned %d content frames — the endpoint now delivers events, so DL-018's relay fallback should be revisited (this failure is good news)", content)
		} else {
			t.Log("confirmed: the durable log delivers no events, only its watermark")
		}
	})

	// Claim under test: does follow=true deliver live durable events?
	// Observed: no — the watermark, then silence, while /api/event
	// carries the same session's full transcript.
	t.Run("follow=true delivers no live events", func(t *testing.T) {
		fctx, c := context.WithTimeout(ctx, 45*time.Second)
		defer c()
		resp, err := client.Do(sandboxHTTP(fctx, t, http.MethodGet, base+logPath(session, 0, true), creds.Token, password))
		if err != nil {
			t.Fatalf("log follow=true: %v", err)
		}
		defer resp.Body.Close()
		go func() {
			time.Sleep(3 * time.Second)
			runAgent(fctx, t, client, base, creds.Token, password, session)
		}()
		frames, _ := readFrames(fctx, resp.Body, 0)
		live := 0
		for _, f := range frames {
			if !strings.Contains(f.Data, "log.synced") {
				live++
				t.Logf("LIVE FRAME: %s", truncate(f.Data, 400))
			}
		}
		t.Logf("follow=true delivered %d frames over 45s, %d of them events", len(frames), live)
		if live > 0 {
			t.Errorf("follow=true now delivers %d live events — DL-018's relay fallback should be revisited (this failure is good news)", live)
		}
	})

	// The volatile global stream is the contrast, and the fallback the
	// finding forces: it carries the same events, tagged durable.
	t.Run("the volatile stream carries what the log does not", func(t *testing.T) {
		ectx, c := context.WithTimeout(ctx, 45*time.Second)
		defer c()
		resp, err := client.Do(sandboxHTTP(ectx, t, http.MethodGet, base+"/api/event", creds.Token, password))
		if err != nil {
			t.Fatalf("/api/event: %v", err)
		}
		defer resp.Body.Close()
		go func() {
			time.Sleep(2 * time.Second)
			runAgent(ectx, t, client, base, creds.Token, password, session)
		}()
		frames, _ := readFrames(ectx, resp.Body, 12)
		durable := 0
		for i, f := range frames {
			if strings.Contains(f.Data, `"durable"`) {
				durable++
			}
			if i < 6 {
				t.Logf("event %d: %s", i, truncate(f.Data, 250))
			}
		}
		t.Logf("/api/event delivered %d frames, %d carrying a durable marker", len(frames), durable)
		if len(frames) == 0 {
			t.Error("the volatile stream delivered nothing either: the relay has no source at all and DL-018's second surface cannot be built")
		}
	})

	// Claim: an unknown session is distinguishable from an empty log.
	t.Run("unknown session", func(t *testing.T) {
		reqCtx, c := context.WithTimeout(ctx, 30*time.Second)
		defer c()
		resp, err := client.Do(sandboxHTTP(reqCtx, t, http.MethodGet, base+logPath("ses_nonexistent000000000", 0, false), creds.Token, password))
		if err != nil {
			t.Fatalf("unknown session: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		t.Logf("unknown session -> %s: %s", resp.Status, truncate(string(body), 300))
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("unknown session answered %s, want 404: the relay could not tell a typo from an empty log", resp.Status)
		}
	})
}

// runAgent admits a prompt with resume:true, which schedules the agent
// loop, and waits for it to produce output. The pinned image has a free
// default model, so no provider key is needed.
func runAgent(ctx context.Context, t *testing.T, c *http.Client, base, token, password, session string) {
	t.Helper()
	body := fmt.Sprintf(`{"id":"msg_%d","text":"Reply with the single word: ack.","resume":true}`, time.Now().UnixNano())
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := c.Do(sandboxHTTPBody(reqCtx, t, http.MethodPost, base+"/api/session/"+session+"/prompt", token, password, body))
	if err != nil {
		t.Logf("prompt: %v", err)
		return
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Logf("prompt -> %s: %s", resp.Status, truncate(string(raw), 300))
		return
	}
	// Let the loop emit before the caller reads the log.
	time.Sleep(8 * time.Second)
}
