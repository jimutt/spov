//go:build integration

package cli

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/store"
)

// TestScenario4 is scenario 4's control-plane layer for the Forgejo
// family (DL-041): the scenario server against a real Forgejo and a real
// Gitea that scripts/forge-test-server.sh runs, each with a member who
// stores their own token, a repository they own with an open pull
// request, and another user's private repository they cannot see. Every
// call spov makes to the forge is the real one: the token check, the
// repository listing, the launch's reach check and gate 2. The sandbox is
// the fake one.
//
//	scripts/forge-test-server.sh start forgejo /tmp/forgejo
//	SPOV_TEST_FORGEJO_ENV=/tmp/forgejo/env SPOV_TEST_DATABASE_URL=... \
//	  go test -tags integration ./internal/cli/ -run TestScenario4 -v
//
// @decision(DL-041) @decision(DL-043) @decision(DL-051)
func TestScenario4(t *testing.T) {
	ran := false
	for _, kind := range []string{"forgejo", "gitea"} {
		path := os.Getenv("SPOV_TEST_" + strings.ToUpper(kind) + "_ENV")
		if path == "" {
			continue
		}
		ran = true
		t.Run(kind, func(t *testing.T) { scenario4On(t, readForgeEnv(t, path)) })
	}
	if !ran {
		t.Skip("set SPOV_TEST_FORGEJO_ENV or SPOV_TEST_GITEA_ENV to a forge-test-server.sh env file")
	}
}

