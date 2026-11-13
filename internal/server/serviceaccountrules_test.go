package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/store"
)

// actAs makes the fixture's admin slot, behind the token authenticator,
// the principal p.
func (f *serviceFixture) actAs(p auth.Principal) {
	chain := f.auth.inner.(auth.Chain)
	f.auth.inner = auth.Chain{chain[0], auth.StaticAuthenticator{Principal: p}}
}

// Removing an admin from the allowlist leaves the service tokens they
// issued working, because revoking them would stop the pipelines that
// use them; the off-boarding counts them in its audit row and names
// the accounts in a warning, and the listing says who issued each.
//
// @decision(DL-141) @decision(DL-132)
func TestRemovingAnAdminLeavesTheServiceTokensTheyIssuedAndSaysSo(t *testing.T) {
	f := newServiceFixture(t, map[string]bool{"me@example.com": true, theirEmail: true}, nil)
	ctx := context.Background()
	if err := f.store.Users().Create(ctx, store.User{ID: theirID, OrgID: auth.LocalID, Email: theirEmail}); err != nil {
		t.Fatal(err)
	}
	me := auth.Principal{UserID: auth.LocalID, OrgID: auth.LocalID, Email: "me@example.com", Admin: true}
	f.actAs(auth.Principal{UserID: theirID, OrgID: auth.LocalID, Email: theirEmail, Admin: true})
	f.createAccount(t, "ci-theirs")
	tok := f.issue(t, "ci-theirs", map[string]any{"name": "deploy"})
	if tok.IssuedBy == nil || tok.IssuedBy.ID != theirID || tok.IssuedBy.Label != theirEmail {
		t.Errorf("the issued token's issuer = %+v", tok.IssuedBy)
	}
	f.actAs(me)

	if resp, out := f.do(http.MethodDelete, "/api/v1/allowlist/"+theirEmail, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("removing the admin = %d: %s", resp.StatusCode, out)
	}
	if resp, _ := f.do(http.MethodGet, "/api/v1/me", nil, bearer(tok.Token)); resp.StatusCode != http.StatusOK {
		t.Errorf("the service token the removed admin issued = %d, want it still working", resp.StatusCode)
	}
	evs := f.actions(t, AdminMemberOffboarded)
	if len(evs) != 1 || !strings.Contains(evs[0].Detail, "1 service account token they issued is still usable") ||
		!strings.Contains(evs[0].Detail, "ci-theirs") {
		t.Errorf("the off-boarding audit rows = %+v", evs)
	}
	f.mu.Lock()
	logs := strings.Join(f.logs, "\n")
	f.mu.Unlock()
	if !strings.Contains(logs, "ci-theirs") {
		t.Errorf("no warning names the account whose token is still usable:\n%s", logs)
	}
	if strings.Contains(logs, strings.TrimPrefix(tok.Token, "spov_pat_")) {
		t.Error("a log line carries the token")
	}
	_, out := f.do(http.MethodGet, "/api/v1/service-accounts/ci-theirs/tokens", nil)
	var list struct{ Tokens []ServiceTokenView }
	if err := json.Unmarshal(out, &list); err != nil || len(list.Tokens) != 1 {
		t.Fatalf("listing = %s", out)
	}
	if by := list.Tokens[0].IssuedBy; by == nil || by.ID != theirID || by.Label != theirEmail {
		t.Errorf("the listing's issuer = %+v", by)
	}
}

// A create authenticated before the account's delete committed, and
// run after it, is refused inside its own transaction and stores
// nothing.
//
// @decision(DL-140)
func TestACreateByADeletedServiceAccountIsRefused(t *testing.T) {
	q := &recordingQueue{}
	f := newServiceFixture(t, nil, func(d *Deps) {
		d.Jobs = &jobs.Supervisor{Store: d.Store, Provider: d.Provider, Queue: q.insert, Warn: d.Warn}
	})
	ctx := context.Background()
	acct := f.createAccount(t, "ci-late")
	svc := auth.Principal{UserID: acct.ID, OrgID: auth.LocalID, Email: auth.ServiceAccountAddress("ci-late"), Service: true}
	f.actAs(svc)
	if resp, out := f.do(http.MethodPost, "/api/v1/sessions", repoReq("svc-live")); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("a live account's create = %d: %s", resp.StatusCode, out)
	}
	if err := f.store.Users().Disable(ctx, auth.LocalID, acct.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]any{"/api/v1/sessions": repoReq("svc-late"), "/api/v1/jobs": jobRequest()} {
		if resp, out := f.do(http.MethodPost, path, body); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("POST %s by a deleted account = %d, want 401: %s", path, resp.StatusCode, out)
		}
	}
	sessions, _ := f.store.Sessions().List(ctx, auth.LocalID, store.SessionFilter{OwnerID: acct.ID, Limit: 50})
	if len(sessions) != 1 {
		t.Errorf("the deleted account owns %d sessions, want only the one from before", len(sessions))
	}
	if js, _ := f.store.Jobs().List(ctx, auth.LocalID, store.JobFilter{OwnerID: acct.ID}); len(js) != 0 {
		t.Errorf("the deleted account owns jobs: %+v", js)
	}
}

