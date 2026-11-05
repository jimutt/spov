// Package scripts tests the release and install scripts by running them,
// against stand-ins for the binary and the release server.
package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// stubBinary writes an executable that answers `version` the way spov
// does, with the given embedded state and version.
func stubBinary(t *testing.T, web, proxy, version string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "spov")
	script := "#!/bin/sh\ncase \"$2\" in\n--short) echo " + version + " ;;\n--embedded) echo 'spov " + version +
		"'; echo 'web: " + web + "'; echo 'proxy: " + proxy + "' ;;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// The release check passes a binary with both parts built at the tag's
// version, and refuses one missing either part or at another version.
//
// @decision(DL-079)
func TestTheReleaseCheckRefusesAPlaceholderBuild(t *testing.T) {
	for _, c := range []struct {
		name, web, proxy, version string
		ok                        bool
	}{
		{"both built", "built", "built", "1.2.3", true},
		{"no web app", "placeholder", "built", "1.2.3", false},
		{"no proxy", "built", "placeholder", "1.2.3", false},
		{"another version", "built", "built", "1.2.4", false},
	} {
		out, err := exec.Command("bash", "release-check.sh", stubBinary(t, c.web, c.proxy, c.version), "v1.2.3").CombinedOutput()
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v\n%s", c.name, err, out)
		}
		if !c.ok && !strings.Contains(string(out), "release-check:") {
			t.Errorf("%s: no reason given:\n%s", c.name, out)
		}
	}
}
