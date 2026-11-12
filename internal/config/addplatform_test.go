package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var gh = Platform{Name: "github", Kind: KindGitHub, Account: "ada"}

// loads reports whether the user file at path loads and holds one
// platform named github.
func loads(t *testing.T, dir string) bool {
	t.Helper()
	res, err := LoadUser(MapEnv{Vars: map[string]string{"XDG_CONFIG_HOME": dir}})
	return err == nil && len(res.Config.Platforms) == 1 && res.Config.Platforms[0].Name == "github" &&
		res.Config.Platforms[0].Account == "ada"
}

func userFile(t *testing.T, body *string) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "spov", UserFileName)
	if body != nil {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(*body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir, path
}

// A missing file is created, and a file with no platforms key gets the
// block appended, the rest of it byte for byte as it was, in its own
// line endings.
//
// @decision(DL-124)
func TestAddPlatformCreatesOrAppends(t *testing.T) {
	dir, path := userFile(t, nil)
	out, err := AddPlatform(path, gh)
	if err != nil || !out.Added || !loads(t, dir) {
		t.Fatalf("a missing file: %+v, %v", out, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("created with mode %v", info.Mode().Perm())
	}
	if err := out.Restore(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("restoring a created file left it")
	}

	for name, before := range map[string]string{
		"with comments":         "# mine\nsandbox:\n  egress: install # keep\n",
		"no trailing newline":   "sandbox:\n  egress: install",
		"windows line endings":  "sandbox:\r\n  egress: install\r\n",
		"a commented platforms": "# platforms:\n#   - name: old\nsandbox:\n  egress: install\n",
		"empty":                 "",
	} {
		dir, path := userFile(t, &before)
		out, err := AddPlatform(path, gh)
		if err != nil || !out.Added {
			t.Fatalf("%s: %+v, %v", name, out, err)
		}
		after, _ := os.ReadFile(path)
		if !strings.HasPrefix(string(after), before) {
			t.Errorf("%s: the file's own content changed:\n%q", name, after)
		}
		if strings.Contains(before, "\r\n") && strings.Contains(strings.ReplaceAll(string(after), "\r\n", ""), "\n") {
			t.Errorf("%s: mixed line endings:\n%q", name, after)
		}
		if !loads(t, dir) {
			t.Errorf("%s: the result does not load with the platform:\n%s", name, after)
		}
		if info, _ := os.Stat(path); info.Mode().Perm() != 0o644 {
			t.Errorf("%s: the file's mode changed to %v", name, info.Mode().Perm())
		}
		if err := out.Restore(); err != nil {
			t.Fatal(err)
		}
		if restored, _ := os.ReadFile(path); string(restored) != before {
			t.Errorf("%s: restore left %q", name, restored)
		}
	}
}

// A hand-written list, or a document an append could break, is never
// edited, and a platform already on the host is used as it is.
//
// @decision(DL-124)
func TestAddPlatformNeverEditsAList(t *testing.T) {
	for name, before := range map[string]string{
		"another platform":  "platforms:\n  - name: codefloe # mine\n    kind: forgejo\n    base_url: https://codefloe.com\n",
		"github enterprise": "platforms:\n  - name: work\n    kind: github\n    base_url: https://ghe.acme.com\n",
		"a document marker": "---\nsandbox:\n  egress: install\n",
		"an indented root":  "  sandbox:\n    egress: install\n",
		"a flow root":       "{sandbox: {egress: install}}\n",
	} {
		_, path := userFile(t, &before)
		out, err := AddPlatform(path, gh)
		if err != nil || out.Added || !out.NotEditable {
			t.Errorf("%s: %+v, %v", name, out, err)
		}
		if after, _ := os.ReadFile(path); string(after) != before {
			t.Errorf("%s: the file was changed:\n%s", name, after)
		}
	}
	for _, before := range []string{
		"platforms:\n  - name: gh\n    kind: github\n    account: ada\n",
		"platforms:\n  - name: gh\n    kind: github\n    base_url: https://github.com\n",
	} {
		_, path := userFile(t, &before)
		out, err := AddPlatform(path, gh)
		if err != nil || out.Added || out.NotEditable || out.Existing != "gh" {
			t.Errorf("an existing github.com platform: %+v, %v", out, err)
		}
	}
	broken := "sandbox: [\n"
	_, path := userFile(t, &broken)
	if _, err := AddPlatform(path, gh); err == nil {
		t.Error("a file that does not parse was accepted")
	}
}

// A symbolic link, as a dotfiles setup has, is written through and kept.
//
// @decision(DL-124)
func TestAddPlatformWritesThroughALink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(t.TempDir(), "dotfiles-config.yaml")
	if err := os.WriteFile(real, []byte("sandbox:\n  egress: install\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "spov", UserFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, path); err != nil {
		t.Skip(err)
	}
	if out, err := AddPlatform(path, gh); err != nil || !out.Added {
		t.Fatalf("%+v, %v", out, err)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Error("the link was replaced by a file")
	}
	if b, _ := os.ReadFile(real); !strings.Contains(string(b), "name: github") {
		t.Errorf("the link's target was not written:\n%s", b)
	}
}

// The account is written as a string whatever it looks like.
//
// @decision(DL-124)
func TestAddPlatformQuotesTheAccount(t *testing.T) {
	for _, login := range []string{"123", "true", "null", "0x1f"} {
		dir, path := userFile(t, nil)
		if _, err := AddPlatform(path, Platform{Name: "github", Kind: KindGitHub, Account: login}); err != nil {
			t.Fatal(err)
		}
		res, err := LoadUser(MapEnv{Vars: map[string]string{"XDG_CONFIG_HOME": dir}})
		if err != nil || len(res.Config.Platforms) != 1 || res.Config.Platforms[0].Account != login {
			t.Errorf("the login %q loaded as %+v, %v", login, res.Config.Platforms, err)
		}
	}
}
