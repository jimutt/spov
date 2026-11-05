#!/usr/bin/env bash
# Run the scenario build's CLI as one of the scenario's users, each with
# its own config, state and credential file, pointed at the server
# hosted-local.sh runs:
#
#   scripts/scenarios/as.sh admin whoami
#   scripts/scenarios/as.sh member fly --no-sync --repo github:<org>/<repo>
#   scripts/scenarios/as.sh member --prefer shared fly --no-sync --repo github:<org>/<repo>
#   scripts/scenarios/as.sh member api GET /api/v1/org/github
#
# `hosted-local.sh seed` signs the users in. api sends a user's CLI
# token to curl in a 0600 header file, never in argv. --prefer writes
# platforms[].prefer (DL-042), --git-access-preference sandbox.git_access
# and job_git_access (DL-043). The OS keyring is off (D-Bus pointed
# nowhere), so the users' tokens land in their own 0600 files.
#
# @decision(DL-022)
set -euo pipefail

state=${SPOV_SCENARIO_STATE:-$HOME/.local/state/spov-scenarios}
secrets=${SPOV_SCENARIO_SECRETS:-$HOME/.config/spov-test}
port=${SPOV_SCENARIO_PORT:-8091}
who=${1:?usage: as.sh admin|member [--prefer personal|shared] [--git-access-preference repos|all] <spov args> | api METHOD PATH [body-file]}
shift
case "$who" in admin|member) ;; *) echo "as.sh: the user is admin or member" >&2; exit 2 ;; esac
prefer=""
git_access=""
while :; do
  case "${1:-}" in
    --prefer) prefer=${2:?--prefer needs personal or shared}; shift 2 ;;
    --git-access-preference) git_access=${2:?--git-access-preference needs repos or all}; shift 2 ;;
    *) break ;;
  esac
done

org=$(sed -n 's/^SCENARIO_GITHUB_ORG=//p' "$secrets/github-app.env" | tr -d "\"'")
[ -n "$org" ] || { echo "as.sh: SCENARIO_GITHUB_ORG is not set in $secrets/github-app.env" >&2; exit 1; }
home="$state/users/$who"
mkdir -p "$home/config/spov" "$home/state"
chmod 700 "$home"
{
  echo "server:"
  echo "  url: http://localhost:$port"
  echo "platforms:"
  echo "  - name: github"
  echo "    kind: github"
  echo "    account: $org"
  if [ -n "$prefer" ]; then echo "    prefer: $prefer"; fi
  if [ -n "$git_access" ]; then printf 'sandbox:\n  git_access: %s\n  job_git_access: %s\n' "$git_access" "$git_access"; fi
} >"$home/config/spov/config.yaml"

if [ "${1:-}" = api ]; then
  method=${2:?api needs a method}
  path=${3:?api needs a path}
  creds="$home/config/spov/credentials.json"
  [ -f "$creds" ] || { echo "as.sh: no CLI token for $who; run: hosted-local.sh seed" >&2; exit 1; }
  hdr=$(mktemp "$home/hdr.XXXXXX")
  trap 'rm -f "$hdr"' EXIT
  python3 -I -c 'import json,sys
key = "server:http_localhost_" + sys.argv[2]
tok = json.load(open(sys.argv[1])).get(key)
if not tok:
    sys.exit("as.sh: no token for this server in the credential file; log in again")
print("Authorization: Bearer " + tok)' "$creds" "$port" >"$hdr"
  args=(-sS -X "$method" -H "@$hdr" -w '\nHTTP %{http_code}\n')
  if [ -n "${4:-}" ]; then args+=(-H 'Content-Type: application/json' --data-binary "@$4"); fi
  curl "${args[@]}" "http://localhost:$port$path"
  exit
fi

export XDG_CONFIG_HOME="$home/config" XDG_STATE_HOME="$home/state"
export DBUS_SESSION_BUS_ADDRESS=unix:path=/nonexistent
unset SPOV_SERVER SPOV_SERVER_TOKEN
exec "$state/bin/spov" "$@"
