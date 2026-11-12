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

The common options are `--timeout`, `--json <path|->`, `--verbose`, and `--redact`. Go `flag` rules put an option before the target. A time value without a unit is seconds, so `--timeout 5` means `5s`.

### Name lookup

![edc dns lookup example.com prints the address list and edc dns config prints the resolver setup, both as PASS](media/dns.gif)

Terminal and JSON output show actual IP addresses by default. Add `--redact` to hide them as `<ip:...>`.

`edc dns compare example.com` compares the system resolver with `1.1.1.1`. Repeat `--resolver IP[:port]` to choose other DNS servers. It compares A, AAAA, CNAME, and response status; TTL is shown for explicit resolvers but not used to mark a mismatch. A different answer is WARN, and a failed system lookup is FAIL.

### Connection check

![edc tcp check connects to example.com:443 and then fails on a closed port with a timeout phase](media/tcp.gif)

A failed probe shows the phase and the cause in an ERROR block. It returns exit code `1`.

Use `-w` to wait until a port accepts a connection, for example while a server starts:

```bash
edc tcp check -w 10.0.0.9:5432      # wait until it connects, Ctrl-C to stop
edc tcp check -w=5m 10.0.0.9:5432   # stop after 5 minutes
```

`-w` tries again every second, and each attempt waits at most `--timeout`, 3s by default. A target that drops SYNs is then tried again every 3s, not once in 15s. In a terminal, one line shows the time, the attempt number, and the last cause. In a pipe or with `--json`, stderr gets a line only when the cause changes. When it ends, it prints the last attempt like a normal check, with the time it waited and the attempt number. The exit code is `0` when it connects, `1` when the time runs out, and `4` after Ctrl-C. Write the time limit with `=`: `-w 5m` reads `5m` as a second target, and edc tells you to write `-w=5m`. `edc alias` adds `etw` for `edc tcp check -w`.

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

### Port scan

`edc tcp scan` connects to many ports of one host and reports which ports answer.

```bash
edc tcp scan example.com
edc tcp scan --ports 22,80,443,8000-8100 example.com
edc tcp scan --ports 1-1024 --parallel 64 --timeout 2m 10.0.0.5
```

The target is a host without a port. `--ports` takes port numbers and ranges, with commas between them. Without `--ports`, edc checks the sensitive ports that `edc audit` watches and the common ports for remote access, web, and mail. edc resolves the name once, prefers an IPv4 address, and connects to that address only. The summary names the address.

Each port gets one TCP connect with a limit of 2 seconds. The result puts each port in one of these states:

| state | what edc saw |
| --- | --- |
| `open` | the handshake completed |
| `refused` | the host answered with an RST, so the host is up and nothing listens on the port |
| `no reply` | no answer in 2 seconds: a firewall dropped the SYN, or the host sends no RST for a closed port |
| `error` | the connect failed before an answer, for example `no route to host` |
| `not scanned` | `--timeout` ended before edc checked the port |

`no reply` is not the same as closed. edc does not guess between a silent firewall and a closed port.

`--parallel` sets the number of connects at one time. The default is 32, and the maximum is 256. `--timeout` is the budget of the whole scan, 60 seconds by default. If the budget ends first, the result is WARN, the evidence lists the ports that edc did not check, and the warning tells you to raise `--timeout` or `--parallel`. A scan of all 65535 ports needs a larger budget than the default.

The result is PASS when at least one port answered with a handshake or an RST. It is WARN when no port answered, and the `next` line names `edc tcp check --diagnose` for one of the ports. It is FAIL when the name does not resolve, or when every connect failed before an answer. The port list shows without `-v`. `--json` keeps every port with its state and connect time in the `ports` metric. The JSON state names use an underscore: `no_reply` and `not_scanned`.

edc opens one TCP connect to each port and sends no data. Scan only hosts that you operate or have permission to test.

### Find the broken segment between two hosts

When a TCP connection fails across a tunnel or a firewall, run two commands. Run `edc tcp check --diagnose` on the client host. Run `edc tcp expect` on the target host at the same time. The first command shows what leaves the client. The second command shows what reaches the target and what the target answers. Together, they show which segment drops the packets.

