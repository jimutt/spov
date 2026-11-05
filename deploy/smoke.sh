#!/usr/bin/env bash
# Smoke test for a hosted spov deployment: the pieces that only a real
# container and a real database can prove.
#
#   ./deploy/smoke.sh                      # bring compose up, test, tear down
#   ./deploy/smoke.sh --keep               # leave it running afterwards
#   ./deploy/smoke.sh --url URL            # test something already running,
#                                          # e.g. a Kamal or Kubernetes deploy
#   ./deploy/smoke.sh --repo owner/name    # also prove state survives a
#                                          # restart, by launching a session
#
# --repo creates a billable sandbox and terminates it before exiting, even
# if the script fails on the way; without it the restart check is skipped.
#
# When SPOV_GITHUB_OAUTH_CLIENT_ID and _SECRET are set the browser login
# and the CLI device flow are checked too; without them both are skipped.
#
# @decision(DL-010) @decision(DL-007) @decision(DL-006)
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$here/.." && pwd)
compose_file="$here/compose.yaml"

url=""
repo_ref=""
keep=0
own_compose=0
launched_session=""

while [ $# -gt 0 ]; do
  case "$1" in
    --url) url="${2:?--url needs a value}"; shift 2 ;;
    --repo) repo_ref="${2:?--repo needs owner/name}"; shift 2 ;;
    --keep) keep=1; shift ;;
    # The header block, minus the shebang and the trailing @decision
    # line: taken up to the first line that is not a comment, so
    # editing the header cannot truncate the help.
    -h|--help) sed -n '2,/^[^#]/p' "${BASH_SOURCE[0]}" \
      | sed '/^# @decision/,$d' | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

pass() { printf '  ok    %s\n' "$1"; }
fail() { printf '  FAIL  %s\n' "$1" >&2; exit 1; }
step() { printf '\n== %s\n' "$1"; }

cleanup() {
  local code=$?
  if [ -n "$launched_session" ] && [ -n "$url" ]; then
    # A sandbox costs money for as long as it runs, so it goes even if
    # the script failed on the way.
    curl -fsS -X DELETE -H "Authorization: Bearer $SPOV_SERVER_TOKEN" \
      "$url/api/v1/sessions/$launched_session" >/dev/null 2>&1 || true
    echo "terminated session $launched_session"
  fi
  if [ "$own_compose" = 1 ] && [ "$keep" = 0 ]; then
    docker compose -f "$compose_file" down -v >/dev/null 2>&1 || true
    echo "compose stack removed"
  elif [ "$own_compose" = 1 ]; then
    echo "compose left running: docker compose -f $compose_file down -v"
  fi
  exit $code
}
trap cleanup EXIT

# curl's status code for a request, without failing the script.
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

# ---------------------------------------------------------------- setup

if [ -z "$url" ]; then
  command -v docker >/dev/null || fail "docker is not installed; use --url to test a running server"
  # No command substitution in the message: bash parses the closing
  # paren as ending the parameter expansion.
  : "${SPOV_SERVER_TOKEN:?is not set. Generate one with: openssl rand -hex 32}"
  : "${MODAL_TOKEN_ID:?set MODAL_TOKEN_ID}"
  : "${MODAL_TOKEN_SECRET:?set MODAL_TOKEN_SECRET}"
  export MODAL_ENVIRONMENT="${MODAL_ENVIRONMENT:-spov-hosted}"

  step "Building and starting the stack"
  ( cd "$repo" && docker compose -f "$compose_file" up --build -d )
  own_compose=1
  url="http://localhost:8080"
else
  : "${SPOV_SERVER_TOKEN:?is not set; use the token the server was given}"
fi

auth=(-H "Authorization: Bearer $SPOV_SERVER_TOKEN")

# ---------------------------------------------------------------- health

step "Health"

deadline=$(( $(date +%s) + 180 ))
until [ "$(code "$url/livez")" = "204" ]; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "livez never answered; check the container logs"
  sleep 2
