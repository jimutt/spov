package toolchain

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Addon is one curated, opt-in addition to the image. Its recipe decides
// how it is installed; a caller only names it and, optionally, a version.
//
// @decision(DL-033)
type Addon struct {
	// Name is how a configuration asks for it.
	Name string
	// Default is the version a bare name gets.
	Default string
	// versions matches the versions it accepts. Every accepted version
	// is safe to interpolate into a build command.
	versions *regexp.Regexp
	// exact matches a version that names one release, which is what a
	// partial version resolves to. Every match is safe to interpolate
	// into a build command.
	exact *regexp.Regexp
	// index is where a partial version is resolved, or nil when the
	// add-on takes exact versions only.
	index func(version string) Index
	// layer renders the build commands for one selection, at its exact
	// version when it has one.
	layer func(s Selection) []string
	// Env is set in the sandbox's environment, for OpenCode and every
	// command run in the sandbox.
	Env map[string]string
	// service, when set, renders the process the entrypoint starts for
	// one version.
	service func(version string) *Service
	// note renders what the workspace guide says about this selection.
	note func(s Selection) string
	// language marks one of the default languages: a selection replaces
	// its version in the image rather than adding to it.
	language bool
}

// Service is a process the entrypoint starts in the background.
//
// @decision(DL-033)
type Service struct {
	// Start is the shell command that starts it. It must return once the
	// service is launched.
	Start string
	// Log is where its start-up output goes.
	Log string
}

const (
	// PlaywrightBrowsers is where the playwright add-on keeps its
	// browsers.
	PlaywrightBrowsers = "/ms-playwright"
	// DebianSuite is the base image's Debian release, which the upstream
	// apt repositories are asked for.
	//
	// @decision(DL-033)
	DebianSuite = "trixie"
)

var (
	semverish   = regexp.MustCompile(`^[0-9]+(\.[0-9]+){0,2}$`)
	exactSemver = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	majorMinor  = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)
)

// Index is where a partial version resolves: a tool in mise's version
// index, or a package in an apt repository's Packages index.
//
// @decision(DL-033)
type Index struct {
	// Mise is the tool's name in mise's index, and Prefix what an entry
	// must start with, stripped before it is read as a version.
	Mise, Prefix string
	// Packages is the URL of an apt Packages index, and Package the
	// package whose version is the answer.
	Packages, Package string
}

// apt renders an apt install layer.
func apt(packages ...string) string {
	return "RUN apt-get update && apt-get install -y --no-install-recommends " +
		strings.Join(packages, " ") + " && rm -rf /var/lib/apt/lists/*"
}

// aptRepo is an upstream apt repository and the fingerprint its signing
// key must have.
//
// @decision(DL-033)
type aptRepo struct {
	name, keyURL, fingerprint string
	// source is the repository's URL, and suite its name for the base's
	// Debian release.
	source, suite string
}

// install renders one layer that trusts the repository's key for that
// repository only, then installs packages from it. The download is read
// into a throwaway keyring, and only the key with the pinned fingerprint
// is exported from it, so anything else in the file is never trusted.
//
// @decision(DL-033)
func (r aptRepo) install(packages ...string) string {
	key := "/etc/apt/keyrings/" + r.name + ".gpg"
	return "RUN set -eu; apt-get update; apt-get install -y --no-install-recommends gpg gpg-agent; " +
		"g=$(mktemp -d); curl -fsSL -o \"$g/key\" " + r.keyURL + "; " +
		"gpg --homedir \"$g\" --batch --quiet --import \"$g/key\"; " +
		"mkdir -p /etc/apt/keyrings; gpg --homedir \"$g\" --batch --export " + r.fingerprint + " > " + key + "; " +
		"gpgconf --homedir \"$g\" --kill all || true; rm -rf \"$g\"; " +
		`if [ ! -s ` + key + ` ]; then echo "spov: ` + r.keyURL + ` holds no key ` + r.fingerprint + `" >&2; exit 1; fi; ` +
		`echo "deb [signed-by=` + key + `] ` + r.source + " " + r.suite + ` main" > /etc/apt/sources.list.d/` + r.name + ".list; " +
		"apt-get update; apt-get install -y --no-install-recommends " + strings.Join(packages, " ") + "; " +
		"rm -rf /var/lib/apt/lists/*"
}

