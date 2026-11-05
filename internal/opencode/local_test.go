package opencode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestLocatePrefersTheManagedCopy(t *testing.T) {
	dir := t.TempDir()
	l := Locator{
		DataDir: dir,
		LookPath: func(string) (string, error) {
			t.Error("PATH was searched even though a managed copy exists")
			return "", errors.New("unused")
		},
	}
	managed := l.ManagedPath("v1")
	writeExecutable(t, managed)

	got, err := l.Locate(context.Background(), "v1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != managed {
		t.Errorf("path = %q, want %q", got.Path, managed)
	}
}

func TestLocateFallsBackToPathWhenVersionsMatch(t *testing.T) {
	onPath := filepath.Join(t.TempDir(), BinaryName)
	writeExecutable(t, onPath)

	l := Locator{
		DataDir:  t.TempDir(),
		LookPath: func(string) (string, error) { return onPath, nil },
		Version:  func(context.Context, string) (string, error) { return "v1", nil },
	}
	got, err := l.Locate(context.Background(), "v1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != onPath {
		t.Errorf("path = %q, want %q", got.Path, onPath)
	}
}

// TestLocateReportsAVersionMismatch: hourly beta churn makes a silent
// mismatch worse than a hard stop.
//
// @decision(DL-035)
func TestLocateReportsAVersionMismatch(t *testing.T) {
	onPath := filepath.Join(t.TempDir(), BinaryName)
	writeExecutable(t, onPath)

	l := Locator{
		DataDir:  t.TempDir(),
		LookPath: func(string) (string, error) { return onPath, nil },
		Version:  func(context.Context, string) (string, error) { return "v-old", nil },
	}
	got, err := l.Locate(context.Background(), "v-new")

	var mismatch ClientVersionMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("err = %v, want a ClientVersionMismatchError", err)
	}
	if mismatch.Found != "v-old" || mismatch.Want != "v-new" {
		t.Errorf("mismatch = %+v", mismatch)
	}
	// The path is still returned so --allow-version-mismatch can use it.
	if got.Path != onPath {
		t.Errorf("path = %q; the binary should still be reported", got.Path)
	}
}

func TestLocateWithoutAnyBinary(t *testing.T) {
	l := Locator{
		DataDir:  t.TempDir(),
		LookPath: func(string) (string, error) { return "", errors.New("not found in $PATH") },
	}
	_, err := l.Locate(context.Background(), "v1")
	if !errors.Is(err, ErrBinaryNotFound) {
		t.Fatalf("err = %v, want ErrBinaryNotFound", err)
	}
	// The message must say where spov would keep its own copy.
	if !strings.Contains(err.Error(), l.ManagedPath("v1")) {
		t.Errorf("error %q does not name the managed path", err)
	}
}

func TestLocateWithoutAVersion(t *testing.T) {
	if _, err := (Locator{}).Locate(context.Background(), ""); err == nil {
		t.Fatal("expected an error")
	}
}

func TestLocateWhenTheVersionCannotBeRead(t *testing.T) {
	onPath := filepath.Join(t.TempDir(), BinaryName)
	writeExecutable(t, onPath)

	l := Locator{
		LookPath: func(string) (string, error) { return onPath, nil },
		Version:  func(context.Context, string) (string, error) { return "", errors.New("exec format error") },
	}
	_, err := l.Locate(context.Background(), "v1")
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, ErrBinaryNotFound) {
		t.Error("a binary that exists but cannot report its version is not a missing binary")
	}
}

