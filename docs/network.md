**English** | [한국어](network.ko.md) · [Back to README](../README.md)

# Network checks

[Network checks](#network-checks) · [Where am I](#where-am-i) · [Probe thresholds](#probe-thresholds) · [Probe live line](#probe-live-line) · [Missing arguments](#missing-arguments) · [Doctor live screen](#doctor-live-screen) · [Network quality](#network-quality)

## Network checks

Check the layer you need, or run the combined diagnosis with `doctor`:

```bash
edc dns lookup example.com
edc tcp check example.com:443
edc tls check --min-days 14 example.com:443
edc http check --expect-status 200 https://example.com
edc net route example.com
edc doctor https://example.com
```

Save a redacted report and open it later:

```bash
edc doctor --redact --json report.json https://example.com
edc report show report.json
edc report diff before.json after.json
```

`report diff` compares two saved reports and returns exit code `1` when a probe gets worse.

The common options are `--timeout`, `--json <path|->`, `--verbose`, and `--redact`. Go `flag` rules put an option before the target.

### Name lookup

![edc dns lookup example.com prints the address list and edc dns config prints the resolver setup, both as PASS](media/dns.gif)

Terminal and JSON output show actual IP addresses by default. Add `--redact` to hide them as `<ip:...>`.

`edc dns compare example.com` compares the system resolver with `1.1.1.1`. Repeat `--resolver IP[:port]` to choose other DNS servers. It compares A, AAAA, CNAME, and response status; TTL is shown for explicit resolvers but not used to mark a mismatch. A different answer is WARN, and a failed system lookup is FAIL.

### Connection check

![edc tcp check connects to example.com:443 and then fails on a closed port with a timeout phase](media/tcp.gif)

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

### File watching and actions (Linux and macOS)

`edc watch fs` observes changes directly below the current directory by default. Use `-r` or `--recursive` for subdirectories, including newly created or moved-in directories. Existing files do not produce startup create events. Directory symlinks are not followed.

```bash
edc watch fs
edc watch fs ./src -r
edc watch fs --event create --match text.txt --exec 'git-kit pull'
edc watch fs ./src --recursive --event modify --match '**/*.go' --exec 'go test ./...'
edc watch fs --rules docs/examples/watch.yaml --dry-run
edc watch fs --duration 1m --json events.jsonl
```

Events are `create`, `modify`, `remove`, and `rename`. A rename reports the old path; the new name can produce a create event inside the watched scope. Reads/access and metadata-only changes are excluded. `--event` accepts a comma-separated list. Globs are relative to the watch root: `*.go` matches direct children, and `**/*.go` matches any depth. Watching those deeper paths also requires `--recursive`.

In a terminal, `edc watch fs` merges the events of one change into one line. This is `--smart`. It collects the events for 200 ms and prints the result. A save that writes a temporary file and renames it shows as one line, for example `CREATE "dir/final"  ← "final.tmp"`. A file that is created and removed in the same 200 ms shows as `TEMP`. Many `MODIFY` events of one file show as `×N`. A rename and the create after it make one line only if no other event is between them. A change that crosses the end of the 200 ms can show as two lines. A pipe, `--json`, and `--no-smart` print every event as it comes. `--smart` turns the merge on for a pipe, but you cannot use it with `--json`. Rules and the summary use the original events, so the summary can count more events than the screen shows.

If the same line comes again within 5 seconds, `edc` does not print a new line. It rewrites the line above the status line and adds `×N`. This works only in a terminal that shows the status line. Any other output between the two lines, such as a hint or an action, starts a new line.

The same mode watches for a noisy directory. If one directory gives at least 30 events in 10 seconds, and at least half of all events in that time, `edc` prints one line on stderr that starts with `hint:`. The line shows the `--exclude` option that leaves the directory out, for example `--exclude 'logs/**'`. `edc` names a directory once, and names at most 3 directories in one run. It does not name the watch root or a directory with a glob character in its name. Stderr must be a terminal, and `--no-smart` turns the hint off.

When the watch stops, `edc` prints a table under the summary line. The table lists the 5 directories with the most events, with the count and the share of all events. This helps you choose what to exclude. `edc` prints the table only if at least 10 events came. It does not print the table with `--json`, and `--no-smart` turns it off. If the watch sees more than 10,000 directories, `edc` keeps the busiest ones. A `~` before a count means that the real count can be lower.

With the `fanotify` driver, a line ends with `by <name>`, the process that changed the file, for example `by syslog-ng`. The kernel tells `edc` the process, and `edc` reads its name when the event arrives. A process that has already ended has no name, and a short command such as `mv` often ends first, so the line shows no name. If the events of one merged line come from different processes, the line lists each name once, for example `by editor, formatter`, and it shows `+N` after 3 names. The `inotify` driver does not tell the process, and `--no-smart` leaves the name out. The JSON records have `pid` when the driver knows it, also when the name is not known, and `process` when the name is known. If the kernel merges events of one path, the process is the one of the first event.

Two limits apply to the process name. First, `edc` reads the name from `/proc/<pid>/comm` after it reads the event. If the kernel gives the same PID to a new process in that time, the line shows the name of the new process. This is rare, because the PID range must turn over in a few milliseconds. The rate is unknown. A fix needs `pidfd` in the event. The kernel support for `pidfd` with the current event format is unknown. Second, a merged line keeps every distinct process name of its 200 ms window. The cost grows with the number of names. In a test, 100,000 names used 6.7 MiB and 1,000,000 names used 85.5 MiB.

By default, `edc` excludes `.git`, `node_modules`, `.venv`, `venv`, `__pycache__`, `.mypy_cache`, `.pytest_cache`, `.ruff_cache`, `.tox`, `.next`, and `.gradle` at any depth. It does not exclude `build` or `dist`, because you can watch build output. Repeat `--exclude 'build/**'` to add exclusions. To watch the default directories too, use `--no-default-exclude`. Your `--exclude` patterns still apply.

On Linux, `--driver` selects how `edc` watches the tree. The default is `auto`.

- `fanotify` puts one mark on the whole filesystem. `edc` does not read the tree at startup, and the inotify watch limit does not apply. This driver needs root (`CAP_SYS_ADMIN`) and Linux 5.17 or later.
- `inotify` adds a watch to each directory before the first output. A large tree takes time. If stderr is a terminal, `edc` shows a notice during this step. If the tree has more directories than `fs.inotify.max_user_watches` allows, the watch stops with an error.

With `--recursive`, `auto` tries `fanotify` first. If `fanotify` cannot start, `edc` uses `inotify`. If stderr is a terminal, a line on stderr gives the reason. Without `--recursive`, `auto` uses `inotify`, because one watch is sufficient. The `driver` field of the `ready` record shows the driver in use. When `auto` chooses `inotify`, the start line and the `driver_reason` field of the `ready` record give the reason: `not_recursive` or `fanotify_unavailable`. A driver that you choose with `--driver`, and `fanotify`, have no reason. If you set `--driver fanotify` and `fanotify` cannot start, `edc` stops and shows the reason.

`fanotify` cannot start in these conditions:

- `edc` does not run as root.
- The kernel is older than Linux 5.17.
- The filesystem of the watch root does not supply the file handles that `fanotify` needs. A btrfs subvolume is an example.

One mark covers one filesystem. With `--recursive`, `edc` puts a mark on each filesystem that is mounted below the watch root. Some filesystems do not accept a mark, for example sysfs and devpts. The changes in a bind mount of a directory outside the watch root also cannot show inside the root. `edc` skips these mounts and names them in one line on stderr. Changes below a skipped mount are not reported. If you need these changes, use `--driver inotify`. `edc` does not mark a mount that `--exclude` covers.

`fanotify` receives the events of the whole filesystem, and `edc` discards the events outside the watch root. Thus a busy filesystem costs CPU time. When few events arrive, `edc` waits 5ms between reads. If the CPU cost is too high, use `--driver inotify`.

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

![edc net interfaces, edc net route example.com, and edc net ping example.com each print PASS and one result line](media/net.gif)

## Where am I

`edc where` answers two questions at once: which cloud region is near this host, and what shape the network has.

```bash
edc where
edc where --provider aws        # one provider only
edc where --count 5 -v          # more samples, every provider row
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

![edc where shows the public IP with its ASN and Cloudflare PoP, the route, the NAT shape, and the near regions in order of round trip time](media/where.gif)

The demo masks the public IP as `222.XXX.XXX.XXX` and replaces the route addresses with private example addresses.

`edc` states only what it confirms. It does not guess the line type from the jitter. The jitter column holds the number, and the reading stays with you.

## Probe thresholds

`edc tls check` gives a warning when the certificate expires in less than 30 days. Set `--min-days` to make an earlier expiry a failure.

`edc http check` gives a warning for a 4xx response and a failure for a 5xx response. Set `--expect-status` to accept one status code only. A different code is a failure.

If the target has no scheme, `edc http check` adds `http://`. `edc http check naver.com` requests `http://naver.com` and follows the redirect. The `url` metric holds the address that `edc` requested. Write `https://` to check a TLS endpoint directly.

If the server sends a redirect, `edc http check` lists each request in order: the status, the URL, the timings, and the peer. Add `-v` to see the headers of each response and the start of the last body. `edc` hides the values of `Authorization`, `Cookie`, `Set-Cookie`, and other credential headers. The JSON keeps each request in the `hops` metric. `edc doctor -v` shows the same list.

Use `-H 'Name: value'` to add a request header, as with `curl -H`. Repeat it for more headers. `-H 'Host: name'` sets the Host header. If a redirect goes to another host, that request does not carry `Authorization` and `Cookie`. Use `-v` to see the headers that each request sent.

Use `--body page.html` to save the whole body of the last response. Use `--body -` to write the body to stdout. Then the result goes to stderr. If stdout is a terminal, edc escapes the control characters in the body. `edc` reads 10 MiB of the body at most, and it tells you when it cuts the body. `--body` overwrites an existing file. Options can come before or after the URL.

```bash
edc http check http://example.com
edc http check -v -H 'Authorization: Bearer <token>' https://example.com/api
edc http check https://example.com/api --body - | jq .
```

```bash
edc tls check --min-days 14 example.com:443
edc http check --expect-status 200 https://example.com/health
```

A failure returns exit code `1`. Use these options in cron to get a synthetic check.

![edc tls check passes and then --min-days 90 fails because the certificate expiry is under the threshold](media/tls.gif)

![edc http check passes with HTTP 200 and then --expect-status 404 fails on the status mismatch](media/http.gif)

The demo shows one pass and one threshold failure for each command.

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

A command group asks which command to run. `edc dns`, `edc net`, `edc report`, `edc route`, `edc change`, `edc disk`, `edc history`, and `edc completion` show their commands with a short description of each. `edc report show` and `edc report diff` then list the reports in the current directory.

`edc log` does not ask. With no arguments, it prints its forms and examples, and returns exit code `2`.

`edc change confirm`, `edc change rollback`, and `edc route rollback` list the changes that still wait. You pick one instead of copying a run id.

If stdin is not a terminal, `edc` prints the usage and returns exit code `2`. A script keeps the same behavior. `edc` asks only for a value that the command needs. It never asks for an optional setting such as `--duration` or `--filter`.

`edc change apply` and `edc route switch` are the exception. They change a host, so they take every value from a flag. A command that you cannot undo must stay a command that you can type again.

## Doctor live screen

![edc doctor runs network probes and summarizes their results](media/doctor.gif)

After DNS, TCP, TLS, or HTTP warnings and failures, `edc doctor` shows manual command templates in investigation order. Independent checks retain their recorded outcomes; their order does not establish a cause. JSON reports contain no suggestions. Replace template placeholders with your target before running a suggested command.

Demo sources are under [`docs/tape/`](tape). Rebuild this demo with `vhs docs/tape/doctor.tape`.

If stdin and stdout are terminals, `edc doctor` shows one line for each probe and updates the line when the probe ends. The finished lines stay on the screen. The details and the summary follow.

Use `edc doctor --all-ips example.com` to check every address returned by DNS. It connects directly to each IP with the original Host and TLS SNI, then lists TCP, TLS, and HTTP results per IP. These HTTP checks read response headers only and do not follow redirects or use a proxy. The option also asks RIPEstat for each public IP's origin ASN and registered ASN holder; lookup failures do not change endpoint health. If any IP fails, `endpoints.check` fails. The checks test each entry point; they cannot establish whether a load balancer exists or where a server is physically located.

Press Ctrl-C to cancel. `edc` stops the running probes and returns exit code `4`.

`edc` waits and prints all lines at the end if stdin or stdout is not a terminal, if `--json` is set, or if `NO_COLOR` is set.


## Network quality

`quality` measures throughput and responsiveness. macOS uses `networkQuality`; Linux uses a built-in responsiveness test. It generates measurement traffic to the configured server.

```bash
edc quality --timeout 60s
edc quality --server https://example.com/.well-known/nq
```

`--server` is a responsiveness configuration URL, not an arbitrary web page. The default is Apple's `https://mensura.cdn-apple.com/api/v1/gm/config`. `defaults.quality.server` sets the same value in configuration. Results include download/upload throughput, responsiveness in RPM, and base RTT when measured; unavailable values are omitted.
