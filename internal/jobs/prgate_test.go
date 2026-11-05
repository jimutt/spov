package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/jobs/jobstest"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/store"
)

// Gate 2's impure half for a pr job: getting the recorded pull requests
// onto the job and asking each one's forge. The pure gate is tested from
// fixtures in verdict_test.go.
//
// @decision(DL-051) @decision(DL-050)

var selected = []store.RepoRef{
	{Platform: "codefloe", Owner: "acme", Name: "app"},
	{Platform: "github", Owner: "acme", Name: "lib"},
}

// prGateJob seeds a delivering pr job whose session selected repos.
func prGateJob(t *testing.T, st store.Store, repos []store.RepoRef) store.Job {
	t.Helper()
	ctx := context.Background()
	job := jobstest.SeedJobWith(t, st, store.JobDelivering, func(j *store.Job) { j.Kind = store.JobKindPR })
	sess, err := st.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	sess.Request.Repos = repos
	if err := st.Sessions().Update(ctx, sess); err != nil {
		t.Fatal(err)
	}
	return job
}

// recordLine is the line the pull request tool writes.
func recordLine(platform, repo, branch string) string {
	b, _ := json.Marshal(map[string]any{
		"event": opencode.DeliverableEvent, "at": 1,
		"platform": platform, "repository": repo, "branch": branch,
	})
	return string(b)
}

// putEvidence stores lines as the evidence marker delivery writes.
func putEvidence(t *testing.T, st store.Store, job store.Job, source store.EventSource, text string) {
	t.Helper()
	if _, err := st.Conversations().PutEvents(context.Background(), []store.Event{{
		ID: store.NewID(time.Now()), OrgID: job.OrgID, SessionID: job.SessionID,
		Source: source, Name: MarkerEvidence, At: time.Now(), Text: text,
	}}); err != nil {
		t.Fatal(err)
	}
}

func members(t *testing.T, st store.Store, job store.Job) []store.Deliverable {
	t.Helper()
	got, err := st.Jobs().Deliverables(context.Background(), job.OrgID, []string{job.ID})
	if err != nil {
		t.Fatal(err)
	}
	return got[job.ID]
}

