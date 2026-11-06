#!/usr/bin/env bash
# Run the same checks as CI, locally.
set -euo pipefail

cd "$(dirname "$0")/.."

echo "==> gofmt"
unformatted=$(gofmt -l .)
if [ -n "$unformatted" ]; then
  echo "These files need gofmt:"
  echo "$unformatted"
  exit 1
fi

echo "==> go vet"
go vet ./...

# A long comment block usually restates a decision record.
echo "==> comment blocks"
python3 scripts/comment-blocks.py

# The integration-tagged files are excluded from every other step here
# and from CI's test run, so nothing else compiles them: an interface
# they implement can gain a method and they keep building locally while
# CI's own typecheck goes red. Measured — `git.Platform` gained
# FindPullRequest and a hand-rolled fake in internal/session went stale
# for fourteen commits.
#
# -run '^$' matches no test, so this compiles them and runs nothing:
# these tests create billable sandboxes and must never run here.
echo "==> typecheck the integration build"
go test -tags integration -run '^$' ./... >/dev/null

# The OpenAPI document and the TypeScript client are committed, so a
# change to the API's Go types has to be regenerated in the same commit.
# CI runs the same two steps. Early, because it is fast and the same on
# every platform, where the coverage gate below is neither — and it
# rewrites two tracked files, so a developer sees why the tree is dirty
# before the slow steps have run.
#
# @decision(DL-005)
echo "==> api client drift (regenerates docs/api/openapi.yaml and web/src/lib/api/types.ts)"
if command -v bun >/dev/null 2>&1; then
  bash "$(dirname "$0")/gen-api-client.sh" >/dev/null
  if ! git diff --quiet -- docs/api/openapi.yaml web/src/lib/api/types.ts; then
    echo "The API changed but the generated client did not. Commit these:" >&2
    git --no-pager diff --stat -- docs/api/openapi.yaml web/src/lib/api/types.ts >&2
    exit 1
  fi
elif [ "${SKIP_WEB:-}" = "1" ]; then
  echo "bun not installed; skipping the client drift check because SKIP_WEB=1"
else
  echo "bun not installed, so the generated client cannot be checked." >&2
  echo "Install it, or set SKIP_WEB=1 to run the rest of the checks:" >&2
  echo "  bash scripts/install-bun.sh" >&2
  exit 1
fi

# The web app's own gate, and the reason it is three steps rather than
# one: `astro check` type-checks but does not compile Svelte, and
# vitest never loads a component — so a template error (two imports
# claiming the same name, say) passes both and only fails when the
# compiler runs. That gap has cost a red CI build; the build is the
# step that closes it.
#
# CI runs the same three (.github/workflows/ci.yml, the `web` job).
#
# @decision(DL-055)
echo "==> web (astro check, vitest, build)"
if command -v bun >/dev/null 2>&1; then
  (cd web && bunx astro check && bun run test && bun run build >/dev/null)
elif [ "${SKIP_WEB:-}" = "1" ]; then
  echo "bun not installed; skipping the web checks because SKIP_WEB=1"
else
  echo "bun not installed, so the web app cannot be checked." >&2
  echo "Install it, or set SKIP_WEB=1 to run the rest of the checks:" >&2
  echo "  bash scripts/install-bun.sh" >&2
  exit 1
fi

echo "==> golangci-lint"
if command -v golangci-lint >/dev/null 2>&1; then
  golangci-lint run ./...
elif [ "${SKIP_LINT:-}" = "1" ]; then
  echo "golangci-lint not installed; skipping because SKIP_LINT=1"
else
  echo "golangci-lint not installed." >&2
  echo "Install it, or set SKIP_LINT=1 to run the rest of the checks:" >&2
  echo "  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0" >&2
  exit 1
fi

echo "==> go test -race"
go test -race -covermode=atomic -coverprofile=coverage.out ./...

echo "==> coverage gate"
bash "$(dirname "$0")/coverage-gate.sh" coverage.out "${COVERAGE_MIN:-87}"

# The OpenAPI document and the TypeScript client are committed, so a
# change to the API's Go types has to be regenerated in the same commit.
# CI runs the same two steps.
#
# @decision(DL-005)
echo "==> api client drift"
if command -v bun >/dev/null 2>&1; then
  bash "$(dirname "$0")/gen-api-client.sh" >/dev/null
  if ! git diff --quiet -- docs/api/openapi.yaml web/src/lib/api/types.ts; then
    echo "The API changed but the generated client did not. Commit these:" >&2
    git --no-pager diff --stat -- docs/api/openapi.yaml web/src/lib/api/types.ts >&2
    exit 1
  fi
elif [ "${SKIP_WEB:-}" = "1" ]; then
  echo "bun not installed; skipping because SKIP_WEB=1"
else
  echo "bun not installed, so the generated client cannot be checked." >&2
  echo "Install it, or set SKIP_WEB=1 to run the rest of the checks:" >&2
  echo "  bash scripts/install-bun.sh" >&2
  exit 1
fi
