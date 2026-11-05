package opencode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// BinaryName is the OpenCode 2 client binary.
//
// @decision(DL-035)
const BinaryName = "opencode"

// ErrBinaryNotFound means no local opencode could be located.
var ErrBinaryNotFound = errors.New("opencode: no local opencode binary found")

// Locator finds the local opencode client.
//
// @decision(DL-035)
type Locator struct {
	// DataDir is where spov keeps its own pinned copies, normally
	// ~/.local/share/spov.
	DataDir string
	// HomeDir is the user's home, used to find the official install
	// directory. Empty skips that check.
	HomeDir string
	// LookPath finds a binary on PATH; defaults to exec.LookPath.
	LookPath func(string) (string, error)
	// Version reports a binary's version; defaults to running it.
	Version func(ctx context.Context, path string) (string, error)
}

// ManagedPath is where spov keeps its own copy of a given version.
//
// @decision(DL-035)
func (l Locator) ManagedPath(version string) string {
	return filepath.Join(l.DataDir, "bin", version, BinaryName)
}

// InstallDir is where the official v2 install script puts the binary. It
// is checked explicitly because the script defaults to --no-modify-path,
// so a correctly installed opencode is routinely absent from PATH.
//
// @decision(DL-035)
const InstallDir = ".opencode/bin"

// Drift is how far a local client's version is from the pin. The order
// matters: Usable and the locator's preference compare drifts.
//
// @decision(DL-035)
type Drift int

const (
	// Exact is the pinned build.
	Exact Drift = iota
	// PatchDrift is another patch of the pin's major and minor release.
	PatchDrift
	// MinorDrift is another minor release of the pin's major release.
	MinorDrift
	// Incompatible is another major release, or a build that is not the
	// pin where either side is a prerelease or unreadable.
	Incompatible
	// Unbridgeable pairs a 0.0.0 prerelease with a release. The two
	// cannot attach to each other (DL-035).
	Unbridgeable
)

// Usable reports whether a client at this drift is used without
// --allow-version-mismatch.
func (d Drift) Usable() bool { return d <= MinorDrift }

// CompareVersions grades a local client's version against the pin.
// Versions are compared as `major.minor.patch` releases; a prerelease or
// anything else needs an exact match.
//
// @decision(DL-035)
func CompareVersions(found, want string) Drift {
	if found == want {
		return Exact
	}
	f, fok := parseRelease(found)
	w, wok := parseRelease(want)
	switch {
	case fok && wok && f[0] != w[0]:
		return Incompatible
	case fok && wok && f[1] != w[1]:
		return MinorDrift
	case fok && wok:
		return PatchDrift
	case (fok && isPrerelease(want)) || (wok && isPrerelease(found)):
		return Unbridgeable
	}
	return Incompatible
}

