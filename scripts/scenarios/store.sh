#!/usr/bin/env bash
# Store a scenario credential through the API from its file, so the
# secret is never typed, printed or passed in argv:
#
#   scripts/scenarios/store.sh app ~/.config/spov-test/app-key-1.pem   as admin
#   scripts/scenarios/store.sh pat ~/.config/spov-test/member-pat.txt  as member
#
# app stores the organisation's App (SCENARIO_APP_ID from github-app.env)
# for the github platform, which the server checks before storing; pat
# stores member's own GitHub token. Prints the status and the server's
# view, which holds no secret.
#
# @decision(DL-022)
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
secrets=${SPOV_SCENARIO_SECRETS:-$HOME/.config/spov-test}
state=${SPOV_SCENARIO_STATE:-$HOME/.local/state/spov-scenarios}
what=${1:?usage: store.sh app <key.pem> | pat <token file>}
file=${2:?usage: store.sh app <key.pem> | pat <token file>}
[ -r "$file" ] || { echo "store.sh: cannot read $file" >&2; exit 2; }

mkdir -p "$state" && chmod 700 "$state"
body=$(mktemp "$state/body.XXXXXX")
trap 'rm -f "$body"' EXIT
case "$what" in
  app)
    app_id=$(sed -n 's/^SCENARIO_APP_ID=//p' "$secrets/github-app.env" | tr -d "\"'")
    [ -n "$app_id" ] || { echo "store.sh: SCENARIO_APP_ID is not set in $secrets/github-app.env" >&2; exit 1; }
    python3 -I -c 'import json,sys; json.dump({"app_id": sys.argv[1], "private_key": open(sys.argv[2]).read()}, open(sys.argv[3], "w"))' \
      "$app_id" "$file" "$body"
    "$here/as.sh" admin api PUT /api/v1/org/github/github "$body"
    ;;
  pat)
    python3 -I -c 'import json,sys; json.dump({"token": open(sys.argv[1]).read().strip()}, open(sys.argv[2], "w"))' "$file" "$body"
    "$here/as.sh" member api PUT /api/v1/me/credentials/github "$body"
    ;;
  *) echo "store.sh: app or pat" >&2; exit 2 ;;
esac
