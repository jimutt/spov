#!/usr/bin/env bash
# Run spov serve in hosted mode on this machine for the combined
# scenarios: its own Postgres container, the
# credential proxy and a dedicated Modal environment. Sign-in is seeded
# (seed), so no OAuth app or second GitHub account is needed.
#
#   scripts/scenarios/hosted-local.sh build      build the web app, proxy and binary
#   scripts/scenarios/hosted-local.sh image      build the base image in the Modal environment, once
#   scripts/scenarios/hosted-local.sh start      start Postgres, migrate, serve in the background
#   scripts/scenarios/hosted-local.sh stop       stop the server (Postgres keeps running)
#   scripts/scenarios/hosted-local.sh restart    stop then start, keeping every key
#   scripts/scenarios/hosted-local.sh status     whether it runs, and where its log is
#   scripts/scenarios/hosted-local.sh seed       sign in admin@ and member@scenario.test
#   scripts/scenarios/hosted-local.sh down       stop the server and the Postgres container
#
# start and restart take --env-app <key.pem>: the App in the environment.
#
# Secrets come from ~/.config/spov-test (SPOV_SCENARIO_SECRETS) and keys
# generated under the state directory, never printed, in argv or the repo.
#
# @decision(DL-022)
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
secrets=${SPOV_SCENARIO_SECRETS:-$HOME/.config/spov-test}
state=${SPOV_SCENARIO_STATE:-$HOME/.local/state/spov-scenarios}
port=${SPOV_SCENARIO_PORT:-8091}
pg_name=spov-scenarios-pg
pg_port=${SPOV_SCENARIO_PG_PORT:-55440}
modal_env=${SPOV_SCENARIO_MODAL_ENV:-spov-scenarios}
bin="$state/bin/spov"
pidfile="$state/server.pid"
log="$state/server.log"
metrics=${SPOV_SCENARIO_METRICS:-127.0.0.1:9491}

# The log and state may hold what the scan looks for, so only you read them.
umask 077
mkdir -p "$state" && chmod 700 "$state"

die() { echo "hosted-local: $*" >&2; exit 1; }

# need_file refuses a secrets file that is missing or readable by others.
need_file() {
  [ -f "$1" ] || die "missing $1"
  case "$(stat -c %a "$1")" in
    600|400) ;;
    *) die "$1 must be chmod 600" ;;
  esac
}

# keyfile makes a random secret once and keeps it, so a restart opens
# what the previous run sealed.
keyfile() {
  local f="$state/keys/$1"
  if [ ! -f "$f" ]; then
    mkdir -p "$state/keys" && chmod 700 "$state/keys"
    (umask 077 && openssl rand -base64 32 >"$f")
  fi
  cat "$f"
}

build() {
  cd "$root"
  bash scripts/build-web.sh
  bash scripts/build-proxy.sh
  mkdir -p "$state/bin"
  go build -o "$bin" ./cmd/spov
  echo "hosted-local: built $bin at $(git rev-parse --short HEAD)"
}

postgres() {
  if ! docker inspect "$pg_name" >/dev/null 2>&1; then
    docker run -d --name "$pg_name" -e POSTGRES_USER=spov -e POSTGRES_PASSWORD=spov -e POSTGRES_DB=spov \
      -p "127.0.0.1:$pg_port:5432" postgres:16 >/dev/null
  elif [ "$(docker inspect -f '{{.State.Running}}' "$pg_name")" != true ]; then
    docker start "$pg_name" >/dev/null
  fi
  for _ in $(seq 30); do
    # Over TCP: the image's first-boot init server listens on its socket only.
    docker exec "$pg_name" pg_isready -h 127.0.0.1 -U spov >/dev/null 2>&1 && return
    sleep 1
  done
  die "Postgres did not become ready"
}

