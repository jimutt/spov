package jobs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jimutt/spov/internal/jobs/jobstest"
	"github.com/jimutt/spov/internal/prep"
	"github.com/jimutt/spov/internal/store"
)

// The pass's title is stored with the assessment, in the same write.
//
// @decision(DL-150)
func TestAPassGivesTheJobItsTitle(t *testing.T) {
	st := store.NewMemory()
	job := prepJob(t, st, "the login test fails one run in ten", "")
	sup, _, _ := prepSup(st, prep.Assessment{Summary: "S", Title: "Fix the flaky login test"}, nil)

	got := sup.prepare(context.Background(), job)
	if got.Title != "Fix the flaky login test" || got.Assessment != "S" {
		t.Errorf("prepared %+v", got)
	}
	stored, err := st.Jobs().Get(context.Background(), job.OrgID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Title != "Fix the flaky login test" || stored.Assessment != "S" {
		t.Errorf("the row holds title %q and assessment %q", stored.Title, stored.Assessment)
	}
}

// A pass that gave only a title is a pass that ran: it is recorded,
// and a redelivery pays for no second call.
//
// @decision(DL-150) @decision(DL-047)
func TestATitleAloneIsRecordedAndNotAskedForAgain(t *testing.T) {
	st := store.NewMemory()
	job := prepJob(t, st, "do the thing", "")
	sup, calls, _ := prepSup(st, prep.Assessment{Title: "Do the thing"}, nil)

	got := sup.prepare(context.Background(), job)
	if got.Title != "Do the thing" || got.Assessment != "" || got.CheckProposed {
		t.Errorf("prepared %+v", got)
	}
	again, err := st.Jobs().Get(context.Background(), job.OrgID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	sup.prepare(context.Background(), again)
	if *calls != 1 {
		t.Errorf("made %d calls; a titled job was asked again", *calls)
	}
}

// A hostile title is stored as one clean line of at most 120
// characters.
//
// @decision(DL-150) @decision(DL-149)
func TestAHostileTitleIsStoredCleanedAndBounded(t *testing.T) {
	st := store.NewMemory()
	job := prepJob(t, st, "fix the build", "")
	hostile := "Fix\x1b[2J the\nbuild\u202e\u2066 now " + strings.Repeat("long ", 100)
	sup, _, _ := prepSup(st, prep.Assessment{Title: hostile}, nil)

	sup.prepare(context.Background(), job)
	stored, err := st.Jobs().Get(context.Background(), job.OrgID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stored.Title, "Fix [2J the build now long") ||
		strings.ContainsAny(stored.Title, "\x1b\n\u202e\u2066") || utf8.RuneCountInString(stored.Title) > store.MaxTitleRunes {
		t.Errorf("stored the title %q", stored.Title)
	}
}

// A job that skips the pass, a pass that fails and a pass that cannot
// be recorded leave the job without a title, and none of them is an
// error.
//
// @decision(DL-150) @decision(DL-138)
func TestAJobWithoutAPassHasNoTitle(t *testing.T) {
	ctx := context.Background()
	titled := prep.Assessment{Summary: "S", Title: "Fix the build"}

	st := store.NewMemory()
	skipping := jobstest.SeedJobWith(t, st, store.JobQueued, func(j *store.Job) { j.SkipPrep = true })
	sup, _, _ := prepSup(st, titled, nil)
	if got := sup.prepare(ctx, skipping); got.Title != "" {
		t.Errorf("a job that skips prep got the title %q", got.Title)
	}

	failing := prepJob(t, st, "fix the build", "")
	sup, _, _ = prepSup(st, titled, errors.New("dial tcp: refused"))
	if got := sup.prepare(ctx, failing); got.Title != "" {
		t.Errorf("a failed pass left the title %q", got.Title)
	}

	base := store.NewMemory()
	unrecorded := prepJob(t, base, "fix the build", "")
	sup = &Supervisor{Store: failingStore{base, errors.New("the database went away")}, Warn: func(string) {},
		Prepare: func(context.Context, string) (prep.Assessment, error) { return titled, nil }}
	if got := sup.prepare(ctx, unrecorded); got.Title != "" {
		t.Errorf("a pass that could not be recorded left the title %q", got.Title)
	}

	for _, j := range []struct {
		st store.Store
		id string
	}{{st, skipping.ID}, {st, failing.ID}, {base, unrecorded.ID}} {
		stored, err := j.st.Jobs().Get(ctx, "org_1", j.id)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Title != "" {
			t.Errorf("the row of %s holds the title %q", j.id, stored.Title)
		}
	}
}
