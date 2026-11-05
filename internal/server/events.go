package server

import (
	"sync"
	"time"

	"github.com/jimutt/spov/internal/session"
)

// StepEvent is one launch-step transition, streamed to the client.
//
// @decision(DL-005)
type StepEvent struct {
	Step  string    `json:"step" doc:"Launch step name"`
	State string    `json:"state" enum:"started,done,failed" doc:"What happened to the step"`
	At    time.Time `json:"at"`
}

// NoteEvent is a line outside the step sequence, such as a warning.
type NoteEvent struct {
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

// ReadyEvent ends a successful launch stream.
//
// @decision(DL-005)
type ReadyEvent struct {
	Session SessionView `json:"session"`
}

// ErrorEvent ends a failed launch stream.
//
// @decision(DL-005)
type ErrorEvent struct {
	Problem ProblemView `json:"problem"`
}

// ProblemView is the RFC 9457 shape of a failure, inside an event.
type ProblemView struct {
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// launchLog records a launch's events and lets any number of streams
// replay and then follow them. It is complete once a terminal event
// (ready or error) is appended.
type launchLog struct {
	mu       sync.Mutex
	events   []any
	done     bool
	waiters  []chan struct{}
	now      func() time.Time
	running  string
	finishAt time.Time
}

func newLaunchLog(now func() time.Time) *launchLog {
	return &launchLog{now: now}
}

func (l *launchLog) append(ev any, terminal bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done {
		return
	}
	l.events = append(l.events, ev)
	l.done = terminal
	if terminal {
		l.finishAt = l.now()
	}
	for _, w := range l.waiters {
		close(w)
	}
	l.waiters = nil
}

// snapshot returns the events after index from, whether the log is
// complete, and a channel closed on the next change.
func (l *launchLog) snapshot(from int) ([]any, bool, <-chan struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	from = min(from, len(l.events))
	var wait chan struct{}
	if !l.done {
		wait = make(chan struct{})
		l.waiters = append(l.waiters, wait)
	}
	return l.events[from:], l.done, wait
}

// finished reports whether the log has its terminal event, and when.
func (l *launchLog) finished() (bool, time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.done, l.finishAt
}

// StepReporter implementation, fed by the launcher.

func (l *launchLog) Start(step session.Step) {
	l.mu.Lock()
	l.running = string(step)
	l.mu.Unlock()
	l.append(StepEvent{Step: string(step), State: "started", At: l.now()}, false)
}

func (l *launchLog) Done(step session.Step) {
	l.mu.Lock()
	l.running = ""
	l.mu.Unlock()
	l.append(StepEvent{Step: string(step), State: "done", At: l.now()}, false)
}

func (l *launchLog) Note(msg string) {
	l.append(NoteEvent{Message: msg, At: l.now()}, false)
}

// fail marks the running step failed, then ends the stream.
func (l *launchLog) fail(p ProblemView) {
	l.mu.Lock()
	running := l.running
	l.mu.Unlock()
	if running != "" {
		l.append(StepEvent{Step: running, State: "failed", At: l.now()}, false)
	}
	l.append(ErrorEvent{Problem: p}, true)
}

func (l *launchLog) ready(v SessionView) {
	l.append(ReadyEvent{Session: v}, true)
}
