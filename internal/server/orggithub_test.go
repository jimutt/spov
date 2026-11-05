package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/store"
)

// fakeOrgGitHub is a deployment with one GitHub platform, whose check
// answers status unless fail is set.
type fakeOrgGitHub struct {
	fromEnv bool
	fail    error
	checked []git.GitHubCredential
}

func (f *fakeOrgGitHub) Platforms() []OrgGitHubPlatform {
	p := OrgGitHubPlatform{Name: "gh", Account: "configured"}
	if f.fromEnv {
		p.FromEnv, p.Env = true, git.GitHubCredential{AppID: "env-app", PrivateKey: "envpem"}
	}
	return []OrgGitHubPlatform{p}
}

func (f *fakeOrgGitHub) Check(_ context.Context, _ OrgGitHubPlatform, cred git.GitHubCredential, account string) (git.AppStatus, error) {
	f.checked = append(f.checked, cred)
	if f.fail != nil {
		return git.AppStatus{}, f.fail
	}
	return git.AppStatus{InstallationID: 77, Account: account, RepoCount: 2, Repos: []string{"acme/a", "acme/b"},
		Missing: []string{"issues:write"}}, nil
}

func orgFixture(t *testing.T, admin bool, gh *fakeOrgGitHub) *apiFixture {
	t.Helper()
	sealer, err := auth.NewSealer(map[int]string{1: base64.StdEncoding.EncodeToString(make([]byte, auth.KeyBytes))})
	if err != nil {
		t.Fatal(err)
	}
	return allowlistFixtureWith(t, admin, map[string]bool{"me@example.com": admin}, func(d *Deps) {
		d.OrgApps = &OrgApps{Store: d.Store, Sealer: sealer}
		d.OrgGitHub = gh
	})
}

type orgBody struct {
	Platforms []OrgGitHubView  `json:"platforms"`
	Events    []AdminEventView `json:"events"`
}

func decodeOrg(t *testing.T, raw []byte) orgBody {
	t.Helper()
	var b orgBody
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	return b
}

// An admin stores an App only after it checks out; the status then says
// where it is installed and what it reaches, the audit trail says who
// stored it, and the key is in neither.
//
// @decision(DL-042)
func TestAnAdminStoresACheckedApp(t *testing.T) {
	gh := &fakeOrgGitHub{}
	f := orgFixture(t, true, gh)
	resp, raw := f.do(http.MethodGet, "/api/v1/org/github", nil)
	if resp.StatusCode != http.StatusOK || decodeOrg(t, raw).Platforms[0].Source != "none" {
		t.Fatalf("before: %d %s", resp.StatusCode, raw)
	}
	resp, raw = f.do(http.MethodPut, "/api/v1/org/github/gh", OrgGitHubApp{AppID: "123", PrivateKey: "-----BEGIN KEY-----secret", Account: "acme"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("store: %d %s", resp.StatusCode, raw)
	}
	if strings.Contains(string(raw), "secret") {
		t.Fatalf("the key came back: %s", raw)
	}
	b := decodeOrg(t, raw)
	v := b.Platforms[0]
	if v.Source != "stored" || v.AppID != "123" || v.Account != "acme" || v.InstallationID != 77 ||
		v.Check == nil || v.Check.RepoCount != 2 || v.UpdatedBy != "me@example.com" {
		t.Errorf("view = %+v %+v", v, v.Check)
	}
	if len(b.Events) != 1 || b.Events[0].Action != AdminGitHubAppStored || strings.Contains(b.Events[0].Detail, "secret") {
		t.Errorf("events = %+v", b.Events)
	}
	app, found, err := f.srv.deps.OrgApps.Get(context.Background(), auth.LocalID, "gh")
	if err != nil || !found || app.Credential.PrivateKey != "-----BEGIN KEY-----secret" || app.Credential.InstallationID != 77 {
		t.Errorf("stored %+v %v %v", app, found, err)
	}
	// Removed: gone, and audited.
	resp, raw = f.do(http.MethodDelete, "/api/v1/org/github/gh", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("remove: %d %s", resp.StatusCode, raw)
	}
	b = decodeOrg(t, raw)
	if b.Platforms[0].Source != "none" || len(b.Events) != 2 || b.Events[0].Action != AdminGitHubAppRemoved {
		t.Errorf("after removal: %+v", b)
	}
	if resp, _ := f.do(http.MethodDelete, "/api/v1/org/github/gh", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a second removal = %d", resp.StatusCode)
	}
}

// An App that does not check out is not stored, and says why.
//
// @decision(DL-042)
func TestAnAppThatFailsItsCheckIsNotStored(t *testing.T) {
	gh := &fakeOrgGitHub{fail: errors.New("finding the installation: not installed")}
	f := orgFixture(t, true, gh)
	resp, raw := f.do(http.MethodPut, "/api/v1/org/github/gh", OrgGitHubApp{AppID: "1", PrivateKey: "k"})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(raw), "not installed") {
		t.Fatalf("= %d %s", resp.StatusCode, raw)
	}
	if _, err := f.store.OrgCredentials().Get(context.Background(), auth.LocalID, "gh"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a failed App was stored: %v", err)
	}
	if len(gh.checked) != 1 || gh.checked[0].AppID != "1" {
		t.Errorf("checked %+v", gh.checked)
	}
}