done
pass "livez answers (the process is up)"

# Readiness is the interesting one: it means the pool connected *and*
# the migrations reached head.
until [ "$(code "$url/readyz")" = "204" ]; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "readyz never became ready; migrations may not have run"
  sleep 2
done
pass "readyz answers (its dependencies are reachable)"

[ "$(code "$url/startupz")" = "204" ] || fail "startupz did not answer"
pass "startupz answers"

for path in livez readyz startupz; do
  [ "$(code "$url/$path")" = "204" ] || fail "$path needs a credential; a load balancer has none"
done
pass "health needs no credential"

# ---------------------------------------------------------- authentication

step "Authentication and the browser guard"

[ "$(code "$url/api/v1/sessions")" = "401" ] \
  || fail "the API answered without a credential"
pass "the API refuses an unauthenticated request"

[ "$(code "${auth[@]}" "$url/api/v1/sessions")" = "200" ] \
  || fail "the API refused a valid token"
pass "the API accepts the token"

# The guard runs before authentication, so a foreign Host is 403 rather
# than 401. This is what breaks when SPOV_PUBLIC_URL is wrong.
[ "$(code "${auth[@]}" -H "Host: attacker.example.com" "$url/api/v1/sessions")" = "403" ] \
  || fail "a request for another host was not refused"
pass "the browser guard refuses a foreign Host"

# ---------------------------------------------------------------- login

step "Browser login"

# The route is served only when an OAuth app is configured; without one
# the path falls through to the authenticated API and answers 401, and
# the server runs on its bearer token alone. That is a valid deployment,
# so it is a skip rather than a failure.
login_code=$(code "$url/login")
if [ "$login_code" = "401" ]; then
  echo "  skip  no OAuth app configured (SPOV_GITHUB_OAUTH_CLIENT_ID unset)"
  echo "        set it, plus an allowlist, to exercise the login flow"
else
  [ "$login_code" = "303" ] || fail "/login answered $login_code, want a redirect to GitHub"
  pass "/login redirects"

  # The redirect is where a misconfigured public URL shows up: the
  # callback GitHub is told to use must match what it has registered.
  location=$(curl -s -o /dev/null -w '%{redirect_url}' "$url/login")
  case "$location" in
    https://github.com/login/oauth/authorize*) ;;
    *) fail "/login redirects to $location, not GitHub" ;;
  esac
  case "$location" in
    *code_challenge_method=S256*) pass "the redirect carries a PKCE challenge" ;;
    *) fail "the redirect has no PKCE challenge" ;;
  esac
  case "$location" in
    *state=*) pass "the redirect carries a state nonce" ;;
    *) fail "the redirect has no state" ;;
  esac
  # The callback must be on the public origin, or GitHub returns the
  # user to a host that cannot complete the login.
  redirect_uri=$(printf '%s' "$location" | sed -n 's/.*redirect_uri=\([^&]*\).*/\1/p')
  expected=$(printf '%s' "$url/oauth/github/callback" | sed 's|:|%3A|g; s|/|%2F|g')
  [ "$redirect_uri" = "$expected" ] \
    || fail "the callback is $redirect_uri, want $expected; check SPOV_PUBLIC_URL"
  pass "the callback is on the public origin"

  # A callback with no flow in progress must not be a 500.
  [ "$(code "$url/oauth/github/callback?code=x&state=y")" = "400" ] \
    || fail "a callback with no login in progress was not refused"
  pass "a stray callback is refused"

  # The device flow is spov's own authorization server (DL-006); its
  # error vocabulary is what x/oauth2's client branches on, so a wrong
  # code here means every CLI login hangs or fails opaquely.
  dc=$(curl -fsS -X POST "$url/device/code" -d "client_id=spov-cli&name=smoke" 2>/dev/null) \
    || fail "the device-code endpoint did not answer"
  device_code=$(printf '%s' "$dc" | sed -n 's/.*"device_code":"\([^"]*\)".*/\1/p')
  user_code=$(printf '%s' "$dc" | sed -n 's/.*"user_code":"\([^"]*\)".*/\1/p')
  [ -n "$device_code" ] && [ -n "$user_code" ] || fail "the device-code response had no codes: $dc"
  pass "the device flow issues a code pair"

  poll() {
    curl -s -X POST "$url/device/token" \
      -d "grant_type=urn:ietf:params:oauth:grant-type:device_code&device_code=$1&client_id=spov-cli"
  }
  case "$(poll "$device_code")" in
    *authorization_pending*) pass "an unapproved code is pending" ;;
    *) fail "polling an unapproved code did not say authorization_pending" ;;
  esac
  case "$(poll nosuchcode)" in
    *invalid_grant*) pass "an unknown device code is refused" ;;
    *) fail "an unknown device code was not refused" ;;
  esac
  # The approval page is a human's, so it must send an anonymous
  # visitor to log in rather than answer 401.
  [ "$(code "$url/device")" = "303" ] \
    || fail "the approval page did not redirect an anonymous visitor to the login"
  pass "the approval page requires a login"

  # The one a plain curl cannot see: GitHub redirects the browser back,
  # so the callback arrives as a top-level cross-site navigation. The
  # browser guard must admit it, and neither curl nor Go's http.Client
  # sends Sec-Fetch-*, so the header is set by hand here.
  cb=$(code -H 'Sec-Fetch-Site: cross-site' -H 'Sec-Fetch-Mode: navigate' \
    -H 'Sec-Fetch-Dest: document' "$url/oauth/github/callback?code=x&state=y")
  [ "$cb" != "403" ] \
    || fail "the guard refuses a browser's callback; no login can ever complete"
  pass "the callback survives a cross-site navigation"
