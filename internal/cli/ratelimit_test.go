package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/server/client"
	"github.com/jimutt/spov/internal/store"
)

// tooMany answers a 429 the way the server's limits do.
func tooMany(w http.ResponseWriter, after string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Retry-After", after)
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = io.WriteString(w, `{"status":429,"title":"Too Many Requests","detail":"You are sending requests faster than this server allows. Try again in 1 seconds."}`)
}

// A 429 is the server saying when to come back, so a wait sleeps for
// its Retry-After, at least one poll and at most a minute, and goes on;
// it is not counted as a failed read.
//
// @decision(DL-133) @decision(DL-103)
func TestJobsWaitHonoursATooManyRequests(t *testing.T) {
	var mu sync.Mutex
	reads := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if reads++; reads <= waitRetries+3 {
			tooMany(w, "0")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"job_1","session_id":"sess_1","kind":"pr","state":"succeeded","goal":"fix it"}`)
	}))
	t.Cleanup(api.Close)
	env := Env{Client: client.New(api.URL, "tok"), JobPoll: time.Millisecond}
	_, stderr, err := runWith(t, env, "jobs", "wait", "job_1")
	if err != nil {
		t.Fatalf("more 429s than the retries, then a finished job: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "the server asked to slow down; reading again in 1ms") {
		t.Errorf("the wait did not say it was slowed down:\n%s", stderr)
	}
}

// Attach's wait honours a 429 on either of its reads the same way.
//
// @decision(DL-133) @decision(DL-025)
func TestAttachWaitHonoursATooManyRequests(t *testing.T) {
	f := newAPIFixture(t)
	f.seedSession("job-1", time.Minute)
	job := f.seedJob("job-1", store.JobQueued)
	env := f.env
	env.AttachPoll = time.Millisecond
	c := *f.env.Client
	var mu sync.Mutex
	sessions, convs, refused := 0, 0, map[string]int{}
	refuse := func() (*http.Response, error) {
		rec := httptest.NewRecorder()
		tooMany(rec, "0")
		return rec.Result(), nil
	}
	c.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method != http.MethodGet:
		case strings.HasSuffix(r.URL.Path, "/messages"):
			// The first three reads of the conversations are refused.
			if convs++; convs <= 3 {
				refused["conversations"]++
				return refuse()
			}
		case strings.HasPrefix(r.URL.Path, "/api/v1/sessions/"):
			// The attach's own read, then three of the wait's refused,
			// then the job runs with its conversation recorded.
			sessions++
			if sessions >= 2 && sessions <= 4 {
				refused["session"]++
				return refuse()
			}
			if sessions == 5 {
				f.moveJob(&job, store.JobProvisioning, store.JobRunning)
				if _, err := f.deps.Store.Conversations().UpsertConversation(context.Background(), store.Conversation{
					ID: store.NewID(time.Now()), OrgID: job.OrgID, SessionID: job.SessionID, OpenCodeID: "ses_theJobsOwn",
				}); err != nil {
					t.Error(err)
				}
			}
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	env.Client = &c
	_, stderr, err := runWith(t, env, "attach", "job-1", "--timeout", "10s")
	if err != nil {
		t.Fatalf("attach through 429s: %v\n%s", err, stderr)
	}
	if refused["session"] != 3 || refused["conversations"] != 3 {
		t.Errorf("refused reads = %v, want three of each", refused)
	}
	if f.tuiSeen.Session != "ses_theJobsOwn" {
		t.Errorf("the TUI was given session %q, want the job's conversation", f.tuiSeen.Session)
	}
	if !strings.Contains(stderr, "the server asked to slow down") {
		t.Errorf("the wait did not say it was slowed down:\n%s", stderr)
	}
}

// The wait a 429 asks for is its Retry-After, at least one poll and at
// most a minute; any other answer is not a 429's.
//
// @decision(DL-133)
func TestTheWaitForATooManyRequestsIsBounded(t *testing.T) {
	poll := 2 * time.Second
	for _, c := range []struct {
		err  error
		want time.Duration
		ok   bool
	}{
		{&client.Error{Status: http.StatusTooManyRequests, RetryAfter: 10 * time.Second}, 10 * time.Second, true},
		{&client.Error{Status: http.StatusTooManyRequests, RetryAfter: 900 * time.Second}, time.Minute, true},
		{&client.Error{Status: http.StatusTooManyRequests}, poll, true},
		{fmt.Errorf("reading: %w", &client.Error{Status: http.StatusTooManyRequests, RetryAfter: time.Second}), poll, true},
		{&client.Error{Status: http.StatusServiceUnavailable, RetryAfter: 10 * time.Second}, 0, false},
		{io.EOF, 0, false},
	} {
		got, ok := slowedDown(c.err, poll)
		if got != c.want || ok != c.ok {
			t.Errorf("slowedDown(%v) = %s, %v; want %s, %v", c.err, got, ok, c.want, c.ok)
		}
	}
}
