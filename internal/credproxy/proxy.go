package credproxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Entry is one audit line: a decrypted request the policy added a
// credential to. It never carries a header value or a body.
type Entry struct {
	Method string
	Host   string
	Path   string
	Status int
	// Route names the routed credential the request got, if any.
	//
	// @decision(DL-072)
	Route string `json:",omitempty"`
}

// Config is what a Proxy needs. Only CA is required.
type Config struct {
	// CA issues the certificates for decrypted hosts.
	CA *CA
	// Decrypt reports whether CONNECT traffic to a target is decrypted.
	// The target is the host, with ":port" added for any port but 443
	// (HostKey). Everything else is tunneled untouched. Nil decrypts
	// nothing.
	Decrypt func(target string) bool
	// Rewrite runs on each decrypted request before it goes upstream,
	// after the client's own credentials have been removed. It may add
	// headers, and reports whether it added a credential. It must not
	// change where the request goes. Nil adds nothing.
	Rewrite func(r *http.Request) bool
	// Secrets are the strings to remove from the response to a request
	// Rewrite added a credential to: the credential in every form an
	// upstream might echo it. Nil scrubs nothing.
	Secrets func(r *http.Request) []string
	// Streamed reports whether a credentialed request's response, when it
	// is an event stream, is passed through without being scrubbed. Nil
	// scrubs every stream.
	//
	// @decision(DL-034)
	Streamed func(r *http.Request) bool
	// Audit receives one Entry per credentialed request.
	Audit func(Entry)
	// RouteName names the routed credential a credentialed request got,
	// for its Entry. Nil, or "", names none.
	RouteName func(r *http.Request) string
	// Dial connects upstream. Nil uses PublicDial.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// UpstreamRoots verifies decrypted upstreams. Nil uses the system
	// roots.
	UpstreamRoots *x509.CertPool
	// Logf reports what failed, such as a client that refused the
	// sandbox CA. Nil discards it.
	Logf func(format string, args ...any)
}

// credentialHeaders are removed from every decrypted request before the
// policy runs, so nothing the client sent is forwarded as a credential.
var credentialHeaders = []string{"Authorization", "Proxy-Authorization", "X-Api-Key", "X-Goog-Api-Key", "Cookie",
	// GitLab's own credential headers, which it reads ahead of
	// Authorization, and Sudo, which picks who a token acts as.
	// @decision(DL-034)
	"Private-Token", "Job-Token", "Deploy-Token", "Sudo"}

// stripCredentialHeaders removes every credential header, including one
// spelled with underscores, which a server behind a CGI-style gateway
// reads as the same header.
//
// @decision(DL-034)
func stripCredentialHeaders(h http.Header) {
	for name := range h {
		if slices.Contains(credentialHeaders, http.CanonicalHeaderKey(strings.ReplaceAll(name, "_", "-"))) {
			delete(h, name)
		}
	}
}

// Proxy is an HTTP proxy. It tunnels CONNECT traffic, decrypts the hosts
// Config.Decrypt names so Config.Rewrite can add credentials, and
// forwards plain-HTTP requests without ever adding one.
//
// @decision(DL-034)
type Proxy struct {
	cfg       Config
	dial      func(ctx context.Context, network, addr string) (net.Conn, error)
	upstream  *http.Transport
	plainHTTP *httputil.ReverseProxy
}

// New builds a Proxy.
func New(cfg Config) (*Proxy, error) {
	if cfg.CA == nil {
		return nil, errors.New("credproxy: a proxy needs a CA")
	}
	p := &Proxy{cfg: cfg, dial: cfg.Dial}
	if p.dial == nil {
		p.dial = PublicDial
	}
	p.upstream = &http.Transport{
		Proxy:                 nil,
		DialContext:           p.dial,
		TLSClientConfig:       &tls.Config{RootCAs: cfg.UpstreamRoots, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 5 * time.Minute,
	}
	p.plainHTTP = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = pr.In.URL
			pr.Out.Host = pr.In.Host
			pr.Out.Header.Del("Proxy-Authorization")
		},
		Transport:     &http.Transport{Proxy: nil, DialContext: p.dial},
		FlushInterval: -1,
	}
	return p, nil
}

func (p *Proxy) logf(format string, args ...any) {
	if p.cfg.Logf != nil {
		p.cfg.Logf(format, args...)
	}
}

