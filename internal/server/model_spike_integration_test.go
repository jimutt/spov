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

// Spike S-M1: what the pinned OpenCode does with a model its
// catalogue lacks, under a provider it knows, with and without the name
// declared in its config at providers.<id>.models.<modelID>. DeepSeek is
// used because its package speaks the OpenAI chat API the fake answers,
// pointed at the fake through the provider's settings.
//
//	SPOV_PROBE=1 go test -tags integration ./internal/server/ -run SM1 -v
func TestIntegrationSM1CustomModel(t *testing.T) {
	if os.Getenv("SPOV_PROBE") == "" && os.Getenv("SPOV_PROBE_OPENCODE") == "" {
		t.Skip("set SPOV_PROBE=1 to run the spike")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	type call = sm1Call
	var mu sync.Mutex
	var calls []call
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"data":[]}`)
			return
		}
		var req struct {
			Model string            `json:"model"`
			Tools []json.RawMessage `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		calls = append(calls, call{req.Model, len(req.Tools)})
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range []string{
			`{"id":"c","object":"chat.completion.chunk","created":0,"model":"x","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`,
			`{"id":"c","object":"chat.completion.chunk","created":0,"model":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		} {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(fake.Close)

	for _, declare := range []bool{true, false} {
		for _, envKey := range []bool{true} {
			t.Run(fmt.Sprintf("declared=%v,envkey=%v", declare, envKey), func(t *testing.T) {
				sm1Round(ctx, t, fake.URL, declare, envKey, &mu, func() []call { return calls })
			})
		}
	}
}

type sm1Call = struct {
	model string
	tools int
}

func sm1Round(ctx context.Context, t *testing.T, fakeURL string, declare, envKey bool, mu *sync.Mutex, calls func() []struct {
	model string
	tools int
}) {
	work := t.TempDir()
	// The key as a sandbox holds it, in the environment, or in config.
	settings := map[string]any{"baseURL": fakeURL + "/v1"}
	var env []string
	if envKey {
		env = append(env, "DEEPSEEK_API_KEY=probe")
	} else {
		settings["apiKey"] = "probe"
	}
	deepseek := map[string]any{"settings": settings}
	if declare {
		deepseek["models"] = map[string]any{
			"spov-declared":       map[string]any{"name": "spov-declared"},
			"spov-declared-tools": map[string]any{"name": "spov-declared-tools", "capabilities": map[string]any{"tools": true}},
		}
	}
	// Merged over the global config the probe writes.
	project, _ := json.Marshal(map[string]any{"providers": map[string]any{"deepseek": deepseek}})
	if err := os.WriteFile(filepath.Join(work, "opencode.json"), project, 0o600); err != nil {
		t.Fatal(err)
	}
	conn := startProbeServerWith(ctx, t, probeBinary(ctx, t), fakeURL, work, "", env...)
	if _, err := opencode.NewClient(conn.URL, conn.Password).WaitHealthy(ctx, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	frames := followRawFramesAll(ctx, t, conn)
	for _, path := range []string{"/api/provider", "/api/model"} {
		code, out := (&spikeRun{t: t, ctx: ctx, conn: conn, start: time.Now()}).doRoot(http.MethodGet, path)
		t.Logf("GET %s -> %d (%d bytes) deepseek: %s", path, code, len(out), clip(deepseekOf(out), 600))
	}

	for _, model := range []string{
		"deepseek/deepseek-v4-flash", // listed upstream, deprecated
		"deepseek/deepseek-flash",    // listed upstream, active
		"deepseek/spov-unlisted",     // neither listed nor declared
		"deepseek/spov-declared",
		"deepseek/spov-declared-tools",
		"deepseek/spov-declared#high", // a variant on a declared model with none
		"probe/m",                     // the probe's own, declared with a package
	} {
		t.Run(strings.ReplaceAll(model, "/", "_"), func(t *testing.T) {
			id, err := createConversation(ctx, conn, work)
			if err != nil {
				t.Fatal(err)
			}
			setErr := setConversationModel(ctx, conn, id, model)
			mu.Lock()
			before := len(calls())
			mu.Unlock()
			r := &spikeRun{t: t, ctx: ctx, conn: conn, id: id, start: time.Now()}
			r.prompt("say ok", "steer", true)
			time.Sleep(3 * time.Second)
			code, out := r.do(http.MethodGet, "/message", nil)
			mu.Lock()
			got := append([]sm1Call(nil), calls()[before:]...)
			mu.Unlock()
			var failed []string
			for _, f := range frames.of(id) {
				if strings.HasPrefix(f.name, "session.model") {
					t.Logf("  frame %s: %s", f.name, clip(f.detail, 600))
				}
				if strings.Contains(f.name, "failed") || strings.Contains(f.name, "error") {
					failed = append(failed, clip(f.detail, 500))
				}
			}
			t.Logf("== %s\n  set model: %v\n  model calls: %+v\n  messages %d: %s\n  failures: %s", model, setErr, got, code, clip(string(out), 300), failed)
		})
	}
}

// deepseekOf picks the deepseek provider out of GET /api/provider.
func deepseekOf(raw []byte) string {
	i := strings.Index(string(raw), `"id":"deepseek"`)
	if i < 0 {
		return "(absent) " + clip(string(raw), 300)
	}
	return string(raw[max(0, i-50):])
}

// doRoot is spikeRun.do on a server-wide path rather than a
// conversation's.
func (r *spikeRun) doRoot(method, path string) (int, []byte) {
	r.t.Helper()
	req, err := http.NewRequestWithContext(r.ctx, method, r.conn.URL+path, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	req.SetBasicAuth(r.conn.User, r.conn.Password)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}
