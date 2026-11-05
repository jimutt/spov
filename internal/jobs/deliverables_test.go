package jobs

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/jobs/jobstest"
	"github.com/jimutt/spov/internal/store"
)

// fakeOut is a job's out directory: files by path under it.
type fakeOut struct {
	files   map[string][]byte
	listErr error
	openErr error
	readErr map[string]error
	reads   []string
}

func (f *fakeOut) open(context.Context, store.Job) (OutDir, error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	return f, nil
}

func (f *fakeOut) List(_ context.Context, dir string) ([]OutEntry, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	seen := map[string]bool{}
	var out []OutEntry
	for p := range f.files {
		rest, ok := strings.CutPrefix(p, dir)
		if !ok {
			continue
		}
		if first, _, deeper := strings.Cut(rest, "/"); deeper {
			if !seen[first] {
				seen[first] = true
				out = append(out, OutEntry{Path: first + "/", Dir: true})
			}
			continue
		}
		out = append(out, OutEntry{Path: rest})
	}
	return out, nil
}

func (f *fakeOut) Read(_ context.Context, p string, max int64) ([]byte, error) {
	f.reads = append(f.reads, p)
	if err := f.readErr[p]; err != nil {
		return nil, err
	}
	body, ok := f.files[p]
	if !ok {
		return nil, ErrNoFile
	}
	if int64(len(body)) > max {
		return body[:max], ErrFileTruncated
	}
	return body, nil
}

// markInto is the Mark seam writing spov markers into st.
func markInto(st store.Store) func(context.Context, store.Job, string, string) error {
	return func(ctx context.Context, job store.Job, name, text string) error {
		_, err := st.Conversations().PutEvents(ctx, []store.Event{{
			ID: store.NewID(time.Now()), OrgID: job.OrgID, SessionID: job.SessionID,
			Source: store.SourceSpov, Name: name, At: time.Now(), Text: text,
		}})
		return err
	}
}

var (
	pngBytes  = []byte("\x89PNG\r\n\x1a\nrest of a png")
	svgBytes  = []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	htmlAsPNG = []byte("<html><script>alert(1)</script></html>")
)

// TestEveryReportAndItsImagesAreCollected checks every top-level
// Markdown file is a report with its title, the images a report links
// are stored with the type their bytes show, and what is not an image,
// not linked, or outside the directory is not.
//
// @decision(DL-051) @decision(DL-063)
func TestEveryReportAndItsImagesAreCollected(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobDelivering)
	out := &fakeOut{files: map[string][]byte{
		"findings.md": []byte("intro\n# Findings\n\n![chart](images/chart.png)\n![x](images/x.svg)\n" +
			"![fake](images/fake.png)\n![up](../secret.png)\n![remote](https://example.com/a.png)\n" +
			"![gone](images/gone.png)\n![ref][r]\n\n[r]: <images/ref%20one.png>\n"),
		"b-report.md":         []byte("no heading here"),
		".hidden.md":          []byte("# hidden"),
		"notes.txt":           []byte("not a report"),
		"sub/nested.md":       []byte("# nested"),
		"images/chart.png":    pngBytes,
		"images/x.svg":        svgBytes,
		"images/fake.png":     htmlAsPNG,
		"images/ref one.png":  pngBytes,
		"images/unlinked.png": pngBytes,
	}}
	sup := &Supervisor{Store: st, Warn: func(string) {}, OpenOut: out.open}
	if err := sup.collectOut(ctx, job); err != nil {
		t.Fatal(err)
	}
	got := members(t, st, job)
	if len(got) != 2 {
		t.Fatalf("members = %+v, want b-report.md and findings.md", got)
	}
	b, f := got[0], got[1]
	if b.Path != "b-report.md" || b.Title != "b-report" || b.Status != store.DeliverableVerified || b.Size != 15 {
		t.Errorf("b-report.md = %+v", b)
	}
	if f.Path != "findings.md" || f.Title != "Findings" || f.Status != store.DeliverableVerified {
		t.Errorf("findings.md = %+v", f)
	}
	for _, want := range []string{"images/x.svg is not a PNG", "images/fake.png is not a PNG", "images/gone.png was not found"} {
		if !strings.Contains(f.Detail, want) {
			t.Errorf("the report's detail %q does not say %q", f.Detail, want)
		}
	}
	files, err := st.Jobs().JobFiles(ctx, job.OrgID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, file := range files {
		paths = append(paths, file.Path+" "+file.ContentType)
	}
	want := []string{
		"b-report.md text/markdown; charset=utf-8",
		"findings.md text/markdown; charset=utf-8",
		"images/chart.png image/png",
		"images/ref one.png image/png",
	}
	if !slices.Equal(paths, want) {
		t.Errorf("stored %v, want %v", paths, want)
	}
	if slices.Contains(out.reads, "../secret.png") || slices.Contains(out.reads, "secret.png") {
		t.Errorf("read a link outside the out directory: %v", out.reads)
	}
}

