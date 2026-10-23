**English** | [한국어](disk-usage.ko.md) · [Back to README](../README.md)

# Disk usage and known caches

[Disk usage of a directory (Linux and macOS)](#disk-usage-of-a-directory-linux-and-macos) · [Known cache sizes (Linux and macOS)](#known-cache-sizes-linux-and-macos)

![Directory usage browser](media/disk-usage.png)

Screen rendered with synthetic demo data using the current UI renderer.

## Disk usage of a directory (Linux and macOS)

**Fast directory exploration:** `du` scans directories in parallel with 4 workers by default. On macOS, it reads metadata in batches with `getattrlistbulk`. It retains directory records and the largest files instead of one record per file. Scan time still depends on storage, directory structure, and filesystem caches.

`edc du` shows the disk usage under a directory. It counts the allocated blocks, as `du` does. A file with more than one hard link counts once. It changes nothing, and it sends nothing over the network.

```bash
edc du                          # the current directory
edc du /var                     # the largest entries under /var
edc du --top 40 /var            # 40 rows
edc du -B /var                  # show progress and size bars, then exit
edc du --no-browse /var         # the same as -B
edc du --workers 1 /var         # read one directory at a time
edc du --cross-mounts /var      # count the mounts below /var too
edc du --json - /var            # the rows in metrics.rows
```

In a terminal, `edc du` opens a screen. While edc reads, the screen shows the number of directories that it read. Press `q` to stop, and the screen shows what edc read. `Ctrl-C` stops and closes the screen. The screen lists the current directory, with the largest entry first. Press `Enter` or `→` to open a directory. Press `←`, `Backspace`, or `Esc` to go up. `↑`, `↓`, `PgUp`, `PgDn`, `g`, and `G` move the cursor. Press `q` to close the screen, and edc prints the result. The screen does not change or delete a file. In a pipe, with `--json`, or with `NO_COLOR` set, edc prints the result and opens no screen. For the screen, edc keeps the 10,000 largest files. In each directory, the other files show as one `(N other files)` row.

In a terminal, `--no-browse` or `-B` shows a spinner during the scan. When the scan ends, edc prints size bars and exits.

These options preserve color and disable directory navigation. With `NO_COLOR`, a pipe, or `--json`, edc uses static output.

The output starts with one `du.scan` result. It shows the total size, the number of entries, the time, and the number of workers. The rows follow. edc opens the largest entry first. When edc opens a directory, the entries in that directory become candidates. A directory row ends with `/`. The last row holds the entries that edc does not show. After the screen, the rows fit the terminal height. In a pipe, edc shows 20 rows. `--top` sets the rows of this printed result, not of the screen.

By default, edc counts one file system only. It does not go into a mount below the path. It lists each skipped mount with its file system type and the used space from `statfs`. A bind mount or an overlay mount shares a file system with another path, so its used space shows as `-`. edc counts a directory that is bind mounted onto itself, because its files have no other location.

edc uses 4 workers by default. Use `--workers` or `-w` to change the number. edc keeps one record for each directory and for the largest files only. The memory use grows with the number of directories, not with the number of files.

edc stops at the time limit, 60 seconds by default. Then the result is `warn`, `metrics.partial` is `true`, and the size includes only the directories that edc read. If the time limit or `q` stops edc, the last lines of the text output also say that the result is partial. After the time limit, these lines show the command with a longer `--timeout`. If edc cannot read a directory, the result is `warn`, and `-v` lists the directories. Run it as root to read all directories. On macOS, privacy permissions can also block access.

After the rows, edc shows the file system of the path. The line shows the mount point, the type, and the used and total space, as `df` shows them. On Linux, edc also shows the deleted files that a process still holds open on that file system. `df` counts these files, but `du` cannot see them. Each row shows the size, the process, and the old path. Restart the process, or make it open the file again, to free the space. The value is for the whole file system, not only for the path. Without root, edc cannot read the files of all processes, so the value starts with `≥`. If open deleted files take 10% of the file system or more, the result is `warn`.

On Linux, if the path is the mount point of a whole file system, edc also compares the numbers. `not found` is the used space minus the counted size and the open deleted files. Files under a mount point and file system metadata can use this space. edc compares only if it read all directories and all processes, and the time limit did not stop it. A bind mount or a subdirectory shows no comparison. The JSON holds these values in `metrics.filesystem`, `metrics.deleted_open`, and `metrics.not_found_bytes`.

On macOS, edc reads metadata in batches. It follows APFS firmlinks and uses the mount table for boundaries.

APFS volumes can share storage, and snapshots can retain blocks. The measured size does not predict the space that deletion will free. macOS shows no open deleted files or `not found` comparison.

To find the directories that grew, save two runs with `--json` and compare them with `edc report diff`. `metrics.largest_dirs` holds the 200 largest directories at any depth. `report diff` matches them by path and shows up to 10 directories with the largest change. If a subdirectory holds 90% or more of the change of its parent, edc shows the subdirectory and not the parent. If a directory is in only one list, its other value shows as `≤` the smallest size in that list. edc compares only two scans of the same path that both reached the end. A file with hard links in two directories counts in the directory that edc reads first, so its size can move between those directories from one run to the next.

On Linux, if `docker` is on the `PATH`, edc also asks docker for its containers with `docker ps` and `docker inspect`. The format reads only the name, the log path, the mounts, and the layer path of each container. edc then names the container of a log directory, a writable layer, the root file system of a running container, a volume, and a bind mount source, for example `volume pgdata of db-1`. The JSON holds the name in `label`. With the containerd image store, a stopped container has no writable layer name, because its layer is not mounted. If docker does not answer in 3 seconds, edc shows no names.

## Known cache sizes (Linux and macOS)

**Inspection only: `clean` never deletes files or runs the cleanup commands it displays.** Its name refers to finding cleanup candidates. `-v` can show commands for you to review and run separately.

`edc clean` shows the disk usage of known development caches. It reads only. It does not delete or change a file.


`-w N` / `--workers N` sets the parallel directories within each size scan, including build folders found with `--project`. The default is 4; `defaults.du.workers` overrides it, and an explicit CLI value wins. Candidate discovery and sections remain sequential. Independent worktree git inspections run concurrently, capped at min(workers, 4); shared repository lists are queried once. More workers therefore do not multiply the speed of the entire command. Progress output shows the selected worker count.

```bash
edc clean                       # measure the known caches
edc clean -v                    # show every candidate, more groups, reasons, and cleanup commands
edc clean --timeout 2m          # give edc more time
edc clean --project ~/work      # also search ~/work for build folders
edc clean --project ~/a --project ~/b   # repeat the option for more roots
edc clean --json -              # the candidates in metrics.candidates
```

edc finds the location of each candidate in one of three ways. For `go-build`, `go-mod`, `npm`, `pnpm`, `uv`, `homebrew`, and `pip`, edc asks the tool that owns the cache. If the tool is not on the `PATH`, the status is `tool_absent`.

For the other candidates, edc reads the environment variables in the order of the table. The first variable that is set decides the path. If no variable is set, edc uses the default path. `located_by` shows which source edc used.

If the first variable that is set is empty or not an absolute path, the status is `unknown_location` with the reason `invalid_env`. edc does not try the next variable or the default path.

edc does not read tool configuration files such as `bunfig.toml`, `.npmrc`, and `.puppeteerrc`. If a file moved a cache, set the environment variable.

| Candidate | How edc finds it |
| --- | --- |
| `go-build`, `go-mod` | `go env GOCACHE GOMODCACHE` |
| `npm` | `npm config get cache` |
| `pnpm` | `pnpm store path` |
| `uv` | `uv cache dir` |
| `homebrew` | `brew --cache` |
| `pip` | `pip3 cache dir` |
| `xcode-derived-data` (macOS) | `~/Library/Developer/Xcode/DerivedData` |
| `swiftpm` (macOS) | `~/Library/Caches/org.swift.swiftpm` |
| `cargo-registry`, `cargo-git` | `$CARGO_HOME/registry`, `$CARGO_HOME/git`, then `~/.cargo/registry`, `~/.cargo/git` |
| `zig` | `$ZIG_GLOBAL_CACHE_DIR`, `$XDG_CACHE_HOME/zig`, then `~/.cache/zig` |
| `bun` | `$BUN_INSTALL_CACHE_DIR`, `$BUN_INSTALL/install/cache`, `$XDG_CACHE_HOME/.bun/install/cache`, then `~/.bun/install/cache` |
| `playwright` | `$PLAYWRIGHT_BROWSERS_PATH`, then `~/Library/Caches/ms-playwright` (macOS) or `$XDG_CACHE_HOME/ms-playwright`, `~/.cache/ms-playwright` (Linux) |
| `puppeteer` | `$PUPPETEER_CACHE_DIR`, then `~/.cache/puppeteer` |
| `node-gyp` | `$npm_config_devdir`, then `~/Library/Caches/node-gyp` (macOS) or `$XDG_CACHE_HOME/node-gyp`, `~/.cache/node-gyp` (Linux) |
| `electron` (macOS) | `$electron_config_cache`, then `~/Library/Caches/electron` |
| `xcode-device-support` (macOS) | The `*DeviceSupport` folders in `~/Library/Developer/Xcode` |
| `coresimulator-caches` (macOS) | `~/Library/Developer/CoreSimulator/Caches` |
| `app-updates` (macOS) | The `*.ShipIt` folders in `~/Library/Caches` |
| `app-logs` (macOS) | `~/Library/Logs` |
| `huggingface` | `$HF_HUB_CACHE`, `$HUGGINGFACE_HUB_CACHE`, `$HF_HOME/hub`, `$XDG_CACHE_HOME/huggingface/hub`, then `~/.cache/huggingface/hub` |
| `rustup-toolchains` | `$RUSTUP_HOME/toolchains`, then `~/.rustup/toolchains` |
| `ollama` (macOS) | `$OLLAMA_MODELS`, then `~/.ollama/models` |
| `journald` (Linux) | `/var/log/journal`. The detail line shows the limits in `journald.conf` and its drop-in files, in the order that systemd applies them. |
| `apt-archives` (Linux) | `/var/cache/apt/archives` |
| `app-caches` (macOS) | The other folders in `~/Library/Caches` |
| `dot-cache` (macOS) | The other folders in `~/.cache` |

On Linux, edc measures the candidates that the table does not mark as macOS. It does not measure the rest of `~/.cache` on Linux.

Each candidate has a priority. P1 holds the tool caches. P2 holds downloaded browsers, Xcode data, app caches, and logs. P3 holds models and toolchains. The text output lists P1 first. In each priority, the largest candidate comes first.

edc reuses the size code of `edc du`. It measures one candidate at a time. It does not go into a mount below a path. A symlink path shows the reported path and the real path, and edc measures the real path. DerivedData shows its folders grouped by app name. `rustup-toolchains` and `app-logs` show their folders by name.

Four candidates measure the first-level items of a folder one by one: `xcode-device-support`, `app-updates`, `app-caches`, and `dot-cache`. The first two measure only the items whose name ends with `DeviceSupport` or `.ShipIt`. If no item has such a name, the status is `absent`.

`app-caches` and `dot-cache` measure all other items. They skip every path that an earlier candidate measures. If an item holds such a path, edc measures only the rest of that item. `excludes` lists the skipped candidates. The result shows one group for each item name.

For these four candidates, edc does not follow a symlink. It skips an item on another device and marks the result `partial`. A hard link counts once for each item that edc measures.

edc does not know what an app keeps in its folders. The candidates `app-caches`, `dot-cache`, `app-updates`, and `app-logs` have `regenerable` set to `false`. Their cost to recreate is `unknown` or `lost`.

edc measures only the cache folder of a tool. For Hugging Face it measures `hub`. For Cargo it measures `registry` and `git`. For Ollama it measures `models`. It never measures a token or a credential file.

edc never calls Xcode Archives, simulator devices, `Application Support`, or `Containers` a cache.

Each candidate has one status.

| Status | Meaning |
| --- | --- |
| `measured` | edc read the whole path. The size is complete. |
| `partial` | The size is a lower bound. A directory is unreadable, a mount was skipped, or the time ended. |
| `absent` | The path does not exist, or no item has the name that the candidate needs. |
| `tool_absent` | The tool is not on the `PATH`. |
| `unknown_location` | The tool failed, timed out, or gave an unexpected answer. An environment variable that is not an absolute path gives this status too. |
| `unreadable` | A permission error or another error blocked the read. |
| `rejected` | The path covers `/`, the home directory, or a parent of it. edc does not measure it. |
| `duplicate` | An earlier candidate has the same real path or holds this path. |
| `not_scanned` | The time ended or you pressed `Ctrl-C` before edc reached the candidate. |

Only `measured` and `partial` have `usage_bytes`. No other status shows `0 B`. A candidate inside another candidate is not added twice to the sum.

The text output shows one line for each candidate that has a size or needs a check. It folds `absent`, `tool_absent`, and `duplicate` into one line for each status. A group list shows 3 items. `-v` shows every candidate and 30 items for each group. `--json` shows all items. The text output writes a path in the home directory with `~`. The JSON holds the full path.

The sum for each file system is usage. The text output shows it at the end. It is not the space that a cleanup frees. APFS clones and snapshots can make the sum larger than the freed space. A hard link counts once in one candidate. A hard link between two candidates counts twice. The pnpm store and the uv cache use hard links. No candidate has a reclaimable value. Only the Docker section shows one, and the engine reports it.

A candidate shows a cleanup command only if the tool documents one. The candidates `bun`, `pip`, `huggingface`, and `rustup-toolchains` show one. The candidates `cargo-registry`, `zig`, `app-caches`, and a few more show none. edc never runs a command.

With `-v`, edc also reads the date of the newest file in each candidate and in each folder group, for example `newest file 2026-10-19`. A cache that no tool has written to for months is a safer candidate than one that changed today. The date is the modification time of a regular file, not the access time. A `≥` before the date means that edc could not read some folders or stopped at a limit, so a newer file can exist. The JSON has `newest_write` and `newest_partial`. edc reads every file once more, so `edc clean -v` takes about as long as the measurement again.

Four more sections follow the candidates. Each section has its own key in `metrics`. A section is not a candidate.

| Key in `metrics` | Content |
| --- | --- |
| `candidates` | The caches in the table above |
| `filesystems` | The usage sum for each file system |
| `project_build_dirs` | The build folders in the roots that you give with `--project` |
| `docker` | The totals that the Docker engine reports |
| `docker_root` | The old Docker data directory that the engine no longer uses (Linux) |
| `system` | The large logs, the snap revisions that are not current, and the buildx cache folders (Linux) |
| `vm_disks` | The disk file of a virtual machine, for example OrbStack |
| `worktrees` | The git-kit worktrees in `~/.gk/worktree` |
| `not_measured` | The types that edc does not measure, and the reason |

edc runs the sections in this order: candidates, build folders, Docker, old Docker data, large logs and snaps, VM disks, and worktrees. They share one time limit.

Only the recreatable build folders join the sum of a file system. The engine totals, the old Docker data, the large logs, the VM disks, and the worktrees never join a sum.

### Progress and timings

In an interactive terminal, clean shows an inline progress board on stderr: a read-only label, total elapsed time, aligned stage timings, and up to three current directory or tool queries. Completed stages stay visible; finishing a stage does not imply every item succeeded. Partial results and failures are identified by the final report. The screen stays in shell scrollback instead of opening an alternate screen.

![Clean progress board rendered with demo data](media/clean.png)

Press `q`, Esc or Ctrl-C to stop collection and keep partial results; `?` toggles the key help. Pasted text is ignored. Ctrl-Z suspends the process; use the shell’s `fg` to resume. The board requires UTF-8 and TTY input/output/stderr. It is designed for 80×24, switches to a compact view below 60 columns or 15 rows, and shows an expansion hint below 40×5. Width calculations preserve CJK, combining characters and emoji clusters. Arabic and mixed-direction text stays in logical order; BiDi shaping depends on the terminal. Mouse navigation is not used.

With `NO_COLOR`, `TERM=dumb`, redirected streams or explicit JSON progress, clean uses plain timestamped logs without terminal controls. START/END retain stage timings, and RUN shows active work every 2 seconds. This plain mode is also the practical choice for screen readers. No spinner animation is used.

```bash
edc clean --project ~/work -w 10
edc clean --progress --project ~/work --json report.json 2>clean-progress.log
edc clean --progress=false
NO_COLOR=1 edc clean --project ~/work
```

Piped output and JSON remain quiet unless `--progress` is explicit. `--redact` also applies to progress targets. `-v` additionally scans for the latest file modification time; that work appears as its own stage. Progress is for the local survey; `--group` uses remote execution output.

#### Local measurement (2026-10-23)

On one macOS host, `clean --project ~/work -w 10 --progress --json report.json` inspected 66 project build folders and 53 worktrees. The sequential baseline already included progress output; the optimized version parallelized only independent worktree git inspections. Each version ran twice. Times include the whole CLI process:

| Version | Run 1 | Run 2 |
| --- | ---: | ---: |
| Sequential worktree inspections | 29.72 s | 28.85 s |
| Bounded parallel worktree inspections | 26.97 s | 26.34 s |
| Worktree stage, sequential | 7.16 s | 7.07 s |
| Worktree stage, parallel | 3.79 s | 3.66 s |

The cache stage still took roughly 13–14 seconds and the project stage roughly 8 seconds. Separately, `du ~/work -w 10` completed approximately 2.01 million entries in 7.52 seconds. This is not an equal-scope speed comparison: clean also surveys caches outside the project root and git-kit worktrees. These are local observations with filesystem-cache and background-load effects, not a speed guarantee. Before adding progress, one clean run took 30.94 seconds; the run-to-run variation does not establish a precise logging overhead.

### Project build folders

`--project DIR` names a root to search. Repeat the option for more roots. Without `--project`, edc does not search, and `project_build_dirs` has the status `no_project_root`. This state is normal.

edc reads the folders of a root to a depth of 6. It does not follow a symlink. It does not enter a mount on another device. It does not enter `.git`, a build folder, or a path that a candidate measures.

edc stops after 300,000 folders or 256 build folders. The section is then `partial` with the reason `project_limit`. A root that edc did not reach has the status `not_scanned`.

A root that does not exist has the status `absent`. A root that is a file has the status `rejected`. edc walks a root once. A root that equals another root, or lies inside it, has the status `duplicate`.

A build folder needs a marker file in the same parent folder.

| Folder | Marker file |
| --- | --- |
| `.zig-cache`, `zig-out` | `build.zig` |
| `target` | `Cargo.toml` |
| `node_modules` | `package.json` |
| `.build` | `Package.swift` |
| `build` | `build.gradle` or `build.gradle.kts` |

A folder with one of these names and no marker file can hold source. edc does not measure it. The root counts it in `unmarked`.

edc then asks git about each build folder. A folder outside a git repository is recreatable when it has a marker file. The `git` field is `none`.

Inside a repository, the folder must have no tracked file. If git tracks a file in it, `regenerable` is `false` with the reason `tracked_files`. A tracked file decides before an ignore rule. The `git` field is `ignored` or `not_ignored`.

If git fails or times out, `regenerable` is `false` with the reason `git_unknown`, and the `git` field is `unknown`. edc does not guess.

```json
{
  "kind": "cargo_target",
  "path": "/Users/me/work/app/target",
  "marker": "Cargo.toml",
  "git": "ignored",
  "regenerable": true,
  "status": "measured",
  "usage_bytes": 4831838208
}
```

A build folder with `regenerable` set to `false` is not in the sum. A build folder that equals the path of a candidate has the status `duplicate`. A build folder inside a candidate has `inside` and is not counted twice.

A build folder inside a worktree has `inside_worktree`. The worktree item shows the number of these folders in `build_dirs`. Without a search, `build_dirs` is `null`.

The text output shows the 10 largest build folders and the number of the rest. `-v` shows all of them. Each root shows the number of folders that edc read and the number of `unmarked` folders.

### Clean on more hosts

`edc clean --group <group>` runs `edc clean --json -` on each host of an `edc remote` inventory group and shows one table. It uses the inventory files and the OpenSSH settings of `edc remote`. `--inventory` sets the file. The `parallel` value of the inventory sets how many hosts run at the same time.

```bash
edc clean --group servers
edc clean --group servers --inventory ./.edc/inventory.yaml --json -
```

Each host must have edc on the PATH of its shell. edc does not install it. A host without edc shows `edc not found`. A host that SSH cannot reach shows `SSH failed` and the first error line.

The table shows the disk use of the root file system, the sum of the candidates, the old Docker data, the large logs, and the largest item. If a host has an older edc that does not report a value, the cell shows `—`. `--json` keeps one row for each host.

`clean` only reads, on each host. `--project` does not combine with `--group`, because each host measures its own folders.

### Docker engine

edc reads the engine with `GET` requests to the API socket. It sends no other method and runs no `prune` command.

edc asks `docker context inspect` for the socket path, so `DOCKER_HOST` and the current context apply. Only a `unix://` endpoint works. Any other endpoint gives `unsupported`.

If `docker` is not on the `PATH`, the status is `tool_absent`. If the socket file does not exist, the status is `not_running`. These two states are normal.

The engine needs API version 1.52 or later. An older engine gives `unsupported` with the reason `api_version`. edc does not compute the totals from the lists.

`types` has one entry each for `images`, `containers`, `build_cache`, and `volumes`. An entry holds `total_bytes`, `reclaimable_bytes`, `total_count`, and `active_count`. The values come from the engine.

If one request fails, only that type has the status `failed`, and the section is `partial`. The volume request can take up to 20 seconds.

`reclaimable_bytes` is the amount that the engine says a cleanup frees. It is the only reclaimable value in the output. The entry shows the cleanup command as text, for example `docker image prune -a`. edc never runs it.

Volumes hold user data. The `volumes` entry has `user_data` set to `true`. It has no `reclaimable_bytes` and no cleanup command.

`docker system df` rounds the sizes and uses units of 1000 bytes. The text output of edc uses units of 1024 bytes. The two numbers differ for this reason. The JSON holds exact bytes.

### Old Docker data directory

On Linux, edc checks `/var/lib/docker`. If the engine uses another data directory, the old directory is a candidate. The engine reports its directory in `docker.root_dir`. This works for an engine older than API 1.52 too.

edc does not decide if you can delete the data. It shows the facts that help you decide. It reads files only and runs no `docker` command.

Each entry in `docker_root.roots` has these facts:

- `usage_bytes` and `dirs`: the size of the directory and of each folder in it.
- `mounted` and `open_files`: whether a mount or an open file is under the directory. If `open_files_complete` is `false`, edc cannot read some processes.
- `last_container_stop` and `newest_volume_write`: the dates of the last activity.
- `containers`: the containers that the directory records, with image, Compose project, and stop time. `in_engine` and `project_in_engine` show if the engine has the same name.
- `overlay`: how the folder `overlay2` splits among owners. `image_bytes`, `container_bytes`, `unused_layer_bytes`, and `unknown_bytes` add up to `total_bytes`. A layer that a layer file names but no image uses is unused. A folder that no metadata names is unknown. `images` lists the largest images with their tags. `shared_bytes` is the part that another image also uses, so deleting this image alone does not free it. `containers` lists the writable layers. `in_engine` shows if the engine has an image with the same tag. edc reads the layer metadata of the classic overlay2 storage. If the metadata is absent, `overlay` is absent too.
- `volumes`: each volume with its size, the date of its newest file, and `used_by`, the containers that mounted it. `in_engine` shows if the engine has a volume with the same name.

The entry has `cleanup`, a `rm -rf --one-file-system` command for the directory. With `-v`, the text output shows it. edc never runs it. The command is empty if a mount or an open file is under the directory. It is also empty if edc cannot read the mount table or all of `/proc`.

Do not use `docker volume rm` for these volumes. The engine does not know the old directory. It removes the volume with the same name in its own directory.

A volume with an empty `used_by` has no container record. It can hold user data. Check it before you delete it.

edc shows the 20 largest volumes. `volumes_omitted` and `omitted_bytes` count the others. Without `-v`, the text output shows unnamed volumes as one line and does not list the containers.

edc reads the container name, image, labels, mounts, and times from `containers/*/config.v2.json`. It does not keep or print environment variables.

If the engine does not report its directory, the status is `unknown_location`. If `docker` is absent or the engine is off, the text output does not show the section, and `docker_root.status` is empty.

### Large logs, snap revisions, and build caches

On Linux, edc lists the files in `/var/log` of 100 MB or more. It skips `/var/log/journal`, which the `journald` candidate measures. It also lists the snap revisions that are not the current one.

edc does not delete a file. It shows the facts that help you decide.

Each entry in `system.large_logs` has the size and the date of the last change. `open_files` is the number of file descriptors that processes hold on the file. `logrotate` is the file with a logrotate rule for the path. edc follows `include` lines that have an absolute path. An empty value means that edc found no rule in the files it read. It does not prove that logrotate skips the file.

With `-v`, the text output shows a command as text. edc never runs it. For a file that a process holds open, the command is `truncate -s 0`. For a closed file, the command is `rm`. If edc cannot read `/proc`, no command appears.

Each entry in `system.snap_revisions` has the name, the revision, and the current revision. The current revision comes from the link `/snap/<name>/current`. The command is `snap remove <name> --revision=<revision>`. If edc cannot read the link, it skips the snap and counts it in `system.snaps_unknown`.

edc also finds the folders that `docker buildx build --cache-to type=local` writes. Such a folder is an OCI image layout. It has the files `oci-layout` and `index.json` and the folder `blobs`. edc checks the folders one and two levels below the home folder. It uses the structure and not the name, and it does not follow links. It stops after 50,000 folders.

Each entry in `system.build_caches` has the size, the date of the newest file, and `open_files`. `docker buildx prune` does not remove these folders. With `-v`, the text output shows `rm -rf <path>`. The command is empty if a process holds a file open or if edc cannot read all of `/proc`. `build_caches_incomplete` is `true` if the search stopped at the limit or at the time limit, or if edc could not measure a folder. In this case, more caches can exist.

edc shows the 10 largest entries of each list. `large_logs_omitted` and `build_caches_omitted` count the others.

### VM disks

On macOS, edc measures the OrbStack data disk in `~/Library/Group Containers/HUAQ24HBR6.dev.orbstack/data`. The file is sparse, so edc measures the allocated blocks and not the logical size.

The disk holds the Docker engine data and the Linux machines. When the engine is OrbStack, `docker.inside` is `orbstack`.

The engine totals and the disk size describe the same data in two ways. edc never adds them together or to a sum. The disk is user data, and `user_data` is `true`.

edc does not measure the disks of Docker Desktop or of other VMs. Linux has no VM disk entry.

### Worktrees

edc reads `~/.gk/worktree`, the folder of git-kit. It looks for a folder with `.git` to a depth of 5 and does not enter it. If the folder does not exist, the status is `absent`. This state is normal.

edc measures the root once and reads the size of each worktree from that tree. `leftover` is the size of the other files in the root.

edc decides the use of a worktree from the state of git. It does not read the modification time of a file. It runs only read commands of git with `GIT_OPTIONAL_LOCKS=0` and `core.fsmonitor=false`. The files in `.git` do not change. edc does not run `git status`, because it reads a changed file through the clean filter of the repository, for example git-lfs. It compares the file stat with the index instead, so a file that was only touched also counts as changed.

Each worktree has one `verdict`. edc picks the first match in this order.

| Verdict | Meaning |
| --- | --- |
| `unlinked` | The git directory of the worktree does not exist |
| `has_changes` | A file is changed or untracked |
| `state_unknown` | A git command failed, so edc cannot tell |
| `locked` | The worktree is locked |
| `not_merged` | The branch has commits that the base does not have |
| `base_unknown` | edc found no base branch to compare |
| `merged_clean` | The base has the branch, and no file changed |

The base is the `gk-parent` of the branch in the git configuration. If it does not exist, the base is `origin/HEAD`. edc never assumes `main`.

A `merged_clean` verdict needs every git command to succeed. edc cleans nothing. The output names `git-kit worktree cleanup` as the command to run.

### What edc does not measure

The output always lists the types that edc does not measure. Use `-v` to see the reason for each type.

- VM disks other than OrbStack, and databases
- Worktrees outside `~/.gk/worktree`
- Trash and downloads
- Simulator devices
- SDKs
- App data
- Caches of other tools whose location edc did not verify

The time limit is 60 seconds by default, and all candidates and sections share it. edc measures the P1 candidates first and the two remainder candidates last. A section that edc did not start has the status `not_scanned`. If the time ends, the result is `warn`, and `next` shows a command with a longer limit. `Ctrl-C` stops the run and keeps the results so far.

