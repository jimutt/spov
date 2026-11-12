package credproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// testCA makes a CA for the given hosts and returns it with a pool that
// trusts it, as the main container's bundle would.
func testCA(t *testing.T, hosts ...string) (*CA, *x509.CertPool) {
	t.Helper()
	certPEM, keyPEM, err := NewCA(hosts, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ca, err := LoadCA(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("the CA is not a usable PEM certificate")
	}
	return ca, pool
}

// rig is a proxy in front of one TLS upstream that every host name
// resolves to, and a client that uses the proxy and trusts both the
// sandbox CA and the upstream's own certificate.
type rig struct {
	proxy    *httptest.Server
	upstream *httptest.Server
	client   *http.Client

	mu    sync.Mutex
	seen  []*http.Request
	audit []Entry
}

func newRig(t *testing.T, cfg Config, upstream http.HandlerFunc) *rig {
	t.Helper()
	r := &rig{}
	r.upstream = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.seen = append(r.seen, req.Clone(context.Background()))
		r.mu.Unlock()
		if upstream != nil {
			upstream(w, req)
			return
		}
		_, _ = io.WriteString(w, "upstream "+req.Host)
	}))
	t.Cleanup(r.upstream.Close)

	upstreamAddr := r.upstream.Listener.Addr().String()
	cfg.Dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, upstreamAddr)
	}
	cfg.UpstreamRoots = x509.NewCertPool()
	cfg.UpstreamRoots.AddCert(r.upstream.Certificate())
	cfg.Audit = func(e Entry) {
		r.mu.Lock()
		r.audit = append(r.audit, e)
		r.mu.Unlock()
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.proxy = httptest.NewServer(p)
	t.Cleanup(r.proxy.Close)

	roots := x509.NewCertPool()
	if cfg.CA != nil {
		roots.AddCert(cfg.CA.cert)
	}
	roots.AddCert(r.upstream.Certificate())
	proxyURL, _ := url.Parse(r.proxy.URL)
	r.client = &http.Client{
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{RootCAs: roots},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       10 * time.Second,
	}
	return r
}

