//go:build integration

package credproxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestIntegrationSGL1GitLabThroughTheProxy is spike S-GL1: the pinned
// glab and git against a disposable gitlab.com project through the real
// credential-injection policy, locally and without Modal. It creates one
// branch and merge request and removes both. Every decrypted request is
// logged with whether the proxy gave it the token, never with headers or
// bodies. Set SPOV_TEST_GLAB to the pinned binary, or put glab on PATH.
//
//	set -a; source ~/.config/spov-test/gitlab.env; set +a
//	SPOV_TEST_GLAB=/path/to/glab go test -tags integration ./internal/credproxy/ -run SGL1 -v
//
// @decision(DL-034) @decision(DL-041)
func TestIntegrationSGL1GitLabThroughTheProxy(t *testing.T) {
	token := os.Getenv("SPOV_TEST_GITLAB_TOKEN")
	repo := os.Getenv("SPOV_TEST_GITLAB_SUBGROUP_REPO")
	otherRepo := os.Getenv("SPOV_TEST_GITLAB_REPO")
	baseURL := strings.TrimRight(os.Getenv("SPOV_TEST_GITLAB_URL"), "/")
	if baseURL == "" {
		baseURL = "https://gitlab.com"
	}
	if token == "" || repo == "" || otherRepo == "" {
		t.Skip("set SPOV_TEST_GITLAB_TOKEN, _REPO and _SUBGROUP_REPO")
	}
	if baseURL != "https://gitlab.com" {
		t.Skip("S-GL1 currently targets gitlab.com")
	}
	glab := os.Getenv("SPOV_TEST_GLAB")
	if glab == "" {
		glab = "glab"
	}
	if _, err := exec.LookPath(glab); err != nil {
		t.Skip("set SPOV_TEST_GLAB to the pinned glab binary")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	direct := &http.Client{Timeout: 30 * time.Second}
	project := sgl1Project{}
	sgl1API(ctx, t, direct, token, http.MethodGet, baseURL+"/api/v4/projects/"+url.PathEscape(repo), nil, &project)
	if project.ID == 0 || project.DefaultBranch == "" {
		t.Fatalf("project lookup returned no ID or default branch: %+v", project)
	}
	t.Logf("measuring %s (project ID %d, default branch %s)", repo, project.ID, project.DefaultBranch)

	certPEM, keyPEM, err := NewCA([]string{"gitlab.com"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ca, err := LoadCA(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	cfg := SidecarConfig{Platforms: []Platform{{
		Name: "gitlab", Kind: KindGitLab, Host: "gitlab.com", User: "oauth2",
		Token: token, Repos: []string{repo}, ProjectIDs: []string{strconv.FormatInt(project.ID, 10)},
	}}}
	store, err := NewStore(filepath.Join(t.TempDir(), "credentials.json"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	recorder := &sgl1Recorder{}
	proxy, err := New(Config{
		CA: ca, Decrypt: func(host string) bool { return host == "gitlab.com" },
		Rewrite: func(r *http.Request) bool {
			credentialed := store.Rewrite(r)
			recorder.add(r, credentialed)
			return credentialed
		},
		Secrets: store.Secrets,
	})
	if err != nil {
		t.Fatal(err)
	}
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	tmp := t.TempDir()
	caFile := filepath.Join(tmp, "ca.pem")
	if err := os.WriteFile(caFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	glabConfig := filepath.Join(tmp, "glab")
	if err := os.MkdirAll(glabConfig, 0o700); err != nil {
		t.Fatal(err)
	}
	config := "check_update: false\ntelemetry: false\nshow_whats_new: false\nno_prompt: true\n" +
		"git_protocol: https\nhost: 'gitlab.com'\nhosts:\n    'gitlab.com':\n" +
		"        token: 'glpat-spovplaceholder01'\n        api_host: 'gitlab.com'\n" +
		"        api_protocol: https\n        git_protocol: https\n"
	if err := os.WriteFile(filepath.Join(glabConfig, "config.yml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + tmp, "GLAB_CONFIG_DIR=" + glabConfig,
		"HTTPS_PROXY=" + proxyServer.URL, "https_proxy=" + proxyServer.URL,
		"HTTP_PROXY=" + proxyServer.URL, "http_proxy=" + proxyServer.URL,
		"SSL_CERT_FILE=" + caFile, "GIT_SSL_CAINFO=" + caFile,
		"NO_PROXY=", "no_proxy=", "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1",
	}
	run := func(dir, name string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		text := strings.ReplaceAll(string(out), token, "<redacted>")
		if err != nil {
			t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, text)
		}
		return text
	}

	clone := filepath.Join(tmp, "repo")
	run(tmp, "git", "clone", "--", baseURL+"/"+repo+".git", clone)
	branch := "spov-gl1-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cleanupCancel()
		sgl1Cleanup(cleanupCtx, t, direct, token, baseURL, project.ID, branch)
	})
	run(clone, "git", "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(clone, "SPOV_GL1.md"), []byte("Disposable S-GL1 proxy measurement.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(clone, "git", "add", "SPOV_GL1.md")
	run(clone, "git", "-c", "user.name=spov S-GL1", "-c", "user.email=spov-gl1@localhost", "commit", "-m", "Measure the GitLab proxy")
	run(clone, "git", "push", "-u", "origin", branch)

	run(clone, glab, "repo", "view")
	t.Log("glab repo view succeeded")
	run(clone, glab, "mr", "list")
	t.Log("glab mr list succeeded")
	out := run(clone, glab, "mr", "create", "--source-branch", branch,
		"--target-branch", project.DefaultBranch, "--title", "S-GL1 proxy measurement",
		"--description", "Disposable merge request created by the local proxy spike.", "--yes")
	t.Logf("glab mr create:\n%s", out)
	if !regexp.MustCompile(`/merge_requests/[0-9]+`).MatchString(out) {
		t.Errorf("glab did not print a merge request URL")
	}

	// A disallowed write names an IID that cannot exist, so even a policy
	// regression cannot merge, approve or delete a real merge request.
	const missingMR = "2147483647"
	api := "/api/v4/projects/" + strconv.FormatInt(project.ID, 10)
	for _, check := range []struct{ method, path string }{
		{http.MethodGet, api + "/members"},
		{http.MethodPost, api + "/merge_requests/" + missingMR + "/merge"},
		{http.MethodPut, api + "/merge_requests/" + missingMR + "/approve"},
		{http.MethodDelete, api + "/merge_requests/" + missingMR},
		{http.MethodGet, "/api/v4/projects/" + url.PathEscape(otherRepo)},
	} {
		before := recorder.count()
		sgl1ProxyRequest(ctx, t, proxyServer.URL, certPEM, check.method, baseURL+check.path)
		entry, ok := recorder.at(before)
		if !ok {
			t.Errorf("%s %s produced no decrypted request", check.method, check.path)
		} else if entry.Credentialed {
			t.Errorf("%s %s carried the token", check.method, check.path)
		}
	}

	for _, want := range []struct{ method, fragment string }{
		{http.MethodGet, "/info/refs"},
		{http.MethodPost, "/git-receive-pack"},
		{http.MethodPost, "/merge_requests"},
	} {
		if !slices.ContainsFunc(recorder.all(), func(e sgl1Entry) bool {
			return e.Credentialed && e.Method == want.method && strings.Contains(e.Path, want.fragment)
		}) {
			t.Errorf("no Credentialed %s request containing %s", want.method, want.fragment)
		}
	}
	for _, entry := range recorder.all() {
		t.Logf("%s %s credential=%t", entry.Method, entry.Path, entry.Credentialed)
	}
	if audit := os.Getenv("SPOV_TEST_GL1_AUDIT"); audit != "" {
		if err := os.WriteFile(audit, recorder.jsonl(), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("request audit written to %s", audit)
	}
}

type sgl1Project struct {
	ID            int64  `json:"id"`
	DefaultBranch string `json:"default_branch"`
}

type sgl1Entry struct {
	Method       string `json:"method"`
	Host         string `json:"host"`
	Path         string `json:"path"`
	Credentialed bool   `json:"credentialed"`
}

type sgl1Recorder struct {
	mu      sync.Mutex
	entries []sgl1Entry
}

func (r *sgl1Recorder) add(req *http.Request, credentialed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, sgl1Entry{req.Method, req.Host, req.URL.EscapedPath(), credentialed})
}

func (r *sgl1Recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

func (r *sgl1Recorder) at(i int) (sgl1Entry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i >= len(r.entries) {
		return sgl1Entry{}, false
	}
	return r.entries[i], true
}

func (r *sgl1Recorder) all() []sgl1Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.entries)
}

func (r *sgl1Recorder) jsonl() []byte {
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	for _, e := range r.all() {
		_ = enc.Encode(e)
	}
	return out.Bytes()
}

func sgl1ProxyRequest(ctx context.Context, t *testing.T, proxyURL string, certPEM []byte, method, rawURL string) {
	t.Helper()
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certPEM) {
		t.Fatal("could not read the proxy CA")
	}
	pu, err := url.Parse(proxyURL)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(pu), TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
	}}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, rawURL, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

func sgl1API(ctx context.Context, t *testing.T, client *http.Client, token, method, rawURL string, body any, out any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode >= 300 {
		t.Fatalf("%s %s: %s: %s", method, rawURL, resp.Status, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("decode %s: %v", rawURL, err)
		}
	}
}

func sgl1Cleanup(ctx context.Context, t *testing.T, client *http.Client, token, baseURL string, project int64, branch string) {
	t.Helper()
	api := baseURL + "/api/v4/projects/" + strconv.FormatInt(project, 10)
	var mrs []struct {
		IID int `json:"iid"`
	}
	sgl1API(ctx, t, client, token, http.MethodGet, api+"/merge_requests?state=opened&source_branch="+url.QueryEscape(branch), nil, &mrs)
	for _, mr := range mrs {
		sgl1API(ctx, t, client, token, http.MethodPut, fmt.Sprintf("%s/merge_requests/%d", api, mr.IID), map[string]string{"state_event": "close"}, nil)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, api+"/repository/branches/"+url.PathEscape(branch), nil)
	if err != nil {
		t.Errorf("cleanup branch: %v", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Errorf("cleanup branch: %v", err)
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		t.Errorf("cleanup branch: %s", resp.Status)
	}
}