// The upstream repositories, each with its key's fingerprint.
//
// @decision(DL-033)
var (
	pgdg = aptRepo{
		name: "pgdg", keyURL: "https://www.postgresql.org/media/keys/ACCC4CF8.asc",
		fingerprint: "B97B0AFCAA1A47F044F244A07FCC7D46ACCC4CF8",
		source:      "https://apt.postgresql.org/pub/repos/apt", suite: DebianSuite + "-pgdg",
	}
	suryPHP = aptRepo{
		name: "sury-php", keyURL: "https://packages.sury.org/php/apt.gpg",
		fingerprint: "15058500A0235D97F5D10063B188E2B695BD4743",
		source:      "https://packages.sury.org/php/", suite: DebianSuite,
	}
	redisIO = aptRepo{
		name: "redis", keyURL: "https://packages.redis.io/gpg",
		fingerprint: "54318FA4052D1E61A6B6F7BB5F4349D6BF53AA0C",
		source:      "https://packages.redis.io/deb", suite: DebianSuite,
	}
)

// composerFor is the Composer release that runs on a PHP major.minor:
// the current line from PHP 7.2, the 2.2 LTS line before it.
//
// @decision(DL-033)
func composerFor(php string) (version, sha256 string) {
	major, minor, _ := strings.Cut(php, ".")
	ma, _ := strconv.Atoi(major)
	mi, _ := strconv.Atoi(minor)
	if ma < 7 || ma == 7 && mi < 2 {
		return "2.2.30", "8c2b4478b64f8f7cdf1574838fdb0033b29049ca821dad452db7a3dcfcdbffc2"
	}
	return "2.10.3", "7a2d379d5b8ffdaa028580ef26494c36d2feef4b178d3dd1473a4dbc5e17c8d6"
}

// redisPin is the apt version a redis selection asks for: an exact
// release, or the newest patch of a major.minor. 6 is the repository's
// version epoch.
//
// @decision(DL-033)
func redisPin(v string) string {
	if exactSemver.MatchString(v) {
		return "6:" + v + "-*"
	}
	return "6:" + v + ".*"
}

// packages is the URL of the repository's Packages index for amd64,
// which is what apt reads.
//
// @decision(DL-033)
func (r aptRepo) packages() string {
	return strings.TrimSuffix(r.source, "/") + "/dists/" + r.suite + "/main/binary-amd64/Packages.gz"
}

// aptPinned is pkg at the selection's exact version, quoted so the shell
// leaves its glob to apt, or unpinned when it has none.
//
// @decision(DL-033)
func aptPinned(s Selection, pkg string) string {
	if s.Resolved == "" {
		return pkg
	}
	return "'" + pkg + "=" + s.Resolved + "-*'"
}

// mise resolves an add-on's partial versions against mise's index of the
// tool.
func mise(tool string) func(string) Index {
	return func(string) Index { return Index{Mise: tool} }
}

// language is an add-on that pins one of the default languages through
// mise, in place of the base's version.
//
// @decision(DL-033)
func language(t Tool) Addon {
	return Addon{
		Name:     t.Name,
		Default:  t.Version,
		versions: semverish,
		exact:    exactSemver,
		index:    mise(t.Name),
		language: true,
		layer: func(s Selection) []string {
			return []string{"RUN mise use -g " + t.Name + "@" + s.Exact()}
		},
		note: func(s Selection) string {
			return displayName(t.Name) + " " + s.Exact() + " is pinned for this sandbox; the base's " + t.Version +
				" is still installed (`mise use " + t.Name + "@" + t.Version + "` selects it)."
		},
	}
}

