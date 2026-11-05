package toolchain

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestParse: a bare name gets its default, the result is sorted, and
// anything spov has no recipe for is refused before any build.
//
// @decision(DL-033)
func TestParse(t *testing.T) {
	t.Run("defaults, trimming and order", func(t *testing.T) {
		got, err := Parse([]string{" rust ", "java@17", "", "dotnet"})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"dotnet@" + mustAddon(t, "dotnet").Default, "java@17", "rust@" + mustAddon(t, "rust").Default}
		if !slices.Equal(Strings(got), want) {
			t.Errorf("Parse = %v, want %v", Strings(got), want)
		}
	})
	t.Run("nothing asked for is nothing", func(t *testing.T) {
		if got, err := Parse(nil); err != nil || len(got) != 0 {
			t.Errorf("Parse(nil) = %v, %v", got, err)
		}
	})
	for _, tt := range []struct{ name, entry, want string }{
		{"an unknown name lists the add-ons", "docker", strings.Join(Names(), ", ")},
		{"a version the add-on does not offer", "postgres@16.4", "does not offer version"},
		{"php wants major.minor", "php@8", "does not offer version"},
		{"a language takes a version, not a word", "node@lts", "does not offer version"},
		{"a version that is not safe in a build command", "rust@1.0;rm -rf /", "does not offer version"},
		{"a name is not a version", "ruby@latest", "does not offer version"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]string{tt.entry})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Parse(%q) = %v, want an error containing %q", tt.entry, err, tt.want)
			}
		})
	}
	t.Run("a name given twice", func(t *testing.T) {
		if _, err := Parse([]string{"rust", "rust@1.98.0"}); err == nil {
			t.Error("a name given twice was accepted")
		}
	})
}

func mustAddon(t *testing.T, name string) *Addon {
	t.Helper()
	a, ok := lookup(name)
	if !ok {
		t.Fatalf("no add-on %s", name)
	}
	return a
}

// TestEveryAddonIsUsableAsItsDefault: each recipe accepts its own
// default, renders one-line build commands, and has a guide note.
//
// @decision(DL-033)
func TestEveryAddonIsUsableAsItsDefault(t *testing.T) {
	safe := regexp.MustCompile(`^[A-Za-z0-9.+-]+$`)
	for _, name := range Names() {
		a := mustAddon(t, name)
		if !a.versions.MatchString(a.Default) {
			t.Errorf("%s does not accept its own default %q", name, a.Default)
		}
		if !safe.MatchString(a.Default) {
			t.Errorf("%s's default %q is not safe in a build command", name, a.Default)
		}
		sel := Selection{Name: name, Version: a.Default}
		layer := sel.Layer()
		if len(layer) == 0 {
			t.Errorf("%s renders no build command", name)
		}
		for _, c := range layer {
			if !strings.HasPrefix(c, "RUN ") || strings.Contains(c, "\n") {
				t.Errorf("%s renders %q, which is not one RUN line", name, c)
			}
		}
		if sel.Note() == "" {
			t.Errorf("%s has no guide note", name)
		}
	}
}

// TestTheDefaultsArePinnedAndChecked: the build installs each default at
// its exact version, checks mise against its sha256 for every target,
// and fails unless each tool reports its pin.
//
// @decision(DL-032)
func TestTheDefaultsArePinnedAndChecked(t *testing.T) {
	var all []string
	for _, l := range Layers() {
		all = append(all, l...)
	}
	joined := strings.Join(all, "\n")
	for _, tool := range Defaults {
		if !strings.Contains(joined, tool.Name+"@"+tool.Version) {
			t.Errorf("%s is not installed at its pin %s", tool.Name, tool.Version)
		}
		if !strings.Contains(Checks(), tool.Version) {
			t.Errorf("the checks do not verify %s %s", tool.Name, tool.Version)
		}
	}
	for _, target := range miseTargets {
		if !strings.Contains(joined, target.SHA256) || len(target.SHA256) != 64 {
			t.Errorf("mise for %s is not checked against its sha256", target.Arch)
		}
	}
	for _, want := range []string{"sha256sum -c", "corepack@" + CorepackVersion, "v" + MiseVersion} {
		if !strings.Contains(joined, want) {
			t.Errorf("the build does not contain %q", want)
		}
	}
	for _, c := range append(all, Checks()) {
		if strings.Contains(c, "\n") || !strings.HasPrefix(c, "RUN ") {
			t.Errorf("%q is not one RUN line", c)
		}
	}
}

