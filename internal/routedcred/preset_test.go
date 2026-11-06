package routedcred

import (
	"slices"
	"strings"
	"testing"
)

// Each preset takes its one host and path prefix from the URL and fixes
// its scheme and username.
//
// @decision(DL-076)
func TestEachPresetFixesItsSchemeAndRoute(t *testing.T) {
	for _, c := range []struct {
		preset string
		o      PresetOptions
		scheme string
		user   string
		paths  []string
	}{
		{PresetNPM, PresetOptions{URL: "https://npm.acme.com", Scope: "@acme"}, "bearer", "", nil},
		{PresetMaven, PresetOptions{URL: "https://repo.acme.com/artifactory/libs/", User: "ci"}, "basic", "ci", []string{"/artifactory/libs/"}},
		{PresetNuGet, PresetOptions{URL: "https://pkgs.acme.com/feed/v3/index.json"}, "basic", "spov", []string{"/feed/v3/index.json/"}},
		{PresetPyPI, PresetOptions{URL: "https://pypi.acme.com/simple/"}, "basic", "__token__", []string{"/simple/"}},
	} {
		r, err := FromPreset("acme", c.preset, c.o)
		if err != nil {
			t.Errorf("%s: %v", c.preset, err)
			continue
		}
		if r.Scheme != c.scheme || r.User != c.user || !slices.Equal(r.Paths, c.paths) || r.Hosts[0] != strings.Split(strings.TrimPrefix(c.o.URL, "https://"), "/")[0] {
			t.Errorf("%s: %+v", c.preset, r)
		}
		if err := r.Validate(); err != nil {
			t.Errorf("%s: a preset's own route does not validate: %v", c.preset, err)
		}
	}
	r, err := FromPreset("acme", PresetMaven, PresetOptions{URL: "https://repo.acme.com/artifactory/libs/", User: "ci",
		Paths: []string{"/artifactory/"}})
	if err != nil || !slices.Equal(r.Paths, []string{"/artifactory/"}) || r.RepoID != "acme" {
		t.Errorf("widened = %+v, %v", r, err)
	}
}

// A preset refuses a URL it cannot route, options of another preset,
// and a path that would narrow it away from its URL; a stored route
// whose hosts are not its preset's does not validate.
//
// @decision(DL-076)
func TestABadPresetIsRefused(t *testing.T) {
	for name, c := range map[string]struct {
		preset string
		o      PresetOptions
	}{
		"no preset":        {"docker", PresetOptions{URL: "https://r.acme.com/"}},
		"no url":           {PresetNPM, PresetOptions{}},
		"http":             {PresetNPM, PresetOptions{URL: "http://npm.acme.com/"}},
		"a port":           {PresetNPM, PresetOptions{URL: "https://npm.acme.com:8443/"}},
		"a user":           {PresetNPM, PresetOptions{URL: "https://u:p@npm.acme.com/"}},
		"a query":          {PresetNPM, PresetOptions{URL: "https://npm.acme.com/?x=1"}},
		"a bad scope":      {PresetNPM, PresetOptions{URL: "https://npm.acme.com/", Scope: "acme"}},
		"maven, no user":   {PresetMaven, PresetOptions{URL: "https://repo.acme.com/"}},
		"a bad id":         {PresetMaven, PresetOptions{URL: "https://repo.acme.com/", User: "ci", RepoID: "a b"}},
		"scope on maven":   {PresetMaven, PresetOptions{URL: "https://repo.acme.com/", User: "ci", Scope: "@a"}},
		"id on pypi":       {PresetPyPI, PresetOptions{URL: "https://pypi.acme.com/", RepoID: "x"}},
		"a path elsewhere": {PresetMaven, PresetOptions{URL: "https://repo.acme.com/libs/", User: "ci", Paths: []string{"/other/"}}},
	} {
		if _, err := FromPreset("acme", c.preset, c.o); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	r, _ := FromPreset("acme", PresetNPM, PresetOptions{URL: "https://npm.acme.com/"})
	r.Hosts = []string{"evil.example.com"}
	if r.Validate() == nil {
		t.Error("a preset route moved off its URL's host validated")
	}
	if (Route{Name: "a", Hosts: []string{"a.acme.com"}, Scheme: "bearer", URL: "https://a.acme.com/", Attach: AttachAlways}).Validate() == nil {
		t.Error("a url without a preset validated")
	}
}

// The files hold the placeholder, never anything else secret, and each
// client's config in its own shape; XML is escaped.
//
// @decision(DL-076)
func TestEachPresetWritesItsClientsConfig(t *testing.T) {
	npm, _ := FromPreset("acme-npm", PresetNPM, PresetOptions{URL: "https://npm.acme.com/api/npm/", Scope: "@acme"})
	mvn, _ := FromPreset("acme-mvn", PresetMaven, PresetOptions{URL: "https://repo.acme.com/libs/", User: "c<i"})
	nuget, _ := FromPreset("acme-nuget", PresetNuGet, PresetOptions{URL: "https://pkgs.acme.com/v3/index.json"})
	pypi, _ := FromPreset("acme-pypi", PresetPyPI, PresetOptions{URL: "https://pypi.acme.com/simple/"})
	plain := Route{Name: "plain", Hosts: []string{"api.acme.com"}, Scheme: "bearer"}
	files := PresetFiles([]Route{npm, mvn, nuget, pypi, plain}, func(name string) string { return "PH-" + name })
	got := map[string]string{}
	for _, f := range files {
		got[f.Path] = f.Content
	}
	want := map[string][]string{
		NPMRCFile: {"//npm.acme.com/api/npm/:_authToken=PH-acme-npm\n", "@acme:registry=https://npm.acme.com/api/npm/\n"},
		MavenFile: {"<id>acme-mvn</id>", "<username>c&lt;i</username>", "<password>PH-acme-mvn</password>",
			"<host>proxy</host>", "<port>3128</port>", "<nonProxyHosts>localhost|127.0.0.1|main</nonProxyHosts>"},
		NuGetFile: {`<add key="acme-nuget" value="https://pkgs.acme.com/v3/index.json" />`, `<add key="Username" value="spov" />`,
			`<add key="ClearTextPassword" value="PH-acme-nuget" />`},
		PipFile: {"extra-index-url = https://__token__:PH-acme-pypi@pypi.acme.com/simple/\n"},
		UVFile:  {`name = "acme-pypi"`, `url = "https://__token__:PH-acme-pypi@pypi.acme.com/simple/"`},
	}
	if len(got) != len(want) {
		t.Errorf("files %v", files)
	}
	for path, parts := range want {
		for _, p := range parts {
			if !strings.Contains(got[path], p) {
				t.Errorf("%s lacks %q:\n%s", path, p, got[path])
			}
		}
	}
	if files := PresetFiles([]Route{plain}, func(string) string { return "x" }); len(files) != 0 {
		t.Errorf("a route without a preset wrote %v", files)
	}
}