// addons is the curated set, then the default languages. Adding one is a
// code change with a recipe, a test and a measured build.
//
// @decision(DL-033)
var addons = append([]Addon{
	{
		Name:     "dotnet",
		Default:  "10.0.401",
		versions: semverish,
		exact:    exactSemver,
		index:    mise("dotnet"),
		layer: func(s Selection) []string {
			return []string{apt("libicu76"), "RUN mise use -g dotnet@" + s.Exact()}
		},
		Env: map[string]string{"DOTNET_CLI_TELEMETRY_OPTOUT": "1", "DOTNET_NOLOGO": "1"},
		note: func(s Selection) string {
			return ".NET SDK " + s.Exact() + " (C#, F#). A repository's `global.json` selects another SDK, which is installed on first use; " +
				"`dotnet tool install -g` puts tools on PATH."
		},
	},
	{
		Name:    "java",
		Default: "21.0.12+101.0.LTS",
		// A Temurin version: a major, or a full release with its build.
		versions: regexp.MustCompile(`^[0-9]+(\.[0-9]+){0,2}(\+[0-9]+(\.[0-9]+)*(\.LTS)?)?$`),
		exact:    regexp.MustCompile(`^[0-9]+(\.[0-9]+){0,2}\+[0-9]+(\.[0-9]+)*(\.LTS)?$`),
		// Temurin's JDK builds; its JRE builds are `temurin-jre-…`.
		index: func(string) Index { return Index{Mise: "java", Prefix: "temurin-"} },
		layer: func(s Selection) []string {
			return []string{"RUN mise use -g java@temurin-" + s.Exact()}
		},
		note: func(s Selection) string {
			return "Java (Temurin " + s.Exact() + "). A repository's `.java-version` selects another, installed on first use."
		},
	},
	{
		Name:     "php",
		Default:  "8.4",
		versions: majorMinor,
		exact:    exactSemver,
		index:    func(v string) Index { return Index{Packages: suryPHP.packages(), Package: "php" + v + "-cli"} },
		layer: func(s Selection) []string {
			v := s.Version
			var pkgs []string
			for _, ext := range []string{"cli", "curl", "mbstring", "xml", "zip", "sqlite3", "pgsql"} {
				pkgs = append(pkgs, aptPinned(s, "php"+v+"-"+ext))
			}
			composer, sum := composerFor(v)
			check := "PHP " + v + "."
			if s.Resolved != "" {
				check = "PHP " + s.Resolved + " "
			}
			return []string{
				suryPHP.install(pkgs...),
				"RUN curl -fsSL -o /usr/local/bin/composer https://getcomposer.org/download/" + composer + "/composer.phar" +
					` && echo "` + sum + `  /usr/local/bin/composer" | sha256sum -c - && chmod 755 /usr/local/bin/composer` +
					" && php --version | grep -qF '" + check + "'",
			}
		},
		Env: map[string]string{"COMPOSER_ALLOW_SUPERUSER": "1"},
		note: func(s Selection) string {
			c, _ := composerFor(s.Version)
			return "PHP " + s.Exact() + " (from deb.sury.org) with Composer " + c + "."
		},
	},
	{
		Name:     "playwright",
		Default:  "1.63.0",
		versions: exactSemver,
		exact:    exactSemver,
		layer: func(s Selection) []string {
			return []string{"RUN " + withPath("PLAYWRIGHT_BROWSERS_PATH="+PlaywrightBrowsers+
				" npx -y playwright@"+s.Exact()+" install --with-deps chromium && rm -rf /var/lib/apt/lists/*")}
		},
		Env: map[string]string{"PLAYWRIGHT_BROWSERS_PATH": PlaywrightBrowsers},
		note: func(s Selection) string {
			return "Playwright " + s.Exact() + "'s Chromium, in `" + PlaywrightBrowsers + "`. A project on another Playwright version " +
				"downloads its own browser, which needs egress to Playwright's CDN."
		},
	},
	{
		Name:     "postgres",
		Default:  "17",
		versions: regexp.MustCompile(`^[1-9][0-9]$`),
		exact:    regexp.MustCompile(`^[1-9][0-9]\.[0-9]+$`),
		index:    func(v string) Index { return Index{Packages: pgdg.packages(), Package: "postgresql-" + v} },
		layer: func(s Selection) []string {
			v := s.Version
			conf := "/etc/postgresql/" + v + "/main/pg_hba.conf"
			return []string{
				pgdg.install(aptPinned(s, "postgresql-"+v)),
				"RUN sed -i -E 's/(peer|scram-sha-256|md5)$/trust/' " + conf +
					" && pg_ctlcluster " + v + " main start && su postgres -c 'createuser -s root && createdb -O root root'" +
					" && pg_ctlcluster " + v + " main stop",
			}
		},
		service: func(v string) *Service {
			return &Service{Start: "pg_ctlcluster " + v + " main start", Log: "/tmp/spov-postgres.log"}
		},
		note: func(s Selection) string {
			v := s.Version
			return "PostgreSQL " + s.Exact() + " (from apt.postgresql.org), started at boot on localhost:5432 with passwordless local access: `psql` connects as " +
				"`root` to database `root`, and the `postgres` superuser also exists. Restart: `pg_ctlcluster " + v + " main start`."
		},
	},
	{
		Name:     "redis",
		Default:  "8.0",
		versions: regexp.MustCompile(`^[0-9]+\.[0-9]+(\.[0-9]+)?$`),
		exact:    exactSemver,
		index:    func(string) Index { return Index{Packages: redisIO.packages(), Package: "redis-server"} },
		layer: func(s Selection) []string {
			v := s.Exact()
			return []string{
				redisIO.install("'redis-server="+redisPin(v)+"'", "'redis-tools="+redisPin(v)+"'"),
				"RUN redis-server --version | grep -qE 'v=" + regexp.QuoteMeta(v) + "[. ]'",
			}
		},
		service: func(string) *Service {
			return &Service{Start: "redis-server /etc/redis/redis.conf --daemonize yes", Log: "/tmp/spov-redis.log"}
		},
		note: func(s Selection) string {
			return "Redis " + s.Exact() + " (from packages.redis.io), started at boot on localhost:6379. Restart: `redis-server /etc/redis/redis.conf --daemonize yes`."
		},
	},
	{
		Name:     "ruby",
		Default:  "3.4.11",
		versions: semverish,
		exact:    exactSemver,
		index:    mise("ruby"),
		layer: func(s Selection) []string {
			return []string{"RUN mise use -g ruby@" + s.Exact()}
		},
		note: func(s Selection) string {
			return "Ruby " + s.Exact() + ". A repository's `.ruby-version` selects another, installed on first use."
		},
	},
	{
		Name:     "rust",
		Default:  "1.99.0",
		versions: regexp.MustCompile(`^(stable|[0-9]+\.[0-9]+(\.[0-9]+)?)$`),
		exact:    exactSemver,
		index:    mise("rust"),
		layer: func(s Selection) []string {
			v := s.Exact()
			return []string{"RUN mise use -g rust@" + v + " && mise exec rust@" + v + " -- rustup component remove rust-docs"}
		},
		note: func(s Selection) string {
			return "Rust " + s.Exact() + " (rustup, through mise). A repository's `rust-toolchain.toml` selects another toolchain, " +
				"installed on first use."
		},
	},
}, languages()...)