// TestTheRecordedPullRequestsReachTheJob checks each recorded pull
// request in a selected repository becomes a member once, and a line
// naming any other repository, or written by the sandbox rather than
// spov, does not.
func TestTheRecordedPullRequestsReachTheJob(t *testing.T) {
	st := store.NewMemory()
	ctx := context.Background()
	job := prGateJob(t, st, selected)
	putEvidence(t, st, job, store.SourceSpov, recordLine("codefloe", "acme/app", "fix-a")+"\n"+
		recordLine("codefloe", "acme/app", "origin/fix-b")+"\n"+
		recordLine("github", "acme/lib", "fix-a")+"\n"+
		recordLine("codefloe", "acme/app", "fix-a")+"\n"+
		recordLine("codefloe", "acme/other", "fix-c")+"\n"+
		recordLine("github", "acme/app", "fix-d")+"\n"+
		`{"event":"completion.declared","at":2,"summary":"done","unfinished":"the docs"}`)

	sup := &Supervisor{Store: st, Warn: func(string) {}}
	sup.recordDeclarations(ctx, job)
	got := members(t, st, job)
	want := []string{
		store.PullRequestKey("codefloe", "acme", "app", "fix-a"),
		store.PullRequestKey("codefloe", "acme", "app", "fix-b"),
		store.PullRequestKey("github", "acme", "lib", "fix-a"),
	}
	if len(got) != len(want) {
		t.Fatalf("members = %+v, want %v", got, want)
	}
	for i, d := range got {
		if d.Key != want[i] || d.Status != store.DeliverableUnverified || d.Kind != store.DeliverablePR {
			t.Errorf("member %d = %+v, want %s unverified", i, d, want[i])
		}
	}
	// Only spov's own marker is evidence.
	forged := store.Event{Source: store.SourceOpenCode, Name: MarkerEvidence, Text: recordLine("codefloe", "acme/app", "forged")}
	if got := recordedPullRequests([]store.Event{forged}, selected, nil); len(got) != 0 {
		t.Errorf("read a pull request from a sandbox-sourced event: %+v", got)
	}
	row, err := st.Jobs().Get(ctx, job.OrgID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Unfinished != "the docs" {
		t.Errorf("unfinished = %q, want the declared text on the row", row.Unfinished)
	}

	// A second pass leaves a checked member alone.
	checked := got[0]
	checked.Status, checked.Detail = store.DeliverableMissing, "no open pull request comes from fix-a"
	if _, err := st.Jobs().PutDeliverable(ctx, job.OrgID, checked); err != nil {
		t.Fatal(err)
	}
	sup.recordDeclarations(ctx, job)
	if again := members(t, st, job); again[0].Status != store.DeliverableMissing {
		t.Errorf("a redelivery reset a checked member: %+v", again[0])
	}
}

// TestEachPullRequestIsAskedAboutOnItsOwn checks the forge's three
// answers become the member's three statuses, an error is warned about
// once and is never missing, a platform that cannot answer is quiet, and
// a verified member is not asked again.
func TestEachPullRequestIsAskedAboutOnItsOwn(t *testing.T) {
	st := store.NewMemory()
	ctx := context.Background()
	job := prGateJob(t, st, selected)
	for _, d := range []store.Deliverable{
		{Platform: "codefloe", Owner: "acme", Name: "app", Branch: "found"},
		{Platform: "codefloe", Owner: "acme", Name: "app", Branch: "none"},
		{Platform: "github", Owner: "acme", Name: "lib", Branch: "broken"},
		{Platform: "github", Owner: "acme", Name: "lib", Branch: "unsupported"},
	} {
		d.JobID, d.Kind, d.Status = job.ID, store.DeliverablePR, store.DeliverableUnverified
		d.Key = store.PullRequestKey(d.Platform, d.Owner, d.Name, d.Branch)
		if _, err := st.Jobs().PutDeliverable(ctx, job.OrgID, d); err != nil {
			t.Fatal(err)
		}
	}
	var asked []string
	var warnings []string
	sup := &Supervisor{Store: st,
		Warn: func(m string) { warnings = append(warnings, m) },
		FindPullRequest: func(_ context.Context, _ store.Job, d store.Deliverable) (*git.PullRequest, error) {
			asked = append(asked, d.Branch)
			switch d.Branch {
			case "found":
				return &git.PullRequest{Number: 7, URL: "https://codefloe.com/acme/app/pulls/7", Title: "Fix", Open: true}, nil
			case "none":
				return nil, nil
			case "broken":
				return nil, errors.New("the forge is unhappy")
			}
			return nil, git.ErrUnsupported
		}}
	sup.checkPullRequests(ctx, job)
	byBranch := map[string]store.Deliverable{}
	for _, d := range members(t, st, job) {
		byBranch[d.Branch] = d
	}
	if d := byBranch["found"]; d.Status != store.DeliverableVerified || d.Number != 7 || d.Title != "Fix" || d.URL == "" {
		t.Errorf("found = %+v", d)
	}
	if d := byBranch["none"]; d.Status != store.DeliverableMissing {
		t.Errorf("none = %+v, want missing", d)
	}
	for _, b := range []string{"broken", "unsupported"} {
		if d := byBranch[b]; d.Status != store.DeliverableUnverified {
			t.Errorf("%s = %+v: spov's failure to ask became a finding about the job", b, d)
		}
	}
	if len(warnings) != 1 {
		t.Errorf("warned %d times, want the broken query alone: %v", len(warnings), warnings)
	}
	asked = nil
	sup.checkPullRequests(ctx, job)
	for _, b := range asked {
		if b == "found" {
			t.Error("a verified pull request was asked about again")
		}
	}
	if len(asked) != 3 {
		t.Errorf("a second pass asked about %v, want the three not yet verified", asked)
	}
}

// FindPullRequestVia: the wiring that turns a member into a forge query.
//
// @decision(DL-050) @decision(DL-051)

func member(platform, owner, name, branch string) store.Deliverable {
	return store.Deliverable{Kind: store.DeliverablePR, Platform: platform, Owner: owner, Name: name, Branch: branch}
}

// TestFindPullRequestViaAsksTheMembersOwnRepository checks the member's
// own repository and platform are asked, with the session owner's forge.
func TestFindPullRequestViaAsksTheMembersOwnRepository(t *testing.T) {
	st := store.NewMemory()
	job := prGateJob(t, st, selected)

	codefloe := git.NewFake()
	codefloe.PlatformName = "codefloe"
	gh := git.NewFake()
	gh.PlatformName = "github"
	gh.PullRequests = map[string]git.PullRequest{
		"acme/lib@impl/widgets": {Number: 74, URL: "https://github.com/acme/lib/pull/74"},
	}
	// The resolver is asked about the session's OWNER (DL-050).
	var askedOrg, askedUser string
	find := FindPullRequestVia(func(_ context.Context, sess store.Session) (Forge, error) {
		askedOrg, askedUser = sess.OrgID, sess.OwnerID
		return &fakeForge{platforms: []*git.Fake{codefloe, gh}}, nil
	}, st)

	got, err := find(context.Background(), job, member("github", "acme", "lib", "impl/widgets"))
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Number != 74 {
		t.Fatalf("got %+v, want the pull request in the member's own repository", got)
	}
	sess, err := st.Sessions().Get(context.Background(), job.OrgID, job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if askedOrg != sess.OrgID || askedUser != sess.OwnerID || sess.OwnerID == "" {
		t.Errorf("resolved the forge for %q/%q, want the session's owner %q/%q",
			askedOrg, askedUser, sess.OrgID, sess.OwnerID)
	}
	// The same repository name on the other platform has none.
	if got, err := find(context.Background(), job, member("codefloe", "acme", "lib", "impl/widgets")); err != nil || got != nil {
		t.Errorf("asked the wrong platform: %+v, %v", got, err)
	}
}

// TestFindPullRequestViaWithoutACredentialCannotAnswer: a resolver that
// produces no forge reports "cannot answer" rather than "found none".
//
// @decision(DL-050)
func TestFindPullRequestViaWithoutACredentialCannotAnswer(t *testing.T) {
	st := store.NewMemory()
	job := prGateJob(t, st, selected)
	find := FindPullRequestVia(func(context.Context, store.Session) (Forge, error) {
		return nil, nil
	}, st)
	if _, err := find(context.Background(), job, member("codefloe", "acme", "app", "b")); !errors.Is(err, git.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	if _, err := FindPullRequestVia(nil, st)(context.Background(), job, member("codefloe", "acme", "app", "b")); !errors.Is(err, git.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported with no forge at all", err)
	}
}

// TestFindPullRequestViaReportsAFailure checks a resolver or forge that
// failed is a reported error, never ErrUnsupported's silent path.
//
// @decision(DL-050)
func TestFindPullRequestViaReportsAFailure(t *testing.T) {
	st := store.NewMemory()
	job := prGateJob(t, st, selected)
	for name, find := range map[string]func(context.Context, store.Job, store.Deliverable) (*git.PullRequest, error){
		"resolver": FindPullRequestVia(func(context.Context, store.Session) (Forge, error) {
			return nil, errors.New("the credential store is unreachable")
		}, st),
		"forge": FindPullRequestVia(staticForge(&fakeForge{err: errors.New("no credential")}), st),
	} {
		_, err := find(context.Background(), job, member("codefloe", "acme", "app", "b"))
		if err == nil || errors.Is(err, git.ErrUnsupported) {
			t.Errorf("%s: err = %v, want a reported failure", name, err)
		}
	}
}

// fakeForge is the narrow half of a forge this package needs: gate 2
// asks about a pull request and never lists repositories. It groups the
// refs by platform, as the server's does.
//
// @decision(DL-050) @decision(DL-046)
type fakeForge struct {
	platform  *git.Fake
	platforms []*git.Fake
	err       error
}

// staticForge is the resolver for a test that does not care which user
// is asked about.
//
// @decision(DL-050)
func staticForge(f Forge) ForgeFor {
	return func(context.Context, store.Session) (Forge, error) { return f, nil }
}

func (f *fakeForge) Selections(_ context.Context, refs []store.RepoRef) ([]git.Selection, error) {
	if f.err != nil {
		return nil, f.err
	}
	plats := f.platforms
	if f.platform != nil {
		plats = append(plats, f.platform)
	}
	var out []git.Selection
	for _, p := range plats {
		sel := git.Selection{Platform: p}
		for _, r := range refs {
			if r.Platform == p.Name() {
				sel.Repos = append(sel.Repos, git.Repo{Owner: r.Owner, Name: r.Name})
			}
		}
		if len(sel.Repos) > 0 {
			out = append(out, sel)
		}
	}
	return out, nil
}

// A merge request in a GitLab subgroup is recorded under the project's
// whole group path, and a cut at the first slash would match nothing.
//
// @decision(DL-041)
func TestARecordedRequestInASubgroupKeepsItsWholePath(t *testing.T) {
	repos := []store.RepoRef{{Platform: "gitlab", Owner: "group/sub", Name: "project"}}
	ev := store.Event{Source: store.SourceSpov, Name: MarkerEvidence, Text: recordLine("gitlab", "group/sub/project", "spov/job") + "\n" +
		recordLine("gitlab", "group/project", "spov/job")}
	got := recordedPullRequests([]store.Event{ev}, repos, map[string]string{"gitlab": "gitlab"})
	if len(got) != 1 || got[0].Owner != "group/sub" || got[0].Name != "project" {
		t.Fatalf("members = %+v", got)
	}
	// The member knows its forge, so it can be called a merge request.
	// @decision(DL-051)
	if got[0].Forge != "gitlab" {
		t.Errorf("forge = %q", got[0].Forge)
	}
	if got[0].Key != store.PullRequestKey("gitlab", "group/sub", "project", "spov/job") {
		t.Errorf("key = %q", got[0].Key)
	}
}

// Gate 2 calls GitLab's members merge requests, and a mixed set pull
// requests.
//
// @decision(DL-051)
func TestGateTwoNamesChangesTheForgesWay(t *testing.T) {
	gl := store.Deliverable{Kind: store.DeliverablePR, Forge: "gitlab"}
	gh := store.Deliverable{Kind: store.DeliverablePR, Forge: "github"}
	for _, c := range []struct {
		members []store.Deliverable
		want    string
	}{
		{[]store.Deliverable{gl, gl}, "merge request"},
		{[]store.Deliverable{gl, gh}, "pull request"},
		{[]store.Deliverable{gh}, "pull request"},
		{nil, "pull request"},
	} {
		if got := changeNoun(c.members); got != c.want {
			t.Errorf("%v: %q, want %q", c.members, got, c.want)
		}
	}
}

// A withdrawal is honoured only when a person wrote after the pull
// request was recorded; one never recorded withdraws nothing, and a later
// record takes a withdrawal back.
//
// @decision(DL-051)
func TestAWithdrawalNeedsAPersonsMessageAfterTheRecord(t *testing.T) {
	t0 := time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC)
	line := func(branch string, at time.Time, withdrawn bool) string {
		m := map[string]any{"event": opencode.DeliverableEvent, "at": at.Format(time.RFC3339Nano),
			"platform": "codefloe", "repository": "acme/app", "branch": branch}
		if withdrawn {
			m["withdrawn"] = true
		}
		b, _ := json.Marshal(m)
		return string(b)
	}
	evidence := func(lines ...string) store.Event {
		return store.Event{Source: store.SourceSpov, Name: MarkerEvidence, Text: strings.Join(lines, "\n")}
	}
	person := func(at time.Time) store.Event {
		return store.Event{Source: store.SourceOpenCode, Name: EventInboxEnqueued, At: at,
			Raw: []byte(`{"type":"session.inbox.enqueued","data":{"item":{"type":"user","payload":{"text":"drop it"}}}}`)}
	}
	status := func(events []store.Event) map[string]store.DeliverableStatus {
		out := map[string]store.DeliverableStatus{}
		for _, d := range recordedPullRequests(events, selected, nil) {
			out[d.Branch] = d.Status
		}
		return out
	}
	ev := evidence(line("a", t0, false), line("a", t0.Add(2*time.Minute), true),
		line("b", t0, false), line("b", t0.Add(2*time.Minute), true), line("b", t0.Add(3*time.Minute), false),
		line("never", t0, true))
	if got := status([]store.Event{ev, person(t0.Add(time.Minute))}); got["a"] != store.DeliverableWithdrawn ||
		got["b"] != store.DeliverableUnverified || len(got) != 2 {
		t.Errorf("with a person's message after the records: %v", got)
	}
	if got := status([]store.Event{person(t0.Add(-time.Minute)), ev}); got["a"] != store.DeliverableUnverified {
		t.Errorf("a person's message before the record honoured the withdrawal: %v", got)
	}
	if got := status([]store.Event{ev}); got["a"] != store.DeliverableUnverified {
		t.Errorf("with no person's message: %v", got)
	}
}

// Gate 2 leaves a withdrawn member out of the set and names it; a
// withdrawal nobody asked for is said, and judged as recorded.
//
// @decision(DL-051)
func TestGateTwoLeavesAWithdrawnPullRequestOut(t *testing.T) {
	job := store.Job{Kind: store.JobKindPR}
	pr := func(branch string, st store.DeliverableStatus) store.Deliverable {
		return store.Deliverable{Kind: store.DeliverablePR, Status: st, Platform: "codefloe", Owner: "acme", Name: "app", Branch: branch,
			Key: store.PullRequestKey("codefloe", "acme", "app", branch)}
	}
	g := judgeDeliverable(job, nil, []store.Deliverable{pr("a", store.DeliverableVerified), pr("b", store.DeliverableWithdrawn)})
	if g.Outcome != GatePassed || !strings.Contains(g.Detail, "1 of 1 pull request verified") || !strings.Contains(g.Detail, "1 withdrawn") {
		t.Errorf("one verified, one withdrawn: %+v", g)
	}
	g = judgeDeliverable(job, nil, []store.Deliverable{pr("b", store.DeliverableWithdrawn)})
	if g.Outcome != GateFailed || !strings.Contains(g.Detail, "every pull request the job recorded was withdrawn") {
		t.Errorf("everything withdrawn: %+v", g)
	}
	unasked := []store.Event{{Source: store.SourceSpov, Name: MarkerEvidence, Text: `{"event":"` + opencode.DeliverableEvent +
		`","platform":"codefloe","repository":"acme/app","branch":"b"}` + "\n" + `{"event":"` + opencode.DeliverableEvent +
		`","platform":"codefloe","repository":"acme/app","branch":"b","withdrawn":true}`}}
	g = judgeDeliverable(job, unasked, []store.Deliverable{pr("a", store.DeliverableVerified), pr("b", store.DeliverableMissing)})
	if g.Outcome != GatePartial || !strings.Contains(g.Detail, "withdrew 1 pull request nobody asked it to drop") {
		t.Errorf("an unasked withdrawal: %+v", g)
	}
}

// The outcome file decides the record lines when there is one: a
// withdrawal past the evidence file's cut still counts.
//
// @decision(DL-051)
func TestAWithdrawalPastTheEvidenceCutCounts(t *testing.T) {
	t0 := time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC)
	rec := `{"event":"` + opencode.DeliverableEvent + `","at":"` + t0.Format(time.RFC3339Nano) +
		`","platform":"codefloe","repository":"acme/app","branch":"b"}`
	drop := `{"event":"` + opencode.DeliverableEvent + `","at":"` + t0.Add(2*time.Minute).Format(time.RFC3339Nano) +
		`","platform":"codefloe","repository":"acme/app","branch":"b","withdrawn":true}`
	events := []store.Event{
		{Source: store.SourceSpov, Name: MarkerOutcome, Text: rec + "\n" + drop},
		{Source: store.SourceSpov, Name: MarkerEvidence, Text: rec},
		{Source: store.SourceOpenCode, Name: EventInboxEnqueued, At: t0.Add(time.Minute),
			Raw: []byte(`{"type":"session.inbox.enqueued","data":{"item":{"type":"user","payload":{"text":"drop b"}}}}`)},
	}
	got := recordedPullRequests(events, selected, nil)
	if len(got) != 1 || got[0].Status != store.DeliverableWithdrawn {
		t.Errorf("members = %+v, want b withdrawn from the outcome file's last word", got)
	}
}

// A merged or closed pull request opened before the job is another job's
// on a reused branch name; an open one counts at any age.
//
// @decision(DL-051)
func TestAnOldPullRequestOnAReusedBranchIsNotTheJobs(t *testing.T) {
	created := time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC)
	job := store.Job{ID: "j", OrgID: "org", Kind: store.JobKindPR, CreatedAt: created}
	d := store.Deliverable{Kind: store.DeliverablePR, Platform: "codefloe", Owner: "acme", Name: "app", Branch: "fix"}
	for _, tc := range []struct {
		pr   git.PullRequest
		want store.DeliverableStatus
	}{
		{git.PullRequest{Number: 1, Merged: true, Created: created.AddDate(0, -1, 0)}, store.DeliverableMissing},
		{git.PullRequest{Number: 2, Created: created.AddDate(0, -1, 0)}, store.DeliverableMissing},
		{git.PullRequest{Number: 3, Open: true, Created: created.AddDate(0, -1, 0)}, store.DeliverableVerified},
		{git.PullRequest{Number: 4, Merged: true, Created: created.Add(10 * time.Minute)}, store.DeliverableVerified},
	} {
		sup := &Supervisor{Warn: func(string) {}, FindPullRequest: func(context.Context, store.Job, store.Deliverable) (*git.PullRequest, error) {
			pr := tc.pr
			return &pr, nil
		}}
		if got := sup.checkPullRequest(context.Background(), job, d); got.Status != tc.want {
			t.Errorf("#%d: %s (%q), want %s", tc.pr.Number, got.Status, got.Detail, tc.want)
		}
	}
}