// TestARedeliveryReadsNothingTwice checks a second collection leaves
// stored files alone and does not read them from the sandbox again.
//
// @decision(DL-051)
func TestARedeliveryReadsNothingTwice(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobDelivering)
	out := &fakeOut{files: map[string][]byte{
		"a.md": []byte("# A\n![c](c.png)"), "c.png": pngBytes,
	}}
	sup := &Supervisor{Store: st, Warn: func(string) {}, OpenOut: out.open}
	if err := sup.collectOut(ctx, job); err != nil {
		t.Fatal(err)
	}
	out.reads = nil
	out.files["a.md"] = []byte("# changed")
	if err := sup.collectOut(ctx, job); err != nil {
		t.Fatal(err)
	}
	if len(out.reads) != 0 {
		t.Errorf("a second collection read %v again", out.reads)
	}
	f, err := st.Jobs().JobFile(ctx, job.OrgID, job.ID, "a.md")
	if err != nil || string(f.Body) != "# A\n![c](c.png)" {
		t.Errorf("a.md = %q, %v; the stored report was replaced", f.Body, err)
	}
}

// TestOneBadReadLosesNothingElse checks a report that cannot be read is
// unverified while the others are collected.
//
// @decision(DL-051)
func TestOneBadReadLosesNothingElse(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobDelivering)
	out := &fakeOut{
		files:   map[string][]byte{"a.md": []byte("# A"), "b.md": []byte("# B")},
		readErr: map[string]error{"a.md": errors.New("connection reset")},
	}
	sup := &Supervisor{Store: st, Warn: func(string) {}, OpenOut: out.open}
	if err := sup.collectOut(ctx, job); err != nil {
		t.Fatal(err)
	}
	got := members(t, st, job)
	if len(got) != 2 || got[0].Status != store.DeliverableUnverified || got[1].Status != store.DeliverableVerified {
		t.Fatalf("members = %+v, want a.md unverified and b.md verified", got)
	}
	if !strings.Contains(got[0].Detail, "connection reset") {
		t.Errorf("a.md's detail = %q, want why", got[0].Detail)
	}
}