| Client verdict | Target verdict | First suspect |
|---|---|---|
| `no_reply` | `no_syn` | The path between the hosts: tunnel selector, firewall policy, or NAT |
| `no_reply` | `synack_no_ack` | The return path: reverse route, reverse selector, or rp_filter on the client side |
| `no_reply` | `syn_no_reply` or `dropped_unseen` | The target host: host firewall, rp_filter, or a full listen queue |
| `refused` | `rst` | The target host: no listener, or the host firewall rejects the port |
| `local_drop` | any | The client host: its own firewall |
| `degraded` | `completed` | Path quality: run `edc net mtu` |
| `connected` | `completed` | The path works. Check TLS and the application next. |

Save both results with `--json` and give them to the network engineer. The reports name the segment, so the engineer can read one policy, selector, or NAT rule instead of the whole device configuration.

### Join the two reports

`edc report join` reads the two saved reports and names the segment. The order of the two files does not matter.

```bash
edc report join [--json <path|->] [--redact] client.json target.json
```

The command matches the client source address with the `sources` of the target report. It applies the table above to the two verdicts. Then it prints a checklist with the real addresses for the device engineer.

| Segment | Meaning | Status |
|---|---|---|
| `between_hosts` | The path between the hosts drops the SYN. | fail |
| `return_path` | The answer of the target does not come back. This includes `no_reply` with `rst`. | fail |
| `target_host` | The target host answers wrong or not at all. | fail |
| `client_host` | The client host drops its own packets. | fail |
| `path_quality` | The path works but loses packets. This includes `intermittent` with `completed`. | warn |
| `works` | The path works. | pass |
| `inconsistent` | The two reports do not describe the same flow. | warn |
| `undetermined` | edc cannot name a segment, for example `reply_hidden_ipsec` or `dns_failed`. | warn |

`refused` with `no_syn` gives `between_hosts`, because a device on the path sent the RST.

The command adds a warning in three cases. The client port and the target port differ. The client source is not in the target sources, or `--from` excludes it. The two runs do not overlap in time. The verdict is then weaker. The output shows the start time skew between the two reports.

If the client report has `basis: phases`, a drop on the client host is not excluded. The command says so.

`--json` writes one object with `inputs`, `segment`, `status`, `summary`, `warnings`, `checklist`, `client`, `target`, and `time`. `--redact` hides addresses and host names. The exit code is `0` if the path works, `1` for any other segment, and `2` for a usage error or a file that is not an edc report.

The `target` section of the join JSON has the `listener` of the target report. If the target used `--listen`, the segment does not change, and a warning says that the service itself was not tested.

### Run both sides from the client

Use `--peer` to run `edc tcp expect` on the target host over ssh. One command on the client then does the work of the two commands and the join.

```bash
edc tcp check --diagnose --peer <ssh-host> [--count N] [--interval 1s] [--timeout 5s] [--json <path|->] [--redact] <host:port>
```

`--peer` implies `--diagnose`. The ssh destination can be an alias or `user@host` from your OpenSSH config. It can differ from `<host:port>`. For example, use a bastion alias for ssh and a private IP for the target.

The peer host must meet these conditions:

- It runs Linux and has edc 0.80.0 or later in the PATH of the login shell.
- The ssh user is root, or the user can run edc with `sudo -n`.
- The local ssh agent and known hosts let you connect without a prompt.

If a condition is not met, the command stops before the first connect and tells you the fix. The exit code is `2`.

The command does these steps:

1. It checks the version of edc and the user ID on the peer.
2. It starts `edc tcp expect --json - --duration <D> <port>` on the peer. `--from` is the source address of this host. If the target address is public and the source address is private, a NAT changes the source. Then the command does not set `--from`.
3. It waits until the peer starts the capture. Then it connects to the target `--count` times.
4. It shows the peer lines with the prefix `[<ssh-host>]` between the attempt lines.
5. It stops the peer, reads its report, and joins the two reports.

The output shows the client result, the peer result, the join verdict, and the checklist. The exit code is `0` if the path works and `1` for any other segment. Push Ctrl-C to stop the attempts. The command then stops the peer, shows the results so far, and exits with `4`. If the ssh connection fails, the peer process stops by itself. `--duration` is a limit in case this step also fails.

`--json client.json` writes three files: `client.json`, `client.target.json`, and `client.join.json`. `--json -` writes one object with `client`, `target`, and `join` to stdout. The live lines then go to stderr. `--redact` hides addresses and host names in the files and in the live lines.

### Diagnose a TCP path

Run the check several times and get a verdict:

