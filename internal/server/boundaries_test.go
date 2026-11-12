package server

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Source-level guards for the two walls that no request test can see: the
// store queries that carry no organisation or owner, and the
// authenticators that accept everyone. Each reads the shipped code (never
// a _test.go file) and fails on a call it does not know, so a new caller
// is a decision made in this file.
//
// @decision(DL-006) @decision(DL-003)

// shippedGo parses every non-test Go file under internal/ and cmd/, keyed
// by its slash path from the repository root. skip names directories
// (also by that path) whose files are left out.
func shippedGo(t *testing.T, skip ...string) map[string]*ast.File {
	t.Helper()
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	out := map[string]*ast.File{}
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				for _, s := range skip {
					if rel == s {
						return filepath.SkipDir
					}
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			out[rel] = file
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(out) < 100 {
		t.Fatalf("read only %d source files: is the working directory internal/server?", len(out))
	}
	return out
}

func exprText(t *testing.T, e ast.Expr) string {
	t.Helper()
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, token.NewFileSet(), e); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// unscopedQueries are the store methods that take no organisation or
// owner: a request handler that called one would read across tenants.
// Each is listed with the files that may, and why. A request path
// reaches a row only through Service.Get, Service.GetJob, or a query
// keyed by the caller's own org and user.
var unscopedQueries = map[string]map[string]string{
	"ListLive": {
		"internal/server/imageprune.go":      "background: images in use by live sessions",
		"internal/server/opencodeconsole.go": "background: the renewal pass pushes a refreshed sign-in to live sidecars",
		"internal/server/proxyrecover.go":    "background: the reconciler's sidecar check",
		"internal/server/reconcile.go":       "background: the reconciler",
		"internal/server/recorder.go":        "background: the recorder attaches to live sessions",
		"internal/server/renew.go":           "background: git token renewal",
		"internal/server/metrics.go":         "background: the metrics snapshot",
		"internal/jobs/supervisor_sweep.go":  "background: the supervisor's sweeps",
		"internal/cli/image.go":              "operator command `spov serve image prune`, run at a terminal on the server",
		"internal/cli/serve.go":              "startup: adopting sessions of a restarted local server",
	},
	"BySandbox": {
		"internal/server/reconcile.go": "background: the reconciler maps a sandbox back to its session",
	},
	"ByHash": {
		"internal/auth/tokenauth.go": "the authenticator: a bearer token names no org until it is hashed and found",
		"internal/server/device.go":  "the CLI polling its own device code, found by the hash of a 256-bit secret",
	},
	"ByUserCode": {
		"internal/server/device.go":        "a free user code is chosen here; the row is not returned to anyone",
		"internal/server/deviceapprove.go": "the signed-in human typing the code their terminal showed, rate limited",
	},
	"Snapshots": {
		"internal/server/snapshots.go": "background: the exit-snapshot collection and pruning passes",
	},
}

// TestUnscopedStoreQueriesAreOnlyCalledFromBackgroundPasses scans the
// shipped code for a call to any of them from a file not listed.
//
// @decision(DL-006) @decision(DL-003)
func TestUnscopedStoreQueriesAreOnlyCalledFromBackgroundPasses(t *testing.T) {
	// The implementations and their contract tests are the store's own.
	files := shippedGo(t, "internal/store")
	seen := map[string]map[string]bool{}
	for name, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			allowed, watched := unscopedQueries[sel.Sel.Name]
			if !watched {
				return true
			}
			if seen[sel.Sel.Name] == nil {
				seen[sel.Sel.Name] = map[string]bool{}
			}
			seen[sel.Sel.Name][name] = true
			if _, ok := allowed[name]; !ok {
				t.Errorf("%s calls %s (%s), a store query with no org or owner: a request path must not. If this is a "+
					"background pass or an authenticator, add the file to unscopedQueries in boundaries_test.go with "+
					"the reason; otherwise use a scoped query behind Service.Get or Service.GetJob",
					name, sel.Sel.Name, exprText(t, call.Fun))
			}
			return true
		})
	}
	for method, allowed := range unscopedQueries {
		var stale []string
		for name := range allowed {
			if !seen[method][name] {
				stale = append(stale, name)
			}
		}
		sort.Strings(stale)
		for _, name := range stale {
			t.Errorf("unscopedQueries lists %s for %s, which no longer calls it: remove the entry", name, method)
		}
	}
}

// TestNoShippedCodeBuildsAnAuthenticatorThatAcceptsEveryone: the only
// authenticators that take no credential are the one that refuses
// everyone, and the empty one `spov serve openapi` builds a document with
// and never serves. There is no environment switch that turns
// authentication off.
//
// @decision(DL-006) @decision(DL-005)
func TestNoShippedCodeBuildsAnAuthenticatorThatAcceptsEveryone(t *testing.T) {
	allowedEmpty := map[string]string{
		"internal/cli/serve.go": "spov serve openapi: prints the document and binds no port",
	}
	var constructed []string
	for name, file := range shippedGo(t) {
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || lit.Type == nil || !strings.HasSuffix(exprText(t, lit.Type), "StaticAuthenticator") {
				return true
			}
			refuses := false
			for _, elt := range lit.Elts {
				if kv, ok := elt.(*ast.KeyValueExpr); ok && exprText(t, kv.Key) == "Err" {
					refuses = true
				}
			}
			if refuses {
				return true
			}
			constructed = append(constructed, name)
			if _, ok := allowedEmpty[name]; !ok {
				t.Errorf("%s builds a StaticAuthenticator that accepts every request: shipped code must not", name)
			}
			return true
		})
	}
	sort.Strings(constructed)
	if len(constructed) != len(allowedEmpty) {
		t.Errorf("StaticAuthenticator built without an Err in %v, want only %v", constructed, allowedEmpty)
	}

	// Nor does an environment variable disable authentication: no
	// string in shipped code names one.
	for name, file := range shippedGo(t) {
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			up := strings.ToUpper(lit.Value)
			for _, bad := range []string{"NO_AUTH", "DISABLE_AUTH", "DISABLED_AUTH", "SKIP_AUTH", "AUTH_DISABLED", "AUTH_OFF", "INSECURE_AUTH", "ANONYMOUS"} {
				if strings.Contains(up, bad) {
					t.Errorf("%s has the string %s, which reads as a switch that disables authentication", name, lit.Value)
				}
			}
			return true
		})
	}
}
