package credentials

import (
	"fmt"
	"io"
	"path/filepath"
)

// Default returns the store spov uses: the OS keyring, falling back to a
// 0600 file when the keyring is unavailable. warn receives the one-time
// notice that secrets are being written unencrypted.
//
// @decision(DL-008)
func Default(configDir string, warn io.Writer) Store {
	file := NewFileStore(DefaultFilePath(configDir))
	if warn != nil {
		file.Warn = func(msg string) { _, _ = fmt.Fprint(warn, msg) }
	}
	return NewFallbackStore(NewKeyringStore(), file)
}

// ConfigDir is the directory holding spov's user-level files.
func ConfigDir(userConfigPath string) string {
	return filepath.Dir(userConfigPath)
}
