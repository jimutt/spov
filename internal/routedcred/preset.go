package routedcred

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
)

// The presets: a package manager's registry, with its scheme and the
// client config the entrypoint writes for it.
//
// @decision(DL-076)
const (
	PresetNPM   = "npm"
	PresetMaven = "maven"
	PresetNuGet = "nuget"
	PresetPyPI  = "pypi"
)

// Presets are every preset, in the order help names them.
var Presets = []string{PresetNPM, PresetMaven, PresetNuGet, PresetPyPI}

// PresetOptions are what a preset is made from: the registry's URL and
// the options each preset takes.
//
// @decision(DL-076)
type PresetOptions struct {
	URL string
	// Scope is npm's, as @acme.
	Scope string
	// RepoID is the repository's id in the project's pom.xml; the
	// credential's name when empty.
	RepoID string
	// User is the Basic username: Maven's is required, NuGet's defaults
	// to spov and PyPI's to __token__.
	User string
	// Paths widen the route to prefixes that hold the URL's path.
	Paths  []string
	Attach string
}

var (
	npmScope = regexp.MustCompile(`^@[a-z0-9][a-z0-9._-]*$`)
	mavenID  = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// FromPreset makes a routed credential from a preset: the URL's host is
// its one host, the URL's path its path prefix, and the preset fixes the
// scheme.
//
// @decision(DL-076)
func FromPreset(name, preset string, o PresetOptions) (Route, error) {
	u, err := url.Parse(strings.TrimSpace(o.URL))
	switch {
	case !slices.Contains(Presets, preset):
		return Route{}, fmt.Errorf("%q is not a preset; the presets are %s", preset, strings.Join(Presets, ", "))
	case o.URL == "":
		return Route{}, fmt.Errorf("the %s preset needs --url, the registry's address", preset)
	case err != nil:
		return Route{}, fmt.Errorf("--url: %w", err)
	case u.Scheme != "https" || u.Host == "" || u.Port() != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "":
		return Route{}, errors.New("--url must be https://host/path, with no port, user, query or fragment")
	}
	r := Route{Name: name, Hosts: []string{u.Hostname()}, Preset: preset, URL: strings.TrimSuffix(u.String(), "/") + "/", Attach: o.Attach}
	if p := strings.TrimSuffix(u.EscapedPath(), "/"); p != "" {
		r.Paths = []string{p + "/"}
	}
	if len(o.Paths) > 0 {
		for _, widened := range o.Paths {
			if !strings.HasSuffix(widened, "/") || !strings.HasPrefix(strings.TrimSuffix(u.EscapedPath(), "/")+"/", widened) {
				return Route{}, fmt.Errorf("--path %s does not hold the URL's path; a preset's path only widens to a prefix of it", widened)
			}
		}
		r.Paths = o.Paths
	}
	switch preset {
	case PresetNPM:
		r.Scheme = "bearer"
		if o.Scope != "" && !npmScope.MatchString(o.Scope) {
			return Route{}, fmt.Errorf("--scope %q is not an npm scope, as @acme", o.Scope)
		}
		r.Scope = o.Scope
	case PresetMaven:
		r.Scheme, r.User, r.RepoID = "basic", o.User, o.RepoID
		if r.User == "" {
			return Route{}, errors.New("the maven preset needs --user, the repository's username")
		}
		if r.RepoID == "" {
			r.RepoID = name
		}
		if !mavenID.MatchString(r.RepoID) {
			return Route{}, fmt.Errorf("--id %q is not a Maven repository id", r.RepoID)
		}
	case PresetNuGet:
		r.Scheme, r.User = "basic", firstOf(o.User, "spov")
	case PresetPyPI:
		r.Scheme, r.User = "basic", firstOf(o.User, "__token__")
	}
	if o.Scope != "" && preset != PresetNPM {
		return Route{}, errors.New("--scope is npm's")
	}
	if o.RepoID != "" && preset != PresetMaven {
		return Route{}, errors.New("--id is Maven's")
	}
	r = r.Normal()
	if r.Attach != AttachAlways && r.Attach != AttachAsked {
		return Route{}, fmt.Errorf("--attach is %q, not always or asked", r.Attach)
	}
	return r, r.validateBase()
}

func firstOf(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// File is a client config file the entrypoint writes, holding only
// placeholders.
//
// @decision(DL-076)
type File struct {
	Path    string
	Content string
}

// Where each preset's file goes in the sandbox, which runs as root.
const (
	NPMRCFile    = "/root/.npmrc"
	MavenFile    = "/root/.m2/settings.xml"
	NuGetFile    = "/root/.nuget/NuGet/NuGet.Config"
	PipFile      = "/root/.config/pip/pip.conf"
	UVFile       = "/root/.config/uv/uv.toml"
	proxyHost    = "proxy"
	proxyPort    = "3128"
	nonProxyHost = "localhost|127.0.0.1|main"
)

// PresetFiles renders the config files for the routes made from presets;
// placeholder gives a route's placeholder by name. Each file is written
// whole, and only when a route of its preset is held.
//
// @decision(DL-076)
func PresetFiles(routes []Route, placeholder func(name string) string) []File {
	by := map[string][]Route{}
	for _, r := range routes {
		if r.Preset != "" {
			by[r.Preset] = append(by[r.Preset], r)
		}
	}
	var out []File
	if rs := by[PresetNPM]; len(rs) > 0 {
		var b strings.Builder
		for _, r := range rs {
			key := "//" + strings.TrimPrefix(r.URL, "https://")
			fmt.Fprintf(&b, "%s:_authToken=%s\n", key, placeholder(r.Name))
			if r.Scope != "" {
				fmt.Fprintf(&b, "%s:registry=%s\n", r.Scope, r.URL)
			}
		}
		out = append(out, File{NPMRCFile, b.String()})
	}
	if rs := by[PresetMaven]; len(rs) > 0 {
		var b strings.Builder
		b.WriteString("<settings>\n  <servers>\n")
		for _, r := range rs {
			fmt.Fprintf(&b, "    <server>\n      <id>%s</id>\n      <username>%s</username>\n      <password>%s</password>\n    </server>\n",
				xmlText(r.RepoID), xmlText(r.User), xmlText(placeholder(r.Name)))
		}
		b.WriteString("  </servers>\n  <proxies>\n")
		for _, proto := range []string{"https", "http"} {
			fmt.Fprintf(&b, "    <proxy>\n      <id>spov-%s</id>\n      <active>true</active>\n      <protocol>%s</protocol>\n"+
				"      <host>%s</host>\n      <port>%s</port>\n      <nonProxyHosts>%s</nonProxyHosts>\n    </proxy>\n",
				proto, proto, proxyHost, proxyPort, nonProxyHost)
		}
		b.WriteString("  </proxies>\n</settings>\n")
		out = append(out, File{MavenFile, b.String()})
	}
	if rs := by[PresetNuGet]; len(rs) > 0 {
		var b strings.Builder
		b.WriteString("<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<configuration>\n  <packageSources>\n")
		for _, r := range rs {
			fmt.Fprintf(&b, "    <add key=\"%s\" value=\"%s\" />\n", xmlAttr(r.Name), xmlAttr(strings.TrimSuffix(r.URL, "/")))
		}
		b.WriteString("  </packageSources>\n  <packageSourceCredentials>\n")
		for _, r := range rs {
			fmt.Fprintf(&b, "    <%s>\n      <add key=\"Username\" value=\"%s\" />\n      <add key=\"ClearTextPassword\" value=\"%s\" />\n    </%s>\n",
				r.Name, xmlAttr(r.User), xmlAttr(placeholder(r.Name)), r.Name)
		}
		b.WriteString("  </packageSourceCredentials>\n</configuration>\n")
		out = append(out, File{NuGetFile, b.String()})
	}
	if rs := by[PresetPyPI]; len(rs) > 0 {
		var urls []string
		var uv strings.Builder
		for _, r := range rs {
			u, _ := url.Parse(r.URL)
			u.User = url.UserPassword(r.User, placeholder(r.Name))
			urls = append(urls, u.String())
			fmt.Fprintf(&uv, "[[index]]\nname = %s\nurl = %s\n\n", tomlString(r.Name), tomlString(u.String()))
		}
		out = append(out, File{PipFile, "[global]\nextra-index-url = " + strings.Join(urls, " ") + "\n"},
			File{UVFile, uv.String()})
	}
	slices.SortFunc(out, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
	return out
}

func xmlText(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func xmlAttr(s string) string { return xmlText(s) }

// tomlString quotes s as a TOML basic string, which JSON's escaping is.
func tomlString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// PresetDirs are the directories the preset files need.
func PresetDirs(files []File) []string {
	var out []string
	for _, f := range files {
		if d := path.Dir(f.Path); !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}
