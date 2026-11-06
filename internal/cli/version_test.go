package cli

import (
	"strings"
	"testing"
)

// `version --embedded` says whether the web app and the proxy are built
// in, in the lines the release check reads.
//
// @decision(DL-079)
func TestVersionReportsWhatIsEmbedded(t *testing.T) {
	if got := embeddedReport(true, false); got != "web: built\nproxy: placeholder\n" {
		t.Errorf("report = %q", got)
	}
	out, _, err := run(t, Env{}, "version", "--embedded")
	if err != nil || !strings.Contains(out, "web: ") || !strings.Contains(out, "proxy: ") {
		t.Errorf("out = %q, %v", out, err)
	}
}