// The environment's App is shown and never changed here; a non-admin
// can neither see nor change it; an unknown platform is a 404.
//
// @decision(DL-042)
func TestTheAppIsAdminOnlyAndTheEnvironmentWins(t *testing.T) {
	env := orgFixture(t, true, &fakeOrgGitHub{fromEnv: true})
	resp, raw := env.do(http.MethodGet, "/api/v1/org/github", nil)
	if v := decodeOrg(t, raw).Platforms[0]; resp.StatusCode != http.StatusOK || v.Source != "environment" || v.AppID != "env-app" {
		t.Errorf("env view = %d %s", resp.StatusCode, raw)
	}
	if resp, _ := env.do(http.MethodPut, "/api/v1/org/github/gh", OrgGitHubApp{AppID: "1", PrivateKey: "k"}); resp.StatusCode != http.StatusConflict {
		t.Errorf("store over the environment = %d", resp.StatusCode)
	}
	if resp, _ := env.do(http.MethodPut, "/api/v1/org/github/other", OrgGitHubApp{AppID: "1", PrivateKey: "k"}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown platform = %d", resp.StatusCode)
	}
	user := orgFixture(t, false, &fakeOrgGitHub{})
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		path := "/api/v1/org/github"
		var body any
		if m != http.MethodGet {
			path += "/gh"
			body = OrgGitHubApp{AppID: "1", PrivateKey: "k"}
		}
		if resp, _ := user.do(m, path, body); resp.StatusCode != http.StatusForbidden {
			t.Errorf("a non-admin %s = %d", m, resp.StatusCode)
		}
	}
	local := newAPIFixture(t)
	if resp, _ := local.do(http.MethodGet, "/api/v1/org/github", nil); resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("a local server = %d", resp.StatusCode)
	}
}

// A stored App opens only under its own org and platform: the org AAD
// keeps it apart from every user row.
//
// @decision(DL-042)
func TestAStoredAppOpensOnlyWhereItWasSealed(t *testing.T) {
	st := store.NewMemory()
	sealer, _ := auth.NewSealer(map[int]string{1: base64.StdEncoding.EncodeToString(make([]byte, auth.KeyBytes))})
	apps := &OrgApps{Store: st, Sealer: sealer}
	row, err := apps.Sealed("org", "gh", "a", git.GitHubCredential{AppID: "1", PrivateKey: "k"}, OrgAppMeta{AppID: "1"})
	if err != nil {
		t.Fatal(err)
	}
	row.Platform = "other"
	_ = st.OrgCredentials().Put(context.Background(), row)
	if _, _, err := apps.Get(context.Background(), "org", "other"); !errors.Is(err, ErrOrgAppUnreadable) {
		t.Errorf("a row moved to another platform opened: %v", err)
	}
	if _, err := sealer.Open(row.Ciphertext, row.Nonce, row.KeyVersion, auth.CredentialAAD("org", "", "gh")); err == nil {
		t.Error("an org row opened as a user's")
	}
}

// A stored App that cannot be opened is shown as such, not as a broken
// page, and can still be removed.
//
// @decision(DL-042)
func TestAnUnreadableAppIsShownAndCanBeRemoved(t *testing.T) {
	f := orgFixture(t, true, &fakeOrgGitHub{})
	other, _ := auth.NewSealer(map[int]string{1: "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="})
	row, err := (&OrgApps{Sealer: other}).Sealed(auth.LocalID, "gh", "x", git.GitHubCredential{AppID: "1", PrivateKey: "k"}, OrgAppMeta{AppID: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.OrgCredentials().Put(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	resp, raw := f.do(http.MethodGet, "/api/v1/org/github", nil)
	v := decodeOrg(t, raw).Platforms[0]
	if resp.StatusCode != http.StatusOK || v.Source != "stored" || v.Check == nil || !strings.Contains(v.Check.Error, "cannot be opened") {
		t.Fatalf("= %d %s", resp.StatusCode, raw)
	}
	if resp, raw := f.do(http.MethodDelete, "/api/v1/org/github/gh", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("remove = %d %s", resp.StatusCode, raw)
	}
}