// audits waits briefly for n audit entries, which the proxy records after
// the response has gone out, and returns what it has.
func (r *rig) audits(n int) []Entry {
	deadline := time.Now().Add(2 * time.Second)
	for {
		r.mu.Lock()
		got := append([]Entry(nil), r.audit...)
		r.mu.Unlock()
		if len(got) >= n || time.Now().After(deadline) {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (r *rig) lastSeen(t *testing.T) *http.Request {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.seen) == 0 {
		t.Fatal("the upstream saw no request")
	}
	return r.seen[len(r.seen)-1]
}

func forgeOnly(host string) bool { return host == "forge.example.com" }

// addToken is a policy that adds a credential to every request.
func addToken(r *http.Request) bool {
	r.Header.Set("Authorization", "Bearer real-token")
	return true
}

// A decrypted request has the client's own credential removed and the
// policy's added, and is audited without the credential.
//
// @decision(DL-034)
func TestADecryptedRequestCarriesThePolicysCredentialOnly(t *testing.T) {
	ca, _ := testCA(t, "forge.example.com")
	r := newRig(t, Config{CA: ca, Decrypt: forgeOnly, Rewrite: addToken}, nil)

	req, _ := http.NewRequest(http.MethodGet, "https://forge.example.com/owner/repo.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Authorization", "Bearer ghs_placeholder")
	req.Header.Set("X-Api-Key", "placeholder")
	resp, err := r.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	seen := r.lastSeen(t)
	if got := seen.Header.Get("Authorization"); got != "Bearer real-token" {
		t.Errorf("upstream Authorization = %q", got)
	}
	if seen.Header.Get("X-Api-Key") != "" {
		t.Error("the client's own X-Api-Key reached the upstream")
	}
	if seen.Host != "forge.example.com" || seen.URL.RawQuery != "service=git-upload-pack" {
		t.Errorf("upstream saw host %q query %q", seen.Host, seen.URL.RawQuery)
	}
	if audit := r.audits(1); len(audit) != 1 || audit[0] != (Entry{Method: "GET", Host: "forge.example.com", Path: "/owner/repo.git/info/refs", Status: 200}) {
		t.Errorf("audit = %+v", audit)
	}
	if peer := resp.TLS.PeerCertificates[0]; peer.Issuer.CommonName != "spov sandbox proxy CA" {
		t.Errorf("the client was not served the sandbox CA's certificate: issuer %q", peer.Issuer.CommonName)
	}
}

// A host the policy does not name is tunneled: the client talks TLS to
// the upstream itself, and nothing is added or removed.
//
// @decision(DL-034)
func TestAnotherHostIsTunneledUntouched(t *testing.T) {
	ca, _ := testCA(t, "forge.example.com")
	r := newRig(t, Config{CA: ca, Decrypt: forgeOnly, Rewrite: addToken}, nil)

	req, _ := http.NewRequest(http.MethodGet, "https://models.example.com/v1/messages", nil)
	req.Header.Set("X-Api-Key", "the-clients-own")
	resp, err := r.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	seen := r.lastSeen(t)
	if seen.Header.Get("X-Api-Key") != "the-clients-own" || seen.Header.Get("Authorization") != "" {
		t.Errorf("a tunneled request was changed: %v", seen.Header)
	}
	if peer := resp.TLS.PeerCertificates[0]; peer.Issuer.CommonName == "spov sandbox proxy CA" {
		t.Error("a tunneled host was decrypted")
	}
	if audit := r.audits(0); len(audit) != 0 {
		t.Errorf("a tunneled request was audited: %+v", audit)
	}
}

// Only port 443 is decrypted, even for a forge host.
func TestOnlyPort443IsDecrypted(t *testing.T) {
	ca, _ := testCA(t, "forge.example.com")
	r := newRig(t, Config{CA: ca, Decrypt: forgeOnly, Rewrite: addToken}, nil)
	resp, err := r.client.Get("https://forge.example.com:8443/x")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if r.lastSeen(t).Header.Get("Authorization") != "" {
		t.Error("a request on port 8443 got a credential")
	}
}

// A request whose Host names another server than the CONNECT target is
// refused, so a credential for one host cannot be sent to another.
//
// @decision(DL-034)
func TestAHostThatDoesNotMatchTheConnectIsRefused(t *testing.T) {
	ca, _ := testCA(t, "forge.example.com")
	r := newRig(t, Config{CA: ca, Decrypt: forgeOnly, Rewrite: addToken}, nil)
	req, _ := http.NewRequest(http.MethodGet, "https://forge.example.com/x", nil)
	req.Host = "elsewhere.example.com"
	resp, err := r.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("status %d, want 421", resp.StatusCode)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.seen) != 0 {
		t.Error("the upstream saw a mismatched request")
	}
}

// A TLS handshake that names another server than the CONNECT target
// fails.
func TestASNIThatDoesNotMatchTheConnectFails(t *testing.T) {
	ca, pool := testCA(t, "forge.example.com", "elsewhere.example.com")
	r := newRig(t, Config{CA: ca, Decrypt: forgeOnly, Rewrite: addToken}, nil)

	conn := connectThrough(t, r.proxy.Listener.Addr().String(), "forge.example.com:443")
	tconn := tls.Client(conn, &tls.Config{ServerName: "elsewhere.example.com", RootCAs: pool})
	defer tconn.Close()
	if err := tconn.Handshake(); err == nil {
		t.Error("a handshake for elsewhere.example.com inside CONNECT forge.example.com succeeded")
	}
}

// A redirect goes back to the client; the proxy never follows it, so a
// credential never goes to where a redirect points.
//
// @decision(DL-034)
func TestARedirectIsNotFollowed(t *testing.T) {
	ca, _ := testCA(t, "forge.example.com")
	r := newRig(t, Config{CA: ca, Decrypt: forgeOnly, Rewrite: addToken}, func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, "https://elsewhere.example.com/archive", http.StatusFound)
	})
	resp, err := r.client.Get("https://forge.example.com/owner/repo/tarball")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "https://elsewhere.example.com/archive" {
		t.Errorf("status %d location %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.seen) != 1 {
		t.Errorf("the upstream saw %d requests, want 1", len(r.seen))
	}
}

// A plain-HTTP request is forwarded, and never gets a credential, even to
// a host the policy decrypts.
//
// @decision(DL-034)
func TestAPlainHTTPRequestNeverGetsACredential(t *testing.T) {
	ca, _ := testCA(t, "forge.example.com")
	var got http.Header
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = io.WriteString(w, "plain")
	}))
	defer plain.Close()
	p, err := New(Config{CA: ca, Decrypt: forgeOnly, Rewrite: addToken,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, plain.Listener.Addr().String())
		}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	defer srv.Close()
	proxyURL, _ := url.Parse(srv.URL)
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	resp, err := c.Get("http://forge.example.com/owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "plain" || got.Get("Authorization") != "" {
		t.Errorf("body %q, Authorization %q", body, got.Get("Authorization"))
	}
}

