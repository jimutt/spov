//go:build integration

package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/credproxy"
	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/store"
)

// TestScenario5 is combined scenario 5's control-plane half, run through
// the API of a server built as `spov serve` builds one, on real Postgres,
// against a GitHub that checks App keys and repository reach. What only a
// real GitHub or a real sandbox can show (git through the proxy, a clone
// that fails, the pages) is not exercised here.
//
//	SPOV_TEST_DATABASE_URL=... go test -tags integration ./internal/cli/ -run TestScenario5 -v
//
// @decision(DL-022)
func TestScenario5(t *testing.T) {
	s := newScenarioServer(t)
	ctx := context.Background()
	gh := s.gh
	key1, key2 := gh.newKey(t, "k1"), gh.newKey(t, "k2")
	const pat = "github_pat_scenario0123456789abcdefghijklmnopqrstuvwxyz"
	appPath := "/api/v1/org/github"
	put := func(id, key string) (int, []byte) {
		return s.call("admin", http.MethodPut, appPath+"/github", map[string]string{"app_id": id, "private_key": key})
	}
	view := func() orgApp {
		var v orgApp
		s.must(http.StatusOK, "admin", http.MethodGet, appPath, nil, &v)
		return v
	}
	// Each step fails on its own, and the helpers report into it; a
	// failed step ends the run, since later steps build on it.
	step := func(name string, f func(t *testing.T)) {
		ok := t.Run(name, func(st *testing.T) {
			s.t = st
			defer func() { s.t = t }()
			f(st)
		})
		if !ok {
			t.FailNow()
		}
	}

	step("5.1 a broken App is refused and nothing is stored", func(t *testing.T) {
		// A key the App does not have is well formed, so only GitHub's
		// signature check refuses it.
		stranger, _ := rsaKey(t)
		for what, body := range map[string][2]string{
			"a wrong ID":                  {"777", key1},
			"a key the App does not have": {gh.appID, stranger},
			"no key at all":               {gh.appID, "not a key"},
		} {
			code, raw := put(body[0], body[1])
			if code != http.StatusUnprocessableEntity || !strings.Contains(string(raw), "nothing was stored") {
				t.Errorf("%s: %d %s", what, code, raw)
			}
			if what != "no key at all" && !strings.Contains(string(raw), "installation") {
				t.Errorf("%s was refused before GitHub was asked: %s", what, raw)
			}
		}
		v := view()
		if v.Platforms[0].Source != "none" || len(v.Events) != 0 {
			t.Errorf("after refusals: source %q, %d audit rows", v.Platforms[0].Source, len(v.Events))
		}
	})

	step("5.2 the stored App's status names what it reaches", func(t *testing.T) {
		if code, raw := put(gh.appID, key1); code != http.StatusOK {
			t.Fatalf("store: %d %s", code, raw)
		}
		p := view().Platforms[0]
		c := p.Check
		if p.Source != "stored" || p.Account != gh.org || p.InstallationID != gh.install || c == nil || c.Error != "" ||
			c.RepoCount != 2 || len(c.Missing) != 0 || !slices.Contains(c.Repos, "acme/shared-only") {
			t.Errorf("status %+v, check %+v", p, c)
		}
	})

	step("5.4 a non-admin can neither read nor change the App", func(t *testing.T) {
		for _, r := range []struct {
			method string
			body   any
		}{
			{http.MethodGet, nil},
			{http.MethodPut, map[string]string{"app_id": "1", "private_key": "x"}},
			{http.MethodDelete, nil},
		} {
			path := appPath
			if r.method != http.MethodGet {
				path += "/github"
			}
			if code, raw := s.call("member", r.method, path, r.body); code != http.StatusForbidden {
				t.Errorf("%s as member: %d %s", r.method, code, raw)
			}
		}
		if view().Platforms[0].Source != "stored" {
			t.Error("a refused change changed the App")
		}
	})

	step("scenario 3's precondition: with no credential of its own a session uses the App", func(t *testing.T) {
		v := s.launch("shared-only", nil)
		if v.State != store.StateRunning || tierOf(v) != "org" {
			t.Fatalf("state %s tier %q error %q", v.State, tierOf(v), v.Error)
		}
		s.end(v.ID)
	})

	step("member stores a personal token, checked against GitHub", func(t *testing.T) {
		gh.mu.Lock()
		gh.pats[pat] = []string{"both", "personal-only"}
		gh.mu.Unlock()
		if code, raw := s.call("member", http.MethodPut, "/api/v1/me/credentials/github", map[string]string{"token": "github_pat_not-a-real-one-at-all"}); code < 400 {
			t.Errorf("a token GitHub refuses was stored: %d %s", code, raw)
		}
		s.must(http.StatusOK, "member", http.MethodPut, "/api/v1/me/credentials/github", map[string]string{"token": pat}, nil)
	})

	step("5.9 the listing marks which tier reaches each repository", func(t *testing.T) {
		var out struct {
			Repos []server.RepoView `json:"repos"`
		}
		s.must(http.StatusOK, "member", http.MethodGet, "/api/v1/repos", nil, &out)
		got := map[string]string{}
		for _, r := range out.Repos {
			var via []string
			for _, v := range r.Via {
				via = append(via, string(v))
			}
			slices.Sort(via)
			got[r.Name] = strings.Join(via, ",")
		}
		want := map[string]string{"both": "org,user", "shared-only": "org", "personal-only": "user"}
		for name, w := range want {
			if got[name] != w {
				t.Errorf("%s via %q, want %q (all: %v)", name, got[name], w, got)
			}
		}
	})

	step("5.10 and 5.11 the browser is personal-first and the CLI's preference takes the App", func(t *testing.T) {
		personal := s.launch("both", nil)
		shared := s.launch("both", map[string]any{"credential_preference": map[string]string{"github": "org"}})
		if tierOf(personal) != "user" || tierOf(shared) != "org" {
			t.Errorf("no preference: %q (%s %q), prefer shared: %q (%s %q)", tierOf(personal), personal.State, personal.Error, tierOf(shared), shared.State, shared.Error)
		}
		s.end(personal.ID)
		s.end(shared.ID)
	})

	step("5.13 git access all on the App is refused when asked for and falls back when preferred", func(t *testing.T) {
		body := map[string]any{"name": "scn-refused", "repos": []map[string]string{{"platform": "github", "owner": gh.org, "name": "both"}},
			"credential_preference": map[string]string{"github": "org"}, "git_access": "all"}
		code, raw := s.call("member", http.MethodPost, "/api/v1/sessions", body)
		if code != http.StatusUnprocessableEntity || !strings.Contains(string(raw), "git access all cannot apply") {
			t.Errorf("all on the App without consent: %d %s", code, raw)
		}
		t.Logf("all on the App without consent: %d %s", code, raw)
		v := s.launch("both", map[string]any{"credential_preference": map[string]string{"github": "org"},
			"git_access": "all", "git_access_preferred": true})
		for _, c := range v.Credentials {
			if c.Platform == "github" && c.Access != store.GitAccessRepos {
				t.Errorf("a preferred all on the App got %q", c.Access)
			}
		}
		if v.State != store.StateRunning {
			t.Errorf("the preferred all did not launch: %s %q", v.State, v.Error)
		}
		s.end(v.ID)
	})

	var a, b server.SessionView
	step("5.14 a shared-first session records the shared tier", func(t *testing.T) {
		a = s.launch("both", map[string]any{"credential_preference": map[string]string{"github": "org"}})
		if a.State != store.StateRunning || tierOf(a) != "org" {
			t.Fatalf("state %s tier %q error %q", a.State, tierOf(a), a.Error)
		}
		if m := gh.mintLog(); m[len(m)-1].Key != "k1" {
			t.Errorf("minted with %q", m[len(m)-1].Key)
		}
	})

	step("5.3 a replacement key applies to new launches at once", func(t *testing.T) {
		if code, raw := put(gh.appID, key2); code != http.StatusOK {
			t.Fatalf("rotate: %d %s", code, raw)
		}
		gh.deleteKey("k1")
		b = s.launch("shared-only", map[string]any{"credential_preference": map[string]string{"github": "org"}})
		if b.State != store.StateRunning || tierOf(b) != "org" {
			t.Fatalf("after the rotation: state %s tier %q error %q", b.State, tierOf(b), b.Error)
		}
		if m := gh.mintLog(); m[len(m)-1].Key != "k2" {
			t.Errorf("minted with %q after the rotation", m[len(m)-1].Key)
		}
	})

	step("5.15 after a restart the sessions keep their tier and token", func(t *testing.T) {
		before := s.row(a.ID).GitTokenMintedAt
		s.restart(nil)
		var v server.SessionView
		s.must(http.StatusOK, "member", http.MethodGet, "/api/v1/sessions/"+a.ID, nil, &v)
		if v.State != store.StateRunning || tierOf(v) != "org" {
			t.Errorf("after the restart: %s %q", v.State, tierOf(v))
		}
		if after := s.row(a.ID).GitTokenMintedAt; after == nil || before == nil || !after.Equal(*before) {
			t.Errorf("the restart moved the mint: %v then %v", before, after)
		}
	})

	step("5.17 a renewal mints from the recorded tier, with the replacement key", func(t *testing.T) {
		before := s.row(a.ID).GitTokenMintedAt
		mints := len(gh.mintLog())
		h := s.provider.Handles()[a.SandboxID]
		execs := len(h.SidecarExecs())
		s.clock.advance(46 * time.Minute)
		s.act(a.ID)
		s.act(b.ID)
		s.srv.Service().RenewGitTokens(ctx)
		after := s.row(a.ID).GitTokenMintedAt
		if after == nil || !after.After(*before) {
			t.Fatalf("not renewed: %v then %v; log:\n%s", before, after, strings.Join(s.warningsFor(a.ID), "\n"))
		}
		var forA string
		for _, m := range gh.mintLog()[mints:] {
			if m.Key != "k2" {
				t.Errorf("a renewal signed with %q", m.Key)
			}
			if slices.Equal(m.Repos, []string{"both"}) {
				forA = m.Secret
			}
		}
		if forA == "" {
			t.Fatal("no renewal narrowed to A's repository")
		}
		written := false
		for _, e := range h.SidecarExecs()[execs:] {
			written = written || strings.Contains(string(e.Stdin), forA)
		}
		if !written {
			t.Error("A's renewed token was not written to its sidecar")
		}
	})

	step("5.16 a killed sidecar is recreated with the recorded tier's token", func(t *testing.T) {
		h := s.provider.Handles()[a.SandboxID]
		h.KillSidecar()
		s.srv.Service().CheckSidecars(ctx)
		started := h.StartedSidecars()
		if len(started) != 1 {
			t.Fatalf("recreated %d sidecars", len(started))
		}
		var cfg credproxy.SidecarConfig
		if err := json.Unmarshal([]byte(started[0].Secrets[credproxy.EnvConfig]), &cfg); err != nil {
			t.Fatal(err)
		}
		m := gh.mintLog()
		newest := m[len(m)-1]
		if len(cfg.Platforms) != 1 || cfg.Platforms[0].Token != newest.Secret || !slices.Equal(newest.Repos, []string{"both"}) {
			t.Errorf("the recreated sidecar holds %d platforms, not the App token just minted for A's repository", len(cfg.Platforms))
		}
		info, _ := h.Info(ctx)
		if info.Tags[sandbox.TagProxyRecreated] != "1" {
			t.Errorf("tags %v", info.Tags)
		}
		// The sandbox's own CA, read back sealed by the server that did not
		// launch it (the restart in 5.15), so nothing is installed (DL-034).
		var launched string
		for _, spec := range s.provider.Specs() {
			if spec.Name == a.Name {
				launched = spec.Env[credproxy.EnvMainCA]
			}
		}
		if launched == "" || started[0].Env[credproxy.EnvCACert] != launched {
			t.Error("the recreated sidecar does not reuse the CA the sandbox was launched trusting")
		}
		if slices.ContainsFunc(h.ExecLog(), func(c []string) bool { return slices.Equal(c, credproxy.InstallCACommand()) }) {
			t.Error("a CA was installed though the sandbox already trusts its own")
		}
	})

	gate2 := jobs.FindPullRequestVia(s.srv.Service().JobOwnerForge, s.store)
	shared := store.Deliverable{Platform: "github", Owner: gh.org, Name: "shared-only", Branch: "spov/scenario"}
	step("5.18 gate 2 verifies through the recorded tier, on a repository only it can see", func(t *testing.T) {
		gh.openPull("shared-only", "spov/scenario", 7)
		pr, err := gate2(ctx, store.Job{OrgID: server.DefaultOrgID, SessionID: b.ID}, shared)
		if err != nil || pr == nil || pr.Number != 7 {
			t.Errorf("gate 2: %+v, %v", pr, err)
		}
	})

	step("5.6 and 5.12 with the App removed, nothing gets it and the preference falls back", func(t *testing.T) {
		s.must(http.StatusOK, "admin", http.MethodDelete, appPath+"/github", nil, nil)
		only := s.launch("shared-only", nil)
		if tierOf(only) == "org" {
			t.Error("a launch after the removal got the organisation's credential")
		}
		// The personal token cannot see it, which the launch finds out
		// before any sandbox exists (DL-043).
		if only.State != store.StateFailed || !strings.Contains(only.Error, "cannot see acme/shared-only") {
			t.Errorf("a launch on a repository the personal token cannot see: %s %q", only.State, only.Error)
		}
		s.end(only.ID)
		fallback := s.launch("both", map[string]any{"credential_preference": map[string]string{"github": "org"}})
		if fallback.State != store.StateRunning || tierOf(fallback) != "user" {
			t.Errorf("prefer shared with no App: %s %q %q", fallback.State, tierOf(fallback), fallback.Error)
		}
		s.end(fallback.ID)
	})

	step("5.19 with the recorded tier gone, renewal stops and gate 2 is unverified", func(t *testing.T) {
		before := s.row(b.ID).GitTokenMintedAt
		mints := len(gh.mintLog())
		s.clock.advance(46 * time.Minute)
		s.act(b.ID)
		s.srv.Service().RenewGitTokens(ctx)
		if after := s.row(b.ID).GitTokenMintedAt; after == nil || !after.Equal(*before) {
			t.Errorf("B was renewed without its tier: %v then %v", before, after)
		}
		if len(gh.mintLog()) != mints {
			t.Error("something minted with no App stored")
		}
		if !strings.Contains(s.log.String(), "renew git token for "+b.ID) {
			t.Errorf("the failed renewal was not logged; log:\n%s", s.log.String())
		}
		pr, err := gate2(ctx, store.Job{OrgID: server.DefaultOrgID, SessionID: b.ID}, shared)
		if err == nil || pr != nil {
			t.Errorf("gate 2 with the tier gone: %+v, %v", pr, err)
		}
		// Where the personal token could see it too, so falling back to
		// it would verify: it must not.
		gh.openPull("both", "spov/a", 8)
		both := store.Deliverable{Platform: "github", Owner: gh.org, Name: "both", Branch: "spov/a"}
		if pr, err := gate2(ctx, store.Job{OrgID: server.DefaultOrgID, SessionID: a.ID}, both); err == nil || pr != nil {
			t.Errorf("gate 2 verified A's pull request through another credential: %+v, %v", pr, err)
		} else {
			t.Logf("gate 2 with the recorded tier gone: %v", err)
		}
		// A sidecar recreated now must not be handed the personal token.
		hb := s.provider.Handles()[b.SandboxID]
		hb.KillSidecar()
		s.srv.Service().CheckSidecars(ctx)
		for _, sc := range hb.StartedSidecars() {
			if strings.Contains(sc.Secrets[credproxy.EnvConfig], pat) {
				t.Error("B's recreated sidecar holds the personal token")
			}
		}
		t.Logf("B after its sidecar died with the tier gone: %d recreated, state %s %q",
			len(hb.StartedSidecars()), s.row(b.ID).State, s.row(b.ID).Error)
	})

	var key3 string
	step("5.5 a deleted App key fails the launch and never falls to the personal token", func(t *testing.T) {
		if code, raw := put(gh.appID, key2); code != http.StatusOK {
			t.Fatalf("restore: %d %s", code, raw)
		}
		gh.deleteKey("k2")
		v := s.launch("both", map[string]any{"credential_preference": map[string]string{"github": "org"}})
		if v.State != store.StateFailed || !strings.Contains(v.Error, "installation") {
			t.Errorf("with the stored key deleted: %s %q", v.State, v.Error)
		}
		t.Logf("a launch with the App's key deleted: %q", v.Error)
		key3 = gh.newKey(t, "k3")
		if code, raw := put(gh.appID, key3); code != http.StatusOK {
			t.Fatalf("store key 3: %d %s", code, raw)
		}
	})

	step("5.5 a revoked personal token fails gate 2 and never falls to the App", func(t *testing.T) {
		mine := s.launch("both", nil)
		if mine.State != store.StateRunning || tierOf(mine) != "user" {
			t.Fatalf("a personal-first launch: %s %q %q", mine.State, tierOf(mine), mine.Error)
		}
		gh.openPull("both", "spov/mine", 9)
		gh.revokePAT(pat)
		// The App sees this repository too, so a fall-through would verify.
		mineDeliverable := store.Deliverable{Platform: "github", Owner: gh.org, Name: "both", Branch: "spov/mine"}
		if pr, err := gate2(ctx, store.Job{OrgID: server.DefaultOrgID, SessionID: mine.ID}, mineDeliverable); err == nil || pr != nil {
			t.Errorf("gate 2 with the personal token revoked: %+v, %v", pr, err)
		} else {
			t.Logf("gate 2 with the personal token revoked: %v", err)
		}
		s.end(mine.ID)
	})

	step("5.7 an App in the environment is shown and cannot be changed here", func(t *testing.T) {
		s.restart(map[string]string{"SPOV_GITHUB_APP_ID": gh.appID, "SPOV_GITHUB_APP_PRIVATE_KEY": key3})
		if src := view().Platforms[0].Source; src != "environment" {
			t.Errorf("source %q", src)
		}
		for _, m := range []string{http.MethodPut, http.MethodDelete} {
			code, raw := s.call("admin", m, appPath+"/github", map[string]string{"app_id": gh.appID, "private_key": key3})
			if code != http.StatusConflict {
				t.Errorf("%s with the environment's App: %d %s", m, code, raw)
			}
		}
		s.restart(nil)
	})

	step("5.8 every change is audited, without a secret", func(t *testing.T) {
		var stored, removed int
		for _, e := range view().Events {
			switch e.Action {
			case server.AdminGitHubAppStored:
				stored++
			case server.AdminGitHubAppRemoved:
				removed++
			}
		}
		if stored != 4 || removed != 1 {
			t.Errorf("audit rows: %d stored, %d removed", stored, removed)
		}
	})

	t.Run("no secret in any response or the server log", func(t *testing.T) {
		s.scan(t, []string{key1, key2, key3}, pat)
	})
}
