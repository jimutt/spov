package webui_test

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jimutt/spov/internal/webui"
)

// builtFS is what a real Astro build looks like to the handler.
func builtFS() fs.FS {
	return fstest.MapFS{
		"index.html":                 {Data: []byte("<h1>home</h1>")},
		"404.html":                   {Data: []byte("<h1>not found</h1>")},
		"sessions/index.html":        {Data: []byte("<h1>sessions</h1>")},
		"_astro/Base.DMORK3dP.css":   {Data: []byte("body{}")},
		"_astro/client.DyG4EDnk.js":  {Data: []byte("export{}")},
		"favicon.svg":                {Data: []byte("<svg/>")},
		".gitkeep":                   {Data: []byte("")},
		"sessions/detail/index.html": {Data: []byte("<h1>detail</h1>")},
	}
}

// response is what a test needs from a finished response: the body is
// already read and closed.
type response struct {
	StatusCode int
	Header     http.Header
	Body       string
}

func get(t *testing.T, h http.Handler, method, target string) response {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	res := rec.Result()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := res.Body.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}
	return response{StatusCode: res.StatusCode, Header: res.Header, Body: string(b)}
}

func TestHandlerServesTheBuild(t *testing.T) {
	h := webui.Handler(builtFS())

	tests := []struct {
		name   string
		path   string
		status int
		want   string
		cache  string
	}{
		{"root is the index", "/", http.StatusOK, "<h1>home</h1>", "no-cache"},
		{"a directory resolves to its index", "/sessions", http.StatusOK, "<h1>sessions</h1>", "no-cache"},
		{"a trailing slash resolves too", "/sessions/", http.StatusOK, "<h1>sessions</h1>", "no-cache"},
		{"a nested directory resolves", "/sessions/detail", http.StatusOK, "<h1>detail</h1>", "no-cache"},
		{"an exact file is served", "/favicon.svg", http.StatusOK, "<svg/>", "no-cache"},
		{"a hashed asset is immutable", "/_astro/Base.DMORK3dP.css", http.StatusOK, "body{}", "public, max-age=31536000, immutable"},
		{"an unknown path gets the 404 page", "/nope", http.StatusNotFound, "<h1>not found</h1>", "no-cache"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := get(t, h, http.MethodGet, tc.path)
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			if got := resp.Body; got != tc.want {
				t.Errorf("body = %q, want %q", got, tc.want)
			}
			if got := resp.Header.Get("Cache-Control"); got != tc.cache {
				t.Errorf("Cache-Control = %q, want %q", got, tc.cache)
			}
		})
	}
}

// A directory listing leaks the whole asset manifest, and http.FileServer
// serves one by default.
func TestHandlerNeverListsADirectory(t *testing.T) {
	h := webui.Handler(builtFS())
	for _, p := range []string{"/_astro/", "/_astro"} {
		resp := get(t, h, http.MethodGet, p)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404", p, resp.StatusCode)
		}
		if got := resp.Body; strings.Contains(got, "Base.DMORK3dP.css") {
			t.Errorf("%s: listed the directory: %q", p, got)
		}
	}
}

// TestSecurityHeaders: these paths are served outside the browser guard,
// which would refuse a framed or sniffed load, so the framing and
// sniffing protection is set explicitly.
//
// @decision(DL-055)
func TestSecurityHeaders(t *testing.T) {
	h := webui.Handler(builtFS())
	for _, p := range []string{"/", "/nope", "/_astro/Base.DMORK3dP.css"} {
		resp := get(t, h, http.MethodGet, p)
		if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q", p, got)
		}
		if got := resp.Header.Get("Content-Security-Policy"); got != "frame-ancestors 'none'; img-src 'self' data:" {
			t.Errorf("%s: Content-Security-Policy = %q", p, got)
		}
	}
	// The placeholder is served outside the guard too.
	resp := get(t, webui.Handler(fstest.MapFS{".gitkeep": {}}), http.MethodGet, "/")
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("placeholder: X-Content-Type-Options = %q", got)
	}
}

// TestHandlerOddPaths pins what http.ServeFileFS does with two shapes
// the handler passes through to it, so a stdlib change is a test failure
// rather than a surprise. Neither leaks anything; both are just not what
// a reader would guess.
//
// @decision(DL-055)
func TestHandlerOddPaths(t *testing.T) {
	h := webui.Handler(builtFS())

	// serveFile redirects any URL ending in /index.html to ./.
	resp := get(t, h, http.MethodGet, "/index.html")
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Errorf("/index.html = %d, want 301", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "./" {
		t.Errorf("/index.html Location = %q", got)
	}

	// ServeFileFS refuses any URL with a .. element, after resolve has
	// already cleaned it away and found a real file.
	resp = get(t, h, http.MethodGet, "/sessions/../favicon.svg")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a path with .. = %d, want 400", resp.StatusCode)
	}
	if strings.Contains(resp.Body, "<svg/>") {
		t.Error("a path with .. served the file")
	}
}

func TestHandlerRefusesDotfiles(t *testing.T) {
	h := webui.Handler(builtFS())
	for _, p := range []string{"/.gitkeep", "/sessions/../.gitkeep"} {
		resp := get(t, h, http.MethodGet, p)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", p, resp.StatusCode)
		}
	}
}

func TestHandlerRefusesWrites(t *testing.T) {
	h := webui.Handler(builtFS())
	resp := get(t, h, http.MethodPost, "/")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
	if got := resp.Header.Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q", got)
	}
}

func TestHandlerHead(t *testing.T) {
	h := webui.Handler(builtFS())
	resp := get(t, h, http.MethodHead, "/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Body; got != "" {
		t.Errorf("HEAD returned a body: %q", got)
	}
}

// A build with no 404.html must still answer an unknown path.
func TestHandlerWithoutA404Page(t *testing.T) {
	h := webui.Handler(fstest.MapFS{"index.html": {Data: []byte("<h1>home</h1>")}})
	resp := get(t, h, http.MethodGet, "/nope")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestPlaceholder(t *testing.T) {
	empty := fstest.MapFS{".gitkeep": {Data: []byte("")}}
	if webui.Built(empty) {
		t.Fatal("Built reported a placeholder as a real build")
	}
	if !webui.Built(builtFS()) {
		t.Fatal("Built did not recognise a real build")
	}

	h := webui.Handler(empty)

	resp := get(t, h, http.MethodGet, "/")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("root status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Body; !strings.Contains(got, "without the web UI") {
		t.Errorf("root body does not explain itself: %q", got)
	}

	// Any other address is still not a page this binary has.
	resp = get(t, h, http.MethodGet, "/sessions")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown path status = %d, want 404", resp.StatusCode)
	}
}

// The embedded copy must be servable, whether or not a build was run
// before `go test`.
func TestDist(t *testing.T) {
	fsys := webui.Dist()
	if _, err := fs.Stat(fsys, "."); err != nil {
		t.Fatalf("embedded dist is not a directory: %v", err)
	}
	resp := get(t, webui.Handler(fsys), http.MethodGet, "/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("root status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Body; got == "" {
		t.Error("root served an empty body")
	}
}
