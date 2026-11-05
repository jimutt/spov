#!/usr/bin/env bash
# Refresh internal/opencode/contract from an OpenCode binary: the OpenAPI
# document its server publishes, and the version it reports.
#
#   scripts/opencode-contract.sh <path-to-opencode-binary>
#
# The binary runs isolated: a throwaway HOME and XDG directories, bound
# to loopback, a random password and no provider keys, so nothing of the
# caller's own OpenCode state is read or written. @decision(DL-035)
set -euo pipefail

bin=${1:?usage: $0 <path-to-opencode-binary>}
# It runs from another directory with a bare PATH.
bin=$(realpath "$(command -v "$bin")")
here=$(cd "$(dirname "$0")/.." && pwd)
out=$here/internal/opencode/contract

tmp=$(mktemp -d)
pid=
cleanup() {
	# The server leads its own process group, so whatever it started stops too.
	if [[ -n $pid ]]; then kill -- "-$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; fi
	rm -rf "$tmp"
}
trap cleanup EXIT

mkdir -p "$tmp"/{home,config,data,state,cache,work}
password=$(head -c 18 /dev/urandom | base64 | tr -d '/+=')
port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')
isolated=(env -i PATH=/usr/bin:/bin HOME="$tmp/home"
	XDG_CONFIG_HOME="$tmp/config" XDG_DATA_HOME="$tmp/data"
	XDG_STATE_HOME="$tmp/state" XDG_CACHE_HOME="$tmp/cache"
	OPENCODE_PASSWORD="$password" OPENCODE_DISABLE_AUTOUPDATE=1)

version=$(cd "$tmp/work" && "${isolated[@]}" "$bin" --version | awk 'NF { v = $NF } END { sub(/^v/, "", v); print v }')
[[ -n $version ]] || { echo "$bin --version printed nothing" >&2; exit 1; }

(cd "$tmp/work" && exec setsid "${isolated[@]}" "$bin" serve --hostname 127.0.0.1 --port "$port") >"$tmp/server.log" 2>&1 &
pid=$!

for _ in $(seq 1 100); do
	# The credential goes on stdin, never in argv.
	if printf 'user = "opencode:%s"\n' "$password" | curl -fsS -K - "http://127.0.0.1:$port/openapi.json" -o "$tmp/openapi.json" 2>/dev/null; then
		break
	fi
	sleep 0.2
done
[[ -s $tmp/openapi.json ]] || { echo "the server never published /openapi.json:" >&2; cat "$tmp/server.log" >&2; exit 1; }

# Sorted and indented, so a bump's diff is reviewable line by line.
python3 - "$tmp/openapi.json" "$out/openapi.json" <<'PY'
import json, sys
with open(sys.argv[1]) as src:
    doc = json.load(src)
with open(sys.argv[2], "w") as dst:
    json.dump(doc, dst, indent=1, sort_keys=True)
    dst.write("\n")
PY
printf '%s\n' "$version" >"$out/version"
echo "contract: OpenCode $version, $(python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); print(sum(len(v) for v in d["paths"].values()))' "$out/openapi.json") operations"
