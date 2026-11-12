package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/store"
)

// Cursors and filters narrow what a caller reads and never widen it: a
// listing's `before`, its `total` and `next`, and a log's `after` are
// all applied on top of the owner filter, so naming another user's ID
// in one tells a caller nothing about that user.
//
// @decision(DL-006) @decision(DL-012)

// interleaved puts three sessions and three jobs of each of two users in
// the store, in the order A, B, A, B, A, B, so a page of one is always
// bracketed by rows of the other.
func interleaved(t *testing.T, f *apiFixture, a, b auth.Principal) (aIDs, bIDs []string) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 6; i++ {
		owner, who := a, "a"
		if i%2 == 1 {
			owner, who = b, "b"
		}
		sess := store.Session{
			ID: store.NewID(seedTime()), OrgID: auth.LocalID, OwnerID: owner.UserID,
			Name: who + "-session-" + string(rune('0'+i)), State: store.StateRunning,
		}
		if err := f.store.Sessions().Create(ctx, sess); err != nil {
			t.Fatal(err)
		}
		job := store.Job{
			ID: store.NewID(seedTime()), OrgID: auth.LocalID, OwnerID: owner.UserID,
			SessionID: sess.ID, Kind: store.JobKindReport, State: store.JobSucceeded, Goal: who + " goal",
		}
		if err := f.store.Jobs().Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		if who == "a" {
			aIDs = append(aIDs, sess.ID, job.ID)
		} else {
			bIDs = append(bIDs, sess.ID, job.ID)
		}
	}
	return aIDs, bIDs
}

type page struct {
	Sessions []SessionView `json:"sessions"`
	Jobs     []JobView     `json:"jobs"`
	Next     string        `json:"next"`
	Total    int           `json:"total"`
}

func (f *apiFixture) page(path string) page {
	f.t.Helper()
	resp, body := f.do(http.MethodGet, path, nil)
	if resp.StatusCode != http.StatusOK {
		f.t.Fatalf("GET %s = %d %s", path, resp.StatusCode, body)
	}
	var p page
	if err := json.Unmarshal(body, &p); err != nil {
		f.t.Fatal(err)
	}
	return p
}

// A user paging one row at a time through a listing meets only their own
// rows, `total` counts only theirs, and a cursor that is another user's
// ID neither shows nor hides anything of theirs.
//
// @decision(DL-006) @decision(DL-012)
func TestAPagingCursorNeverWidensTheOwnerFilter(t *testing.T) {
	f := newAPIFixture(t)
	a := auth.Principal{UserID: "u-a", OrgID: auth.LocalID}
	b := auth.Principal{UserID: "u-b", OrgID: auth.LocalID}
	aIDs, _ := interleaved(t, f, a, b)
	f.auth.inner = auth.StaticAuthenticator{Principal: b}
	mine := func(owner string) bool { return owner == b.UserID }

	for _, kind := range []string{"sessions", "jobs"} {
		var got []string
		path := "/api/v1/" + kind + "?limit=1"
		for pages := 0; pages < 10; pages++ {
			p := f.page(path)
			if p.Total != 3 {
				t.Errorf("%s: total = %d, want 3, B's own: a total that counts A's rows says how many there are", kind, p.Total)
			}
			for _, s := range p.Sessions {
				got = append(got, s.ID)
				if !mine(s.OwnerID) {
					t.Errorf("B was paged to %s of %s", s.ID, s.OwnerID)
				}
			}
			for _, j := range p.Jobs {
				got = append(got, j.ID)
				if !mine(j.OwnerID) {
					t.Errorf("B was paged to job %s of %s", j.ID, j.OwnerID)
				}
			}
			if p.Next == "" {
				break
			}
			path = "/api/v1/" + kind + "?limit=1&before=" + p.Next
		}
		if len(got) != 3 {
			t.Errorf("%s: B paged through %d rows, want their 3", kind, len(got))
		}
		for _, id := range aIDs {
			// A's own IDs as the cursor: the answer is B's rows older
			// than it and nothing else.
			p := f.page("/api/v1/" + kind + "?before=" + id)
			if p.Total != 3 {
				t.Errorf("%s: before=%s: total = %d, want 3", kind, id, p.Total)
			}
			for _, s := range p.Sessions {
				if !mine(s.OwnerID) || s.ID >= id {
					t.Errorf("before=%s listed %s of %s", id, s.ID, s.OwnerID)
				}
			}
			for _, j := range p.Jobs {
				if !mine(j.OwnerID) || j.ID >= id {
					t.Errorf("before=%s listed job %s of %s", id, j.ID, j.OwnerID)
				}
			}
		}
	}
	// A filter naming another user's session narrows B's listing to
	// nothing rather than reaching A's job.
	if p := f.page("/api/v1/jobs?session=" + aIDs[0]); len(p.Jobs) != 0 {
		t.Errorf("B listed A's job through the session filter: %+v", p.Jobs)
	}
}

// A log read after a position returns the events of that session alone,
// whatever the position: positions are one sequence across the store,
// and another session's events sit between a session's own.
//
// @decision(DL-006) @decision(DL-018)
func TestALogCursorReadsOnlyThatSessionsEvents(t *testing.T) {
	f := newAPIFixture(t)
	f.srv.streamLifetime = 50 * time.Millisecond
	f.srv.heartbeat = time.Hour
	a := auth.Principal{UserID: "u-a", OrgID: auth.LocalID}
	b := auth.Principal{UserID: "u-b", OrgID: auth.LocalID}
	mk := func(owner auth.Principal, id, name string) store.Session {
		s := store.Session{ID: id, OrgID: auth.LocalID, OwnerID: owner.UserID, Name: name, State: store.StateRunning}
		if err := f.store.Sessions().Create(context.Background(), s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	sa := mk(a, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "a-log")
	sb := mk(b, "01ARZ3NDEKTSV4RRFFQ69G5FAW", "b-log")
	ca := seedConversation(t, f, sa, "ses_a", "a")
	cb := seedConversation(t, f, sb, "ses_b", "b")
	var aPos int64
	for i := 0; i < 3; i++ {
		ea := seedEvent(t, f, store.Event{OrgID: sa.OrgID, SessionID: sa.ID, ConversationID: ca.ID,
			Source: store.SourceOpenCode, Seq: int64(i + 1), Name: "private.of.a", At: recordedAt})
		aPos = ea.Pos
		seedEvent(t, f, store.Event{OrgID: sb.OrgID, SessionID: sb.ID, ConversationID: cb.ID,
			Source: store.SourceOpenCode, Seq: int64(i + 1), Name: "own.of.b", At: recordedAt})
	}
	f.auth.inner = auth.StaticAuthenticator{Principal: b}
	for _, after := range []string{"", "?after=0", "?after=" + strconv.FormatInt(aPos, 10), "?after=" + strconv.FormatInt(aPos-2, 10)} {
		resp, body := f.do(http.MethodGet, "/api/v1/sessions/b-log/log"+after, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("B reading their own log%s = %d %s", after, resp.StatusCode, body)
		}
		if strings.Contains(string(body), "private.of.a") {
			t.Errorf("B's log%s carries A's events: %s", after, body)
		}
		if after == "" && !strings.Contains(string(body), "own.of.b") {
			t.Errorf("B's own log is empty, so the test proves nothing: %s", body)
		}
	}
}
