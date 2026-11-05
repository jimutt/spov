package git

import (
	"strings"
	"testing"
	"time"
)

func TestCloneURLWithToken(t *testing.T) {
	tok := Token{Username: "alice", Secret: "s3cret"}

	got, err := CloneURLWithToken("https://codefloe.com/alice/spov.git", tok)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://alice:s3cret@codefloe.com/alice/spov.git"
	if got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
}

// distinctiveSecret is deliberately unlike any word that could appear in
// an error message, so a leak assertion cannot pass by accident.
const distinctiveSecret = "gto-zzq7-do-not-log-me"

func TestCloneURLWithTokenRejects(t *testing.T) {
	tests := []struct {
		name string
		url  string
		tok  Token
		want string
	}{
		{"non-https", "http://codefloe.com/o/r.git", Token{Username: "u", Secret: distinctiveSecret}, "not https"},
		{"ssh url", "ssh://git@codefloe.com/o/r.git", Token{Username: "u", Secret: distinctiveSecret}, "not https"},
		{"no username", "https://codefloe.com/o/r.git", Token{Secret: distinctiveSecret}, "incomplete"},
		{"no secret", "https://codefloe.com/o/r.git", Token{Username: "u"}, "incomplete"},
		// A ':' or '@' would re-split the userinfo and could redirect the
		// clone to another host.
		{"username with @", "https://codefloe.com/o/r.git", Token{Username: "u@evil.com", Secret: distinctiveSecret}, "cannot appear in a URL"},
		{"secret with @", "https://codefloe.com/o/r.git", Token{Username: "u", Secret: distinctiveSecret + "@evil.com"}, "cannot appear in a URL"},
		{"secret with :", "https://codefloe.com/o/r.git", Token{Username: "u", Secret: "a:" + distinctiveSecret}, "cannot appear in a URL"},
		{"secret with /", "https://codefloe.com/o/r.git", Token{Username: "u", Secret: "a/" + distinctiveSecret}, "cannot appear in a URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := CloneURLWithToken(tt.url, tt.tok)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
			// The error may be logged.
			if strings.Contains(err.Error(), tt.tok.Secret) && tt.tok.Secret != "" {
				t.Errorf("the error leaked the secret: %q", err)
			}
		})
	}
}

// TestCloneURLCannotRedirectHost is the property behind the character
// checks: a crafted credential must not be able to point the clone
// somewhere else.
func TestCloneURLCannotRedirectHost(t *testing.T) {
	_, err := CloneURLWithToken("https://codefloe.com/o/r.git", Token{
		Username: "u",
		Secret:   "p@evil.example.com/x",
	})
	if err == nil {
		t.Fatal("a credential that would redirect the clone host was accepted")
	}
}

func TestRedactURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"credentialed", "https://alice:s3cret@codefloe.com/o/r.git", "https://alice:***@codefloe.com/o/r.git"},
		{"no credentials", "https://codefloe.com/o/r.git", "https://codefloe.com/o/r.git"},
		{"username only", "https://alice@codefloe.com/o/r.git", "https://alice@codefloe.com/o/r.git"},
		{"not a url", "not a url", "not a url"},
		{"ssh", "ssh://git@codefloe.com/o/r.git", "ssh://git@codefloe.com/o/r.git"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RedactURL(tt.in); got != tt.want {
				t.Errorf("RedactURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestRedactURLRoundTrip: whatever CloneURLWithToken produces must redact
// cleanly, since that is the URL that gets logged.
func TestRedactURLRoundTrip(t *testing.T) {
	const secret = "gto_abcdef1234567890"
	url, err := CloneURLWithToken("https://codefloe.com/alice/spov.git", Token{Username: "alice", Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(url, secret) {
		t.Fatal("the token was not embedded")
	}
	if red := RedactURL(url); strings.Contains(red, secret) {
		t.Errorf("RedactURL left the secret in: %q", red)
	}
}

func TestRepoSlug(t *testing.T) {
	if got := (Repo{Owner: "alice", Name: "spov"}).Slug(); got != "alice/spov" {
		t.Errorf("Slug = %q", got)
	}
}

func TestTokenExpired(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		tok  Token
		want bool
	}{
		{"no expiry", Token{}, false},
		{"future", Token{ExpiresAt: now.Add(time.Hour)}, false},
		{"past", Token{ExpiresAt: now.Add(-time.Second)}, true},
		{"exactly now", Token{ExpiresAt: now}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.tok.Expired(now); got != tt.want {
				t.Errorf("Expired = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestRedactAll covers free text, which is where RedactURL is not enough:
// git output can name a remote more than once, and RedactURL only handles
// the first.
//
// @decision(DL-041)
func TestRedactAll(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			"two credentials in one line",
			"fatal: could not read from https://u:" + distinctiveSecret + "@h/a.git and https://u2:" + distinctiveSecret + "2@h/b.git",
			"fatal: could not read from https://u:***@h/a.git and https://u2:***@h/b.git",
		},
		{"no credentials", "fatal: repository not found at https://h/a.git", "fatal: repository not found at https://h/a.git"},
		{"plain text", "something went wrong", "something went wrong"},
		{"multiline", "line one https://u:" + distinctiveSecret + "@h/a.git\nline two", "line one https://u:***@h/a.git\nline two"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RedactAll(tt.in)
			if got != tt.want {
				t.Errorf("RedactAll = %q, want %q", got, tt.want)
			}
			if strings.Contains(got, distinctiveSecret) {
				t.Errorf("a secret survived redaction: %q", got)
			}
		})
	}
}

// TestRedactURLHandlesOneURLOnly documents the split of responsibility, so
// nobody reaches for RedactURL when scrubbing log output.
func TestRedactURLHandlesOneURLOnly(t *testing.T) {
	text := "a https://u:" + distinctiveSecret + "@h/x.git b https://u:" + distinctiveSecret + "@h/y.git"
	if !strings.Contains(RedactURL(text), distinctiveSecret) {
		t.Skip("RedactURL now handles free text; fold RedactAll into it")
	}
	if strings.Contains(RedactAll(text), distinctiveSecret) {
		t.Error("RedactAll left a secret in free text")
	}
}
