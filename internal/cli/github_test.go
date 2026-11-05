package cli

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/git"
)

// githubCredentialJSON builds a stored GitHub App credential with a real
// key, since newPlatform parses it.
func githubCredentialJSON(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
	encoded, err := json.Marshal(git.GitHubCredential{AppID: "Iv23liTEST", PrivateKey: keyPEM})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// TestNewPlatformBuildsGitHub: a GitHub App credential builds a platform.
//
// @decision(DL-041)
func TestNewPlatformBuildsGitHub(t *testing.T) {
	p, err := newPlatform(context.Background(), config.Platform{
		Name: "gh", Kind: config.KindGitHub, Account: "alice",
	}, githubCredentialJSON(t))
	if err != nil {
		t.Fatalf("newPlatform: %v", err)
	}
	if p.Kind() != "github" {
		t.Errorf("Kind = %q", p.Kind())
	}
	if p.Host() != "github.com" {
		t.Errorf("Host = %q", p.Host())
	}
}

// TestNewPlatformBuildsGitHubPAT: a token-shaped credential is a user's
// own, and builds the PAT platform rather than being rejected as a
// malformed App credential.
//
// @decision(DL-042)
func TestNewPlatformBuildsGitHubPAT(t *testing.T) {
	p, err := newPlatform(context.Background(), config.Platform{
		Name: "gh", Kind: config.KindGitHub, Account: "alice",
	}, "github_pat_a_personal_access_token")
	if err != nil {
		t.Fatalf("newPlatform: %v", err)
	}
	if p.Kind() != "github" {
		t.Errorf("Kind = %q", p.Kind())
	}
	if _, ok := p.(*git.GitHubPAT); !ok {
		t.Errorf("newPlatform built %T, want *git.GitHubPAT", p)
	}
	// spov did not mint it and cannot narrow it; TokenScope must say so
	// rather than claiming the App's per-repo guarantee.
	if p.TokenScope().PerRepo {
		t.Error("a PAT platform claims per-repo scoping")
	}
}

// TestNewPlatformRejectsAMalformedGitHubCredential: something that is
// neither App JSON nor a recognisable token is still an error, and it
// should name the shape spov wants without echoing the secret.
//
// @decision(DL-041) @decision(DL-042)
func TestNewPlatformRejectsAMalformedGitHubCredential(t *testing.T) {
	_, err := newPlatform(context.Background(), config.Platform{
		Name: "gh", Kind: config.KindGitHub, Account: "alice",
	}, "not_a_token_and_not_json")
	if err == nil {
		t.Fatal("a malformed credential was accepted")
	}
	if !strings.Contains(err.Error(), "app_id") {
		t.Errorf("err = %v; it should show the expected credential shape", err)
	}
	if strings.Contains(err.Error(), "not_a_token") {
		t.Errorf("the error echoes the stored secret: %v", err)
	}
}
