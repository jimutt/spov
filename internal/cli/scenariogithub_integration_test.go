//go:build integration

package cli

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// scenarioGitHub is the GitHub API the combined scenarios run against
// (DL-022): one organisation with an App installed on some of its
// repositories and a personal token reaching others. It checks an App's
// JWT signature against the App's current keys, so a wrong or deleted key
// fails as it would on GitHub, and it answers a repository only to a
// credential that reaches it. The real git.GitHub and git.GitHubPAT talk
// to it as a GitHub Enterprise server.
type scenarioGitHub struct {
	t   *testing.T
	srv *httptest.Server
	now func() time.Time

	mu      sync.Mutex
	appID   string
	keys    map[string]*rsa.PublicKey // the App's live keys, by name
	org     string
	install int64
	// appRepos are the installation's repositories; pats maps each
	// personal token to the repositories it reaches.
	appRepos []string
	pats     map[string][]string
	tokens   map[string]*ghsToken
	// mints logs every installation token: the key that signed for it
	// and the repositories it was narrowed to.
	mints []ghsMint
	pulls map[string]int // "repo\x00branch" -> number
	// ended are pull requests no longer open: closed, or merged.
	ended map[string]ghsEnded
}

type ghsEnded struct {
	number int
	merged bool
}

type ghsToken struct {
	repos   []string // empty: every installation repository
	revoked bool
}

type ghsMint struct {
	Key    string
	Secret string
	Repos  []string
}

func newScenarioGitHub(t *testing.T, now func() time.Time) *scenarioGitHub {
	t.Helper()
	g := &scenarioGitHub{
		t: t, now: now, appID: "424242", keys: map[string]*rsa.PublicKey{},
		org: "acme", install: 9191, pats: map[string][]string{},
		tokens: map[string]*ghsToken{}, pulls: map[string]int{}, ended: map[string]ghsEnded{},
	}
	// TLS, since a clone URL must be https. Go's default transport,
	// which the GitHub clients use, trusts only this server meanwhile, so
	// nothing in the run reaches the real internet either.
	g.srv = httptest.NewTLSServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(g.srv.Certificate())
	tr := http.DefaultTransport.(*http.Transport)
	old := tr.TLSClientConfig
	tr.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	t.Cleanup(func() {
		tr.TLSClientConfig = old
		tr.CloseIdleConnections()
	})
	return g
}

// newKey makes a key for the App, live until deleteKey, and returns its
// PEM as GitHub hands it out.
func (g *scenarioGitHub) newKey(t *testing.T, name string) string {
	t.Helper()
	pemKey, pub := rsaKey(t)
	g.mu.Lock()
	g.keys[name] = pub
	g.mu.Unlock()
	return pemKey
}

// rsaKey is a new private key as GitHub hands one out, and its public key.
func rsaKey(t *testing.T) (string, *rsa.PublicKey) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})), &k.PublicKey
}

func (g *scenarioGitHub) deleteKey(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.keys, name)
}

func (g *scenarioGitHub) revokePAT(token string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.pats, token)
}

func (g *scenarioGitHub) mintLog() []ghsMint {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.mints)
}

func (g *scenarioGitHub) openPull(repo, branch string, number int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pulls[repo+"\x00"+branch] = number
}

// closePull closes the pull request from branch without merging it, and
// mergePull merges it; a lookup in every state still finds either.
func (g *scenarioGitHub) closePull(repo, branch string) { g.endPull(repo, branch, false) }

func (g *scenarioGitHub) mergePull(repo, branch string) { g.endPull(repo, branch, true) }

func (g *scenarioGitHub) endPull(repo, branch string, merged bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	key := repo + "\x00" + branch
	g.ended[key] = ghsEnded{number: g.pulls[key], merged: merged}
	delete(g.pulls, key)
}

// installRepo adds a repository to the App's installation.
func (g *scenarioGitHub) installRepo(repo string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.appRepos = append(g.appRepos, repo)
}

// secrets are every installation token minted so far, for the scan.
func (g *scenarioGitHub) secrets() []string {
	var out []string
	for _, m := range g.mintLog() {
		out = append(out, m.Secret)
	}
	return out
}