// A request that is neither CONNECT nor an absolute http:// URL is
// refused.
func TestANonProxyRequestIsRefused(t *testing.T) {
	ca, _ := testCA(t, "forge.example.com")
	p, err := New(Config{CA: ca})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status %d", rec.Code)
	}
}

// The default dialer refuses loopback, private and link-local addresses:
// the main container, the proxy itself and the metadata address.
//
// @decision(DL-034)
func TestTheProxyDialsPublicAddressesOnly(t *testing.T) {
	ca, _ := testCA(t, "forge.example.com")
	p, err := New(Config{CA: ca})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	defer srv.Close()
	for _, target := range []string{"127.0.0.1:22", "localhost:3128", "10.0.0.1:443", "169.254.169.254:80", "[::1]:443"} {
		conn, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(conn, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n")
		resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		_ = resp.Body.Close()
		_ = conn.Close()
		if resp.StatusCode != http.StatusBadGateway {
			t.Errorf("CONNECT %s answered %d, want 502", target, resp.StatusCode)
		}
	}
	for ip, want := range map[string]bool{"140.82.112.3": true, "2606:4700::1111": true, "172.20.1.27": false, "fe80::1": false, "0.0.0.0": false} {
		if got := Public(net.ParseIP(ip)); got != want {
			t.Errorf("Public(%s) = %v", ip, got)
		}
	}
}

// A CONNECT without a port is refused.
func TestAConnectWithoutAPortIsRefused(t *testing.T) {
	ca, _ := testCA(t, "forge.example.com")
	p, err := New(Config{CA: ca})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodConnect, "http://forge.example.com", nil)
	req.Host = "forge.example.com"
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status %d", rec.Code)
	}
}

func TestANewProxyNeedsACA(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("a proxy without a CA was built")
	}
}

// connectThrough opens a CONNECT tunnel through the proxy.
func connectThrough(t *testing.T, proxyAddr, target string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(conn, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT %s answered %d", target, resp.StatusCode)
	}
	if br.Buffered() > 0 {
		t.Fatal("unexpected bytes after the CONNECT answer")
	}
	return conn
}

// A forge on a port other than 443 is decrypted when its target, with
// the port, is named, and gets its credential.
//
// @decision(DL-034)
func TestAForgeOnAnotherPortIsDecrypted(t *testing.T) {
	ca, _ := testCA(t, "forge.example.com")
	r := newRig(t, Config{CA: ca, Decrypt: func(target string) bool { return target == "forge.example.com:3000" }, Rewrite: addToken}, nil)
	resp, err := r.client.Get("https://forge.example.com:3000/o/r.git/info/refs")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if r.lastSeen(t).Header.Get("Authorization") != "Bearer real-token" {
		t.Error("a forge on :3000 did not get its credential")
	}
	if audit := r.audits(1); len(audit) != 1 || audit[0].Host != "forge.example.com:3000" {
		t.Errorf("audit = %+v", audit)
	}
}

