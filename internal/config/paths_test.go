package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestUserConfigPath(t *testing.T) {
	tests := []struct {
		name string
		env  MapEnv
		want string
		err  error
	}{
		{
			name: "xdg wins over home",
			env:  MapEnv{Vars: map[string]string{"XDG_CONFIG_HOME": "/xdg"}, Home: "/home/u"},
			want: filepath.Join("/xdg", "spov", "config.yaml"),
		},
		{
			name: "falls back to home/.config",
			env:  MapEnv{Home: "/home/u"},
			want: filepath.Join("/home/u", ".config", "spov", "config.yaml"),
		},
		{
			name: "no home at all",
			env:  MapEnv{},
			err:  ErrNoHome,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := UserConfigPath(tt.env)
			if tt.err != nil {
				if !errors.Is(err, tt.err) {
					t.Fatalf("err = %v, want %v", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFindProjectFile(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "a", ProjectFileName)
	if err := os.WriteFile(marker, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("finds nearest ancestor", func(t *testing.T) {
		got, err := FindProjectFile(nested)
		if err != nil {
			t.Fatal(err)
		}
		if got != marker {
			t.Errorf("got %q, want %q", got, marker)
		}
	})

	t.Run("prefers the closest file", func(t *testing.T) {
		closer := filepath.Join(root, "a", "b", ProjectFileName)
		if err := os.WriteFile(closer, []byte("version: 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(closer) })
		got, err := FindProjectFile(nested)
		if err != nil {
			t.Fatal(err)
		}
		if got != closer {
			t.Errorf("got %q, want %q", got, closer)
		}
	})

	t.Run("returns empty when absent", func(t *testing.T) {
		other := t.TempDir()
		got, err := FindProjectFile(other)
		if err != nil {
			t.Fatal(err)
		}
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("ignores a directory named .spov.yaml", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, ProjectFileName), 0o755); err != nil {
			t.Fatal(err)
		}
		got, err := FindProjectFile(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

func TestMapEnvEnvironIsSorted(t *testing.T) {
	e := MapEnv{Vars: map[string]string{"B": "2", "A": "1", "C": "3"}}
	got := e.Environ()
	want := []string{"A=1", "B=2", "C=3"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestOSEnv(t *testing.T) {
	t.Setenv("SPOV_TEST_OSENV", "yes")
	var e Env = OSEnv{}
	if e.Getenv("SPOV_TEST_OSENV") != "yes" {
		t.Error("Getenv did not read the process environment")
	}
	if len(e.Environ()) == 0 {
		t.Error("Environ returned nothing")
	}
	if _, err := e.UserHomeDir(); err != nil {
		t.Errorf("UserHomeDir: %v", err)
	}
}

// TestUserConfigPathRejectsANilEnv: callers construct an Env with
// optional fields, so a nil environment must produce an error rather
// than a nil-pointer panic deep in config loading.
func TestUserConfigPathRejectsANilEnv(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("UserConfigPath panicked on a nil Env: %v", r)
		}
	}()
	if _, err := UserConfigPath(nil); err == nil {
		t.Error("a nil Env was accepted")
	}
}
