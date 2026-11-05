// Package version exposes the spov build identity.
package version

import "runtime/debug"

// These are set by the linker at release time (goreleaser).
var (
	version = ""
	commit  = ""
	date    = ""
)

// Info describes the running spov build.
type Info struct {
	Version string
	Commit  string
	Date    string
}

// Get returns the build identity, falling back to Go module build info
// when the binary was not stamped by a release build.
func Get() Info {
	var settings []debug.BuildSetting
	if bi, ok := debug.ReadBuildInfo(); ok {
		settings = bi.Settings
	}
	return resolve(Info{Version: version, Commit: commit, Date: date}, settings)
}

// resolve fills the gaps in a stamped Info from Go build settings.
// Linker-stamped values always win over VCS metadata.
func resolve(i Info, settings []debug.BuildSetting) Info {
	if i.Version == "" {
		i.Version = "dev"
	}
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			if i.Commit == "" {
				i.Commit = s.Value
			}
		case "vcs.time":
			if i.Date == "" {
				i.Date = s.Value
			}
		}
	}
	return i
}

// String renders the build identity on one line.
func (i Info) String() string {
	s := "spov " + i.Version
	if i.Commit != "" {
		s += " (" + short(i.Commit) + ")"
	}
	if i.Date != "" {
		s += " built " + i.Date
	}
	return s
}

func short(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