func scenario4On(t *testing.T, env map[string]string) {
	kind, base := env["FORGE_KIND"], env["FORGE_URL"]
	f := &realForge{t: t, base: base, admin: env["FORGE_ADMIN_TOKEN"]}
	// Names of their own, so one forge serves several runs.
	suffix := strings.ToLower(randomAlnum(6))
	member, other := "member-"+suffix, "other-"+suffix

	s := newScenarioServer(t, fmt.Sprintf("  - name: %s\n    kind: %s\n    base_url: %s\n    account: %s\n", kind, kind, base, member))
	// scenarioGitHub's roots are the only ones trusted meanwhile; the
	// forge's certificate joins them.
	ca, err := os.ReadFile(env["FORGE_CA"])
	if err != nil || !http.DefaultTransport.(*http.Transport).TLSClientConfig.RootCAs.AppendCertsFromPEM(ca) {
		t.Fatalf("the forge's certificate %s: %v", env["FORGE_CA"], err)
	}
	memberPass := f.user(member)
	f.user(other)
	token := f.token(member, memberPass)
	alpha := f.repo(member, "alpha")
	f.repo(member, "beta")
	f.repo(other, "secret")
	pr := f.pullRequest(member, "alpha", alpha, "spov/scn4")

	oc := newJobOpenCode(t)
	s.withJobs(oc)
	step := s.steps(t)
	u, _ := url.Parse(base)
	key := server.HostPlatform(kind, u.Host)
	repo := func(owner, name string) []map[string]string {
		return []map[string]string{{"platform": kind, "owner": owner, "name": name}}
	}

	step("4.1 a token the forge refuses is not stored, and the member's own is, checked", func(t *testing.T) {
		if code, raw := s.call("member", http.MethodPut, "/api/v1/me/credentials/"+key,
			map[string]string{"token": strings.Repeat("0", 40)}); code < 400 || code >= 500 {
			t.Errorf("a token the forge refuses: %d %s, want it refused", code, raw)
		}
		var v server.CredentialView
		s.must(http.StatusOK, "member", http.MethodPut, "/api/v1/me/credentials/"+key, map[string]string{"token": token}, &v)
		if !v.Present || v.VerifiedAs != member {
			t.Errorf("stored %+v, want it verified as %s", v, member)
		}
	})

	step("4.2 the listing shows the member's repositories and not another user's private one", func(t *testing.T) {
		var out struct {
			Repos []server.RepoView `json:"repos"`
		}
		s.must(http.StatusOK, "member", http.MethodGet, "/api/v1/repos", nil, &out)
		seen := map[string]bool{}
		for _, r := range out.Repos {
			if r.Platform == kind {
				seen[r.Owner+"/"+r.Name] = true
			}
		}
		if !seen[member+"/alpha"] || !seen[member+"/beta"] || seen[other+"/secret"] {
			t.Errorf("listed %v, want %s's alpha and beta only", seen, member)
		}
	})

	step("4.3 a launch on a repository the token cannot see fails before any sandbox", func(t *testing.T) {
		before := len(s.provider.Handles())
		var created server.SessionView
		s.must(http.StatusAccepted, "member", http.MethodPost, "/api/v1/sessions", map[string]any{
			"name": "scn4-" + suffix, "repos": repo(other, "secret"),
		}, &created)
		var v server.SessionView
		deadline := time.Now().Add(20 * time.Second)
		for {
			s.must(http.StatusOK, "member", http.MethodGet, "/api/v1/sessions/"+created.ID, nil, &v)
			if v.State != store.StateStarting || time.Now().After(deadline) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if v.State != store.StateFailed || !strings.Contains(v.Error, other+"/secret") {
			t.Errorf("the launch on %s/secret is %s (%q), want it failed naming the repository", other, v.State, v.Error)
		}
		if n := len(s.provider.Handles()) - before; n != 0 {
			t.Errorf("the refused launch made %d sandboxes", n)
		}
	})

	step("4.4 gate 2 asks the forge: the open pull request is verified, a branch without one is missing", func(t *testing.T) {
		created := s.createJob(map[string]any{"kind": "pr", "goal": "Add scn4.md.", "repos": repo(member, "alpha")})
		j := s.passUntil(created.ID, store.JobRunning)
		s.worked(j)
		rec := func(branch string) string {
			return `{"event":"` + opencode.DeliverableEvent + `","platform":"` + kind + `","repository":"` +
				member + `/alpha","branch":"` + branch + `"}`
		}
		oc.evidence(rec("spov/scn4"), rec("spov/none"), `{"event":"completion.declared","summary":"two"}`)
		s.stopped(j)
		s.passUntilDone(created.ID)
		v := s.job(created.ID)
		if v.State != store.JobSucceeded || !v.Partial {
			t.Fatalf("state %s, partial %v (%q), want a partial success", v.State, v.Partial, v.Error)
		}
		if g := gate(v, jobs.GateDeliverable); !strings.Contains(g.Detail, "1 of 2 pull requests verified") {
			t.Errorf("gate 2 = %+v", g)
		}
		for _, d := range v.Deliverables {
			switch d.Branch {
			case "spov/scn4":
				if d.Status != store.DeliverableVerified || d.Number != pr || d.Forge != kind || !strings.HasSuffix(d.URL, fmt.Sprintf("/pulls/%d", pr)) {
					t.Errorf("the open one: %+v, want #%d verified", d, pr)
				}
			case "spov/none":
				if d.Status != store.DeliverableMissing {
					t.Errorf("the branch without a pull request: %+v", d)
				}
			}
		}
	})

	step("4.4b a merged pull request is verified, and one closed without merging is missing", func(t *testing.T) {
		// Opened during the job, as the agent would: one opened before it
		// would be an earlier job's on a reused branch name (DL-051).
		created := s.createJob(map[string]any{"kind": "pr", "goal": "Two changes.", "repos": repo(member, "alpha")})
		j := s.passUntil(created.ID, store.JobRunning)
		s.worked(j)
		merged := f.pullRequest(member, "alpha", alpha, "spov/merged")
		f.do(http.MethodPost, fmt.Sprintf("/repos/%s/alpha/pulls/%d/merge", member, merged), f.asAdmin(), map[string]any{"Do": "merge"}, nil)
		closed := f.pullRequest(member, "alpha", alpha, "spov/closed")
		f.do(http.MethodPatch, fmt.Sprintf("/repos/%s/alpha/pulls/%d", member, closed), f.asAdmin(), map[string]any{"state": "closed"}, nil)
		oc.reset()
		rec := func(branch string) string {
			return `{"event":"` + opencode.DeliverableEvent + `","platform":"` + kind + `","repository":"` +
				member + `/alpha","branch":"` + branch + `"}`
		}
		oc.evidence(rec("spov/merged"), rec("spov/closed"), `{"event":"completion.declared","summary":"two"}`)
		s.stopped(j)
		s.passUntilDone(created.ID)
		v := s.job(created.ID)
		for _, d := range v.Deliverables {
			switch d.Branch {
			case "spov/merged":
				if d.Status != store.DeliverableVerified || d.Detail != "merged" || d.Number != merged {
					t.Errorf("the merged one: %+v, want #%d verified as merged", d, merged)
				}
			case "spov/closed":
				if d.Status != store.DeliverableMissing || d.Detail != "closed without merging" || d.Number != closed {
					t.Errorf("the closed one: %+v, want #%d missing", d, closed)
				}
			}
		}
	})

	step("4.5 nothing secret reached a response or the log", func(t *testing.T) {
		s.scan(t, nil, token)
		for where, text := range map[string]string{"the server log": s.log.String(), "a response": strings.Join(s.bodies, "\n")} {
			if strings.Contains(text, f.admin) || strings.Contains(text, memberPass) {
				t.Errorf("the forge's admin token or the member's password is in %s", where)
			}
		}
	})
}

func readForgeEnv(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok {
			env[k] = v
		}
	}
	for _, k := range []string{"FORGE_KIND", "FORGE_URL", "FORGE_CA", "FORGE_ADMIN_TOKEN"} {
		if env[k] == "" {
			t.Fatalf("%s has no %s", path, k)
		}
	}
	return env
}

