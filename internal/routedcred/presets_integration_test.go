//go:build integration

package routedcred

import (
	"context"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/credproxy"
)

// Spike S-RP1: each preset's real client, in Docker, reaches a private
// registry through the real proxy with the files PresetFiles writes, the
// sandbox CA in its system bundle and, for the JVM, the trust store the
// entrypoint builds with openssl's -jdktrust. The registry records what
// arrives; the client's own exit status is only logged, since the
// registry serves nothing installable. Local Docker, no Modal, no key:
//
//	SPOV_ROUTED_PRESETS=1 go test -tags integration ./internal/routedcred/ -run SRP1 -v
//
// It listens on :3128, the port the files and JAVA_TOOL_OPTIONS name, and
// reaches it from the containers as `proxy` through host-gateway.
//
// @decision(DL-076)
func TestIntegrationSRP1PresetsThroughTheProxy(t *testing.T) {
	if os.Getenv("SPOV_ROUTED_PRESETS") == "" {
		t.Skip("set SPOV_ROUTED_PRESETS=1 to run the clients in Docker")
	}
	// httptest's certificate covers *.example.com.
	const host = "registry.example.com"
	type seen struct{ path, auth, agent string }
	var (
		mu   sync.Mutex
		hits []seen
	)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, seen{r.URL.Path, r.Header.Get("Authorization"), r.UserAgent()})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	npm, _ := FromPreset("acme-npm", PresetNPM, PresetOptions{URL: "https://" + host + "/npm/", Scope: "@acme"})
	mvn, _ := FromPreset("acme-mvn", PresetMaven, PresetOptions{URL: "https://" + host + "/maven/", User: "ci"})
	nuget, _ := FromPreset("acme-nuget", PresetNuGet, PresetOptions{URL: "https://" + host + "/nuget/v3/index.json"})
	pypi, _ := FromPreset("acme-pypi", PresetPyPI, PresetOptions{URL: "https://" + host + "/pypi/simple/"})
	// One host, four prefixes: the proxy holds one route per host, so
	// the spike holds one route covering all four and checks each path.
	route := credproxy.Route{Name: "acme", Hosts: []string{host}, Scheme: credproxy.SchemeBearer, Secret: "realsecret"}
	cfg := credproxy.SidecarConfig{Routes: []credproxy.Route{route}}
	certPEM, keyPEM, err := credproxy.NewCA(cfg.CAHosts(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ca, err := credproxy.LoadCA(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	store, err := credproxy.NewStore(filepath.Join(t.TempDir(), "credentials.json"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	p, err := credproxy.New(credproxy.Config{
		CA: ca, Decrypt: func(h string) bool { return h == host },
		Rewrite: store.Rewrite, Secrets: store.Secrets, Streamed: store.Streamed,
		UpstreamRoots: roots,
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			if strings.HasPrefix(addr, host+":") {
				return d.DialContext(ctx, network, upstream.Listener.Addr().String())
			}
			return d.DialContext(ctx, network, addr)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp4", "0.0.0.0:3128")
	if err != nil {
		t.Skipf("port 3128 is taken: %v", err)
	}
	srv := &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	dir := t.TempDir()
	ph := func(string) string { return "placeholder" }
	for _, f := range PresetFiles([]Route{npm, mvn, nuget, pypi}, ph) {
		path := filepath.Join(dir, "fs", f.Path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(f.Content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"HTTPS_PROXY=http://proxy:3128", "https_proxy=http://proxy:3128", "NO_PROXY=localhost", "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt",
		"REQUESTS_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt", "NODE_EXTRA_CA_CERTS=/spike/ca.pem"}
	trust := "mkdir -p /etc/ssl/certs && cat /spike/ca.pem >> /etc/ssl/certs/ca-certificates.crt && cp -r /spike/fs/. / && "
	// The trust store is built where the sandbox builds it, on Debian
	// trixie, then read by a JVM of another distribution.
	run := func(name, image, script string) {
		t.Helper()
		args := []string{"run", "--rm", "--add-host", "proxy:host-gateway", "-v", dir + ":/spike"}
		for _, e := range env {
			args = append(args, "-e", e)
		}
		args = append(args, image, "bash", "-c", script)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		t.Logf("%s (%v):\n%s", name, err, tail(string(out), 60))
	}
	run("trust store", "debian:trixie-slim", "apt-get update -qq >/dev/null && apt-get install -y -qq openssl ca-certificates >/dev/null && "+
		trust+strings.ReplaceAll(credproxy.JavaTrustCommand(), credproxy.JavaTrustStore, "/spike/java-truststore.p12"))
	run("curl", "debian:trixie-slim", "apt-get update -qq >/dev/null && apt-get install -y -qq curl >/dev/null 2>&1; "+trust+
		"curl -sS -o /dev/null -w '%{http_code}\\n' https://"+host+"/npm/probe")
	run("npm", "node:22-slim", trust+"npm view @acme/pkg --loglevel=http")
	run("pip", "python:3.13-slim", trust+"pip download --no-deps -d /tmp/x acme-pkg --retries 0 --timeout 5")
	run("uv", "ghcr.io/astral-sh/uv:debian", trust+"uv venv -q /tmp/v && VIRTUAL_ENV=/tmp/v uv pip install --dry-run acme-pkg --index-strategy unsafe-best-match -v 2>&1 | grep -iv 'debug' | tail -20")
	run("maven", "maven:3.9-eclipse-temurin-21", trust+"JAVA_TOOL_OPTIONS='"+strings.ReplaceAll(credproxy.JavaToolOptions(), credproxy.JavaTrustStore, "/spike/java-truststore.p12")+
		"' mvn -B -q dependency:get -Dartifact=com.acme:lib:1.0 -DremoteRepositories=acme-mvn::default::https://"+host+"/maven/")
	run("dotnet", "mcr.microsoft.com/dotnet/sdk:9.0", trust+"dotnet package search acme --source acme-nuget")

	mu.Lock()
	defer mu.Unlock()
	for _, c := range []struct{ client, agent, prefix string }{
		{"npm", "npm/", "/npm/"}, {"pip", "pip/", "/pypi/simple/"}, {"uv", "uv/", "/pypi/simple/"},
		{"maven", "Apache-Maven", "/maven/"}, {"dotnet", "NuGet", "/nuget/"},
	} {
		found := false
		for _, h := range hits {
			if strings.HasPrefix(h.path, c.prefix) && strings.Contains(h.agent, c.agent) {
				found = true
				if h.auth != "Bearer realsecret" {
					t.Errorf("%s: %s arrived with %q", c.client, h.path, h.auth)
				}
			}
		}
		if !found {
			t.Errorf("%s never reached %s through the proxy", c.client, c.prefix)
		}
	}
	t.Logf("the registry saw %d requests: %v", len(hits), hits)
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
