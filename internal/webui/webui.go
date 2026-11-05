// Package webui serves the built web app from inside the server binary.
//
// The bytes are embedded, so there is one artifact, one container and
// one origin; the app and the API it calls ship from the same commit.
//
// @decision(DL-055)
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// dist holds the built app. A plain `go build` embeds only the
// placeholder marker, so the Go toolchain never depends on Bun; CI,
// release and image builds run scripts/build-web.sh first, which copies
// a real build in over it.
//
// @decision(DL-055)
//
//go:embed all:dist
var dist embed.FS

const (
	// IndexFile is what a directory resolves to, and the file whose
	// presence distinguishes a real build from a placeholder one.
	IndexFile = "index.html"
	// NotFoundFile is Astro's static 404 page.
	NotFoundFile = "404.html"
	// AssetPrefix is where Astro writes content-hashed files. Their
	// names change whenever their contents do, so they can be cached
	// forever; nothing else can.
	AssetPrefix = "_astro/"

	immutableCache = "public, max-age=31536000, immutable"
	revalidate     = "no-cache"
)

// Dist is the embedded app.
func Dist() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		// Unreachable: the directory is embedded above, and fs.Sub
		// only fails on an invalid name.
		panic("webui: " + err.Error())
	}
	return sub
}

// Built reports whether fsys carries a real build rather than the
// placeholder. `spov serve` logs when it does not, so a binary built
// without the web app says so at startup instead of at first request.
//
// @decision(DL-055)
func Built(fsys fs.FS) bool {
	f, err := fsys.Open(IndexFile)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// placeholderPage is what a binary built without the web app serves. It
// is Go source rather than a committed dist/index.html so that a local
// `bun run build` cannot clobber a tracked file and get committed by
// accident.
//
// @decision(DL-055)
const placeholderPage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>spov</title></head>
<body style="font-family: system-ui, sans-serif; max-width: 40rem; margin: 4rem auto; padding: 0 1rem">
<h1>This binary was built without the web UI</h1>
<p>The API is running and unaffected. To build the web app into a binary,
run <code>scripts/build-web.sh</code> before <code>go build</code>; the
release and container images do it for you.</p>
</body></html>
`

// Handler serves the app in fsys: exact files, directory paths resolved
// to their index, and anything unknown answered with Astro's 404 page.
//
// Dotfiles are refused as a class, so a build that ever needs to publish
// a /.well-known/… path will have to carve one out here.
//
// @decision(DL-055)
func Handler(fsys fs.FS) http.Handler {
	built := Built(fsys)
	// Read once: this is an unauthenticated public endpoint, and a miss
	// should not cost a file read per request.
	notFoundPage, noNotFoundPage := fs.ReadFile(fsys, NotFoundFile)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// This branch runs outside the browser guard, which refuses a
		// framed or sniffed load, so the two headers that matter are
		// set explicitly.
		//
		// img-src keeps a report's images to spov's own origin even if
		// the renderer's own check were bypassed.
		//
		// @decision(DL-055) @decision(DL-063)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'; img-src 'self' data:")
		if !built {
			// Nothing to look up, so the status reflects the address
			// rather than the missing build: only the root is a page
			// this binary claims to have.
			status := http.StatusNotFound
			if r.URL.Path == "/" {
				status = http.StatusOK
			}
			writeHTML(w, r, status, []byte(placeholderPage))
			return
		}
		name, ok := resolve(fsys, r.URL.Path)
		if !ok {
			if noNotFoundPage != nil {
				// A build without a 404 page still has to answer.
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			writeHTML(w, r, http.StatusNotFound, notFoundPage)
			return
		}
		w.Header().Set("Cache-Control", cacheControl(name))
		http.ServeFileFS(w, r, fsys, name)
	})
}

// resolve maps a request path to a file in fsys, or reports that there
// is none. Directories never serve a listing: they resolve to their
// index or to nothing at all.
func resolve(fsys fs.FS, urlPath string) (string, bool) {
	name := path.Clean("/" + urlPath)
	name = strings.TrimPrefix(name, "/")
	if name == "." || name == "" {
		name = IndexFile
	}
	// A dotfile is never part of a build's public surface, and the
	// placeholder marker lives in the same directory. Refusing the
	// whole class also declines the /.env and /.git probes that arrive
	// unprompted on any public address.
	for _, seg := range strings.Split(name, "/") {
		if strings.HasPrefix(seg, ".") {
			return "", false
		}
	}
	if isFile(fsys, name) {
		return name, true
	}
	// Astro's default build format writes /sessions as
	// sessions/index.html.
	if indexed := path.Join(name, IndexFile); isFile(fsys, indexed) {
		return indexed, true
	}
	return "", false
}

func isFile(fsys fs.FS, name string) bool {
	info, err := fs.Stat(fsys, name)
	return err == nil && info.Mode().IsRegular()
}

// writeHTML answers with a page this package holds rather than a file.
// The body is written even for HEAD: net/http discards it, and returning
// early instead would drop Content-Length and make HEAD and GET disagree.
func writeHTML(w http.ResponseWriter, _ *http.Request, status int, body []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", revalidate)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// cacheControl caches hashed assets forever and revalidates everything
// else. Getting this backwards serves a stale page against a new API.
func cacheControl(name string) string {
	if strings.HasPrefix(name, AssetPrefix) {
		return immutableCache
	}
	return revalidate
}