// languages are the default languages as add-ons.
func languages() []Addon {
	out := make([]Addon, len(Defaults))
	for i, t := range Defaults {
		out[i] = language(t)
	}
	return out
}

// Names lists every add-on, sorted.
func Names() []string {
	names := make([]string, len(addons))
	for i, a := range addons {
		names[i] = a.Name
	}
	slices.Sort(names)
	return names
}

func lookup(name string) (*Addon, bool) {
	for i := range addons {
		if addons[i].Name == name {
			return &addons[i], true
		}
	}
	return nil, false
}

// Selection is one add-on at the version a configuration asked for, and
// the exact release that version resolved to, when it was resolved.
//
// @decision(DL-033)
type Selection struct {
	Name    string
	Version string
	// Resolved is the exact release a partial Version names, or empty
	// when it was not resolved or is exact already. Only Resolve sets it.
	Resolved string
}

// String is the canonical `name@version` form, at the requested version.
func (s Selection) String() string { return s.Name + "@" + s.Version }

// Exact is the version the image installs: the resolved release, or the
// requested version when there is none.
func (s Selection) Exact() string {
	if s.Resolved != "" {
		return s.Resolved
	}
	return s.Version
}

// Installed is the `name@version` form at the version the image installs.
func (s Selection) Installed() string { return s.Name + "@" + s.Exact() }

// Addon is the selected add-on's recipe. A Selection only comes from
// Parse, so the name is always known.
func (s Selection) Addon() *Addon {
	a, _ := lookup(s.Name)
	return a
}

// Layer renders the selection's build commands.
func (s Selection) Layer() []string { return s.Addon().layer(s) }

// Note renders what the workspace guide says about the selection.
func (s Selection) Note() string { return s.Addon().note(s) }

