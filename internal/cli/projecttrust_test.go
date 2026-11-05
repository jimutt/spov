package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jimutt/spov/internal/config"
)

// In a clone whose project file names a server or redefines a platform,
// every command refuses before a request leaves, and a completion
// offers nothing, so the token never reaches the host the file names.
//
// @decision(DL-026)
func TestAProjectFileCannotRedirectCredentials(t *testing.T) {
	var asked atomic.Int32
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		http.NotFound(w, r)
	}))
	t.Cleanup(evil.Close)

	for _, project := range []string{
		"server:\n  url: " + evil.URL + "\n",
		"platforms:\n  - name: codefloe\n    kind: forgejo\n    base_url: " + evil.URL + "\n    account: alice\n",
	} {
		wd := t.TempDir()
		writeProject(t, wd, project)
		env := Env{
			Workdir: wd,
			Environ: config.MapEnv{Vars: map[string]string{ServerTokenEnvVar: "tok-env"}},
			Spawn:   func(context.Context) error { t.Error("a server was spawned"); return nil },
		}
		for _, args := range [][]string{{"ls"}, {"credentials"}, {"config", "show"}} {
			_, _, err := runWith(t, env, args...)
			if err == nil || !strings.Contains(err.Error(), "a project file may not set") {
				t.Errorf("%v with %q: %v", args, project, err)
			}
		}
		if got := completions(t, env, "attach", ""); len(got) != 0 {
			t.Errorf("completion with %q offered %v", project, got)
		}
	}
	if n := asked.Load(); n != 0 {
		t.Errorf("the host the project file named was asked %d times", n)
	}
}
