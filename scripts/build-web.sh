#!/usr/bin/env bash
# Build the web app and put it where the Go binary embeds it from.
#
# `go build` alone produces a working server with a placeholder page, so
# nothing in the Go toolchain depends on Bun. This script is what turns
# that placeholder into the real dashboard, and CI, the release build and
# the container image all run it first.
#
# @decision(DL-055)
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
src="$root/web"
dest="$root/internal/webui/dist"

if ! command -v bun >/dev/null 2>&1; then
  echo "bun is not installed; run scripts/install-bun.sh" >&2
  exit 1
fi

cd "$src"
bun install --frozen-lockfile
bun run build

# The marker keeps the go:embed directory present in a fresh checkout;
# everything beside it is build output and is replaced wholesale.
find "$dest" -mindepth 1 ! -name .gitkeep -delete
cp -R "$src/dist/." "$dest/"

test -f "$dest/index.html" || { echo "the build produced no index.html" >&2; exit 1; }
echo "web app built into internal/webui/dist"
