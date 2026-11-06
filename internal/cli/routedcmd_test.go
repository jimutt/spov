package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/routedcred"
)

// Locally, `credentials add` with a route writes the secret to this
// machine's store and the route to the index, without a server, and the
// listing and `remove` read them back.
//
// @decision(DL-073)
func TestARoutedCredentialRoundTripsOnThisMachine(t *testing.T) {
	f := newLoginFixture(t)
	f.env.StateDir = t.TempDir()
	f.reply = "npm_realsecret"
	out, _, err := runWith(t, f.env, "credentials", "add", "acme-npm", "--host", "npm.acme.com", "--bearer")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "on this machine") || strings.Contains(out, "npm_realsecret") {
		t.Errorf("out = %q", out)
	}
	if got, err := f.store.Get(routedcred.Key("acme-npm")); err != nil || got != "npm_realsecret" {
		t.Errorf("stored %q, %v", got, err)
	}
	info, err := os.Stat(filepath.Join(f.env.StateDir, routedcred.IndexName))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("index = %v, %v", info, err)
	}
	out, _, err = runWith(t, f.env, "credentials")
	if err != nil || !strings.Contains(out, "acme-npm  npm.acme.com  bearer") {
		t.Errorf("listing = %q, %v", out, err)
	}
	out, _, err = runWith(t, f.env, "credentials", "--json")
	var body credentialsBody
	if err != nil || json.Unmarshal([]byte(out), &body) != nil || len(body.RoutedCredentials) != 1 {
		t.Errorf("json = %q, %v", out, err)
	}
	if out, _, err = runWith(t, f.env, "credentials", "remove", "acme-npm"); err != nil || !strings.Contains(out, "Removed acme-npm") {
		t.Errorf("remove = %q, %v", out, err)
	}
	if _, err := f.store.Get(routedcred.Key("acme-npm")); err == nil {
		t.Error("the secret outlived remove")
	}
}

// A routed credential's secret never comes from a flag; it comes from
// the prompt, a file or standard input.
//
// @decision(DL-073)
func TestARoutedSecretNeverComesFromAFlag(t *testing.T) {
	f := newLoginFixture(t)
	f.env.StateDir = t.TempDir()
	if _, _, err := runWith(t, f.env, "credentials", "add", "acme-npm", "--host", "npm.acme.com", "--bearer",
		"--token", "npm_realsecret"); err == nil {
		t.Fatal("a secret given as a flag was accepted")
	}
	file := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(file, []byte("from-the-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runWith(t, f.env, "credentials", "add", "route:acme-mvn", "--host", "repo.acme.com",
		"--basic", "ci", "--path", "/libs/", "--attach", "asked", "--secret-file", file); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.store.Get(routedcred.Key("acme-mvn")); got != "from-the-file" {
		t.Errorf("stored %q", got)
	}
	if len(f.prompted) != 0 {
		t.Errorf("prompted %v with a secret file", f.prompted)
	}
}

// A name that could also name a platform, or that is not a routed
// credential's name at all, is refused before anything is stored, as is
// a route the proxy would refuse.
//
// @decision(DL-073)
func TestARoutedCredentialNameCannotShadowAPlatform(t *testing.T) {
	f := newLoginFixture(t)
	f.env.StateDir = t.TempDir()
	f.reply = "s3cret"
	for _, args := range [][]string{
		{"codefloe", "--host", "npm.acme.com", "--bearer"},             // a configured platform
		{"github", "--host", "npm.acme.com", "--bearer"},               // GitHub's own name
		{"Acme_NPM", "--host", "npm.acme.com", "--bearer"},             // not a routed name
		{"acme", "--host", "*.acme.com", "--bearer"},                   // a wildcard
		{"acme", "--host", "npm.acme.com"},                             // no scheme
		{"acme", "--host", "npm.acme.com", "--bearer", "--basic", "u"}, // two schemes
		{"acme", "--bearer"},                                           // no host
		{"acme", "--host", "npm.acme.com", "--header", "Host"},         // a forbidden header
	} {
		if _, _, err := runWith(t, f.env, append([]string{"credentials", "add"}, args...)...); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
	if _, err := os.Stat(filepath.Join(f.env.StateDir, routedcred.IndexName)); err == nil {
		t.Error("a refused credential reached the index")
	}
}

// On a hosted server the routed credential goes to the server, which
// lists it by its route alone.
//
// @decision(DL-073)
func TestARoutedCredentialIsStoredOnAHostedServer(t *testing.T) {
	f := keyedFixture(t)
	f.env.PromptSecret = func(string) (string, error) { return "npm_realsecret", nil }
	out, _, err := runWith(t, f.env, "credentials", "add", "acme-npm", "--host", "npm.acme.com", "--bearer")
	if err != nil || !strings.Contains(out, "on the server") {
		t.Fatalf("add = %q, %v", out, err)
	}
	out, _, err = runWith(t, f.env, "credentials")
	if err != nil || !strings.Contains(out, "acme-npm  npm.acme.com  bearer") || strings.Contains(out, "npm_realsecret") {
		t.Errorf("listing = %q, %v", out, err)
	}
	if out, _, err = runWith(t, f.env, "credentials", "remove", "acme-npm"); err != nil || !strings.Contains(out, "Removed acme-npm from the server") {
		t.Errorf("remove = %q, %v", out, err)
	}
}

// --org stores the organisation's routed credential, which a local
// server, having no organisation, refuses before reading the secret.
//
// @decision(DL-074)
func TestAnOrgRoutedCredentialNeedsAHostedServer(t *testing.T) {
	f := newLoginFixture(t)
	f.env.StateDir = t.TempDir()
	f.reply = "s3cret"
	if _, _, err := runWith(t, f.env, "credentials", "add", "shared", "--org", "--host", "npm.acme.com", "--bearer"); err == nil ||
		!strings.Contains(err.Error(), "hosted") {
		t.Errorf("err = %v", err)
	}
	if _, _, err := runWith(t, f.env, "credentials", "remove", "shared", "--org"); err == nil {
		t.Error("a local remove --org was accepted")
	}
	if len(f.prompted) != 0 {
		t.Errorf("prompted %v", f.prompted)
	}
}
