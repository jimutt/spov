package scripts

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/jimutt/spov/internal/config"
)

// tarGz packs files, name to contents, each executable.
func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// release is a fake GitHub release and npm registry: version 1.2.3 of
// spov for linux/amd64, and OpenCode 2.0.22's linux-x64-baseline package.
type release struct {
	*httptest.Server
	// mu guards what a test changes while the server reads it.
	mu                sync.Mutex
	archive, opencode []byte
	checksums         string
	integrity         string
	pin               string
}

const stubSpov = `#!/bin/sh
case "$1" in
version) echo "spov 1.2.3" ;;
config) printf '{\n  "opencode": {\n    "version": "2.0.21",\n    "port": 4096\n  }\n}\n' ;;
esac
`

func newRelease(t *testing.T) *release {
	t.Helper()
	r := &release{pin: "2.0.22"}
	r.archive = tarGz(t, map[string]string{"spov": stubSpov, "LICENSE": "MIT"})
	sum := sha256.Sum256(r.archive)
	r.checksums = hex.EncodeToString(sum[:]) + "  spov_1.2.3_linux_amd64.tar.gz\n" + strings.Repeat("0", 64) + "  spov_1.2.3_darwin_arm64.tar.gz\n"
	r.opencode = tarGz(t, map[string]string{"package/bin/opencode": "#!/bin/sh\necho opencode\n"})
	osum := sha512.Sum512(r.opencode)
	r.integrity = "sha512-" + base64.StdEncoding.EncodeToString(osum[:])
	mux := http.NewServeMux()
	mux.HandleFunc("GET /releases/latest", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "/releases/tag/v1.2.3", http.StatusFound)
	})
	mux.HandleFunc("GET /releases/tag/v1.2.3", func(w http.ResponseWriter, _ *http.Request) {})
	mux.HandleFunc("GET /releases/download/v1.2.3/spov_1.2.3_linux_amd64.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		_, _ = w.Write(r.archive)
	})
	mux.HandleFunc("GET /releases/download/v1.2.3/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, r.checksums)
	})
	mux.HandleFunc("GET /opencode-pin.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"scheme":1,"opencode":%q}`, r.pin)
	})
	mux.HandleFunc("GET /npm/", func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, ".tgz") {
			_, _ = w.Write(r.opencode)
			return
		}
		if !strings.Contains(req.URL.RawPath+req.URL.Path, "cli-linux-x64-baseline") {
			http.NotFound(w, req)
			return
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"name":"@opencode/cli-linux-x64-baseline","version":%q,"dist":{"integrity":%q,"tarball":"%s/npm/cli.tgz"}}`,
			strings.TrimPrefix(req.URL.Path[strings.LastIndex(req.URL.Path, "/"):], "/"), r.integrity, "http://"+req.Host)
	})
	r.Server = httptest.NewServer(mux)
	t.Cleanup(r.Close)
	return r
}

// install runs install.sh against the fake release, as linux/amd64, with
// a home of its own; extra are more variables.
func (r *release) install(t *testing.T, home string, args []string, extra ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("cosign"); err == nil {
		t.Skip("cosign is installed, and the fake release is not signed")
	}
	cmd := exec.Command("sh", append([]string{"install.sh"}, args...)...)
	cmd.Env = append([]string{"HOME=" + home, "PATH=" + os.Getenv("PATH"),
		"SPOV_RELEASES_URL=" + r.URL + "/releases", "SPOV_NPM_REGISTRY=" + r.URL + "/npm",
		"SPOV_INSTALL_OS=Linux", "SPOV_INSTALL_ARCH=x86_64"}, extra...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// The script finds the latest release, checks the archive against the
// checksums before it unpacks it, and installs to ~/.local/bin; a
// tampered archive installs nothing.
//
// @decision(DL-080)
func TestInstallScriptVerifiesBeforeItUnpacks(t *testing.T) {
	r := newRelease(t)
	home := t.TempDir()
	out, err := r.install(t, home, nil)
	bin := filepath.Join(home, ".local", "bin", "spov")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if b, err := os.ReadFile(bin); err != nil || string(b) != stubSpov || !strings.Contains(out, "installed spov 1.2.3") {
		t.Fatalf("installed %q, %v\n%s", b, err, out)
	}
	if !strings.Contains(out, "not on your PATH") {
		t.Errorf("no PATH advice:\n%s", out)
	}

	tampered := tarGz(t, map[string]string{"spov": "#!/bin/sh\necho tampered\n"})
	r.mu.Lock()
	r.archive = tampered
	r.mu.Unlock()
	home = t.TempDir()
	out, err = r.install(t, home, nil)
	if err == nil || !strings.Contains(out, "does not match its checksum") {
		t.Errorf("a tampered archive: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "bin", "spov")); err == nil {
		t.Error("a tampered archive was installed")
	}
	if out, err := r.install(t, t.TempDir(), nil, "SPOV_REQUIRE_SIGNATURE=1"); err == nil || !strings.Contains(out, "cosign is not installed") {
		t.Errorf("a required signature without cosign: %v\n%s", err, out)
	}
}

// SPOV_VERSION pins a release, with or without its v, and a version the
// release lacks fails cleanly.
//
// @decision(DL-080)
func TestInstallScriptPinsAVersion(t *testing.T) {
	r := newRelease(t)
	for _, v := range []string{"1.2.3", "v1.2.3"} {
		dir := t.TempDir()
		if out, err := r.install(t, t.TempDir(), nil, "SPOV_VERSION="+v, "SPOV_INSTALL_DIR="+dir); err != nil {
			t.Errorf("SPOV_VERSION=%s: %v\n%s", v, err, out)
		}
		if _, err := os.Stat(filepath.Join(dir, "spov")); err != nil {
			t.Errorf("SPOV_VERSION=%s installed nothing in SPOV_INSTALL_DIR", v)
		}
	}
	if out, err := r.install(t, t.TempDir(), nil, "SPOV_VERSION=9.9.9"); err == nil || !strings.Contains(out, "could not download") {
		t.Errorf("a missing version: %v\n%s", err, out)
	}
}

// Only Linux and macOS on amd64 and arm64 are installed, and Windows is
// pointed at WSL.
//
// @decision(DL-080)
func TestInstallScriptRefusesAnUnknownPlatform(t *testing.T) {
	r := newRelease(t)
	for _, c := range []struct{ os, arch, want string }{
		{"FreeBSD", "x86_64", "Linux and macOS"},
		{"MINGW64_NT-10.0", "x86_64", "WSL"},
		{"Linux", "riscv64", "amd64 and arm64"},
	} {
		out, err := r.install(t, t.TempDir(), nil, "SPOV_INSTALL_OS="+c.os, "SPOV_INSTALL_ARCH="+c.arch)
		if err == nil || !strings.Contains(out, c.want) {
			t.Errorf("%s/%s: %v\n%s", c.os, c.arch, err, out)
		}
	}
}

// --with-opencode installs OpenCode at the server's pin, checked against
// npm's integrity, where spov's locator looks first; without a server it
// takes the new binary's own default, and a bad download installs
// nothing.
//
// @decision(DL-080)
func TestInstallScriptFetchesThePinnedOpenCode(t *testing.T) {
	r := newRelease(t)
	home := t.TempDir()
	out, err := r.install(t, home, []string{"--with-opencode"}, "SPOV_SERVER="+r.URL)
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "share", "spov", "bin", "2.0.22", "opencode")); err != nil {
		t.Errorf("OpenCode is not at the server's pin:\n%s", out)
	}
	home = t.TempDir()
	if out, err := r.install(t, home, []string{"--with-opencode"}); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "share", "spov", "bin", "2.0.21", "opencode")); err != nil {
		t.Error("without a server, OpenCode is not at the binary's own default")
	}
	r.mu.Lock()
	r.integrity = "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, 64))
	r.mu.Unlock()
	home = t.TempDir()
	out, err = r.install(t, home, []string{"--with-opencode"}, "SPOV_SERVER="+r.URL)
	if err == nil || !strings.Contains(out, "does not match npm's integrity") {
		t.Errorf("a bad OpenCode download: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "share", "spov", "bin", "2.0.22", "opencode")); err == nil {
		t.Error("a bad OpenCode download was installed")
	}
}

// The real binary reads every SPOV_* variable as config, so the script
// must not hand it its own: SPOV_VERSION is also the config's version
// field, and spov config show then fails, leaving --with-opencode with no
// pin. The stub cannot show this.
//
// @decision(DL-080)
func TestTheRealBinaryNeverSeesTheScriptsVariables(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("the fake release is linux/amd64")
	}
	if testing.Short() {
		t.Skip("builds spov")
	}
	bin := filepath.Join(t.TempDir(), "spov")
	build := exec.Command("go", "build", "-o", bin, "../cmd/spov")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	real, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	r := newRelease(t)
	r.mu.Lock()
	r.archive = tarGz(t, map[string]string{"spov": string(real)})
	sum := sha256.Sum256(r.archive)
	r.checksums = hex.EncodeToString(sum[:]) + "  spov_1.2.3_linux_amd64.tar.gz\n"
	r.mu.Unlock()

	home := t.TempDir()
	out, err := r.install(t, home, []string{"--with-opencode"}, "SPOV_VERSION=1.2.3")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	want := filepath.Join(home, ".local", "share", "spov", "bin", config.DefaultOpenCodeVersion, "opencode")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("OpenCode is not at the binary's own default %s:\n%s", config.DefaultOpenCodeVersion, out)
	}
}
