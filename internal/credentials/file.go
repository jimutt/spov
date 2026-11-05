package credentials

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// FileMode is the permission the credentials file is created with and
// required to have. Anything wider is refused rather than silently used.
//
// @decision(DL-008)
const FileMode fs.FileMode = 0o600

// DirMode is the permission the credentials directory is created with.
//
// @decision(DL-008)
const DirMode fs.FileMode = 0o700

// FileWarning is shown the first time a secret is written to the file
// backend, so nobody is surprised about where their tokens live.
//
// @decision(DL-008)
const FileWarning = "spov: the OS keyring is unavailable; secrets are stored UNENCRYPTED in %s (mode 0600).\n" +
	"spov: anything that can read your files as this user can read them. Back this file up accordingly.\n"

// FileStore keeps secrets in a JSON file readable only by the owner. It is
// the fallback for systems with no working OS keyring, notably WSL.
//
// @decision(DL-008)
type FileStore struct {
	path string

	mu     sync.Mutex
	warned bool
	// Warn receives the one-time storage warning. Nil discards it.
	Warn func(string)
}

// NewFileStore returns a store backed by path.
func NewFileStore(path string) *FileStore {
	return &FileStore{path: path}
}

// DefaultFilePath is where the fallback file lives.
func DefaultFilePath(configDir string) string {
	return filepath.Join(configDir, "credentials.json")
}

func (s *FileStore) Name() string { return "file:" + s.path }

// Path is the file secrets are stored in.
func (s *FileStore) Path() string { return s.path }

func (s *FileStore) Get(key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	secrets, err := s.load()
	if err != nil {
		return "", err
	}
	secret, ok := secrets[key]
	if !ok {
		return "", fmt.Errorf("credentials: %q: %w", key, ErrNotFound)
	}
	return secret, nil
}

func (s *FileStore) Set(key, secret string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	secrets, err := s.load()
	if err != nil {
		return err
	}
	secrets[key] = secret
	if err := s.save(secrets); err != nil {
		return err
	}
	s.warnOnce()
	return nil
}

func (s *FileStore) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	secrets, err := s.load()
	if err != nil {
		return err
	}
	if _, ok := secrets[key]; !ok {
		return fmt.Errorf("credentials: %q: %w", key, ErrNotFound)
	}
	delete(secrets, key)
	return s.save(secrets)
}

// Keys lists the stored credential keys, sorted. Values are never
// returned; this exists so `spov ls` can find sandboxes with a stored
// password.
func (s *FileStore) Keys() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	secrets, err := s.load()
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(secrets))
	for k := range secrets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, nil
}

// load reads the file, treating absence as empty. It refuses a file whose
// permissions are wider than 0600 rather than reading secrets from a
// world-readable file.
func (s *FileStore) load() (map[string]string, error) {
	info, err := os.Stat(s.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return map[string]string{}, nil
	case err != nil:
		return nil, fmt.Errorf("credentials: stat %s: %w", s.path, err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("credentials: %s has permissions %04o; it must not be readable by others (chmod 600 %s)",
			s.path, perm, s.path)
	}

	data, err := os.ReadFile(s.path)
	if err != nil {
		return nil, fmt.Errorf("credentials: read %s: %w", s.path, err)
	}
	if len(data) == 0 {
		return map[string]string{}, nil
	}
	var secrets map[string]string
	if err := json.Unmarshal(data, &secrets); err != nil {
		return nil, fmt.Errorf("credentials: parse %s: %w", s.path, err)
	}
	if secrets == nil {
		secrets = map[string]string{}
	}
	return secrets, nil
}

// save writes the file atomically, so a crash mid-write cannot leave a
// truncated file that loses every stored secret.
func (s *FileStore) save(secrets map[string]string) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, DirMode); err != nil {
		return fmt.Errorf("credentials: create %s: %w", dir, err)
	}
	// MkdirAll leaves an existing directory's permissions alone, and the
	// config directory may predate spov. The file itself is 0600, but a
	// group- or world-writable directory lets someone replace it.
	if info, err := os.Stat(dir); err == nil && info.Mode().Perm()&0o022 != 0 {
		if err := os.Chmod(dir, DirMode); err != nil {
			return fmt.Errorf("credentials: tighten permissions on %s: %w", dir, err)
		}
	}
	data, err := json.MarshalIndent(secrets, "", "  ")
	if err != nil {
		return fmt.Errorf("credentials: encode: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".credentials-*.tmp")
	if err != nil {
		return fmt.Errorf("credentials: create temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	// Chmod before writing: the file must never exist with secrets in it
	// and default permissions, however briefly.
	if err := tmp.Chmod(FileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("credentials: set permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("credentials: write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("credentials: close: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("credentials: replace %s: %w", s.path, err)
	}
	return nil
}

// warnOnce emits the storage warning the first time a secret is written.
// The caller must hold the lock.
func (s *FileStore) warnOnce() {
	if s.warned || s.Warn == nil {
		return
	}
	s.warned = true
	s.Warn(fmt.Sprintf(FileWarning, s.path))
}
