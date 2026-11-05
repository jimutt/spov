#!/usr/bin/env bash
# Install Bun at the pinned version and print the directory to add to
# PATH. Everything else goes to stderr, so the caller can do:
#
#   bash scripts/install-bun.sh >> "$GITHUB_PATH"
#
# The release archive is downloaded directly and checked against a
# checksum committed here: not piped from an installer script (pins
# Bun's version but not the installer's), and not oven-sh/setup-bun
# (needs node24 and calls the GitHub API for non-exact versions) —
# neither is safe to assume on a CI runner.
#
# To bump Bun, change VERSION and replace all four sums from
# https://github.com/oven-sh/bun/releases/download/bun-v<VERSION>/SHASUMS256.txt
#
# @decision(DL-055)
set -euo pipefail

VERSION="${BUN_VERSION:-1.4.0}"

# bun-<os>-<arch>.zip -> sha256, for Bun 1.4.0.
sum_for() {
  case "$1" in
    bun-darwin-aarch64.zip) echo c669e97f6164e1c96e0701748db98dfa77492908cbd8394c7557134a735de381 ;;
    bun-darwin-x64.zip)     echo 1d0211b8f1dc991182344687ad15e72ee86f154845a5f7fa477994cd341dd9b0 ;;
    bun-linux-aarch64.zip)  echo 4b1a332ee861983eb93bcfe6f770fff94e3e31b2c388bdaea3c8ed35e58eed0e ;;
    bun-linux-x64.zip)      echo 2d03fb5fb83ac8b567aca0a281b2ce1a1a19d488f56c2968d88c3f25e92fe452 ;;
    *) return 1 ;;
  esac
}

if command -v bun >/dev/null 2>&1; then
  have=$(bun --version)
  if [ "${have%%-*}" = "$VERSION" ]; then
    echo "bun $have is already installed" >&2
    dirname "$(command -v bun)"
    exit 0
  fi
  echo "bun $have is installed, but this project pins $VERSION" >&2
fi

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux)  os=linux ;;
  *) echo "unsupported OS $(uname -s); install bun $VERSION by hand" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  arm64|aarch64) arch=aarch64 ;;
  x86_64|amd64)  arch=x64 ;;
  *) echo "unsupported architecture $(uname -m); install bun $VERSION by hand" >&2; exit 1 ;;
esac

asset="bun-${os}-${arch}.zip"
if ! want=$(sum_for "$asset"); then
  echo "no committed checksum for $asset at bun $VERSION; update scripts/install-bun.sh" >&2
  exit 1
fi

# unzip is the one dependency commonly missing from slim images.
if ! command -v unzip >/dev/null 2>&1; then
  SUDO=""
  if [ "$(id -u)" -ne 0 ]; then
    command -v sudo >/dev/null || { echo "not root and no sudo; install unzip first" >&2; exit 1; }
    SUDO="sudo"
  fi
  $SUDO apt-get update -qq >&2
  $SUDO apt-get install -y --no-install-recommends unzip >&2
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

url="https://github.com/oven-sh/bun/releases/download/bun-v${VERSION}/${asset}"
echo "downloading $url" >&2
curl -fsSL "$url" -o "$work/$asset"

if command -v sha256sum >/dev/null 2>&1; then
  got=$(sha256sum "$work/$asset" | cut -d' ' -f1)
else
  got=$(shasum -a 256 "$work/$asset" | cut -d' ' -f1)
fi
if [ "$got" != "$want" ]; then
  echo "checksum mismatch for $asset" >&2
  echo "  expected $want" >&2
  echo "  got      $got" >&2
  exit 1
fi

dest="${BUN_INSTALL:-$HOME/.bun}/bin"
mkdir -p "$dest"
unzip -qo "$work/$asset" -d "$work"
install -m 0755 "$work/bun-${os}-${arch}/bun" "$dest/bun"
# The archive holds only `bun`; the upstream installer is what creates
# bunx, and it dispatches on argv[0]. Without this, `bun install` works
# and the first `bunx` three steps later fails with "command not found".
ln -sf bun "$dest/bunx"

# Both entry points, before anyone depends on them: a missing bunx
# otherwise surfaces in whichever later step happens to use it first.
for exe in bun bunx; do
  "$dest/$exe" --version >/dev/null || { echo "$exe does not run after install" >&2; exit 1; }
done
echo "installed bun $("$dest/bun" --version) to $dest" >&2
echo "$dest"