// TestMiseHonoursTheRepositorysVersionFiles: the version files mise
// leaves off by default are switched on, and the workspace is trusted.
//
// @decision(DL-032)
func TestMiseHonoursTheRepositorysVersionFiles(t *testing.T) {
	settings := strings.Join(miseSettings(), "\n")
	for _, tool := range []string{"node", "python", "rust", "dotnet", "ruby", "java"} {
		if !strings.Contains(settings, `"`+tool+`"`) {
			t.Errorf("mise does not read %s's version files:\n%s", tool, settings)
		}
	}
	if !strings.Contains(settings, `trusted_config_paths = ["/workspace"]`) {
		t.Errorf("the workspace is not trusted:\n%s", settings)
	}
	for _, s := range miseSettings() {
		if strings.Contains(s, "'") {
			t.Errorf("setting %q carries a single quote, which the build's quoting cannot", s)
		}
	}
	if PathDirs()[0] != MiseShims {
		t.Errorf("PATH starts with %s, not mise's shims", PathDirs()[0])
	}
}

// TestTheGuideNamesExactlyWhatIsInstalled: every default and selected
// add-on is named, and nothing that is not installed.
//
// @decision(DL-033)
func TestTheGuideNamesExactlyWhatIsInstalled(t *testing.T) {
	plain := Guide(nil)
	for _, tool := range Defaults {
		if !strings.Contains(plain, tool.Version) {
			t.Errorf("the guide does not name %s %s", tool.Name, tool.Version)
		}
	}
	if strings.Contains(plain, "Add-ons") {
		t.Error("a sandbox with no add-ons is told about add-ons")
	}
	all, err := Parse(Names())
	if err != nil {
		t.Fatal(err)
	}
	full := Guide(all)
	for _, s := range all {
		if !strings.Contains(full, s.Note()) {
			t.Errorf("the guide does not describe %s", s)
		}
	}
	rustOnly, _ := Parse([]string{"rust"})
	if g := Guide(rustOnly); strings.Contains(g, "PostgreSQL") || !strings.Contains(g, "Rust") {
		t.Errorf("a rust-only guide names the wrong add-ons:\n%s", g)
	}
	if !strings.Contains(full, "/tmp/spov-postgres.log") {
		t.Error("the guide does not say where a service's start-up log is")
	}
}

// TestServicesAndEnvironmentFollowTheSelection.
//
// @decision(DL-033)
func TestServicesAndEnvironmentFollowTheSelection(t *testing.T) {
	sel, _ := Parse([]string{"redis", "playwright", "postgres", "rust"})
	var names []string
	for _, s := range Services(sel) {
		names = append(names, s.Start)
	}
	if len(names) != 2 || !strings.HasPrefix(names[0], "pg_ctlcluster") || !strings.HasPrefix(names[1], "redis-server") {
		t.Errorf("services = %v", names)
	}
	if Env(sel)["PLAYWRIGHT_BROWSERS_PATH"] != PlaywrightBrowsers {
		t.Errorf("env = %v", Env(sel))
	}
	if len(Services(nil)) != 0 || len(Env(nil)) != 0 {
		t.Error("no add-ons still produced services or environment")
	}
}

