//go:build integration

package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestIntegrationS11ModelPrecedence is spike S11. With a global config
// naming one model and a repository config naming another, it checks that
// config discovered at the repository outranks the global file, and that
// POST /api/session/:id/model is accepted and stored on the conversation.
// It does not show that a turn prefers the set model; that needs a real
// turn with a provider key.
//
// It creates a billable sandbox, terminates it via t.Cleanup, runs no
// agent turn and needs no model key. Read the logs as well as the result:
// the response shapes are inferred.
//
//	go test -tags integration ./internal/sandbox/ -run S11Model -v
//
// @decision(DL-014) @decision(DL-038)
func TestIntegrationS11ModelPrecedence(t *testing.T) {
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	t.Logf("opencode build under test: %s (image %s)", testOpenCodeVersion(), testImage())

	const (
		repoDir     = "/workspace/repo"
		repoModel   = "anthropic/claude-haiku-4-5-20251001"
		globalModel = "anthropic/claude-sonnet-5"
		setModel    = "anthropic/claude-opus-4-5"
	)
	// Seeded before boot rather than exec'd after it: the config is read
	// at startup, and a global directory created afterwards depends on
	// the live watcher noticing it.
	_, creds, password := startOpenCodeSandboxWith(t, ctx, p, "spov-s11", 8*time.Minute,
		map[string]string{
			"/root/.config/opencode/opencode.json": fmt.Sprintf(`{"model":%q}`, globalModel),
			repoDir + "/opencode.json":             fmt.Sprintf(`{"model":%q}`, repoModel),
		})
	client := &http.Client{Timeout: 60 * time.Second}
	base := creds.URL

	// Half 1: which configs does each directory discover? /api/config
	// returns the ordered entries, naming the files. /api/model cannot
	// answer this: it is empty in a sandbox with no provider keys.
	t.Run("a discovered repo config outranks the global one", func(t *testing.T) {
		inRepo := configAt(ctx, t, client, base, creds.Token, password, repoDir)
		atHome := configAt(ctx, t, client, base, creds.Token, password, "/root")

		repoSeen := strings.Contains(inRepo, repoDir+"/opencode.json")
		globalInRepo := strings.Contains(inRepo, globalModel)
		t.Logf("MEASURED at %s: repo config discovered=%v, global model present=%v",
			repoDir, repoSeen, globalInRepo)
		t.Logf("MEASURED at /root: repo config discovered=%v",
			strings.Contains(atHome, repoDir+"/opencode.json"))

		if !repoSeen {
			t.Errorf("the repository's own opencode.json was NOT discovered from %s: "+
				"DL-014 step 5's premise is wrong and the per-conversation set guards "+
				"nothing", repoDir)
			return
		}
		if strings.Contains(atHome, repoDir+"/opencode.json") {
			t.Error("the repository's config was discovered from /root, which is above it: " +
				"discovery is not the upward walk this rests on")
		}
		// Order is precedence: the LAST entry for a key wins, and the
		// global file is documented as ranking below discovered ones.
		_, globalID, _ := strings.Cut(globalModel, "/")
		_, repoID, _ := strings.Cut(repoModel, "/")
		gi, ri := strings.Index(inRepo, globalID), strings.Index(inRepo, repoID)
		if gi < 0 || ri < 0 {
			t.Fatalf("one of the seeded models is absent from the entries, so the order "+
				"check below would be vacuous (global=%d repo=%d)", gi, ri)
		}
		t.Logf("MEASURED entry order in %s: global at %d, repo at %d", repoDir, gi, ri)
		if gi >= 0 && ri >= 0 && ri < gi {
			t.Errorf("the global config is ordered AFTER the repository's, so it would win; "+
				"DL-014 step 5 has the precedence backwards (global=%d repo=%d)", gi, ri)
		}
	})

	// Half 2: is the call spov makes accepted, and does it stick?
	t.Run("the per-conversation set is accepted and recorded", func(t *testing.T) {
		// At /root, where no repository config is discovered, so only
		// the set itself is measured.
		convID := createSessionAt(ctx, t, client, base, creds.Token, password, "/root")
		t.Logf("conversation: %s", convID)

		// The endpoint takes a Model.Ref object {id, providerID,
		// variant?} and answers 400 to the string. Both forms are
		// tried so a build that accepts the string again is visible.
		provider, id, _ := strings.Cut(setModel, "/")
		shapes := []struct{ name, body string }{
			{"object", fmt.Sprintf(`{"model":{"providerID":%q,"id":%q}}`, provider, id)},
			{"string", fmt.Sprintf(`{"model":%q}`, setModel)},
		}
		var accepted string
		for _, shape := range shapes {
			status, raw := post(ctx, t, client,
				base+"/api/session/"+convID+"/model", creds.Token, password, shape.body)
			t.Logf("MEASURED %s form -> %d %s", shape.name, status, truncate(raw, 300))
			if status == http.StatusNotFound {
				t.Fatalf("the route does not exist on this build: DL-014 step 5 has no "+
					"mechanism, and OPENCODE_CONFIG_CONTENT is the fallback it names (%s)",
					truncate(raw, 200))
			}
			if status >= 200 && status <= 299 && accepted == "" {
				accepted = shape.name
			}
		}
		if accepted == "" {
			t.Fatalf("neither payload shape was accepted for %q; setConversationModel "+
				"cannot work as written", setModel)
		}
		t.Logf("MEASURED the accepted payload shape is the %s form", accepted)

		got := sessionModel(ctx, t, client, base, creds.Token, password, convID)
		t.Logf("MEASURED session model after the set: %q (sent %q)", got, setModel)
		if got == "" {
			t.Skip("the session reports no model of its own; the set was accepted but " +
				"nothing here reads it back, so persistence is unmeasured")
		}
		if !strings.Contains(got, "opus") {
			t.Errorf("the conversation did not keep the model it was set to: %q", got)
		}
	})
}