func (g *scenarioGitHub) serve(w http.ResponseWriter, r *http.Request) {
	path, ok := strings.CutPrefix(r.URL.Path, "/api/v3")
	if !ok {
		http.NotFound(w, r)
		return
	}
	authz := r.Header.Get("Authorization")
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case path == "/users/"+g.org+"/installation" || path == "/orgs/"+g.org+"/installation":
		if _, ok := g.app(w, authz); ok {
			ghJSON(w, map[string]any{"id": g.install})
		}
	case strings.HasPrefix(path, "/users/") || strings.HasPrefix(path, "/orgs/"):
		if _, ok := g.app(w, authz); ok {
			http.NotFound(w, r)
		}
	case path == fmt.Sprintf("/app/installations/%d", g.install):
		if _, ok := g.app(w, authz); ok {
			ghJSON(w, map[string]any{
				"id": g.install, "account": map[string]string{"login": g.org},
				"permissions": map[string]string{"contents": "write", "pull_requests": "write", "issues": "write", "metadata": "read"},
			})
		}
	case path == fmt.Sprintf("/app/installations/%d/access_tokens", g.install) && r.Method == http.MethodPost:
		key, ok := g.app(w, authz)
		if !ok {
			return
		}
		var body struct {
			Repositories []string `json:"repositories"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, name := range body.Repositories {
			if !slices.Contains(g.appRepos, name) {
				http.Error(w, `{"message":"There is at least one repository that does not exist or is not accessible to the parent installation."}`, http.StatusUnprocessableEntity)
				return
			}
		}
		secret := "ghs_" + randomAlnum(36)
		g.tokens[secret] = &ghsToken{repos: body.Repositories}
		g.mints = append(g.mints, ghsMint{Key: key, Secret: secret, Repos: body.Repositories})
		w.WriteHeader(http.StatusCreated)
		ghJSON(w, map[string]string{"token": secret, "expires_at": g.now().Add(time.Hour).UTC().Format(time.RFC3339)})
	case path == "/installation/repositories":
		tok := g.installationToken(authz)
		if tok == nil {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		names := g.appRepos
		if len(tok.repos) > 0 {
			names = tok.repos
		}
		ghJSON(w, map[string]any{"total_count": len(names), "repositories": g.repoJSON(names)})
	case path == "/installation/token" && r.Method == http.MethodDelete:
		if tok := g.installationToken(authz); tok != nil {
			tok.revoked = true
		}
		w.WriteHeader(http.StatusNoContent)
	case path == "/user":
		if _, ok := g.pats[bearerOf(authz)]; !ok {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		ghJSON(w, map[string]string{"login": "member-gh"})
	case path == "/user/repos":
		repos, ok := g.pats[bearerOf(authz)]
		if !ok {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("page") != "1" && r.URL.Query().Get("page") != "" {
			repos = nil
		}
		ghJSON(w, g.repoJSON(repos))
	case strings.HasPrefix(path, "/repos/"+g.org+"/") && !strings.Contains(strings.TrimPrefix(path, "/repos/"+g.org+"/"), "/"):
		// One repository, as a launch's reach check reads it.
		repo := strings.TrimPrefix(path, "/repos/"+g.org+"/")
		if !g.reaches(authz, repo) {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		ghJSON(w, g.repoJSON([]string{repo})[0])
	case strings.HasPrefix(path, "/repos/"+g.org+"/") && strings.HasSuffix(path, "/pulls"):
		repo := strings.TrimSuffix(strings.TrimPrefix(path, "/repos/"+g.org+"/"), "/pulls")
		if _, pat := g.pats[bearerOf(authz)]; !pat && g.installationToken(authz) == nil {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		if !g.reaches(authz, repo) {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		branch := strings.TrimPrefix(r.URL.Query().Get("head"), g.org+":")
		key := repo + "\x00" + branch
		out := []map[string]any{}
		pull := func(n int, state string, mergedAt any) map[string]any {
			return map[string]any{"number": n, "state": state, "title": "scenario", "merged_at": mergedAt,
				"html_url": fmt.Sprintf("%s/%s/%s/pull/%d", g.srv.URL, g.org, repo, n)}
		}
		if n, ok := g.pulls[key]; ok {
			out = append(out, pull(n, "open", nil))
		}
		if e, ok := g.ended[key]; ok && r.URL.Query().Get("state") == "all" {
			var at any
			if e.merged {
				at = g.now().Format(time.RFC3339)
			}
			out = append(out, pull(e.number, "closed", at))
		}
		ghJSON(w, out)
	default:
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}
}

// app checks an App JWT: a live key's signature, the App's ID, and that
// it is current by the wall clock, which is what spov signs with. It
// names the key that signed, or answers 401 as GitHub does.
func (g *scenarioGitHub) app(w http.ResponseWriter, authz string) (string, bool) {
	jwt, ok := strings.CutPrefix(authz, "Bearer ")
	parts := strings.Split(jwt, ".")
	if ok && len(parts) == 3 {
		var claims struct {
			Iss any   `json:"iss"`
			Iat int64 `json:"iat"`
			Exp int64 `json:"exp"`
		}
		payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		now := time.Now().Unix()
		if json.Unmarshal(payload, &claims) == nil && fmt.Sprint(claims.Iss) == g.appID &&
			claims.Iat <= now+60 && claims.Exp > now {
			sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
			for name, k := range g.keys {
				if rsa.VerifyPKCS1v15(k, crypto.SHA256, sum[:], sig) == nil {
					return name, true
				}
			}
		}
	}
	http.Error(w, `{"message":"A JSON web token could not be decoded"}`, http.StatusUnauthorized)
	return "", false
}

func (g *scenarioGitHub) installationToken(authz string) *ghsToken {
	tok := g.tokens[bearerOf(authz)]
	if tok == nil || tok.revoked {
		return nil
	}
	return tok
}

// reaches reports whether the credential sees the repository: an
// installation token narrowed to it (or not narrowed, on an installation
// repository), or a personal token granted it.
func (g *scenarioGitHub) reaches(authz, repo string) bool {
	if tok := g.installationToken(authz); tok != nil {
		if len(tok.repos) == 0 {
			return slices.Contains(g.appRepos, repo)
		}
		return slices.Contains(tok.repos, repo)
	}
	return slices.Contains(g.pats[bearerOf(authz)], repo)
}

func (g *scenarioGitHub) repoJSON(names []string) []map[string]any {
	out := []map[string]any{}
	for i, n := range names {
		out = append(out, map[string]any{
			"name": n, "full_name": g.org + "/" + n, "private": true,
			"owner":      map[string]string{"login": g.org},
			"clone_url":  g.srv.URL + "/" + g.org + "/" + n + ".git",
			"updated_at": time.Date(2026, 10, 1, 0, 0, i, 0, time.UTC),
		})
	}
	return out
}

func bearerOf(authz string) string {
	for _, p := range []string{"token ", "Bearer ", "bearer "} {
		if v, ok := strings.CutPrefix(authz, p); ok {
			return v
		}
	}
	return ""
}

func ghJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func randomAlnum(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}
