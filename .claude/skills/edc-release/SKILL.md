---
name: edc-release
description: Cut an edc release — pick the version, verify locally, tag, and let GitHub Actions publish the assets. Use when the user asks to release edc, cut a version, publish a tag, or fix a failed release run.
---

# edc release

Release `edc` with an annotated `v<version>` tag. GitHub Actions builds every asset and publishes the release. Never upload an asset by hand.

## Pieces that share the version

These four must agree. If one changes, check the others.

| piece | where | value |
|---|---|---|
| tag | git | `v0.2.0` |
| `main.version` | `-ldflags` in `Makefile` | `0.2.0` |
| asset name | `make dist` | `edc_0.2.0_<os>_<arch>.tar.gz` |
| lookup name | `updateAssetName` in `internal/edc/update.go` | same string |

`install.sh` and `edc update` both read `checksums.txt` from the release. A rename breaks both.

## Steps

1. Confirm the branch is `main` and the tree is clean. Run `git-kit context --include=diff,log,precheck,remotes`.
2. Pick the version. Read the commits since the last tag and propose a number. Ask the user to confirm it.
   ```bash
   git describe --tags --abbrev=0 2>/dev/null || echo "no tag yet"
   git-kit log -n 20
   ```
3. Verify locally before the tag. A failure after the tag needs a tag deletion.
   ```bash
   make check
   make dist VERSION=<version>
   ```
4. Check the assets. There must be four archives and one `checksums.txt`.
   ```bash
   ls -1 dist
   tar -xzf dist/edc_<version>_darwin_arm64.tar.gz -O edc > /dev/null && echo "archive ok"
   ```
5. Check the README demos. If a command output changed in this release, render the tapes again and commit the GIFs.
   ```bash
   ls docs/tape/*.tape
   ```
   Do not render `info`, `sockets`, or `top`. Those tapes leave host information on the screen.
6. Create the tag and push it. Push the tag only after the branch is pushed.
   ```bash
   git tag -a v<version> -m "edc v<version>"
   git push origin main
   git push origin v<version>
   ```
7. Watch the release run.
   ```bash
   gh run watch --exit-status
   gh release view v<version>
   ```
8. Verify the published release from the outside.
   ```bash
   curl -fsSL https://raw.githubusercontent.com/x-mesh/edc/main/install.sh | BINDIR=/tmp/edc-check sh
   /tmp/edc-check/edc version
   ```
9. Verify the update path from the previous version. Install the earlier version, then update.
   ```bash
   EDC_VERSION=<previous> BINDIR=/tmp/edc-old sh install.sh
   /tmp/edc-old/edc update --check
   ```

## If the release run fails

Delete the tag, fix the cause, then tag again with the same number. A published release needs `gh release delete` first.

```bash
gh release delete v<version> --yes   # only if the release exists
git push origin :refs/tags/v<version>
git tag -d v<version>
```

Never move a tag that a release already published. Users who installed it get a different binary under the same version.

## Rules

- Ask the user before the first `git push` of a tag. A tag starts a public build.
- Keep `make check` green. The release workflow runs it again and stops on a failure.
- Do not put a host name, an IP address, or a user name in release notes.
- The workflow uses `${{ github.token }}`. It needs no personal token.
