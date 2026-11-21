package opencode

import (
	"fmt"
	"regexp"
	"strings"
)

// MaxPlugins bounds the plugins a launch adds to its sandbox's config.
//
// @decision(DL-171)
const MaxPlugins = 16

// pluginName is an npm package name, scoped or not, as npm accepts a new
// one today: lowercase, at most 214 characters, starting with a letter or
// a digit, so none reads as a flag.
var pluginName = regexp.MustCompile(`^(@[a-z0-9][a-z0-9-._~]*/)?[a-z0-9][a-z0-9-._~]*$`)

// pluginVersion is what may follow the name's `@`: a version, a tag or
// a semver range, without spaces, so nothing names a URL, a path or a
// git repository.
var pluginVersion = regexp.MustCompile(`^[A-Za-z0-9.^~*+=<>|_-]{1,64}$`)

// CheckPluginSpec checks one entry of sandbox.plugins: an npm package
// spec, `name` or `name@version`, and nothing that would make OpenCode
// fetch from anywhere but the registry.
//
// @decision(DL-171)
func CheckPluginSpec(spec string) error {
	if spec != strings.TrimSpace(spec) || spec == "" {
		return fmt.Errorf("plugin %q is not an npm package name", spec)
	}
	name, version := spec, ""
	// A scoped name starts with its own `@`; the version's is the next.
	if i := strings.LastIndex(spec, "@"); i > 0 {
		name, version = spec[:i], spec[i+1:]
		if version == "" {
			return fmt.Errorf("plugin %q has an empty version after @", spec)
		}
	}
	if len(name) > 214 || !pluginName.MatchString(name) {
		return fmt.Errorf("plugin %q is not an npm package name: name it as on npm, like envsitter-guard or @scope/name, with an optional @version", spec)
	}
	// npm reads a version that starts with a dot as a directory and one
	// named like an archive as a local tarball.
	if version != "" && (!pluginVersion.MatchString(version) || strings.HasPrefix(version, ".") || tarball(version)) {
		return fmt.Errorf("plugin %q has an unusable version: give a version, a tag or a semver range, never a URL or a path", spec)
	}
	return nil
}

// tarball reports whether npm would read a version as a local archive.
func tarball(version string) bool {
	v := strings.ToLower(version)
	return strings.HasSuffix(v, ".tgz") || strings.HasSuffix(v, ".tar") || strings.HasSuffix(v, ".tar.gz")
}

// CheckPlugins checks a launch's whole list.
//
// @decision(DL-171)
func CheckPlugins(specs []string) error {
	if len(specs) > MaxPlugins {
		return fmt.Errorf("%d plugins is more than the %d a sandbox takes", len(specs), MaxPlugins)
	}
	seen := make(map[string]bool, len(specs))
	for _, s := range specs {
		if err := CheckPluginSpec(s); err != nil {
			return err
		}
		if seen[s] {
			return fmt.Errorf("plugin %q is listed twice", s)
		}
		seen[s] = true
	}
	return nil
}
