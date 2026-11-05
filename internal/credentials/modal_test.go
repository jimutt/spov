package credentials

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveModalFromEnvironment(t *testing.T) {
	env := map[string]string{
		"MODAL_TOKEN_ID":     "ak-123",
		"MODAL_TOKEN_SECRET": "as-456",
	}
	got, err := ResolveModal(func(k string) string { return env[k] }, func() (string, error) {
		t.Error("the home directory was consulted even though the environment was set")
		return "", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.HasTokens() || got.TokenID != "ak-123" || got.TokenSecret != "as-456" {
		t.Errorf("credentials = %+v", got)
	}
	// Source is for diagnostics and may be printed.
	if strings.Contains(got.Source, "as-456") {
		t.Errorf("Source leaked the token secret: %q", got.Source)
	}
}

func TestResolveModalRejectsHalfSetEnvironment(t *testing.T) {
	for _, only := range []string{"MODAL_TOKEN_ID", "MODAL_TOKEN_SECRET"} {
		t.Run(only, func(t *testing.T) {
			_, err := ResolveModal(
				func(k string) string {
					if k == only {
						return "value"
					}
					return ""
				},
				func() (string, error) { return t.TempDir(), nil },
			)
			if err == nil {
				t.Fatal("half-set Modal credentials were accepted")
			}
			if !strings.Contains(err.Error(), "together") {
				t.Errorf("err = %v", err)
			}
		})
	}
}

func TestResolveModalFromTokenFile(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ModalTokenFile), []byte("[default]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveModal(func(string) string { return "" }, func() (string, error) { return home, nil })
	if err != nil {
		t.Fatal(err)
	}
	if got.HasTokens() {
		t.Error("spov must not parse the token file itself; the SDK reads it")
	}
	if !strings.Contains(got.Source, ModalTokenFile) {
		t.Errorf("Source = %q, want the token file path", got.Source)
	}
}

func TestResolveModalWithNothingConfigured(t *testing.T) {
	_, err := ResolveModal(func(string) string { return "" }, func() (string, error) { return t.TempDir(), nil })
	if err == nil {
		t.Fatal("expected an error")
	}
	// The message has to tell the user what to actually do.
	if !strings.Contains(err.Error(), "modal setup") {
		t.Errorf("err = %v", err)
	}
}

func TestResolveModalWithoutHome(t *testing.T) {
	sentinel := errors.New("no home")
	_, err := ResolveModal(func(string) string { return "" }, func() (string, error) { return "", sentinel })
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want it to wrap the home-directory error", err)
	}
}
