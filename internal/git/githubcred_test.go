package git

import (
	"strings"
	"testing"
)

func TestParseGitHubCredential(t *testing.T) {
	cred, err := ParseGitHubCredential(`{
		"app_id": "Iv23liABC",
		"private_key": "-----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIVATE KEY-----\n",
		"installation_id": 12345
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if cred.AppID != "Iv23liABC" {
		t.Errorf("AppID = %q", cred.AppID)
	}
	if !strings.Contains(cred.PrivateKey, "BEGIN RSA PRIVATE KEY") {
		t.Errorf("the private key did not survive JSON decoding: %q", cred.PrivateKey)
	}
	if cred.InstallationID != 12345 {
		t.Errorf("InstallationID = %d", cred.InstallationID)
	}
}

func TestParseGitHubCredentialOptionalInstallation(t *testing.T) {
	cred, err := ParseGitHubCredential(`{"app_id":"a","private_key":"k"}`)
	if err != nil {
		t.Fatal(err)
	}
	if cred.InstallationID != 0 {
		t.Errorf("InstallationID = %d, want 0 when absent", cred.InstallationID)
	}
}

// TestParseGitHubCredentialExplainsABareToken: pasting a Forgejo-style
// token is the likely mistake, and a JSON syntax error would not explain
// it.
//
// @decision(DL-041)
func TestParseGitHubCredentialExplainsABareToken(t *testing.T) {
	_, err := ParseGitHubCredential("ghp_a_pasted_personal_access_token")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "app_id") {
		t.Errorf("err = %v; it should show the expected shape", err)
	}
}

// TestParseGitHubCredentialNeverQuotesTheStoredValue: it holds a private
// key, and errors reach logs.
//
// @decision(DL-041)
func TestParseGitHubCredentialNeverQuotesTheStoredValue(t *testing.T) {
	secret := "SUPERSECRETKEYMATERIAL"
	for _, stored := range []string{
		secret,
		`{"app_id":"a","private_key":"` + secret + `"` + `,}`, // malformed JSON
		`{"private_key":"` + secret + `"}`,                    // no app_id
		`{"app_id":"a"}`,                                      // no key
	} {
		_, err := ParseGitHubCredential(stored)
		if err == nil {
			t.Errorf("stored %q was accepted", stored)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the error carries the stored value: %v", err)
		}
	}
}

func TestParseGitHubCredentialEmpty(t *testing.T) {
	if _, err := ParseGitHubCredential("   "); err == nil {
		t.Fatal("an empty credential was accepted")
	}
}

// EncodeGitHubCredential is what assembles a deployment's App
// credential from its separate environment variables.
//
// @decision(DL-042)
func TestEncodeGitHubCredential(t *testing.T) {
	const key = "-----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIVATE KEY-----"
	encoded := EncodeGitHubCredential("12345", key)
	cred, err := ParseGitHubCredential(encoded)
	if err != nil {
		t.Fatalf("the encoded credential does not parse: %v", err)
	}
	if cred.AppID != "12345" {
		t.Errorf("AppID = %q", cred.AppID)
	}
	if cred.PrivateKey != key {
		t.Errorf("PrivateKey = %q", cred.PrivateKey)
	}
}

func TestExportedHostFromURL(t *testing.T) {
	got, err := HostFromURL("https://codefloe.com/some/path")
	if err != nil {
		t.Fatalf("HostFromURL: %v", err)
	}
	if got != "codefloe.com" {
		t.Errorf("HostFromURL = %q", got)
	}
	// A port is part of the host.
	if got, err := HostFromURL("http://localhost:3000"); err != nil || got != "localhost:3000" {
		t.Errorf("HostFromURL = %q, %v", got, err)
	}
	if _, err := HostFromURL("ftp://nope"); err == nil {
		t.Error("a non-http scheme was accepted")
	}
}
