//go:build integration

package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	modal "github.com/modal-labs/modal-client/go"
)

// TestIntegrationConnectToken is the DL-030 spike. It creates a sandbox
// with no encrypted ports, mints a connect token for the OpenCode port,
// and checks against the real edge and the pinned OpenCode build:
//
//   - the edge rejects a request without a token;
//   - a token in the _modal_connect_token cookie leaves Authorization
//     free for OpenCode's Basic auth;
//   - a token sent as Bearer leaves OpenCode without Basic, so it answers 401;
//   - an SSE stream and a WebSocket upgrade survive the edge and a
//     stdlib ReverseProxy on loopback;
//   - what the token contains, in particular whether it expires.
func TestIntegrationConnectToken(t *testing.T) {
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	password := randomPassword(t)
	name := fmt.Sprintf("spov-connect-%d", time.Now().UnixNano())
	spec := Spec{
		Name:  name,
		Image: testImage(),
		Command: []string{"bash", "-c",
			"set -e; cd /root; exec /root/.opencode/bin/opencode serve --hostname 0.0.0.0 --port 4096"},
		Env: map[string]string{
			"OPENCODE_PASSWORD": password,
			"HOME":              "/root",
			"PATH":              "/root/.opencode/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		},
		Egress:      EgressOpen(),
		Ports:       []int{testPort},
		ReadyPort:   testPort,
		CPU:         0.5,
		MemoryMiB:   2048,
		Timeout:     15 * time.Minute,
		IdleTimeout: 5 * time.Minute,
		Tags:        BuildTags(TagSet{Name: name, User: "integration-test", CreatedAt: time.Now()}),
	}

	// Spec.Validate insists the ready port is exposed, so the tunnel is
	// removed after translation: the readiness probe is a TCP probe on
	// the container port and does not need one.
	image, err := p.image(ctx, spec.Image)
	if err != nil {
		t.Fatal(err)
	}
	params, err := p.createParams(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	params.EncryptedPorts = nil

	start := time.Now()
	sb, err := p.client.Sandboxes.ExperimentalCreate(ctx, p.app, image, params)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	h := &modalHandle{sb: sb}
	t.Cleanup(func() {
		if err := h.Terminate(context.WithoutCancel(ctx)); err != nil {
			t.Errorf("cleanup terminate: %v", err)
		}
	})
	if err := sb.WaitUntilReady(ctx, readyTimeout, nil); err != nil {
		t.Fatalf("ready: %v", err)
	}
	t.Logf("ready after %s", time.Since(start).Round(100*time.Millisecond))

	t.Run("no tunnel exists", func(t *testing.T) {
		tunnels, err := h.Tunnels(ctx)
		t.Logf("tunnels = %v, err = %v", tunnels, err)
		if len(tunnels) != 0 {
			t.Errorf("a sandbox created without encrypted ports has tunnels: %v", tunnels)
		}
	})

	creds, err := sb.CreateConnectToken(ctx, &modal.SandboxCreateConnectTokenParams{Port: testPort})
	if err != nil {
		t.Fatalf("create connect token: %v", err)
	}
	t.Logf("connect URL: %s", creds.URL)
	t.Logf("token: %d bytes, %d dot-separated parts", len(creds.Token), len(strings.Split(creds.Token, ".")))
	if parts := strings.Split(creds.Token, "."); len(parts) == 3 {
		for i, part := range parts[:2] {
			if raw, err := base64.RawURLEncoding.DecodeString(part); err == nil {
				t.Logf("token part %d: %s", i, raw)
			}
		}
	}

	// HTTP/2 is refused deliberately: an Upgrade request cannot be sent
	// over h2, and this is the transport the real proxy would need too.
	direct := &http.Client{
		Transport: &http.Transport{
			TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       30 * time.Second,
	}
	cookie := &http.Cookie{Name: "_modal_connect_token", Value: creds.Token}

	t.Run("edge rejects a request without a token", func(t *testing.T) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, creds.URL+"/api/info", nil)
		req.SetBasicAuth("opencode", password)
		resp := do(t, direct, req)
		t.Logf("status %d, WWW-Authenticate=%q, body=%q", resp.StatusCode, resp.Header.Get("Www-Authenticate"), resp.body)
		if resp.StatusCode == http.StatusOK {
			t.Error("the edge let a tokenless request through")
		}
		if strings.HasPrefix(resp.Header.Get("Www-Authenticate"), "Basic") {
			t.Error("the response came from OpenCode, not the edge")
		}
	})

	t.Run("cookie token plus Basic reaches OpenCode authenticated", func(t *testing.T) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, creds.URL+"/api/info", nil)
		req.AddCookie(cookie)
		req.SetBasicAuth("opencode", password)
		resp := do(t, direct, req)
		t.Logf("status %d, body=%q", resp.StatusCode, resp.body)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
	})

	t.Run("cookie token without Basic is refused by OpenCode", func(t *testing.T) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, creds.URL+"/api/info", nil)
		req.AddCookie(cookie)
		resp := do(t, direct, req)
		t.Logf("status %d, WWW-Authenticate=%q", resp.StatusCode, resp.Header.Get("Www-Authenticate"))
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401 from OpenCode", resp.StatusCode)
		}
	})

	t.Run("bearer token occupies the Authorization header", func(t *testing.T) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, creds.URL+"/api/info", nil)
		req.Header.Set("Authorization", "Bearer "+creds.Token)
		resp := do(t, direct, req)
		t.Logf("status %d, WWW-Authenticate=%q, body=%q", resp.StatusCode, resp.Header.Get("Www-Authenticate"), resp.body)
	})

	t.Run("query token sets the cookie", func(t *testing.T) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, creds.URL+"/api/info?_modal_connect_token="+url.QueryEscape(creds.Token), nil)
		req.SetBasicAuth("opencode", password)
		resp := do(t, direct, req)
		t.Logf("status %d, Set-Cookie=%q", resp.StatusCode, resp.Header.Values("Set-Cookie"))
	})

	// The loopback proxy the CLI would run: adds the cookie, rewrites the
	// host, streams responses unbuffered.
	target, err := url.Parse(creds.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Host = target.Host
			r.Out.AddCookie(cookie)
		},
		Transport:     direct.Transport,
		FlushInterval: -1,
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: proxy}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	local := "http://" + ln.Addr().String()
	t.Logf("proxy on %s", local)

	// Plain HTTP to loopback: no TLS, so HTTP/1.1 is automatic.
	viaProxy := &http.Client{Timeout: 30 * time.Second}

	t.Run("proxy: health with Basic", func(t *testing.T) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, local+"/api/info", nil)
		req.SetBasicAuth("opencode", password)
		resp := do(t, viaProxy, req)
		t.Logf("status %d, body=%q", resp.StatusCode, resp.body)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
	})

	t.Run("discover routes", func(t *testing.T) {
		for _, path := range []string{"/api/doc", "/doc", "/api/openapi.json"} {
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, local+path, nil)
			req.SetBasicAuth("opencode", password)
			resp := do(t, viaProxy, req)
			if resp.StatusCode != http.StatusOK || !strings.Contains(resp.body, "paths") {
				t.Logf("%s: %d %s", path, resp.StatusCode, resp.Header.Get("Content-Type"))
				continue
			}
			var doc struct {
				Paths map[string]json.RawMessage `json:"paths"`
			}
			if err := json.Unmarshal([]byte(resp.body), &doc); err != nil {
				t.Logf("%s: %v", path, err)
				continue
			}
			for p := range doc.Paths {
				if strings.Contains(p, "event") || strings.Contains(p, "pty") || strings.Contains(p, "health") {
					t.Logf("route %s", p)
				}
			}
			return
		}
	})

	t.Run("proxy: SSE event stream", func(t *testing.T) {
		for _, path := range []string{"/api/event", "/event", "/api/global/event", "/global/event"} {
			sseCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			req, _ := http.NewRequestWithContext(sseCtx, http.MethodGet, local+path, nil)
			req.SetBasicAuth("opencode", password)
			req.Header.Set("Accept", "text/event-stream")
			resp, err := (&http.Client{}).Do(req)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			t.Logf("%s: status %d, Content-Type=%q", path, resp.StatusCode, resp.Header.Get("Content-Type"))
			if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
				resp.Body.Close()
				cancel()
				continue
			}
			line, err := bufio.NewReader(resp.Body).ReadString('\n')
			t.Logf("first line %q, err %v", line, err)
			resp.Body.Close()
			cancel()
			if err != nil {
				t.Errorf("no event arrived through the proxy: %v", err)
			}
			return
		}
		t.Error("no event stream route found")
	})

	t.Run("proxy: PTY WebSocket upgrade", func(t *testing.T) {
		post := func(path string, body string) response {
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, local+path, strings.NewReader(body))
			req.SetBasicAuth("opencode", password)
			req.Header.Set("Content-Type", "application/json")
			// The ticket endpoint refuses requests without this marker.
			req.Header.Set("x-opencode-ticket", "1")
			return do(t, viaProxy, req)
		}
		root := ""
		var created response
		for _, r := range []string{"/api/pty", "/pty"} {
			created = post(r, `{}`)
			t.Logf("POST %s: %d %q", r, created.StatusCode, created.body)
			if created.StatusCode == http.StatusOK {
				root = r
				break
			}
		}
		if root == "" {
			t.Skip("cannot create a pty on this build")
		}
		var pty struct {
			ID   string
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(created.body), &pty); err != nil || pty.Data.ID == "" {
			t.Fatalf("no pty id in %q", created.body)
		}
		pty.ID = pty.Data.ID
		ticketResp := post(root+"/"+pty.ID+"/connect-token", "")
		t.Logf("POST connect-token: %d %q", ticketResp.StatusCode, ticketResp.body)
		var ticket struct {
			Ticket string `json:"ticket"`
			Token  string `json:"token"`
			Data   struct {
				Ticket string `json:"ticket"`
				Token  string `json:"token"`
			} `json:"data"`
		}
		_ = json.Unmarshal([]byte(ticketResp.body), &ticket)
		for _, candidate := range []string{ticket.Ticket, ticket.Token, ticket.Data.Ticket, ticket.Data.Token} {
			if candidate != "" {
				ticket.Ticket = candidate
				break
			}
		}
		if ticket.Ticket == "" {
			t.Fatalf("no ticket in %q", ticketResp.body)
		}

		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, local+root+"/"+pty.ID+"/connect?ticket="+url.QueryEscape(ticket.Ticket), nil)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Sec-WebSocket-Version", "13")
		req.Header.Set("Sec-WebSocket-Key", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 16)))
		resp, err := (&http.Client{}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		t.Logf("upgrade: %d, Upgrade=%q, Sec-WebSocket-Accept=%q", resp.StatusCode, resp.Header.Get("Upgrade"), resp.Header.Get("Sec-Websocket-Accept"))
		if resp.StatusCode != http.StatusSwitchingProtocols {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			t.Errorf("upgrade returned %d: %q", resp.StatusCode, body)
		}
	})
}

type response struct {
	*http.Response
	body string
}

func do(t *testing.T, c *http.Client, req *http.Request) response {
	t.Helper()
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return response{resp, string(b)}
}