// TestTheJobsTotalIsBounded checks reports are collected in name order
// until the next would pass the job's total, and the rest are over the
// limit, and a gone sandbox is a failed collection, not an empty one.
//
// @decision(DL-051)
func TestTheJobsTotalIsBounded(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobDelivering)
	files := map[string][]byte{}
	for _, name := range []string{"01.md", "02.md", "03.md", "04.md", "05.md", "06.md", "07.md", "08.md", "09.md", "10.md", "11.md"} {
		files[name] = make([]byte, MaxFileBytes)
	}
	out := &fakeOut{files: files}
	sup := &Supervisor{Store: st, Warn: func(string) {}, OpenOut: out.open}
	if err := sup.collectOut(ctx, job); err != nil {
		t.Fatal(err)
	}
	got := members(t, st, job)
	if len(got) != 11 {
		t.Fatalf("members = %d, want 11", len(got))
	}
	for i, d := range got {
		want := store.DeliverableVerified
		if i == 10 {
			want = store.DeliverableOverLimit
		}
		if d.Status != want {
			t.Errorf("%s = %s, want %s", d.Path, d.Status, want)
		}
	}

	gone := jobstest.SeedJob(t, st, store.JobDelivering)
	sup = &Supervisor{Store: st, Warn: func(string) {}, Mark: markInto(st),
		OpenOut: (&fakeOut{openErr: ErrSandboxGone}).open}
	if err := sup.collectOut(ctx, gone); err != nil {
		t.Fatal(err)
	}
	events, err := sup.jobEvents(ctx, gone)
	if err != nil {
		t.Fatal(err)
	}
	if why, broke := collectionFailed(events); !broke || !strings.Contains(why, "already stopped") {
		t.Errorf("a gone sandbox = %q, %v; want a failed collection saying why", why, broke)
	}
}

// TestOutRelativeStaysInside checks a link resolves under the out
// directory or not at all.
//
// @decision(DL-063)
func TestOutRelativeStaysInside(t *testing.T) {
	for dest, want := range map[string]string{
		"images/a.png":          "images/a.png",
		"./images/a.png":        "images/a.png",
		"images/../a.png":       "a.png",
		"<images/my file.png>":  "images/my file.png",
		"images/my%20file.png":  "images/my file.png",
		"a.png?x=1":             "a.png",
		"../a.png":              "",
		"images/../../a.png":    "",
		"/etc/passwd":           "",
		"https://example.com":   "",
		"data:image/png;base64": "",
		"javascript:alert(1)":   "",
		"#heading":              "",
		"..":                    "",
		"a\\b.png":              "",
		"%2e%2e/a.png":          "",
	} {
		got, ok := OutRelative(dest)
		if ok != (want != "") || got != want {
			t.Errorf("OutRelative(%q) = %q, %v; want %q", dest, got, ok, want)
		}
	}
}