// realForge sets up users, repositories and pull requests on a Forgejo or
// Gitea through its API, as its admin. Both answer the same calls here.
type realForge struct {
	t     *testing.T
	base  string
	admin string
}

func (f *realForge) do(method, path, auth string, body, out any) {
	f.t.Helper()
	var payload io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, f.base+"/api/v1"+path, payload)
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		f.t.Fatalf("forge %s %s: %d %s", method, path, resp.StatusCode, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			f.t.Fatalf("forge %s %s: %v", method, path, err)
		}
	}
}

func (f *realForge) asAdmin() string { return "token " + f.admin }

// user makes a user and returns their password.
func (f *realForge) user(name string) string {
	pass := "Pw-" + randomAlnum(20)
	f.do(http.MethodPost, "/admin/users", f.asAdmin(), map[string]any{
		"username": name, "email": name + "@example.test", "password": pass, "must_change_password": false,
	}, nil)
	return pass
}

// token makes a token for a user, as they would on the forge's settings
// page, with the scopes docs/forges.md asks for.
func (f *realForge) token(name, pass string) string {
	var out struct {
		Sha1 string `json:"sha1"`
	}
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte(name+":"+pass))
	f.do(http.MethodPost, "/users/"+name+"/tokens", basic, map[string]any{
		"name": "spov", "scopes": []string{"write:repository", "write:issue", "read:user"},
	}, &out)
	if out.Sha1 == "" {
		f.t.Fatal("the forge returned no token")
	}
	return out.Sha1
}

// repo makes a private repository with one commit, for owner, and returns
// its default branch.
func (f *realForge) repo(owner, name string) string {
	var out struct {
		DefaultBranch string `json:"default_branch"`
	}
	f.do(http.MethodPost, "/admin/users/"+owner+"/repos", f.asAdmin(), map[string]any{
		"name": name, "private": true, "auto_init": true,
	}, &out)
	return out.DefaultBranch
}

// pullRequest commits a file on a new branch and opens a pull request
// from it, as the agent would, and returns its number.
func (f *realForge) pullRequest(owner, name, base, branch string) int {
	file := strings.ReplaceAll(branch, "/", "-") + ".md"
	f.do(http.MethodPost, "/repos/"+owner+"/"+name+"/contents/"+file, f.asAdmin(), map[string]any{
		"content": base64.StdEncoding.EncodeToString([]byte("scenario 4\n")), "message": "Add " + file,
		"branch": base, "new_branch": branch,
	}, nil)
	var out struct {
		Number int `json:"number"`
	}
	f.do(http.MethodPost, "/repos/"+owner+"/"+name+"/pulls", f.asAdmin(), map[string]any{
		"head": branch, "base": base, "title": "scenario 4",
	}, &out)
	return out.Number
}