fi

# ------------------------------------------------------------- the store

step "State survives a restart"

if [ -z "$repo_ref" ]; then
  echo "  skip  no --repo given, so nothing was launched"
  echo "        this is the check that proves the store is Postgres rather"
  echo "        than memory; re-run with --repo owner/name to exercise it"
elif [ "$own_compose" = 0 ]; then
  echo "  skip  --url was given, so this script cannot restart the server"
  echo "        restart it yourself, then confirm the session is still listed"
else
  name="smoke-$(date +%s)"
  owner=${repo_ref%%/*}
  reponame=${repo_ref##*/}
  [ "$owner" != "$repo_ref" ] || fail "--repo must be owner/name"

  created=$(curl -fsS "${auth[@]}" -H 'Content-Type: application/json' \
    -X POST "$url/api/v1/sessions" \
    -d "{\"name\":\"$name\",\"repos\":[{\"platform\":\"github\",\"owner\":\"$owner\",\"name\":\"$reponame\"}]}") \
    || fail "creating a session failed; check the server has a credential for $repo_ref"

  launched_session=$(printf '%s' "$created" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  [ -n "$launched_session" ] || fail "the create response had no session id: $created"
  pass "created session $launched_session"

  docker compose -f "$compose_file" restart server >/dev/null
  restart_deadline=$(( $(date +%s) + 180 ))
  until [ "$(code "$url/readyz")" = "204" ]; do
    [ "$(date +%s)" -lt "$restart_deadline" ] || fail "the server did not come back after a restart"
    sleep 2
  done
  pass "the server came back"

  # The whole point: with an in-memory store the session is gone after a
  # restart and the listing comes back empty.
  if curl -fsS "${auth[@]}" "$url/api/v1/sessions" | grep -q "$launched_session"; then
    pass "the session survived a restart (the store is Postgres, not memory)"
  else
    fail "the session vanished across a restart; the server is on the memory store"
  fi
fi

# --------------------------------------------------------------- summary

step "Done"
echo "  the deployment answers health and enforces both authentication"
echo "  and the browser guard."
if [ -n "$launched_session" ]; then
  echo "  its state survived a restart, so it is running on Postgres."
else
  echo "  whether it is on Postgres is untested: pass --repo owner/name."
fi
