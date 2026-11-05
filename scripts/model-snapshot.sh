#!/usr/bin/env bash
# Refresh the model list spov ships in its binary from models.dev's
# mirror (DL-013). Run before a release; review the diff.
set -euo pipefail
cd "$(dirname "$0")/.."
go run ./internal/modelcat/snapshotgen "${1:-https://models.opencode.ai/api.json}" internal/modelcat/snapshot.json
