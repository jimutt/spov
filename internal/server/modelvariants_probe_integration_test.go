//go:build integration

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/opencode/contract"
	"github.com/jimutt/spov/internal/prep"
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
	// From 2.4 the list is built a few seconds after the server starts;
	// read it as the recorder does (DL-183).
	var body []byte
	for wait := 0; ; wait++ {
		var err error
		body, err = sandboxGetBounded(ctx, conn.URL+modelListPath, conn.Token, conn.User, conn.Password, MaxModelListBytes)
		if err != nil {
			t.Fatalf("reading the model list: %v", err)
		}
		if _, err := parseModelVariants(body); !errors.Is(err, errNoModelsListed) || wait == ModelListWaits {
			t.Logf("the list was read %d times", wait+1)
			break
		}
		time.Sleep(ModelListPause)
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

// TestIntegrationWriteTheVariantSnapshot writes the pinned build's
// reasoning modes into internal/modelcat/variants.json: the binary run
// isolated, with a placeholder for every key variable spov forwards and
// models.dev's fetch off, so the list is the binary's own and the same
// on every run. Only the providers in spov's catalogue are kept.
//
//	SPOV_PROBE=1 go test -tags integration ./internal/server/ -run WriteTheVariantSnapshot -v
//
// @decision(DL-185)
func TestIntegrationWriteTheVariantSnapshot(t *testing.T) {
	if os.Getenv("SPOV_PROBE") == "" && os.Getenv("SPOV_PROBE_OPENCODE") == "" {
		t.Skip("set SPOV_PROBE=1 to write the snapshot from the pinned build")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	bin := probeBinary(ctx, t)
	version := contract.Version()
	env := []string{"OPENCODE_DISABLE_MODELS_FETCH=1", "HTTPS_PROXY=http://127.0.0.1:9", "HTTP_PROXY=http://127.0.0.1:9"}
	for _, name := range prep.ProviderKeyEnvs() {
		env = append(env, name+"=spov-placeholder")
	}
	conn := startProbeServerWith(ctx, t, bin, newFakeModel(t).URL, t.TempDir(), "", env...)
	health, err := opencode.NewClient(conn.URL, conn.Password).WaitHealthy(ctx, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("the server never answered as healthy: %v", err)
	}
	if health.Version != version {
		t.Fatalf("the binary is OpenCode %q, the pin is %q", health.Version, version)
	}
	var models map[string][]string
	for wait := 0; ; wait++ {
		body, err := sandboxGetBounded(ctx, conn.URL+modelListPath, conn.Token, conn.User, conn.Password, MaxModelListBytes)
		if err != nil {
			t.Fatalf("reading the model list: %v", err)
		}
		if models, err = parseModelVariants(body); err == nil {
			break
		} else if !errors.Is(err, errNoModelsListed) || wait == ModelListWaits {
			t.Fatalf("the pinned build's model list: %v", err)
		}
		time.Sleep(ModelListPause)
	}
	carried := map[string]bool{}
	for _, p := range prep.Catalogue() {
		carried[p.ID] = true
	}
	refs := []string{}
	for ref := range models {
		if provider, _, _ := strings.Cut(ref, "/"); carried[provider] {
			refs = append(refs, ref)
		}
	}
	sort.Strings(refs)
	providers := map[string]int{}
	var b strings.Builder
	b.WriteString("{\n  \"opencode\": ")
	v, _ := json.Marshal(version)
	b.Write(v)
	b.WriteString(",\n  \"models\": {")
	for i, ref := range refs {
		if i > 0 {
			b.WriteString(",")
		}
		k, _ := json.Marshal(ref)
		m, _ := json.Marshal(models[ref])
		fmt.Fprintf(&b, "\n    %s: %s", k, m)
		provider, _, _ := strings.Cut(ref, "/")
		providers[provider]++
	}
	b.WriteString("\n  }\n}\n")
	if b.Len() > MaxModelVariantsBytes {
		t.Fatalf("the snapshot is %d bytes, over MaxModelVariantsBytes", b.Len())
	}
	out := filepath.Join("..", "modelcat", "variants.json")
	if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s: OpenCode %s, %d models with modes, by provider %v", out, version, len(refs), providers)
}
