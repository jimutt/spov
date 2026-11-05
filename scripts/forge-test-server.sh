#!/usr/bin/env bash
# Run a real Forgejo or Gitea on this machine for TestScenario4, on
# 127.0.0.1 over HTTPS with a certificate of its own (a launch clones
# only over https) and SQLite, and an admin user and token made
# from the forge's own command line. Where Docker is available it runs
# the release's official image; otherwise (the CI runner, which has no
# Docker) the release binary, checked against a checksum committed here.
# FORGE_RUNTIME=docker or binary picks one.
#
#   scripts/forge-test-server.sh start forgejo|gitea <dir>
#   scripts/forge-test-server.sh stop <dir>
#
# start writes <dir>/env, which the suite reads (SPOV_TEST_FORGEJO_ENV
# or SPOV_TEST_GITEA_ENV): FORGE_KIND, FORGE_URL, FORGE_CA (the
# certificate, which the suite trusts), FORGE_ADMIN,
# FORGE_ADMIN_PASSWORD and FORGE_ADMIN_TOKEN. Everything under <dir> is
# the run's alone and is readable only by you.
# Forgejo follows Codefloe's version; a bump changes the version and
# both sums, from the release's .sha256 files.
#
# @decision(DL-041)
set -euo pipefail

FORGEJO_VERSION=17.0.1
GITEA_VERSION=28.3.0

sum_for() {
  case "$1" in
    forgejo-17.0.1-linux-amd64) echo 51a8cdf4a1252e79f4b3855d7a7891af6c6635105420768c460e2f0690385d6f ;;
    forgejo-17.0.1-linux-arm64) echo 1e99403746406e8199efc903ddd1940d5728a60e50241f7aa6c9f2b907bd8ec9 ;;
    gitea-28.3.0-linux-amd64)   echo 5f834e8f04986f3b9206cf14743b5a6e6996ceddc238dac6409866ebda2a0686 ;;
    gitea-28.3.0-linux-arm64)   echo 24d146e8f5c986b86e5a3311e3ec349ded35202cba056952ab5b585511ace82d ;;
    *) return 1 ;;
  esac
}

die() { echo "forge-test-server: $*" >&2; exit 1; }

# fetch downloads the release binary once, into the user's cache, and
# prints its path. A file that does not match its checksum is refused.
fetch() {
  local kind=$1 version url asset arch
  case "$(uname -m)" in
    x86_64|amd64)  arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
    *) die "unsupported architecture $(uname -m)" ;;
  esac
  [ "$(uname -s)" = Linux ] || die "the binary runtime is Linux only; use Docker"
  case "$kind" in
    forgejo) version=$FORGEJO_VERSION; asset="forgejo-$version-linux-$arch"
             url="https://codeberg.org/forgejo/forgejo/releases/download/v$version/$asset" ;;
    gitea)   version=$GITEA_VERSION; asset="gitea-$version-linux-$arch"
             url="https://dl.gitea.com/gitea/$version/$asset" ;;
  esac
  local want cache bin
  want=$(sum_for "$asset") || die "no committed checksum for $asset"
  cache=${XDG_CACHE_HOME:-$HOME/.cache}/spov/forges
  bin="$cache/$asset"
  mkdir -p "$cache"
  if [ ! -x "$bin" ] || [ "$(sha256sum "$bin" | cut -d' ' -f1)" != "$want" ]; then
    echo "forge-test-server: downloading $asset" >&2
    curl -fsSL --retry 3 -o "$bin.part" "$url"
    [ "$(sha256sum "$bin.part" | cut -d' ' -f1)" = "$want" ] || { rm -f "$bin.part"; die "$asset does not match its checksum"; }
    mv "$bin.part" "$bin" && chmod 755 "$bin"
  fi
  echo "$bin"
}

free_port() {
  python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
}

# wait_up waits for the forge's API, makes the admin user with the
# forge's own CLI (the remaining arguments) and writes the env file.
wait_up() {
  local kind=$1 dir=$2 port=$3
  shift 3
  for _ in $(seq 90); do
    curl -sf --cacert "$dir/tls/cert.pem" -o /dev/null "https://127.0.0.1:$port/api/v1/version" && break
    sleep 1
  done
  curl -sf --cacert "$dir/tls/cert.pem" -o /dev/null "https://127.0.0.1:$port/api/v1/version" ||
    die "$kind did not come up on port $port"
  local pass token
  pass=$(openssl rand -hex 16)
  token=$("$@" admin user create --username scn-admin --password "$pass" \
    --email scn-admin@example.test --admin --must-change-password=false --access-token 2>&1 |
    grep -oE '[0-9a-f]{40}' | head -1) || true
  [ -n "$token" ] || die "could not create the admin user"
  {
    echo "FORGE_KIND=$kind"
    echo "FORGE_URL=https://127.0.0.1:$port"
    echo "FORGE_CA=$dir/tls/cert.pem"
    echo "FORGE_ADMIN=scn-admin"
    echo "FORGE_ADMIN_PASSWORD=$pass"
    echo "FORGE_ADMIN_TOKEN=$token"
  } >"$dir/env"
  echo "forge-test-server: $kind on https://127.0.0.1:$port, env in $dir/env" >&2
}