```bash
edc tcp check --diagnose example.internal:443
sudo edc tcp check --diagnose --count 10 --interval 2s --timeout 5s 10.0.0.9:5432
```

The command shows the route first: the interface, the gateway, and the source address. Then it connects `--count` times (default 5). It prints one line for each attempt.

`--timeout` limits each connect attempt. `--interval` sets the wait after each attempt (default 1s). The total time is at most count × timeout plus the intervals and about 2s to start the trace.

On Linux as root, edc also traces TCP events and kernel drops for this flow. It counts SYN retransmits, resets, RTT, and drops for each attempt. This trace adds a 300ms wait after each attempt. On macOS, or without root, edc skips the trace. Then the verdict uses the connect phases only, and a warning tells you so.

| Verdict | Meaning | Next command |
|---|---|---|
| `local_drop` | This host dropped packets of the flow. | `sudo nft list ruleset` |
| `refused` | The connect got an RST at once. | `edc listen --tcp` on the target |
| `no_reply` | SYNs got no answer, and this host did not drop them. | `edc tcp expect --from <source> <port>` on the target |
| `intermittent` | Some attempts connected and some failed. | `edc tcp expect --from <source> <port>` on the target |
| `degraded` | All attempts connected with retransmits or a slow connect. | `edc net mtu <host>` |
| `connected` | All attempts connected cleanly. Check TLS and the application next. | TLS ports such as 443: `edc tls check <host:port>`. Ports 80 and 8080: `edc http check http://<host:port>`. Other ports: none |

A firewall on the path can also send the RST. Only drop reasons that block a flow change the verdict, such as `NETFILTER_DROP` and `XFRM_POLICY`. Other drop reasons go to `other_drops`.

`--json` writes one `tcp.diagnose` result. Its metrics include `attempts`, `totals`, `drops`, `trace`, `verdict`, and `basis`. If edc does not trace a value, the key is not in the JSON.

If you test in a network namespace, use `nsenter --net=/run/netns/<name>`. `ip netns exec` hides tracefs, so edc skips the trace.

### Watch arrivals on the target host

Run `edc tcp expect` on the destination host. It shows if a SYN arrives and how this host answers.

```bash
sudo edc tcp expect [--from <ip|cidr>] [--duration 30s] [--until syn|handshake] [--stop-after N] [--listen] [--json <path|->] [--redact] [-v] <port>
```

- The command runs on Linux only. It needs root or `CAP_NET_RAW`. If not, the exit code is `3`.
- The command reads TCP headers on all interfaces. It does not keep payload.
- If you do not set `--duration`, the command runs until you push Ctrl-C. The maximum is 1h.
- `--from` counts only packets from one address or from one CIDR range.

Each event shows on one line: `SYN in`, `SYN-ACK out`, `RST out`, `ESTABLISHED`, `DROP`, and `ROUTE`.
The time on a packet line is the time when the kernel received or sent the packet. edc holds the lines for 100 ms and shows them in time order, so `RST out` comes before the `DROP` line of the same SYN.
A second SYN-ACK for the same attempt shows as a retransmit. This is a sign of a broken return path.
If this host refuses an attempt, a `HINT` line follows the `ROUTE` line. It gives the cause, the drop reason, and the error that the client sees. edc shows it one time for each source. edc uses the same rules as the `rst` cause below, so there is no `HINT` line without a drop reason.
At the end, the command shows a table for each source and arrival interface, the route back, and a verdict.

| Verdict | Meaning | Status | Exit code |
|---|---|---|---|
| `completed` | The client sent the last ACK. The path works. | pass | `0` |
| `rst` | This host sent RST. The summary names the cause. Refer to the list after this table. | fail | `1` |
| `synack_no_ack` | This host sent SYN-ACK, but no ACK came back. Examine the reverse route and the reverse IPsec selector. | fail | `1` |
| `syn_no_reply` | The SYN arrived, but this host sent no reply. A firewall, rp_filter, or a full listen queue drops it. | fail | `1` |
| `dropped_unseen` | The packet socket saw no SYN, but the kernel dropped packets for the port. | fail | `1` |
| `reply_hidden_ipsec` | The reply goes out through an IPsec policy as ESP. edc cannot see it. | warn | `1` |
| `no_syn` | No SYN arrived. | fail | `2` after `--duration`, `4` after Ctrl-C |

For the `rst` verdict, edc finds the cause from the kernel drop reason of the source that it shows first:

- `NETFILTER_DROP`: a host firewall rule rejects the SYN. The next step shows the commands that list the rules.
- `NO_SOCKET` and no listener on the port: no process listens on the port.
- `NO_SOCKET` and a listener that takes a different address, for example `127.0.0.1`: the summary shows the address of the listener. If the listener is `docker-proxy`, the next step shows a `-p` value with the address that the SYN came to.
- `NO_SOCKET` in other cases: the kernel found no socket for the address and port.

If there is no drop reason, the summary gives two causes: no listener, or a host firewall. The summary also gives the two causes if the source has both `NETFILTER_DROP` and `NO_SOCKET`, because the drop reasons of all its attempts are added together. If a listener is on the port, the next step does not show `--listen`, because `--listen` stops with exit code `2` on a used port.
`rst_cause` in the metrics is `firewall`, `no_listener`, `loopback_only`, `other_address`, or `no_socket`. If edc cannot find the cause, the metrics do not have `rst_cause`.

If a source has one or more completed handshakes, its verdict is `completed`.
The exit code is `0` only if each source has a completed handshake.

The route back is the same lookup as `ip route get <source> from <local address>`.
If the route uses a different interface than the arrival interface and rp_filter is `1`, edc shows a warning.
The kernel drops these SYNs. edc uses the larger rp_filter value of the arrival interface and `all`.
This check is for IPv4 only.

If eBPF and the `skb/kfree_skb` tracepoint are available, edc adds kernel drop reasons.
Examples are `NO_SOCKET`, `IP_RPFILTER`, and `NETFILTER_DROP`.
If they are not available, this part shows `skip` and the reason. The command continues.
In `ip netns exec`, tracefs is not mounted. Use `nsenter --net` to keep the drop reasons.

If this host is an IPsec endpoint, read these notes:
- Tunnel mode: the decrypted SYN is visible. The SYN-ACK and the RST go out as ESP. edc uses the client ACK and the drop reasons.
- Transport mode: the decrypted SYN is not visible. edc uses the drop reasons and shows a warning. Read the counters of `ip -s xfrm state`.

`--json` writes one report with the probe `tcp.expect`. With `--json -`, the live lines go to stderr.
`edc report show` shows the saved file.

Limits:
- edc finds the handshake from a last ACK without payload. If the client puts data in that ACK, edc does not see the handshake.
- edc does not read IPv6 extension headers or IPv4 fragments after the first fragment.
- The route lookup does not use fwmark or UID rules.

#### Start, stop, and the next step

At the start, edc shows the listener of the port: the process, the PID, and the address. If no process listens, the line says that SYNs get an RST. If the listener is bound to loopback only, edc shows a warning, because SYNs from other hosts get an RST.

The waiting line shows a command for the client with a real address of this host. With `--from`, edc uses the source address of the route back to that address. Without `--from`, edc shows up to 3 IPv4 addresses of interfaces with a carrier. The default route interface is first, and container bridges are last.

- `--until syn` stops 1s after the first SYN, so the reply of this host is in the result.
- `--until handshake` stops at the first completed handshake.
- `--stop-after N` stops 1s after the Nth connection attempt. Attempts in that second are also counted.

An early stop is a normal end, and the verdict sets the exit code. `stop_reason` is `duration`, `cancelled`, `until_syn`, `until_handshake`, or `stop_after`. If `--until` is not `syn` or `handshake`, or `--stop-after` is less than 1, the exit code is `2`.

In a terminal, a status line at the bottom shows the elapsed time, SYNs, handshakes, kernel drops, and SYNs for other ports. edc removes the line before the result. edc does not show the status line with `--json -` or when stdout or stderr is not a terminal.

edc counts SYNs to other TCP ports with a second packet socket and keeps one count for each port only. It does not count loopback, forwarded packets, or sources outside `--from`. `other_port_syns` in the metrics has the 5 ports with the most SYNs. If no SYN arrived for the port, edc shows a warning with these ports, because the client can use a different port.

After the result, edc shows the next step for the verdict and the command that joins this report with the client report.

#### Test the path when the service is down

With `--listen`, edc opens a temporary listener on all addresses if no process listens on the port. It accepts each connection and closes it at once, so a reachable path gives `completed` even when the service is down. If a process listens on the port, `--listen` stops with exit code `2`; edc does not take over a port. edc closes the temporary listener at each end: `--duration`, `--until`, `--stop-after`, Ctrl-C, or an error.

