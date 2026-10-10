**English** | [한국어](README.ko.md)

# edc

`edc` is short for **everyday carry**. An everyday carry is the small kit you keep in a pocket and reach for first. `edc` is that kit for SE and SRE work in a terminal.

An incident starts with one question: is the fault here, in the network, or at the far end? `edc` answers it with one command. It runs DNS, TCP, TLS, HTTP, route, ping, interface, and socket probes in one pass, and every probe prints the same result format. It also reports host resources and host information on Linux and macOS, and it measures network responsiveness (RPM) and throughput: macOS runs `networkQuality`, and Linux runs a built-in test that follows the IETF responsiveness draft.

Diagnostic and observation commands are read-only. `watch fs` can execute commands explicitly configured with `--exec` or `--rules`. Observation alone finds the fault and stops there. It runs no DNS flush, no interface reset, and no firewall change, so it stays safe on a production host.

Only the `host changes` group changes a host: `edc route`, `edc disk`, and `edc change`. Each change needs a subcommand that you type, root, and a confirmation. `edc` does not add `sudo` itself.

![edc doctor https://example.com runs nine probes in order and prints a 9 pass summary](docs/media/doctor.gif)

The whole run takes about three seconds. Each line keeps the probe name, the target, and the result in the same columns, so you read down one column to find the failure.

After DNS, TCP, TLS, or HTTP warnings and failures, `edc doctor` shows manual command templates in investigation order.

Independent checks retain their recorded outcomes. The order does not establish a cause, and JSON reports contain no suggestions.

Replace the template placeholders with your target. Run a suggested command only when you need that check.

The source of each demo is a `.tape` file under [`docs/tape/`](docs/tape). To build one again, run `vhs docs/tape/doctor.tape`.

## Install

Install the latest release with the script. It reads the operating system and the architecture, checks the SHA-256, and installs the binary.

```bash
curl -fsSL https://raw.githubusercontent.com/x-mesh/edc/main/install.sh | sh
```

The script installs `edc` in `/usr/local/bin` on Linux and in `~/.local/bin` on macOS.

On Linux, `trace` and `capture` need root. The `sudo` path includes `/usr/local/bin`, but it often excludes `~/.local/bin`.

If a non-root user cannot write there, the script uses `sudo`. If root cannot write there on Linux, the script uses `~/.local/bin`.

Set `BINDIR` to use another directory. Set `EDC_VERSION` to install an earlier version.

```bash
curl -fsSL https://raw.githubusercontent.com/x-mesh/edc/main/install.sh | BINDIR="$HOME/.local/bin" sh
curl -fsSL https://raw.githubusercontent.com/x-mesh/edc/main/install.sh | EDC_VERSION=0.1.0 sh
```

Earlier versions of the script installed `edc` in `~/.local/bin` also on Linux. Ubuntu puts `~/.local/bin` before `/usr/local/bin` in `PATH`, so the shell runs that old copy. If the old copy is `edc`, the script removes it. The script checks the home of the user who runs it and, with `sudo`, the home of the user who ran `sudo`. Remove copies in other homes by hand, for example with `sudo rm /home/<user>/.local/bin/edc`.

If the install directory is not on `PATH`, the script finds your shell and prints the commands that add the directory. Set `EDC_MODIFY_PATH=1` to let the script add the line to the startup file of your shell, for example `~/.zshrc` or `~/.bashrc`. The script adds the line only once. The current shell does not get the new `PATH`. Open a new shell, or run the command that the script prints.

```bash
curl -fsSL https://raw.githubusercontent.com/x-mesh/edc/main/install.sh | EDC_MODIFY_PATH=1 sh
```

A release holds binaries for Linux and macOS on `amd64` and `arm64`.

## Update

`edc update` reads the latest release, checks the SHA-256, and replaces the running binary.

```bash
edc update           # confirm, then replace
edc update --check   # print the two versions only
edc update --yes     # skip the confirmation
```

`edc` writes the new file next to the old one and renames it. A failed download leaves the earlier binary in place. If you cannot write to the directory, `edc` uses `sudo` to copy and rename the new file, as the install script does. `edc` downloads and checks the file as your user. The confirmation shows `privilege sudo`, and `sudo` asks for your password once if it needs one. If `sudo` is not available or fails, `edc` stops with exit code `3` before it downloads anything.

## Build

`edc` needs Go 1.25 or later. The live screens use these dependencies.

- `charm.land/bubbletea/v2`
- `charm.land/bubbles/v2`
- `charm.land/lipgloss/v2`

```bash
make build VERSION=0.1.0-dev
./bin/edc version
```

Install `edc` in `~/.local/bin`.

```bash
make install VERSION=0.1.0-dev
~/.local/bin/edc version
```

Set `PREFIX` or `BINDIR` to use another install path.

```bash
make install PREFIX=/usr/local
```

## Language

`edc` prints English by default. It also carries Korean and Japanese.

Set the language in the config file. On Linux and macOS, `edc` reads `$XDG_CONFIG_HOME/edc/config.toml` when set, or `~/.config/edc/config.toml` by default.

```toml
lang = "ko"
```

If `config.toml` is absent, Linux still reads `~/.config/edc/config.yaml`, and macOS still reads `~/Library/Application Support/edc/config.yaml`. `edc setup` saves a new TOML file and leaves the YAML file intact.

Set `EDC_LANG` to change the language for one run. It wins over the config file.

```bash
EDC_LANG=ja edc where
```

`edc` accepts `en`, `ko`, and `ja`. It reads a locale name such as `ko_KR.UTF-8` and keeps the language part. An unknown value falls back to English, and so does a message that a language misses.

## Command defaults

The same config file can hold repeat-safe command defaults. Precedence is built-in default, config, then an explicit CLI option. Invalid keys, types, or ranges stop the command with exit code `2` instead of being ignored.

Run `edc setup` in a terminal to create or update the file. The wizard configures one section at a time, keeps existing values on Enter, removes an optional value with `!clear`, previews the complete config, and asks before an atomic mode `0600` save. The config directory is mode `0700`; cancel returns exit code `4`.

The following TOML example applies to both Linux and macOS.

```toml
lang = "en"

[defaults.common]
timeout = "15s"
verbose = false
redact = false

[defaults.doctor]
profile = "default"

[defaults.tls]
min_days = 14

[defaults.capture]
duration = "15s"
count = 500

[defaults.remote]
connect_timeout = "10s"
output_limit = 65536
parallel = 0

[defaults.update]
timeout = "60s"

[defaults.quality]
timeout = "30s"
server = ""

[defaults.log]
stream = "both"
output = ""
command_display = "full"
max_size_mb = 10
keep_files = 3
restart = "never"
max_restarts = 3
restart_delay = "5s"
timeout = "0s"
kill_after = "5s"
```

Command-specific values override `defaults.common`. Positional targets, URLs, hosts, and remote groups are never stored, nor are action options such as `yes`, `force`, `dry-run`, `list`, and `check`. Persisted remote inventory and recipe paths must be absolute. Empty path values disable that default.

The setup wizard recommends automatic files for `edc log`.

An empty output path selects one file per run. Custom output paths require an existing parent directory.

The command also creates the parent for the legacy recommended `edc.log` path.

## Quick start

```bash
# live host resource dashboard (press q to quit)
./bin/edc top
# the earlier table output
./bin/edc top --interval 2s --count 10
# one JSON line for each sample
./bin/edc top --count 5 --json -

# Claude Code and Codex token use and account limits
./bin/edc ai

# system, network, and disk information, with the public IP
./bin/edc info
# skip the ipinfo.io request
./bin/edc info --public=false

# default diagnosis
./bin/edc doctor https://example.com

# save a redacted machine-readable report (file mode 0600)
./bin/edc doctor --redact --json report.json https://example.com
./bin/edc report show report.json
# compare two reports (exit 1 if one probe gets worse)
./bin/edc report diff before.json after.json

# full diagnosis with bandwidth and responsiveness
./bin/edc doctor --profile full --timeout 60s example.com

# single probes
./bin/edc dns lookup example.com
./bin/edc dns compare example.com
./bin/edc tcp check example.com:443
./bin/edc tls check example.com:443
./bin/edc tls check --min-days 14 example.com:443
./bin/edc http check https://example.com
./bin/edc http check --expect-status 200 https://example.com
./bin/edc net route example.com
./bin/edc net ping example.com
./bin/edc net trace example.com
./bin/edc net interfaces
./bin/edc listen              # open ports, --unix or --all adds unix sockets
./bin/edc listen --watch -i 0.5 --duration 10s
./bin/edc quality --timeout 60s
./bin/edc quality --server https://example.com/.well-known/nq   # responsiveness config URL

# repeat a page check; omit --duration to run until Ctrl-C
./bin/edc watch http -i 0.1 --duration 10s https://example.com

# which region is near, and what shape is this network
./bin/edc where
./bin/edc where --provider aws --count 5

# shell completion
source <(./bin/edc completion zsh)

# update to the latest release
./bin/edc update --check
```

The common options are `--timeout`, `--json <path|->`, `--verbose`, and `--redact`. Go `flag` rules put an option before the target.

`edc info` asks ipinfo.io for the public IP by default. The request stops after 3 seconds and the line disappears. Use `--public=false` to skip the request, `--timeout` to change the limit, and `-v` to print the cause of a failure.

`edc info` also shows memory status, a process snapshot, and diagnostic support.

The default output groups related values on one line. Use `-v` for kernel details, the memory calculation basis, and shared-page notes.

On Linux, used memory is `MemTotal - MemAvailable`. Cache counters include `Cached`, `Buffers`, and reclaimable slab when present.

On macOS, available memory is an estimate from free and inactive Mach pages. File-backed memory and physical compression appear separately.

Memory detail counters can overlap. They do not measure memory pressure and must not be added together.

The process snapshot shows observed process and thread counts, plus the three processes with the highest RSS. Thread counts show coverage.

Diagnostic support tests process I/O access for the current process and PSI availability. Access to other PIDs can differ.

The Linux eBPF check reads kernel BTF and effective capabilities. It does not load or attach programs.

`prerequisites met` does not guarantee attachment. Unsupported features, absent permissions, and failed checks show their reasons.

If memory or process collection fails, the command shows the error and returns exit code `1`.

On Linux, `edc info` adds `Network Limits`: conntrack occupancy, the local port range and reserved ports, accept/SYN backlog limits, socket buffer ceilings, receive backlog/budgets, neighbor limits, forwarding, and `rp_filter`. Use `-v` for TCP buffer settings, sysctl names, and reasons for unavailable values. Missing or inaccessible values are `unavailable`; these Linux limits are `unsupported` on macOS. Configuration alone does not establish a connection failure.

### Name lookup

![edc dns lookup example.com prints the address list and edc dns config prints the resolver setup, both as PASS](docs/media/dns.gif)

Terminal and JSON output show actual IP addresses by default. Add `--redact` to hide them as `<ip:...>`.

`edc dns compare example.com` compares the system resolver with `1.1.1.1`. Repeat `--resolver IP[:port]` to choose other DNS servers. It compares A, AAAA, CNAME, and response status; TTL is shown for explicit resolvers but not used to mark a mismatch. A different answer is WARN, and a failed system lookup is FAIL.

### Connection check

![edc tcp check connects to example.com:443 and then fails on a closed port with a timeout phase](docs/media/tcp.gif)

A failed probe shows the phase and the cause in an ERROR block. It returns exit code `1`.

`edc watch http -i 0.1 https://example.com` checks the page until Ctrl-C.

`-i` accepts decimal seconds (minimum `0.1`) or a duration such as `100ms`. `--duration 1m` sets the observation duration.

Each sample shows HTTP status, body bytes read (up to 10 MiB), elapsed time, and available DNS/TCP/TLS/TTFB times.

If the resolved IP set changes, the output shows the new set.

For completed samples, the final summary shows min/avg/p95/max latency and the longest continuous failure.

`--json <path|->` emits JSON Lines with a final summary. The summary adds `observation_status` (`observed` or `no_samples`) and `stop_reason` (`duration` or `cancelled`).

Without completed samples, the text summary states the stop reason and omits latency statistics. The target health remains unknown.

Without completed samples, duration expiry returns exit code `2`, and cancellation returns `4`.

With completed samples, a failed sample returns exit code `1`. Otherwise, the exit code is `0`, including after cancellation.

`edc listen --watch` prints the current listeners once, then reports socket creation, closure, or process changes. Change lines use reverse video in a terminal. It accepts the same interval and duration options. The `--json` option emits a snapshot, events, and a summary as JSON Lines.

The SCOPE column and watch events identify the socket's bound address: loopback, all interfaces, specific address, Unix, or unknown.

For wildcard addresses, all interfaces refers to the observed address family. The scope does not establish external access, firewall rules, or IPv6 dual-stack behavior.

On macOS, lsof provides no Unix socket state. The Unix socket list is approximate.

### File watching and actions (Linux and macOS)

`edc watch fs` observes changes directly below the current directory by default. Use `--recursive` for subdirectories, including newly created or moved-in directories. Existing files do not produce startup create events. Directory symlinks are not followed.

```bash
edc watch fs
edc watch fs ./src --recursive
edc watch fs --event create --match text.txt --exec 'git-kit pull'
edc watch fs ./src --recursive --event modify --match '**/*.go' --exec 'go test ./...'
edc watch fs --rules docs/examples/watch.yaml --dry-run
edc watch fs --duration 1m --json events.jsonl
```

Events are `create`, `modify`, `remove`, and `rename`. A rename reports the old path; the new name can produce a create event inside the watched scope. Reads/access and metadata-only changes are excluded. `--event` accepts a comma-separated list. Globs are relative to the watch root: `*.go` matches direct children, and `**/*.go` matches any depth. Watching those deeper paths also requires `--recursive`.

By default, `edc` excludes `.git`, `node_modules`, `.venv`, `venv`, `__pycache__`, `.mypy_cache`, `.pytest_cache`, `.ruff_cache`, `.tox`, `.next`, and `.gradle` at any depth. It does not exclude `build` or `dist`, because you can watch build output. Repeat `--exclude 'build/**'` to add exclusions. To watch the default directories too, use `--no-default-exclude`. Your `--exclude` patterns still apply.

On Linux, with `--recursive`, `edc` adds a watch to each directory before its first output. A large tree takes time. If stderr is a terminal, `edc` shows a notice during this step.

On macOS, `watch fs` uses FSEvents. One stream watches the whole tree, so the number of files does not use file descriptors. FSEvents collects changes for up to 50ms before it sends them. If macOS drops events, `watch fs` scans that directory again and reports the differences as `create`, `modify`, and `remove`. A move then shows as a `remove` and a `create`. One scan reads at most 200,000 entries. A line on stderr tells you about each scan, and it also tells you if the scan stopped at the limit. FSEvents also reports changes in excluded directories, and `edc` discards them.

If stdout is a terminal and `--json` is not set, the last line of the screen shows statistics. It shows the elapsed time, the number of each event type, the events per second over the last 10 seconds, the actions and failures when rules are set, and the time since the last event. The line updates each second. If the terminal is narrow, the line drops the time since the last event first, then the events per second. When the watch stops, the line goes away and the summary line stays. A pipe, a file, and JSON output do not get this line.

The JSON output file and regular files connected to stdout are excluded to avoid output feedback. `--event` and `--match` filter both event output and rule actions. There is no default action.

Rules use one strict YAML document. With no `directory`, the current working directory is watched. An explicit `directory` is relative to the rules file; a CLI directory overrides it. A rule's `cwd` is relative to the watch root and defaults to that root.

```yaml
recursive: true
exclude: [build/**]
rules:
  - name: pull-on-trigger
    events: [create]
    match: text.txt
    command: [git-kit, pull]
    debounce: 200ms
    timeout: 30s
  - name: test-on-go-change
    events: [create, modify]
    match: "**/*.go"
    command: [go, test, ./...]
    debounce: 500ms
```

```bash
edc watch fs --rules watch.yaml
```

`--exec` uses `/bin/sh -c`; YAML `command` executes argv directly. Shell aliases are not loaded. File paths are not interpolated into commands. Actions receive `EDC_WATCH_ROOT` (absolute), `EDC_WATCH_PATH` (relative), `EDC_WATCH_EVENT`, and `EDC_WATCH_RULE`. Quote shell variables such as `"$EDC_WATCH_PATH"`.

Actions run serially across all rules. The default debounce is 200ms, coalescing each rule's events to its latest event. Events during an action are coalesced into one pending action per rule and run after the active action ends. `--dry-run` reports matches without execution. Actions that change matching files can trigger themselves again; narrow the match or exclude generated paths.

Actions receive no stdin. Their default timeout is 30s. `--debounce` and `--timeout` supply defaults for rules without their own values. Combined stdout/stderr is capped at 64 KiB. Failures and timeouts do not stop watching, but produce final exit code `1`. `Ctrl-C` or the watch duration ends the active action's process group and discards pending actions. A normal observation with no events exits `0`; option/output errors exit `2`. Removing or renaming the root, watcher errors, and event overflow stop observation with an error.

JSON Lines uses `ready`, `event`, `action_start`, `action_result`, and `summary`. Child output stays inside `action_result.output`. This watches local filesystems; save operations can coalesce or generate multiple events. It is not a filesystem audit log.

### Route and interfaces

![edc net interfaces, edc net route example.com, and edc net ping example.com each print PASS and one result line](docs/media/net.gif)

## Where am I

`edc where` answers two questions at once: which cloud region is near this host, and what shape the network has.

```bash
./bin/edc where
./bin/edc where --provider aws        # one provider only
./bin/edc where --count 5 -v          # more samples, every provider row
```

`edc` opens a TCP handshake to a public endpoint of each region and closes it. It sends no request and reads no body, so the number holds the round trip and nothing else. `edc` resolves each name once and connects to that address, which keeps the DNS time out of the distance value.

The table groups the endpoints by city and keeps the fastest provider of each city. Use `-v` to see every provider.

| provider | endpoint |
|---|---|
| `aws` | `s3.<region>.amazonaws.com` |
| `gcp` | `storage.<region>.rep.googleapis.com` |

Azure is absent. Its public addresses that carry a region name do not terminate in that region, so the numbers do not follow distance.

`edc where` also reports the public IP with its ASN, the Cloudflare PoP that anycast picks, and the shape of the local network:

- behind NAT, or a public address on the interface
- behind carrier-grade NAT, which `100.64.0.0/10` shows
- through a tunnel, which the default route interface shows

In a terminal, `edc` shows a progress line with the number of regions it has reached. Press `q` to cancel.

The screen keeps the addresses as they are by default. Add `--redact` to hide them in screen or JSON output.

![edc where shows the public IP with its ASN and Cloudflare PoP, the route, the NAT shape, and the near regions in order of round trip time](docs/media/where.gif)

The demo masks the public IP as `222.XXX.XXX.XXX` and replaces the route addresses with private example addresses.

`edc` states only what it confirms. It does not guess the line type from the jitter. The jitter column holds the number, and the reading stays with you.

## Probe thresholds

`edc tls check` gives a warning when the certificate expires in less than 30 days. Set `--min-days` to make an earlier expiry a failure.

`edc http check` gives a warning for a 4xx response and a failure for a 5xx response. Set `--expect-status` to accept one status code only. A different code is a failure.

If the target has no scheme, `edc http check` adds `http://`. `edc http check naver.com` requests `http://naver.com` and follows the redirect. The `url` metric holds the address that `edc` requested. Write `https://` to check a TLS endpoint directly.

```bash
./bin/edc tls check --min-days 14 example.com:443
./bin/edc http check --expect-status 200 https://example.com/health
```

A failure returns exit code `1`. Use these options in cron to get a synthetic check.

![edc tls check passes and then --min-days 90 fails because the certificate expiry is under the threshold](docs/media/tls.gif)

![edc http check passes with HTTP 200 and then --expect-status 404 fails on the status mismatch](docs/media/http.gif)

The demo shows one pass and one threshold failure for each command.

## Report diff

`edc report` views and compares **edc diagnostic results** saved with `--json`. It supports `doctor` and other edc diagnostic commands that use the same report format. General JSON files and comparison JSON from `report diff` are unsupported. `edc log` saves command output as text separately. `edc report list [directory]` lists valid JSON reports in the specified directory (the current directory by default), newest run first, with the run time and PASS/WARN/FAIL/SKIP counts. It does not search subdirectories.

```bash
./bin/edc doctor --json report.json https://example.com
./bin/edc report list
./bin/edc report show report.json
```

Without paths, `edc report show` and `edc report diff` let you choose from up to 20 recent reports or enter another JSON file path. If no report is found, they show an example of how to save one.

`edc report diff` compares two JSON reports by probe name. It shows the status change and the scalar metric differences of each probe.

```bash
./bin/edc doctor --json before.json https://example.com
./bin/edc doctor --json after.json https://example.com
./bin/edc report diff before.json after.json
./bin/edc report diff --json diff.json before.json after.json
```

Reports require schema version `1.0`, tool name `edc`, a nonblank tool version, a run ID, and a valid nonzero run start time.

Reports require `results` and a summary object. Empty results can be `null` or `[]`. Unknown extension fields are accepted.

Each result requires a probe name and a `pass`, `warn`, `fail`, or `skip` status. Durations and summary counts must be nonnegative.

Summary counts must match the results. Invalid report input returns exit code `2`.

`STATUS SAME` and `STATUS CHANGED` compare statuses only. The JSON `same` and `changed` fields retain this rule.

The output marks a probe as `WORSE` for pass-to-warn, pass-to-fail, or warn-to-fail changes. If one probe gets worse, the exit code is `1`.

Scalar metrics present in both reports and probe durations have separate deltas. Arrays, objects, and added or removed metric keys are excluded.

An absent delta does not establish equal measurements. The collapsed viewer shows the scalar and duration delta count.

The header shows both target URLs, target hosts, and collection hostnames. Different identities produce a notice and do not block the comparison.

Missing identities appear as unavailable. JSON diff sides can include the optional `target_url` and `target_host` fields beside the collection `hostname`.

## Report viewer

If stdin and stdout are terminals, `edc report show` and `edc report diff` open a full screen viewer.

| key | action |
|---|---|
| `f` | change the filter |
| `e` | show or hide the details |
| `↑` `↓` `PgUp` `PgDn` | scroll |
| `q` | quit |

`edc report show` filters by 전체, 실패와 경고, then 실패만. `edc report diff` filters by all entries, changed statuses, then worse statuses.

The viewer leaves no output on the screen. The exit code stays the same. A pipe, a file, or `--json` gets the earlier output.

![the edc report show viewer changes the filter with f and shows the details with e, then edc report diff shows the duration_ms difference of each probe](docs/media/report.gif)

The demo opens the viewer, changes the filter, shows the details, and then compares two reports.

## Top thresholds

`edc top` uses two colors. Yellow is a warning. Red is a risk. A normal value keeps the terminal color, so a color marks only a value that needs attention.

The table colors load, `usr%`, `sys%`, `i/o`, and `mem_%`. The dashboard colors these values and also `hot core`, `await`, `err`, `drop`, and the pressure values.

The load thresholds follow the core count of the host.

| value | warning | risk |
|---|---|---|
| load | 0.7 × cores | 1.0 × cores |
| usr%, sys% | 70 | 90 |
| i/o | 10 | 25 |
| mem_% | 90 | 95 |
| await | 20 ms | 50 ms |
| err, drop | 1/s | 50/s |
| psi | 10 | 25 |
| hot core | 90 | — |
| process READ, WRITE | 10 MB/s | 50 MB/s |

`hot core` shows the usage of one core, so it gets a warning at 90 and no risk level. A host with many cores keeps room when one core is full.

The process panel colors `READ` and `WRITE` with the thresholds above. It colors `iowait` in the `STATE` column yellow. It colors the CPU total of a process group red at 80%, the level of one busy core.

The dashboard gives no color to `iops`, `busy%`, `swap/s`, `steal%`, `blocked`, `queue`, the byte, packet, and TCP rates, and the `signal` column. These values have no threshold, or they show the level without a color. Aggregate `busy%` goes above 100 on a host with more than one busy disk, so a fixed threshold gives a wrong signal. The `cores` bar shows the level with `.`, `:`, `*`, and `#`.

To remove the colors, set `NO_COLOR`. A pipe or a file gets no colors.

`edc info` draws a bar for each disk. The bar uses the same thresholds as `mem_%`. One block is 5 percent. The bar shows the level without color, so a pipe keeps the information.

`edc info` counts the disk usage as the total minus the available space. On macOS, several APFS volumes share one container, so the `Used` column of a single volume misses the space that the other volumes take.

`edc info` lists only a file system that uses a storage device. It hides `tmpfs`, `overlay`, and the read only images that `snap` mounts. These use memory, or they count the same disk a second time. A loop device stays in the list, so a disk image that you mount yourself shows.

On macOS, `edc` reads the memory usage from the Mach `host_statistics64` call. It subtracts the free, speculative, and inactive pages. This matches `MemAvailable` on Linux. The `PhysMem` line of `top` includes the cache, so it stays above 97 percent.

## Top dashboard

If stdin and stdout are terminals, `edc top` opens a full-screen dashboard. The dashboard needs no `--count` limit.

| key | action |
|---|---|
| `q` | quit |
| `p` | pause and resume |
| `+` | make the interval longer |
| `-` | make the interval shorter |
| `1`, `c`, `m`, `d`, `n` | switch to all, CPU, memory, disk, or network columns |
| `s` | switch to pressure columns |
| `v` | Return to the box screen. |
| `f` | Open the signal view. Press again to select a process candidate. If a filter is active, open the process view. |
| `Tab` | Switch between history and process selection. |
| `?` | Open help. Use arrows to scroll. Press `Esc` to return. |
| `/` | type a process filter: a command name or a PID, comma-separated |
| `Esc` | Close help or process selection. Otherwise, clear the process filter. |
| `↑`, `↓`, `PgUp`, `PgDn`, `End` | select an earlier row, move one screen, or return to the live row |
| `Enter` | Show time details. If process selection is active, focus the selected PID. |
| `h` | show the load, CPU, iowait, and memory peaks from the last 60 seconds, each with its time |
| `e` | Open the event list. Use arrows to select an event. Press `Enter` to go to its start. Press `e` or `Esc` to return. |
| `[`, `]` | Go to the start of the previous or next event. |

The `signal` column shows the highest-priority host warning and the number of other warnings.

On Linux, the `signal` column shows `blocked N` when `i/o` is at the warning level and 4 or more tasks wait for I/O. macOS has no `i/o` value, so it shows `blocked N` when 4 or more processes are in the `U` state. On macOS, the `signal` column also shows `mem pressure warn` or `mem pressure critical` when the kernel raises the memory pressure level. A process group also gets a warning when its CPU total is 80% or more, or its I/O total is 50 MB/s or more. The warning shows the name and the process count, for example `gm (200) 100%`. Only the disk view calculates the I/O total, so the I/O warning of a group shows only in the disk view.

Network errors and drops count as a warning from one per second. Use the network view for packet counts.

If you select an earlier row, collection continues. Press `End` to follow the latest row.

If collection fails, the dashboard keeps the last row and retries at the next interval.

The default table follows the terminal width. If the terminal is wider than 80 columns, the table adds columns in this order:

| terminal width | added columns |
|---|---|
| 84 | hot core |
| 96 | disk IOPS and `await` |
| 110 | packet in/out |
| 120 | network errors and drops |
| 125 | disk `busy` |
| 143 | CPU, memory, and I/O pressure (`psi`) |
| 149 | memory swap out (`swap`) |
| 161 | listen queue overflows (`listen`) and softnet drops (`soft`) |
| 167 | conntrack usage (`ct%`) |

The `signal` column gets 13 to 16 characters. That is room for at least one warning and the number of the other warnings. The other columns share the remaining width, so the table fills the terminal. The title line also adds the OS name, the memory size, and the CPU model when the terminal has room. The right edge of the title shows the view and `live` or `history`. It adds the edc version when the terminal has room. If the terminal becomes narrower, the table removes those columns immediately.

If the terminal has fewer than 80 columns, the overview shows CPU, memory, load, and signals. Other views omit columns that do not fit.

The minimum terminal size is 24 columns and 8 rows. Help supports scroll on small terminals.

Use `--split` to show several views at the same time. Each view gets a box with its own table of recent samples.

`--split` takes a list of names: `cpu`, `mem`, `disk`, `net`, and `psi`. Boxes appear in the order that you write them. `--split` without a list shows all five boxes. `--split none` starts with a single view.

Write each name once. An empty value, an unknown name, or a repeated name stops the command with exit code 2.

Set `defaults.top.split` in the config file to start the dashboard with the same list every time. The value uses the same syntax as `--split`. A `--split` option on the command line has priority over the config value.

The boxes keep the order of the list. The layout uses the smallest number of rows that fit the terminal width. It then divides the boxes so that the widest row is as narrow as possible. All rows share the height equally.

Every box omits the `signal` column. One line below the boxes shows the signals of the selected time. If the cpu box is on the screen, the mem and psi boxes omit `load`. If the mem box is on the screen, the psi box omits `mem%`. Only the first box in each row shows the `time` column. The other boxes in that row use the same times. The detail, peaks, and process panels appear below that line, as in the single view.

All boxes share the selected row. The arrow keys, `PgUp`, `PgDn`, and `End` move all boxes together. `PgUp` and `PgDn` move by the number of data rows in one box.

Press `1`, `c`, `m`, `d`, `n`, or `s` to leave the box screen. Press `v` to return.

Each box needs at least 3 data rows. If a box has fewer rows, the dashboard shows the first box as a single view. The status line explains this. A box that is wider than the terminal drops its last columns.

If the optional columns, such as `steal%` and `retr/s`, add a row of boxes, the boxes omit these columns.

A terminal with 80 columns and 24 rows shows only the CPU view for `--split`. At 80 columns, all five boxes need about 33 rows.

On macOS, the dashboard omits the `psi` box and shows a notice. The mem box shows the macOS memory pressure level. The config file can still list `psi`, so one file works on both systems.

`--split` works only in the dashboard. If you add `--split` to a run that prints the table, the command exits with code 2. That includes `--count`, `--json`, a pipe, and `NO_COLOR`. The same applies to `--process`. The config value does not stop those runs. With `--process`, the process view stays.

On macOS and Linux, the disk view shows IOPS and average `await` across physical disks.

On Linux, the disk view shows aggregate `busy%`, and the memory view shows memory pressure. Aggregate `busy%` can exceed 100 across multiple disks.

On Linux, some views also show optional columns. The CPU view shows `steal%` and `blocked`. `steal%` is the CPU time that the hypervisor gave to other guests. `blocked` is the number of tasks that wait for I/O now. The disk view shows `queue`, the average number of I/O requests in progress, added across physical disks. The network view shows TCP retransmitted segments (`retr/s`), sent resets (`rst/s`), and failed connection attempts (`fail/s`) per second. Optional columns appear only if all other columns fit and `signal` keeps at least 13 columns.

On macOS, the dashboard omits unsupported iowait, PSI, disk busy, file descriptor, listen overflow, softnet drop, conntrack, and I/O latency columns. The help page lists these limits. The memory and pressure views show `mem lvl`, the memory pressure level of the kernel (`kern.memorystatus_vm_pressure_level`): `normal`, `warn`, or `critical`. The CPU and pressure views show `blocked`, the number of processes in the `U` state. Linux counts tasks, so the two values are not the same unit.

On macOS and Linux, the network view shows interface errors and drops. On macOS, `edc` reads kernel interface statistics (`net.link.generic.ifdata`).

The memory view shows `swap/s`, the bytes per second that the kernel moves out to swap.

Press `s` for pressure. On macOS, it shows `mem lvl`, `blocked`, `load`, and `mem%`. On Linux, it shows CPU, memory, and I/O `some avg10`: the percentage of the last ten seconds during which at least some tasks waited for that resource. The `mem full` and `io full` columns show memory and I/O `full avg10`. This is the percentage of the last ten seconds during which all non-idle tasks waited at the same time. The Linux pressure view also shows `blocked`, the tasks that wait for I/O now, next to `io psi`. The CPU view shows the hottest core and an ASCII bar; on machines with more than 24 cores, the bar shows the first 24.

The detail view also lists the top three processes by CPU. The list refreshes in the background at most once a second, so it does not lengthen the observation interval. Without `--write`, only the dashboard collects it; the table and unfiltered `--json` output skip it. On Linux, `edc` compares the CPU ticks in `/proc/<pid>/stat` with the previous refresh, so the value covers the time since that refresh. On macOS, it uses the recent decaying average that `ps` reports.

The process panel shows `PID`, `COMMAND`, `STATE`, `CPU%`, and `RSS`. `STATE` shows `run`, `sleep`, `iowait`, `zombie`, `stop`, `trace`, `idle`, `dead`, or `park`. `iowait` is the Linux `D` state: the process waits for I/O, and a signal cannot wake it. An `iowait` process waits for the disk. It does not prove that the process uses the disk.

If the terminal has 60 or more columns, the panel adds `READ` and `WRITE`. The disk view and `--process` always add them. Other views add them only when a value is available. These values are the bytes per second that reach the storage device, from `/proc/<pid>/io`. The panel shows `—` if `edc` cannot read the value. To read the processes of other users, run `edc top` as root.

The panel ranks candidates by CPU in most views, by RSS in the memory view, and by I/O in the disk view. The disk view reads the I/O of every process, so a process with low CPU and high I/O appears. Other views read the I/O of the candidates only. Before the list limit, the panel keeps five leaders by CPU, by RSS, and by `iowait` state. The disk view also keeps five leaders by I/O. The panel shows three candidates. If the terminal has 40 or more rows, the panel shows five.

The panel also shows up to two process groups above the candidates. A group is two or more processes with the same executable name, for example `gm (200 procs, 150 iowait)`. Each view ranks the groups by its own total: CPU, RSS, or I/O. A group must reach the minimum total of the view: 10% CPU, 100 MB RSS, or 1 MB/s I/O. In the disk view, a group with `iowait` processes also appears. The RSS total counts a shared page one time for each process, so `≤` marks it as an upper bound. A group line has no PID, and you cannot select it.

On Linux, the kernel adds the I/O of a child process to its parent when the parent reaps the child. A parent that starts many short-lived children can show the I/O of those children. Check the children before you blame the parent.

The panels and the key hints stay at the bottom of the screen. If the two hint lines fit in one line, the dashboard joins them. If the terminal has 145 or more columns, the process panel moves to the right. The detail and peaks panels and the key hints use the left side. The events use the left side when no other panel uses it. From 89 to 144 columns, the events stay to the right of the process panel. With `--process`, the panels stay below each other because the process lines are longer.

Press `Tab` to select a candidate. Use arrows to choose a process. Press `Enter` to focus its PID.

Process selection keeps the selected time while new samples arrive. Press `End` to return to live history.

The `signal` column puts host warnings before process CPU candidates. A candidate does not prove the cause of a host warning.

Process CPU uses 100% per core. Host CPU uses all cores as its total.

The process panel and `PROCESS` bar show the selected time. If a host sample fails, the dashboard shows the last success time.

The interval moves between 200ms, 500ms, 1s, 2s, 5s, 10s, 30s, and 1m. Resuming first creates a new baseline, and later rows show rates.

The dashboard quits to the previous screen and leaves no rows behind. Use `--write <DB>` to record while keeping the dashboard, or `--json` for JSON Lines.

`edc top` prints the earlier table instead of the dashboard in these cases:

- The command uses `--count` or `--json`.
- stdin or stdout is not a terminal.
- `NO_COLOR` is set.

On macOS, `edc` reads the CPU ticks of each core from the kernel with the Mach `host_processor_info` call. On Linux, `edc` reads `/proc/stat`. On both systems, every column follows the interval.

On Linux, the `n` view adds conntrack occupancy (`ct%`), listen overflows/s (`listen/s`), and softnet drops/s (`soft/s`). Its panel shows the selected sample's conntrack entries, TCP socket counts, and rates for listen drops, SYN cookies, conntrack drops, UDP receive-buffer errors, and softnet budget exhaustion. Use `↑`/`↓` for history, `Enter` for settings at that sample, and `h` for peaks in the last 60 seconds. Conntrack occupancy warns at 90% and marks risk at 98%; neither proves a connection failure.

Collection uses the current network namespace, but softnet counters and TCP TIME_WAIT can be host-wide. TCP `CurrEstab` includes ESTABLISHED and CLOSE_WAIT. Socket counts are not local port utilization. Missing baselines, failed reads, or counter resets show `—` for rates. Conntrack statistics require an exposed `/proc/net/stat/nf_conntrack`; missing statistics do not prevent other collection.

JSON samples add `network_limits` with the namespace, `settings`, `gauges`, cumulative `counters`, and per-second `rates`. The counters and rates include `tcp_retrans_segs`, `tcp_out_rsts`, and `tcp_attempt_fails`. Readings include `status` and, where needed, `reason`; unobserved numbers are omitted. Save JSON Lines to analyze trends after the command exits.

## Top events

`edc top` keeps a warning from the `signal` column as an event when the warning stays on for 5 seconds. A warning that stops and starts again within 5 seconds stays in the same event. The dashboard keeps the last 100 events. Events stay in memory, and they disappear when the dashboard quits.

Each event keeps its worst value and the leading group and process at that sample. The candidates come from the view of the warning: I/O for disk and pressure warnings, RSS for memory warnings, and CPU for other warnings. Network warnings get no candidate. A candidate does not prove the cause.

One fault often starts several warnings at the same time. A full disk raises `await`, `blocked`, `load`, `i/o`, and `psi io` together. Events that start within 5 seconds of the first one share one line. The line shows the worst warning and the number of the other warnings, for example `await 2034ms +4`.

| mark | meaning |
|---|---|
| `●` | The event is still on. |
| `≤` before the time | The warning was already on at the first sample. It can have started earlier. |
| `!` after the time of a history row | An event started at this row. |

The footer shows the recent events. If the footer has no room, one line above the panels shows the worst event that is still on.

Press `e` to open the event list. Each entry shows the worst warning, the other warnings with their worst values, and the candidates. Press `Enter` to go to the start of the entry. The history keeps 500 rows, so an older event keeps only its summary. Press `[` or `]` to go to the start of the previous or next entry.

## Top JSON output

Use `--json` to write one JSON object for each sample. Use `-` for stdout. A path gets a new file with mode 0600.

```bash
./bin/edc top --count 5 --json -
```

Each line has `time`, `hostname`, `cores`, the network and disk rates in bytes per second, the CPU values in percent, `load1`, `memory_pct`, and `swap_out_bytes_per_s`. macOS and Linux emit network errors and drops, and disk IOPS and await values. Linux additionally emits disk busy values, PSI `some avg10`, and memory and I/O PSI `full avg10`; `*_health_supported`, `disk_busy_supported`, and `psi_supported` tell consumers whether those values are supported. macOS adds `memory_pressure` (`normal`, `warn`, or `critical`). The `--json` option removes the table and the header.

## Top recordings and history

`--write [path]` (`-w [path]`) appends observations to a local SQLite database. The dashboard stays open; `--count`, pipes, and `--json` retain their output behavior. Recording also allows `--process` in table mode. With no path, `-w` uses `~/.local/state/edc/history.db` on Linux or `~/Library/Application Support/edc/history.db` on macOS. An absolute `XDG_STATE_HOME` overrides the default on both platforms: `$XDG_STATE_HOME/edc/history.db`. The default directory is created with mode 0700. With no `-w` or `--write`, top does not save observations.

```bash
./bin/edc top -w
./bin/edc history list
./bin/edc top -w incident.db
./bin/edc top --process nginx --count 60 -w incident.db
./bin/edc top --count 10 -w incident.db --json samples.jsonl
./bin/edc history list incident.db
./bin/edc history top --metric memory_pct --min 90 incident.db
./bin/edc history top --from 2026-10-05T09:00:00+09:00 --to 2026-10-05T10:00:00+09:00 incident.db
./bin/edc history process --process nginx --metric cpu_pct --min 100 incident.db
./bin/edc history top --run <run-id> --limit 0 --json - incident.db
```

Each invocation adds a separate run with host information, edc version, collection settings, start/end times, status, and sample count. `unfinished` means the run has no recorded end, which can indicate an active run or a forced termination. Normal exits flush queued samples; recording errors end observation with exit code `1`. Forced termination preserves committed transactions. A new database has mode 0600. There is no automatic deletion or rotation.

Host samples include the existing numeric top metrics, per-core CPU values, support flags, the actual observation interval, and the process filter active at that time. Without a process filter, recording retains the union of the CPU top five and RSS top five (at most ten). With a filter, it retains up to fifty processes and the total of all matches. It does not record every process. Pausing top pauses recording, and resuming establishes a fresh baseline before the next recorded rate. Process refreshes may be less frequent than host samples; `process_observed_at` identifies the refresh time. PID and precise process start time distinguish reused PIDs when start time is available.

`history list`, `top`, and `process` show newest results first. Omitting the database path selects the same default database, without creating it. `--run` selects one run; `--from` includes the start and `--to` excludes the end, using RFC3339 timestamps. `--limit` defaults to 200; `0` means all matching results. `--json <path|->` emits JSON Lines. Options precede the database path, following Go flag parsing.

`top` and `process` support `--metric <field>` with an inclusive `--min`, `--max`, or both. Use top-level numeric JSON field names, such as `memory_pct`, `disk_await_ms`, `rss_bytes`, or `disk_read_bytes_per_s`. `cpu_pct` means user+system for host samples and the existing per-core percentage for processes. Unsupported or unmeasured SQL values are NULL and do not match numeric thresholds; JSON preserves availability information. The process name/PID filter follows the same rules as `top --process`.

The database uses WAL so you can query while recording. Keep it on a local filesystem. External SQLite tools can inspect `runs`, `top_samples`, and `top_process_samples`; numeric fields are columns and `payload` retains JSON details, including cgroup/eBPF data. `history` opens existing databases for reading and does not create or migrate them. Unrelated databases and unsupported schema versions are rejected. JSON output cannot overwrite the database or its journal files.

## Filter processes in top

`--process <filter>` narrows the process list to the processes you name. The filter is a comma-separated list.

A number must equal a PID. Other terms match part of the command name without case sensitivity.

A process that matches any term stays in the list.

On Linux, the command name is `comm`, which the kernel cuts at 15 characters. On macOS, it is the executable path.

```bash
# dashboard: opens the process view, and Enter shows the busiest matches
./bin/edc top --process output-mesh
# JSON lines: add a processes array to each sample
./bin/edc top --process 4321,worker --json /tmp/edc-host.jsonl
```

With `--process`, the dashboard shows a highlighted `PROCESS` bar under the title. It names the filter and the current values of the matched processes. When the terminal is narrow, the bar drops the last values first. The dashboard also opens the process view. Each row shows the matched processes at one sample. It shows the number of matches, their CPU%, memory (`rss`), threads, open files, and disk read and write per second. One core is 100 CPU%. With `--ebpf`, the row also shows the average run-queue wait and the average block I/O latency in milliseconds. A value that `edc` cannot read shows `—`. Press `1` for the host columns and `f` to return to the process view. `Enter` shows the busiest matches for the selected row.

Select a row and press `f`. If the first signal is a host value, `f` opens its view.

Press `f` again to select a process candidate. Use arrows to choose a process. Press `Enter` to focus its PID.

Press `/` to edit the filter. Press `Esc` to close process selection, then press `Esc` again to clear the filter.

Rows from a different filter show `—` for process values. They do not represent the current filter.

The filter runs before the list is cut to the busiest processes. A quiet process that matches stays in the list.

The detail view starts with totals over every match: count, CPU, RSS, threads, open file descriptors, and disk I/O. Next, it lists the three busiest matches and `+N` for the rest. The remaining lines show process resource limits. The `signal` column then reflects only the matches.

With `--json`, a sample adds two fields. `processes` holds up to 50 matches, busiest first. `process_total` is the sum over every match: `count`, `cpu_pct`, `rss_bytes`, and `threads`. CPU is 100% per core. `processes` is `[]` when nothing matches. Each process has these fields:

| Field | Meaning |
|---|---|
| `pid`, `started` | `started` is the start time in UTC. A PID can be reused, so `(pid, started)` names one process. |
| `command`, `cpu_pct`, `rss_bytes` | name, CPU since the last refresh, resident memory |
| `threads` | thread count (Linux and macOS) |
| `fds` | open file descriptors (Linux) |
| `disk_read_bytes_per_s`, `disk_write_bytes_per_s` | disk I/O bytes per second from `/proc/<pid>/io` on Linux and libproc on macOS |

A field that `edc` cannot read is absent. It is not set to zero.

On Linux, `fds` and disk fields need the same user or root. A filter reads details for at most 50 processes.

On macOS, libproc supplies thread counts and disk I/O counters. File descriptor counts remain unavailable. Process CPU retains the recent `ps` average.

Disk rates require two samples of the same process. The dashboard shows the baseline state or the libproc error.

`no ev` means that the eBPF observer is active but collected no events in that interval. It does not mean zero latency.

Without `--process`, JSON output has no process fields.

`--process` needs the dashboard, `--json`, or `--write`. The table has no process column, so `edc top --process x --count 5` stops with exit code `2`.

### Process resource limits (Linux)

With `--process`, the process panel shows FD usage and the soft limit for the selected candidate.

Press `Enter` on a history row to see resource details for the retained processes.

The cgroup v2 details show group memory usage, the local memory limit, local OOM counts, and CPU throttle counts.

Each shared cgroup appears once in the detail view. These values describe the group, not one process.

Memory usage includes descendant groups. The local memory limit does not include ancestor limits.

Local OOM counts are cumulative for the lifetime of the cgroup. They do not include descendant events.

CPU throttle counts cover the interval between two samples. They describe the group's own CPU limit, not ancestor limits.

The first CPU sample establishes a baseline. A counter reset or an interrupted observation requires a new baseline.

JSON adds `limits.fd` and `limits.cgroup` to each process. Each metric includes a `status` and an optional `reason`.

Unavailable values remain absent. Unlimited FD and memory limits use explicit boolean fields.

The command distinguishes unsupported metrics, denied access, unavailable namespace paths, and read errors. cgroup v1 and macOS resource limits are unsupported.

These metrics use the [Linux cgroup v2 interfaces](https://docs.kernel.org/admin-guide/cgroup-v2.html).

### CPU wait and I/O latency with `-d`

`-d` or `--detail` adds what `/proc` cannot give: how long the matched processes wait for a CPU and how long their block I/O takes. It needs `--process`. On Linux, it also needs root or `CAP_BPF` and `CAP_PERFMON`, and kernel BTF. Without them, `edc top` stops with exit code `3` and says whether the host is unsupported or a capability is missing. It is tested on Linux 5.15 and 6.17. Linux 5.15 does not give `prev_state` to the `sched_switch` tracepoint, so on that kernel `edc` reads the task state instead.

`--ebpf` is the same option. It stays for scripts that use the earlier name. In the dashboard, `-d` also works without `--process`. The values then start when you choose a process with `f` or `/`. The `PROCESS` bar and the process view then show the run-queue wait and the I/O latency.

```bash
sudo ./bin/edc top --process output-mesh -d
sudo ./bin/edc top --process output-mesh -d --json /tmp/edc-host.jsonl
```

The dashboard adds a third line to the detail view: `ebpf 1s · runq 7584 avg 5.80ms p95 <16.384ms · io 704 avg 0.07ms p95 <0.256ms`. With `--json`, each process and `process_total` get an `ebpf` object:

| Field | Meaning |
|---|---|
| `window_s` | seconds the counts cover, since the previous sample |
| `runq_count`, `runq_avg_ms`, `runq_p95_ms` | times a thread of the process became runnable and then got a CPU, and the wait in between. A high wait with a low `cpu_pct` means the CPU is oversubscribed. |
| `io_ops`, `io_bytes`, `io_avg_ms`, `io_p95_ms` | block I/O requests the process issued, their bytes, and the time from issue to completion |

`p95` is the upper edge of the power-of-two bucket that holds the 95th percentile, so the real value is below it. A latency is left out when there were no events. `process_total` merges every match, and its p95 comes from the merged distribution. `edc` watches the 4096 busiest matches.

- A block I/O request belongs to the task that issues it. Synchronous reads, direct I/O, and `fsync` land on the process. Buffered writes are issued later by a kernel flusher, so they land on `kworker`. Use `disk_write_bytes_per_s` for those bytes.
- `edc` translates the kernel's PIDs into the PID namespace it runs in, so the filter matches inside a container as well.
- The first sample after a process appears has no `ebpf` object, because the counts start when `edc` begins to watch it.
- `source` in the `ebpf` object names how the values were counted: `ebpf` on Linux, `libproc` on macOS.

macOS has no public hook for scheduler or block I/O events. On macOS, `-d` therefore counts only the CPU wait, without root, from libproc's cumulative counters. The wait is the runnable time minus the CPU time, and `runq_count` is the number of context switches in the window, so `runq_avg_ms` is the average wait per switch. There is no distribution, so `runq_p95_ms` is left out, and I/O cannot be counted, so the `io_*` fields are absent. The dashboard shows `n/a` in those cells. Without root, macOS refuses to read the processes of other users. `edc` counts them in `unreadable`, leaves out the `runq_*` fields when it read none of the matches, and shows `root` in the dashboard. The x86_64 build stops `-d` under Rosetta, because the translated clock may not match the kernel counters; use the arm64 build.

```bash
./bin/edc top --process Safari -d
```

On macOS, a process in the `U` state (uninterruptible wait) shows as `iowait` like Linux `D` and joins the blocked candidates.

## AI tool usage

`edc ai` shows the tokens that Claude Code and Codex use on this host. It also shows the account limits of both tools and their reset times.

```bash
./bin/edc ai                      # dashboard (press q to quit)
./bin/edc ai --count 3            # three samples as a table
./bin/edc ai --count 1 --json -   # one JSON line
```

In a terminal, `edc ai` opens a dashboard. Each row of the table is one interval: 10s, 1m, 5m, or 1h. To change the interval, press `+` or `-`. The `▸` row is still open. To move through 24 hours of history, use `↑`, `↓`, PgUp, PgDn, and End. The `Σ` rows sum the last 10 minutes, the last hour, and the last 24 hours.

edc counts the tokens from the local logs. Every 2 seconds, it reads only the new lines.

- Claude Code: `~/.claude/projects/**/*.jsonl`. edc counts each message once.
- Codex: the growth of the cumulative token count in `~/.codex/sessions`.

The table counts this host only. The tokens that other machines or claude.ai use show only in the limit percentages.

`in` is new input, and `cache` is input that the tool read from the prompt cache. `hit` is `cache / (in + cache)`, rounded down. For Claude, `in` includes the tokens that Claude Code wrote to the cache, so these tokens count as misses. If an interval has no input, `hit` shows `-`. The `--json` output has `cache_hit_percent` with one decimal place, and it omits the field for an interval without input. The table needs 88 columns.

The limit boxes show each window with its use, its reset time, and the time left.

- Codex: edc starts `codex app-server` and asks it. The `codex` command must be in `PATH`.
- Claude on Linux: edc calls `https://api.anthropic.com/api/oauth/usage`, the API behind `/usage` in Claude Code. It sends the token in `~/.claude/.credentials.json`. edc does not print, store, or refresh this token.
- Claude on macOS: edc reads the usage that the Claude Code status line gives to `edc ai statusline`. See the end of this section.

Anthropic does not document the Claude usage API, and the API answers frequent calls with HTTP 429. So edc calls it at most every 5 minutes. After a 429, edc waits 10 minutes, then 20 minutes, then at most 30 minutes. Until the next call, the box shows the saved values and their age.

`--poll` sets the time between limit calls. The default is 60s, and the minimum is 30s. On Linux, Claude keeps its own interval of 5 minutes or more.

Without a terminal, or with `--count` or `--json`, edc prints samples in place of the dashboard. `--count N` stops after N samples, and 0 runs until you stop edc. `--json <path|->` writes one JSON object for each sample. A file gets mode 0600.

edc writes each reset that it detects to `ai-resets.jsonl`. It keeps the last Claude values in `ai-claude.json`. Both files are in the directory of the history database. For that directory, see [Top recordings and history](#top-recordings-and-history).

On macOS, Claude Code keeps the token in the Keychain. edc does not read the Keychain. Claude Code gives the 5-hour and 7-day limits to its status line command. `edc ai statusline` saves these limits to `ai-claude-statusline.json` in the same directory.

To connect the status line, install edc in its permanent location first. Then run this command:

```sh
edc ai statusline install
```

The command changes only `statusLine.command` in `~/.claude/settings.json`. It keeps the old command, so `cship` becomes `edc ai statusline -- cship`. The old file stays in `settings.json.edc-backup`. To restore the old command, run `edc ai statusline uninstall`.

The values come from the last Claude Code request. The box shows their age. The text output adds an `updated` line when the values are 1 minute old or more. The macOS box does not show the plan name. The status line input does not include it.

## Remote recipes

`edc remote <group>` executes a YAML recipe on an inventory group. It uses local OpenSSH configuration, agents, and known host checks.

Each command loads the remote account default shell in interactive mode. Shell startup output and prompt hooks stay hidden.

Store no passwords or private keys in inventory and recipe files. Configure SSH aliases in `~/.ssh/config`.

Each step needs `name` and either `command` or `upload`. The `verify` field is optional. If `verify` is absent, the action result decides the step result.

Use `upload` instead of `command` to send a local file. Use `source` for the local file and `destination` for the remote path. The `mode` field is an optional octal permission.

`upload` runs OpenSSH `scp`. If you set `mode`, `edc` runs remote `chmod` after the transfer.

Keep `name` for group references. If `target` is absent, `edc` uses `name` as the SSH target.

![edc remote daily --dry-run prints the host by step plan table without an SSH connection and marks an unmatched step with –](docs/media/remote.gif)

The demo shows the tags of a recipe and the plan table that `--dry-run` prints.

### Command shape

The group is a positional argument. The `--group` flag is an alias for the same value. Use only one of the two.

```bash
cp examples/remote/inventory.yaml ./inventory.yaml
./bin/edc remote              # select the group, then confirm
./bin/edc remote daily        # name the group, then confirm
./bin/edc remote daily -f     # name the group and skip the confirmation
```

`edc` finds `inventory.yaml` in these directories, in this order:

1. `./.edc/`
2. `./`
3. `os.UserConfigDir()/edc/`. This path follows the operating system.

The `recipe.yaml` file uses the same order. The file selectors list the YAML files of the same directories, in the same order.

Keep the remote files of a project in `./.edc/`. Then the project root stays clean. To keep the directory out of git, put a `.gitignore` file in it. `edc` does not make this file.

```bash
mkdir -p .edc
printf '*\n' > .edc/.gitignore
```

If you want to commit the files, remove `.edc/.gitignore`.

A relative `upload.source` path starts from the directory where you run `edc`. It does not start from the directory of the recipe file.

Add `-v` to show the search order. The line marks a directory that does not exist. It also marks a `./.edc/` directory that has no `.gitignore` file. `--list -v` shows the same line.

```
inventory  ./.edc/inventory.yaml      recipe  ./.edc/recipe.yaml
search     ./.edc/  →  ./  →  /home/me/.config/edc/ (missing)
```

The `--inventory` and `--recipe` flags win over the found files.

If you name a group, `edc` asks no path questions. It shows the plan and requests the confirmation only.

If you name no group, `edc` selects the group first. It then shows the inventory path, selects the recipe, and requests confirmation.

After the selection, the plan starts with an `edc remote` command. The command names the group, the inventory, and the recipe that you selected. It also keeps the flags that you gave. Run this command to use the same selection again. `edc` then requests the confirmation only.

The interactive selectors draw the list in place. Use the up and down arrow keys, or `j` and `k`. Press Enter to select. Press `q` or Esc to cancel. A cancelled selection returns exit code `4`.

While you choose, `edc` reverses the colors of the row under the cursor and puts a `▌` bar on its left. The reverse makes the row clear in a black and white terminal too.

The list stays on the screen after you select. The bar stays on the item you chose, without the reverse, and the first line keeps the name of the question.

```
inventory 파일
  lab-hosts.yaml  ·  group 1개, host 1개
▌ prod-hosts.yaml  ·  group 2개, host 2개
```

If `edc` finds no `inventory.yaml`, it lists the YAML files of the search directories that read as an inventory. The list shows the group and host counts. The recipe list shows the recipe name and the step count. `edc` hides a YAML file that does not read as an inventory or a recipe. If the list is empty, `edc` asks for a path.

The confirmation questions put the question, the two answers, and the key help on one line. Move with the left and right arrow keys and press Enter, or press `y` or `n` to answer at once. The answer you point at gets a `▌` bar and reversed colors. The default answer is no.

Use `-f` or `--force` to skip the confirmation. Combine it with `-v` for streaming output. If you name no group, `-f` needs an inventory with exactly one group.

A recipe command runs without a terminal, and its input is empty. If a step runs a `host changes` command without `--yes`, the command cannot get a confirmation. `edc disk grow` stops with exit code `4`. `edc route switch` restores the route. `edc change apply` rolls back when its timer fires.

If a step adds `--yes`, `edc disk grow` and `edc change apply` run on every host of the group without a question. `edc route switch` does the same when the exit identity matches. Read the plan before you confirm. Do not run such a recipe with `-f`.

These group names are reserved for future subcommands: `run`, `list`, `plan`, `hosts`, `groups`. An inventory that uses one of them fails to load.

The earlier `edc remote run` form is gone. Use `edc remote <group>`.

### Dry run and inventory listing

Use `-n` or `--dry-run` to print the plan and exit. `edc` opens no SSH connection. Add `--json` to get the plan as JSON.

```bash
./bin/edc remote daily --dry-run
./bin/edc remote daily --dry-run --json -
```

Use `-l` or `--list` to print the groups and hosts of the inventory. Name a group to limit the output to that group.

```bash
./bin/edc remote --list
./bin/edc remote daily --list --json -
```

The `--dry-run` and `--list` options do not combine with `-f`. Add `--redact` to hide IP addresses in the output.

### Host tags

Add `tags` to a host and to a step. A step without `tags` runs on every host of the group. A step with `tags` runs only on the hosts that carry one of the same tags. Use this to send different work to macOS and Linux hosts in one recipe.

```yaml
# inventory.yaml
hosts:
  - name: build-server
    tags: [linux]
  - name: workstation
    tags: [mac]
```

```yaml
# recipe.yaml
steps:
  - name: git-kit          # no tags, so every host runs this step
    command: git-kit update
    verify: git-kit --version
  - name: brew
    tags: [mac]
    command: brew update && brew upgrade
    verify: brew --version
  - name: apt
    tags: [linux]
    command: apt-get update && apt-get -y upgrade
    verify: apt-get --version
```

A host that does not match a step gets no result for that step. The report keeps the skip count for failed hosts only.

If no host in the group matches the tags of a step, `edc` prints a warning to stderr and continues. Check the tag spelling.

### One table

`edc remote` uses one table for the plan and the results. The rows are the hosts and the columns are the steps. The table starts with `·` in every cell, and each cell changes when the step of that host ends. A cell shows `–` if the tags of the step do not match the host.

```
edc remote  daily  ·  host 3  ·  step 3  ·  실행 8
inventory  ./inventory.yaml      recipe  ./recipe.yaml

host          git-kit  x-mesh  brew
build-server  PASS     PASS       –
ci-runner     PASS     ⠋          –
workstation   PASS     ·          ·

⠋  ci-runner / x-mesh / command  ·  4/8 완료  6.6s

git-kit  git-kit update  →  git-kit --version
x-mesh   xm update  →  xm version
brew     brew update && brew upgrade -f   tags mac
```

The line under the table names the host, the step, and the phase that runs now. The lines below it give the command of each step.

If stdin and stdout are terminals, the confirmation question appears right under this table, above the command list. The answer you point at gets a `▌` bar and reversed colors. Answer with the left and right arrow keys and Enter, or with `y` or `n`. The same line then becomes the progress line and the table fills in. Use `-f` to skip the question.

```
host   uname  uptime
alpha  ·      ·

실행할까요?     예   ▌ 아니오       ←/→ 이동   Enter 선택   y/n 바로 답하기
```

The table fits the terminal width. `edc` first shortens the column names, then changes `PASS` and `FAIL` to `✓` and `✗` with a legend, and last shortens the host names.

Add `-v` to show the last output lines below the table.

Press Ctrl-C to cancel. `edc` stops the running commands, marks the remaining steps as `SKIP`, prints the summary, and returns exit code `4`. Press Ctrl-C again to close the screen at once.

`edc` prints the earlier result lines instead of the table if stdin or stdout is not a terminal, if `--json` is set, or if `NO_COLOR` is set.

### Automation

Use all flags for cron or launchd. A non-terminal command never waits for input. It also skips the confirmation.

```bash
./bin/edc remote daily \
  --inventory ./inventory.yaml \
  --recipe ./examples/remote/daily-update.yaml \
  --parallel 2 \
  --json ./remote-report.json
```

A non-terminal command needs a group. Name it as the positional argument or with `--group`.

Use `-v` or `--verbose` to stream each remote command.

`edc` prints the PASS or FAIL line at the end of each step. The final output shows the failures and one summary line with the counts and the elapsed time.

Set `parallel` in the inventory to run hosts concurrently. Use `group_options.<group>.parallel` for one group.

The `--parallel` option overrides both inventory values. Each host still runs its steps in order.

Each host runs in inventory order. Each step runs its command and verify command in recipe order.

If a step fails, `edc` skips later steps on that host. The next host still runs. Any failure returns exit code `1`.

## Route switching

`edc route` changes the default route of a gateway VM and rolls the change back when it fails. Linux only. `switch` and `rollback` need root. `edc` does not add `sudo` itself.

Run this command on the gateway VM, not from a remote machine. `edc` reads `SSH_CONNECTION` to judge whether the change cuts your own session.

```bash
edc route check                 # inspect the current state, change nothing
edc route switch --to <name>    # switch, arm a rollback timer, verify, confirm
edc route status                # show a pending or finished switch
edc route rollback --state <f>  # restore one state file
```

`rollback` is a separate command because the armed timer runs it. The timer and a person call the same code path.

### Exit list

`switch --to` takes a name from `exits.yaml`. A name list prevents a typo in a next-hop address. A wrong next-hop still returns exit code `0` from the kernel, so free-form input hides the mistake.

```yaml
dest: default
exits:
  - name: lab-nat-01
    via: 10.0.2.1
    dev: ens3
    expect_public_ip: 203.0.113.10
  - name: lab-nat-02
    via: 10.0.3.1
    dev: ens3
    expect_public_ip: 203.0.113.20
```

`edc` finds `exits.yaml` in the same directories as `inventory.yaml`. Use `--exits <file>` to name another path.

`expect_public_ip` is the address that the exit presents to the internet. `edc` reads the public address after the switch and compares it. Without this field `edc` skips the identity check and asks a person to confirm.

### Safety

`edc` runs three guards. Each one stops a different failure.

1. **Preflight.** `edc` reads the neighbor entry of the next-hop. An `INCOMPLETE` or `FAILED` entry means the change fails for certain, so `edc` refuses it and changes nothing. An absent entry is unknown, not broken, so `edc` allows it. Use `--force` to override a refusal.
2. **Rollback timer.** Before the change, `edc` arms a systemd timer that runs `edc route rollback`. The timer lives in PID 1, so it fires even after the `edc` process dies. If the arming fails, `edc` changes nothing.
3. **Verification.** `edc` compares the route count before and after, confirms that the destination still uses the changed route, and checks the public address of the new exit.

Use `--seconds` to set the rollback grace period, between 10 and 900. The default is 120.

```bash
edc route switch --to lab-nat-02 --dry-run   # print the plan, change nothing
edc route switch --to lab-nat-02 --seconds 60
```

`--dry-run` prints the route that `edc` replaces, the exact commands, the timer that it arms, and the reachability of the exit. It needs no root.

A switch breaks existing connections. The NAT state stays on the old exit, so established flows stop. New flows use the new exit.

## Protected host changes

Use `edc change` for host changes that can block access. Linux only. Run it on the host as root.

Before `edc` changes the target, it saves the current state and arms a systemd timer. If no confirmation arrives, the timer runs the rollback command.

Change an SSH public key file:

```bash
edc change apply \
  --kind authorized-keys \
  --path /home/<user>/.ssh/authorized_keys \
  --content-file /path/to/authorized_keys \
  --seconds 120
```

Change the IPv4 firewall rules:

```bash
edc change apply \
  --kind iptables \
  --rules-file /path/to/rules.v4 \
  --seconds 120
```

Use the run ID from the apply result to confirm the change:

```bash
edc change confirm --state /run/edc/change-<run-id>.json
```

Use the same state path to roll back before the timer fires:

```bash
edc change rollback --state /run/edc/change-<run-id>.json
```

Inspect pending changes and their remaining time:

```bash
edc change status
```

`authorized-keys` accepts a regular file with public keys. `iptables` validates the rules with `iptables-restore --test` before it arms the timer.

Run only one protected change at a time. If the target changes after apply, `edc` refuses to overwrite the new state during rollback.

Use `--yes` to confirm immediately. This removes the rollback window.

## Disk growth

Use `edc disk` after you make a cloud volume larger, for example an AWS EBS volume or an OCI block volume. Linux only. `grow` needs root. `edc` does not add `sudo` itself.

```bash
edc disk check                 # show the mounts that can grow, change nothing
edc disk check /data           # show the layers under one mount
edc disk grow /data -n         # print the plan, change nothing
edc disk grow /data            # show the plan, ask, then grow
```

`edc` reads the layers from `/proc/self/mountinfo` and `/sys/class/block`. A layer is the disk, the partition, the LVM physical volume (PV), the logical volume (LV), or the file system. Then `edc` runs only the steps that add space:

1. `rescan` if the disk is a SCSI disk. This step follows the OCI procedure: a direct read of one block, then a write to `/sys/class/block/<disk>/device/rescan`. NVMe and virtio disks have no rescan file. The kernel sees their new size at once.
2. `growpart <disk> <number>` if the partition is shorter than the disk.
3. `pvresize <pv>` if the PV is shorter than its partition.
4. `lvextend -l +100%FREE <lv>` if the volume group has free space. The LV gets all the free space of the volume group.
5. `resize2fs <device>` for ext3 and ext4, or `xfs_growfs -d <mount>` for xfs.

After each step, `edc` reads the layers again and checks that the layer grew. If a step fails, run the same command again. The layers that grew drop out of the plan, so the next run starts at the failed step.

The time limit applies only to the reads before the plan. It does not stop a step that changes the disk, because the kernel finishes a resize even after `edc` kills the command. If you press Ctrl+C, `edc` lets the current step finish and stops before the next step.

The grow is permanent. A partition or a file system cannot shrink back, and xfs cannot shrink at all. Take a snapshot of the volume before you grow it.

`edc` refuses these cases and changes nothing:

- The file system is not ext3, ext4, or xfs.
- Another partition starts after the partition. `edc` finds the last partition by its start, not by its number. The Ubuntu cloud image keeps the root partition `1` at the end of the disk.
- The disk uses an MBR table, and the partition reaches the 2 TiB limit of MBR. Convert the table to GPT first.
- The LV spans more than one PV, or a device-mapper device is not an LVM volume.
- `growpart` is not installed. Install `cloud-guest-utils` on Debian and Ubuntu, or `cloud-utils-growpart` on RHEL.

`check` needs no root, but it shows every size only as root. Without root, `edc` cannot read the ext superblock or the LVM report. Without a mount, `check` shows the mounted ext3, ext4, and xfs file systems on block devices.

`grow` asks before it changes the disk. Use `--yes` to skip the question in a script. If you answer no, `grow` stops with exit code `4`.

## Probe live line

A single probe command shows one progress line if stdin and stdout are terminals. The line has the probe name, the target, the elapsed time, and the last output line of the command.

```
⠋     net.trace                 example.com  2.9s  ·   4  <ip:778fad8d>  5.573 ms
```

`edc` starts this line only if the probe runs longer than 300 milliseconds. A fast probe prints the result and nothing else.

Press Ctrl-C to cancel. `edc` stops the command and returns exit code `4`.

The line is always one line. Long output gets a cut at the terminal width.

## Missing arguments

If you run a command with no target and stdin is a terminal, `edc` asks for the value. `edc` then prints the command with the value, so you type it directly next time.

```bash
$ edc doctor
target (host or URL): example.com
→ edc doctor example.com
```

`edc doctor` and the single probe commands ask for a target. The question uses the form from the usage line. `edc tcp check` asks for `host:port`, and `edc http check` asks for a `URL`.

`edc capture` needs `--interface`. Without it, `edc` lists the interfaces that carry an address and marks the one that the default route uses. You pick a line instead of a name.

```bash
$ edc capture
which interface do you want to capture?
> en0        192.168.1.92   default route
  bridge100  192.168.139.3
→ edc capture --interface en0
```

A command group asks which command to run. `edc dns`, `edc net`, `edc report`, `edc route`, `edc change`, `edc disk`, and `edc completion` show their commands. `edc report show` and `edc report diff` then list the reports in the current directory.

`edc change confirm`, `edc change rollback`, and `edc route rollback` list the changes that still wait. You pick one instead of copying a run id.

If stdin is not a terminal, `edc` prints the usage and returns exit code `2`. A script keeps the same behavior. `edc` asks only for a value that the command needs. It never asks for an optional setting such as `--duration` or `--filter`.

`edc change apply` and `edc route switch` are the exception. They change a host, so they take every value from a flag. A command that you cannot undo must stay a command that you can type again.

## Doctor live screen

If stdin and stdout are terminals, `edc doctor` shows one line for each probe and updates the line when the probe ends. The finished lines stay on the screen. The details and the summary follow.

Use `edc doctor --all-ips example.com` to check every address returned by DNS. It connects directly to each IP with the original Host and TLS SNI, then lists TCP, TLS, and HTTP results per IP. These HTTP checks read response headers only and do not follow redirects or use a proxy. The option also asks RIPEstat for each public IP's origin ASN and registered ASN holder; lookup failures do not change endpoint health. If any IP fails, `endpoints.check` fails. The checks test each entry point; they cannot establish whether a load balancer exists or where a server is physically located.

Press Ctrl-C to cancel. `edc` stops the running probes and returns exit code `4`.

`edc` waits and prints all lines at the end if stdin or stdout is not a terminal, if `--json` is set, or if `NO_COLOR` is set.

## Packet capture

Only `capture` uses a privilege. `doctor` does not use `sudo`. Capture supports Linux and macOS. Capture keeps hard limits of 60 seconds and 10,000 packets. Capture does not overwrite an existing file. Install `tcpdump` in `/usr/sbin`, `/usr/bin`, `/sbin`, or `/bin`. Capture does not search `PATH` for `tcpdump` or `sudo`.

Use `--mode events` on Linux to record TCP socket state, retransmission, reset, and destroy events with process metadata. The mode writes JSONL and requires BTF and eBPF capabilities. It does not create a PCAP file. In each event, `timestamp_ns` is Unix epoch time in nanoseconds, the same clock as the summary line. `boot_time_ns` is the kernel monotonic time since boot. In a TCP event, `pid` and `process` show the process that uses the socket, not the task that ran on the CPU when the kernel handled the packet. If you press Ctrl-C, capture stops collection. It writes the events that it collected and the summary line.

Use `trace tcp` or `trace udp` on Linux or macOS to print network events as they arrive. The default terminal view scrolls through events. The command runs until you press Ctrl-C. It then prints a summary.

On Linux, `trace tcp` also reports change-based `tcp_sample` events. These events show RTT, RTT variation, congestion window, slow-start threshold, unacknowledged packets, lost packets, and zero-window state. A valid field can contain `0`. An absent field is unavailable. RTT and RTT variation use microseconds. Linux stores `srtt_us` with three fixed-point bits and `rttvar_us` with two fixed-point bits. edc shifts these values before output. Zero-window means that an ESTABLISHED TCP socket has `snd_wnd` equal to zero. It does not mean that the accept queue is full. `tcp_sample` does not run on each send or receive event. It runs when a connection reaches ESTABLISHED or retransmits, and only emits changed values.

`tcp_accept` records TCP handshake completion. `tcp_app_accept` records a successful return from `inet_csk_accept`. It reports the time from child ESTABLISHED to application accept and the listener accept queue values. Failed accepts do not produce `tcp_app_accept`. `ListenDrops` and `ListenOverflows` are network namespace counters. edc does not assign them to one listener.

Use Enter in the terminal view to see raw event JSON. The TCP connection JSON includes the last observed diagnostic values. The text summary shows mean RTT, lost packets, zero-window state, and mean application accept time for each process and peer. Linux 5.15 load and attach verification is not complete.

Use `trace io` on Linux to measure block I/O latency. It records queue latency from request insert to issue, service latency from issue to complete, and total latency. It keeps the submitter process and cgroup at request insert. The completion context does not change that attribution. A device with the `none` I/O scheduler sends most requests to the driver without an insert. For these requests, `attribution` is `issue`, and the event has no `queue_ms`. The process and cgroup then come from the issue context, and this context can be a kernel worker.

```bash
./bin/edc trace io --duration 15s
./bin/edc trace io --device 8:0 --group-by device
./bin/edc trace io --process postgres --raw
./bin/edc trace io --container database --json io.json
```

`trace io` reports read and write operations, bytes, and average, p95, and maximum queue, service, and total latency. By default, it emits requests at or above 1ms. Use `--slow <duration>` to change this threshold, for example `--slow 200us` on a fast NVMe device. The summary reports ring loss, pending-map insertion failures, unmatched completions, incomplete requests, and requeues. A request without saved issue state has no invented latency or process owner.

Use `--group-by device`, `--group-by process`, `--group-by cgroup`, or `--group-by event` for I/O reports. `--raw` writes each completed request and then the summary as JSONL. In each event, `timestamp_ns` is Unix epoch time in nanoseconds, the same clock as the summary line. `boot_time_ns` is the kernel monotonic time since boot. `--json` writes the summary report. `trace io` requires kernel BTF, block request tracepoints, `CAP_BPF`, and `CAP_PERFMON`. It does not require `CAP_NET_ADMIN`. It finds the tracepoints in kernel BTF and does not require tracefs, so it runs in a container without a tracefs mount.

```bash
./bin/edc trace tcp
./bin/edc trace tcp --duration 15s
./bin/edc trace tcp --process slackbot --destination 100.66.11.194:443 --json trace.json
./bin/edc trace tcp --raw
./bin/edc trace udp --duration 15s
./bin/edc trace udp --group-by target
./bin/edc trace udp --group-by source
./bin/edc trace udp --group-by port
./bin/edc trace tcp --group-by process
./bin/edc trace tcp --group-by event
./bin/edc trace tcp -d
```

Use `trace sched` on Linux to measure scheduler delays. The trace reports wakeup-to-run after sleep, runnable queue after preemption, and off-CPU spans. The off-CPU spans include the two delay types as subsets. It reports voluntary and preempted off-CPU spans separately. The trace does not identify sleep, lock, or I/O causes.

```bash
./bin/edc trace sched --duration 15s
./bin/edc trace sched --process api --group-by process
./bin/edc trace sched --group-by event --json sched.json
./bin/edc trace sched --raw
```

By default, the trace emits spans of at least 1 ms. Use `--slow <duration>` to change this threshold, for example `--slow 100us`. It uses fixed pending maps and a fixed ring buffer. The summary and raw JSON summary show ring loss, map full, unmatched, repeated wakeup, and omitted span counts. On Linux kernels that expose the required BTF fields, raw events include the cgroup v2 ID and `--container` filters this ID. Use `--group-by cgroup` to group rows by this ID. edc verifies these fields before it loads the program. The trace works on Linux 5.15. In each raw event, `timestamp_ns` is Unix epoch time in nanoseconds, the same clock as the summary line. `boot_time_ns` is the kernel monotonic time since boot.

Use `--raw` to print JSONL events as they arrive. Use `--json` to write the connection summary after Ctrl-C.
The text summary at the end groups the rows by process and peer. A client row shows the destination. A server row shows the local service, for example `127.0.0.1:2379 (server)`, because each client uses a different port. A TCP row shows the number of connections, the connections for each result, the mean connect time, and the traffic. A UDP row shows the datagrams and the traffic. Use `-d` or `--detail` to show one row for each connection or UDP flow. The JSON output always has one row for each connection or flow. The `trace dns` summary always has one row for each name and record type, so `-d` does not change it.
The connection detail shows one row for each socket, from its creation to its destruction. The kernel can give the address of a closed socket to a new socket, so edc starts a new row when a socket is destroyed. The summary keeps the rows of the open connections and of the last 1,000 closed connections. The totals count all connections. `connections_omitted` in the JSON output shows the number of closed connections that have no row.
Each row has one of these results:

- `established`: the trace saw the connect or the accept. A later reset does not change the result. The `RESET` column shows the reset.
- `failed`: the connection was not established. The connect failed before the handshake, for example because no route exists, or the connection closed or got a reset during the handshake.
- `incomplete`: the handshake did not finish before the trace ended.
- `existing`: the connection was open before the trace started, so the trace did not see its handshake.

`Attempts` counts the connections with a handshake in the trace. It is the sum of `Established` and `Incomplete`, and `Incomplete` counts the `failed` and `incomplete` rows. `Existing` counts the `existing` rows. Listening sockets and sockets that close without a connect are not connections, so they have no row.
Use `--duration 15s` to stop after 15 seconds. `--live` remains accepted for compatibility.
Use `--group-by source`, `--group-by target`, `--group-by port`, `--group-by process`, or `--group-by event` to show one live row for each selected dimension. TCP rows show connect, retransmission, reset, and traffic values. UDP rows show TX and RX traffic values. `EVENT/s` is an event count rate. TX and RX bytes are socket payload bytes. B/s is a byte rate. bps and Mbps are bit rates. Mbps uses decimal units: bps / 1,000,000.
`--group-by source` groups events by the source host. It ignores the source port because the OS assigns a new port to each connection.
`--group-by event` groups events by the event name, for example `tcp_connect` or `tcp_retransmit`.
`--group-by port` groups events by the peer port. A server socket uses its local port, for example `53 (server)`. If a port row has more than one peer, the row shows the number of peers, for example `3478 (65 peers)`.
`--group-by process` groups events by the process name. Processes with the same name share one row, and the `--process` filter uses the same name. On Linux, edc uses the name of the process, as `ps` shows it, and not the name of the thread that made the event. The kernel keeps only the first 15 bytes of the name. If edc cannot find the process of an event, the event goes to the `-` row.
`--group-by target` shows one `(server)` row for each local service, such as `127.0.0.53:53 (server)`, when an event has no target and comes from a server socket. edc treats a socket as a server if its local port is outside the ephemeral port range and the peer port is inside it.
edc picks the target of an event in this order: a DNS answer that the same process received during the trace, the command line of the process, and then the last name seen for the address in any DNS answer or in the systemd-resolved cache. The `target_source` field of a JSON event shows `dns`, `command`, or `resolver-cache`. Two names can share one address, so a name from another lookup can be wrong.
The kernel records retransmissions, resets, and some state changes outside the process that owns the socket. If edc did not see the owner use the socket during the trace, the process of these events is `-`. For example, a socket that connected before the trace started has no known owner until it sends or receives data.

Use `--container <name|id>` to show only the events of one docker container on Linux. edc asks `docker inspect` for the first process of the container and reads its cgroup v2 directory. Then edc keeps the events of that cgroup and of the cgroups under it. Processes from `docker exec` are in the same cgroup. edc reads the cgroups once, when the trace starts. If the container restarts, start the trace again. An event without a known owner has no cgroup, so `--container` drops it, as `--process` does. `trace arp` and `trace ndp` do not accept `--container`, because their events have no process. The full-screen header shows the container name.

```bash
./bin/edc trace http --side server --container web
./bin/edc trace tcp --container 5c9e77b3f470 --raw
```

In a bridge network, the addresses and ports are the addresses and ports inside the container. For example, a server published with `-p 8080:80` shows port 80.

In the full-screen terminal view, press `s` for source rows, `t` for target rows, `p` for port rows, `c` for process rows, `e` for event rows, or `g` for scrolling events. In `trace http`, press `u` for path rows. Press `Tab` to show the next view. Press `Shift+Tab` to show the previous view. If the terminal is wide, the first column becomes wider and shows the full group value. If the terminal is narrow, the byte columns use short units, for example `195K` for 195 KiB.
The full-screen view keeps the last 10,000 events. The live rates use the time that these events cover. The summary after Ctrl-C uses all events.
Grouped rows in the full-screen view show the groups with the most traffic first. If the rows do not fit the terminal, the view shows the top rows.

In the event list, press Up or Down (or `k` and `j`) to select an event. While an event is selected, the list stops and does not follow new events. The header shows the number of newer events. Press Enter to see all the fields of the event and the whole payload. The detail view wraps long lines. Press Up, Down, PgUp (or `b`), PgDn (or Space), `g`, or `G` to scroll, and press Esc or `q` to go back. Press `l`, Esc, or End in the list to follow new events again. Many Mac keyboards have no End key. In the list, `b` and Space also move the selection by a page. If the selected event becomes the oldest one in the list, the list shows newer events below it. The list keeps the first 4 KiB of each payload. The detail view shows the whole payload of recent events, up to 64 MiB in total.

Press `f` to open the detail view on the newest event and follow new events. Press `f` again to stop at the event on the screen. In `trace http`, the full screen collects the first 4 KiB of each message also without `--payload`. The list hides the payload lines until you press `v`. With `--payload`, the list shows them from the start. Press `m` to show or hide the values of the headers that `--payload` hides. While they are visible, the header shows `secrets shown`. In the detail view, press `z` to decode a gzip body. edc decodes up to 1 MiB and shows how many bytes it decoded.

Press `i` to split the screen. The list uses the top half, and a preview of one message uses the bottom half. The preview shows the selected event. If no event is selected, the preview shows the newest event and changes when a new event arrives. If the event has a payload, the preview shows the payload. Otherwise, the preview shows the fields of the event. Press `J` and `K` to scroll the preview, and press Enter to open the full detail view. In `trace http`, `m` and `z` also change the preview. If the terminal has fewer than 9 lines, edc shows only the list.

The title line of the preview and of the detail view starts with the time of the event, for example `09:05:07.123`. The time is in the time zone of this host. A line of `─` fills the rest of the preview title line, so the border between the list and the preview is clear also without color. If the terminal has 120 columns or more, the event list shows a `TIME` column before `PROCESS`.

On macOS, `trace` reads socket counters from the kernel network statistics interface (`com.apple.network.statistics`). `nettop` uses the same interface. `edc` reads the counters each second and makes events from the changes. The interface is private, so a macOS update can change its format. If the format changes, the trace stops with an error.

On macOS, some values are not available. The text output shows `-` for these values. The JSON output shows `null`.

- `connect_ms`: The counters do not give the connection time.
- `reset` and `resets`: The counters do not show RST packets.
- `retransmissions`: The counters give retransmitted bytes, not a packet count. In `--raw` output, the `bytes` field of each `tcp_retransmit` event holds these bytes.
- The destination of an unconnected UDP socket: The kernel does not record the destination of each datagram. The destination is empty, and the tables show `-`.

On macOS, each event holds the change since the previous reading. The `bytes` field holds the bytes, and the `packets` field holds the packet count. UDP `sent` and `received` values count packets. `EVENTS` and `EVENT/s` count these events, not socket calls. The byte totals and traffic rates are exact.

On macOS, the process name holds up to 32 bytes. `edc` reads the ephemeral port range from `net.inet.ip.portrange.first` and `net.inet.ip.portrange.last`.

Without root, the macOS trace shows only the kernel sockets of the current user. It does not show the connections that the user-space network stack of macOS handles. Network.framework can send traffic through that stack. The trace also does not show a `target` hostname, so `--group-by target` groups events by the destination address.

If you run the trace with `sudo`, it shows the processes of all users and the connections of the user-space network stack. It also shows the domain name that macOS records for a connection as the `target`, and `target_source` shows `system`. If macOS has no domain name for a connection, `--group-by target` uses the destination address. QUIC uses UDP, so `trace udp` shows the QUIC connections of the user-space network stack.

Use `trace dns` on Linux to print DNS queries and answers as they arrive. It reads the DNS messages of UDP port 53 and also shows TCP connections to port 53.

```bash
./bin/edc trace dns
./bin/edc trace dns --duration 15s --json dns.json
./bin/edc trace dns --group-by target
./bin/edc trace dns --process curl --destination 127.0.0.53:53
```

A query is a `dns_query` event. An answer gets the name of its result code, for example `dns_noerror`, `dns_nxdomain`, or `dns_servfail`. A successful answer without records is `dns_nodata`.

An answer with the TC bit is `dns_truncated`. The answer did not fit in UDP, so the client asks the same server again over TCP. A TCP connection to port 53 is `dns_tcp_connect`, or `dns_tcp_fail` if the connection fails. The DNS messages in the TCP connection are normal query and answer events. Events over TCP have `"transport": "tcp"`. If the same process got a `dns_truncated` answer from that server, the TCP event gets the name of that query.

`Errors` counts `dns_tcp_fail` and all answers except `dns_noerror`, `dns_nodata`, and `dns_truncated`. `TCP connections` in the summary counts `dns_tcp_connect` and `dns_tcp_fail`.

The `target` of a DNS event is the name in the query. The `destination` is the DNS server. In the scroll view, the `DESTINATION` column shows the name, the record type, and the server. The `EVENT` column shows the result and the latency. So `--group-by target` groups events by name, and `--destination` filters events by server. The DNS server port is always 53, so `trace dns` has no port view.

edc matches an answer to the query with the same local port, server, and transaction ID. `latency_ms` starts when the kernel sends the query and stops when the process reads the answer.

edc also divides this time into two parts. `network_ms` stops when the answer enters the receive queue of the socket. `read_delay_ms` starts at that point and stops when the process reads the answer. A long `read_delay_ms` shows a busy or slow program, not a slow DNS server.

If the process sends the same query again before the answer, the answer counts for all these queries. The latency starts at the first query.

`Unanswered` counts the queries without an answer at the end of the trace. In the full-screen view, `NOANS` counts the queries that wait for an answer.

The summary after Ctrl-C shows one row for each name and record type. Grouped rows show queries, answers, errors, and unanswered queries instead of byte counts. They also show the average and maximum latency, and the average network time (`NETms`) and read delay (`RDms`).

On a host with systemd-resolved, one lookup can appear two times: between the program and `127.0.0.53`, and between systemd-resolved and the upstream server. If a name has only the program row, systemd-resolved answered from its cache.

Use `--side server` to watch a local DNS server, for example systemd-resolved, dnsmasq, or CoreDNS. It shows the queries that the server receives on port 53 and the answers that it sends. The events use the same names as the client side, and JSON events and reports add `"side": "server"`. The `destination` of a server event is the client, and the `process` is the DNS server. `--group-by source` groups server events by the address that the server listens on.

```bash
./bin/edc trace dns --side server
./bin/edc trace dns --side server --group-by target
```

On the server side, the latency starts when the server reads the query and stops when the server sends the answer. The `read_delay_ms` of a server query is the time that the query waited in the receive queue of the server. A short latency usually shows an answer from the cache. A long latency usually shows a query to an upstream server. On the server side, `dns_tcp_accept` shows a TCP connection that the server accepts on port 53. If the server sent a truncated answer to that client, the event gets the name of the query. edc learns the process of a listen socket when the server calls `accept()`. So for a server that started before the trace, the first accepted connection can have no process.

`trace dns` watches only port 53. DNS over TCP puts a 2-byte length before each message. Many programs write the length and the message as two buffers, or read them in two reads. edc reads the first two buffers of a write, and it joins a 2-byte read with the next read on the same connection. It does not find a message that starts in the middle of a read. It does not show DNS over TLS, DNS over HTTPS, mDNS, or LLMNR. BPF reads the first 1,024 bytes of a DNS message. For a longer answer, edc reads the result code from the header and takes the name from the query. macOS does not give the payload of a socket, so `trace dns` requires Linux.

Use `trace arp` on Linux or macOS to print the changes of the IPv4 neighbor table (the ARP cache). It needs no root and no eBPF. On Linux, it reads kernel notifications through netlink as they arrive. On macOS, it reads the ARP table each second, with the same query as `arp -an`, and compares it with the previous table.

```bash
./bin/edc trace arp
./bin/edc trace arp --group-by target
./bin/edc trace arp --json arp.json --duration 30s
```

The entries that exist when the trace starts do not make events. After that, each change is one event:

- `arp_new`: a new entry.
- `arp_state`: the state changed, for example from `REACHABLE` to `STALE`. `old_state` and `new_state` show the states.
- `arp_mac_change`: the MAC address of an IP changed. `old_mac` and `mac` show the addresses. A gateway failover, an IP conflict, or a spoofed ARP answer can cause this event.
- `arp_failed`: the kernel did not get an answer for the IP.
- `arp_delete`: the kernel removed the entry.

The `target` of an ARP event is the IP address, and the `source` is the interface. ARP events have no process or port, so `trace arp` has no process view and no port view. `--destination` filters events by IP address. ARP events have no process, so `--process` matches no ARP event. The summary after Ctrl-C shows one row for each interface and IP. Grouped rows show the number of MAC addresses (`MACS`), the MAC changes (`CHG`), and the failures (`FAIL`).

The kernel does not report the start of an address lookup, so a failed lookup shows only `arp_failed`. `trace arp` does not show the entries without ARP (`NOARP`). It watches the neighbor table, not the ARP packets. So it does not show ARP packets that do not change the table, for example requests from other hosts. On macOS, a change that starts and ends between two reads does not show, and a lookup that fails again without a change does not show again. macOS has no neighbor states such as `STALE`. `trace arp` shows `COMPLETE` for an entry with a MAC address, `INCOMPLETE` for an entry without one, `PERMANENT` for a static entry, and `FAILED` for an entry that macOS marks as rejected (`RTF_REJECT`).

On Linux, `trace arp` asks for an 8 MB netlink receive buffer. If the kernel reports many changes at one time, for example after a table flush, the buffer can overflow. Then the kernel drops changes. `Lost events` counts the overflows, not the dropped changes. One overflow can drop many changes, so the summary shows that the number of missed changes is unknown. Without root, the kernel limits the buffer to `net.core.rmem_max`. If `trace arp` shows lost events without root, increase `net.core.rmem_max`. You can also run it as root.

Use `trace ndp` for the IPv6 neighbor table (NDP). It works the same way as `trace arp` on Linux and macOS. Its events are `ndp_new`, `ndp_state`, `ndp_mac_change`, `ndp_failed`, and `ndp_delete`, and its summary title is `NDP trace`.

```bash
./bin/edc trace ndp
./bin/edc trace ndp --group-by target
```

On macOS, the kernel puts the interface number into the link-local addresses of the table. `trace ndp` removes it, so a link-local address shows as, for example, `fe80::1`, and the `source` column shows the interface.

Use `trace http` on Linux 5.15 or later to print plain HTTP/1.x requests and responses as they arrive. edc reads the first 512 bytes of each TCP read and write in the kernel, or the first 4 KiB with `--payload`. It keeps the method, the `Host` header, the path, and the status code. It removes the query from the path, because the query can contain tokens. Without `--payload`, it also drops the other headers and the body.

```bash
./bin/edc trace http
./bin/edc trace http --group-by target
./bin/edc trace http --group-by path
./bin/edc trace http --side server --process nginx
./bin/edc trace http --payload
./bin/edc trace http --side server --port 8080
./bin/edc trace http --port 8080 --payload=all
```

A request is an `http_request` event. A response is one of `http_1xx` to `http_5xx`, and `status` has the code. HTTP/1.x answers the requests on one connection in order. So a response matches the oldest request on the same connection that has no answer. `latency_ms` starts when the client sends the request and stops when the client reads the response. A `1xx` response does not end the request.

The `target` of an HTTP event is the `Host` header. If there is no `Host` header, the target is the server address. On the server side, `latency_ms` starts when the server reads the request and stops when the server writes the response.

`trace http` shows two sides of HTTP on this host. Each event row starts with the side:

- `client:` is a request that this host sent. For example, a proxy sends requests to its backend, or a program calls an API.
- `server:` is a request that a local server received.

JSON events have `"side": "client"` or `"side": "server"`. Use `--side client` or `--side server` to keep one side. In the full-screen view, press `/` and type `server` to keep the server rows. `trace dns` shows only the client side by default.

A proxy shows one request on each hop. For example, nginx receives requests on port 9900 and sends them to a backend on port 9000 on the same host. Then one user request gives three request rows: `server:` for nginx on port 9900, `client:` for nginx to port 9000, and `server:` for the backend on port 9000. The grouped views keep the server side in separate rows, for example `nginx (server)`.

Use `--group-by path` to group the events by the request path. In the full-screen view, press `u`. Only `trace http` has the path view. The path has no query, and one path row includes all hosts and methods. A response goes into the row of its request. A `tls_hello` event and a response without a request go into the `-` row. If the path contains an ID, for example `/users/123`, each ID gets a different row.

| To see | Command |
| --- | --- |
| All HTTP on this host | `./bin/edc trace http` |
| The requests that local servers received | `./bin/edc trace http --side server` |
| The requests that this host sent, for example to a backend or an API | `./bin/edc trace http --side client` |
| Both ends of the connections to port 9000 | `./bin/edc trace http --port 9000` |
| Only the requests that the proxy on port 9900 received | `./bin/edc trace http --side server --port 9900` |
| The latency of each process on each side | `./bin/edc trace http --port 9000 --group-by process` |
| The requests, errors, and latency of each path | `./bin/edc trace http --group-by path` |
| The HTTP/1.1 and HTTP/2 requests in HTTPS | `./bin/edc trace http --tls` |

The client latency and the server latency of one hop measure different times. The client latency includes the network and the wait before the server reads the request. The server latency includes only the work of the server. If the client latency is much larger than the server latency, examine the network and the server queue.

edc finds HTTP by the start of the data, not by the port, so it sees HTTP on any port. The `source` column is always this host, and `destination` is the peer. Use `--port` to keep the connections that use one port on this host or on the peer. With `--side server`, it shows one local server. With `--side client`, it shows the requests from this host to the servers on that port. edc checks the port in the kernel, so it does not read the data of other connections.

Use `--payload` to see the data of each message. Under each event, edc prints the body. If there is no body, it prints the headers. With `--raw`, the `payload` field has all the data. The data is the first 4 KiB (4,096 bytes) of the read or the write, so edc cuts a longer body. If a program writes the headers and the body in two writes, edc does not see the body. Each record is larger with `--payload`, so a busy server can cause lost events. The summary shows the number of lost events. `--payload` keeps the query, but it hides the values of the `Authorization`, `Proxy-Authorization`, `Cookie`, `Set-Cookie`, `X-Api-Key`, `X-Goog-Api-Key`, `Api-Key`, and `X-Amz-Security-Token` headers. edc shows the other headers, the query, and the body as they are, and they can contain tokens and passwords. Before you share the output, check it for tokens and passwords. edc changes control characters to `\xNN`, so the data cannot change the terminal. `--payload` does not work with `--json`, because `--json` writes only the summary.

Use `--payload=all` to see each whole message, up to 1 MiB. edc follows the message into the next reads and writes, and into the other buffers of a `writev`. edc prints the event when the message ends. The end is the last byte of `Content-Length`, the last chunk of a chunked body, or one second without data. So the event can come out later than with `--payload`, but the latency does not change. The plain output prints the whole message under the event. The full screen still shows one line. If edc cuts the message at 1 MiB, loses a part, or stops before the end, the event has `"payload_truncated": true`. Write `--payload=all` without a space. `--payload all` is an error. `--payload=all` uses more CPU than `--payload`, so a busy server causes lost events sooner.

Use `--show-secrets` with `--payload` to show the values of the headers that `--payload` hides. Other people can use an account with these values. Do not share output that has them. In the full screen, press `m` instead.

The summary after Ctrl-C shows one row for each side, method, host, and path. If the trace has both sides, the summary shows the totals of each side and adds a `SIDE` column. JSON adds the `client` and `server` objects with the totals of each side. Grouped rows show the requests, the responses, the 4xx and 5xx responses, the unanswered requests, and the average and maximum latency.

HTTPS is encrypted, so edc cannot read the method, the path, or the status. The TLS ClientHello at the start of each connection is plain text. edc reads it and shows a `tls_hello` event. The `target` is the server name (SNI). The `alpn` field lists the protocols that the client offers, for example `h2` and `http/1.1`, and the event row shows the first one. A client that sends a ClientHello makes a `client:` row. A local server that receives one makes a `server:` row. The summary counts these connections in a separate `TLS connections` table, and JSON adds `tls_connections` and `tls`. Use `--port 443` to see only the HTTPS connections on port 443.

edc does not see a TLS connection that started before the trace. If a client uses Encrypted Client Hello (ECH), the SNI is the public name of the provider. To see the requests of HTTPS, use `--tls`. You can also trace the plain HTTP behind the TLS end point, for example a proxy that sends plain HTTP to its backend.

Use `--tls` to see the HTTP/1.1 and HTTP/2 requests in HTTPS. edc reads plaintext before encryption and after decryption. It supports OpenSSL, GnuTLS, NSS, wolfSSL, Mbed TLS, rustls-ffi, Go TLS, and registered BoringSSL builds. It does not need a certificate or a key.

```bash
./bin/edc trace http --tls
./bin/edc trace http --tls --side server --port 443
./bin/edc trace http --tls=/usr/local/bin/node
```

Without a value, `--tls` finds these files when the trace starts:

- the `libssl`, `libgnutls`, `libssl3`, `libnspr4`, `libwolfssl`, `libmbedtls`, and `librustls` of this host in the standard library directories
- each of these libraries that a process loads, also in a container
- the program file of a process, if the file contains OpenSSL and exports `SSL_read`, for example `node`
- a stripped BoringSSL program with a supported GNU build ID

edc opens the file that each process loaded. So edc also sees a process that still uses an old `libssl` after a package update. To open these files, edc needs `CAP_SYS_ADMIN` or `CAP_CHECKPOINT_RESTORE`. The probes can also need `CAP_SYS_ADMIN`. Root has it. In a container, add `SYS_ADMIN` with `--cap-add` or use `--privileged`. If edc cannot open these files, it shows a notice.

edc continues the search while the trace runs. When a process starts a program, edc checks that process several times in the next 3 seconds. It also checks all the processes again at an interval of 2 seconds or more. On a host with many processes, the interval is longer. If a process uses a new file, edc adds the probes to that file. edc does not see the requests that the process sends before that. The kernel sends the start of a program only to the first network namespace. If edc runs in another network namespace or PID namespace, for example in a container without the host network or `--pid=host`, edc shows a notice and uses only the check of all the processes. If the kernel stops sending the start of programs during the trace, edc tells you after the trace.

To watch only one file, use `--tls=<path>`. Then edc does not search for other files. Write the path without a space, as with `--payload=all`.

edc attaches the probes to this file. If a program loads a copy of the library from another file, edc does not see that program. To find the file that a process loads, read `/proc/<pid>/maps`.

NSS needs two libraries: `libssl3` for the TLS state and `libnspr4` for the plaintext I/O. If you specify either library, edc also selects its companion from the same directory or a standard library directory.

```bash
./bin/edc trace http --tls=/usr/lib/x86_64-linux-gnu/libssl3.so
```

Start the trace before the NSS program starts. edc needs the SSL setup calls to distinguish TLS from plain files and sockets.

NSS uses `PR_Read`, `PR_Recv`, `PR_Write`, and `PR_Send` for plaintext. edc follows `SSL_SECURITY`, its default value, model copies, accepted connections, and `PR_Close`.

NSPR also uses these functions for plain files and sockets. So each NSPR read and write runs a probe, also in a program without TLS.

An NSS connection with `SSL_SECURITY` off produces no TLS event. `PR_Recv` with `PR_MSG_PEEK` also produces no TLS event.

Go TLS uses the Go function table and probes at function returns. The verified scope is Go 1.26.8 and 1.27.1 on Linux amd64 and arm64.

```bash
./bin/edc trace http --tls=my-go-program
```

Specify the binary path or command name. edc supports normal, stripped, and PIE binaries. Automatic library discovery does not select Go binaries.

These events have no socket addresses. A `--port` filter excludes them. For Go TLS, edc supports no other version or architecture.

For Go clients, latency starts at `Write` entry and ends at `Read` return. For servers, latency ends at `Write` entry.

The capture tests cover Go HTTP/1.1 and HTTP/2, including concurrent streams, repeated headers and fragmented bodies.

rustls-ffi uses the C functions `rustls_connection_read` and `rustls_connection_write`. edc clears the connection state at `rustls_connection_free`.

```bash
./bin/edc trace http --tls=/usr/local/lib/librustls.so
```

These events have no socket addresses. A `--port` filter excludes them. `--tls` does not see programs that use the native Rust API of rustls.

Mbed TLS uses `mbedtls_ssl_read`, `mbedtls_ssl_write`, `mbedtls_ssl_read_early_data`, and `mbedtls_ssl_write_early_data`. edc clears the connection state at `mbedtls_ssl_session_reset` and `mbedtls_ssl_free`. On a server, `mbedtls_ssl_read_early_data` returns data that the handshake already read. So its events have no socket address, and a `--port` filter excludes them.

```bash
./bin/edc trace http --tls=/usr/local/lib/libmbedtls.so
```

edc does not read DTLS connections.

wolfSSL uses `wolfSSL_read` and `wolfSSL_write`, or their `_ex` variants. edc clears the connection state at `wolfSSL_free`.

```bash
./bin/edc trace http --tls=/usr/local/lib/libwolfssl.so
```

Use `--tls=claude` to find an executable in PATH. A file in the current directory takes priority over PATH.

edc supports the stripped amd64 Bun 1.4.3 runtime in Claude Code 2.1.291. Its GNU build ID is `ca2032b38650b44e05b2074617d524c7475c80f0`.

edc checks the build ID and the function code before it attaches probes. Other stripped BoringSSL builds need a separate entry with checked offsets.

```bash
./bin/edc trace http --tls=claude
```

With `sudo`, `PATH` often does not include `~/.local/bin`. If edc does not find the program, give the path from your shell:

```bash
sudo ./bin/edc trace http --tls="$(command -v claude)"
```

An HTTPS request or response from OpenSSL, GnuTLS, NSS, wolfSSL, Mbed TLS, rustls-ffi, Go TLS, or BoringSSL has `"tls": true`. The event row shows `tls` after the event name. The destination starts with `https://`. A plain HTTP destination starts with `http://`. The path, the status, the latency, the grouped views, `--payload`, and the summary work as with plain HTTP. `--payload` hides the same header values. The body of HTTPS often contains tokens. Before you share the output, check it for tokens.

edc reads HTTP/2 over TLS as it reads h2c. It shows the method, the path, the status, and the latency of each stream. To follow the frames and the header tables, edc reads all the plaintext of an HTTP/2 connection. So a busy HTTP/2 connection costs more than HTTP/1, and it can make edc lose events of other connections. To reduce the cost, use `--port`. If edc loses plaintext of an HTTP/2 connection, it stops reading that direction and counts lost events. edc does not read an HTTP/2 connection that started before the trace. With HTTP/2, `--payload` shows a start line and the body. edc makes the start line from the method and the path, or from the status. It does not show the headers. edc prints an HTTP/2 event when its body ends. `--payload` keeps the first 4 KiB of each body, and `--payload=all` keeps up to 1 MiB. If edc cuts the body at the limit, or the body does not end before the stream or the trace ends, the event has `"payload_truncated": true`. If more than 4096 bodies or 64 MiB of bodies wait, edc prints the oldest event early with this field. edc does the same when it stops reading a direction. Without `--payload`, the full screen shows no HTTP/2 body, and it shows each HTTP/2 event when its headers arrive.

Some programs do not use the socket inside the TLS call, for example Bun, `node`, and Python `asyncio`. Then edc does not know the connection. The event has the process, but it has no `source` and no `destination`. The `target` is the `Host` header. The summary shows the number of these events as `TLS plaintext without an address`, and JSON adds `tls_unmapped`. With `--port`, edc cannot check the port of this plaintext. So edc does not show it and adds it to the same number.

`--tls` sees programs that call the OpenSSL functions `SSL_read` and `SSL_write`, or `SSL_read_ex` and `SSL_write_ex`. It also sees programs that call the GnuTLS functions `gnutls_record_recv` and `gnutls_record_send`, for example `wget` and `git` on Debian and Ubuntu. NSS, wolfSSL, Mbed TLS, and rustls-ffi programs also work, as described above.

`--tls` does not see Java programs. Go support has the limits above. It also does not see a program without the exported functions of these libraries, except supported Go binaries and the supported BoringSSL build.

With `--tls=<path>`, edc also reads the symbol table, so a static program with symbols can work.

`--tls` does not see a program that reads through the SSL BIO of OpenSSL, for example `openssl s_server -www`.

edc reads the plaintext without a page fault. If the kernel changes the page at that moment, for example when it splits or merges a transparent huge page, edc cannot read the plaintext of that call. This is rare, and edc counts it as a lost event.

Each call of these functions runs a probe in each process that uses the files. This cost also applies to the processes that `--process` hides. At the end, the kernel removes each probe, so the trace can stop a few seconds after Ctrl-C. `--tls` needs no newer kernel than `trace http`.

On amd64 with Linux 6.11, 6.12 before 6.12.14, or 6.13 before 6.13.3, a process under a seccomp filter can stop when a TLS call returns. A Docker container uses such a filter. On these kernels, edc shows a warning before it attaches the probes. Before the full screen opens, edc waits for Enter. To stop, press Ctrl-C. A distribution kernel can include the fix.

Without `--tls`, `trace http` does not show the requests in HTTPS, because the kernel sees only encrypted data. It reads HTTP/2 without TLS (h2c), for example gRPC inside a cluster. It shows the method, the path, the status, and the latency of each stream. To follow the frames and the header tables, edc reads all the bytes of an h2c connection, so a busy h2c connection costs more than HTTP/1. These bytes share one buffer with the other HTTP records, so a busy h2c connection can also make edc lose events of other connections. To reduce the cost, use `--port`. If edc loses bytes of an h2c connection, it stops reading that direction and counts lost events. It does not show the requests in HTTP/3, because they use binary frames. HTTP/3 uses UDP, so it also has no `tls_hello` event. edc finds a message only at the start of a read or a write. If one read has the end of a response and the start of the next response, edc misses the next response. If a program writes one message from several buffers, edc reads only the first buffer. So the `Host` header must be in the first buffer and in the first 512 bytes. If it is not, the target is the server address. edc supports this field on Linux 5.15 or later.

Use `trace mysql` on Linux 5.15 or later to print plain MySQL commands and results as they arrive. edc reads the start of each TCP read and write on the MySQL port in the kernel. The default port is 3306. Use `--port` for another port.

```bash
./bin/edc trace mysql
./bin/edc trace mysql --side server
./bin/edc trace mysql --port 3307
./bin/edc trace mysql --slow 250ms
./bin/edc trace mysql --group-by process
./bin/edc trace mysql --show-secrets
./bin/edc trace mysql --raw
```

`trace mysql` shows two sides, like `trace http`. Each event row starts with the side:

- `client:` is a command that this host sent to a MySQL server.
- `server:` is a command that a local MySQL server received.

JSON events have `"side": "client"` or `"side": "server"`. Use `--side client` or `--side server` to keep one side. If the local port is the MySQL port, the socket is on the server side. If the peer port is the MySQL port, the socket is on the client side.

Use `--slow <duration>` only with `trace mysql`, `trace io`, or `trace sched`. It accepts Go duration syntax, such as `250ms` and `1.5s`. For `trace io`, it sets the minimum request latency. For `trace sched`, it sets the minimum span latency. The default for both is 1ms. For `trace mysql`, in the ungrouped interactive view, it keeps a command row only when its first paired response has latency at or above the duration. It excludes unanswered commands, unmatched responses, responses without latency, and TLS rows. It does not change raw JSON, noninteractive output, summaries, grouped views, `--group-by`, or `--side`.

| To see | Command |
| --- | --- |
| All MySQL on this host | `./bin/edc trace mysql` |
| The commands that local servers received | `./bin/edc trace mysql --side server` |
| The commands that this host sent | `./bin/edc trace mysql --side client` |
| MySQL on port 3307 | `./bin/edc trace mysql --port 3307` |
| Paired commands at or above 250ms | `./bin/edc trace mysql --slow 250ms` |
| The latency of each process on each side | `./bin/edc trace mysql --group-by process` |
| The SQL text without the mask | `./bin/edc trace mysql --show-secrets` |

A command is one of these events. The `mysql` object of the JSON event has the fields.

- `mysql_connect` is a login. It has `user`, `database`, and `server_version`.
- `mysql_query` is a text query. It has `sql`.
- `mysql_prepare` is a prepared statement. It has `sql`.
- `mysql_execute` runs a prepared statement. It has `statement_id` and the `sql` of the prepare that edc saw. edc does not read the binary parameter values.

A response is one of these events:

- `mysql_ok` has `affected_rows`. For a prepare, it has `statement_id`. The answer to a login is also `mysql_ok`.
- `mysql_error` has `error_code`, `sql_state`, and `message`.
- `mysql_result` has `columns`, the number of columns. edc does not count or read the rows.

A response event has the `command` and the `sql` of the command that it answers. `mysql_tls` is not a command. It shows that the connection uses TLS and has `tls: true`. edc reads no more of that connection.

MySQL answers one command at a time. So the first response packet after a command is the response of that command. `latency_ms` starts when the client sends the command and stops when the client reads that packet. On the server side, it starts when the server reads the command and stops when the server writes that packet. The client latency includes the network. The server latency includes only the work of the server. edc does not read the rows after the first packet, so `latency_ms` is not the time to the last row.

`COM_QUIT`, `COM_STMT_CLOSE`, and `COM_STMT_SEND_LONG_DATA` have no response, and edc shows no event for them. Other commands, for example a ping or a change of the default database, have no event. edc skips their response.

By default, edc replaces the text in single quotes and double quotes of the SQL with `?`, for example `INSERT INTO t VALUES (1,'?')`. The mask applies to the scroll view, the full screen, `--raw`, and the summary. Numbers, names, and comments are not hidden. Do not write a secret in a comment or a number. If the server uses `ANSI_QUOTES`, edc also hides the quoted names.

Use `--show-secrets` to show the SQL as sent. It needs no `--payload`. Then a statement such as `CREATE USER ... IDENTIFIED BY 'password'` shows the password. Do not share output that has it. The `m` key of the full screen works only in `trace http`. In `trace mysql`, use `--show-secrets` on the command line.

edc does not hide the `message` of a `mysql_error` event. The message can hold values. For example, `Duplicate entry '1' for key 't.PRIMARY'` has the key value, and an access error has the user and the host. Check the output for values before you share it. edc skips the authentication data of a login and does not keep it.

The summary after Ctrl-C shows one row for each side, command, and SQL shape. The shape replaces the string values and the numbers with `?` and joins the spaces to one space. So `WHERE id = 7` and `WHERE id = 8` share one row. edc does not merge the items of an `IN` list. A row shows the number of commands, errors, and unanswered commands, the average and maximum latency, and the process. If the trace has both sides, the summary shows the totals of each side and adds a `SIDE` column. The summary also shows the number of connections, TLS connections, and compressed connections. JSON has the same content. With `--group-by`, the rows show `CMD`, `RSP`, `ERR`, `NOANS`, `AVGms`, and `MAXms`. The grouped views keep the server side in separate rows.

`trace mysql` has these limits:

- It reads TCP only. `mysql -h localhost` uses a unix socket, so `trace mysql` sees nothing. `mysql -h 127.0.0.1` uses TCP. Use `trace socket` to see the raw payload of a unix socket.
- It does not read TLS. The `mysql` client of MySQL 8.4 uses TLS on TCP by default, so many connections show only `mysql_tls`. Use `--ssl-mode=DISABLED` in a test.
- It does not read a compressed connection. It shows `mysql_connect` with `compressed: true` and the result of the login. It shows no event after that.
- It does not read the X Protocol on port 33060.
- If a command has the values of query attributes, edc cannot find the SQL. It shows the command without `sql`.
- If a client writes the packet header and the payload from two buffers in one `writev`, edc reads only the header. It does not see the command.
- A command of 16 MiB or more comes as several packets. edc shows the first part and matches the response.
- `LOCAL INFILE` has a special response. edc does not match it, so the command counts as unanswered.
- If the trace starts in the middle of a connection, there is no `mysql_connect` for it. edc finds the packet boundary at a later command, so edc can miss the first commands. If edc did not see the prepare, `mysql_execute` shows only `statement_id`.
- edc keeps the first 4 KiB of each command read or write. It keeps the first 1 KiB of each response read or write. An event shows up to 1,024 bytes of SQL, and `sql_truncated` is true if edc cuts it.
- If a lost event or a partial send moves the packet boundary, edc can miss the next commands of that connection. The summary shows the lost events.
- `docker run -p` starts `docker-proxy`. It shows one query on each hop. Use the container address to skip it.

Use `trace socket` on Linux to follow one unix domain socket file. Give the path of the socket file. edc prints each connect, accept, send, recv, end of data (`eof`), and close on that socket. The options can come before or after the path.

```bash
./bin/edc trace socket /run/docker.sock
./bin/edc trace socket /run/docker.sock --payload
./bin/edc trace socket /run/php/php-fpm.sock --payload=all --raw
```

The destination is the socket path. The event shows the call and its byte count. If a call fails, the event shows the errno name, for example `connect ECONNREFUSED`. The source is the peer process. On the server side, the peer is the process that connected. On the client side, the peer is the process that accepted the connection, or the last server process that sent or received data on it. Before the accept, the peer is the process that called `listen()`, for example `systemd` for a socket that systemd opens.

The server side shows an `accept` event. It shows how long the connection waited in the backlog, for example `accept 120µs`. A long wait means that the server accepts connections too slowly, for example because all workers are busy. edc does not see an `accept` call that started before the trace, but the next server call on that connection still shows the correct peer.

Use `--payload` to see the first 4 KiB of the data of each send and recv. Use `--payload=all` to see up to 1 MiB of each call. edc reads the data of one call in 16 KiB parts and joins them into one event. A stream socket has no message boundaries, so one event is one call, not one message. edc does not know the format of the payload, so it hides nothing. `--show-secrets` is not available. The payload can contain tokens and passwords, for example the `X-Registry-Auth` header on `docker.sock`. Before you share the output, check it for tokens and passwords. In the full screen, edc collects the first 4 KiB of each call also without `--payload`. Press `v` to show the payload lines. Press Enter to see the whole payload.

If the server makes the socket file again, for example after a restart, edc finds the new file within one second. Connections on the old file stay in the trace. `trace socket` supports stream sockets only. Datagram and seqpacket sockets, abstract sockets, and socket pairs are not available. edc does not see the data of `sendfile` and `splice`. A failed connect is in the trace only if the program uses the same path as the command.

Use `trace drop` on Linux to see why the kernel drops packets. For each drop, edc shows the reason, the kernel function, the addresses and ports, and the size. If the packet belongs to a local socket, edc also shows the process.

```bash
./bin/edc trace drop
./bin/edc trace drop --reason NO_SOCKET,SOCKET_RCVBUFF
./bin/edc trace drop --container web --raw
```

The reason is the name from the kernel, for example `NO_SOCKET` (no socket uses the port), `SOCKET_RCVBUFF` (the receive buffer of the socket is full), or `NETFILTER_DROP` (a firewall rule dropped the packet). The reasons need Linux 5.17 or later. On an earlier kernel, the reason is `unknown`, and only the function shows. Use `--reason` with names separated by commas to see only some reasons. The names are not case-sensitive.

The kernel also frees packets as part of normal work, for example when a program closes a socket with unread data (`QUEUE_PURGE` and `TCP_ABORT_ON_DATA`). edc shows these too, because they tell you that a program did not read the data.

The totals per reason in the summary are exact. If one CPU drops more than 1,000 packets in one second, edc sends only 1,000 events for that second and counts the rest. The summary shows this number as `Sampled out`. A process shows only for a packet that has a socket. A packet to a closed port has no socket.

The kernel does not report every drop with a reason. For example, if the accept queue of a listening socket is full, the kernel frees the SYN as a normal packet. For that case, the summary shows how much `ListenOverflows` and `ListenDrops` in `/proc/net/netstat` increased during the trace. These counters cover all listening sockets in the network namespace of edc, also with `--container`.

```bash
./bin/edc capture \
  --interface en0 \
  --duration 15s \
  --count 500 \
  --filter 'host 203.0.113.10 and port 443' \
  --output incident.pcap
```

A PCAP file can hold credentials and personal data. The JSON redaction does not apply to the PCAP payload.

`edc capture` shows the plan before it starts. The plan has the interface, the duration, the packet limit, the filter, the output path, and the privilege that `edc` uses. Answer the question with the left and right arrow keys and Enter, or with `y` or `n`. The plan stays on the screen above the `tcpdump` output.

Use `--yes` to skip the question. A non-terminal command prints the plan and reads `y` or `n` from stdin.

## Cron and application logs

`edc log` records both stdout and stderr without setup. Each run uses a separate file, so independent jobs do not wait for each other.

```cron
* * * * * /usr/local/bin/edc log -- /usr/local/bin/job --daily
```

Linux uses `${XDG_STATE_HOME:-~/.local/state}/edc/log/<command>/`. macOS uses `~/Library/Logs/edc/<command>/`.

File names include the UTC time, wrapper PID, and a unique suffix. An interactive terminal shows the generated path.

Each attempt records the command, work directory, process IDs, start and end times, duration, and exit status.

A command start failure records its cause in the file. A silent nonzero exit still produces an end record.

The default command display includes all arguments. Use `--command-display name` or `none` if arguments contain credentials.

An explicit `--output` appends to that file. `--stream stdout` or `stderr` records only the selected stream and preserves the other stream.

Commands with the same explicit output file wait for its `.lock` file. The lock remains stable across file rotation.

New files use mode `0600`. Existing log files retain their mode. Automatic command directories use mode `0700`.

### Command history

`edc log history` lists command keys based on the complete argv, in aligned Command / Last run / Runs / Failed columns. An Unknown column appears only when needed; hashes and repeated metadata are hidden in the browser overview. In a terminal, use ↑/↓ and Enter to open a key’s runs. Within history, select a run and press Enter for its working directory, full key, and source file. Esc or b goes back one screen and q quits. Selection is cyan; successful runs are green, failures red, and unknown outcomes and notices yellow. Piped output or `NO_COLOR` prints a static key list.

```bash
edc log -- ls -l /tmp
edc log history
edc log history ls
edc log history --command ls
edc log history ls -l /tmp
edc log history --failed ls -l /tmp
edc log history --key <key>
edc log history --file /tmp/job.log
edc log history --dir /path/to/logs
```

`history ls` prints runs of `ls` without arguments. `history ls -l /tmp` prints runs with that exact argv. Use `history --command ls` to list all argument keys for `ls`. Options are parsed only before the first command token; all following tokens are argv, including `--failed` and `--`. An optional `--` before the command ends option parsing without changing the query. `--command`, `--key`, and direct argv queries are mutually exclusive. Queries do not execute commands. Argument order and boundaries are preserved: `echo "a b"` and `echo a b` have different keys. The same argv shares a key across working directories, while statistics stay separate by directory.

Each attempt shows its start time, elapsed time, outcome, exit code, and retry number. Outcomes distinguish success, nonzero exit, timeout, signal, wrapper error, and unknown completion. The default is the latest 20 attempts; `--limit` accepts 1–1000. Elapsed time includes output draining and process cleanup. Summaries cover the displayed attempts, and average/min/max duration includes successful attempts only. Incomplete attempts are not counted as failures or included by `--failed`.

Discovery reads `.log` files in the default directory and one subdirectory level, plus `defaults.log.output`. Rotation archives are read together. Use `--file` or `--dir` for other locations. Scans are limited to 1,000 file families, 10,000 directory entries, 128 MiB, and 10,000 attempts. Limits, changes during reading, and malformed records produce notices, partial results, and exit code `2`. A successful query returns `0` even if historical commands failed.

New start markers record the SHA-256 argv key and command display mode. `name` hides arguments but retains the key; `none` omits the key as well. Legacy records can reconstruct keys when the complete argv is known. A legacy singleton basename cannot distinguish a no-argument invocation from name mode, so it is excluded with a notice. Hidden arguments are not reconstructed.

History reads metadata embedded in text logs. It cannot authenticate markers imitated by child output.

### Log rotation

The default maximum file size is 10 MiB. `--max-size` accepts an integer in MiB. A value of `0` disables rotation.

Rotation retains three archives by default. `--keep-files` accepts values from 1 to 100.

The archive names are `<output>.edc.1` through `<output>.edc.N`. These names are reserved for this log's rotation files.

Rotation removes only the oldest reserved archive. It does not expire files from other runs or compress them.

Use a regular file as the output. Rotation rejects symbolic links and nonregular archive paths.

If a log write fails, the wrapper stops the child process group and returns exit code `2`.

### Restart and timeout

Restart is disabled by default. `--restart on-failure` retries a nonzero exit, a child signal, or a timeout.

`--restart always` also retries a successful exit. Command start errors and log errors do not trigger another attempt.

For an exit eligible for restart, the wrapper stops remaining group members before it returns or starts another attempt.

The default limit is three additional attempts, with a five-second delay. `--max-restarts` and `--restart-delay` change these values.

An external SIGINT or SIGTERM stops the current attempt or retry delay. It prevents another attempt.

Timeout is disabled by default. `--timeout` limits each attempt, which includes the time to drain its recorded output.

On timeout, the wrapper sends SIGTERM to the child process group. After `--kill-after` (default five seconds), it sends SIGKILL.

It records `status=timeout exit=124`. If a later attempt succeeds, the wrapper returns `0`. Otherwise, it returns the last attempt's exit code.

```bash
edc log --timeout 10m --restart on-failure --max-restarts 3 -- /path/to/job
edc log --max-size 20 --keep-files 5 --output /tmp/job.log -- /path/to/job
```

The log settings also accept defaults under `[defaults.log]`. Explicit CLI options override them. The common timeout does not apply to `log`.

SIGKILL of the wrapper cannot produce an end record. Descendants outside the child's process group are outside its termination scope.

## Shell completion

`edc completion` prints a completion script. The script completes commands, options, and the group names of the inventory that `edc` finds.

```bash
source <(edc completion zsh)
source <(edc completion bash)
```

For zsh, you can also save the script as `_edc` in a directory of `fpath`.

`edc completion groups` prints the group names of the inventory, one per line. The scripts call it.

## Exit code

- `0`: success, or warnings only
- `1`: one probe fails or more, or `report diff` finds a worse probe
- `2`: a run error, such as an argument, a config, a log start, or a report parse error
- `3`: not enough privilege for a privileged task
- `4`: user cancel, which includes a cancelled selection and Ctrl-C in `remote` and `doctor`
- `124`: the final `log` attempt exceeds its timeout

## Current scope

`top`, `info`, `doctor`, and the single network probes support Linux and macOS.

On Linux, `edc` reads `/proc`, `/sys`, `ip`, `ss`, `ping`, `traceroute` or `tracepath`, and `/etc/resolv.conf`. If `resolvectl` exists, `edc` adds `resolvectl status` as evidence.

On macOS, `edc` uses a system command adapter. Linux and macOS run `capture`. `quality` runs `networkQuality` on macOS and a built-in responsiveness test on Linux. Both report `download_bps`, `upload_bps`, `responsiveness_rpm`, and `base_rtt_ms` when the run measured them; a missing value is left out. The config URL defaults to Apple's `https://mensura.cdn-apple.com/api/v1/gm/config`; `--server` or `defaults.quality.server` replaces it. An empty `server` keeps the default.

Every diagnostic command keeps to read-only inspection. `edc` runs no automatic repair, such as a DNS flush, an interface reset, or a firewall change. `edc log` writes its output, rotation archives, and lock file. `edc top --write` writes a SQLite database and its WAL files. `edc ai` writes `ai-resets.jsonl` and `ai-claude.json`, and it calls the Claude usage API.

## License

MIT. See [LICENSE](LICENSE).