// TestImageTypeReadsTheBytes checks only the four raster formats are
// recognised, by content.
//
// @decision(DL-051)
func TestImageTypeReadsTheBytes(t *testing.T) {
	for name, c := range map[string]struct {
		body []byte
		want string
	}{
		"png":  {pngBytes, "image/png"},
		"jpeg": {[]byte{0xff, 0xd8, 0xff, 0xe0, 1}, "image/jpeg"},
		"gif":  {[]byte("GIF89a..."), "image/gif"},
		"webp": {[]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), "image/webp"},
		"svg":  {svgBytes, ""},
		"html": {htmlAsPNG, ""},
		"riff": {[]byte("RIFF\x00\x00\x00\x00WAVEfmt "), ""},
		"none": {nil, ""},
	} {
		if got := ImageType(c.body); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

// TestAReportsTitleIsItsFirstHeading checks the title rule.
//
// @decision(DL-051)
func TestAReportsTitleIsItsFirstHeading(t *testing.T) {
	for body, want := range map[string]string{
		"# Title\n\n# Second":     "Title",
		"text\n  # Indented ##\n": "Indented",
		"# Windows ##\r\nbody":    "Windows",
		"## Not level one\n":      "r",
		"#NoSpace\n":              "r",
		"":                        "r",
	} {
		if got := reportTitle([]byte(body), "r.md"); got != want {
			t.Errorf("reportTitle(%q) = %q, want %q", body, got, want)
		}
	}
}

// TestAPassThatBrokeOffCollectsAgain checks collection is gated by its
// own marker: a pass that stored a report and died before writing its
// member collects again, and one that finished does not.
//
// @decision(DL-051)
func TestAPassThatBrokeOffCollectsAgain(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobDelivering)
	// What a pass that died mid-collection left: the file, no member.
	if _, err := st.Jobs().PutJobFile(ctx, job.OrgID, store.JobFile{
		JobID: job.ID, Path: "a.md", ContentType: reportContentType, Size: 3, Body: []byte("# A"),
	}); err != nil {
		t.Fatal(err)
	}
	gone := &Supervisor{Store: st, Warn: func(string) {}, Mark: markInto(st),
		OpenOut: (&fakeOut{openErr: ErrSandboxGone}).open}
	if err := gone.collectOut(ctx, job); err != nil {
		t.Fatal(err)
	}
	if got := members(t, st, job); len(got) != 1 || got[0].Status != store.DeliverableVerified || got[0].Title != "A" {
		t.Fatalf("members = %+v, want the stored report delivered although the sandbox had gone", got)
	}

	out := &fakeOut{files: map[string][]byte{"a.md": []byte("# A"), "b.md": []byte("# B")}}
	sup := &Supervisor{Store: st, Warn: func(string) {}, Mark: markInto(st), OpenOut: out.open}
	if err := sup.collectOut(ctx, job); err != nil {
		t.Fatal(err)
	}
	if got := members(t, st, job); len(got) != 2 {
		t.Fatalf("members = %+v, want both reports once the sandbox answers", got)
	}
	events, err := sup.jobEvents(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if _, broke := collectionFailed(events); broke {
		t.Error("a collection that worked did not clear the earlier failure")
	}
	out.reads = nil
	if err := sup.collectOut(ctx, job); err != nil {
		t.Fatal(err)
	}
	if len(out.reads) != 0 {
		t.Errorf("a finished collection read %v again", out.reads)
	}
}

// TestTheOutcomeFileOutlivesTheEvidenceBound checks a pull request
// recorded only in the outcome marker still reaches the job, and a pr
// job whose sandbox had gone is unverified rather than failed.
//
// @decision(DL-051) @decision(DL-050)
func TestTheOutcomeFileOutlivesTheEvidenceBound(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := prGateJob(t, st, selected)
	sup := &Supervisor{Store: st, Warn: func(string) {}, Mark: markInto(st),
		Evidence: func(context.Context, store.Job) ([]byte, error) {
			// The evidence file's head only, as the bound keeps it.
			return []byte(`{"event":"permission.allowed","at":1}`), ErrFileTruncated
		},
		Outcome: func(context.Context, store.Job) ([]byte, error) {
			return []byte(recordLine("codefloe", "acme/app", "late") + "\n"), nil
		},
	}
	sup.collectEvidence(ctx, job)
	sup.recordDeclarations(ctx, job)
	if got := members(t, st, job); len(got) != 1 || got[0].Branch != "late" {
		t.Fatalf("members = %+v, want the pull request the outcome file kept", got)
	}

	gone := prGateJob(t, st, selected)
	sup.Outcome = func(context.Context, store.Job) ([]byte, error) { return nil, ErrSandboxGone }
	sup.collectEvidence(ctx, gone)
	events, err := sup.jobEvents(ctx, gone)
	if err != nil {
		t.Fatal(err)
	}
	if g := judgeDeliverable(gone, events, nil); g.Outcome != GateUnverified || !strings.Contains(g.Detail, "already stopped") {
		t.Errorf("gate 2 = %s (%q), want unverified saying the sandbox had gone", g.Outcome, g.Detail)
	}
}

// TestTheCollectionSaysWhatItCollected checks the marker reads as a line
// of the transcript.
//
// @decision(DL-051)
func TestTheCollectionSaysWhatItCollected(t *testing.T) {
	for _, c := range []struct {
		stored []string
		want   string
	}{
		{[]string{"a.md", "b.md", "images/c.png"}, "Collected 2 reports and 1 image from /workspace/out."},
		{[]string{"a.md"}, "Collected 1 report from /workspace/out."},
		{nil, "Collected 0 reports from /workspace/out."},
	} {
		b := &budget{stored: map[string]bool{}}
		for _, p := range c.stored {
			b.stored[p] = true
		}
		if got := collectedNote(b); got != c.want {
			t.Errorf("%v: %q, want %q", c.stored, got, c.want)
		}
	}
}

// TestAnImageThatCannotBeCollectedIsNamedInTheReportsDetail checks an
// oversized image and one that fails to read are each dropped with their
// own reason, while the report itself stays verified and its good image
// is kept.
//
// @decision(DL-051)
func TestAnImageThatCannotBeCollectedIsNamedInTheReportsDetail(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobDelivering)
	big := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, MaxFileBytes)...)
	out := &fakeOut{
		files: map[string][]byte{
			"r.md":    []byte("# R\n![a](big.png)\n![b](flaky.png)\n![c](ok.png)\n"),
			"big.png": big, "flaky.png": pngBytes, "ok.png": pngBytes,
		},
		readErr: map[string]error{"flaky.png": errors.New("sandbox hiccup")},
	}
	sup := &Supervisor{Store: st, Warn: func(string) {}, OpenOut: out.open}
	if err := sup.collectOut(ctx, job); err != nil {
		t.Fatal(err)
	}
	got := members(t, st, job)
	if len(got) != 1 || got[0].Status != store.DeliverableVerified {
		t.Fatalf("members = %+v, want one verified report", got)
	}
	// @decision(DL-051)
	if !got[0].ImagesLost {
		t.Error("the report is not marked as having lost images, so gate 2 would read it as passed")
	}
	for _, want := range []string{"big.png is larger than 10 MiB", "flaky.png could not be read"} {
		if !strings.Contains(got[0].Detail, want) {
			t.Errorf("detail %q does not say %q", got[0].Detail, want)
		}
	}
	files, err := st.Jobs().JobFiles(ctx, job.OrgID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	if want := []string{"ok.png", "r.md"}; !slices.Equal(paths, want) {
		t.Errorf("stored %v, want %v", paths, want)
	}
}

