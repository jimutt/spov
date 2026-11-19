//go:build integration

package server

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/opencode/contract"
)

// TestIntegrationLocalOpenCodeProbeModelList reads the pinned build's
// model list the way the recorder does, with placeholder keys for two
// providers and no network call made with them: every model is in the
// document's Model.Info shape, the list is within MaxModelListBytes, and
// some models keep variants. It runs with the local probe:
//
//	SPOV_PROBE=1 go test -tags integration ./internal/server/ -run LocalOpenCodeProbe -v
//
// @decision(DL-159) @decision(DL-035)
func TestIntegrationLocalOpenCodeProbeModelList(t *testing.T) {
	if os.Getenv("SPOV_PROBE") == "" && os.Getenv("SPOV_PROBE_OPENCODE") == "" {
		t.Skip("set SPOV_PROBE=1 to run the local OpenCode probe")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	bin := probeBinary(ctx, t)
	model := newFakeModel(t)
	conn := startProbeServerWith(ctx, t, bin, model.URL, t.TempDir(), "",
		"ANTHROPIC_API_KEY=probe-placeholder", "OPENAI_API_KEY=probe-placeholder")
	if _, err := opencode.NewClient(conn.URL, conn.Password).WaitHealthy(ctx, 100*time.Millisecond); err != nil {
		t.Fatalf("the server never answered as healthy: %v", err)
	}
	contract.CheckRequest(t, "GET", modelListPath, nil)
	body, err := sandboxGetBounded(ctx, conn.URL+modelListPath, conn.Token, conn.User, conn.Password, MaxModelListBytes)
	if err != nil {
		t.Fatalf("reading the model list: %v", err)
	}
	doc, err := contract.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.CheckSchema("Location.PublicRef", locationOf(t, body)); err != nil {
		t.Errorf("the list's location: %v", err)
	}
	var list struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("the model list is not JSON: %v", err)
	}
	refused := 0
	for _, m := range list.Data {
		if err := doc.CheckSchema("Model.Info", m); err != nil {
			if refused++; refused <= 5 {
				t.Errorf("%v", err)
			}
		}
	}
	models, err := parseModelVariants(body)
	if err != nil {
		t.Fatalf("the recorder cannot read the pinned build's model list: %v", err)
	}
	t.Logf("%d models listed in %d bytes; %d keep variants", len(list.Data), len(body), len(models))
	if len(models) == 0 {
		t.Errorf("no model of %d has variants the recorder keeps", len(list.Data))
	}
}

// locationOf is the model list's location object.
func locationOf(t *testing.T, body []byte) []byte {
	t.Helper()
	var v struct {
		Location json.RawMessage `json:"location"`
	}
	if err := json.Unmarshal(body, &v); err != nil || len(v.Location) == 0 {
		t.Fatalf("the model list has no location: %s", body[:min(len(body), 200)])
	}
	return v.Location
}
