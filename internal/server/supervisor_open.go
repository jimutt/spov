package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	gopath "path"
	"strings"
	"time"

	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/proxy"
	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/store"
)

// This file is the supervisor's own sandbox I/O, deliberately not in
// sandboxio.go, which is the recorder's alone. Issuing a job's opening
// turn is lifecycle — the same category as creating and terminating the
// sandbox — and happens once per job rather than once per viewer.
//
// @decision(DL-018) @decision(DL-046)

// OpenTimeout bounds the opening request. A sandbox that will not accept
// its first turn is not one to wait on.
//
// @decision(DL-046)
const OpenTimeout = 30 * time.Second

// oneLine collapses any run of whitespace to a single space.
// internal/jobs has its own copy, for protecting the gate line format.
//
// @decision(DL-047) @decision(DL-046)
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// openingText is the text a job's session is opened with. The directory
// a report job must write to is not added here — it travels on the
// plugin's re-asserted goal instead, so it survives compaction. With no
// assessment this is the goal verbatim. With one, the original comes
// first and is labelled as the request; the assessment follows,
// labelled as automated and as possibly wrong, in plain prose rather
// than a delimiter it could close.
//
// @decision(DL-045) @decision(DL-047)
func openingText(job store.Job) string {
	// Flattened here as well as at the parser: this is what puts the
	// text in front of the agent, so it must not depend on the only
	// current writer of that column having flattened it.
	summary := oneLine(job.Assessment)
	if summary == "" {
		return job.Goal
	}
	return "This is the request, exactly as it was given:\n\n" + job.Goal +
		"\n\nAn automated pass read that request before you were started and " +
		"summarised it as follows. It is spov's reading, not the requester's " +
		"words, and it may be wrong or incomplete. Where the two differ, the " +
		"request above is what was asked for.\n\n" + summary
}

// openJob issues a job's first turn as a synthetic, never a prompt.
//
// @decision(DL-045) @decision(DL-046)
func openJob(ctx context.Context, conn Connection, conversationID string, job store.Job) error {
	body, err := json.Marshal(map[string]any{
		"text": openingText(job),
		// resume schedules execution. Without it the item is enqueued
		// and nothing runs.
		"resume": true,
	})
	if err != nil {
		return fmt.Errorf("supervisor: encoding the opening goal: %w", err)
	}
	// @decision(DL-019)
	return sandboxPost(ctx, conn, conn.URL+"/api/session/"+url.PathEscape(conversationID)+"/synthetic", body)
}

// OutDir is where a `report` job leaves its reports and the images they
// link: opencode's constant, aliased here for the collection path. It is
// the workspace's, never sessionDir and never the landing.
//
// @decision(DL-037) @decision(DL-051)
const OutDir = opencode.OutDir

// MaxListBytes bounds a listing of the out directory read back.
//
// @decision(DL-051)
const MaxListBytes = 1 << 20

// readOutFile reads one file under OutDir. It is a lifecycle read, not a
// conversation read, made at the end against a sandbox about to be
// terminated, on no path a browser or a listing can reach. Each segment
// is escaped, so a name cannot add a query or climb out of the path.
//
// @decision(DL-045) @decision(DL-018) @decision(DL-051) @decision(DL-037)
func readOutFile(ctx context.Context, conn Connection, rel string, max int64) ([]byte, error) {
	segs := strings.Split(rel, "/")
	for i, seg := range segs {
		if seg == "" || seg == "." || seg == ".." {
			return nil, jobs.ErrNoFile
		}
		segs[i] = url.PathEscape(seg)
	}
	return readSandboxFile(ctx, conn, strings.Join(segs, "/"), OutDir, int(max))
}

// listOutDir lists one directory under OutDir ("" for the top) without
// descending. Measured on 2.0.22 (spike S-D1): entry paths are relative
// to the location, a directory's ends in "/", and a symlink is not
// listed. The directory exists from the sandbox's start, so any status
// but 200 is a read that broke.
//
// @decision(DL-051) @decision(DL-018)
func listOutDir(ctx context.Context, conn Connection, dir string) ([]jobs.OutEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, OpenTimeout)
	defer cancel()
	loc := OutDir
	if dir != "" {
		loc = gopath.Join(OutDir, dir)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, conn.URL+"/api/fs/list", nil)
	if err != nil {
		return nil, err
	}
	req.URL.RawQuery = url.Values{"location[directory]": {loc}}.Encode()
	req.AddCookie(&http.Cookie{Name: proxy.TokenCookie, Value: conn.Token})
	req.SetBasicAuth(conn.User, conn.Password)
	client := &http.Client{
		Timeout:       OpenTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list in the sandbox: %s", redact.Error(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, sandboxStatus(actReadFile, resp.StatusCode, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxListBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading the listing: %s", redact.Error(err))
	}
	if len(body) > MaxListBytes {
		return nil, fmt.Errorf("the listing of %s is larger than %d bytes", loc, MaxListBytes)
	}
	var listed struct {
		Data []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &listed); err != nil {
		return nil, fmt.Errorf("the listing of %s did not parse: %w", loc, err)
	}
	out := make([]jobs.OutEntry, 0, len(listed.Data))
	for _, e := range listed.Data {
		out = append(out, jobs.OutEntry{Path: e.Path, Dir: e.Type == "directory"})
	}
	return out, nil
}