# server_config writes the server's own user file (the forge platforms
# and the Modal environment) and the two seeded users' allowlist. Each
# user stores their own GitLab and Forgejo tokens (scenario 4).
# SCENARIO_FORGEJO_ACCOUNT is the Forgejo account the platform uses and
# has no default. SCENARIO_FORGEJO_URL points the platform somewhere
# other than Codefloe: the scenario's own Forgejo in Modal (DL-041).
server_config() {
  local forgejo=${SCENARIO_FORGEJO_URL:-https://codefloe.com} forgejo_account=${SCENARIO_FORGEJO_ACCOUNT:?set SCENARIO_FORGEJO_ACCOUNT, the Forgejo account the scenario uses}
  mkdir -p "$state/server/spov"
  cat >"$state/server/spov/config.yaml" <<EOF
modal:
  environment: $modal_env
platforms:
  - name: github
    kind: github
    account: $SCENARIO_GITHUB_ORG
  - name: gitlab
    kind: gitlab
  - name: codefloe
    kind: forgejo
    base_url: $forgejo
    account: $forgejo_account
EOF
  printf 'admin@scenario.test admin\nmember@scenario.test\n' >"$state/allowlist"
}

# login_env is the environment both the server and seed read: the
# database, the public URL, the allowlist, and an OAuth client that is
# never used (login is on so tokens and cookies are, but nobody signs in
# through GitHub).
login_env() {
  export SPOV_DATABASE_URL="postgres://spov:spov@127.0.0.1:$pg_port/spov?sslmode=disable"
  export SPOV_PUBLIC_URL="http://localhost:$port" SPOV_ALLOWLIST_FILE="$state/allowlist"
  export SPOV_GITHUB_OAUTH_CLIENT_ID=spov-scenario-unused
  SPOV_GITHUB_OAUTH_CLIENT_SECRET=$(keyfile oauth-unused)
  export SPOV_GITHUB_OAUTH_CLIENT_SECRET
}

# image publishes the base image in the scenario's Modal environment,
# which a launch never builds; it returns at once when it exists.
image() {
  [ -x "$bin" ] || die "no binary; run: $0 build"
  need_file "$secrets/github-app.env"
  set -a
  # shellcheck disable=SC1091
  . "$secrets/github-app.env"
  set +a
  postgres
  server_config
  login_env
  XDG_CONFIG_HOME="$state/server" MODAL_ENVIRONMENT="$modal_env" "$bin" serve image build
}

seed() {
  running || die "start the server first, so the allowlist is in the database"
  login_env
  (cd "$root" && SPOV_SCENARIO_SEED="$state" go test -tags integration -count=1 ./internal/cli/ -run '^TestIntegrationScenarioSeedUsers$' -v) |
    grep -E 'scenario.test|FAIL|ok ' || die "seeding failed"
}

# running checks the recorded pid is still this binary, so a pid reused
# after a reboot is never signalled. A server started before a rebuild
# runs a binary the kernel names "<path> (deleted)", and is still ours.
running() {
  [ -f "$pidfile" ] || return 1
  local pid exe
  pid=$(cat "$pidfile")
  kill -0 "$pid" 2>/dev/null || return 1
  exe=$(readlink "/proc/$pid/exe" 2>/dev/null) || return 1
  [ "${exe% (deleted)}" = "$(readlink -f "$bin")" ]
}

start() {
  local env_app=""
  if [ "${1:-}" = "--env-app" ]; then
    env_app=${2:?--env-app needs a key file}
    need_file "$env_app"
  fi
  running && die "already running (pid $(cat "$pidfile"))"
  [ -x "$bin" ] || die "no binary; run: $0 build"
  # Nothing from your shell's SPOV_* reaches the server, such as a
  # SPOV_PLATFORM_TOKEN_GITHUB that would take the org tier's place.
  while IFS= read -r v; do unset "$v"; done < <(compgen -e | grep '^SPOV_' || true)
  need_file "$secrets/hosted.env"
  need_file "$secrets/github-app.env"
  set -a
  # shellcheck disable=SC1091
  . "$secrets/hosted.env"
  # shellcheck disable=SC1091
  . "$secrets/github-app.env"
  set +a
  : "${SCENARIO_GITHUB_ORG:?not set in github-app.env}"
  postgres
  server_config
  login_env

  export XDG_CONFIG_HOME="$state/server" XDG_STATE_HOME="$state/server-state"
  export SPOV_LISTEN="127.0.0.1:$port" MODAL_ENVIRONMENT="$modal_env"
  # Scenario 5 runs four jobs at once; the default limit is two.
  export SPOV_QUOTAS_MAX_CONCURRENT_JOBS="${SPOV_QUOTAS_MAX_CONCURRENT_JOBS:-4}"
  export SPOV_METRICS_PROMETHEUS_LISTEN="$metrics"
  SPOV_SERVER_TOKEN=$(keyfile server-token)
  SPOV_CREDENTIAL_KEY=$(keyfile credential-key)
  export SPOV_SERVER_TOKEN SPOV_CREDENTIAL_KEY
  unset SPOV_GITHUB_APP_ID SPOV_GITHUB_APP_PRIVATE_KEY
  if [ -n "$env_app" ]; then
    : "${SCENARIO_APP_ID:?not set in github-app.env}"
    SPOV_GITHUB_APP_ID=$SCENARIO_APP_ID
    SPOV_GITHUB_APP_PRIVATE_KEY=$(cat "$env_app")
    export SPOV_GITHUB_APP_ID SPOV_GITHUB_APP_PRIVATE_KEY
  fi
  # A server left running on the port would answer every request in the
  # new one's place.
  if curl -fsS "http://localhost:$port/livez" >/dev/null 2>&1; then
    die "something already answers on port $port; stop it first"
  fi
  "$bin" serve migrate up >>"$log" 2>&1 || die "migrate failed; see $log"
  {
    echo "=== start $(date -u +%FT%TZ) commit $(git -C "$root" rev-parse --short HEAD) env-app=${env_app:+yes}"
  } >>"$log"
  nohup "$bin" serve >>"$log" 2>&1 &
  echo $! >"$pidfile"
  for _ in $(seq 30); do
    if curl -fsS "http://localhost:$port/livez" >/dev/null 2>&1; then
      echo "hosted-local: serving http://localhost:$port (pid $(cat "$pidfile")), log $log"
      return
    fi
    running || die "the server exited; see $log"
    sleep 1
  done
  die "the server did not answer /livez; see $log"
}

stop() {
  if running; then
    kill "$(cat "$pidfile")"
    # The drain can take the whole 20 s shutdown budget with a page's
    # stream open, then the queue's and the store's.
    for _ in $(seq 120); do running || break; sleep 0.5; done
    running && die "the server did not stop"
    echo "hosted-local: stopped"
  fi
  rm -f "$pidfile"
}

case "${1:-}" in
  build) build ;;
  start) shift; start "$@" ;;
  stop) stop ;;
  restart) shift; stop; start "$@" ;;
  image) image ;;
  seed) seed ;;
  status)
    if running; then echo "running (pid $(cat "$pidfile")) on http://localhost:$port, log $log"; else echo "stopped"; fi ;;
  down) stop; docker stop "$pg_name" >/dev/null 2>&1 || true; echo "hosted-local: Postgres stopped" ;;
  *) sed -n '2,19p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
