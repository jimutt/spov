package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/store"
)

// sandbox.plugins is sent with a launch, --plugin replaces it, and a
// spec that is not an npm package is refused before the server is
// asked.
//
// @decision(DL-171)
func TestFlySendsThePluginsAndTheFlagReplacesTheConfig(t *testing.T) {
	f := newAPIFixtureWithConfig(t, defaultConfig+"sandbox:\n  plugins: [envsitter-guard@0.1.1, opencode-rules]\n")
	if _, stderr, err := runWith(t, f.env, "fly", "--keep", "--name", "configured"); err != nil {
		t.Fatalf("fly: %v\n%s", err, stderr)
	}
	if got := f.session("configured").Request.Plugins; !slices.Equal(got, []string{"envsitter-guard@0.1.1", "opencode-rules"}) {
		t.Errorf("the configured plugins were sent as %v", got)
	}
	if _, stderr, err := runWith(t, f.env, "fly", "--keep", "--name", "flagged", "--plugin", "@scope/one@2.0.0"); err != nil {
		t.Fatalf("fly: %v\n%s", err, stderr)
	}
	if got := f.session("flagged").Request.Plugins; !slices.Equal(got, []string{"@scope/one@2.0.0"}) {
		t.Errorf("--plugin did not replace the configured list: %v", got)
	}
	created := len(f.provider.Created)
	_, _, err := runWith(t, f.env, "fly", "--keep", "--name", "refused", "--plugin", "git+https://example.com/x")
	if err == nil || !strings.Contains(err.Error(), "plugin") {
		t.Errorf("a git spec: %v", err)
	}
	if len(f.provider.Created) != created {
		t.Error("a sandbox was created for a refused plugin")
	}
}

// A template takes the place of the config files' plugins, and says so;
// a --plugin flag still wins.
//
// @decision(DL-171) @decision(DL-122)
func TestATemplateTakesThePlaceOfTheConfiguredPlugins(t *testing.T) {
	f := newAPIFixtureWithConfig(t, defaultConfig+"sandbox:\n  plugins: [from-config]\n")
	f.putTemplate("acme", store.TemplateSettings{Plugins: []string{"from-template"}})
	_, stderr, err := runWith(t, f.env, "fly", "--keep", "--name", "templated", "--template", "acme")
	if err != nil {
		t.Fatalf("fly: %v\n%s", err, stderr)
	}
	if got := f.session("templated").Request.Plugins; !slices.Equal(got, []string{"from-template"}) {
		t.Errorf("plugins %v: want the template's", got)
	}
	if !strings.Contains(stderr, "sandbox.plugins") {
		t.Errorf("the launch does not say the template replaced sandbox.plugins:\n%s", stderr)
	}
	if _, stderr, err := runWith(t, f.env, "fly", "--keep", "--name", "flag", "--template", "acme", "--plugin", "from-flag"); err != nil {
		t.Fatalf("fly: %v\n%s", err, stderr)
	}
	if got := f.session("flag").Request.Plugins; !slices.Equal(got, []string{"from-flag"}) {
		t.Errorf("plugins %v: want the flag's", got)
	}
}