// readSandboxFile is the one request both reads make.
//
// @decision(DL-045) @decision(DL-018)
func readSandboxFile(ctx context.Context, conn Connection, path, workdir string, max int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, OpenTimeout)
	defer cancel()

	u := fmt.Sprintf("%s/api/fs/read/%s", conn.URL, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if workdir != "" {
		q := req.URL.Query()
		q.Set("location[directory]", workdir)
		req.URL.RawQuery = q.Encode()
	}
	req.AddCookie(&http.Cookie{Name: proxy.TokenCookie, Value: conn.Token})
	req.SetBasicAuth(conn.User, conn.Password)
	// Refused for the same reason the write above refuses them: a
	// redirect is the one way this credential could be carried to a
	// host it was not minted for.
	client := &http.Client{
		Timeout:       OpenTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("read from the sandbox: %s", redact.Error(err))
	}
	defer func() { _ = resp.Body.Close() }()
	// A missing file answers 500, not 404, so both are reported as
	// jobs.ErrNoFile, the harmless reading. 404 is accepted too, so
	// a build that starts answering the conventional status keeps
	// working.
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusInternalServerError {
		return nil, jobs.ErrNoFile
	}
	if resp.StatusCode != http.StatusOK {
		return nil, sandboxStatus(actReadFile, resp.StatusCode, resp.Status)
	}
	// Read one byte past the cap, so a file exactly at the limit is not
	// reported as truncated and one over it is.
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(max)+1))
	if err != nil {
		return nil, fmt.Errorf("reading the file: %s", redact.Error(err))
	}
	if len(body) > max {
		return body[:max], jobs.ErrFileTruncated
	}
	return body, nil
}

// EvidencePath is where the plugin records what it saw, including the
// loop.stopped line that says why the run ended. It mirrors
// opencode.PluginEvidencePath.
//
// @decision(DL-045) @decision(DL-039)
const EvidencePath = opencode.PluginEvidencePath

// OutcomePath is the plugin's outcome file, read with the evidence's bound.
//
// @decision(DL-051)
const OutcomePath = opencode.PluginOutcomePath

// MaxEvidenceBytes bounds the evidence read. The plugin writes one line
// per permission evaluation, so a busy job's file is much larger than
// its report; only the tail is wanted.
//
// @decision(DL-051)
const MaxEvidenceBytes = 256 << 10

// MaxCreatedSessionBytes caps the create-session answer, which is a
// small envelope carrying an id.
//
// @decision(DL-045)
const MaxCreatedSessionBytes = 64 << 10

// readEvidence reads the plugin's record out of a sandbox. The path is
// split into a directory and a name, and the directory is sent as the
// location: an absolute path answers 500.
//
// @decision(DL-045) @decision(DL-018)
func readEvidence(ctx context.Context, conn Connection, path string) ([]byte, error) {
	dir, name := gopath.Split(path)
	return readSandboxFile(ctx, conn, name, strings.TrimSuffix(dir, "/"), MaxEvidenceBytes)
}

// sessionDir is where a session's agent actually is. It reads
// store.Session.Landing, which is entrypoint.WorkingDir() as recorded
// at launch — the server's own cwd, and deliberately not
// Sandbox.Workdir, which the entrypoint's first act is to leave. An
// empty location is never sent in its place.
//
// @decision(DL-037)
func sessionDir(sess store.Session) string {
	if sess.Landing == "" {
		return opencode.WorkspaceRoot
	}
	return sess.Landing
}

// createConversation opens a conversation in a job's sandbox and
// returns its OpenCode id. A fresh sandbox has none, so the job
// contract creates its own rather than waiting for one that may never
// appear.
//
// @decision(DL-045) @decision(DL-046) @decision(DL-037)
func createConversation(ctx context.Context, conn Connection, workdir string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"location": map[string]any{"directory": workdir},
	})
	if err != nil {
		return "", fmt.Errorf("supervisor: encoding the session request: %w", err)
	}
	raw, err := sandboxPostJSON(ctx, conn, conn.URL+"/api/session", body)
	if err != nil {
		return "", err
	}
	// Wrapped in a `data` envelope, as the listing is.
	var out struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("supervisor: decoding the created session: %w", err)
	}
	if out.Data.ID == "" {
		// A body that parsed but carried no id is an unverified shape
		// before it is the sandbox's fault, exactly as frames.go says.
		return "", fmt.Errorf("supervisor: the sandbox created no session id")
	}
	return out.Data.ID, nil
}