func TestLocateIgnoresANonExecutableManagedCopy(t *testing.T) {
	dir := t.TempDir()
	l := Locator{
		DataDir:  dir,
		LookPath: func(string) (string, error) { return "", errors.New("not found") },
	}
	managed := l.ManagedPath("v1")
	if err := os.MkdirAll(filepath.Dir(managed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(managed, []byte("not executable"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := l.Locate(context.Background(), "v1"); !errors.Is(err, ErrBinaryNotFound) {
		t.Errorf("err = %v; a non-executable file is not a usable binary", err)
	}
}

func TestManagedPathIsVersioned(t *testing.T) {
	l := Locator{DataDir: "/home/u/.local/share/spov"}
	a, b := l.ManagedPath("v1"), l.ManagedPath("v2")
	if a == b {
		t.Error("two versions share a path")
	}
	if !strings.Contains(a, "v1") || !strings.HasSuffix(a, BinaryName) {
		t.Errorf("path = %q", a)
	}
}

func TestParseVersion(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		// The real binary prints "opencode v2.0.22" while the
		// server reports a bare "2.0.22"; both must compare
		// equal or every launch fails the version gate.
		{"real binary output", "opencode v2.0.22\n", "2.0.22", false},
		{"a beta binary's output", "opencode2 v0.0.0-beta-18684\n", "0.0.0-beta-18684", false},
		{"bare version", "0.0.0-beta-18684\n", "0.0.0-beta-18684", false},
		{"v prefix only", "v1.2.3\n", "1.2.3", false},
		{"leading blank lines", "\n\n0.0.0-beta-18684\n", "0.0.0-beta-18684", false},
		{"surrounding whitespace", "  0.0.0-beta-1  ", "0.0.0-beta-1", false},
		{"extra output", "opencode2 v0.0.0-beta-1\nsome notice\n", "0.0.0-beta-1", false},
		{"nothing", "", "", true},
		{"only whitespace", "  \n\n ", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseVersion(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("ParseVersion(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestLocateRunsARealBinary covers the default Version hook, which shells
// out to the binary.
func TestLocateRunsARealBinary(t *testing.T) {
	onPath := filepath.Join(t.TempDir(), BinaryName)
	// Mimic the real binary's output format.
	if err := os.WriteFile(onPath, []byte("#!/bin/sh\necho opencode v9.9.9\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	l := Locator{LookPath: func(string) (string, error) { return onPath, nil }}

	got, err := l.Locate(context.Background(), "9.9.9")
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != onPath {
		t.Errorf("path = %q", got.Path)
	}
}

// TestLocateFindsTheOfficialInstall covers the common case on a fresh
// machine: the v2 install script defaults to --no-modify-path, so a
// correctly installed opencode is routinely absent from PATH.
//
// @decision(DL-035)
func TestLocateFindsTheOfficialInstall(t *testing.T) {
	home := t.TempDir()
	installed := filepath.Join(home, InstallDir, BinaryName)
	if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed, []byte("#!/bin/sh\necho opencode v1.2.3\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	l := Locator{
		HomeDir:  home,
		DataDir:  t.TempDir(),
		LookPath: func(string) (string, error) { return "", errors.New("not in $PATH") },
	}
	got, err := l.Locate(context.Background(), "1.2.3")
	if err != nil {
		t.Fatalf("an installed binary that is not on PATH was not found: %v", err)
	}
	if got.Path != installed {
		t.Errorf("path = %q, want %q", got.Path, installed)
	}
}

// TestLocatePrefersAMatchingBinaryOverAnEarlierMismatch: a stale binary
// in one location must not mask a correct one in another.
func TestLocatePrefersAMatchingBinaryOverAnEarlierMismatch(t *testing.T) {
	home := t.TempDir()
	stale := filepath.Join(home, InstallDir, BinaryName)
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("#!/bin/sh\necho opencode v-old\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	good := filepath.Join(t.TempDir(), BinaryName)
	if err := os.WriteFile(good, []byte("#!/bin/sh\necho opencode v1.2.3\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	l := Locator{HomeDir: home, LookPath: func(string) (string, error) { return good, nil }}
	got, err := l.Locate(context.Background(), "1.2.3")
	if err != nil {
		t.Fatalf("the matching binary on PATH was not used: %v", err)
	}
	if got.Path != good {
		t.Errorf("path = %q, want the matching binary %q", got.Path, good)
	}
}

// TestLocateSuggestsTheInstallCommand: a missing binary is the first
// thing a new user hits.
func TestLocateSuggestsTheInstallCommand(t *testing.T) {
	l := Locator{
		HomeDir:  t.TempDir(),
		DataDir:  t.TempDir(),
		LookPath: func(string) (string, error) { return "", errors.New("not found") },
	}
	_, err := l.Locate(context.Background(), "1.2.3")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "opencode.ai/v2/install") {
		t.Errorf("error %q does not say how to install it", err)
	}
	if !strings.Contains(err.Error(), "1.2.3") {
		t.Errorf("error %q does not name the version needed", err)
	}
}

// Each row of the local client's version rule.
//
// @decision(DL-035)
func TestCompareVersions(t *testing.T) {
	for _, tt := range []struct {
		found, want string
		drift       Drift
	}{
		{"2.0.21", "2.0.21", Exact},
		{"2.0.23", "2.0.21", PatchDrift},
		{"2.0.1", "2.0.21", PatchDrift},
		{"2.1.0", "2.0.21", MinorDrift},
		{"2.0.21", "2.3.0", MinorDrift},
		{"3.0.0", "2.0.21", Incompatible},
		{"1.18.34", "2.0.21", Incompatible},
		{"0.0.0-beta-19151", "0.0.0-beta-19151", Exact},
		{"0.0.0-beta-19151", "0.0.0-beta-19200", Incompatible},
		{"0.0.0-dev-1", "0.0.0-beta-19151", Incompatible},
		{"0.0.0-beta-19151", "2.0.21", Unbridgeable},
		{"2.0.21", "0.0.0-beta-19151", Unbridgeable},
		{"2.0.21-rc.1", "2.0.21", Incompatible},
		{"v2.0.21", "2.0.21", Incompatible},
		{"2.0", "2.0.21", Incompatible},
		{"2.00.21", "2.0.21", Incompatible},
		{"garbage", "2.0.21", Incompatible},
		{"", "2.0.21", Incompatible},
		{"+2.0.21", "2.0.21", Incompatible},
		{"2.0.99999999999999999999", "2.0.21", Incompatible},
		{"1.18.34", "0.0.0-beta-19151", Unbridgeable},
	} {
		t.Run(tt.found+" for "+tt.want, func(t *testing.T) {
			if got := CompareVersions(tt.found, tt.want); got != tt.drift {
				t.Errorf("CompareVersions(%q, %q) = %d, want %d", tt.found, tt.want, got, tt.drift)
			}
		})
	}
	for d, usable := range map[Drift]bool{Exact: true, PatchDrift: true, MinorDrift: true, Incompatible: false, Unbridgeable: false} {
		if d.Usable() != usable {
			t.Errorf("drift %d usable = %v", d, d.Usable())
		}
	}
}

// The locator takes an exact match, then another patch, then another
// minor release, and reports a client further off as a mismatch it can
// still hand to --allow-version-mismatch.
//
// @decision(DL-035)
func TestLocatePrefersTheClosestUsableClient(t *testing.T) {
	home := t.TempDir()
	installed := filepath.Join(home, InstallDir, BinaryName)
	writeExecutable(t, installed)
	onPath := filepath.Join(t.TempDir(), BinaryName)
	writeExecutable(t, onPath)

	locate := func(versions map[string]string) (Located, error) {
		l := Locator{
			HomeDir:  home,
			LookPath: func(string) (string, error) { return onPath, nil },
			Version:  func(_ context.Context, p string) (string, error) { return versions[p], nil },
		}
		return l.Locate(context.Background(), "2.0.21")
	}

	for name, c := range map[string]struct {
		installed, onPath string
		want              string
		drift             Drift
	}{
		"a patch before a minor":        {"2.1.0", "2.0.30", onPath, PatchDrift},
		"an exact match before both":    {"2.0.22", "2.0.21", onPath, Exact},
		"a minor when nothing nearer":   {"2.2.0", "3.0.0", installed, MinorDrift},
		"a patch after a refused major": {"3.0.0", "2.0.1", onPath, PatchDrift},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := locate(map[string]string{installed: c.installed, onPath: c.onPath})
			if err != nil || got.Path != c.want || got.Drift != c.drift {
				t.Errorf("got %+v, %v; want %s at drift %d", got, err, c.want, c.drift)
			}
		})
	}

	// The flag can use a later client of another major release, so it is
	// reported over an earlier beta it could not use.
	got, err := locate(map[string]string{installed: "0.0.0-beta-19151", onPath: "3.0.0"})
	var overridable ClientVersionMismatchError
	if !errors.As(err, &overridable) || overridable.Unbridgeable || got.Path != onPath {
		t.Errorf("a beta then another major: got %+v, %v", got, err)
	}

	got, err = locate(map[string]string{installed: "3.0.0", onPath: "0.0.0-beta-19151"})
	var mismatch ClientVersionMismatchError
	if !errors.As(err, &mismatch) || mismatch.Found != "3.0.0" || mismatch.Unbridgeable || got.Path != installed {
		t.Errorf("another major release: got %+v, %v", got, err)
	}

	got, err = locate(map[string]string{installed: "0.0.0-beta-19151", onPath: "0.0.0-beta-19151"})
	if !errors.As(err, &mismatch) || !mismatch.Unbridgeable || got.Path != installed {
		t.Errorf("a beta against a release: got %+v, %v", got, err)
	}
	if !strings.Contains(err.Error(), "cannot attach to each other") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}
