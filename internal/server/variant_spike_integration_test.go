//go:build integration

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/opencode"
)

// Spike S-MV1: what the pinned OpenCode does with a reasoning variant a
// model offers and with one it does not, set through the session's
// model endpoint (a switch) and through the config's model (a launch).
// DeepSeek's package speaks the OpenAI chat API the fake answers; what
// reaches the provider is read off each request's body.
//
//	SPOV_PROBE=1 go test -tags integration ./internal/server/ -run SMV1 -v
func TestIntegrationSMV1AnUnknownVariant(t *testing.T) {
	if os.Getenv("SPOV_PROBE") == "" && os.Getenv("SPOV_PROBE_OPENCODE") == "" {
		t.Skip("set SPOV_PROBE=1 to run the spike")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	var mu sync.Mutex
	var bodies []map[string]any
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"data":[]}`)
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if tools, _ := req["tools"].([]any); len(tools) > 0 {
			delete(req, "messages")
			delete(req, "tools")
			mu.Lock()
			bodies = append(bodies, req)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"c","object":"chat.completion.chunk","created":0,"model":"x","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(fake.Close)
	sent := func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), bodies...)
	}

	// The model to try is the first DeepSeek model with variants.
	start := func(t *testing.T, model string) (Connection, string) {
		work := t.TempDir()
		cfg := map[string]any{"providers": map[string]any{"deepseek": map[string]any{"settings": map[string]any{"baseURL": fake.URL + "/v1"}}}}
		if model != "" {
			cfg["model"] = model
		}
		project, _ := json.Marshal(cfg)
		if err := os.WriteFile(filepath.Join(work, "opencode.json"), project, 0o600); err != nil {
			t.Fatal(err)
		}
		conn := startProbeServerWith(ctx, t, probeBinary(ctx, t), fake.URL, work, "", "DEEPSEEK_API_KEY=probe")
		if _, err := opencode.NewClient(conn.URL, conn.Password).WaitHealthy(ctx, 100*time.Millisecond); err != nil {
			t.Fatal(err)
		}
		return conn, work
	}
	conn, home := start(t, "")
	var model string
	var variants []string
	for wait := 0; wait <= ModelListWaits && model == ""; wait++ {
		_, out := (&spikeRun{t: t, ctx: ctx, conn: conn}).doRoot(http.MethodGet, modelListPath)
		got, _ := parseModelVariants(out)
		for ref, v := range got {
			if strings.HasPrefix(ref, "deepseek/") && (model == "" || ref < model) {
				model, variants = ref, v
			}
		}
		if model == "" {
			time.Sleep(ModelListPause)
		}
	}
	if model == "" {
		t.Fatal("no DeepSeek model with variants is listed")
	}
	t.Logf("%s offers %v", model, variants)

	try := func(t *testing.T, conn Connection, work, ref string, viaEndpoint bool) {
		id, err := createConversation(ctx, conn, work)
		if err != nil {
			t.Fatal(err)
		}
		var setErr error
		if viaEndpoint {
			setErr = setConversationModel(ctx, conn, id, ref)
		}
		before := len(sent())
		r := &spikeRun{t: t, ctx: ctx, conn: conn, id: id, start: time.Now()}
		r.prompt("say ok", "steer", true)
		probeWait(ctx, func() bool { return len(sent()) > before })
		time.Sleep(time.Second)
		after := sent()[before:]
		var shown []string
		for _, b := range after {
			raw, _ := json.Marshal(b)
			shown = append(shown, clip(string(raw), 400))
		}
		_, msgs := r.do(http.MethodGet, "/message", nil)
		failed := strings.Contains(string(msgs), "rror")
		t.Logf("== %s (endpoint %v): set %v; %d provider requests %v; an error in the messages: %v %s",
			ref, viaEndpoint, setErr, len(after), shown, failed, clip(string(msgs), 300))
	}
	for _, ref := range []string{model, model + "#" + variants[len(variants)-1], model + "#spov-no-such-mode"} {
		t.Run("endpoint "+ref, func(t *testing.T) { try(t, conn, home, ref, true) })
	}
	for _, ref := range []string{model + "#" + variants[len(variants)-1], model + "#spov-no-such-mode"} {
		t.Run(fmt.Sprintf("config %s", ref), func(t *testing.T) {
			c, work := start(t, ref)
			try(t, c, work, ref, false)
		})
	}
}