// parseRelease reads `major.minor.patch`, each a decimal number.
func parseRelease(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p != strconv.Itoa(n) {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// isPrerelease reports a build of the pre-2.0 line, `0.0.0-beta-*` or
// `0.0.0-dev-*`.
func isPrerelease(v string) bool { return strings.HasPrefix(v, "0.0.0-") }

// Located is the local client Locate chose.
//
// @decision(DL-035)
type Located struct {
	Path string
	// Version is what the client reports.
	Version string
	Drift   Drift
}

// Locate finds the opencode closest to want. It looks in spov's own
// pinned copy, then the official install directory, then PATH, so a
// user's existing install is used when it agrees and never fought with
// when it does not. An exact match wins, then another patch, then another
// minor release. A client further off is returned beside a
// ClientVersionMismatchError, so --allow-version-mismatch can use it.
//
// @decision(DL-035) @decision(DL-025)
func (l Locator) Locate(ctx context.Context, want string) (Located, error) {
	if want == "" {
		return Located{}, fmt.Errorf("opencode: no pinned version to look for")
	}

	// spov's own copy is pinned by construction, so its version needs no
	// checking.
	if l.DataDir != "" {
		if managed := l.ManagedPath(want); isExecutable(managed) {
			return Located{Path: managed, Version: want, Drift: Exact}, nil
		}
	}

	candidates, lookErr := l.candidates()
	if len(candidates) == 0 {
		return Located{}, fmt.Errorf("%w: install it with `curl -fsSL https://opencode.ai/v2/install | bash -s -- --version %s`, "+
			"or let spov manage a copy at %s: %w",
			ErrBinaryNotFound, want, l.ManagedPath(want), lookErr)
	}

	// A refusal is reported only if nothing usable is found, so a stale
	// binary earlier in the search does not mask a good one later. Among
	// refusals, one the flag can override beats one it cannot, which
	// beats a binary whose version could not be read.
	var best, refused Located
	var mismatch error
	rank := -1
	for _, candidate := range candidates {
		got, err := l.version(ctx, candidate)
		if err != nil {
			if rank < 0 {
				rank, mismatch = 0, fmt.Errorf("opencode: %s exists but its version could not be read: %w", candidate, err)
			}
			continue
		}
		found := Located{Path: candidate, Version: got, Drift: CompareVersions(got, want)}
		switch {
		case found.Drift == Exact:
			return found, nil
		case found.Drift.Usable():
			if best.Path == "" || found.Drift < best.Drift {
				best = found
			}
		default:
			r := 2
			if found.Drift == Unbridgeable {
				r = 1
			}
			if r > rank {
				rank, refused = r, found
				mismatch = ClientVersionMismatchError{Found: got, Want: want, Unbridgeable: found.Drift == Unbridgeable}
			}
		}
	}
	if best.Path != "" {
		return best, nil
	}
	return refused, mismatch
}

// ClientVersionMismatchError reports that the opencode binary on THIS
// machine is not a build the session can use.
//
// @decision(DL-035)
type ClientVersionMismatchError struct {
	// Found is what the local binary reports, Want what the session
	// needs.
	Found string
	Want  string
	// Unbridgeable is a prerelease paired with a release, which
	// --allow-version-mismatch does not override.
	Unbridgeable bool
}

func (e ClientVersionMismatchError) Error() string {
	msg := fmt.Sprintf("opencode: the local client is %s, and this session needs %s", e.Found, e.Want)
	if e.Unbridgeable {
		msg += "; a 0.0.0 prerelease and a release cannot attach to each other, so install " + e.Want
	}
	return msg
}

// candidates lists the places an opencode might be, in preference order.
func (l Locator) candidates() ([]string, error) {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if p != "" && !seen[p] && isExecutable(p) {
			seen[p] = true
			out = append(out, p)
		}
	}

	if l.HomeDir != "" {
		add(filepath.Join(l.HomeDir, InstallDir, BinaryName))
	}

	lookPath := l.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	onPath, lookErr := lookPath(BinaryName)
	if lookErr == nil {
		add(onPath)
	}
	return out, lookErr
}

// version reports the version a binary announces.
func (l Locator) version(ctx context.Context, path string) (string, error) {
	if l.Version != nil {
		return l.Version(ctx, path)
	}
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("run %s --version: %w", path, err)
	}
	return ParseVersion(string(out))
}

// ParseVersion extracts the version from `opencode --version` output,
// which prints "opencode v2.0.22". The server reports the same build as
// a bare "2.0.22", so the binary name and the "v" are stripped to make
// the two comparable.
//
// @decision(DL-035)
func ParseVersion(out string) (string, error) {
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Take the last field, so both "opencode v1.2.3" and a bare
		// "1.2.3" work.
		fields := strings.Fields(line)
		version := fields[len(fields)-1]
		return strings.TrimPrefix(version, "v"), nil
	}
	return "", fmt.Errorf("opencode: --version printed nothing")
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode().Perm()&0o111 != 0
}
