// Package proxybin carries the spov-proxy binary the server bakes into
// the sidecar's image.
//
// @decision(DL-034)
package proxybin

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
)

// dist holds the linux/amd64 proxy binary. A plain `go build` embeds only
// the placeholder marker; scripts/build-proxy.sh builds the real one in.
//
// @decision(DL-034)
//
//go:embed all:dist
var dist embed.FS

// File is the binary's name in dist.
const File = "spov-proxy"

// Binary is the embedded proxy, or nil in a build without one.
//
// @decision(DL-034)
func Binary() []byte {
	return binaryFrom(dist, "dist/"+File)
}

func binaryFrom(fsys fs.FS, name string) []byte {
	b, err := fs.ReadFile(fsys, name)
	if err != nil || len(b) == 0 {
		return nil
	}
	return b
}

// Hash identifies a proxy binary, and so the image it is baked into.
func Hash(bin []byte) string {
	sum := sha256.Sum256(bin)
	return hex.EncodeToString(sum[:])
}
