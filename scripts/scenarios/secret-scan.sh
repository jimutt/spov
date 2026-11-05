#!/usr/bin/env bash
# Look for any of the scenario's secrets in files or directories (the
# server log, saved API responses, CLI output, page snapshots) and say
# which secret appeared where, never the secret itself:
#
#   scripts/scenarios/secret-scan.sh ~/.local/state/spov-scenarios/server.log evidence/
#
# The secrets are the env files' values as sourcing gives them (not
# SCENARIO_* addresses and names, nor the OAuth client ID), each private
# key's body lines, *.txt tokens, the generated server keys, each user's
# CLI token and the Modal token secret; then anything shaped like a
# private key, GitHub, GitLab or provider token, which catches a minted
# token no file holds. Exits 1 on a find, 2 on an unreadable path.
#
# @decision(DL-022)
set -euo pipefail

secrets=${SPOV_SCENARIO_SECRETS:-$HOME/.config/spov-test}
state=${SPOV_SCENARIO_STATE:-$HOME/.local/state/spov-scenarios}
[ $# -gt 0 ] || { echo "usage: secret-scan.sh <file or directory>..." >&2; exit 2; }
for a in "$@"; do
  [ -r "$a" ] || { echo "secret-scan: cannot read $a" >&2; exit 2; }
done

work=$(mktemp -d)
chmod 700 "$work"
trap 'rm -rf "$work"' EXIT
umask 077

# add writes one secret's patterns to its own file, named for the label
# shown when it is found.
add() {
  local label=$1 value=$2
  [ "${#value}" -ge 12 ] || return 0
  printf '%s\n' "$value" >>"$work/fixed.$label"
}

# The env files' values, as sourcing gives them, so quotes and trailing
# comments are what bash makes of them.
for f in "$secrets"/*.env; do
  [ -f "$f" ] || continue
  while IFS= read -r -d '' pair; do
    name=${pair%%=*}
    case "$name" in SCENARIO_*|SPOV_GITHUB_OAUTH_CLIENT_ID|PWD|OLDPWD|SHLVL|_) continue ;; esac
    add "$(basename "$f"):$name" "${pair#*=}"
  done < <(env -i bash -c 'set -a; . "$1"; env -0' _ "$f")
done
for f in "$secrets"/*.pem; do
  [ -f "$f" ] || continue
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in -----*) continue ;; esac
    add "$(basename "$f")" "$line"
  done <"$f"
done
for f in "$secrets"/*.txt "$state"/keys/*; do
  [ -f "$f" ] || continue
  add "$(basename "$f")" "$(tr -d '\n' <"$f")"
done
for f in "$state"/users/*/config/spov/credentials.json; do
  [ -f "$f" ] || continue
  who=$(basename "$(dirname "$(dirname "$(dirname "$f")")")")
  while IFS= read -r -d '' v; do add "cli-$who" "$v"; done < <(
    python3 -I -c 'import json,sys
for v in json.load(open(sys.argv[1])).values():
    sys.stdout.write(v + "\0")' "$f")
done
if [ -f "$HOME/.modal.toml" ]; then
  while IFS= read -r v; do add modal.toml:token_secret "$v"; done < <(
    sed -n 's/^[[:space:]]*token_secret[[:space:]]*=[[:space:]]*"\(.*\)".*/\1/p' "$HOME/.modal.toml")
fi

# hit reports where one pattern file matched; grep's 2 is an error.
found=0
hit() {
  local label=$1
  shift
  local out rc=0
  out=$(grep -r -l "$@" -- "${targets[@]}") || rc=$?
  [ "$rc" -le 1 ] || { echo "secret-scan: grep failed ($rc)" >&2; exit 2; }
  if [ -n "$out" ]; then
    found=1
    while IFS= read -r h; do echo "FOUND $label in $h"; done <<<"$out"
  fi
}
targets=("$@")
for p in "$work"/fixed.*; do
  [ -f "$p" ] || continue
  hit "${p#"$work"/fixed.}" -F -f "$p"
done
# shaped reports where a pattern matched anything but the placeholders
# the proxy gives a sandbox (<prefix>spovplaceholder<hex>).
shaped() {
  local label=$1
  shift
  local out rc=0
  out=$(grep -r -H -o -E "$@" -- "${targets[@]}") || rc=$?
  [ "$rc" -le 1 ] || { echo "secret-scan: grep failed ($rc)" >&2; exit 2; }
  out=$(grep -v 'spovplaceholder' <<<"$out" | sed 's/:.*//' | sort -u || true)
  if [ -n "$out" ]; then
    found=1
    while IFS= read -r h; do echo "FOUND $label in $h"; done <<<"$out"
  fi
}
shaped "a private key" -e '-----BEGIN [A-Z ]*PRIVATE KEY-----'
shaped "a GitHub token" -e 'gh[pousr]_[A-Za-z0-9]{36,}' -e 'github_pat_[A-Za-z0-9_]{20,}'
shaped "a GitLab token" -e 'glpat-[A-Za-z0-9_-]{20,}'
shaped "a provider key" -e 'sk-ant-[A-Za-z0-9_-]{20,}' -e 'sk-or-[A-Za-z0-9_-]{20,}' -e 'sk-proj-[A-Za-z0-9_-]{20,}'

if [ "$found" = 0 ]; then
  echo "secret-scan: no secret found in $*"
fi
exit "$found"
