package store

import (
	"encoding/json"
	"time"
)

// Quota bounds what one user may ask a server for. Every field is a
// pointer so an override can say "three concurrent sessions" without
// also asserting a timeout: a zero value is a real limit (nothing at
// all), so absence has to be distinguishable from it.
//
// Org defaults come from configuration; a user's row may override any
// subset. Admins are subject to quotas too — there is no bypass, only a
// higher override.
//
// @decision(DL-009)
type Quota struct {
	// MaxConcurrentSessions bounds a user's starting + running sessions.
	MaxConcurrentSessions *int `json:"max_concurrent_sessions,omitempty"`
	// MaxConcurrentJobs bounds a user's unfinished jobs. Separate from
	// the session limit rather than a share of it.
	//
	// @decision(DL-045)
	MaxConcurrentJobs *int `json:"max_concurrent_jobs,omitempty"`
	// MaxJobDuration caps one job's wall clock.
	//
	// @decision(DL-049)
	MaxJobDuration *time.Duration `json:"max_job_duration,omitempty"`
	// MaxTimeout bounds a session's requested lifetime, itself hard-capped
	// by the sandbox rules of DL-029.
	MaxTimeout *time.Duration `json:"max_timeout,omitempty"`
	// MaxCPU and MaxMemoryMiB bound requested resources.
	MaxCPU       *float64 `json:"max_cpu,omitempty"`
	MaxMemoryMiB *int     `json:"max_memory_mib,omitempty"`
}

// Empty reports whether the quota asserts nothing.
func (q Quota) Empty() bool {
	return q.MaxConcurrentSessions == nil && q.MaxConcurrentJobs == nil &&
		q.MaxJobDuration == nil && q.MaxTimeout == nil && q.MaxCPU == nil &&
		q.MaxMemoryMiB == nil
}

// Clone deep-copies the quota, so a stored row and a caller's copy never
// share a pointer.
func (q Quota) Clone() Quota {
	var out Quota
	if q.MaxConcurrentSessions != nil {
		v := *q.MaxConcurrentSessions
		out.MaxConcurrentSessions = &v
	}
	if q.MaxConcurrentJobs != nil {
		v := *q.MaxConcurrentJobs
		out.MaxConcurrentJobs = &v
	}
	if q.MaxJobDuration != nil {
		v := *q.MaxJobDuration
		out.MaxJobDuration = &v
	}
	if q.MaxTimeout != nil {
		v := *q.MaxTimeout
		out.MaxTimeout = &v
	}
	if q.MaxCPU != nil {
		v := *q.MaxCPU
		out.MaxCPU = &v
	}
	if q.MaxMemoryMiB != nil {
		v := *q.MaxMemoryMiB
		out.MaxMemoryMiB = &v
	}
	return out
}

// MarshalJSON writes MaxTimeout and MaxJobDuration as duration strings
// ("8h"), which is what the config file and the API use; encoding/json
// would otherwise write nanoseconds.
//
// @decision(DL-009) @decision(DL-049)
func (q Quota) MarshalJSON() ([]byte, error) {
	type alias struct {
		MaxConcurrentSessions *int     `json:"max_concurrent_sessions,omitempty"`
		MaxConcurrentJobs     *int     `json:"max_concurrent_jobs,omitempty"`
		MaxJobDuration        *string  `json:"max_job_duration,omitempty"`
		MaxTimeout            *string  `json:"max_timeout,omitempty"`
		MaxCPU                *float64 `json:"max_cpu,omitempty"`
		MaxMemoryMiB          *int     `json:"max_memory_mib,omitempty"`
	}
	a := alias{
		MaxConcurrentSessions: q.MaxConcurrentSessions,
		MaxConcurrentJobs:     q.MaxConcurrentJobs,
		MaxCPU:                q.MaxCPU,
		MaxMemoryMiB:          q.MaxMemoryMiB,
	}
	if q.MaxTimeout != nil {
		s := q.MaxTimeout.String()
		a.MaxTimeout = &s
	}
	if q.MaxJobDuration != nil {
		s := q.MaxJobDuration.String()
		a.MaxJobDuration = &s
	}
	return json.Marshal(a)
}

// UnmarshalJSON reads what MarshalJSON wrote, and also accepts a number
// of nanoseconds so a row written by an older encoding still loads.
//
// @decision(DL-009)
func (q *Quota) UnmarshalJSON(data []byte) error {
	var a struct {
		MaxConcurrentSessions *int             `json:"max_concurrent_sessions"`
		MaxConcurrentJobs     *int             `json:"max_concurrent_jobs"`
		MaxJobDuration        *json.RawMessage `json:"max_job_duration"`
		MaxTimeout            *json.RawMessage `json:"max_timeout"`
		MaxCPU                *float64         `json:"max_cpu"`
		MaxMemoryMiB          *int             `json:"max_memory_mib"`
	}
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	q.MaxConcurrentSessions = a.MaxConcurrentSessions
	q.MaxConcurrentJobs = a.MaxConcurrentJobs
	q.MaxCPU = a.MaxCPU
	q.MaxMemoryMiB = a.MaxMemoryMiB
	var err error
	if q.MaxTimeout, err = durationField(a.MaxTimeout); err != nil {
		return err
	}
	q.MaxJobDuration, err = durationField(a.MaxJobDuration)
	return err
}

// durationField reads a duration string, or a number of nanoseconds from
// an older encoding. Absent is nil.
func durationField(raw *json.RawMessage) (*time.Duration, error) {
	if raw == nil {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(*raw, &s); err == nil {
		d, err := time.ParseDuration(s)
		if err != nil {
			return nil, err
		}
		return &d, nil
	}
	var ns int64
	if err := json.Unmarshal(*raw, &ns); err != nil {
		return nil, err
	}
	d := time.Duration(ns)
	return &d, nil
}
