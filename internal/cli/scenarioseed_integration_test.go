//go:build integration

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/credentials"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/store"
)

// TestIntegrationScenarioSeedUsers signs in the combined scenarios' two
// users on the server scripts/scenarios/hosted-local.sh runs, without
// GitHub: it makes admin@scenario.test (an admin) and member@scenario.test
// if they are missing, and gives each a CLI token, written where
// scripts/scenarios/as.sh's CLI reads it, and a browser session, whose
// cookie is written beside it. This is the scenario's tool, not a test of
// spov (DL-022). It reads the server's environment as hosted-local.sh
// sets it:
//
//	SPOV_SCENARIO_SEED=~/.local/state/spov-scenarios SPOV_DATABASE_URL=... \
//	  SPOV_PUBLIC_URL=http://localhost:8091 SPOV_ALLOWLIST_FILE=... \
//	  SPOV_GITHUB_OAUTH_CLIENT_ID=... SPOV_GITHUB_OAUTH_CLIENT_SECRET=... \
//	  go test -tags integration ./internal/cli/ -run ScenarioSeedUsers -v
//
// @decision(DL-022)
func TestIntegrationScenarioSeedUsers(t *testing.T) {
	stateDir := os.Getenv("SPOV_SCENARIO_SEED")
	if stateDir == "" {
		t.Skip("set SPOV_SCENARIO_SEED to the scenario's state directory to seed its users")
	}
	ctx := context.Background()
	vars := map[string]string{}
	for _, k := range []string{DatabaseURLEnvVar, PublicURLEnvVar, "SPOV_ALLOWLIST_FILE",
		"SPOV_GITHUB_OAUTH_CLIENT_ID", "SPOV_GITHUB_OAUTH_CLIENT_SECRET"} {
		if vars[k] = os.Getenv(k); vars[k] == "" {
			t.Fatalf("%s is not set", k)
		}
	}
	env := &Env{Out: os.Stdout, Err: os.Stderr, Environ: config.MapEnv{Vars: vars}, Workdir: t.TempDir()}
	st, closeStore, _, err := env.openStore(ctx, false, config.Defaults(), func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore()
	login, stop, err := env.newLogin(ctx, st, vars[PublicURLEnvVar], func(string) {})
	if err != nil || login == nil {
		t.Fatalf("login: %v", err)
	}
	defer stop()
	serverKey, err := credentials.ServerKey(vars[PublicURLEnvVar])
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, who := range []string{"admin", "member"} {
		email := who + "@scenario.test"
		// The allowlist file seeds only a database with no admin, so a
		// database from an earlier run gets the entries here.
		if err := st.Allowlist().Put(ctx, store.AllowlistEntry{OrgID: server.DefaultOrgID, Email: email, Admin: who == "admin", CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		u, err := st.Users().ByEmail(ctx, server.DefaultOrgID, email)
		if errors.Is(err, store.ErrNotFound) {
			// A GitHub sign-in would give a git identity; the seed gives one
			// so a browser launch can commit.
			u = store.User{ID: store.NewID(now), OrgID: server.DefaultOrgID, Email: email, Name: who, Admin: who == "admin",
				GitName: "Scenario " + who, GitEmail: email}
			err = st.Users().Create(ctx, u)
		}
		if err != nil {
			t.Fatal(err)
		}
		tok, err := auth.NewToken()
		if err != nil {
			t.Fatal(err)
		}
		exp := now.Add(30 * 24 * time.Hour)
		if err := st.Tokens().Create(ctx, store.APIToken{
			ID: store.NewID(now), OrgID: server.DefaultOrgID, UserID: u.ID, Hash: auth.HashToken(tok),
			Name: "scenario", ExpiresAt: &exp, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		sctx, err := login.Sessions.Load(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		login.Sessions.Put(sctx, auth.SessionKeyUserID, u.ID)
		cookie, _, err := login.Sessions.Commit(sctx)
		if err != nil {
			t.Fatal(err)
		}

		dir := filepath.Join(stateDir, "users", who, "config", "spov")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		creds := map[string]string{}
		path := filepath.Join(dir, "credentials.json")
		if raw, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(raw, &creds)
		}
		creds[serverKey] = tok
		raw, _ := json.MarshalIndent(creds, "", "  ")
		cookieFile := filepath.Join(stateDir, "users", who, "cookie")
		writePrivate(t, path, raw)
		writePrivate(t, cookieFile, []byte(login.Sessions.Cookie.Name+"="+cookie+"\n"))
		t.Logf("%s (%s): CLI token and browser cookie written under %s", email, map[bool]string{true: "admin", false: "member"}[u.Admin], filepath.Join(stateDir, "users", who))
	}
}

// writePrivate writes a file only its owner can read, whatever mode a
// file already there had.
func writePrivate(t *testing.T, path string, data []byte) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}
