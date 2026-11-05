package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/store"
)

// Event is one server-sent event from a session's launch stream.
type Event struct {
	Name string
	Data json.RawMessage
}

// LaunchObserver receives a launch's progress. Exactly one of Ready and
// Failed ends a Follow that returns nil.
//
// @decision(DL-002)
type LaunchObserver interface {
	Step(ev server.StepEvent)
	Note(ev server.NoteEvent)
	Ready(ev server.ReadyEvent)
	Failed(ev server.ErrorEvent)
}

// FollowOptions tune Follow. The zero value is what the CLI uses.
type FollowOptions struct {
	// PollInterval is how often the session is polled once streaming has
	// been given up on. Zero means one second.
	PollInterval time.Duration
	// StreamAttempts is how many times the stream is opened before
	// falling back to polling. Zero means two: once, and once more from
	// the beginning, since the server replays.
	StreamAttempts int
}

// MaxEventBytes bounds one server-sent event. A session view is a few
// kilobytes; anything near this is not the spov server.
const MaxEventBytes = 1 << 20

// ErrLaunchFailed is returned by Follow when the launch ended in an error
// event; the observer has already seen the problem.
var ErrLaunchFailed = errors.New("client: the launch failed")

// Follow reports a session's launch to obs until it is ready or failed.
// It streams the server's events, reopening the stream once from the
// beginning if the connection drops, and falls back to polling the
// session when streaming fails twice.
//
// @decision(DL-002) @decision(DL-005)
func (c *Client) Follow(ctx context.Context, id string, obs LaunchObserver, opts FollowOptions) error {
	attempts := opts.StreamAttempts
	if attempts == 0 {
		attempts = 2
	}
	var last error
	for i := 0; i < attempts; i++ {
		done, err := c.stream(ctx, id, obs)
		if done {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		last = err
	}
	// Two broken streams: whatever is wrong with streaming, the session
	// still exists and its state says how the launch ended.
	return c.poll(ctx, id, obs, opts.PollInterval, last)
}

// stream opens the event stream and feeds obs. done reports whether a
// terminal event was seen; when it is false the stream broke and err says
// how.
func (c *Client) stream(ctx context.Context, id string, obs LaunchObserver) (done bool, err error) {
	req, err := c.newRequest(ctx, http.MethodGet, "sessions/"+id+"/events", nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "text/event-stream")
	// No timeout on a stream: a launch legitimately takes minutes. The
	// caller's context is the bound.
	transport := http.DefaultTransport
	if c.HTTP != nil && c.HTTP.Transport != nil {
		transport = c.HTTP.Transport
	}
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return false, fmt.Errorf("client: events: %w", redactURL(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		perr := problemOf(resp)
		// A problem the server chose to send is final: retrying a 404 or
		// a 401 cannot help.
		return true, perr
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), MaxEventBytes)
	var cur Event
	var data []string
	var size int
	flush := func() (bool, error) {
		if cur.Name == "" && len(data) == 0 {
			return false, nil
		}
		cur.Data = json.RawMessage(strings.Join(data, "\n"))
		terminal, err := dispatch(cur, obs)
		cur, data, size = Event{}, nil, 0
		return terminal, err
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, ":"):
			// A comment, typically a keepalive.
		case strings.HasPrefix(line, "event:"):
			cur.Name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			// Bounded per event, not per line: the server is trusted but a
			// stream that never ends an event must not grow without limit.
			size += len(line)
			if size > MaxEventBytes {
				return true, fmt.Errorf("client: an event exceeded %d bytes", MaxEventBytes)
			}
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case line == "":
			if terminal, err := flush(); terminal || err != nil {
				return true, err
			}
		}
	}
	if terminal, err := flush(); terminal || err != nil {
		return true, err
	}
	if err := sc.Err(); err != nil {
		return false, fmt.Errorf("client: events: %w", err)
	}
	return false, errors.New("client: the event stream ended without a result")
}

// dispatch hands one event to the observer and reports whether it was
// terminal. A progress event that cannot be decoded is skipped like an
// unknown one: it is cosmetic, and the terminal event is what decides
// the launch. A terminal event that cannot be decoded is an error, since
// nothing else can say how the launch ended.
func dispatch(ev Event, obs LaunchObserver) (bool, error) {
	switch ev.Name {
	case "step":
		var e server.StepEvent
		if json.Unmarshal(ev.Data, &e) == nil {
			obs.Step(e)
		}
	case "note":
		var e server.NoteEvent
		if json.Unmarshal(ev.Data, &e) == nil {
			obs.Note(e)
		}
	case "ready":
		var e server.ReadyEvent
		if err := json.Unmarshal(ev.Data, &e); err != nil {
			return false, fmt.Errorf("client: bad ready event: %w", err)
		}
		obs.Ready(e)
		return true, nil
	case "error":
		var e server.ErrorEvent
		if err := json.Unmarshal(ev.Data, &e); err != nil {
			return false, fmt.Errorf("client: bad error event: %w", err)
		}
		obs.Failed(e)
		return true, ErrLaunchFailed
	}
	// Unknown events are skipped: a newer server may add kinds this
	// client does not know, and the terminal ones are what matter.
	return false, nil
}

// poll watches the session's state until the launch has ended. Steps are
// invisible this way, so the observer only learns the outcome.
func (c *Client) poll(ctx context.Context, id string, obs LaunchObserver, interval time.Duration, streamErr error) error {
	if interval <= 0 {
		interval = time.Second
	}
	obs.Note(server.NoteEvent{Message: fmt.Sprintf("the event stream is unavailable (%v); polling the session instead", streamErr), At: time.Now()})
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		sess, err := c.GetSession(ctx, id)
		if err != nil {
			return err
		}
		switch sess.State {
		case store.StateRunning:
			obs.Ready(server.ReadyEvent{Session: sess})
			return nil
		case store.StateFailed, store.StateStopped:
			detail := sess.Error
			if detail == "" {
				detail = "the session is " + string(sess.State)
			}
			obs.Failed(server.ErrorEvent{Problem: server.ProblemView{Title: "Launch failed", Status: http.StatusBadGateway, Detail: detail}})
			return ErrLaunchFailed
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}