The `listener` metric has `state` (`present`, `none`, `unknown`, or `temporary`), `process`, `pid`, `address`, and `scope`.

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

If one directory gets more than 50 lines of one kind in the 200 ms window, `edc` prints one line for the directory, for example `REMOVE "logs/2026/..."  ×1000`. The count is the number of entries, and the line lists the processes of all entries. A large `rm -rf` or copy then costs one line for each directory, so the terminal keeps up with the events. The entries of a directory with 50 lines or fewer stay on separate lines, and `--no-smart` prints every entry.

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

The kernel keeps the events in a queue until `edc` reads them. By default, the queue holds 16384 events (`fs.fanotify.max_queued_events` and `fs.inotify.max_queued_events`). If `edc` reads slower than the events arrive, the kernel drops the new events and sends one overflow event. `edc` then prints one line on stderr and continues the watch. It prints this line at most once in 10 seconds. The dropped changes are missing from the output, from the rules, and from the summary. With `fanotify`, `edc` also forgets the directory paths that it has cached, because a dropped directory rename makes them wrong. With `inotify` and `--recursive`, a dropped directory create would leave that directory unwatched, so `edc` reads the tree again after the notice and adds a watch to each directory that it missed. The changes during the overflow stay missing.

A terminal prints lines slower than the events of a large `rm -rf` or copy arrive. So `edc` keeps up to 262,144 events in memory while the output is behind. The full queue uses about 40 MB. In a test that created and removed 400,000 files while the output printed 10,000 lines per second, the `edc` process used about 350 MB in total. If this queue is also full, `edc` drops the new events until the output has consumed half of the queue. It prints one line when it starts to drop, and one line with the count when it accepts events again. To avoid drops, narrow the scope with `--exclude`, or write `--json` to a file, which is faster than a terminal.

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

Actions receive no stdin. Their default timeout is 30s. `--debounce` and `--timeout` supply defaults for rules without their own values. Combined stdout/stderr is capped at 64 KiB. Failures and timeouts do not stop watching, but produce final exit code `1`. `Ctrl-C` or the watch duration ends the active action's process group and discards pending actions. A normal observation with no events exits `0`; option/output errors exit `2`. Removing or renaming the root and watcher errors stop observation with an error. An event overflow does not stop it: the watch continues, and the `summary` record reports the loss.

JSON Lines uses `ready`, `event`, `action_start`, `action_result`, and `summary`. The `summary` record has `incomplete`. It is `true` when the watch lost events, and the record then has the counts `overflows` (kernel queue overflows), `dropped` (events that `edc` dropped), and `rescans` (directories that `edc` read again on macOS). The text summary line shows the same counts after `incomplete:`. The exit code does not change. Child output stays inside `action_result.output`. This watches local filesystems; save operations can coalesce or generate multiple events. It is not a filesystem audit log.

### Route and interfaces

![edc net interfaces, edc net route example.com, and edc net ping example.com each print PASS and one result line](media/net.gif)

### Path MTU

`edc net mtu` measures the largest packet that reaches a host without fragmentation.

```bash
edc net mtu example.com
edc net mtu --max 1400 10.0.0.5
```

`edc` sends ICMP echo with the Don't Fragment bit set. It starts at 576 bytes (1280 for IPv6) and stops at the MTU of the route interface. Between these sizes, it uses a binary search.

The result shows the path MTU, the interface MTU, the gap, and the TCP MSS that fits.

- `PASS`: the path MTU is equal to the interface MTU.
- `WARN`: the path MTU is smaller than the interface MTU, or large packets disappear without an ICMP reply.
- `SKIP`: the host does not reply to ICMP echo at the smallest size. Use a TCP-based check instead.

If large packets disappear without an ICMP "fragmentation needed" reply, `edc` reports a PMTU black hole. In this case, the TCP handshake succeeds, but large responses stall.

The gap is a hint, not a finding. A gap of 50 to 80 bytes is typical of tunnel overhead, such as IPsec ESP, WireGuard, or VXLAN.

To try smaller sizes only, set `--max N`. `edc` does not try sizes above the interface MTU.

In a black hole, each failed size takes about two seconds. If the result shows a range, run the command again with a longer `--timeout`.

On macOS, IPv6 measurement needs root. Run `sudo edc net mtu <host>`.

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
