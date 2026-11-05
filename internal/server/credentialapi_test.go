package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/store"
)

// fakeVerifier stands in for a real platform round trip.
type fakeVerifier struct {
	as      string
	scopes  []string
	expires time.Time
	err     error
	seen    []string
}

func (v *fakeVerifier) Verify(_ context.Context, platform, secret string) (Verified, error) {
	v.seen = append(v.seen, platform+"="+secret)
	return Verified{As: v.as, Scopes: v.scopes, ExpiresAt: v.expires}, v.err
}

// credFixture is an API fixture with user credentials enabled.
func credFixture(t *testing.T, verifier CredentialVerifier) *apiFixture {
	t.Helper()
	key := base64.StdEncoding.EncodeToString(make([]byte, auth.KeyBytes))
	sealer, err := auth.NewSealer(map[int]string{1: key})
	if err != nil {
		t.Fatal(err)
	}
	return newAPIFixtureWith(t, func(d *Deps) {
		d.Credentials = &CredentialStore{Store: d.Store.Credentials(), Sealer: sealer}
		d.Verifier = verifier
	})
}

func TestPutAndListCredential(t *testing.T) {
	f := credFixture(t, &fakeVerifier{as: "alice"})

	resp, body := f.do(http.MethodPut, "/api/v1/me/credentials/github",
		map[string]string{"token": "github_pat_secret"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d: %s", resp.StatusCode, body)
	}
	var view CredentialView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatal(err)
	}
	if view.VerifiedAs != "alice" || !view.Present {
		t.Errorf("view = %+v", view)
	}
	// The response must not echo the credential back.
	if strings.Contains(string(body), "github_pat_secret") {
		t.Errorf("the response carries the token: %s", body)
	}

	resp, body = f.do(http.MethodGet, "/api/v1/me/credentials", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d: %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "github_pat_secret") {
		t.Errorf("the listing carries the token: %s", body)
	}
	if !strings.Contains(string(body), "alice") {
		t.Errorf("the listing lost verified_as: %s", body)
	}
}

func TestDeleteCredential(t *testing.T) {
	f := credFixture(t, &fakeVerifier{})
	if resp, body := f.do(http.MethodPut, "/api/v1/me/credentials/github",
		map[string]string{"token": "github_pat_secret"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d: %s", resp.StatusCode, body)
	}
	if resp, body := f.do(http.MethodDelete, "/api/v1/me/credentials/github", nil); resp.StatusCode >= 300 {
		t.Fatalf("DELETE = %d: %s", resp.StatusCode, body)
	}
	_, body := f.do(http.MethodGet, "/api/v1/me/credentials", nil)
	if strings.Contains(string(body), "github") {
		t.Errorf("the credential survived the delete: %s", body)
	}
}

// Removing a credential that is not there is the outcome the caller
// asked for. It must not 404 — and certainly not with the shared error
// mapper's "no such session".
func TestDeleteMissingCredentialSucceeds(t *testing.T) {
	f := credFixture(t, &fakeVerifier{})
	resp, body := f.do(http.MethodDelete, "/api/v1/me/credentials/github", nil)
	if resp.StatusCode >= 300 {
		t.Fatalf("DELETE = %d: %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "session") {
		t.Errorf("a credential delete reported a session error: %s", body)
	}
}

// A credential the platform rejects must not be stored: that is the
// whole reason for verifying before writing.
func TestPutRefusesAnUnverifiableCredential(t *testing.T) {
	f := credFixture(t, &fakeVerifier{err: errors.New("bad credentials")})
	resp, body := f.do(http.MethodPut, "/api/v1/me/credentials/github",
		map[string]string{"token": "github_pat_wrong"})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("PUT = %d, want 422: %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "github_pat_wrong") {
		t.Errorf("the error echoes the token: %s", body)
	}
	_, body = f.do(http.MethodGet, "/api/v1/me/credentials", nil)
	if strings.Contains(string(body), "github") {
		t.Errorf("a rejected credential was stored anyway: %s", body)
	}
}

// Forgejo answers a bad token by quoting it back. That message must not
// reach the response body, where every proxy and access log between
// here and the caller would see it.
func TestVerificationErrorDoesNotEchoTheToken(t *testing.T) {
	const token = "a0123456789abcdef0123456789abcdef01234567"
	f := credFixture(t, &fakeVerifier{
		err: errors.New("access token does not exist [sha: " + token + "]"),
	})
	resp, body := f.do(http.MethodPut, "/api/v1/me/credentials/forgejo:codefloe.com",
		map[string]string{"token": token})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("PUT = %d, want 422: %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), token) {
		t.Errorf("the 422 echoes the token: %s", body)
	}
	// The message must still say what happened.
	if !strings.Contains(string(body), "rejected the credential") {
		t.Errorf("the error lost its meaning: %s", body)
	}
}

// A provider key has nothing to verify against, and must still store.
func TestPutStoresWhatCannotBeVerified(t *testing.T) {
	f := credFixture(t, &fakeVerifier{err: ErrVerifyUnsupported})
	resp, body := f.do(http.MethodPut, "/api/v1/me/credentials/provider:ANTHROPIC_API_KEY",
		map[string]string{"token": "sk-ant-secret"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d, want 200: %s", resp.StatusCode, body)
	}
}

// An empty token is refused before verification is attempted.
func TestPutRejectsAnEmptyToken(t *testing.T) {
	f := credFixture(t, &fakeVerifier{})
	resp, body := f.do(http.MethodPut, "/api/v1/me/credentials/github",
		map[string]string{"token": "   "})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("PUT = %d, want 422: %s", resp.StatusCode, body)
	}
}

// Without a verifier the credential is stored on the user's word.
func TestPutWithoutAVerifier(t *testing.T) {
	f := credFixture(t, nil)
	resp, body := f.do(http.MethodPut, "/api/v1/me/credentials/github",
		map[string]string{"token": "github_pat_secret"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d: %s", resp.StatusCode, body)
	}
}

// The delete path validates the platform too, so a typo is named rather
// than silently deleting nothing.
func TestDeleteRejectsAnUnknownPlatform(t *testing.T) {
	f := credFixture(t, &fakeVerifier{})
	resp, body := f.do(http.MethodDelete, "/api/v1/me/credentials/gitlab", nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("DELETE = %d, want 422: %s", resp.StatusCode, body)
	}
}

func TestPutRejectsAnUnknownPlatform(t *testing.T) {
	f := credFixture(t, &fakeVerifier{})
	resp, body := f.do(http.MethodPut, "/api/v1/me/credentials/gitlab",
		map[string]string{"token": "whatever"})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("PUT = %d, want 422: %s", resp.StatusCode, body)
	}
}

// Without a key the server must say so plainly rather than failing
// somewhere inside the store.
func TestPutWithoutAKeyIs501(t *testing.T) {
	f := newAPIFixture(t)
	resp, body := f.do(http.MethodPut, "/api/v1/me/credentials/github",
		map[string]string{"token": "github_pat_secret"})
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("PUT = %d, want 501: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "SPOV_CREDENTIAL_KEY") {
		t.Errorf("the error does not name the missing key: %s", body)
	}
}

// The credential endpoints are authenticated like every other API path.
func TestCredentialEndpointsNeedAuthentication(t *testing.T) {
	f := credFixture(t, &fakeVerifier{})
	strip := func(r *http.Request) { r.Header.Del("Authorization") }
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/v1/me/credentials", nil},
		{http.MethodPut, "/api/v1/me/credentials/github", map[string]string{"token": "x"}},
		{http.MethodDelete, "/api/v1/me/credentials/github", nil},
	} {
		t.Run(tc.method, func(t *testing.T) {
			resp, _ := f.do(tc.method, tc.path, tc.body, strip)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s = %d, want 401", tc.method, tc.path, resp.StatusCode)
			}
		})
	}
}

