---
name: edc-release
description: Cut an edc release — pick the version, verify locally, open and merge the develop→main release PR, tag the merge commit, let GitHub Actions publish the assets, and write the release notes. Use when the user asks to release edc, cut a version, publish a tag, write release notes, or fix a failed release run.
---

# edc release

Release `edc` with an annotated `v<version>` tag. GitHub Actions builds every asset and publishes the release. Never upload an asset by hand.

The release goes from `develop` to `main` through a pull request. The tag goes on the merge commit of that PR.

## Pieces that share the version

These four must agree. If one changes, check the others.

| piece | where | value |
|---|---|---|
| tag | git | `v0.2.0` |
| `main.version` | `-ldflags` in `Makefile` | `0.2.0` |
| asset name | `make dist` | `edc_0.2.0_<os>_<arch>.tar.gz` |
| lookup name | `updateAssetName` in `internal/edc/update.go` | same string |

`install.sh` and `edc update` both read `checksums.txt` from the release. A rename breaks both.

## Approval

A `/release` request from the user approves the whole release: the version, the release PR, the merge, and the tag push. Do not ask for a separate confirmation of each step. The user has said that too many questions are a defect.

Stop and report only when a step cannot continue: a failed check that the PR cannot fix, a conflict, a breaking change that makes the version unclear.

## Steps

1. Orient. Run `git-kit context --include=diff,log,precheck,remotes --fetch`.
   - Work on `develop`. Do not switch to `main`: another worktree (`~/.gk/worktree/edc/main`) holds it, and its dirty tree can stay as it is.
   - Do not use `git-kit land`. Its last step merges `develop` into the local `main` and fails on that dirty worktree. Use `git-kit pull` and `git-kit push`.
   - If `develop` has local commits that the release must carry, run `git-kit pull`, then `git-kit push`. The release uses `origin/develop`.
2. Pick the version. Do not ask the user.
   ```bash
   git tag --sort=-v:refname | head -1
   git log <latest-tag>..origin/develop --no-merges --format='%h %s'
   ```
   `git describe` on `develop` can print an older tag, so read the tag list. Use a minor bump if any `feat` commit exists, a patch bump if only `fix`, `perf`, `test` or `docs` commits exist. Ask only for a breaking change.
3. Verify locally on the head of `origin/develop`. A failure after the tag needs a tag deletion.
   ```bash
   make check
   make dist VERSION=<version>
   ```
   This build only checks the assets. Nothing is published, so the version stays free until the tag. The release workflow builds the published assets again from the tag.
4. Check the assets. There must be four archives, `install.sh` and one `checksums.txt`.
   ```bash
   ls -1 dist
   tar -xzf dist/edc_<version>_darwin_arm64.tar.gz -O edc > /dev/null && echo "archive ok"
   ```
5. Check the README demos. If a command output changed in this release, render the tapes again and commit the GIFs.
   ```bash
   ls docs/tape/*.tape
   ```
   Do not render `info`, `listen`, or `top`. Those tapes leave host information on the screen.
6. Open the release PR with the `xm:write` skill in `pr` mode. Do not ask for a confirmation.
   - Base `main`, head `develop`, title `release: v<version>`, English body. Read the last release PR for the shape: `gh pr list --state merged --base main --limit 1`.
   - List the PR numbers, then one bullet for each change that a user sees, then a `Verification:` list.
   - In `Verification`, name the commit SHA that step 3 ran on. After the head of `develop` moves, run step 3 again and put the new SHA in the body (`gh pr edit <n> --body-file <file>`).
7. Wait for the checks of the release PR. Run the wait as a background command with an `until` loop. Do not chain `sleep` in the foreground.
   ```bash
   until out=$(gh pr checks <n> 2>/dev/null) && [ -n "$out" ] && ! echo "$out" | grep -q pending; do sleep 15; done; echo "$out"
   ```
   Right after the PR opens, `gh pr checks` prints nothing because the checks are not registered yet. An empty result is not a pass.
8. If the `reviewer` check fails, read the review. Use the API: `gh api repos/x-mesh/edc/pulls/<n>/reviews`, then `gh api repos/x-mesh/edc/pulls/<n>/reviews/<id> --jq .body`. `gh pr view --json reviews` prints nothing useful for it.
   - The `blocking` section ("차단") stops the merge. The other findings do not. Leave them open.
   - Fix a blocker in a separate branch from `develop` and open a PR into `develop`. Do not commit to the release PR. Wait for its checks, merge it, then run `git-kit pull`.
   - The head of the release PR moves with `develop`. Run steps 3, 6 (body SHA and PR list) and 7 again.
   - The `reviewer` check keeps the old result until the review of the new head appears. Compare `commit_id` of the last review with the head SHA before you trust the result.
   - Other people can push to `develop` while you wait. Read `git log <old-head>..origin/develop` after each wait and add what is new to the body.
