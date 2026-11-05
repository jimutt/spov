package version

import (
	"runtime/debug"
	"strings"
	"testing"
)

func TestGetFallsBackToDev(t *testing.T) {
	i := Get()
	if i.Version == "" {
		t.Error("Version is empty; Get must always report something")
	}
}

func TestGetUsesStampedValues(t *testing.T) {
	version, commit, date = "1.2.3", "abcdef0123456789", "2026-08-28T00:00:00Z"
	t.Cleanup(func() { version, commit, date = "", "", "" })

	i := Get()
	if i.Version != "1.2.3" {
		t.Errorf("Version = %q", i.Version)
	}
	if i.Commit != "abcdef0123456789" {
		t.Errorf("Commit = %q", i.Commit)
	}
	s := i.String()
	if !strings.Contains(s, "spov 1.2.3") {
		t.Errorf("String() = %q", s)
	}
	if !strings.Contains(s, "abcdef012345") || strings.Contains(s, "abcdef0123456789") {
		t.Errorf("String() = %q, want a shortened commit", s)
	}
	if !strings.Contains(s, "built 2026-08-28T00:00:00Z") {
		t.Errorf("String() = %q", s)
	}
}

func TestStringOmitsMissingParts(t *testing.T) {
	i := Info{Version: "dev"}
	if got := i.String(); got != "spov dev" {
		t.Errorf("String() = %q, want %q", got, "spov dev")
	}
}

func TestShort(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"abc", "abc"},
		{"abcdefghijkl", "abcdefghijkl"},
		{"abcdefghijklm", "abcdefghijkl"},
	}
	for _, tt := range tests {
		if got := short(tt.in); got != tt.want {
			t.Errorf("short(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestResolve(t *testing.T) {
	settings := []debug.BuildSetting{
		{Key: "vcs.revision", Value: "deadbeef"},
		{Key: "vcs.time", Value: "2026-01-01T00:00:00Z"},
		{Key: "vcs.modified", Value: "true"},
	}
	tests := []struct {
		name string
		in   Info
		want Info
	}{
		{
			name: "unstamped build takes everything from vcs",
			in:   Info{},
			want: Info{Version: "dev", Commit: "deadbeef", Date: "2026-01-01T00:00:00Z"},
		},
		{
			name: "stamped values win over vcs",
			in:   Info{Version: "1.0.0", Commit: "cafe", Date: "yesterday"},
			want: Info{Version: "1.0.0", Commit: "cafe", Date: "yesterday"},
		},
		{
			name: "partial stamp fills only the gaps",
			in:   Info{Version: "1.0.0"},
			want: Info{Version: "1.0.0", Commit: "deadbeef", Date: "2026-01-01T00:00:00Z"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolve(tt.in, settings); got != tt.want {
				t.Errorf("resolve() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestResolveWithoutBuildInfo(t *testing.T) {
	got := resolve(Info{}, nil)
	if got.Version != "dev" || got.Commit != "" || got.Date != "" {
		t.Errorf("resolve() = %+v", got)
	}
}
