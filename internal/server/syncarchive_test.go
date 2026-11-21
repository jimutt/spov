package server

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/sandbox"
)

// archiveOf is a whole sync archive of one bundle.
func archiveOf(t *testing.T, head string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	add := func(name string, body []byte) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Size: int64(len(body)), Mode: 0o600, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(body)
	}
	meta, _ := json.Marshal(git.SyncMeta{Format: 1, Branch: "feat/sync", HeadCommit: head, Bundle: true})
	add("spov-sync.json", meta)
	add("state.bundle", []byte("BUNDLE"))
	add("spov-sync-end.json", []byte(`{"bundle_bytes":6,"worktree_bytes":0}`))
	_ = tw.Close()
	return buf.Bytes()
}

// TestASyncArchiveStreamsAndAFailureAfterItsFirstByteBreaksIt: the
// archive route answers the script's tar as it comes; a refusal before
// any byte is an ordinary error with the script's reason; a failure
// after the first byte cuts the response off, so the client can never
// read a short archive as a whole one.
//
// @decision(DL-176)
func TestASyncArchiveStreamsAndAFailureAfterItsFirstByteBreaksIt(t *testing.T) {
	f := newAPIFixture(t)
	head := strings.Repeat("c", 40)
	req := repoReq("streamed")
	req.Sync = &SessionSyncRequest{Branch: "feat/sync", BaseCommit: strings.Repeat("b", 40)}
	id := f.create(req)
	_ = f.events(id)

	var answer sandbox.ExecResult
	f.provider.ExecMatch = func(command []string) (sandbox.ExecResult, bool) {
		if slices.Contains(command, "archive") {
			return answer, true
		}
		return sandbox.ExecResult{}, false
	}
	path := "/api/v1/sessions/" + id + "/sync-archive"

	whole := archiveOf(t, head)
	answer = sandbox.ExecResult{Stdout: string(whole)}
	res, body := f.do(http.MethodPost, path, nil)
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "application/x-tar" || !bytes.Equal(body, whole) {
		t.Fatalf("archive = %d %q, %d bytes", res.StatusCode, res.Header.Get("Content-Type"), len(body))
	}
	if got, err := git.SpoolSyncArchive(bytes.NewReader(body), t.TempDir(), nil); err != nil || got.HeadCommit != head {
		t.Errorf("the answer does not spool: %+v, %v", got, err)
	}

	answer = sandbox.ExecResult{ExitCode: 3, Stderr: "This sync-back is too large: data.bin is 300 MiB"}
	res, body = f.do(http.MethodPost, path, nil)
	if res.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "data.bin is 300 MiB") ||
		!strings.HasPrefix(res.Header.Get("Content-Type"), "application/problem+json") {
		t.Errorf("a refusal = %d %s %s", res.StatusCode, res.Header.Get("Content-Type"), body)
	}

	answer = sandbox.ExecResult{ExitCode: 1, Stdout: string(whole[:700]), Stderr: "killed"}
	r, err := http.NewRequest(http.MethodPost, f.http.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+f.token)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || err == nil {
		t.Errorf("a failure after the first byte read %d bytes with status %d and no error", len(got), resp.StatusCode)
	}
	if _, err := git.SpoolSyncArchive(bytes.NewReader(got), t.TempDir(), nil); !errors.Is(err, git.ErrSyncCutShort) {
		t.Errorf("the cut answer spools as %v, want cut short", err)
	}
}
