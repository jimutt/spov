package jobs

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/store"
)

// wholeHistoryStore fails a test that reads every message of a session.
type wholeHistoryStore struct {
	store.Store
	t *testing.T
}

func (s wholeHistoryStore) Conversations() store.ConversationStore {
	return wholeHistoryConvs{s.Store.Conversations(), s.t}
}

type wholeHistoryConvs struct {
	store.ConversationStore
	t *testing.T
}

func (c wholeHistoryConvs) ListMessages(context.Context, string, string) ([]store.Message, error) {
	c.t.Error("a pass read every message of the session")
	return nil, nil
}

// TestTheStopMarkerIsLookedForOnlyAmongTheNewestOfTheJobsConversation is
// DL-131: the pass that recovers a stop marker from the transcript reads
// the newest StopMarkerWindow messages of the job's own conversation,
// never the session's whole history.
func TestTheStopMarkerIsLookedForOnlyAmongTheNewestOfTheJobsConversation(t *testing.T) {
	ctx := context.Background()
	at := time.Now()
	put := func(t *testing.T, f *followFixture, conv, role, text string) {
		t.Helper()
		at = at.Add(time.Second)
		if _, err := f.st.Conversations().PutMessages(ctx, []store.Message{{
			ID: store.NewID(at), OrgID: f.job.OrgID, SessionID: f.job.SessionID,
			ConversationID: conv, OpenCodeID: store.NewID(at), Role: role, Text: text, At: at,
		}}); err != nil {
			t.Fatal(err)
		}
	}
	stateAfter := func(t *testing.T, f *followFixture) store.JobState {
		t.Helper()
		f.sup.Store = wholeHistoryStore{f.st, t}
		if err := f.sup.followOnce(ctx, f.job); err != nil {
			t.Fatal(err)
		}
		got, err := f.st.Jobs().Get(ctx, f.job.OrgID, f.job.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.State
	}

	t.Run("a marker among the newest is found", func(t *testing.T) {
		f := newFollowFixture(t, store.JobRunning)
		put(t, f, f.conv, "synthetic", stopMarkerText())
		for i := range StopMarkerWindow - 1 {
			put(t, f, f.conv, "assistant", fmt.Sprintf("after %d", i))
		}
		if got := stateAfter(t, f); got != store.JobDelivering {
			t.Errorf("state = %v, want delivering", got)
		}
	})
	t.Run("a marker older than the window is not", func(t *testing.T) {
		f := newFollowFixture(t, store.JobRunning)
		put(t, f, f.conv, "synthetic", stopMarkerText())
		for i := range StopMarkerWindow {
			put(t, f, f.conv, "assistant", fmt.Sprintf("after %d", i))
		}
		if got := stateAfter(t, f); got != store.JobRunning {
			t.Errorf("state = %v, want running", got)
		}
	})
	t.Run("a marker in a subagent's conversation is not the run's", func(t *testing.T) {
		f := newFollowFixture(t, store.JobRunning)
		sub := store.NewID(time.Now().Add(time.Hour))
		if _, err := f.st.Conversations().UpsertConversation(ctx, store.Conversation{
			ID: sub, OrgID: f.job.OrgID, SessionID: f.job.SessionID, OpenCodeID: "ses_sub", ParentOpenCodeID: "ses_main",
		}); err != nil {
			t.Fatal(err)
		}
		put(t, f, sub, "synthetic", stopMarkerText())
		if got := stateAfter(t, f); got != store.JobRunning {
			t.Errorf("state = %v, want running", got)
		}
	})
}
