package sandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha512"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/opencode/contract"
)

// installRig runs the rendered OpenCode install under sh, with curl and
// dpkg replaced by stubs: curl serves a package built here and the
// registry's metadata for it, and logs every URL it was asked for.
type installRig struct {
	dir, bin, log string
	tgz           []byte
	integrity     string
}

func newInstallRig(t *testing.T, binaryName, version string) *installRig {
	t.Helper()
	dir := t.TempDir()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	script := "#!/bin/sh\necho '" + binaryName + " v" + version + "'\n"
	for name, body := range map[string]string{"package/package.json": "{}", "package/bin/" + binaryName: script} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha512.Sum512(buf.Bytes())
	r := &installRig{
		dir: dir, bin: filepath.Join(dir, "root", "bin", "opencode2"), log: filepath.Join(dir, "urls"),
		tgz: buf.Bytes(), integrity: "sha512-" + base64.StdEncoding.EncodeToString(sum[:]),
	}
	stubs := filepath.Join(dir, "stubs")
	if err := os.MkdirAll(stubs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.tgz"), r.tgz, 0o644); err != nil {
		t.Fatal(err)
	}
	curl := `#!/bin/sh
out=; url=
while [ $# -gt 0 ]; do case "$1" in -o) out=$2; shift 2;; -*) shift;; *) url=$1; shift;; esac; done
echo "$url" >> "` + r.log + `"
case "$url" in
*.tgz) cp "` + filepath.Join(dir, "package.tgz") + `" "$out";;
*) printf '{"name":"x","dist":{"integrity":"%s","tarball":"y"}}' "$REGISTRY_INTEGRITY";;
esac
`
	for name, body := range map[string]string{"curl": curl, "dpkg": "#!/bin/sh\necho \"$ARCH\"\n"} {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

// run executes the install and the version check for spec as the image
// build would, and returns the combined output.
func (r *installRig) run(t *testing.T, spec ImageSpec, arch, registryIntegrity string) (string, error) {
	t.Helper()
	_ = os.Remove(r.log)
	cmd := exec.Command("sh", "-c", spec.openCodeInstall(r.bin)+"; "+spec.openCodeVersionCheck(r.bin))
	cmd.Env = []string{
		"PATH=" + filepath.Join(r.dir, "stubs") + string(os.PathListSeparator) + os.Getenv("PATH"),
		"ARCH=" + arch, "REGISTRY_INTEGRITY=" + registryIntegrity, "TMPDIR=" + r.dir,
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (r *installRig) urls(t *testing.T) string {
	t.Helper()
	b, _ := os.ReadFile(r.log)
	return string(b)
}

// withRelease records a release for the length of a test.
func withRelease(t *testing.T, version string, rel openCodeRelease) {
	t.Helper()
	saved, had := openCodeReleases[version]
	openCodeReleases[version] = rel
	t.Cleanup(func() {
		if had {
			openCodeReleases[version] = saved
		} else {
			delete(openCodeReleases, version)
		}
	})
}

// The image installs the recorded package for its architecture, checks
// it against the recorded integrity before unpacking it, and refuses
// one that does not match.
//
// @decision(DL-032)
func TestTheImageInstallsThePinnedPackageAndChecksIt(t *testing.T) {
	if _, err := exec.LookPath("sha512sum"); err != nil {
		t.Skip("no coreutils here")
	}
	const version = "0.0.0-test-1"
	r := newInstallRig(t, "opencode2", version)
	spec := ImageSpec{OpenCodeVersion: version}

	withRelease(t, version, openCodeRelease{Scope: "@opencode-ai", Binary: "opencode2", Integrity: map[string]string{
		"linux-x64-baseline": r.integrity, "linux-arm64": r.integrity,
	}})
	for arch, target := range map[string]string{"amd64": "linux-x64-baseline", "arm64": "linux-arm64"} {
		_ = os.Remove(r.bin)
		if out, err := r.run(t, spec, arch, "unused"); err != nil {
			t.Fatalf("%s: the install failed: %v\n%s", arch, err, out)
		}
		want := "https://registry.npmjs.org/@opencode-ai/cli-" + target + "/-/cli-" + target + "-" + version + ".tgz"
		if got := strings.TrimSpace(r.urls(t)); got != want {
			t.Errorf("%s: fetched %q, want only %q", arch, got, want)
		}
		if _, err := os.Stat(r.bin); err != nil {
			t.Errorf("%s: nothing was installed at %s", arch, r.bin)
		}
	}

	_ = os.Remove(r.bin)
	withRelease(t, version, openCodeRelease{Scope: "@opencode-ai", Binary: "opencode2", Integrity: map[string]string{
		"linux-x64-baseline": "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, 64)),
	}})
	out, err := r.run(t, spec, "amd64", "unused")
	if err == nil || !strings.Contains(out, "does not match its integrity") {
		t.Errorf("a package that does not match was accepted: %v\n%s", err, out)
	}
	if _, err := os.Stat(r.bin); err == nil {
		t.Error("a package that does not match was unpacked")
	}

	out, err = r.run(t, spec, "riscv64", "unused")
	if err == nil || !strings.Contains(out, "no OpenCode build for riscv64") {
		t.Errorf("an unknown architecture was not refused: %v\n%s", err, out)
	}
}

// A version with no recorded integrity takes the registry's, says so,
// and is still checked against it.
//
// @decision(DL-032)
func TestAnUnrecordedVersionUsesTheRegistrysIntegrity(t *testing.T) {
	if _, err := exec.LookPath("sha512sum"); err != nil {
		t.Skip("no coreutils here")
	}
	const version = "0.0.0-test-unrecorded"
	r := newInstallRig(t, unrecordedRelease.Binary, version)
	spec := ImageSpec{OpenCodeVersion: version}

	out, err := r.run(t, spec, "amd64", r.integrity)
	if err != nil {
		t.Fatalf("the install failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "has no recorded integrity; the registry gives "+r.integrity) {
		t.Errorf("the build did not say where the integrity came from:\n%s", out)
	}
	if !strings.Contains(r.urls(t), "/cli-linux-x64-baseline/"+version+"\n") {
		t.Errorf("the registry's metadata was not read: %s", r.urls(t))
	}

	_ = os.Remove(r.bin)
	out, err = r.run(t, spec, "amd64", "")
	if err == nil || !strings.Contains(out, "the registry gave no sha512 integrity") {
		t.Errorf("a registry that gave no integrity was accepted: %v\n%s", err, out)
	}
	if _, err := os.Stat(r.bin); err == nil {
		t.Error("a package with no integrity was unpacked")
	}

	other := sha512.Sum512([]byte("another package"))
	if out, err := r.run(t, spec, "amd64", "sha512-"+base64.StdEncoding.EncodeToString(other[:])); err == nil {
		t.Errorf("a package that does not match the registry's integrity was accepted:\n%s", out)
	}
}

// A package whose binary has another name than the release records, as
// the beta's had when the live installer changed, fails the build.
//
// @decision(DL-032)
func TestAPackageWithAnotherBinaryNameFailsTheBuild(t *testing.T) {
	if _, err := exec.LookPath("sha512sum"); err != nil {
		t.Skip("no coreutils here")
	}
	const version = "0.0.0-test-4"
	r := newInstallRig(t, "opencode", version)
	withRelease(t, version, openCodeRelease{Scope: "@opencode", Binary: "opencode2", Integrity: map[string]string{
		"linux-x64-baseline": r.integrity,
	}})
	if out, err := r.run(t, ImageSpec{OpenCodeVersion: version}, "amd64", ""); err == nil {
		t.Errorf("a package without the recorded binary passed:\n%s", out)
	}
}

// The build fails when the installed binary is not the pinned version.
//
// @decision(DL-032)
func TestTheImageChecksTheInstalledVersion(t *testing.T) {
	if _, err := exec.LookPath("sha512sum"); err != nil {
		t.Skip("no coreutils here")
	}
	const version = "0.0.0-test-2"
	r := newInstallRig(t, "opencode2", "0.0.0-test-other")
	withRelease(t, version, openCodeRelease{Scope: "@opencode-ai", Binary: "opencode2", Integrity: map[string]string{
		"linux-x64-baseline": r.integrity,
	}})
	out, err := r.run(t, ImageSpec{OpenCodeVersion: version}, "amd64", "")
	if err == nil || !strings.Contains(out, "reports 0.0.0-test-other, not "+version) {
		t.Errorf("a binary of another version passed the check: %v\n%s", err, out)
	}
}

// The pin is recorded, and every recorded release has a well-formed
// sha512 integrity for every target the image installs, so a pinned
// build never falls back to the registry's own value.
//
// @decision(DL-032)
func TestEveryRecordedReleaseCoversEveryTarget(t *testing.T) {
	if _, ok := openCodeReleases[contract.Version()]; !ok {
		t.Errorf("the pin, %s, has no entry in openCodeReleases", contract.Version())
	}
	for version, rel := range openCodeReleases {
		for _, target := range openCodeTargets {
			raw, ok := strings.CutPrefix(rel.Integrity[target.Target], "sha512-")
			sum, err := base64.StdEncoding.DecodeString(raw)
			if !ok || err != nil || len(sum) != sha512.Size {
				t.Errorf("%s has no well-formed sha512 integrity for %s", version, target.Target)
			}
		}
	}
}

// A change to how OpenCode is installed is a new image, never a reuse of
// one built from another install.
//
// @decision(DL-032)
func TestAChangedInstallIsANewImage(t *testing.T) {
	const version = "0.0.0-test-3"
	before := ImageSpec{OpenCodeVersion: version}.Name()
	withRelease(t, version, openCodeRelease{Scope: "@opencode-ai", Binary: "opencode2", Integrity: map[string]string{
		"linux-x64-baseline": "sha512-a", "linux-arm64": "sha512-b",
	}})
	if after := (ImageSpec{OpenCodeVersion: version}).Name(); after == before {
		t.Errorf("recording an integrity kept the image name %q", after)
	}
}
