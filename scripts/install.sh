#!/bin/sh
# Install spov from a GitHub release, checking the archive against the
# release's checksums before anything is unpacked, and the checksums'
# signature when cosign is installed (DL-080).
#
#   curl -fsSLO https://github.com/jimutt/spov/releases/latest/download/install.sh
#   sh install.sh [--with-opencode]
#
# SPOV_VERSION=0.1.0       install that release instead of the latest
# SPOV_INSTALL_DIR=<dir>   where spov goes (default ~/.local/bin)
# SPOV_REQUIRE_SIGNATURE=1 refuse to install without verifying the signature
# --with-opencode          also install OpenCode at the pin: the server's
#                          (SPOV_SERVER) when one is set, else spov's own
#
# Running it again updates spov: there is no update command.
#
# @decision(DL-080)
set -eu

REPO=jimutt/spov
RELEASES=${SPOV_RELEASES_URL:-https://github.com/$REPO/releases}
NPM=${SPOV_NPM_REGISTRY:-https://registry.npmjs.org}
INSTALL_DIR=${SPOV_INSTALL_DIR:-$HOME/.local/bin}
DATA_DIR=${XDG_DATA_HOME:-$HOME/.local/share}/spov
WITH_OPENCODE=0

say() { printf 'spov-install: %s\n' "$*" >&2; }
die() { say "$*"; exit 1; }
# spov runs the binary just installed without this script's own variables:
# it reads every SPOV_* variable as config, and SPOV_VERSION names its
# config file's version field.
spov() {
  (unset SPOV_VERSION SPOV_INSTALL_DIR SPOV_REQUIRE_SIGNATURE SPOV_RELEASES_URL \
    SPOV_NPM_REGISTRY SPOV_INSTALL_OS SPOV_INSTALL_ARCH
  "$INSTALL_DIR/spov" "$@")
}

for arg in "$@"; do
  case "$arg" in
    --with-opencode) WITH_OPENCODE=1 ;;
    -h | --help) sed -n '2,18p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown argument $arg (try --help)" ;;
  esac
done

# The platform, as the release names it.
os=${SPOV_INSTALL_OS:-$(uname -s)}
arch=${SPOV_INSTALL_ARCH:-$(uname -m)}
case "$os" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  MINGW* | MSYS* | CYGWIN* | Windows*) die "spov runs on Windows only inside WSL; open a WSL shell and run this there" ;;
  *) die "spov is released for Linux and macOS, not $os" ;;
esac
case "$arch" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) die "spov is released for amd64 and arm64, not $arch" ;;
esac

if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL --retry 2 -o "$2" "$1"; }
  final_url() { curl -fsSLI -o /dev/null -w '%{url_effective}' "$1"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -q -O "$2" "$1"; }
  final_url() { wget -q --max-redirect=5 -S --spider "$1" 2>&1 | sed -n 's/^ *[Ll]ocation: *//p' | tail -1 | tr -d '\r'; }
else
  die "curl or wget is needed"
fi
if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  die "sha256sum or shasum is needed to check the download"
fi

