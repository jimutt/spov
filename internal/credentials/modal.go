package credentials

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ModalCredentials identifies a Modal workspace.
type ModalCredentials struct {
	TokenID     string
	TokenSecret string
	// Source says where the credentials came from, for diagnostics. It
	// never contains a secret.
	Source string
}

// ModalTokenFile is Modal's own credentials file.
const ModalTokenFile = ".modal.toml"

// ResolveModal finds Modal credentials without asking the user to log in
// twice: the environment first, then Modal's own token file. spov does not
// parse the file itself — the Modal SDK reads it — so this only reports
// whether it exists.
//
// @decision(DL-008)
func ResolveModal(getenv func(string) string, homeDir func() (string, error)) (ModalCredentials, error) {
	id, secret := getenv("MODAL_TOKEN_ID"), getenv("MODAL_TOKEN_SECRET")
	switch {
	case id != "" && secret != "":
		return ModalCredentials{TokenID: id, TokenSecret: secret, Source: "MODAL_TOKEN_ID/MODAL_TOKEN_SECRET"}, nil
	case id != "" || secret != "":
		return ModalCredentials{}, fmt.Errorf("credentials: MODAL_TOKEN_ID and MODAL_TOKEN_SECRET must be set together")
	}

	home, err := homeDir()
	if err != nil {
		return ModalCredentials{}, fmt.Errorf("credentials: no Modal credentials in the environment and no home directory to look in: %w", err)
	}
	path := filepath.Join(home, ModalTokenFile)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ModalCredentials{}, fmt.Errorf("credentials: no Modal credentials found; run `modal setup` or set MODAL_TOKEN_ID and MODAL_TOKEN_SECRET")
		}
		return ModalCredentials{}, fmt.Errorf("credentials: check %s: %w", path, err)
	}
	// The SDK reads the file itself; spov only confirms it is there.
	return ModalCredentials{Source: path}, nil
}

// HasTokens reports whether the credentials carry an explicit token pair
// rather than deferring to Modal's own file.
func (c ModalCredentials) HasTokens() bool {
	return c.TokenID != "" && c.TokenSecret != ""
}
