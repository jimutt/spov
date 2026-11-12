package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"io/fs"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/store"
)

// The delivery limits: one collected file, everything one job delivers,
// and how many files that may be.
//
// @decision(DL-051) @decision(DL-063)
const (
	MaxFileBytes      = 10 << 20
	MaxDeliveredBytes = 100 << 20
	MaxDeliveredFiles = 1000
)

// OutEntry is one entry of a listing of a job's out directory. Path is
// relative to the directory listed, and a directory's ends in "/".
//
// @decision(DL-051)
type OutEntry struct {
	Path string
	Dir  bool
}

// OutDir reads a job's out directory in its sandbox, at delivery. Paths
// are relative to opencode.OutDir.
//
// @decision(DL-051) @decision(DL-018)
type OutDir interface {
	// List lists dir ("" for the top) without descending.
	List(ctx context.Context, dir string) ([]OutEntry, error)
	// Read reads one file. ErrNoFile when it is not there or is not
	// a plain file; ErrFileTruncated, with the first max bytes, when
	// it is longer than max.
	Read(ctx context.Context, path string, max int64) ([]byte, error)
}

// OpenOut opens a job's out directory. ErrNoFile when the sandbox
// has already gone.
//
// @decision(DL-051)
type OpenOut func(ctx context.Context, job store.Job) (OutDir, error)

// reportContentType is what a collected report is stored and served as.
const reportContentType = "text/markdown; charset=utf-8"

// collectOut collects a report job's reports and the images they link.
// Each member is written on its own, so one bad read never loses the
// others, and a file already stored is not read again. A listing that
// breaks, or a sandbox that has gone, is marked so gate 2 reads it as
// unverified.
//
// @decision(DL-051) @decision(DL-063) @decision(DL-050)
func (s *Supervisor) collectOut(ctx context.Context, job store.Job) error {
	if s.OpenOut == nil || job.Kind != store.JobKindReport {
		return nil
	}
	events, err := s.jobEvents(ctx, job)
	if err != nil {
		return err
	}
	if spovNote(events, MarkerCollected) != "" {
		return nil
	}
	stored, err := s.Store.Jobs().JobFiles(ctx, job.OrgID, job.ID)
	if err != nil {
		return fmt.Errorf("supervisor: job %s: listing collected files: %w", job.ID, err)
	}
	b := newBudget(stored)
	out, err := s.OpenOut(ctx, job)
	if err != nil {
		why := err
		if errors.Is(err, ErrSandboxGone) {
			why = errors.New("the sandbox had already stopped, so its reports could not be read")
		} else {
			s.warn("supervisor: job %s: opening the out directory: %s", job.ID, redact.Error(err))
		}
		s.storedMembers(ctx, job, stored)
		s.markCollectionFailed(ctx, job, why)
		return nil
	}
	entries, err := out.List(ctx, "")
	if err != nil {
		s.warn("supervisor: job %s: listing the out directory: %s", job.ID, redact.Error(err))
		s.storedMembers(ctx, job, stored)
		s.markCollectionFailed(ctx, job, fmt.Errorf("listing %s: %w", opencode.OutDir, err))
		return nil
	}
	// Every report before any image, so images cannot crowd a report out
	// of the limits. A verified member is written last, with what its
	// images came to, since a verified member is never rewritten.
	reports := reportNames(entries)
	bodies := make([][]byte, len(reports))
	members := make([]store.Deliverable, len(reports))
	for i, name := range reports {
		bodies[i], members[i] = s.collectReport(ctx, job, out, b, name)
	}
	for i := range reports {
		if bodies[i] == nil {
			continue
		}
		if lost := s.collectImages(ctx, job, out, b, bodies[i]); len(lost) > 0 {
			why := make([]string, len(lost))
			shown := ShownImages(bodies[i])
			for j, l := range lost {
				why[j] = l.why
				// @decision(DL-051)
				if shown[l.path] {
					members[i].ImagesLost = true
				}
			}
			members[i].Detail = "not every linked image was collected: " + strings.Join(why, "; ")
		}
	}
	for _, m := range members {
		s.putMember(ctx, job, m)
	}
	if s.Mark != nil {
		if err := s.Mark(ctx, job, MarkerCollected, collectedNote(b)); err != nil {
			s.warn("supervisor: job %s: recording the collection: %s", job.ID, redact.Error(err))
		}
	}
	return nil
}

