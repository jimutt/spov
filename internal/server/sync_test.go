package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/store"
)

// @decision(DL-027)
func TestSessionSyncSeedAndExportEndpoint(t *testing.T) {
	f := newAPIFixture(t)
	base := strings.Repeat("b", 40)
	head := strings.Repeat("c", 40)
	bundle := base64.StdEncoding.EncodeToString([]byte("seed-bundle"))

	req := repoReq("synced")
	req.Sync = &SessionSyncRequest{
		Branch:     "feat/sync",
		BaseCommit: base,
		Bundle:     bundle,
	}
	id := f.create(req)
	_ = f.events(id)

	res, body := f.do(http.MethodGet, "/api/v1/sessions/"+id, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET session: %d %s", res.StatusCode, body)
	}
	var view SessionView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatal(err)
	}
	if view.Sync == nil || view.Sync.Branch != "feat/sync" || view.Sync.BaseCommit != base {
		t.Fatalf("view.Sync = %+v", view.Sync)
	}
	if strings.Contains(string(body), bundle) {
		t.Error("session view leaked the sync bundle bytes")
	}

	// Prime the export command key on the fake handle, then set canned output.
	_, _ = f.do(http.MethodPost, "/api/v1/sessions/"+id+"/sync-export", nil)
	h := f.provider.Handles()[view.SandboxID]
	execs := h.ExecLog()
	exportKey := strings.Join(execs[len(execs)-1], " ")
	outJSON, _ := json.Marshal(map[string]any{
		"branch":       "feat/sync",
		"base_commit":  base,
		"head_commit":  head,
		"bundle":       base64.StdEncoding.EncodeToString([]byte("exported-bundle")),
		"worktree_tar": base64.StdEncoding.EncodeToString([]byte("exported-tar")),
		"deleted":      []string{"gone.txt"},
	})
	f.provider.ExecResults[exportKey] = sandbox.ExecResult{ExitCode: 0, Stdout: string(outJSON)}

	res, body = f.do(http.MethodPost, "/api/v1/sessions/"+id+"/sync-export", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST sync-export: %d %s", res.StatusCode, body)
	}
	var exported SyncExportView
	if err := json.Unmarshal(body, &exported); err != nil {
		t.Fatal(err)
	}
	if exported.HeadCommit != head || exported.BaseCommit != base || exported.Branch != "feat/sync" {
		t.Errorf("exported = %+v", exported)
	}
	if len(exported.Deleted) != 1 || exported.Deleted[0] != "gone.txt" {
		t.Errorf("deleted = %v", exported.Deleted)
	}

	t.Run("validation refuses invalid sync requests", func(t *testing.T) {
		badMulti := SessionRequest{
			Name:  "multi",
			Repos: []RepoRef{{Platform: "fake", Owner: "acme", Name: "app"}, {Platform: "fake", Owner: "acme", Name: "two"}},
			Sync:  &SessionSyncRequest{Branch: "main", BaseCommit: base},
		}
		if r, _ := f.do(http.MethodPost, "/api/v1/sessions", badMulti); r.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("multi-repo sync status = %d, want 422", r.StatusCode)
		}
		badB64 := repoReq("badb64")
		badB64.Sync = &SessionSyncRequest{Branch: "main", BaseCommit: base, Bundle: "not valid base64!!!"}
		if r, _ := f.do(http.MethodPost, "/api/v1/sessions", badB64); r.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("bad base64 status = %d, want 422", r.StatusCode)
		}
	})

	t.Run("adopted session with Clones only still exports sync", func(t *testing.T) {
		adopted := store.Session{
			ID:        store.NewID(time.Now()),
			OrgID:     auth.LocalID,
			OwnerID:   auth.LocalID,
			Name:      "adopted-sync",
			State:     store.StateRunning,
			SandboxID: view.SandboxID,
			Request:   store.SessionRequest{Name: "adopted-sync"},
			Clones:    []string{"acme/app"},
		}
		if err := f.store.Sessions().Create(context.Background(), adopted); err != nil {
			t.Fatal(err)
		}
		_, _ = f.do(http.MethodPost, "/api/v1/sessions/adopted-sync/sync-export", nil)
		execs := h.ExecLog()
		adoptedKey := strings.Join(execs[len(execs)-1], " ")
		f.provider.ExecResults[adoptedKey] = sandbox.ExecResult{ExitCode: 0, Stdout: string(outJSON)}
		if r, b := f.do(http.MethodPost, "/api/v1/sessions/adopted-sync/sync-export", nil); r.StatusCode != http.StatusOK {
			t.Fatalf("adopted session export status = %d: %s", r.StatusCode, b)
		}
	})

	t.Run("export refuses job sessions and stopped sessions", func(t *testing.T) {
		job := store.Job{
			ID: store.NewID(time.Now()), OrgID: auth.LocalID, OwnerID: auth.LocalID,
			SessionID: id, Kind: store.JobKindPR, State: store.JobRunning, Goal: "g",
		}
		if err := f.store.Jobs().Create(context.Background(), job); err != nil {
			t.Fatal(err)
		}
		if r, _ := f.do(http.MethodPost, "/api/v1/sessions/"+id+"/sync-export", nil); r.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("job session export status = %d, want 422", r.StatusCode)
		}
	})
}