// An upstream that echoes the credential, as Forgejo's LFS batch API
// does, has it removed from the body and the headers before the client
// sees them; git's pack responses pass through unread.
//
// @decision(DL-034)
func TestAnEchoedCredentialNeverReachesTheClient(t *testing.T) {
	ca, _ := testCA(t, "forge.example.com")
	secrets := func(*http.Request) []string { return []string{"real-token"} }
	var gotEncoding string
	r := newRig(t, Config{CA: ca, Decrypt: forgeOnly, Rewrite: addToken, Secrets: secrets},
		func(w http.ResponseWriter, req *http.Request) {
			gotEncoding = req.Header.Get("Accept-Encoding")
			if strings.HasSuffix(req.URL.Path, "git-upload-pack") {
				w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
				_, _ = io.WriteString(w, "PACK real-token")
				return
			}
			w.Header().Set("Content-Type", "application/vnd.git-lfs+json")
			w.Header().Set("X-Echo", req.Header.Get("Authorization"))
			_, _ = io.WriteString(w, `{"objects":[{"actions":{"download":{"header":{"Authorization":"`+
				req.Header.Get("Authorization")+`"}}}}]}`)
		})

	req, _ := http.NewRequest(http.MethodPost, "https://forge.example.com/o/r.git/info/lfs/objects/batch", strings.NewReader("{}"))
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := r.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if strings.Contains(string(body), "real-token") || strings.Contains(resp.Header.Get("X-Echo"), "real-token") {
		t.Errorf("the credential reached the client: %s %v", body, resp.Header)
	}
	if !strings.Contains(string(body), "Bearer "+scrubbed) {
		t.Errorf("the body was not passed on: %s", body)
	}
	if gotEncoding != "gzip" {
		t.Errorf("the upstream was asked for %q; the transport should ask for gzip itself and decode it", gotEncoding)
	}

	pack, err := r.client.Post("https://forge.example.com/o/r.git/git-upload-pack", "application/x-git-upload-pack-request", strings.NewReader("0000"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(pack.Body)
	_ = pack.Body.Close()
	if string(body) != "PACK real-token" {
		t.Errorf("a pack response was changed: %q", body)
	}
}

// GitLab's own credential headers, Sudo, and their underscore spellings
// never reach the upstream.
//
// @decision(DL-034)
func TestGitLabsCredentialHeadersNeverReachTheUpstream(t *testing.T) {
	ca, _ := testCA(t, "forge.example.com")
	r := newRig(t, Config{CA: ca, Decrypt: forgeOnly, Rewrite: addToken}, nil)
	req, _ := http.NewRequest(http.MethodGet, "https://forge.example.com/api/v4/user", nil)
	for _, h := range []string{"Private-Token", "Job-Token", "Deploy-Token", "Sudo"} {
		req.Header.Set(h, "placeholder")
	}
	req.Header["Private_Token"] = []string{"placeholder"}
	req.Header["SUDO"] = []string{"root"}
	resp, err := r.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	seen := r.lastSeen(t)
	for name := range seen.Header {
		if c := http.CanonicalHeaderKey(strings.ReplaceAll(name, "_", "-")); slices.Contains(credentialHeaders, c) && c != "Authorization" {
			t.Errorf("%s reached the upstream", name)
		}
	}
}

// A credentialed response the scrub cannot cover safely is refused: a
// part of a body, which can cut the secret in two; and early hints, whose
// headers the scrub never sees. The escaped spellings of the secret go
// too.
//
// @decision(DL-135)
func TestAnEchoedCredentialInAnotherFormNeverReachesTheClient(t *testing.T) {
	ca, _ := testCA(t, "forge.example.com")
	const secret = "real-token/with+chars"
	secrets := func(*http.Request) []string { return []string{secret} }
	r := newRig(t, Config{CA: ca, Decrypt: forgeOnly, Rewrite: addToken, Secrets: secrets},
		func(w http.ResponseWriter, req *http.Request) {
			switch req.URL.Path {
			case "/escaped":
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"auth":"`+strings.ReplaceAll(secret, "/", `\/`)+`"}`)
			case "/urlencoded":
				_, _ = io.WriteString(w, "token="+url.QueryEscape(secret))
			case "/range":
				w.Header().Set("Content-Range", "bytes 0-9/20")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = io.WriteString(w, secret[:10])
			case "/pack-range":
				w.Header().Set("Content-Type", "application/octet-stream")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = io.WriteString(w, "bytes")
			case "/hints":
				w.Header().Set("Link", "</x>; rel=preload")
				w.Header().Set("X-Early-Echo", "Bearer "+secret)
				w.WriteHeader(http.StatusEarlyHints)
				w.Header().Del("Link")
				w.Header().Del("X-Early-Echo")
				_, _ = io.WriteString(w, "done")
			}
		})
	get := func(path string, trace *httptrace.ClientTrace) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, "https://forge.example.com"+path, nil)
		if trace != nil {
			req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
		}
		resp, err := r.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	if _, body := get("/escaped", nil); strings.Contains(body, "real-token") || !strings.Contains(body, scrubbed) {
		t.Errorf("a slash-escaped echo reached the client: %s", body)
	}
	if _, body := get("/urlencoded", nil); strings.Contains(body, "real-token") {
		t.Errorf("a URL-encoded echo reached the client: %s", body)
	}
	if status, body := get("/range", nil); status == http.StatusPartialContent || strings.Contains(body, "real-token") {
		t.Errorf("a partial response was passed on: %d %s", status, body)
	}
	if status, body := get("/pack-range", nil); status != http.StatusPartialContent || body != "bytes" {
		t.Errorf("a partial binary response was refused: %d %s", status, body)
	}
	var hinted http.Header
	status, body := get("/hints", &httptrace.ClientTrace{Got1xxResponse: func(_ int, h textproto.MIMEHeader) error {
		hinted = http.Header(h)
		return nil
	}})
	if len(hinted) != 0 {
		t.Errorf("early hint headers reached the client: %v", hinted)
	}
	if status != http.StatusOK || body != "done" {
		t.Errorf("the final response after a hint = %d %q", status, body)
	}
	got := r.audits(5)
	if len(got) < 5 {
		t.Fatalf("audits = %v", got)
	}
	for _, e := range got {
		if e.Path == "/hints" && e.Status != http.StatusOK {
			t.Errorf("the audit recorded the hint's status: %d", e.Status)
		}
	}
}
