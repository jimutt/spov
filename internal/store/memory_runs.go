package store

import (
	"context"
	"slices"
	"sort"
)

// memRuns is the memory store's RunStore.
//
// @decision(DL-085)
type memRuns struct{ m *Memory }

func cloneRuns(in map[string][]Run) map[string][]Run {
	out := make(map[string][]Run, len(in))
	for k, v := range in {
		out[k] = slices.Clone(v)
	}
	return out
}

func (s memRuns) Insert(_ context.Context, r Run) error {
	defer s.m.lock()()
	sess, ok := s.m.sessions[r.SessionID]
	if !ok || sess.OrgID != r.OrgID {
		return ErrNotFound
	}
	for _, have := range s.m.runs[r.SessionID] {
		if have.N == r.N {
			return ErrConflict
		}
	}
	s.m.runs[r.SessionID] = append(s.m.runs[r.SessionID], r)
	sort.Slice(s.m.runs[r.SessionID], func(i, j int) bool { return s.m.runs[r.SessionID][i].N < s.m.runs[r.SessionID][j].N })
	return nil
}

func (s memRuns) Update(_ context.Context, r Run) error {
	defer s.m.lock()()
	runs := s.m.runs[r.SessionID]
	for i, have := range runs {
		if have.N == r.N && have.OrgID == r.OrgID {
			r.OwnerID, r.StartedAt = have.OwnerID, have.StartedAt
			runs[i] = r
			return nil
		}
	}
	return ErrNotFound
}

func (s memRuns) Current(_ context.Context, orgID, sessionID string) (Run, error) {
	defer s.m.lock()()
	runs := s.m.runs[sessionID]
	if len(runs) == 0 || runs[len(runs)-1].OrgID != orgID {
		return Run{}, ErrNotFound
	}
	return runs[len(runs)-1], nil
}

func (s memRuns) List(_ context.Context, orgID string, sessionIDs []string) (map[string][]Run, error) {
	defer s.m.lock()()
	out := map[string][]Run{}
	for _, id := range sessionIDs {
		if runs := s.m.runs[id]; len(runs) > 0 && runs[0].OrgID == orgID {
			out[id] = slices.Clone(runs)
		}
	}
	return out, nil
}

func (s memRuns) Snapshots(_ context.Context, states ...SnapshotState) ([]Run, error) {
	defer s.m.lock()()
	var out []Run
	for _, runs := range s.m.runs {
		for _, r := range runs {
			if r.EndedAt != nil && slices.Contains(states, r.SnapshotState) {
				out = append(out, r)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].EndedAt.Equal(*out[j].EndedAt) {
			return out[i].EndedAt.Before(*out[j].EndedAt)
		}
		if out[i].SessionID != out[j].SessionID {
			return out[i].SessionID < out[j].SessionID
		}
		return out[i].N < out[j].N
	})
	return out, nil
}

func (s memRuns) Reopen(_ context.Context, orgID, sessionID string) error {
	defer s.m.lock()()
	runs := s.m.runs[sessionID]
	if len(runs) == 0 || runs[0].OrgID != orgID {
		return ErrNotFound
	}
	cur := &runs[len(runs)-1]
	cur.EndedAt = nil
	if cur.SnapshotState == SnapshotFailed {
		cur.SnapshotState = SnapshotPending
	}
	return nil
}