// collectedNote is the collection marker's text, which the transcript
// shows as a line of its own.
//
// @decision(DL-051)
func collectedNote(b *budget) string {
	reports := 0
	for p := range b.stored {
		if !strings.Contains(p, "/") && strings.HasSuffix(strings.ToLower(p), ".md") {
			reports++
		}
	}
	images := len(b.stored) - reports
	note := "Collected " + plural(reports, "report")
	if images > 0 {
		note += " and " + plural(images, "image")
	}
	return note + " from " + opencode.OutDir + "."
}

// storedMembers writes a member for each report an earlier pass stored,
// for a pass that cannot read the sandbox: a pass that broke off after
// storing a report's file still delivers it.
//
// @decision(DL-051)
func (s *Supervisor) storedMembers(ctx context.Context, job store.Job, stored []store.JobFile) {
	for _, f := range stored {
		if strings.Contains(f.Path, "/") || !strings.HasPrefix(f.ContentType, "text/markdown") {
			continue
		}
		full, err := s.Store.Jobs().JobFile(ctx, job.OrgID, job.ID, f.Path)
		if err != nil {
			continue
		}
		s.putMember(ctx, job, reportMember(job, f.Path, full.Body))
	}
}

// budget tracks what one job has delivered against the limits, starting
// from what an earlier pass stored.
//
// @decision(DL-051)
type budget struct {
	bytes  int64
	files  int
	stored map[string]bool
}

func newBudget(stored []store.JobFile) *budget {
	b := &budget{stored: map[string]bool{}}
	for _, f := range stored {
		b.bytes += f.Size
		b.files++
		b.stored[f.Path] = true
	}
	return b
}

// fits reports whether one more file of n bytes is within the limits.
func (b *budget) fits(n int64) bool {
	return b.files < MaxDeliveredFiles && b.bytes+n <= MaxDeliveredBytes
}

func (b *budget) add(p string, n int64) {
	b.bytes += n
	b.files++
	b.stored[p] = true
}

