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

// titledWorld is a page of sessions covering each source of a title: an
// interactive session's main conversation, a job's own, a job whose
// conversation has one too, and a session whose only titled
// conversation is a subagent's.
type titledWorld struct {
	interactive, unattended, steered, subagent store.Session
	unattendedJob, steeredJob                  store.Job
}

func seedTitledWorld(t *testing.T, f *apiFixture) titledWorld {
	t.Helper()
	ctx := context.Background()
	at := recordedAt
	next := func() time.Time { at = at.Add(time.Second); return at }
	var w titledWorld
	w.interactive = seedSession(t, f, store.NewID(next()), "fly-1", store.StateRunning)
	seedConversation(t, f, w.interactive, "ses_main", "Fix the flaky login test")
	newJob := func(sess store.Session, title string) store.Job {
		j := store.Job{
			ID: store.NewID(next()), OrgID: sess.OrgID, OwnerID: sess.OwnerID, SessionID: sess.ID,
			Kind: store.JobKindReport, State: store.JobRunning, Goal: "g", Title: title,
		}
		if err := f.store.Jobs().Create(ctx, j); err != nil {
			t.Fatal(err)
		}
		return j
	}
	w.unattended = seedSession(t, f, store.NewID(next()), "job-a", store.StateRunning)
	w.unattendedJob = newJob(w.unattended, "Survey the slow queries")
	seedConversation(t, f, w.unattended, "ses_job", "")
	w.steered = seedSession(t, f, store.NewID(next()), "job-b", store.StateRunning)
	w.steeredJob = newJob(w.steered, "The prep pass's title")
	seedConversation(t, f, w.steered, "ses_steered", "OpenCode's title")
	w.subagent = seedSession(t, f, store.NewID(next()), "fly-2", store.StateRunning)
	if _, err := f.store.Conversations().UpsertConversation(ctx, store.Conversation{
		ID: store.NewID(next()), OrgID: auth.LocalID, SessionID: w.subagent.ID, OpenCodeID: "ses_kid",
		ParentOpenCodeID: "ses_gone", Title: "a subagent's title",
	}); err != nil {
		t.Fatal(err)
	}
	return w
}

// A session's title is its main conversation's, else its job's, and a
// subagent's never stands in; the listing, the session read and both
// job reads carry it.
//
// @decision(DL-149)
func TestASessionsTitleIsItsMainConversationsElseItsJobs(t *testing.T) {
	f, _ := jobFixture(t)
	w := seedTitledWorld(t, f)

	byName := listByName(t, f)
	for name, want := range map[string]string{
		"fly-1": "Fix the flaky login test",
		"job-a": "Survey the slow queries",
		"job-b": "OpenCode's title",
		"fly-2": "",
	} {
		if got := byName[name].Title; got != want {
			t.Errorf("the listing's %s has the title %q, want %q", name, got, want)
		}
	}

	for _, c := range []struct {
		sess store.Session
		want string
	}{{w.interactive, "Fix the flaky login test"}, {w.unattended, "Survey the slow queries"}, {w.subagent, ""}} {
		sess, want := c.sess, c.want
		_, body := f.do(http.MethodGet, "/api/v1/sessions/"+sess.ID, nil)
		var v SessionView
		if err := json.Unmarshal(body, &v); err != nil {
			t.Fatal(err)
		}
		if v.Title != want {
			t.Errorf("GET %s has the title %q, want %q", sess.Name, v.Title, want)
		}
	}

	_, body := f.do(http.MethodGet, "/api/v1/jobs", nil)
	var page listJobsOutput
	if err := json.Unmarshal(body, &page.Body); err != nil {
		t.Fatal(err)
	}
	jobTitles := map[string]string{}
	for _, j := range page.Body.Jobs {
		jobTitles[j.ID] = j.Title
	}
	if jobTitles[w.unattendedJob.ID] != "Survey the slow queries" || jobTitles[w.steeredJob.ID] != "OpenCode's title" {
		t.Errorf("the job listing's titles are %v", jobTitles)
	}
	_, body = f.do(http.MethodGet, "/api/v1/jobs/"+w.steeredJob.ID, nil)
	var one JobView
	if err := json.Unmarshal(body, &one); err != nil {
		t.Fatal(err)
	}
	if one.Title != "OpenCode's title" {
		t.Errorf("GET the job has the title %q", one.Title)
	}
}

// A title arriving moves the list's stamp, so an open list's row
// changes its headline.
//
// @decision(DL-149)
func TestTheListStampMovesWhenATitleArrives(t *testing.T) {
	f := newAPIFixture(t)
	ctx := context.Background()
	sess := seedSession(t, f, store.NewID(recordedAt), "fly-1", store.StateRunning)
	seedConversation(t, f, sess, "ses_main", "")
	sessions, err := f.store.Sessions().List(ctx, auth.LocalID, store.SessionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	p := auth.Principal{OrgID: auth.LocalID, UserID: auth.LocalID}
	before := f.srv.decorationStamp(ctx, p, sessions)
	seedConversation(t, f, sess, "ses_main", "Fix the flaky login test")
	if after := f.srv.decorationStamp(ctx, p, sessions); after == before {
		t.Error("the stamp did not move when the session got a title; the row would keep its old headline")
	}
}