// Only an image a reader would see makes a report partial when it is
// lost: an inline image outside fenced code, or a reference definition an
// image uses. A data file's reference link and an example in a code
// block are collected when they can be, and lost without consequence.
//
// @decision(DL-051) @decision(DL-063)
func TestOnlyAShownImageMakesAReportPartial(t *testing.T) {
	body := []byte("# R\n\n![chart](chart.png) and ![logo][brand] and ![badge]\n\n" +
		"[brand]: logo.png\n[badge]: badge.png\n[raw data]: results.csv\n\n" +
		"```markdown\n![example](example.png)\n```\n")
	got := ShownImages(body)
	for _, p := range []string{"chart.png", "logo.png", "badge.png"} {
		if !got[p] {
			t.Errorf("%s is shown but was not counted", p)
		}
	}
	for _, p := range []string{"results.csv", "example.png"} {
		if got[p] {
			t.Errorf("%s is not shown as an image but was counted", p)
		}
	}

	// Through collection: a lost data file and a lost example leave the
	// report whole; a lost shown image does not.
	for name, c := range map[string]struct {
		report string
		lost   bool
	}{
		"a data file's reference link": {"# R\n\n[raw data]: results.csv\n", false},
		"an example in a code block":   {"# R\n\n```\n![x](example.png)\n```\n", false},
		"a shown image":                {"# R\n\n![x](gone.png)\n", true},
	} {
		st := store.NewMemory()
		job := jobstest.SeedJob(t, st, store.JobDelivering)
		out := &fakeOut{files: map[string][]byte{"r.md": []byte(c.report), "results.csv": []byte("a,b\n")}}
		sup := &Supervisor{Store: st, Warn: func(string) {}, OpenOut: out.open}
		if err := sup.collectOut(context.Background(), job); err != nil {
			t.Fatal(err)
		}
		if m := members(t, st, job); len(m) != 1 || m[0].ImagesLost != c.lost {
			t.Errorf("%s: members %+v, want images lost %v", name, m, c.lost)
		}
	}
}