// ServeHTTP handles one proxy request.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.connect(w, r)
		return
	}
	// A plain-HTTP proxy request carries an absolute URL. It is
	// forwarded as it is and never gets a credential.
	if r.URL.Scheme != "http" || r.URL.Host == "" {
		http.Error(w, "credproxy: only CONNECT and absolute http:// requests are proxied", http.StatusBadRequest)
		return
	}
	p.plainHTTP.ServeHTTP(w, r)
}

// connect answers CONNECT: it decrypts a host the policy names on port
// 443 and tunnels everything else.
func (p *Proxy) connect(w http.ResponseWriter, r *http.Request) {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil || host == "" || port == "" {
		http.Error(w, "credproxy: CONNECT needs host:port", http.StatusBadRequest)
		return
	}
	host = normalHost(host)
	if target := HostKey(host, port); p.cfg.Decrypt != nil && p.cfg.Decrypt(target) {
		conn := hijack(w, "200 Connection Established")
		if conn == nil {
			return
		}
		p.decrypt(conn, host, target)
		return
	}
	upstream, err := p.dial(r.Context(), "tcp", net.JoinHostPort(host, port))
	if err != nil {
		p.logf("tunnel %s:%s: %v", host, port, err)
		http.Error(w, "credproxy: "+err.Error(), http.StatusBadGateway)
		return
	}
	conn := hijack(w, "200 Connection Established")
	if conn == nil {
		_ = upstream.Close()
		return
	}
	splice(conn, upstream)
}

// decrypt terminates TLS for host with a certificate from the CA, and
// serves the requests inside it one at a time.
//
// @decision(DL-034)
func (p *Proxy) decrypt(conn net.Conn, host, target string) {
	tconn := tls.Server(conn, &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if hello.ServerName != "" && normalHost(hello.ServerName) != host {
				return nil, fmt.Errorf("credproxy: SNI %q inside CONNECT %s", hello.ServerName, host)
			}
			return p.cfg.CA.Leaf(host)
		},
	})
	hctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err := tconn.HandshakeContext(hctx)
	cancel()
	if err != nil {
		p.logf("decrypt %s: client handshake: %v", host, err)
		_ = tconn.Close()
		return
	}
	srv := &http.Server{
		Handler:           p.forward(target),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          nil,
	}
	_ = srv.Serve(newOneConnListener(tconn))
}

// forward serves the requests of one decrypted connection to target.
func (p *Proxy) forward(target string) http.Handler {
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "https"
			pr.Out.URL.Host = target
			pr.Out.Host = target
		},
		Transport:      p.upstream,
		FlushInterval:  -1,
		ModifyResponse: scrubResponse,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if RequestKey(r.Host) != target {
			http.Error(w, "credproxy: Host does not match the CONNECT target", http.StatusMisdirectedRequest)
			return
		}
		stripCredentialHeaders(r.Header)
		credentialed := p.cfg.Rewrite != nil && p.cfg.Rewrite(r)
		if credentialed && p.cfg.Secrets != nil {
			// Without the client's Accept-Encoding the transport asks for
			// gzip itself and hands back the body decoded, so a secret in
			// it can be found.
			r.Header.Del("Accept-Encoding")
			sc := scrub{secrets: p.cfg.Secrets(r), streams: p.cfg.Streamed != nil && p.cfg.Streamed(r)}
			r = r.WithContext(context.WithValue(r.Context(), scrubKey{}, sc))
		}
		if !credentialed || p.cfg.Audit == nil {
			rp.ServeHTTP(w, r)
			return
		}
		sw := &statusWriter{ResponseWriter: w}
		rp.ServeHTTP(sw, r)
		e := Entry{Method: r.Method, Host: target, Path: r.URL.Path, Status: sw.status()}
		if p.cfg.RouteName != nil {
			e.Route = p.cfg.RouteName(r)
		}
		p.cfg.Audit(e)
	})
}

// scrubKey carries a credentialed request's scrub to scrubResponse.
type scrubKey struct{}

// scrub is what scrubResponse removes from a response, and whether an
// event stream passes through unread.
type scrub struct {
	secrets []string
	streams bool
}

// MaxScrubbed is the largest response to a credentialed request the proxy
// reads to remove its secrets; a larger one is refused.
const MaxScrubbed = 32 << 20

// scrubbed is what replaces a secret an upstream echoed.
const scrubbed = "spov-redacted"

