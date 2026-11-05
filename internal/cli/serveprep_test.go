package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/credentials"
	"github.com/jimutt/spov/internal/prep"
)

// prepEnv builds an Env whose environment is the given map and whose
// output is captured.
func prepEnv(t *testing.T, environ map[string]string) (*Env, *bytes.Buffer, *[]string) {
	t.Helper()
	var errBuf bytes.Buffer
	var warnings []string
	if environ == nil {
		environ = map[string]string{}
	}
	e := &Env{Err: &errBuf, Environ: config.MapEnv{Vars: environ}}
	return e, &errBuf, &warnings
}

// TestNoProviderIsANoteAndNotAWarning pins that "no provider" and "a
// misconfigured one" are reported differently.
//
// @decision(DL-047)
func TestNoProviderIsANoteAndNotAWarning(t *testing.T) {
	e, out, warnings := prepEnv(t, nil)
	warn := func(m string) { *warnings = append(*warnings, m) }

	if got := e.prepareGoal(prep.Config{}, nil, warn); got != nil {
		t.Error("built a preparation pass with no provider key")
	}
	if len(*warnings) != 0 {
		t.Errorf("an unconfigured deployment warned: %v", *warnings)
	}
	// It is still SAID, so an operator can tell the pass is not running.
	if !strings.Contains(out.String(), "not prepared") {
		t.Errorf("nothing said the pass is not running: %q", out.String())
	}
	// And it names how to turn it on.
	if !strings.Contains(out.String(), prep.OpenRouterKeyEnv) {
		t.Error("the note does not say which variable would enable the pass")
	}
}

// A misconfiguration is the other half: something was asked for and
// could not be built. It is a fault and is warned about.
//
// @decision(DL-047)
func TestAMisconfiguredPassWarnsRatherThanPassingQuietly(t *testing.T) {
	e, _, warnings := prepEnv(t, map[string]string{prep.OpenRouterKeyEnv: "k"})
	warn := func(m string) { *warnings = append(*warnings, m) }

	// A base URL with no model: spov cannot know what the gateway
	// serves.
	if got := e.prepareGoal(prep.Config{BaseURL: "https://gw.internal/v1"}, nil, warn); got != nil {
		t.Error("built a pass from a configuration it could not resolve")
	}
	if len(*warnings) == 0 {
		t.Error("a misconfiguration passed quietly; it is indistinguishable from having no key at all")
	}
}

// The startup line names the model and endpoint so an operator can see
// what they are paying for. It must never name the key.
//
// @decision(DL-047)
func TestTheStartupLineNamesTheModelAndNeverTheKey(t *testing.T) {
	e, out, warnings := prepEnv(t, map[string]string{prep.OpenRouterKeyEnv: "sk-super-secret"})
	warn := func(m string) { *warnings = append(*warnings, m) }

	if got := e.prepareGoal(prep.Config{}, nil, warn); got == nil {
		t.Fatal("built no pass despite a usable key")
	}
	line := out.String()
	if strings.Contains(line, "sk-super-secret") {
		t.Fatalf("the startup line leaked the key: %q", line)
	}
	if !strings.Contains(line, prep.OpenRouterModel) {
		t.Errorf("the startup line does not name the model: %q", line)
	}
	if len(*warnings) != 0 {
		t.Errorf("a working configuration warned: %v", *warnings)
	}
}

// A key stored by `spov credentials add OPENROUTER_API_KEY` is found.
//
// @decision(DL-047) @decision(DL-008)
func TestAStoredProviderKeyIsFoundWhenTheEnvironmentHasNone(t *testing.T) {
	creds := credentials.NewMemoryStore()
	key, err := credentials.ProviderKey(prep.OpenRouterKeyEnv)
	if err != nil {
		t.Fatal(err)
	}
	if err := creds.Set(key, "sk-stored"); err != nil {
		t.Fatal(err)
	}

	e, out, warnings := prepEnv(t, nil)
	e.Credentials = creds
	warn := func(m string) { *warnings = append(*warnings, m) }

	if got := e.prepareGoal(prep.Config{}, nil, warn); got == nil {
		t.Fatal("built no pass despite a stored key; the deployment would be told none was configured")
	}
	if strings.Contains(out.String(), "no model provider key") {
		t.Errorf("said no key was configured with one in the store: %q", out.String())
	}
	if strings.Contains(out.String(), "sk-stored") {
		t.Errorf("the startup line leaked the stored key: %q", out.String())
	}
}

// The environment WINS over the store, so an exported key is always the
// one used and a stored key only fills a gap. Reversing it would make a
// forgotten stored key silently outrank a deliberate export.
//
// @decision(DL-047) @decision(DL-008)
func TestAnExportedKeyOutranksAStoredOne(t *testing.T) {
	creds := credentials.NewMemoryStore()
	key, err := credentials.ProviderKey(prep.OpenRouterKeyEnv)
	if err != nil {
		t.Fatal(err)
	}
	if err := creds.Set(key, "sk-stored"); err != nil {
		t.Fatal(err)
	}

	e, _, _ := prepEnv(t, map[string]string{prep.OpenRouterKeyEnv: "sk-exported"})
	e.Credentials = creds

	if got := e.prepKey(prep.OpenRouterKeyEnv); got != "sk-exported" {
		t.Errorf("prepKey chose the stored key over the exported one")
	}
}

// A missing or unreadable store is not a startup failure: the
// environment may well have answered, and a locked keyring is a support
// problem rather than a reason to refuse to start.
//
// @decision(DL-047)
func TestAnUnreadableCredentialStoreIsNotFatal(t *testing.T) {
	e, _, warnings := prepEnv(t, map[string]string{prep.OpenRouterKeyEnv: "sk-exported"})
	e.Credentials = nil // no store at all
	warn := func(m string) { *warnings = append(*warnings, m) }

	if got := e.prepareGoal(prep.Config{}, nil, warn); got == nil {
		t.Error("a missing credential store stopped the pass being built from the environment")
	}
	if len(*warnings) != 0 {
		t.Errorf("a missing store warned: %v", *warnings)
	}
}
