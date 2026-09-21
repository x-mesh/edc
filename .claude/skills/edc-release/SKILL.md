---
name: edc-release
description: Cut an edc release — pick the version, verify locally, tag, let GitHub Actions publish the assets, and write the release notes. Use when the user asks to release edc, cut a version, publish a tag, write release notes, or fix a failed release run.
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
   Do not render `info`, `listen`, or `top`. Those tapes leave host information on the screen.
6. Create the tag and push it. Push the tag only after the branch is pushed.
   ```bash
   git tag -a v<version> -m "edc v<version>"
   git push origin main
   git push origin v<version>
   ```
7. Watch the release run. `gh run watch` needs the run ID, so read it first.
   ```bash
   gh run list --limit 3
   gh run watch <run-id> --exit-status --compact
   gh release view v<version>
   ```
8. Write the release notes. See [Release notes](#release-notes) for the shape.
   ```bash
   gh release edit v<version> --notes-file <path>
   gh release view v<version> --json body -q .body
   ```
9. Verify the published release from the outside.
   ```bash
   curl -fsSL https://raw.githubusercontent.com/x-mesh/edc/main/install.sh | BINDIR=/tmp/edc-check sh
   /tmp/edc-check/edc version
   ```
10. Verify the update path from the previous version. Install the earlier version, then update.
    ```bash
    EDC_VERSION=<previous> BINDIR=/tmp/edc-old sh install.sh
    /tmp/edc-old/edc update --check
    ```

## Release notes

The workflow writes only a `Full Changelog` link. Replace the body with notes that a user reads before an update.

Write one `##` section for each change that a user sees. Name the command in the heading. Order the sections by the size of the change for a user.

In each section, write what the user gets, then why it changed. For a fix, name the defect and the effect on a host. For a new command, write what it does and the platform that it needs.

State every change that a user must act on: a state schema bump, a renamed flag, a changed default.

Keep the `Full Changelog` line at the end. Leave out a host name, an IP address, a user name, and an identifier that a user cannot see.

Read the `v0.12.0` notes for the shape.

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