// Service is the process the entrypoint starts for the selection, or
// nil.
//
// @decision(DL-033)
func (s Selection) Service() *Service {
	if svc := s.Addon().service; svc != nil {
		return svc(s.Version)
	}
	return nil
}

// Index is where the selection's version resolves. ok is false when it
// needs no resolving: the version is exact, or the add-on takes exact
// versions only.
//
// @decision(DL-033)
func (s Selection) Index() (Index, bool) {
	a := s.Addon()
	if a.index == nil || a.exact.MatchString(s.Version) {
		return Index{}, false
	}
	return a.index(s.Version), true
}

// Resolve returns the selection resolved to exact. An empty exact, or the
// requested version itself, leaves it unresolved. It refuses an answer
// that is not an exact version of this add-on or does not extend the
// requested one, since the answer reaches a build command.
//
// @decision(DL-033)
func (s Selection) Resolve(exact string) (Selection, error) {
	s.Resolved = ""
	if exact == "" || exact == s.Version {
		return s, nil
	}
	if !s.Addon().exact.MatchString(exact) {
		return s, fmt.Errorf("add-on %s: %q is not an exact version", s.Name, exact)
	}
	if !extends(exact, s.Version) {
		return s, fmt.Errorf("add-on %s: %q is not a release of %s", s.Name, exact, s.Version)
	}
	s.Resolved = exact
	return s, nil
}

// extends reports whether exact is a release of the partial version v:
// `22` covers `22.23.3`, `21` covers `21.0.12+101.0.LTS`, and `stable`
// covers any release.
func extends(exact, v string) bool {
	return v == "stable" || strings.HasPrefix(exact, v+".") || strings.HasPrefix(exact, v+"+")
}

// Language reports whether the selection pins a default language.
func (s Selection) Language() bool { return s.Addon().language }

// Parse resolves `name[@version]` entries into selections sorted by name,
// with each bare name given its add-on's default. It refuses an unknown
// name, a version the add-on does not accept, and a name given twice. A
// default language at the base's version is left out.
//
// @decision(DL-033)
func Parse(entries []string) ([]Selection, error) {
	var out []Selection
	var seen []string
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		name, v, _ := strings.Cut(e, "@")
		a, ok := lookup(name)
		if !ok {
			return nil, fmt.Errorf("unknown add-on %q; the add-ons are %s", name, strings.Join(Names(), ", "))
		}
		if v == "" {
			v = a.Default
		}
		if !a.versions.MatchString(v) {
			return nil, fmt.Errorf("add-on %s does not offer version %q (its default is %s)", name, v, a.Default)
		}
		if slices.Contains(seen, name) {
			return nil, fmt.Errorf("add-on %s is named more than once", name)
		}
		seen = append(seen, name)
		// A default language at the base's own version changes nothing,
		// so it builds no variant.
		//
		// @decision(DL-033)
		if a.language && v == a.Default {
			continue
		}
		out = append(out, Selection{Name: name, Version: v})
	}
	slices.SortFunc(out, func(a, b Selection) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// Strings renders selections in their canonical form.
func Strings(sel []Selection) []string {
	out := make([]string, len(sel))
	for i, s := range sel {
		out[i] = s.String()
	}
	return out
}

// Installed renders selections at the versions the image installs.
//
// @decision(DL-033)
func Installed(sel []Selection) []string {
	out := make([]string, len(sel))
	for i, s := range sel {
		out[i] = s.Installed()
	}
	return out
}

// Unresolved is sel without its resolved versions: the selections as
// they were asked for.
//
// @decision(DL-016)
func Unresolved(sel []Selection) []Selection {
	if sel == nil {
		return nil
	}
	out := make([]Selection, len(sel))
	for i, s := range sel {
		out[i] = Selection{Name: s.Name, Version: s.Version}
	}
	return out
}

// ResolveTo resolves each selection to the version installed names for
// it, as Installed renders it. A selection installed does not name is
// left unresolved.
//
// @decision(DL-016)
func ResolveTo(sel []Selection, installed []string) ([]Selection, error) {
	got := map[string]string{}
	for _, e := range installed {
		name, v, _ := strings.Cut(e, "@")
		got[name] = v
	}
	out := Unresolved(sel)
	for i := range out {
		r, err := out[i].Resolve(got[out[i].Name])
		if err != nil {
			return nil, err
		}
		out[i] = r
	}
	return out, nil
}
