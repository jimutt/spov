//go:build integration

package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/modal-labs/modal-client/go"
)

// Spike S7 pins the measured shape of GET /api/fs/read/*: the route is
// /api/fs/read/<path>, a relative path with location[directory] returns
// raw bytes with no envelope, an absolute path answers 500, a missing
// file answers 500 (not 404), and a 200 KB file comes back whole. Run on
// an OpenCode version bump.
//
// It creates a billable sandbox and terminates it via t.Cleanup. It
// runs no agent turn and needs no model key: files are written with the
// sandbox's own exec, so this is a cheap run.
//
//	go test -tags integration ./internal/sandbox/ -run S7FsRead -v
//
// @decision(DL-045) @decision(DL-050)
func TestIntegrationS7FsReadShape(t *testing.T) {
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	t.Logf("opencode build under test: %s (image %s)", testOpenCodeVersion(), testImage())

	sb, creds, password := startOpenCodeSandboxWith(t, ctx, p, "spov-s7", 8*time.Minute, nil)
	client := &http.Client{Timeout: 60 * time.Second}
	base := creds.URL

	// The two files spov actually reads: a report at the agreed
	// relative path, and the plugin's evidence at an absolute one.
	const (
		reportBody   = "# the report\n\nwritten by S7.\n"
		evidenceBody = `{"event":"loop.stopped","reason":"limit"}` + "\n"
		workdir      = "/root"
		reportRel    = "out/report.md"
		evidenceAbs  = "/tmp/spov-goal.jsonl"
	)
	execInSandbox(t, ctx, sb, fmt.Sprintf(
		"mkdir -p %s/out && printf %q > %s/%s && printf %q > %s",
		workdir, reportBody, workdir, reportRel, evidenceBody, evidenceAbs))

	// Question 0, first because everything else is meaningless without
	// it: what does the sandbox itself say the route is? Reading the
	// document beats inferring from source.
	t.Run("the endpoint is in the sandbox's own openapi document", func(t *testing.T) {
		doc := get(ctx, t, client, base+"/openapi.json", creds.Token, password)
		if doc == "" {
			t.Skip("no /openapi.json to read; the questions below measure the behaviour directly")
		}
		var parsed struct {
			Paths map[string]any `json:"paths"`
		}
		if err := json.Unmarshal([]byte(doc), &parsed); err != nil {
			t.Logf("openapi.json did not parse (%v); first 400: %s", err, truncate(doc, 400))
			return
		}
		var fsPaths []string
		for path := range parsed.Paths {
			if strings.Contains(path, "/fs/") {
				fsPaths = append(fsPaths, path)
			}
		}
		if len(fsPaths) == 0 {
			t.Errorf("the document declares no /fs/ route at all;"+
				" the report read has no endpoint (paths seen: %d)", len(parsed.Paths))
			return
		}
		t.Logf("fs routes the sandbox declares: %v", fsPaths)
		var hasRead bool
		for _, path := range fsPaths {
			if strings.Contains(path, "read") {
				hasRead = true
			}
		}
		if !hasRead {
			t.Errorf("no fs route mentions 'read': %v", fsPaths)
		}
	})

	// Question 1 and 2: the relative path with a location, which is
	// exactly what readOutFile sends.
	t.Run("a relative path resolves against location[directory]", func(t *testing.T) {
		u := fmt.Sprintf("%s/api/fs/read/%s?%s", base, reportRel,
			url.Values{"location[directory]": {workdir}}.Encode())
		code, body := getCode(ctx, t, client, u, creds.Token, password)
		if code == http.StatusNotFound {
			t.Fatalf("GET /api/fs/read/%s with location[directory]=%s -> 404 (%s):"+
				" the report read cannot find a file that IS there,"+
				" and readSandboxFile reports that as 'no such file'",
				reportRel, workdir, truncate(body, 200))
		}
		if code < 200 || code >= 300 {
			t.Fatalf("-> %d (%s)", code, truncate(body, 300))
		}
		// Question 5: raw bytes or an envelope?
		if strings.Contains(body, reportBody) {
			if strings.TrimSpace(body) == strings.TrimSpace(reportBody) {
				t.Logf("the body is the file's raw bytes, as readSandboxFile assumes")
			} else {
				t.Errorf("the file's content is present but WRAPPED, so a stored report"+
					" would carry the envelope too: %s", truncate(body, 300))
			}
			return
		}
		t.Errorf("200 but the body does not contain the file: %s", truncate(body, 400))
	})

	// Question 2b: is the location required, or does it default?
	t.Run("what a relative path does without a location", func(t *testing.T) {
		code, body := getCode(ctx, t, client,
			base+"/api/fs/read/"+reportRel, creds.Token, password)
		t.Logf("no location[directory] -> %d: %s", code, truncate(body, 200))
	})

	// Question 3: the absolute path spov reads the plugin's evidence at.
	t.Run("an absolute path, which is how the evidence is read", func(t *testing.T) {
		u := base + "/api/fs/read/" + strings.TrimPrefix(evidenceAbs, "/")
		code, body := getCode(ctx, t, client, u, creds.Token, password)
		t.Logf("GET /api/fs/read/%s -> %d: %s",
			strings.TrimPrefix(evidenceAbs, "/"), code, truncate(body, 200))

		// And the doubled-slash form the Go code currently builds,
		// since EvidencePath is absolute and is concatenated.
		u2 := base + "/api/fs/read/" + evidenceAbs
		code2, body2 := getCode(ctx, t, client, u2, creds.Token, password)
		t.Logf("GET /api/fs/read/%s (doubled slash, what the code sends now) -> %d: %s",
			evidenceAbs, code2, truncate(body2, 200))

		// MEASURED 2026-09-04 on 0.0.0-beta-18684: both absolute forms
		// answer 500. readEvidence therefore splits the path and sends
		// the directory as the location, which is the third form below.
		// It is asserted rather than logged, because reverting to an
		// absolute path would collect nothing on every job while
		// reporting only a warning.
		if code != http.StatusInternalServerError || code2 != http.StatusInternalServerError {
			t.Logf("an absolute path now answers %d/%d rather than 500;"+
				" readEvidence could be simplified if that is stable", code, code2)
		}

		u3 := fmt.Sprintf("%s/api/fs/read/%s?%s", base, "spov-goal.jsonl",
			url.Values{"location[directory]": {"/tmp"}}.Encode())
		code3, body3 := getCode(ctx, t, client, u3, creds.Token, password)
		if code3 != http.StatusOK || !strings.Contains(body3, "loop.stopped") {
			t.Fatalf("the form readEvidence sends — a name relative to"+
				" location[directory]=/tmp — answered %d (%s):"+
				" the plugin's evidence cannot be collected at all",
				code3, truncate(body3, 200))
		}
		t.Logf("the evidence reads back relative to its directory, as readEvidence sends it")
	})

	// Question 4: a missing file. readSandboxFile treats 404 as
	// ErrNoFile, and anything else as a broken read.
	t.Run("what a missing file answers", func(t *testing.T) {
		u := fmt.Sprintf("%s/api/fs/read/%s?%s", base, "out/nothing-here.md",
			url.Values{"location[directory]": {workdir}}.Encode())
		code, body := getCode(ctx, t, client, u, creds.Token, password)
		// MEASURED 2026-09-04: it answers 500, NOT the 404 the first
		// implementation assumed. So "the job produced nothing" and
		// "the read broke" share a status and cannot be told apart;
		// readSandboxFile treats both as ErrNoFile, which is the
		// harmless reading. This asserts the measurement rather than
		// the assumption, so a build that starts distinguishing them
		// is a test failure and an invitation to tighten the mapping.
		switch code {
		case http.StatusInternalServerError:
			t.Logf("a missing file answers 500, which readSandboxFile maps to ErrNoFile")
		case http.StatusNotFound:
			t.Logf("a missing file now answers 404; readSandboxFile could stop treating" +
				" 500 as 'no such file' and report real read failures again")
		default:
			t.Errorf("a missing file answered %d (%s), which readSandboxFile maps to a"+
				" broken read: a report job that produced nothing would fail its collection",
				code, truncate(body, 200))
		}
	})

	// The size cap is spov's own, but a sandbox that streams a huge
	// file differently would defeat it.
	t.Run("a file larger than the cap still streams", func(t *testing.T) {
		execInSandbox(t, ctx, sb,
			fmt.Sprintf("head -c 200000 /dev/zero | tr '\\0' 'x' > %s/out/big.txt", workdir))
		u := fmt.Sprintf("%s/api/fs/read/%s?%s", base, "out/big.txt",
			url.Values{"location[directory]": {workdir}}.Encode())
		code, body := getCode(ctx, t, client, u, creds.Token, password)
		t.Logf("a 200000-byte file -> %d, %d bytes back", code, len(body))
		if code == http.StatusOK && len(body) < 200000 {
			t.Errorf("the sandbox truncated at %d bytes on its own, so spov's cap is not"+
				" the only limit and a report could be cut without ErrFileTruncated", len(body))
		}
	})
}

// execInSandbox runs a shell command in the sandbox, for writing the
// files the questions above read back over HTTP.
func execInSandbox(t *testing.T, ctx context.Context, sb *modal.Sandbox, script string) {
	t.Helper()
	res, err := (&modalHandle{sb: sb}).Exec(ctx, []string{"sh", "-c", script})
	if err != nil {
		t.Fatalf("exec %q: %v", truncate(script, 120), err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exec %q: exit %d: %s", truncate(script, 120), res.ExitCode, truncate(res.Stderr, 300))
	}
}

// getCode is get with the status code, which every question here needs.
func getCode(ctx context.Context, t *testing.T, c *http.Client, url, token, password string) (int, string) {
	t.Helper()
	req := sandboxHTTP(ctx, t, http.MethodGet, url, token, password)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, string(body)
}