// The admin sets a service account's commit author; the account
// cannot set its own, though it sets its other settings as a user does.
//
// @decision(DL-140)
func TestAServiceAccountCannotSetItsOwnAuthor(t *testing.T) {
	f := newServiceFixture(t, nil, nil)
	acct := f.createAccount(t, "ci-author")
	tok := f.issue(t, "ci-author", map[string]any{"name": "ci"})
	for _, body := range []map[string]any{{"git_name": "Someone Else"}, {"git_email": "ceo@example.com"}} {
		if resp, out := f.do(http.MethodPatch, "/api/v1/me", body, bearer(tok.Token)); resp.StatusCode != http.StatusForbidden {
			t.Errorf("PATCH /me %v as the account = %d: %s", body, resp.StatusCode, out)
		}
	}
	if resp, out := f.do(http.MethodPatch, "/api/v1/me", map[string]any{"default_job_duration": "2h"}, bearer(tok.Token)); resp.StatusCode != http.StatusOK {
		t.Errorf("PATCH /me of another setting = %d: %s", resp.StatusCode, out)
	}
	u, _ := f.store.Users().Get(context.Background(), auth.LocalID, acct.ID)
	if u.GitName != "ci-author" || u.GitEmail != "ci-author@service.invalid" || u.DefaultJobDuration != 2*time.Hour {
		t.Errorf("the account's row = %+v", u)
	}
}

// An account revoking one of its own tokens through /me/tokens writes
// the same audit row an admin's revoke does, with the account as actor.
//
// @decision(DL-141)
func TestAServiceAccountRevokingItsOwnTokenIsAudited(t *testing.T) {
	f := newServiceFixture(t, nil, nil)
	f.createAccount(t, "ci-self")
	tok := f.issue(t, "ci-self", map[string]any{"name": "self"})
	if resp, out := f.do(http.MethodDelete, "/api/v1/me/tokens/current", nil, bearer(tok.Token)); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoking its own token = %d: %s", resp.StatusCode, out)
	}
	evs := f.actions(t, AdminServiceTokenRevoked)
	if len(evs) != 1 || evs[0].Actor != auth.ServiceAccountAddress("ci-self") || !strings.Contains(evs[0].Detail, tok.ID) ||
		!strings.Contains(evs[0].Detail, "ci-self") {
		t.Errorf("revoke audit rows = %+v", evs)
	}
	if resp, _ := f.do(http.MethodGet, "/api/v1/me", nil, bearer(tok.Token)); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the revoked token = %d", resp.StatusCode)
	}
}

// A session or job a service account creates is authored as the admin
// set it, whatever author the request names: the CLI sends the git
// identity of the machine it runs on.
//
// @decision(DL-140)
func TestAServiceAccountsCreateIgnoresTheRequestsAuthor(t *testing.T) {
	q := &recordingQueue{}
	f := newServiceFixture(t, nil, func(d *Deps) {
		d.Jobs = &jobs.Supervisor{Store: d.Store, Provider: d.Provider, Queue: q.insert, Warn: d.Warn}
	})
	ctx := context.Background()
	acct := f.createAccount(t, "ci-runner")
	f.actAs(auth.Principal{UserID: acct.ID, OrgID: auth.LocalID, Email: auth.ServiceAccountAddress("ci-runner"), Service: true})
	theirs := GitIdentityView{Name: "Someone Else", Email: "ceo@example.com"}
	sreq := repoReq("svc-authored")
	sreq.Git = theirs
	jreq := jobRequest()
	jreq.Git = theirs
	if resp, out := f.do(http.MethodPost, "/api/v1/sessions", sreq); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("the session create = %d: %s", resp.StatusCode, out)
	}
	if resp, out := f.do(http.MethodPost, "/api/v1/jobs", jreq); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("the job create = %d: %s", resp.StatusCode, out)
	}
	sessions, _ := f.store.Sessions().List(ctx, auth.LocalID, store.SessionFilter{OwnerID: acct.ID, Limit: 50})
	if len(sessions) != 2 {
		t.Fatalf("the account owns %d sessions, want the session and the job's", len(sessions))
	}
	for _, s := range sessions {
		if s.Request.GitName != "" || s.Request.GitEmail != "" {
			t.Errorf("session %s keeps the request's author %q <%s>", s.Name, s.Request.GitName, s.Request.GitEmail)
		}
	}
}