// configAt returns the raw ordered config entries a DIRECTORY
// discovers. The array is returned rather than a merged object, so the
// entries name the files themselves and their order is the precedence.
func configAt(ctx context.Context, t *testing.T, client *http.Client,
	base, token, password, dir string,
) string {
	t.Helper()
	raw := get(ctx, t, client,
		base+"/api/config?location[directory]="+dir, token, password)
	t.Logf("GET /api/config at %s -> %s", dir, truncate(raw, 1200))
	return raw
}

// firstModelIn pulls a provider/model out of whatever shape came back.
// Deliberately generous: this is the spike that measures the shape.
func firstModelIn(raw string) string {
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	body := []byte(raw)
	if err := json.Unmarshal(body, &envelope); err == nil && len(envelope.Data) > 0 {
		body = envelope.Data
	}
	var asObject map[string]any
	if err := json.Unmarshal(body, &asObject); err == nil {
		return modelOf(asObject)
	}
	var asList []map[string]any
	if err := json.Unmarshal(body, &asList); err == nil && len(asList) > 0 {
		return modelOf(asList[0])
	}
	return ""
}

func modelOf(m map[string]any) string {
	for _, key := range []string{"default", "model", "modelID", "model_id", "id"} {
		switch v := m[key].(type) {
		case string:
			if v != "" {
				return v
			}
		case map[string]any:
			if got := modelOf(v); got != "" {
				return got
			}
		}
	}
	provider, _ := m["providerID"].(string)
	model, _ := m["modelID"].(string)
	if model != "" {
		return provider + "/" + model
	}
	return ""
}

// createSessionAt opens a conversation at dir, exactly as the
// supervisor's createConversation does. The package's own createSession
// is fixed at /root, and the directory is the whole point here.
func createSessionAt(ctx context.Context, t *testing.T, client *http.Client,
	base, token, password, dir string,
) string {
	t.Helper()
	status, raw := post(ctx, t, client, base+"/api/session", token, password,
		fmt.Sprintf(`{"location":{"directory":%q}}`, dir))
	if status < 200 || status > 299 {
		t.Fatalf("creating a session answered %d: %s", status, truncate(raw, 400))
	}
	var out struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out.Data.ID == "" {
		t.Fatalf("no session id in %s", truncate(raw, 400))
	}
	return out.Data.ID
}

// sessionModel reads back whatever the session says its model is. This
// is the session's OWN field — what the create body or the model
// endpoint stored — not what config discovery resolves.
func sessionModel(ctx context.Context, t *testing.T, client *http.Client,
	base, token, password, id string,
) string {
	t.Helper()
	raw := get(ctx, t, client, base+"/api/session/"+id, token, password)
	t.Logf("GET /api/session/%s -> %s", id, truncate(raw, 600))
	return firstModelIn(raw)
}