# The version: SPOV_VERSION, else where releases/latest redirects.
version=${SPOV_VERSION:-}
if [ -z "$version" ]; then
  latest=$(final_url "$RELEASES/latest") || die "could not find the latest release at $RELEASES"
  version=${latest##*/tag/}
  [ "$version" != "$latest" ] || die "could not read the latest release from $latest"
fi
version=${version#v}

tmp=$(mktemp -d 2>/dev/null || mktemp -d -t spov-install)
trap 'rm -rf "$tmp"' EXIT INT TERM
archive="spov_${version}_${os}_${arch}.tar.gz"
base="$RELEASES/download/v$version"
say "downloading spov $version for $os/$arch"
fetch "$base/$archive" "$tmp/$archive" || die "could not download $base/$archive"
fetch "$base/checksums.txt" "$tmp/checksums.txt" || die "could not download the release's checksums"

# The signature first, so a checksums file nobody signed is never
# trusted when cosign can tell.
if command -v cosign >/dev/null 2>&1; then
  fetch "$base/checksums.txt.sigstore.json" "$tmp/checksums.txt.sigstore.json" || die "the release has no signature bundle"
  # cosign's own words on failure: an old cosign cannot read the bundle
  # format, which is not the same as a bad signature.
  if ! out=$(cosign verify-blob "$tmp/checksums.txt" --bundle "$tmp/checksums.txt.sigstore.json" \
    --certificate-identity-regexp "^https://github.com/$REPO/\.github/workflows/release\.yml@refs/tags/v" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com 2>&1); then
    printf '%s\n' "$out" >&2
    die "the checksums' signature does not verify (this needs cosign v2.4.2 or newer); nothing was installed"
  fi
  say "the checksums' signature verifies"
elif [ "${SPOV_REQUIRE_SIGNATURE:-}" = 1 ]; then
  die "SPOV_REQUIRE_SIGNATURE=1 and cosign is not installed"
else
  say "cosign is not installed, so only the checksums are checked"
fi

want=$(awk -v f="$archive" '$2 == f { print $1 }' "$tmp/checksums.txt")
[ -n "$want" ] || die "$archive is not in the release's checksums"
got=$(sha256 "$tmp/$archive")
[ "$got" = "$want" ] || die "$archive does not match its checksum; nothing was installed"

mkdir "$tmp/unpacked"
tar -xzf "$tmp/$archive" -C "$tmp/unpacked" spov
mkdir -p "$INSTALL_DIR"
mv -f "$tmp/unpacked/spov" "$INSTALL_DIR/spov"
chmod 755 "$INSTALL_DIR/spov"
say "installed $(spov version)"
# What to run next, by the name that works in this shell.
next_cmd=spov
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *)
    say "$INSTALL_DIR is not on your PATH; add it, for example: export PATH=\"$INSTALL_DIR:\$PATH\""
    next_cmd="$INSTALL_DIR/spov"
    ;;
esac
next_step() { say "next: run '$next_cmd init' to check this machine and set up what is missing"; }

if [ "$WITH_OPENCODE" != 1 ]; then
  next_step
  exit 0
fi

# OpenCode at the pin: the server's when one is configured, else the new
# binary's own default. It goes where spov's locator looks first.
pin=${SPOV_OPENCODE_VERSION:-}
if [ -z "$pin" ] && [ -n "${SPOV_SERVER:-}" ]; then
  fetch "${SPOV_SERVER%/}/opencode-pin.json" "$tmp/pin.json" 2>/dev/null &&
    pin=$(sed -n 's/.*"opencode" *: *"\([^"]*\)".*/\1/p' "$tmp/pin.json")
fi
if [ -z "$pin" ]; then
  pin=$(spov config show --format json 2>/dev/null |
    sed -n '/"opencode": {/,/}/s/.*"version": *"\([^"]*\)".*/\1/p' | head -1)
fi
[ -n "$pin" ] || die "could not tell which OpenCode version to install; set SPOV_OPENCODE_VERSION"
case "$os/$arch" in
  linux/amd64) target=linux-x64-baseline ;;
  linux/arm64) target=linux-arm64 ;;
  darwin/amd64) target=darwin-x64 ;;
  darwin/arm64) target=darwin-arm64 ;;
esac
say "downloading OpenCode $pin ($target)"
fetch "$NPM/@opencode%2fcli-$target/$pin" "$tmp/opencode.json" || die "npm has no OpenCode $pin for $target"
tarball=$(sed -n 's/.*"tarball" *: *"\([^"]*\)".*/\1/p' "$tmp/opencode.json" | head -1)
integrity=$(sed -n 's/.*"integrity" *: *"sha512-\([^"]*\)".*/\1/p' "$tmp/opencode.json" | head -1)
[ -n "$tarball" ] && [ -n "$integrity" ] || die "npm's metadata for OpenCode $pin has no tarball or integrity"
fetch "$tarball" "$tmp/opencode.tgz" || die "could not download $tarball"
command -v openssl >/dev/null 2>&1 || die "openssl is needed to check OpenCode's download"
got=$(openssl dgst -sha512 -binary "$tmp/opencode.tgz" | openssl base64 -A)
[ "$got" = "$integrity" ] || die "OpenCode's download does not match npm's integrity; it was not installed"
mkdir "$tmp/opencode"
tar -xzf "$tmp/opencode.tgz" -C "$tmp/opencode" package/bin/opencode
dest="$DATA_DIR/bin/$pin"
mkdir -p "$dest"
mv -f "$tmp/opencode/package/bin/opencode" "$dest/opencode"
chmod 755 "$dest/opencode"
say "installed OpenCode $pin at $dest/opencode, where spov looks for it"
next_step
