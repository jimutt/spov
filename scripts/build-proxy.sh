#!/usr/bin/env bash
# Build the sandbox sidecar's proxy and put it where the Go binary embeds
# it from.
#
# `go build` alone produces a server without it, which launches sandboxes
# with their tokens in the main container and says so. This script is
# what the release build runs first.
#
# @decision(DL-034)
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
dest="$root/internal/proxybin/dist"

cd "$root"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' \
  -o "$dest/spov-proxy" ./cmd/spov-proxy
echo "spov-proxy built into internal/proxybin/dist"
