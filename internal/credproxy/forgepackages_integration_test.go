//go:build integration

package credproxy

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Spike S-PK1, the Forgejo half: the proxy's own policy and presentation
// (Store.Rewrite) against a real Forgejo whose packages live in private
// organisations, so a read needs the credential. A session on acme/app
// reads acme's packages with the token, never another organisation's,
// and never publishes. Run a forge with scripts/forge-test-server.sh,
// then:
//
//	SPOV_TEST_FORGEJO_ENV=<dir>/env go test -tags integration ./internal/credproxy/ -run SPK1 -v
//
// GitLab's half needs a test token and is not here.
//
// @decision(DL-078)
func TestIntegrationSPK1ForgejoPackages(t *testing.T) {
	envFile := os.Getenv("SPOV_TEST_FORGEJO_ENV")
	if envFile == "" {
		t.Skip("set SPOV_TEST_FORGEJO_ENV to a forge-test-server env file")
	}
	env := readForgeEnv(t, envFile)
	base, err := url.Parse(env["FORGE_URL"])
	if err != nil {
		t.Fatal(err)
	}
	caPEM, err := os.ReadFile(env["FORGE_CA"])
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(caPEM)
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}}
	admin, token := env["FORGE_ADMIN"], env["FORGE_ADMIN_TOKEN"]
	direct := func(method, path, body string) int {
		t.Helper()
		req, _ := http.NewRequest(method, base.String()+path, strings.NewReader(body))
		req.SetBasicAuth(admin, token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	for _, org := range []string{"acme", "other"} {
		direct(http.MethodPost, "/api/v1/orgs", `{"username":"`+org+`","visibility":"private"}`)
		if s := direct(http.MethodPut, "/api/packages/"+org+"/generic/tool/1.0/hello.txt", "hello"); s != http.StatusCreated && s != http.StatusConflict {
			t.Fatalf("publishing to %s: %d", org, s)
		}
	}

	host := base.Host
	store, err := NewStore(filepath.Join(t.TempDir(), "credentials.json"), SidecarConfig{Platforms: []Platform{{
		Name: "fj", Kind: KindForgejo, Host: host, User: admin, Token: token, Repos: []string{"acme/app"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	// through sends a request as the main container would, the proxy's
	// policy deciding whether it carries the token.
	through := func(method, path, body string) (int, bool) {
		t.Helper()
		req, _ := http.NewRequest(method, base.String()+path, strings.NewReader(body))
		req.RequestURI = path
		credentialed := store.Rewrite(req)
		req.RequestURI = ""
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode, credentialed
	}
	for _, c := range []struct {
		name, method, path, body string
		credentialed             bool
		status                   int
	}{
		{"its owner's package", http.MethodGet, "/api/packages/acme/generic/tool/1.0/hello.txt", "", true, http.StatusOK},
		{"another owner's package", http.MethodGet, "/api/packages/other/generic/tool/1.0/hello.txt", "", false, 0},
		{"a publish", http.MethodPut, "/api/packages/acme/generic/tool/2.0/hello.txt", "x", false, 0},
		{"its owner's npm metadata", http.MethodGet, "/api/packages/acme/npm/@acme%2Fpkg", "", true, 0},
		{"its owner's pypi index", http.MethodGet, "/api/packages/acme/pypi/simple/pkg", "", true, 0},
	} {
		status, credentialed := through(c.method, c.path, c.body)
		t.Logf("%s: credentialed %v, status %d", c.name, credentialed, status)
		if credentialed != c.credentialed {
			t.Errorf("%s: credentialed = %v", c.name, credentialed)
		}
		switch {
		case c.status != 0 && status != c.status:
			t.Errorf("%s: status %d, want %d", c.name, status, c.status)
		case !c.credentialed && status/100 == 2:
			t.Errorf("%s: succeeded without the token (%d)", c.name, status)
		case c.credentialed && status == http.StatusUnauthorized:
			t.Errorf("%s: the forge refused the token's presentation", c.name)
		}
	}
}

func readForgeEnv(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok {
			out[k] = strings.Trim(v, `"'`)
		}
	}
	return out
}
