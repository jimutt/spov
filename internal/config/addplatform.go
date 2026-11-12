package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/knadh/koanf/parsers/yaml"
)

// AddOutcome is what AddPlatform did with the user file.
//
// @decision(DL-124)
type AddOutcome struct {
	// Added says the file was created or appended to.
	Added bool
	// Existing is the name of a platform on the same host and of the
	// same kind that the file already had; nothing was written.
	Existing string
	// NotEditable says the file was left alone: it has a platforms list
	// (HasList), or a shape that an append could break.
	NotEditable bool
	HasList     bool

	path    string
	before  []byte
	existed bool
}

// Restore puts the file back as it was before an Added outcome.
func (o AddOutcome) Restore() error {
	if !o.Added {
		return nil
	}
	if !o.existed {
		return os.Remove(o.path)
	}
	return writeThrough(o.path, o.before)
}

// PlatformSnippet is the YAML for one platform, as AddPlatform appends
// it and as a caller prints it for a file it would not edit.
//
// @decision(DL-124)
func PlatformSnippet(p Platform, newline string) string {
	lines := []string{"platforms:", "  - name: " + p.Name, "    kind: " + string(p.Kind)}
	if p.BaseURL != "" {
		lines = append(lines, "    base_url: "+p.BaseURL)
	}
	if p.Account != "" {
		// Quoted: a login such as 123 or true is still a string.
		lines = append(lines, "    account: "+strconv.Quote(p.Account))
	}
	return strings.Join(lines, newline) + newline
}

// AddPlatform adds one platform to the user file at path, and only ever
// by creating the file or appending a new platforms block to one that
// has none: a hand-written list is never rewritten. The rest of the file
// is left byte for byte as it was, in its own line endings, and a
// symbolic link is written through.
//
// @decision(DL-124) @decision(DL-026)
func AddPlatform(path string, p Platform) (AddOutcome, error) {
	out := AddOutcome{path: path}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		body := "# spov's configuration for this user. Every key is in docs/configuration.md:\n" +
			"# https://github.com/jimutt/spov/blob/main/docs/configuration.md\n" + PlatformSnippet(p, "\n")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return out, fmt.Errorf("config: %w", err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			return out, fmt.Errorf("config: write %s: %w", path, err)
		}
		out.Added = true
		return out, nil
	case err != nil:
		return out, fmt.Errorf("config: read %s: %w", path, err)
	}
	out.before, out.existed = raw, true

	top, err := yaml.Parser().Unmarshal(raw)
	if err != nil {
		return out, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if name := samePlatform(top["platforms"], p); name != "" {
		out.Existing = name
		return out, nil
	}
	if _, has := top["platforms"]; has || !appendable(raw) {
		out.NotEditable, out.HasList = true, has
		return out, nil
	}
	newline := "\n"
	if bytes.Contains(raw, []byte("\r\n")) {
		newline = "\r\n"
	}
	body := append([]byte{}, raw...)
	if len(body) > 0 && !bytes.HasSuffix(body, []byte("\n")) {
		body = append(body, newline...)
	}
	if len(bytes.TrimSpace(body)) > 0 {
		body = append(body, newline...)
	}
	body = append(body, PlatformSnippet(p, newline)...)
	if err := writeThrough(path, body); err != nil {
		return out, err
	}
	out.Added = true
	return out, nil
}

// samePlatform is the name of a listed platform of p's kind on p's host,
// or "".
func samePlatform(list any, p Platform) string {
	items, _ := list.([]any)
	for _, item := range items {
		m, _ := item.(map[string]any)
		kind, _ := m["kind"].(string)
		base, _ := m["base_url"].(string)
		name, _ := m["name"].(string)
		if kind == string(p.Kind) && hostOf(base) == hostOf(p.BaseURL) && name != "" {
			return name
		}
	}
	return ""
}

// hostOf is a base URL's host, lowercased, and "" for none: a GitHub
// platform with no base URL is github.com's.
func hostOf(base string) string {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Hostname() == "" || strings.EqualFold(u.Hostname(), "github.com") {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// appendable reports whether a block can be appended to the document:
// one block-style document with no markers.
func appendable(raw []byte) bool {
	first := true
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "---") || strings.HasPrefix(line, "...") {
			return false
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		// A flow-style root, or a root mapping that is indented: a
		// block appended at column zero would not join either.
		if first && (strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") || strings.TrimLeft(line, " \t") != line) {
			return false
		}
		first = false
	}
	return true
}

// writeThrough replaces path's content through a rename, writing to the
// file a symbolic link names so the link survives.
func writeThrough(path string, body []byte) error {
	target := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		target = resolved
	}
	mode := os.FileMode(0o600)
	if info, err := os.Stat(target); err == nil {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".config-*")
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return fmt.Errorf("config: write %s: %w", path, err)
	}
	return nil
}