// reportNames is every report in a listing of the out directory's top:
// the plain Markdown files, dotfiles excluded, in name order.
//
// @decision(DL-051)
func reportNames(entries []OutEntry) []string {
	var out []string
	for _, e := range entries {
		name := strings.TrimPrefix(e.Path, "./")
		if e.Dir || strings.HasSuffix(name, "/") || strings.Contains(name, "/") {
			continue
		}
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(strings.ToLower(name), ".md") {
			continue
		}
		// A name an extractor could read as a path is not collected.
		//
		// @decision(DL-135)
		if !SafeEntryName(name) {
			continue
		}
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// SafeEntryName reports whether a collected file's path can be a zip
// entry and a stored path without meaning anything else on an extractor:
// valid UTF-8 of a bounded length; segments that are neither empty nor a
// dot segment; and no backslash, colon or control character.
//
// @decision(DL-135)
func SafeEntryName(name string) bool {
	if name == "" || len(name) > 255 || !utf8.ValidString(name) || !fs.ValidPath(name) || name == "." {
		return false
	}
	for _, r := range name {
		if r == '\\' || r == ':' || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// collectReport reads and stores one report, and returns its body (nil
// when it was not collected) and its member.
//
// @decision(DL-051)
func (s *Supervisor) collectReport(ctx context.Context, job store.Job, out OutDir, b *budget, name string) ([]byte, store.Deliverable) {
	member := reportMember(job, name, nil)
	if b.stored[name] {
		f, err := s.Store.Jobs().JobFile(ctx, job.OrgID, job.ID, name)
		if err != nil {
			member.Status, member.Detail = store.DeliverableUnverified, "the stored report could not be read back"
			return nil, member
		}
		return f.Body, reportMember(job, name, f.Body)
	}
	body, err := out.Read(ctx, name, MaxFileBytes)
	switch {
	case errors.Is(err, ErrFileTruncated):
		member.Status, member.Detail = store.DeliverableOverLimit, "larger than 10 MiB, so it was not collected"
		return nil, member
	case err != nil:
		s.warn("supervisor: job %s: reading %s: %s", job.ID, name, redact.Error(err))
		member.Status, member.Detail = store.DeliverableUnverified, "it could not be read: "+redact.Error(err)
		return nil, member
	}
	if !b.fits(int64(len(body))) {
		member.Status, member.Detail = store.DeliverableOverLimit,
			"not collected: the job's deliverables would pass 100 MiB or 1,000 files"
		return nil, member
	}
	if _, err := s.Store.Jobs().PutJobFile(ctx, job.OrgID, store.JobFile{
		JobID: job.ID, Path: name, ContentType: reportContentType, Size: int64(len(body)), Body: body,
	}); err != nil {
		s.warn("supervisor: job %s: storing %s: %s", job.ID, name, redact.Error(err))
		member.Status, member.Detail = store.DeliverableUnverified, "it could not be stored"
		return nil, member
	}
	b.add(name, int64(len(body)))
	return body, reportMember(job, name, body)
}

// reportMember is a report's member, verified when its body is given.
//
// @decision(DL-051)
func reportMember(job store.Job, name string, body []byte) store.Deliverable {
	d := store.Deliverable{
		JobID: job.ID, Key: store.ReportKey(name), Kind: store.DeliverableReport,
		Path: name, Title: reportTitle(body, name),
	}
	if body != nil {
		d.Status, d.Size = store.DeliverableVerified, int64(len(body))
	}
	return d
}

// headingLine is an ATX heading of level one.
var headingLine = regexp.MustCompile(`(?m)^ {0,3}# +(.+?)(?: +#+)? *\r?$`)

// reportTitle is a report's first `# ` heading, or its file name without
// `.md`.
//
// @decision(DL-051)
func reportTitle(body []byte, name string) string {
	if m := headingLine.FindSubmatch(body); m != nil {
		if t := strings.TrimSpace(string(m[1])); t != "" {
			return truncateRunes(t, 200)
		}
	}
	return strings.TrimSuffix(strings.TrimSuffix(name, ".md"), ".MD")
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// putMember writes one member, best-effort.
func (s *Supervisor) putMember(ctx context.Context, job store.Job, d store.Deliverable) {
	if _, err := s.Store.Jobs().PutDeliverable(ctx, job.OrgID, d); err != nil {
		s.warn("supervisor: job %s: recording %s: %s", job.ID, d.Key, redact.Error(err))
	}
}

// Link destinations in Markdown: an inline image, and a reference
// definition. Read generously — a match inside a code block only costs a
// read of a file that must still be an image under the out directory.
var (
	inlineImage   = regexp.MustCompile(`!\[[^\]]*\]\(\s*(<[^>]*>|[^\s)]+)`)
	referenceLink = regexp.MustCompile(`(?m)^ {0,3}\[[^\]]+\]:\s*(<[^>]*>|\S+)`)
)

// ImageLinks is every local path a report may show as an image,
// resolved against the out directory, deduplicated, in order.
//
// @decision(DL-051)
func ImageLinks(body []byte) []string {
	var out []string
	seen := map[string]bool{}
	for _, re := range []*regexp.Regexp{inlineImage, referenceLink} {
		for _, m := range re.FindAllSubmatch(body, -1) {
			p, ok := OutRelative(string(m[1]))
			if !ok || seen[p] {
				continue
			}
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

var (
	fenceLine     = regexp.MustCompile("^ {0,3}(```|~~~)")
	labelledLink  = regexp.MustCompile(`(?m)^ {0,3}\[([^\]]+)\]:\s*(<[^>]*>|\S+)`)
	imageRef      = regexp.MustCompile(`!\[([^\]]*)\]\[([^\]]*)\]`)
	imageShortcut = regexp.MustCompile(`!\[([^\]]+)\](?:[^\[(]|$)`)
)

// ShownImages is the part of ImageLinks a reader would see as an image:
// an inline image outside fenced code, or a reference definition that an
// image outside fenced code uses. Only losing one of these makes a report
// partial; the rest is collected when it can be.
//
// @decision(DL-051) @decision(DL-063)
func ShownImages(body []byte) map[string]bool {
	var prose []string
	fenced := false
	for _, line := range strings.Split(string(body), "\n") {
		if fenceLine.MatchString(line) {
			fenced = !fenced
			continue
		}
		if !fenced {
			prose = append(prose, line)
		}
	}
	text := []byte(strings.Join(prose, "\n"))
	shown := map[string]bool{}
	for _, m := range inlineImage.FindAllSubmatch(text, -1) {
		if p, ok := OutRelative(string(m[1])); ok {
			shown[p] = true
		}
	}
	used := map[string]bool{}
	for _, m := range imageRef.FindAllSubmatch(text, -1) {
		label := string(m[2])
		if label == "" {
			label = string(m[1])
		}
		used[strings.ToLower(strings.TrimSpace(label))] = true
	}
	for _, m := range imageShortcut.FindAllSubmatch(text, -1) {
		used[strings.ToLower(strings.TrimSpace(string(m[1])))] = true
	}
	for _, m := range labelledLink.FindAllSubmatch(text, -1) {
		if !used[strings.ToLower(strings.TrimSpace(string(m[1])))] {
			continue
		}
		if p, ok := OutRelative(string(m[2])); ok {
			shown[p] = true
		}
	}
	return shown
}

// OutRelative resolves a link destination written in a top-level report
// to a path under the out directory, refusing a URL, an absolute path,
// and anything that leaves the directory.
//
// @decision(DL-063)
func OutRelative(dest string) (string, bool) {
	dest = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(dest), "<"), ">")
	if dest == "" || strings.Contains(dest, ":") || strings.HasPrefix(dest, "/") ||
		strings.HasPrefix(dest, "\\") || strings.HasPrefix(dest, "#") {
		return "", false
	}
	if i := strings.IndexAny(dest, "?#"); i >= 0 {
		dest = dest[:i]
	}
	unescaped, err := url.PathUnescape(dest)
	if err != nil {
		return "", false
	}
	clean := path.Clean(unescaped)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") ||
		strings.ContainsAny(clean, "\\\x00") {
		return "", false
	}
	return clean, true
}

// imageTypes are the image formats a report may show, by their first
// bytes. SVG is not among them.
//
// @decision(DL-051)
var imageTypes = []struct {
	prefix [][]byte
	typ    string
}{
	{[][]byte{[]byte("\x89PNG\r\n\x1a\n")}, "image/png"},
	{[][]byte{{0xff, 0xd8, 0xff}}, "image/jpeg"},
	{[][]byte{[]byte("GIF87a"), []byte("GIF89a")}, "image/gif"},
}

// ImageType is the type a file's first bytes show, or "" when they are
// not a PNG, JPEG, GIF or WebP image.
//
// @decision(DL-051)
func ImageType(body []byte) string {
	for _, t := range imageTypes {
		for _, p := range t.prefix {
			if bytes.HasPrefix(body, p) {
				return t.typ
			}
		}
	}
	if len(body) >= 12 && bytes.Equal(body[:4], []byte("RIFF")) && bytes.Equal(body[8:12], []byte("WEBP")) {
		return "image/webp"
	}
	return ""
}

// lostImage is a linked image collection could not keep, and why, for a
// person.
type lostImage struct{ path, why string }

// collectImages collects the images one report links, and returns why
// each one that was not collected was dropped.
//
// @decision(DL-051)
func (s *Supervisor) collectImages(ctx context.Context, job store.Job, out OutDir, b *budget, report []byte) []lostImage {
	var dropped []lostImage
	for _, p := range ImageLinks(report) {
		if b.stored[p] || strings.HasSuffix(strings.ToLower(p), ".md") {
			continue
		}
		body, err := out.Read(ctx, p, MaxFileBytes)
		switch {
		case errors.Is(err, ErrNoFile):
			dropped = append(dropped, lostImage{p, p + " was not found"})
			continue
		case errors.Is(err, ErrFileTruncated):
			dropped = append(dropped, lostImage{p, p + " is larger than 10 MiB"})
			continue
		case err != nil:
			dropped = append(dropped, lostImage{p, p + " could not be read"})
			continue
		}
		typ := ImageType(body)
		if typ == "" {
			dropped = append(dropped, lostImage{p, p + " is not a PNG, JPEG, GIF or WebP image"})
			continue
		}
		if !b.fits(int64(len(body))) {
			dropped = append(dropped, lostImage{p, p + " would pass the job's 100 MiB or 1,000 files"})
			continue
		}
		if _, err := s.Store.Jobs().PutJobFile(ctx, job.OrgID, store.JobFile{
			JobID: job.ID, Path: p, ContentType: typ, Size: int64(len(body)), Body: body,
		}); err != nil {
			dropped = append(dropped, lostImage{p, p + " could not be stored"})
			continue
		}
		b.add(p, int64(len(body)))
	}
	return dropped
}

// recordedPR is one deliverable line the pull request tool wrote.
//
// @decision(DL-051)
type recordedPR struct {
	Event      string `json:"event"`
	Platform   string `json:"platform"`
	Repository string `json:"repository"`
	Branch     string `json:"branch"`
	// Withdrawn takes back an earlier record of the same branch (DL-051).
	Withdrawn bool `json:"withdrawn"`
	// At is the plugin's ISO time; anything else reads as no time.
	At json.RawMessage `json:"at"`
}

// evidenceLines calls fn with every line of spov's stored evidence,
// never with anything the sandbox wrote elsewhere.
//
// @decision(DL-050) @decision(DL-018)
func evidenceLines(events []store.Event, fn func(line []byte)) {
	for _, e := range events {
		if !isRecordMarker(e) {
			continue
		}
		for _, line := range strings.Split(e.Text, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				fn([]byte(line))
			}
		}
	}
}

// recordedPullRequests is every pull request the agent recorded that
// names one of the session's repositories, deduplicated, in order. A
// line naming any other repository is dropped: the evidence file is the
// agent's to write.
//
// forges maps each platform to its kind, from the session's recorded
// credentials, so a member says whether it is a merge request.
//
// A member the agent last withdrew is withdrawn when a person wrote to
// the job after it was first recorded (withdrawalHonoured), and recorded
// as it was otherwise.
//
// @decision(DL-051) @decision(DL-041)
func recordedPullRequests(events []store.Event, repos []store.RepoRef, forges map[string]string) []store.Deliverable {
	var out []store.Deliverable
	for _, r := range pullRequestRecords(events, repos) {
		d := store.Deliverable{
			Key: r.key, Kind: store.DeliverablePR, Status: store.DeliverableUnverified,
			Detail:   "not checked on the forge yet",
			Platform: r.platform, Forge: forges[r.platform], Owner: r.owner, Name: r.name, Branch: r.branch,
		}
		if r.withdrawn && withdrawalHonoured(events, r.first) {
			d.Status, d.Detail = store.DeliverableWithdrawn, "withdrawn at the reader's request"
		}
		out = append(out, d)
	}
	return out
}

// pullRequestRecord is one recorded branch: when it was first recorded,
// and whether the agent's last word on it was a withdrawal.
type pullRequestRecord struct {
	key, platform, owner, name, branch string
	first                              time.Time
	withdrawn                          bool
}

// pullRequestRecords reads the record lines for the session's
// repositories, in the order each branch was first recorded.
//
// @decision(DL-051)
func pullRequestRecords(events []store.Event, repos []store.RepoRef) []pullRequestRecord {
	var out []pullRequestRecord
	index := map[string]int{}
	recordLines(events, func(line []byte) {
		var r recordedPR
		if json.Unmarshal(line, &r) != nil || r.Event != opencode.DeliverableEvent {
			return
		}
		// A GitLab project's owner is its whole group path, so the
		// repository splits at its last slash. @decision(DL-041)
		owner, name, err := git.SplitPath(r.Repository)
		branch := strings.TrimPrefix(strings.TrimSpace(r.Branch), "origin/")
		if err != nil || branch == "" || len(branch) > 255 {
			return
		}
		if !slices.Contains(repos, store.RepoRef{Platform: r.Platform, Owner: owner, Name: name}) {
			return
		}
		key := store.PullRequestKey(r.Platform, owner, name, branch)
		i, seen := index[key]
		if !seen {
			// A withdrawal of a branch never recorded withdraws nothing.
			if r.Withdrawn {
				return
			}
			var stamp string
			_ = json.Unmarshal(r.At, &stamp)
			at, _ := time.Parse(time.RFC3339Nano, stamp)
			index[key] = len(out)
			out = append(out, pullRequestRecord{key: key, platform: r.Platform, owner: owner, name: name, branch: branch, first: at})
			return
		}
		out[i].withdrawn = r.Withdrawn
	})
	return out
}

// recordLines calls fn with each record line, in order: from the outcome
// marker when there is one, since it keeps every record line however
// long the run, and from the evidence otherwise. Reading both would let
// a truncated evidence file's older copy have the last word.
//
// @decision(DL-051)
func recordLines(events []store.Event, fn func(line []byte)) {
	var outcome []store.Event
	for _, e := range events {
		if e.Source == store.SourceSpov && e.Name == MarkerOutcome {
			outcome = append(outcome, e)
		}
	}
	if len(outcome) == 0 {
		evidenceLines(events, fn)
		return
	}
	evidenceLines(outcome, fn)
}

// withdrawalHonoured reports whether a person's message reached the job
// after a pull request first recorded at first: a user inbox item
// enqueued after it, by the frame's own time (the sandbox's clock, which
// the plugin's record time also reads). A record with no time honours
// nothing.
//
// @decision(DL-051)
func withdrawalHonoured(events []store.Event, first time.Time) bool {
	if first.IsZero() {
		return false
	}
	for _, e := range events {
		if e.Source != store.SourceOpenCode || e.Name != EventInboxEnqueued || InboxItemType(e.Raw) != "user" {
			continue
		}
		if e.At.After(first) {
			return true
		}
	}
	return false
}

// unaskedWithdrawals counts the pull requests the agent last withdrew
// that are stored as judged rather than withdrawn: the withdrawal was not
// honoured when they were recorded, whatever the events say now.
//
// @decision(DL-051)
func unaskedWithdrawals(events []store.Event, members []store.Deliverable) int {
	stored := map[string]bool{}
	repos := make([]store.RepoRef, 0, len(members))
	for _, m := range members {
		if m.Kind == store.DeliverablePR && m.Status != store.DeliverableWithdrawn {
			stored[m.Key] = true
			repos = append(repos, store.RepoRef{Platform: m.Platform, Owner: m.Owner, Name: m.Name})
		}
	}
	n := 0
	for _, r := range pullRequestRecords(events, repos) {
		if r.withdrawn && stored[r.key] {
			n++
		}
	}
	return n
}

// declaredUnfinished is what the agent last said it left undone.
//
// @decision(DL-050)
func declaredUnfinished(events []store.Event) string {
	unfinished := ""
	evidenceLines(events, func(line []byte) {
		var c struct {
			Event      string `json:"event"`
			Unfinished string `json:"unfinished"`
		}
		if json.Unmarshal(line, &c) == nil && c.Event == declaredCompletionName {
			unfinished = strings.TrimSpace(c.Unfinished)
		}
	})
	return truncateRunes(unfinished, 4000)
}

// recordDeclarations writes what the agent recorded and declared onto the
// job: each pull request as a member, and its unfinished work on the row.
// It reads stored events only, so every delivery pass may run it. A
// member already stored is not overwritten here.
//
// @decision(DL-051) @decision(DL-050)
func (s *Supervisor) recordDeclarations(ctx context.Context, job store.Job) {
	events, err := s.jobEvents(ctx, job)
	if err != nil {
		s.warn("supervisor: job %s: reading the declarations: %s", job.ID, redact.Error(err))
		return
	}
	// A withdrawal is honoured by a person's message in the job's own
	// conversation only. @decision(DL-114)
	own, err := s.ownConversation(ctx, job)
	if err != nil {
		s.warn("supervisor: job %s: %s", job.ID, redact.Error(err))
		return
	}
	events = withoutOthersInbox(events, own)
	if job.Kind == store.JobKindPR {
		sess, err := s.Store.Sessions().Get(ctx, job.OrgID, job.SessionID)
		if err != nil {
			s.warn("supervisor: job %s: reading its session: %s", job.ID, redact.Error(err))
		} else {
			have, err := s.Store.Jobs().Deliverables(ctx, job.OrgID, []string{job.ID})
			if err != nil {
				s.warn("supervisor: job %s: reading its deliverables: %s", job.ID, redact.Error(err))
			}
			forges := map[string]string{}
			for _, c := range sess.Credentials {
				forges[c.Platform] = c.Forge
			}
			for _, d := range recordedPullRequests(events, sess.Request.Repos, forges) {
				if slices.ContainsFunc(have[job.ID], func(h store.Deliverable) bool { return h.Key == d.Key }) {
					continue
				}
				d.JobID = job.ID
				s.putMember(ctx, job, d)
			}
		}
	}
	unfinished := declaredUnfinished(events)
	if unfinished == "" {
		return
	}
	if err := s.Store.Tx(ctx, func(tx store.Store) error {
		current, err := tx.Jobs().Get(ctx, job.OrgID, job.ID)
		if err != nil || current.Unfinished == unfinished || isTerminalJob(current.State) {
			return err
		}
		current.Unfinished = unfinished
		return tx.Jobs().Update(ctx, current)
	}); err != nil {
		s.warn("supervisor: job %s: recording the unfinished work: %s", job.ID, redact.Error(err))
	}
}

// checkPullRequests asks each pull request member's forge about it,
// except one already verified. An error leaves the member unverified,
// never missing: a broken query is spov's failure, not the job's. It
// reads the forge, not the sandbox, so every delivery pass may run it.
//
// @decision(DL-051) @decision(DL-050)
func (s *Supervisor) checkPullRequests(ctx context.Context, job store.Job) {
	if job.Kind != store.JobKindPR {
		return
	}
	have, err := s.Store.Jobs().Deliverables(ctx, job.OrgID, []string{job.ID})
	if err != nil {
		s.warn("supervisor: job %s: reading its deliverables: %s", job.ID, redact.Error(err))
		return
	}
	for _, d := range have[job.ID] {
		// A withdrawn one is not part of the set; the forge is not asked.
		//
		// @decision(DL-051)
		if d.Kind != store.DeliverablePR || d.Status == store.DeliverableVerified || d.Status == store.DeliverableWithdrawn {
			continue
		}
		s.putMember(ctx, job, s.checkPullRequest(ctx, job, d))
	}
}

// forgeClockSkew is how far a forge's clock may trail spov's when gate 2
// asks whether a pull request was opened during the job.
//
// @decision(DL-051)
const forgeClockSkew = time.Minute

// checkPullRequest is one member as the forge answered for it.
//
// @decision(DL-051) @decision(DL-050)
func (s *Supervisor) checkPullRequest(ctx context.Context, job store.Job, d store.Deliverable) store.Deliverable {
	slug := d.Owner + "/" + d.Name
	if s.FindPullRequest == nil {
		d.Status, d.Detail = store.DeliverableUnverified, "spov cannot ask this forge about pull requests"
		return d
	}
	pr, err := s.FindPullRequest(ctx, job, d)
	switch {
	case errors.Is(err, git.ErrUnsupported):
		d.Status, d.Detail = store.DeliverableUnverified, "spov cannot ask this forge about pull requests"
	case err != nil:
		s.warn("supervisor: job %s: looking for its pull request in %s: %s", job.ID, slug, redact.Error(err))
		d.Status, d.Detail = store.DeliverableUnverified, "spov could not ask the forge about it"
	// A merged or closed one opened before this job is an earlier job's
	// on a reused branch name, not this one's. @decision(DL-051)
	case pr != nil && !pr.Open && !pr.Created.IsZero() && pr.Created.Before(job.CreatedAt.Add(-forgeClockSkew)):
		d.Status, d.Detail = store.DeliverableMissing, "no "+git.ChangeNoun(d.Forge)+" from "+d.Branch+
			" was opened during the job"
	case pr == nil:
		// @decision(DL-051)
		d.Status, d.Detail = store.DeliverableMissing, "no "+git.ChangeNoun(d.Forge)+" comes from "+d.Branch
	// @decision(DL-051)
	case !pr.Open && !pr.Merged:
		d.Status, d.Detail = store.DeliverableMissing, "closed without merging"
		d.Number, d.URL, d.Title = pr.Number, pr.URL, truncateRunes(pr.Title, 200)
	default:
		d.Status, d.Detail = store.DeliverableVerified, ""
		if pr.Merged {
			d.Detail = "merged"
		}
		d.Number, d.URL, d.Title = pr.Number, pr.URL, truncateRunes(pr.Title, 200)
	}
	return d
}
