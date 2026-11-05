#!/usr/bin/env bash
# Regenerate the committed OpenAPI document and the TypeScript types.
#
# Both artifacts are committed so that an API change is reviewable in one
# diff, and CI runs this script and then `git diff --exit-code`.
#
# @decision(DL-005)
set -euo pipefail

cd "$(dirname "$0")/.."

spec=docs/api/openapi.yaml
types=web/src/lib/api/types.ts

for tool in go bun; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "$tool is not installed, and the client cannot be generated without it." >&2
    if [ "$tool" = bun ]; then
      echo "  bash scripts/install-bun.sh   # prints the directory to add to PATH" >&2
    fi
    exit 1
  fi
done

# -buildvcs=false keeps the binary from stamping itself. It is belt and
# braces: info.version is a fixed "v1" (server.APIVersion) precisely so
# that the document does not carry build identity, and this flag means a
# future stamp cannot creep back in through debug.ReadBuildInfo either.
echo "==> $spec"
mkdir -p "$(dirname "$spec")"
# Via a temporary file: a redirect truncates the committed document
# before the build has even started, so a compile error would leave an
# empty spec in the working tree and exit before the check below.
# Beside the destination, not in $TMPDIR: a cross-filesystem mv would
# also carry mktemp's 0600 across, leaving the committed document
# readable only by whoever last generated it.
tmp="$spec.tmp.$$"
trap 'rm -f "$tmp"' EXIT
go run -buildvcs=false ./cmd/spov serve openapi > "$tmp"
test -s "$tmp"
mv "$tmp" "$spec"

echo "==> $types"
cd web
bun install --frozen-lockfile
mkdir -p "$(dirname "../$types")"
# `bun run`, not `bunx`: the script resolves the pinned devDependency,
# where bunx would happily fetch whatever is latest.
bun run gen:types

cd ..
for f in "$spec" "$types"; do
  test -s "$f" || { echo "$f is empty" >&2; exit 1; }
done
echo "==> generated $spec and $types"