// TestAnyVersionOfTheServiceAddons: postgres, php and redis take any
// version of the right shape, install it from their upstream repository
// only once its key matches, and start and describe that version.
//
// @decision(DL-033)
func TestAnyVersionOfTheServiceAddons(t *testing.T) {
	sel, err := Parse([]string{"postgres@14", "php@8.1", "redis@7.2"})
	if err != nil {
		t.Fatal(err)
	}
	layers := map[string]string{}
	for _, s := range sel {
		layers[s.Name] = strings.Join(s.Layer(), "\n")
	}
	for name, want := range map[string][]string{
		"postgres": {"postgresql-14", "trixie-pgdg main", "--export " + pgdg.fingerprint, "/etc/postgresql/14/main/pg_hba.conf"},
		"php":      {"php8.1-cli", "php8.1-pgsql", "packages.sury.org/php/ trixie main", "--export " + suryPHP.fingerprint, "download/2.10.3/composer.phar", "'PHP 8.1.'"},
		"redis":    {"'redis-server=6:7.2.*'", "'redis-tools=6:7.2.*'", "packages.redis.io/deb trixie main", "--export " + redisIO.fingerprint},
	} {
		for _, w := range want {
			if !strings.Contains(layers[name], w) {
				t.Errorf("%s's layer lacks %q:\n%s", name, w, layers[name])
			}
		}
		// Only the pinned key is exported from a throwaway keyring into
		// the file apt trusts, never the download itself.
		if !strings.Contains(layers[name], "signed-by=/etc/apt/keyrings/") ||
			!strings.Contains(layers[name], `gpg --homedir "$g" --batch --export `) ||
			strings.Contains(layers[name], `install -D -m 644 "$g/key"`) || !strings.Contains(layers[name], "if [ ! -s ") {
			t.Errorf("%s's repository is trusted without checking its key:\n%s", name, layers[name])
		}
	}
	if svc := Services(sel); len(svc) != 2 || svc[0].Start != "pg_ctlcluster 14 main start" {
		t.Errorf("services = %+v", svc)
	}
	if g := Guide(sel); !strings.Contains(g, "PostgreSQL 14") || !strings.Contains(g, "PHP 8.1") || !strings.Contains(g, "Redis 7.2") {
		t.Errorf("the guide does not name the versions:\n%s", g)
	}
	if redisPin("7.2.16") != "6:7.2.16-*" || redisPin("8.0") != "6:8.0.*" {
		t.Errorf("redisPin = %q, %q", redisPin("7.2.16"), redisPin("8.0"))
	}
	if c, _ := composerFor("7.1"); c != "2.2.30" {
		t.Errorf("PHP 7.1 gets Composer %s, want the 2.2 LTS line", c)
	}
	if c, _ := composerFor("8.10"); c != "2.10.3" {
		t.Errorf("PHP 8.10 gets Composer %s", c)
	}
	if _, err := Parse([]string{"redis@7.2.16", "postgres@18", "php@5.6"}); err != nil {
		t.Errorf("an exact redis, a newer postgres or an old php was refused: %v", err)
	}
}

// TestADefaultLanguageCanBePinned: node, python, go, bun and uv are
// add-ons that replace the base's version; at the base's own version
// they are left out, so no variant is built.
//
// @decision(DL-033)
func TestADefaultLanguageCanBePinned(t *testing.T) {
	for _, d := range Defaults {
		if a := mustAddon(t, d.Name); !a.language || a.Default != d.Version {
			t.Errorf("%s is not a language add-on at the base's version", d.Name)
		}
	}
	sel, err := Parse([]string{"node@22", "python@3.12", "go@" + version(Defaults, "go")})
	if err != nil {
		t.Fatal(err)
	}
	if got := Strings(sel); !slices.Equal(got, []string{"node@22", "python@3.12"}) {
		t.Errorf("Parse = %v; the go entry at the base's version should be left out", got)
	}
	if l := sel[0].Layer(); len(l) != 1 || l[0] != "RUN mise use -g node@22" {
		t.Errorf("node@22's layer = %v", l)
	}
	g := Guide(sel)
	if !strings.Contains(g, "Node 22,") || strings.Contains(g, "Node "+version(Defaults, "node")) || !strings.Contains(g, "Python 3.12,") {
		t.Errorf("the guide does not list the pinned versions:\n%s", g)
	}
	if strings.Contains(g, "Add-ons in this sandbox") {
		t.Errorf("a pinned language is listed as an add-on:\n%s", g)
	}
	if len(Services(sel)) != 0 || len(Env(sel)) != 0 {
		t.Error("a pinned language brought a service or environment")
	}
	if _, err := Parse([]string{"node", "node@22"}); err == nil {
		t.Error("node named twice was accepted")
	}
}
