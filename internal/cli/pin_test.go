package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
)

// envAnswering builds an Env whose server answers the pin endpoint with
// pin. A nil pin 404s it, which is every server older than DL-035.
func envAnswering(t *testing.T, pin *string) *Env {
	t.Helper()
	mux := http.NewServeMux()
	if pin != nil {
		mux.HandleFunc("GET "+server.OpenCodePinPath, func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(server.OpenCodePin{Scheme: server.PinScheme, OpenCode: *pin})
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Env{Client: client.New(srv.URL, "tok"), Environ: config.MapEnv{}}
}

func ptr(s string) *string { return &s }

// TestAnEmptyPinIsNotAPin pins that a deployment answering an empty
// version is treated as "no pin", not as a pin of "".
//
// @decision(DL-035)
func TestAnEmptyPinIsNotAPin(t *testing.T) {
	env := envAnswering(t, ptr(""))
	cfg := config.Config{}
	cfg.OpenCode.Version = "0.0.0-beta-18684"

	version, source := expectedOpenCodeVersion(context.Background(), env, cfg)
	if version != "0.0.0-beta-18684" {
		t.Errorf("version = %q; a deployment that pins nothing must leave the caller's own in place", version)
	}
	if strings.Contains(source, "deployment") {
		t.Errorf("source = %q; nothing was learned from the deployment", source)
	}
}

// TestWithoutAClientTheConfigurationStands: no client, no pin, and no
// launch failure either.
//
// @decision(DL-035)
func TestWithoutAClientTheConfigurationStands(t *testing.T) {
	cfg := config.Config{}
	cfg.OpenCode.Version = "0.0.0-beta-18684"
	version, _ := expectedOpenCodeVersion(context.Background(), &Env{Environ: config.MapEnv{}}, cfg)
	if version != "0.0.0-beta-18684" {
		t.Errorf("version = %q", version)
	}
}

// TestTheLocalBinaryIsLocatedAgainstTheDeploymentsPin pins that the
// local binary is located against the deployment's pin, not the
// client's own config.
//
// @decision(DL-035)
func TestTheLocalBinaryIsLocatedAgainstTheDeploymentsPin(t *testing.T) {
	env := envAnswering(t, ptr("0.0.0-beta-19151"))
	var askedFor string
	env.LocateBinary = func(_ context.Context, version string) (opencode.Located, error) {
		askedFor = version
		return opencode.Located{Path: "/fake/opencode"}, nil
	}
	env.RunTUI = func(context.Context, opencode.TUI) (int, error) { return 0, nil }
	cfg := config.Config{}
	cfg.OpenCode.Version = "0.0.0-beta-18684"

	sess := &liveSession{Health: opencode.Health{Healthy: true, Version: "0.0.0-beta-19151"}}
	if _, err := prepareTUI(context.Background(), env, cfg, sess, flyOptions{}); err != nil {
		t.Fatal(err)
	}
	if askedFor != "0.0.0-beta-19151" {
		t.Errorf("the binary was located for %q; it must be the deployment's pin, not the client's config", askedFor)
	}
}

// TestTheDeploymentsPinBeatsTheClientsConfiguration pins that the
// server's pin decides, not the client's configuration.
//
// @decision(DL-035)
func TestTheDeploymentsPinBeatsTheClientsConfiguration(t *testing.T) {
	env := envAnswering(t, ptr("0.0.0-beta-19151"))
	cfg := config.Config{}
	cfg.OpenCode.Version = "0.0.0-beta-18684"

	version, source := expectedOpenCodeVersion(context.Background(), env, cfg)
	if version != "0.0.0-beta-19151" {
		t.Errorf("expected version %q; the deployment's pin must win over the client's config", version)
	}
	if !strings.Contains(source, "deployment") {
		t.Errorf("source = %q; it must say the number came from the deployment", source)
	}
}

// TestAServerThatCannotAnswerFallsBackToTheConfiguration pins that a
// server that cannot answer leaves the old behaviour in place.
//
// @decision(DL-035)
func TestAServerThatCannotAnswerFallsBackToTheConfiguration(t *testing.T) {
	env := envAnswering(t, nil)
	cfg := config.Config{}
	cfg.OpenCode.Version = "0.0.0-beta-18684"

	version, source := expectedOpenCodeVersion(context.Background(), env, cfg)
	if version != "0.0.0-beta-18684" {
		t.Errorf("version = %q, want the configured one", version)
	}
	if !strings.Contains(source, "configuration") {
		t.Errorf("source = %q; it must say the number came from the caller's own config", source)
	}
}

// TestTheBuiltInDefaultIsNamedAsTheBuiltInDefault pins that spov's
// built-in default is named as such rather than as "configured".
//
// @decision(DL-035)
func TestTheBuiltInDefaultIsNamedAsTheBuiltInDefault(t *testing.T) {
	env := envAnswering(t, nil)
	cfg := config.Config{}
	cfg.OpenCode.Version = config.DefaultOpenCodeVersion

	version, source := expectedOpenCodeVersion(context.Background(), env, cfg)
	if version != config.DefaultOpenCodeVersion {
		t.Errorf("version = %q", version)
	}
	if !strings.Contains(source, "built-in") {
		t.Errorf("source = %q; a caller with no config file must be told the number is spov's own", source)
	}
}

// TestTheMismatchMessageNamesWhereEachVersionCameFrom pins that the
// mismatch message says where each number came from.
//
// @decision(DL-035)
func TestTheMismatchMessageNamesWhereEachVersionCameFrom(t *testing.T) {
	err := opencode.CheckVersion("0.0.0-beta-18684", "spov's built-in default is", "0.0.0-beta-19151")
	if err == nil {
		t.Fatal("two different versions did not mismatch")
	}
	msg := err.Error()
	for _, want := range []string{"built-in default", "0.0.0-beta-18684", "0.0.0-beta-19151"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not contain %q", msg, want)
		}
	}
	// The expected version must not sit behind "sandbox server is",
	// which would send the reader to the wrong component.
	if strings.Contains(msg, "sandbox server is 0.0.0-beta-18684") {
		t.Errorf("message %q still reports the configured version as the sandbox's", msg)
	}
}

// The local client's mismatch and the sandbox's are different
// sentences, and the types are separate so they cannot be confused.
//
// @decision(DL-035) @decision(DL-025)
func TestTheTwoMismatchesReadDifferently(t *testing.T) {
	local := opencode.ClientVersionMismatchError{Found: "v-old", Want: "v-new"}.Error()
	sandbox := opencode.VersionMismatchError{
		Expected: "v-new", Source: "this deployment pins", Reported: "v-old",
	}.Error()
	if !strings.Contains(local, "local client") {
		t.Errorf("the local mismatch says %q and does not name the local client", local)
	}
	if !strings.Contains(sandbox, "sandbox") {
		t.Errorf("the sandbox mismatch says %q and does not name the sandbox", sandbox)
	}
	if local == sandbox {
		t.Error("the two mismatches read identically")
	}
}

// How the handover treats a local client by its drift from the pin: a
// friendly note for another patch, a warning for another minor release,
// a refusal for another major release that the flag overrides, and a
// refusal for a beta against a release that it does not.
//
// @decision(DL-035)
func TestTheLocalClientIsGradedAgainstThePin(t *testing.T) {
	for _, c := range []struct {
		name, found      string
		allow, wantErr   bool
		unbridgeable     bool
		say, notSay, err string
	}{
		{name: "the pin itself", found: "2.0.21", notSay: "2.0.21"},
		{name: "another patch", found: "2.0.23", say: "Using your local " + opencode.BinaryName + " 2.0.23; this deployment pins 2.0.21.", notSay: "warning"},
		{name: "another minor", found: "2.1.0", say: "warning: your local " + opencode.BinaryName + " is 2.1.0, and this deployment pins 2.0.21; a client from another minor release may not match the server."},
		{name: "another major", found: "3.0.0", wantErr: true, err: "re-run with --allow-version-mismatch"},
		{name: "another major, overridden", found: "3.0.0", allow: true, say: "continuing because --allow-version-mismatch"},
		{name: "a beta", found: "0.0.0-beta-19151", wantErr: true, unbridgeable: true, err: "cannot attach to each other"},
		{name: "a beta, overridden", found: "0.0.0-beta-19151", allow: true, wantErr: true, unbridgeable: true, notSay: "continuing"},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := envAnswering(t, ptr("2.0.21"))
			var stderr bytes.Buffer
			env.Err = &stderr
			env.LocateBinary = func(ctx context.Context, want string) (opencode.Located, error) {
				dir := t.TempDir()
				bin := filepath.Join(dir, opencode.BinaryName)
				if err := os.WriteFile(bin, []byte("#!/bin/sh\necho "+opencode.BinaryName+" v"+c.found+"\n"), 0o755); err != nil {
					t.Fatal(err)
				}
				l := opencode.Locator{LookPath: func(string) (string, error) { return bin, nil }}
				return l.Locate(ctx, want)
			}
			sess := &liveSession{Health: opencode.Health{Healthy: true, Version: "2.0.21"}}
			binary, err := prepareTUI(context.Background(), env, config.Config{}, sess, flyOptions{AllowVersionMismatch: c.allow})
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v", err)
			}
			if err == nil && !strings.HasSuffix(binary, opencode.BinaryName) {
				t.Errorf("handed over %q", binary)
			}
			var mismatch opencode.ClientVersionMismatchError
			if c.unbridgeable && (!errors.As(err, &mismatch) || !mismatch.Unbridgeable) {
				t.Errorf("err = %v, want an unbridgeable mismatch", err)
			}
			if c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)) {
				t.Errorf("err = %v, want it to say %q", err, c.err)
			}
			if c.say != "" && !strings.Contains(stderr.String(), c.say) {
				t.Errorf("printed %q, want it to say %q", stderr.String(), c.say)
			}
			if c.notSay != "" && strings.Contains(stderr.String(), c.notSay) {
				t.Errorf("printed %q, which should not say %q", stderr.String(), c.notSay)
			}
		})
	}
}
