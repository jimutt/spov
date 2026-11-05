#!/usr/bin/env bash
# Fail if statement coverage falls below a threshold.
#
# Files listed in scripts/coverage-exclude.txt are dropped from the
# calculation: they are thin adapters over an external SDK whose behaviour
# is covered by integration tests behind the `integration` build tag, which
# the default `go test ./...` run excludes.
set -euo pipefail

cd "$(dirname "$0")/.."

profile="${1:-coverage.out}"
minimum="${2:-90}"
exclude_file="${3:-scripts/coverage-exclude.txt}"

if [ ! -f "$profile" ]; then
  echo "coverage profile $profile not found" >&2
  exit 1
fi

# Build a grep pattern from the exclusion list, ignoring blanks and comments.
excluded=""
if [ -f "$exclude_file" ]; then
  excluded=$(grep -vE '^\s*(#|$)' "$exclude_file" || true)
fi

filtered=$(mktemp)
trap 'rm -f "$filtered"' EXIT

if [ -n "$excluded" ]; then
  # The profile's first line is the mode header and must be kept.
  head -n 1 "$profile" > "$filtered"
  body=$(tail -n +2 "$profile")

  # Profile lines look like "<import-path>/<file>:<line>.<col>,...".
  # Anchor each pattern on the path separator and the trailing colon so
  # that "sandbox.go" cannot also match "xsandbox.go".
  #
  # Each entry is filtered in its own pass, into a temporary file rather
  # than a shell variable: a profile is tens of thousands of lines, and
  # round-tripping it through $(...) for every entry is both slow and
  # easy to get subtly wrong.
  work=$(mktemp)
  next=$(mktemp)
  trap 'rm -f "$filtered" "$work" "$next"' EXIT
  printf '%s\n' "$body" > "$work"

  while IFS= read -r entry; do
    [ -n "$entry" ] || continue
    if ! grep -qF "/${entry}:" "$work"; then
      echo "warning: coverage exclusion '${entry}' matched no lines; is it stale?" >&2
      continue
    fi
    grep -vF "/${entry}:" "$work" > "$next" || true
    mv "$next" "$work"
    next=$(mktemp)
  done <<< "$excluded"

  cat "$work" >> "$filtered"

else
  cp "$profile" "$filtered"
fi

total=$(LC_ALL=C go tool cover -func="$filtered" | LC_ALL=C awk '/^total:/ {gsub(/%/, "", $NF); print $NF}')

if [ -z "$total" ]; then
  echo "could not read total coverage from $profile" >&2
  exit 1
fi

if [ -n "$excluded" ]; then
  echo "excluded from coverage (integration-tested only):"
  echo "$excluded" | sed 's/^/  /'
fi
echo "coverage: ${total}% (minimum ${minimum}%)"

LC_ALL=C awk -v total="$total" -v min="$minimum" 'BEGIN { exit !(total + 0 >= min + 0) }' || {
  echo "coverage ${total}% is below the ${minimum}% minimum" >&2
  exit 1
}