9. Merge the release PR with a merge commit. Never squash: the tag must sit on a merge commit of `main`.
   ```bash
   gh pr merge <n> --merge
   ```
10. Tag the merge commit. Do not switch branches. Check that the commit subject is `Merge pull request #<n> from x-mesh/develop`.
    ```bash
    git fetch origin --tags
    git log -1 --format=%s origin/main
    git tag -a v<version> -m "edc v<version>" "$(git rev-parse origin/main)"
    git push origin v<version>
    ```
    Push only the tag. `main` is already on the remote.
11. Watch the release run. `gh run watch` needs the run ID, so read it first.
    The run appears in the list some seconds after the tag push, so wait for it. Find it by the tag: the workflow name is `Release`, the run title is the merge commit subject, and `headBranch` is the tag. An empty run ID makes `gh run watch` fail at once.
    ```bash
    for i in $(seq 1 30); do
      run=$(gh run list --workflow Release --limit 5 --json databaseId,headBranch \
        -q '.[] | select(.headBranch=="v<version>") | .databaseId' | head -1)
      [ -n "$run" ] && break
      sleep 10
    done
    gh run watch "$run" --exit-status --compact
    gh release view v<version> --json assets -q '[.assets[].name]'
    ```
    The run takes about five minutes. Run it as a background command if the tool limit is shorter.
12. Write the release notes. See [Release notes](#release-notes) for the shape.
    ```bash
    gh release edit v<version> --notes-file <path>
    gh release view v<version> --json body -q .body
    ```
13. Verify the published release from the outside.
    ```bash
    curl -fsSL https://github.com/x-mesh/edc/releases/latest/download/install.sh | BINDIR=/tmp/edc-check sh
    /tmp/edc-check/edc version
    ```
14. Verify the update path from the previous version. Install the earlier version, then update. `EDC_VERSION` takes the number without `v`: `0.95.0`, not `v0.95.0`. A `v` makes the download URL `vv0.95.0` and fails with a 404.
    ```bash
    EDC_VERSION=<previous> BINDIR=/tmp/edc-old sh install.sh
    /tmp/edc-old/edc update --check
    ```
15. Check that your own install is still there. Before this fix, `install.sh` deleted `~/.local/bin/edc` whenever `BINDIR` pointed elsewhere, so steps 13 and 14 removed the copy on a macOS host. Keep the installer output whole; a `removed old` line means the side install touched the real one.
    ```bash
    command -v edc && edc version
    ```
    Remove `/tmp/edc-check` and `/tmp/edc-old` after the check. The local `rm` can be a shell function that fails, so use `/bin/rm -rf`.

## Test on Linux

`go test` on macOS skips every `//go:build linux` file. A change in such a file needs a Linux run before the PR.

```bash
GOOS=linux GOARCH=amd64 go test -c -o <scratch>/edc.test ./internal/edc
scp -q <scratch>/edc.test root@jw-server:/tmp/edc.test
ssh root@jw-server '/tmp/edc.test -test.run "<TestNames>" -test.count=1 -test.v; /bin/rm -f /tmp/edc.test'
```

Select the tests with `-test.run`. The binary runs in `/tmp`, so a test that reads `testdata/` fails with `no such file or directory` for that reason. Such a failure is not a defect of the change. Run the same binary built from the parent commit to compare.

## Release notes

The workflow writes only a list of PR titles and a `Full Changelog` link. Replace the body with notes that a user reads before an update.

Write one `##` section for each change that a user sees. Name the command in the heading. Order the sections by the size of the change for a user.

In each section, write what the user gets, then why it changed. For a fix, name the defect and the effect on a host. For a new command, write what it does and the platform that it needs.

State every change that a user must act on: a state schema bump, a renamed flag, a changed default, a platform that loses a feature.

Keep the `Full Changelog` line at the end. Leave out a host name, an IP address, a user name, and an identifier that a user cannot see. Claim a number only when a commit or PR measured it.

Read the `v0.95.0` notes for the shape.

## If the release run fails

Delete the tag, fix the cause, then tag again with the same number. A published release needs `gh release delete` first.

```bash
gh release delete v<version> --yes   # only if the release exists
git push origin :refs/tags/v<version>
git tag -d v<version>
```

Never move a tag that a release already published. Users who installed it get a different binary under the same version.

## Rules

- Do not ask for a confirmation of the release PR, the merge, or the tag push. See [Approval](#approval).
- Never put the tag on a commit of `develop`. It goes on the merge commit of `main`.
- Keep `make check` green. The release workflow runs it again and stops on a failure.
- Do not put a host name, an IP address, or a user name in release notes.
- The workflow uses `${{ github.token }}`. It needs no personal token.
- In zsh the exit codes of a pipe are in `pipestatus`, not `PIPESTATUS`.
