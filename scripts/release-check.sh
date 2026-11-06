#!/usr/bin/env bash
# Refuse a release whose binary lacks the web app or the credential
# proxy, or reports a version other than the tag's. Both missing parts
# build and run normally, which is why nothing else would notice.
#
#   scripts/release-check.sh <binary> <tag>
#
# @decision(DL-079)
set -euo pipefail

bin=${1:?usage: release-check.sh <binary> <tag>}
tag=${2:?usage: release-check.sh <binary> <tag>}
want=${tag#v}

report=$("$bin" version --embedded)
echo "$report"
fail=0
for part in web proxy; do
  if ! grep -qx "$part: built" <<<"$report"; then
    echo "release-check: the binary has no $part built in; run scripts/build-web.sh and scripts/build-proxy.sh before building" >&2
    fail=1
  fi
done
got=$("$bin" version --short)
if [ "$got" != "$want" ]; then
  echo "release-check: the binary reports version $got, and the tag is $tag" >&2
  fail=1
fi
exit "$fail"