start_docker() {
  local kind=$1 dir=$2 port=$3 prefix image
  case "$kind" in
    forgejo) prefix=FORGEJO; image="codeberg.org/forgejo/forgejo:$FORGEJO_VERSION" ;;
    gitea)   prefix=GITEA; image="docker.gitea.com/gitea:$GITEA_VERSION" ;;
  esac
  local name="spov-forge-$kind-$port"
  # The image's user must read the key; it is this run's throwaway one.
  chmod 755 "$dir" "$dir/tls" && chmod 644 "$dir/tls/key.pem"
  docker run -d --rm --name "$name" -p "127.0.0.1:$port:3000" -v "$dir/tls:/tls:ro" \
    -e "${prefix}__security__INSTALL_LOCK=true" -e "${prefix}__database__DB_TYPE=sqlite3" \
    -e "${prefix}__server__PROTOCOL=https" -e "${prefix}__server__CERT_FILE=/tls/cert.pem" \
    -e "${prefix}__server__KEY_FILE=/tls/key.pem" \
    -e "${prefix}__server__ROOT_URL=https://127.0.0.1:$port/" -e "${prefix}__server__DISABLE_SSH=true" \
    -e "${prefix}__service__DISABLE_REGISTRATION=true" "$image" >/dev/null
  echo "docker:$name" >"$dir/handle"
  wait_up "$kind" "$dir" "$port" docker exec -u git "$name" "$kind"
}

start_binary() {
  local kind=$1 dir=$2 port=$3 bin
  bin=$(fetch "$kind")
  mkdir -p "$dir"/{custom/conf,data,log}
  {
    echo "RUN_MODE = prod"
    echo "WORK_PATH = $dir"
    # The CI runner runs as root, which both refuse unless the config's
    # top section allows it.
    echo "I_AM_BEING_UNSAFE_RUNNING_AS_ROOT = true"
    echo "[server]"
    echo "PROTOCOL = https"
    echo "CERT_FILE = $dir/tls/cert.pem"
    echo "KEY_FILE = $dir/tls/key.pem"
    echo "HTTP_ADDR = 127.0.0.1"
    echo "HTTP_PORT = $port"
    echo "ROOT_URL = https://127.0.0.1:$port/"
    echo "DISABLE_SSH = true"
    echo "APP_DATA_PATH = $dir/data"
    echo "[database]"
    echo "DB_TYPE = sqlite3"
    echo "PATH = $dir/data/forge.db"
    echo "[repository]"
    echo "ROOT = $dir/data/repos"
    echo "[security]"
    echo "INSTALL_LOCK = true"
    echo "[service]"
    echo "DISABLE_REGISTRATION = true"
    echo "[log]"
    echo "ROOT_PATH = $dir/log"
    echo "LEVEL = Warn"
  } >"$dir/custom/conf/app.ini"
  local cfg=(--work-path "$dir" --config "$dir/custom/conf/app.ini")
  "$bin" "${cfg[@]}" migrate >"$dir/log/migrate.log" 2>&1 || die "migrate failed: $(tail -3 "$dir/log/migrate.log")"
  nohup "$bin" "${cfg[@]}" web >"$dir/log/web.log" 2>&1 &
  echo "pid:$!" >"$dir/handle"
  wait_up "$kind" "$dir" "$port" "$bin" "${cfg[@]}"
}

start() {
  local kind=$1 dir=$2 port runtime
  case "$kind" in forgejo|gitea) ;; *) die "the kind is forgejo or gitea" ;; esac
  umask 077
  mkdir -p "$dir"
  dir=$(cd "$dir" && pwd)
  port=$(free_port)
  mkdir -p "$dir/tls"
  openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj "/CN=127.0.0.1" \
    -addext "subjectAltName=IP:127.0.0.1" -keyout "$dir/tls/key.pem" -out "$dir/tls/cert.pem" 2>/dev/null
  runtime=${FORGE_RUNTIME:-}
  if [ -z "$runtime" ]; then
    if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then runtime=docker; else runtime=binary; fi
  fi
  case "$runtime" in
    docker) start_docker "$kind" "$dir" "$port" ;;
    binary) start_binary "$kind" "$dir" "$port" ;;
    *) die "FORGE_RUNTIME is docker or binary" ;;
  esac
}

stop() {
  local dir=$1 handle
  [ -f "$dir/handle" ] || return 0
  handle=$(cat "$dir/handle")
  case "$handle" in
    docker:*) docker stop "${handle#docker:}" >/dev/null 2>&1 || true ;;
    pid:*)    kill "${handle#pid:}" 2>/dev/null || true ;;
  esac
  rm -f "$dir/handle"
}

case "${1:-}" in
  start) start "${2:?forgejo or gitea}" "${3:?a directory}" ;;
  stop)  stop "${2:?a directory}" ;;
  *) die "usage: forge-test-server.sh start forgejo|gitea <dir> | stop <dir>" ;;
esac
