# Releasing spov

A release is a tag. `.github/workflows/release.yml` turns it into a
**draft** GitHub release, which a maintainer publishes after looking at it
(DL-079).

```sh
git tag v0.1.0 && git push origin v0.1.0
```

## Before tagging

- **Refresh the model list the binary ships.** A server reads models.dev's
  live list within its first hour and keeps it fresh, but it falls back
  to the embedded snapshot until then and whenever models.dev is out of
  reach (DL-013). Nothing refreshes the snapshot automatically:

  ```sh
  scripts/model-snapshot.sh
  ```

  Review the diff in `internal/modelcat/snapshot.json`, and merge it before
  the tag. `TestTheSnapshotHoldsEveryRung` fails if a prep pass's model
  family has dropped out of the list (DL-047).
- **Write the release notes by hand.** goreleaser's changelog lists every
  commit since the last tag. Replace it in the draft before publishing.

## What the workflow does

1. builds this platform's binary as the release does and runs
   `scripts/release-check.sh` on it. The release stops if the binary lacks
   the web app or the credential proxy (`spov version --embedded`), or
   reports a version other than the tag's;
2. runs goreleaser: `spov_<version>_<os>_<arch>.tar.gz` for Linux and macOS
   on amd64 and arm64, `checksums.txt`, and its Sigstore bundle
   `checksums.txt.sigstore.json`, signed keylessly through the workflow's
   OIDC token;
3. attests each archive's build provenance (`gh attestation verify`);
4. writes the Homebrew cask to `jimutt/homebrew-tap`, if the
   `HOMEBREW_TAP_TOKEN` secret is set (DL-081).

A tag with a prerelease suffix (`v0.2.0-rc.1`) makes a prerelease, and its
cask is not uploaded.

## One-time setup

- **The tap:** create the public repository `jimutt/homebrew-tap`, and a
  fine-grained token with contents read and write on it alone, stored as
  the repository secret `HOMEBREW_TAP_TOKEN`. Without it every release
  still succeeds, without a cask.

## Checking a release

```sh
gh attestation verify spov_0.1.0_linux_amd64.tar.gz --repo jimutt/spov
cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github.com/jimutt/spov/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum --ignore-missing -c checksums.txt
```

## Rehearsing without publishing

```sh
goreleaser release --snapshot --skip=publish,sign --clean
bash scripts/release-check.sh dist/spov_linux_amd64_v1/spov v<the snapshot's version>
```