// One user must not be able to name another's credential: the path
// carries a platform, never a user.
func TestCredentialsAreScopedToTheCaller(t *testing.T) {
	f := credFixture(t, &fakeVerifier{as: "alice"})
	if resp, body := f.do(http.MethodPut, "/api/v1/me/credentials/github",
		map[string]string{"token": "github_pat_mine"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d: %s", resp.StatusCode, body)
	}
	// A second principal in the same org sees nothing of the first's.
	other := &auth.StaticAuthenticator{
		Principal: auth.Principal{UserID: "someone-else", OrgID: auth.LocalID},
	}
	f.auth.inner = other
	_, body := f.do(http.MethodGet, "/api/v1/me/credentials", nil)
	if strings.Contains(string(body), "github") {
		t.Errorf("another user's credential is visible: %s", body)
	}
}

func TestPatchMeUpdatesTheGitIdentity(t *testing.T) {
	f := newAPIFixtureWith(t, func(d *Deps) {
		if err := d.Store.Users().Create(context.Background(), store.User{
			ID: auth.LocalID, OrgID: auth.LocalID, Email: "me@example.com",
		}); err != nil {
			t.Fatal(err)
		}
	})
	resp, body := f.do(http.MethodPatch, "/api/v1/me",
		map[string]string{"git_name": "Alice", "git_email": "alice@example.com"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", resp.StatusCode, body)
	}
	var me MeView
	if err := json.Unmarshal(body, &me); err != nil {
		t.Fatal(err)
	}
	if me.GitName != "Alice" || me.GitEmail != "alice@example.com" {
		t.Errorf("me = %+v", me)
	}
	// It must survive a re-read, not just be echoed back.
	_, body = f.do(http.MethodGet, "/api/v1/me", nil)
	if !strings.Contains(string(body), "alice@example.com") {
		t.Errorf("the identity did not persist: %s", body)
	}
}

// An omitted field must be left alone rather than cleared.
func TestPatchMeLeavesOmittedFields(t *testing.T) {
	f := newAPIFixtureWith(t, func(d *Deps) {
		if err := d.Store.Users().Create(context.Background(), store.User{
			ID: auth.LocalID, OrgID: auth.LocalID, Email: "me@example.com",
			GitName: "Alice", GitEmail: "alice@example.com",
		}); err != nil {
			t.Fatal(err)
		}
	})
	if resp, body := f.do(http.MethodPatch, "/api/v1/me",
		map[string]string{"git_name": "Someone Else"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", resp.StatusCode, body)
	}
	_, body := f.do(http.MethodGet, "/api/v1/me", nil)
	if !strings.Contains(string(body), "alice@example.com") {
		t.Errorf("the omitted email was cleared: %s", body)
	}
}

// The user tier wins over the org tier, and /repos reports which
// answered.
func TestReposReportsTheResolvedTier(t *testing.T) {
	f := newAPIFixtureWith(t, func(d *Deps) {
		d.UserForge = func(context.Context, string, string, TierPolicy) (Forge, []PlatformTier, error) {
			return nil, []PlatformTier{
				{Platform: "gh", Tier: TierUser},
				{Platform: "cf", Tier: TierNone, Reason: "add one with `spov credentials add forgejo:cf`"},
			}, nil
		}
	})
	resp, body := f.do(http.MethodGet, "/api/v1/repos", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /repos = %d: %s", resp.StatusCode, body)
	}
	var out struct {
		Platforms []PlatformTier `json:"platforms"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Platforms) != 2 {
		t.Fatalf("platforms = %+v", out.Platforms)
	}
	if out.Platforms[0].Tier != TierUser {
		t.Errorf("platform 0 = %+v, want the user tier", out.Platforms[0])
	}
	if !strings.Contains(out.Platforms[1].Reason, "spov credentials add") {
		t.Errorf("a missing credential does not say what to do: %+v", out.Platforms[1])
	}
}

// A user with their own credential must not get the deployment's forge.
func TestLaunchUsesTheOwnersForge(t *testing.T) {
	userForge := &fakeForge{platform: git.NewFake()}
	userForge.platform.Repos = []git.Repo{
		{Owner: "acme", Name: "app", CloneURL: "https://git.test/acme/app.git"},
	}
	var (
		mu         sync.Mutex
		askedFor   string
		selections int
	)
	counting := &countingForge{inner: userForge, onSelect: func() {
		mu.Lock()
		defer mu.Unlock()
		selections++
	}}
	f := newAPIFixtureWith(t, func(d *Deps) {
		d.UserForge = func(_ context.Context, orgID, userID string, _ TierPolicy) (Forge, []PlatformTier, error) {
			mu.Lock()
			askedFor = orgID + "/" + userID
			mu.Unlock()
			return counting, nil, nil
		}
	})
	id := f.create(repoReq("uses-user-forge"))
	f.events(id)
	mu.Lock()
	defer mu.Unlock()
	if askedFor != auth.LocalID+"/"+auth.LocalID {
		t.Errorf("the forge was resolved for %q", askedFor)
	}
	if selections == 0 {
		t.Error("the launch did not use the owner's forge")
	}
	// The deployment's own forge must not have been consulted at all.
	if f.forge.selections() != 0 {
		t.Error("the launch fell back to the org forge")
	}
}

// countingForge records that a per-user forge was the one used.
type countingForge struct {
	inner    Forge
	onSelect func()
}

func (c *countingForge) ListRepos(ctx context.Context) ([]git.PlatformRepo, []string, error) {
	return c.inner.ListRepos(ctx)
}

func (c *countingForge) Selections(ctx context.Context, refs []store.RepoRef) ([]git.Selection, error) {
	c.onSelect()
	return c.inner.Selections(ctx, refs)
}

func (c *countingForge) Platforms(ctx context.Context) ([]git.Platform, error) {
	return c.inner.Platforms(ctx)
}

// What the forge said about a token is stored with it and shown where
// it is listed, and the token never is.
//
// @decision(DL-041)
func TestAStoredTokenKeepsItsScopesAndExpiry(t *testing.T) {
	expires := time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC)
	f := credFixture(t, &fakeVerifier{as: "spov-bot", scopes: []string{"api"}, expires: expires})
	resp, raw := f.do(http.MethodPut, "/api/v1/me/credentials/gitlab:gitlab.com", map[string]string{"token": "glpat-secret-value"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put: %d %s", resp.StatusCode, raw)
	}
	_, list := f.do(http.MethodGet, "/api/v1/me/credentials", nil)
	var body struct {
		Credentials []CredentialView `json:"credentials"`
	}
	if err := json.Unmarshal(list, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Credentials) != 1 {
		t.Fatalf("credentials = %+v", body.Credentials)
	}
	got := body.Credentials[0]
	if got.Platform != "gitlab:gitlab.com" || got.VerifiedAs != "spov-bot" || !slices.Equal(got.Scopes, []string{"api"}) || !got.ExpiresAt.Equal(expires) {
		t.Errorf("view = %+v", got)
	}
	if strings.Contains(string(list), "glpat-secret-value") {
		t.Error("the listing carried the token")
	}
}
