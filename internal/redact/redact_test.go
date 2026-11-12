package redact

import (
	"errors"
	"strings"
	"testing"
)

// TestRedactStripsCredentialShapes: every message that crosses the API
// boundary goes through redact, so each shape a leak takes must be
// caught.
//
// @decision(DL-005)
func TestRedactStripsCredentialShapes(t *testing.T) {
	cases := map[string]string{
		"clone https://x-access-token:ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123@github.com/o/r failed": "ghp_",
		"GET https://forge.test/api?token=abc123def failed":                                     "abc123def",
		"authorization: Bearer eyJhbGciOiJSUzI1NiJ9.payload.sig rejected":                       "eyJ",
		"authorization: Basic b3BlbmNvZGU6c2VjcmV0 rejected":                                    "b3Bl",
		"modal: token ak-AbCdEfGh1234 / as-XyZ987654321 invalid":                                "ak-A",
		"github_pat_11ABCDEFG0123456789abcdefghijk expired":                                     "github_pat_11",
		"gitlab said glpat-xYz12_AbCdEfGh3456 is revoked":                                       "glpat-xYz",
		"deploy gldt-AbCdEfGh1234 failed":                                                       "gldt-Ab",
		"runner glrt-t1_AbCdEfGh1234 failed":                                                    "glrt-t1",
		"build glcbt-64_AbCdEfGh1234 failed":                                                    "glcbt-64",
		"opaque " + strings.Repeat("Q", 48) + " string":                                         strings.Repeat("Q", 48),
	}
	for in, secret := range cases {
		got := Error(errors.New(in))
		if strings.Contains(got, secret) {
			t.Errorf("Error(%q) = %q still contains %q", in, got, secret)
		}
		if !strings.Contains(got, "[redacted]") {
			t.Errorf("Error(%q) = %q redacted nothing", in, got)
		}
	}
	if got := Error(errors.New("sandbox: cpu must be positive, got 0")); got != "sandbox: cpu must be positive, got 0" {
		t.Errorf("a clean message was altered: %q", got)
	}
	if Error(nil) != "" {
		t.Error("nil error")
	}
}

// TestRedactStripsShortLabelledSecrets: a generated session password is
// long enough for the opaque-string rule, but a third-party secret in an
// upstream error may be short and only recognisable by its label.
//
// @decision(DL-005)
func TestRedactStripsShortLabelledSecrets(t *testing.T) {
	cases := map[string]string{
		"OPENCODE_PASSWORD=aB3dEfGh is wrong":  "aB3dEfGh",
		`{"password":"s3cretpw"} rejected`:     "s3cretpw",
		"api_key: sk-短 failed":                 "sk-",
		"forge said token = abc123 is expired": "abc123",
		"SECRET=hunter2":                       "hunter2",
	}
	for in, secret := range cases {
		got := Error(errors.New(in))
		if strings.Contains(got, secret) {
			t.Errorf("Error(%q) = %q still contains %q", in, got, secret)
		}
	}
	// The label survives, so the message still says what failed.
	if got := Error(errors.New("OPENCODE_PASSWORD=aB3dEfGh is wrong")); !strings.Contains(got, "OPENCODE_PASSWORD") {
		t.Errorf("redact removed the label too: %q", got)
	}
	for _, clean := range []string{
		"sandbox: cpu must be positive, got 0",
		"session: the sandbox has stopped",
		"modal: rate limited, retry in 3s",
	} {
		if got := Error(errors.New(clean)); got != clean {
			t.Errorf("a clean message was altered: %q -> %q", clean, got)
		}
	}
}