// scrubResponse removes the credential from a response to a request that
// carried it, since an upstream can echo it: Forgejo's LFS batch API puts
// the request's Authorization header into every action it returns. git's
// own pack and binary object transfers are passed through unread. A body
// that cannot be read in full, or arrives still encoded, is refused. A
// model provider's event stream has its headers scrubbed only.
//
// @decision(DL-034)
func scrubResponse(resp *http.Response) error {
	sc, _ := resp.Request.Context().Value(scrubKey{}).(scrub)
	secrets := sc.secrets
	// A 101's body is the upgraded connection, which the proxy splices.
	if len(secrets) == 0 || resp.StatusCode == http.StatusSwitchingProtocols {
		return nil
	}
	for k, vs := range resp.Header {
		for i, v := range vs {
			for _, sec := range secrets {
				if sec != "" {
					v = strings.ReplaceAll(v, sec, scrubbed)
				}
			}
			resp.Header[k][i] = v
		}
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.HasPrefix(ct, "application/x-git-") || strings.HasPrefix(ct, "application/octet-stream") ||
		(sc.streams && strings.HasPrefix(ct, "text/event-stream")) {
		return nil
	}
	if resp.Header.Get("Content-Encoding") != "" {
		return errors.New("credproxy: an encoded response to a credentialed request cannot be checked")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxScrubbed+1))
	_ = resp.Body.Close()
	if err != nil {
		return fmt.Errorf("credproxy: reading a response to scrub: %w", err)
	}
	if len(body) > MaxScrubbed {
		return errors.New("credproxy: a response to a credentialed request is too large to check")
	}
	for _, s := range secrets {
		if s != "" {
			body = bytes.ReplaceAll(body, []byte(s), []byte(scrubbed))
		}
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return nil
}

// PublicDial dials a public address only. It resolves the host itself
// and dials the address it checked, so a name cannot resolve to one
// address for the check and another for the dial. Loopback, private,
// link-local and unspecified addresses are refused, which covers the
// main container, the proxy itself and the cloud metadata address.
//
// @decision(DL-034)
func PublicDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	var lastErr error
	for _, ip := range ips {
		if !Public(ip.IP) {
			lastErr = fmt.Errorf("%s resolves to %s, which is not a public address", host, ip.IP)
			continue
		}
		conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("%s has no address", host)
	}
	return nil, lastErr
}

// Public reports whether ip is an address the proxy may dial.
func Public(ip net.IP) bool {
	return !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() &&
		!ip.IsLinkLocalMulticast() && !ip.IsUnspecified() && !ip.IsMulticast()
}

// HostKey is how the proxy names a target: the host, with ":port" added
// for any port but 443.
func HostKey(host, port string) string {
	host = normalHost(host)
	if port == "" || port == "443" {
		return host
	}
	return host + ":" + port
}

// RequestKey is HostKey for a request's Host header, whose port is 443
// when it names none.
func RequestKey(hostHeader string) string {
	host, port, err := net.SplitHostPort(hostHeader)
	if err != nil {
		return HostKey(hostHeader, "")
	}
	return HostKey(host, port)
}

// bareHost is a target without its port.
func bareHost(target string) string {
	if host, _, err := net.SplitHostPort(target); err == nil {
		return host
	}
	return target
}

func normalHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(h), ".")
}

// hijack takes over the client connection and writes the status line.
// It answers the client itself and returns nil when it cannot.
func hijack(w http.ResponseWriter, status string) net.Conn {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "credproxy: cannot take over the connection", http.StatusInternalServerError)
		return nil
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil
	}
	if _, err := io.WriteString(conn, "HTTP/1.1 "+status+"\r\n\r\n"); err != nil {
		_ = conn.Close()
		return nil
	}
	return &bufferedConn{Conn: conn, r: rw.Reader}
}

// splice copies both ways until either side is done, then closes both.
func splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	_ = a.Close()
	_ = b.Close()
	<-done
}

// bufferedConn reads what the server had buffered before the hijack.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// oneConnListener hands out one connection, then blocks until it closes,
// so http.Server serves exactly that connection for its whole life.
type oneConnListener struct {
	conn   net.Conn
	once   sync.Once
	closed chan struct{}
}

func newOneConnListener(c net.Conn) *oneConnListener {
	l := &oneConnListener{closed: make(chan struct{})}
	l.conn = &notifyConn{Conn: c, closed: l.closed}
	return l
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	var c net.Conn
	l.once.Do(func() { c = l.conn })
	if c != nil {
		return c, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}

func (l *oneConnListener) Close() error   { return nil }
func (l *oneConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

type notifyConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (c *notifyConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.closed) })
	return err
}

// statusWriter remembers the status the upstream answered.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.code == 0 {
		s.code = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.code == 0 {
		s.code = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusWriter) status() int {
	if s.code == 0 {
		return http.StatusOK
	}
	return s.code
}
