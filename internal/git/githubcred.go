package git

import (
	"encoding/json"
	"fmt"
	"strings"
)

// GitHubCredential is what spov stores under `platform:<name>` for a
// GitHub App, as JSON since the credential store holds one string per key
// and a GitHub App needs several values.
//
// @decision(DL-041) @decision(DL-008)
type GitHubCredential struct {
	// AppID is the App ID or client ID. GitHub accepts either as the JWT
	// issuer and recommends the client ID.
	AppID string `json:"app_id"`
	// PrivateKey is the App's PEM-encoded RSA private key.
	PrivateKey string `json:"private_key"`
	// InstallationID skips installation discovery when set.
	InstallationID int64 `json:"installation_id,omitempty"`
}

// EncodeGitHubCredential builds the stored form of an App credential from
// the parts a deployment supplies as separate variables. The installation
// is discovered from the account at mint time, so it is not encoded here.
//
// @decision(DL-042) @decision(DL-041)
func EncodeGitHubCredential(appID, privateKey string) string {
	encoded, err := json.Marshal(GitHubCredential{AppID: appID, PrivateKey: privateKey})
	if err != nil {
		// The struct is three strings; marshalling cannot fail.
		return ""
	}
	return string(encoded)
}

// HostFromURL is the host a base URL names, with its port.
//
// @decision(DL-042)
func HostFromURL(raw string) (string, error) { return hostFromURL(raw) }

// ParseGitHubCredential reads a stored GitHub App credential.
//
// Errors never quote the stored value: it holds a private key.
//
// @decision(DL-041)
func ParseGitHubCredential(stored string) (GitHubCredential, error) {
	var cred GitHubCredential
	trimmed := strings.TrimSpace(stored)
	if trimmed == "" {
		return cred, fmt.Errorf("git: the stored GitHub credential is empty")
	}
	// A stored Forgejo-style bare token is the likely mistake here, and
	// "invalid character" would not explain it.
	if !strings.HasPrefix(trimmed, "{") {
		return cred, fmt.Errorf("git: the stored GitHub credential is not JSON; " +
			"a GitHub App needs {\"app_id\": \"...\", \"private_key\": \"-----BEGIN RSA PRIVATE KEY-----\\n...\"}")
	}
	if err := json.Unmarshal([]byte(trimmed), &cred); err != nil {
		return GitHubCredential{}, fmt.Errorf("git: the stored GitHub credential is not valid JSON")
	}
	if cred.AppID == "" {
		return GitHubCredential{}, fmt.Errorf("git: the stored GitHub credential has no app_id")
	}
	if cred.PrivateKey == "" {
		return GitHubCredential{}, fmt.Errorf("git: the stored GitHub credential has no private_key")
	}
	return cred, nil
}
