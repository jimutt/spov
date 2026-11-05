package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/store"
)

// pagedSessions seeds n sessions a second apart, oldest first, all in
// state, and returns their IDs in that order.
func pagedSessions(t *testing.T, f *apiFixture, n int, state store.SessionState) []string {
	t.Helper()
	ids := make([]string, n)
	for i := range n {
		ids[i] = store.NewID(recordedAt.Add(time.Duration(i) * time.Second))
		seedSession(t, f, ids[i], "", state)
	}
	return ids
}

func listSessionsPage(t *testing.T, f *apiFixture, query string) (int, listOutput) {
	t.Helper()
	resp, body := f.do(http.MethodGet, "/api/v1/sessions"+query, nil)
	var out listOutput
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, &out.Body); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, out
}

// @decision(DL-012)
func TestTheSessionListingPagesNewestFirst(t *testing.T) {
	f := newAPIFixture(t)
	ids := pagedSessions(t, f, 5, store.StateStopped)

	var seen []string
	query := "?limit=2"
	for range 5 {
		status, page := listSessionsPage(t, f, query)
		if status != http.StatusOK {
			t.Fatalf("%s: %d", query, status)
		}
		if page.Body.Total != 5 {
			t.Errorf("%s: total = %d, want 5", query, page.Body.Total)
		}
		for _, s := range page.Body.Sessions {
			seen = append(seen, s.ID)
		}
		if page.Body.Next == "" {
			break
		}
		query = "?limit=2&before=" + page.Body.Next
	}
	want := []string{ids[4], ids[3], ids[2], ids[1], ids[0]}
	if len(seen) != len(want) {
		t.Fatalf("paged through %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("paged through %v, want %v newest first", seen, want)
			break
		}
	}

	// A page that ends exactly at the oldest says there is no more.
	if _, page := listSessionsPage(t, f, "?limit=5"); page.Body.Next != "" || len(page.Body.Sessions) != 5 {
		t.Errorf("the whole history in one page: next %q, %d sessions", page.Body.Next, len(page.Body.Sessions))
	}
	// No limit is the default page, and a large one is clamped.
	if status, page := listSessionsPage(t, f, ""); status != http.StatusOK || len(page.Body.Sessions) != 5 {
		t.Errorf("default page: %d, %d sessions", status, len(page.Body.Sessions))
	}
	if status, _ := listSessionsPage(t, f, "?limit=100000"); status != http.StatusOK {
		t.Errorf("an oversized limit is refused (%d), want it clamped", status)
	}
}

// @decision(DL-012)
func TestTheSessionListingFiltersByState(t *testing.T) {
	f := newAPIFixture(t)
	pagedSessions(t, f, 3, store.StateStopped)
	live := pagedSessions(t, f, 1, store.StateRunning)
	_, page := listSessionsPage(t, f, "?state=starting,running")
	if len(page.Body.Sessions) != 1 || page.Body.Sessions[0].ID != live[0] || page.Body.Total != 1 {
		t.Errorf("live sessions = %+v, total %d", page.Body.Sessions, page.Body.Total)
	}
	if status, _ := listSessionsPage(t, f, "?state=sleeping"); status != http.StatusUnprocessableEntity {
		t.Errorf("an unknown state: %d, want 422", status)
	}
}

// The stream's list is the newest page and every live session older
// than it, once each.
//
// @decision(DL-012)
func TestTheStreamListKeepsEveryLiveSession(t *testing.T) {
	f := newAPIFixture(t)
	old := pagedSessions(t, f, 1, store.StateRunning)[0]
	newer := make([]string, 4)
	for i := range newer {
		newer[i] = store.NewID(recordedAt.Add(time.Duration(10+i) * time.Second))
		seedSession(t, f, newer[i], "", store.StateStopped)
	}
	p := auth.Principal{OrgID: auth.LocalID, UserID: auth.LocalID, Admin: true}

	got, err := f.srv.svc.StreamList(context.Background(), p, 2)
	if err != nil {
		t.Fatal(err)
	}
	var gotIDs []string
	for _, s := range got {
		gotIDs = append(gotIDs, s.ID)
	}
	want := []string{newer[3], newer[2], old}
	if len(gotIDs) != len(want) || gotIDs[0] != want[0] || gotIDs[1] != want[1] || gotIDs[2] != want[2] {
		t.Errorf("stream list = %v, want %v", gotIDs, want)
	}

	all, _ := f.srv.svc.StreamList(context.Background(), p, 10)
	if len(all) != 5 {
		t.Errorf("a page larger than the history: %d sessions, want 5 with no repeat", len(all))
	}
}

// @decision(DL-012)
func TestTheJobListingPagesNewestFirst(t *testing.T) {
	f, _ := jobFixture(t)
	var ids []string
	for i := range 3 {
		at := recordedAt.Add(time.Duration(i) * time.Second)
		sess := seedSession(t, f, store.NewID(at), "", store.StateStopped)
		j := store.Job{
			ID: store.NewID(at.Add(time.Millisecond)), OrgID: sess.OrgID, OwnerID: sess.OwnerID, SessionID: sess.ID,
			Kind: store.JobKindReport, State: store.JobQueued, Goal: "g",
		}
		if err := f.store.Jobs().Create(context.Background(), j); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, j.ID)
	}
	resp, body := f.do(http.MethodGet, "/api/v1/jobs?limit=2", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: %d %s", resp.StatusCode, body)
	}
	var page listJobsOutput
	if err := json.Unmarshal(body, &page.Body); err != nil {
		t.Fatal(err)
	}
	if len(page.Body.Jobs) != 2 || page.Body.Jobs[0].ID != ids[2] || page.Body.Next != ids[1] || page.Body.Total != 3 {
		t.Fatalf("first page = %d jobs, first %s, next %q, total %d", len(page.Body.Jobs), page.Body.Jobs[0].ID, page.Body.Next, page.Body.Total)
	}
	_, body = f.do(http.MethodGet, "/api/v1/jobs?limit=2&before="+page.Body.Next, nil)
	page = listJobsOutput{}
	if err := json.Unmarshal(body, &page.Body); err != nil {
		t.Fatal(err)
	}
	if len(page.Body.Jobs) != 1 || page.Body.Jobs[0].ID != ids[0] || page.Body.Next != "" {
		t.Errorf("last page = %+v, next %q", page.Body.Jobs, page.Body.Next)
	}
}