// setConversationModel pins a conversation to one model. The config
// already carries it (DL-038), but a job needs this too, because a
// cloned repository's own opencode.json can outrank the global file. A
// failure is reported and not fatal, and not retried: the conversation
// is recorded whatever this returns, so a later pass takes the
// store-first path regardless.
//
// @decision(DL-014)
func setConversationModel(ctx context.Context, conn Connection, conversationID, model string) error {
	if model == "" {
		return nil
	}
	ref, ok := modelRef(model)
	if !ok {
		return fmt.Errorf("supervisor: %q is not a provider/model reference", model)
	}
	body, err := json.Marshal(map[string]any{"model": ref})
	if err != nil {
		return fmt.Errorf("supervisor: encoding the model request: %w", err)
	}
	// @decision(DL-019)
	return sandboxPost(ctx, conn, conn.URL+"/api/session/"+url.PathEscape(conversationID)+"/model", body)
}

// modelRef splits OpenCode's own provider/model[#variant] identifier
// into the Model.Ref object this endpoint takes ({id, providerID,
// variant?}), unlike the config file, which takes the plain string. A
// reference with no "/" is refused rather than sent.
//
// @decision(DL-014)
func modelRef(model string) (map[string]any, bool) {
	provider, rest, ok := strings.Cut(model, "/")
	if !ok || provider == "" || rest == "" {
		return nil, false
	}
	ref := map[string]any{"providerID": provider}
	if id, variant, hasVariant := strings.Cut(rest, "#"); hasVariant {
		if id == "" || variant == "" {
			return nil, false
		}
		ref["id"], ref["variant"] = id, variant
		return ref, true
	}
	ref["id"] = rest
	return ref, true
}

// sandboxPost makes one authenticated write to a sandbox, discarding
// the response body.
//
// @decision(DL-046)
func sandboxPost(ctx context.Context, conn Connection, url string, body []byte) error {
	_, err := sandboxPostJSON(ctx, conn, url, body)
	return err
}

// sandboxPostJSON is sandboxPost with the response body returned, for
// the one write whose answer is needed.
//
// @decision(DL-045) @decision(DL-046)
func sandboxPostJSON(ctx context.Context, conn Connection, url string, body []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, OpenTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: proxy.TokenCookie, Value: conn.Token})
	req.SetBasicAuth(conn.User, conn.Password)
	// Redirects are refused for the same reason the read path refuses
	// them: they are the one way this credential could be carried to a
	// host it was not minted for.
	client := &http.Client{
		Timeout:       OpenTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("write to the sandbox: %s", redact.Error(err))
	}
	defer func() { _ = resp.Body.Close() }()
	// A synthetic answers 200; 204 is what the command endpoint
	// answers, and either means accepted.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return nil, sandboxStatus(actWrite, resp.StatusCode, resp.Status)
	}
	// Capped for the same reason the reads are: the sandbox is the
	// untrusted side, and this body is only ever a small envelope.
	out, err := io.ReadAll(io.LimitReader(resp.Body, MaxCreatedSessionBytes))
	if err != nil {
		return nil, fmt.Errorf("reading the sandbox's answer: %s", redact.Error(err))
	}
	return out, nil
}

// LoopPause, LoopResume and LoopBound are the arguments the plugin's own
// command takes. They are matched by the JavaScript's switch, lowercased
// and trimmed, so the constants here are what that switch reads
// (TestTheLoopCommandsAreOnesThePluginHandles). Bound is the wall
// clock's final stop, announced as `wall-clock` rather than as the
// person's `stopped` (DL-049).
//
// @decision(DL-054) @decision(DL-039) @decision(DL-049)
const (
	LoopPause  = "pause"
	LoopResume = "resume"
	LoopBound  = "bound"
)

// sendLoopCommand drives the plugin's own /spov-goal command from the
// server, its documented HTTP surface rather than a new marker.
//
// The endpoint answers 204 and runs its handler asynchronously, so an
// accepted request means the sandbox took it and not that the plugin
// acted — a sandbox whose plugin never loaded answers exactly the same
// way.
//
// @decision(DL-054) @decision(DL-039) @decision(DL-018) @decision(DL-035)
func sendLoopCommand(ctx context.Context, conn Connection, conversationID, arg string) error {
	body, err := json.Marshal(map[string]any{
		"name": opencode.PluginCommand,
		"text": arg,
	})
	if err != nil {
		return fmt.Errorf("supervisor: encoding the loop command: %w", err)
	}
	// Escaped for the same reason as in steer.go: the sandbox chose this
	// id, and a `?` in it would truncate the path.
	//
	// @decision(DL-019)
	return sandboxPost(ctx, conn, conn.URL+"/api/session/"+url.PathEscape(conversationID)+"/command", body)
}
