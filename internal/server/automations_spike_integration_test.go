//go:build integration

package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/opencode"
)

// Spike S-AU1: what OpenCode's automations setting changes for the
// model, under the config a job and an interactive session are given.
// A job's offers no automation tool in its Code Mode catalog; an
// interactive session's still does, so the check would notice a build
// that renamed the catalog's entry.
//
//	SPOV_PROBE=1 go test -tags integration ./internal/server/ -run SAU1 -v
//
// @decision(DL-184)
func TestIntegrationSAU1AJobIsOfferedNoAutomation(t *testing.T) {
	if os.Getenv("SPOV_PROBE") == "" && os.Getenv("SPOV_PROBE_OPENCODE") == "" {
		t.Skip("set SPOV_PROBE=1 to run the spike")
	}
	bin := probeBinary(context.Background(), t)
	const catalog = "tools.automation.create("
	for name, cfg := range map[string]string{
		"job":         opencode.JobConfigJSON("", false, nil),
		"interactive": opencode.SandboxConfigJSON("", false, nil, nil),
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			var mu sync.Mutex
			var bodies []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = io.WriteString(w, `{"data":[{"id":"m","object":"model"}]}`)
					return
				}
				raw, _ := io.ReadAll(r.Body)
				mu.Lock()
				bodies = append(bodies, string(raw))
				mu.Unlock()
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, `data: {"id":"c","object":"chat.completion.chunk","created":0,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`+"\n\n")
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			}))
			t.Cleanup(srv.Close)
			work := t.TempDir()
			// The config is laid over the probe's own, which names the
			// fake model's provider.
			conn := startProbeServerWith(ctx, t, bin, srv.URL, work, "", "OPENCODE_CONFIG_CONTENT="+cfg)
			if _, err := opencode.NewClient(conn.URL, conn.Password).WaitHealthy(ctx, 100*time.Millisecond); err != nil {
				t.Fatal(err)
			}
			id, err := createConversation(ctx, conn, work)
			if err != nil {
				t.Fatal(err)
			}
			if err := setConversationModel(ctx, conn, id, "probe/m"); err != nil {
				t.Fatal(err)
			}
			(&spikeRun{t: t, ctx: ctx, conn: conn, id: id}).prompt("hello", "steer", true)
			var offered, asked bool
			probeWait(ctx, func() bool {
				mu.Lock()
				defer mu.Unlock()
				for _, b := range bodies {
					// The title request offers no tools.
					if strings.Contains(b, `"tools"`) {
						asked = true
						offered = offered || strings.Contains(b, catalog)
					}
				}
				return asked
			})
			if !asked {
				t.Fatal("the model was never asked with tools")
			}
			t.Logf("%s: the catalog offers automations: %v", name, offered)
			if want := name == "interactive"; offered != want {
				t.Errorf("%s: the Code Mode catalog offers automations: %v, want %v", name, offered, want)
			}
		})
	}
}
