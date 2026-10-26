**English** | [한국어](host.ko.md) · [Back to README](../README.md)

# Host inspection

[Host information](#host-information) · [Host audit (Linux)](#host-audit-linux) · [Service view (Linux)](#service-view-linux) · [Container view (Linux and macOS)](#container-view-linux-and-macos) · [Recent host changes (Linux)](#recent-host-changes-linux) · [Kernel and systemd events (Linux)](#kernel-and-systemd-events-linux) · [Current scope](#current-scope) · [Listening ports](#listening-ports)

## Host information

`edc info` gets the public IP by default. It sends one DNS query for `o-o.myaddr.l.google.com` to `ns1.google.com` over IPv4. `-v` also asks ipinfo.io for the provider, the city, the region, the country, and the time zone. A lookup stops after 3 seconds, and its line disappears. Use `--public=false` to skip the lookups, `--timeout` to change the limit, and `-v` to print the cause of a failure. If a network blocks DNS to external servers, the default output has no public IP.

`edc info` also shows memory status, a process snapshot, and diagnostic support.

The default output groups related values on one line. Use `-v` for kernel details, the memory calculation basis, shared-page notes, and the Python version. A shim such as pyenv can make the Python check take several hundred milliseconds, so the default output skips it.

On Linux, used memory is `MemTotal - MemAvailable`. Cache counters include `Cached`, `Buffers`, and reclaimable slab when present.

On macOS, available memory is an estimate from free and inactive Mach pages. File-backed memory and physical compression appear separately.

Memory detail counters can overlap. They do not measure memory pressure and must not be added together.

The process snapshot shows observed process and thread counts, plus the three processes with the highest RSS. Thread counts show coverage.

`edc info` also shows these signals by default. It reads them from files, syscalls, and sysctl, and runs no external command.

- `Platform`: the DMI vendor and product, for example `Amazon EC2 m5.large`. On Oracle Cloud, it adds `Oracle Cloud` from the chassis asset tag. On Linux, it adds `virtual machine` if the CPU reports a hypervisor or the DMI names QEMU or a virtual machine. It also adds the container runtime if it finds a marker. On macOS, it shows the model identifier.
- `Clock` (Linux): the kernel time synchronization state from `adjtimex`. It does not show an offset. The kernel error values are bounds that only grow between updates, not the real offset.
- `DNS`: the nameservers and search domains in `/etc/resolv.conf`. If the file points to the systemd-resolved stub, edc shows the upstream servers.
- `Pressure` and `OOM kills since boot` (Linux): PSI some avg10 for CPU, memory, and I/O, and the `oom_kill` counter in `/proc/vmstat`.
- A disk line ends with `(read-only)` or `(inodes N%)` if the mount is read-only or inode use is 80% or more. On macOS, the sealed system volume is read-only by design, so edc does not mark it.
- On Linux, a disk line also ends with `(deleted-open 12.0 GB in 3 files, java)` if a process still holds a file that was deleted. `du` does not see this space, but `df` counts it, and the file system frees it only when the last process closes the file. edc reads `/proc/<pid>/fd`, counts each inode once, and names the process that holds the largest file. `-v` lists the five largest files with the PID, the process, and the path. Without root, edc cannot read the files of other users, so the value is a lower bound and shows `≥`. edc stops after 200000 file descriptors.

`-v` adds the system-wide open files, the IPv6 default route (Linux), the MTU, MAC, link speed (Linux), and IPv6 addresses of each interface, and the options of each mount. It lists only interfaces with an IPv4 address or a non-link-local IPv6 address.

`Listening Ports` shows the count of TCP and UDP sockets that wait for connections. A socket on a loopback address adds to the count only. The other sockets show in a list of up to five rows with the port, the address, and the process. `-v` lists all TCP and UDP sockets. `edc listen` shows the full table. On macOS without root, the list does not include the sockets of other accounts.

Diagnostic support tests process I/O access for the current process and PSI availability. Access to other PIDs can differ.

The Linux eBPF check reads kernel BTF and effective capabilities. It does not load or attach programs.

`prerequisites met` does not guarantee attachment. Unsupported features, absent permissions, and failed checks show their reasons.

If memory or process collection fails, the command shows the error and returns exit code `1`.

On Linux, `edc info` adds `Network Limits`: conntrack occupancy, TCP memory pages against the `tcp_mem` ceiling with the orphan socket count, the local port range and reserved ports, accept/SYN backlog limits, socket buffer ceilings, receive backlog/budgets, neighbor limits, forwarding, and `rp_filter`. Use `-v` for TCP buffer settings, sysctl names, and reasons for unavailable values. Missing or inaccessible values are `unavailable`; these Linux limits are `unsupported` on macOS. Configuration alone does not establish a connection failure.

## Health check (Linux)

`edc check` looks for what is broken on a Linux host now. It reads services, processes, memory, disks, the kernel log, time, network, containers, file handles, and the reboot state. It changes nothing, and it sends nothing over the network. Each finding names the edc command that shows more.

```bash
edc check
edc check --json -                  # one result for each check
edc check --group daily             # run edc check on each host of an edc remote group
```

| Check | Fail | Warn |
|---|---|---|
| `service` | a unit failed, or a unit in `require_units` is inactive or does not exist | a unit is in a state change or in `auto-restart` |
| `process` | | a defunct process, or a process in D state |
| `memory` | less than 5% of memory is available | less than 10% of memory is available |
| `disk` | a mount has 95% or more use, or 95% or more inode use | the same at 90%, or a mount is read-only |
| `kernel` | an OOM kill, a disk I/O error, a read-only remount, a lockup, or an oops in the last hour | a segfault, a hung task, or a kernel warning in the last hour |
| `time` | | the clock is not synchronized |
| `network` | no default route, or 90% or more use of conntrack or TCP memory | no `/etc/resolv.conf`, no nameserver, or the same use at 80% |
| `container` | a container stopped with a nonzero code or for memory | a container restarts, pauses, or is unhealthy |
| `filehandles` | 90% or more of `fs.file-max` is in use | 80% or more of `fs.file-max` is in use |
| `reboot` | | a package asks for a reboot, or `/boot` has a newer kernel than the running kernel |

A `fail` makes the exit code 1. edc does not judge counters that only grow after boot, such as NIC drops. One reading cannot show when they changed.

`--group` uses the inventory and the OpenSSH setup of `edc remote`. Each host must have an edc with `check`. The table shows one row for each host. If a host gives no result, the row shows the reason, for example an SSH failure or an edc without `check`.

To accept a known finding, add a `[check]` table to `config.toml`:

```toml
[check]
accept = ["reboot"]
accept_units = ["systemd-networkd-wait-online.service"]
accept_containers = ["job-*"]
accept_mounts = ["/media/*"]
require_units = ["nginx.service"]

[check.limits]
disk = [90, 95]
memory_available = [10, 5]
usage = [80, 90]
```

`accept` holds check names from the table above. An accepted check shows as `skip`, and its summary starts with `accepted (warn)` or `accepted (fail)`. In JSON, `metrics.accepted_status` holds the original status.

`accept_units`, `accept_containers`, and `accept_mounts` hold glob patterns. The check does not judge a name that matches a pattern. The summary ends with the number of accepted names, and `metrics.accepted` holds the names.

Each value in `[check.limits]` is `[warn, fail]` in percent. `usage` applies to conntrack, TCP memory, and file handles. `memory_available` is the share that is left, so its warn value must be at least its fail value.

With `--group`, each host reads its own config file. Put the `[check]` table on the host that the finding comes from.

## Host audit (Linux)

`edc audit` reads the exposure and the settings of a Linux host. It reads files, sysctl values, and the listening sockets. It changes nothing, and it sends nothing over the network. It ends in less than one second.

```bash
sudo edc audit
sudo edc audit -v               # add the values and the rules behind each result
sudo edc audit --json -         # one result for each check
```

| Check | Fail | Warn |
|---|---|---|
| `net.exposed` | a database or control port listens beyond loopback, and the host firewall accepts inbound traffic | the same port, but the firewall drops by default or edc cannot read it |
| `net.firewall` | | the input chains have no rule |
| `net.ip_forward` | | forwarding is on, and edc finds no container runtime |
| `ssh.root_login` | | `PermitRootLogin yes` |
| `ssh.password_auth` | `PermitEmptyPasswords yes` with password login | password login is on |
| `ssh.key_files` | another user can write `~/.ssh` or `authorized_keys` | |
| `user.uid0` | an account other than root has UID 0 | |
| `user.empty_pass` | an account has an empty password | |
| `user.sudo_nopass` | | a sudo rule runs every command without a password |
| `user.root_groups` | | the `docker` or `lxd` group has members |
| `file.sensitive` | other users can read `/etc/shadow`, or write `/etc/passwd`, `/etc/sudoers`, or `docker.sock` | |
| `file.path_writable` | other users can write a directory in the root `PATH`, or `PATH` has a relative entry | |
| `kernel.sysctl` | | a hardening value differs from the recommended value |
| `patch.reboot` | | a package asks for a reboot, or `/boot` has a newer kernel than the running kernel |
| `ctr.privileged` | edc holds every capability, so the container is privileged | |
| `ctr.docker_sock` | `/var/run/docker.sock` or `/run/docker.sock` is a socket in the container | |
| `ctr.root_user` | | edc runs as UID 0 in the container |
| `ctr.host_ns` | the container uses the PID namespace of the host | |

The database and control ports are 2375, 2379, 3306, 5432, 6379, 9200, 10250, 11211, and 27017.

The `ctr.*` checks run only inside a container. edc finds the container with the same markers as `edc info`: `KUBERNETES_SERVICE_HOST`, `/.dockerenv`, or `/run/.containerenv`. On a host, the report does not show these checks. If the container uses the PID namespace of the host, `ctr.host_ns` also compares the network namespace with the namespace of PID 1.

Run it as root. Without root, edc cannot read the firewall, `/etc/shadow`, `/etc/sudoers`, or the homes of other users. These checks show `skip`, not `pass`.

The firewall check reads `nft list ruleset`. If nft shows no input chain, it reads `iptables-save`. It counts the input rules, but it does not read the rule order. The check sees only this host. A cloud security group can still block a port, so a `fail` means that the host itself does not block the port.

The SSH checks read `sshd_config` and its `Include` files. The first value wins, as in sshd. A `Match` block applies only to some connections, so edc does not read it. Run `sshd -T` to see the effective values.

To accept a known finding, add an `[audit]` table to `config.toml`:

```toml
[audit]
accept = ["net.ip_forward", "user.root_groups"]
allow_ports = [5432]
```

`accept` holds check names from the table above. `net.exposed` does not judge the ports in `allow_ports`, and a warning names them. An unknown check name or a port outside 1 to 65535 makes the config invalid.

An accepted finding stays in the report. It shows as `skip`, and its summary starts with a marker such as `accepted (fail)`. The marker follows the language setting. In JSON, `metrics.accepted` is `true`, and `metrics.accepted_status` holds the original status. An accepted finding does not change the exit code.

If you run `sudo edc audit`, edc reads the config file of root, for example `/root/.config/edc/config.toml`. Put the `[audit]` table in that file.

`edc audit` does not match package versions against CVEs. It does not scan the whole file system, and it does not fix a finding.

## Service view (Linux)

`edc svc <unit>` shows one systemd service on one screen. It reads the unit state, the processes, the fds, the sockets, and the journal. It does not start, stop, or reload the unit.

```bash
sudo edc svc nginx
sudo edc svc -v nginx.service   # add the process rows, the fd rows, the sockets, and the journal lines
sudo edc svc --json - nginx     # one result for each view
```

Put the options before the unit name. `nginx` and `nginx.service` give the same unit. edc accepts letters, digits, and `:_.@-` only, and it refuses other unit types such as `.timer` or `.socket`.

| Result | Pass | Warn | Fail |
|---|---|---|---|
| `svc.state` | the unit is active, with no restart | the unit is active but restarted, or it is in a different state | the unit failed, or systemd cannot load it |
| `svc.processes` | edc lists the processes in the unit cgroup, with CPU% and RSS | | |
| `svc.fd` | each process uses less than 80% of its soft fd limit | a process uses 80% or more of its limit | |
| `svc.listen` | edc lists the TCP and UDP sockets that the unit processes own | | |
| `svc.journal` | the journal has no error line for the unit | the journal has error lines, and edc shows the last 5 | |

The process list shows the main process first and 10 rows at most. The count and the RSS include all processes in the unit cgroup.

Run it as root. Without root, edc cannot read the fds of other users, the socket owners, or the full journal. These results show `skip`, not `pass`. A unit with no process shows `skip` for the process views.

A socket that only systemd holds (socket activation) does not belong to a unit process. edc does not show that socket.

On macOS, or on Linux without systemd, `edc svc` shows one `skip` and exits with 0. If the unit does not exist, edc writes an error and exits with 1. A `fail` result also gives exit code 1. A usage error gives exit code 2.

### Service list

Run `edc svc` without a unit to list the services. The failed services come first. Then come the services that start or wait to restart, and then the services with restarts. The services that are not running stay out unless you add `--all`.

```bash
edc svc               # the list; a terminal opens it as a screen
edc svc --all         # also the services that are not running
edc svc --json -      # the list in metrics.services
```

In a terminal, the list refreshes every 2 seconds. Use `↑`/`↓` to select a service, `Enter` to open it, and `Esc` to go back. `/` filters by name, `f` shows only the failed services, `a` switches `--all`, `r` reads the list again, and `q` quits. An open service shows the same results as `edc svc -v <unit>`, and `r` reads it again.

In a pipe or with `--json`, edc prints one `svc.list` result and a table. A failed service makes the result `fail` and the exit code 1. A service that starts or waits to restart makes it `warn`. edc reads the list without root, but an open service needs root as above.

<a id="container-view-linux"></a>
<a id="container-view-linux-and-macos"></a>

## Container view (Linux and macOS)

`edc ctr <container>` reads one container through the Docker Engine API over a Unix socket. It runs no Docker CLI command. It does not change target state.

```bash
edc ctr web
edc ctr -v 0123456789ab
edc ctr --json - web
edc ctr --collector-image edc-collector:local web
```

Set `DOCKER_HOST` to an absolute Unix socket URL to select a socket. Set `DOCKER_CONTEXT=default` to ignore `DOCKER_HOST`. edc reads no Docker context or credential files. It rejects nondefault contexts and TCP, SSH, or TLS endpoints. Linux uses `/var/run/docker.sock`. macOS uses `~/.docker/run/docker.sock` when that socket exists. Otherwise, it uses `/var/run/docker.sock`.

The command sends read-only GET requests for normal inspection. It supports Docker API versions 1.41 through 1.51. Each request has a five-second limit. The default command deadline is 15 seconds.

By default, edc uses Docker Engine API data only. Add `--collector-image <local-image>` to a detail request for extra Linux metrics. The image must exist on the selected daemon. edc checks its immutable image ID, Linux platform, daemon architecture, and protocol label. edc does not pull, build, or publish the image. List refreshes never create a helper. Press `Enter` to open a detail. Press `r` to read that detail again.

The helper runs on the daemon host. A macOS client can read a Linux daemon VM through its Unix socket. The daemon must use rootful Linux and cgroup v2. The helper runs as UID 0. It drops all capabilities, then adds only `SYS_PTRACE` and `DAC_READ_SEARCH`. It uses the host PID and cgroup namespaces. It uses network mode `none`, a read-only root file system, and Docker log driver `none`. It never uses `SYS_ADMIN`, privileged mode, a Docker socket mount, or a host root mount.

The helper mounts `/sys/fs/cgroup` read-only. It mounts one validated container log file read-only for `json-file` or `local` drivers. It reads file metadata only. It does not read log contents. The collector rejects rootless daemons, user namespace modes, and cgroup v1. LSM or Enhanced Container Isolation policies can deny the requested access. edc does not widen permissions or retry with extra capabilities.

The collector sends JSON through the attach stream. It writes no metric history to a database, file, or Docker log. The Docker Engine retains the local image and temporary helper metadata or events. Successful cleanup removes the helper container. If cleanup fails, the report shows the helper name and ID. Inspect that ID before manual removal.

Each collector metric has a status and a reason when collection is incomplete or unavailable. API results remain visible if the helper fails. FD metrics show counts and soft limits for target cgroup processes. Listener metrics show TCP and UDP sockets that match target process FD inodes. The helper does not infer IPv6 wildcard coverage or accept queue limits. OOM counters cover the container cgroup lifetime. The helper uses `fstat` for an eligible log file and does not read its contents. If the container has no process, FD, listener, and OOM metrics are unavailable. The helper can still report an eligible log file size.

Build one helper binary per target architecture. Run these commands from the repository root:

```bash
make collector-build COLLECTOR_ARCH=amd64
make collector-build COLLECTOR_ARCH=arm64
```

The target builds only a local binary. It does not build, pull, or publish an image. `collector.Dockerfile` uses `scratch` and the selected binary. Set `arch` to match the daemon. This command creates a temporary context with only the Dockerfile and binary:

```bash
set -eu
arch=amd64
collector_context="$(mktemp -d)"
trap 'rm -rf "$collector_context"' EXIT
mkdir -p "$collector_context/bin"
cp collector.Dockerfile "$collector_context/"
cp "bin/edc-collector-linux-$arch" "$collector_context/bin/"
docker build --platform "linux/$arch" --build-arg "TARGETARCH=$arch" \
  -f "$collector_context/collector.Dockerfile" -t "edc-collector:$arch" "$collector_context"
```

For an arm64 daemon, set `arch=arm64`. Use the tag that matches the daemon. Run the helper only for a detail request:

```bash
edc ctr --collector-image edc-collector:amd64 web
edc ctr --collector-image edc-collector:amd64 --json - web
```

Live daemon validation of the required capabilities and mounts is unverified. Get user confirmation before a live test. Show the Unix socket, local image ID and protocol label, target container ID, capabilities, exact mount sources, helper name, and cleanup target.

Missing API fields show an unavailable reason. A missing value does not become zero. Partial limit data stays visible. If every resource limit field is unavailable, `ctr.limits` shows `skip`. Any collector OOM values remain visible.

The list puts failed containers first, then containers that need attention, then containers with restart history. It omits containers that exited with code 0 unless you add `--all`.

```bash
edc ctr
edc ctr --all
edc ctr --json -
```

The list sends one version request and one list request. It sends one inspect request per listed container and two stats requests per container whose status is `running`. Each stats phase uses up to four concurrent requests. The list waits one second between phases to calculate CPU use. It does not request logs, process rows, or writable layer size. It never creates a helper, even when you set `--collector-image`.

The list shows CPU percent, raw memory use and configured limit, memory percent, and current PIDs. An unknown value shows `n/a`. A stopped container shows `—`. A configured unlimited memory limit shows `∞`. Memory use keeps Docker's raw `memory_stats.usage` value. It does not subtract cache. At wider widths, the list adds network I/O, block I/O, restart count, uptime, and image when each column fits. The terminal screen uses the same keys as `edc svc`. Press `Enter` to open a container. Press `Esc` to return.

In a pipe or with `--json`, edc prints one `ctr.list` result and a table. A failed container makes the result `fail` and the exit code 1. A container that restarts, pauses, is unhealthy, or was only created makes it `warn`. Socket permission errors show `skip`.

## Recent host changes (Linux)

`edc changes` lists the changes on a Linux host in a time window. It reads package logs, the mtime of the files under `/etc`, and the systemd unit state. It changes nothing, and it sends nothing over the network. It ends in less than one second.

```bash
edc changes                     # the last 24 hours
edc changes --since 2h          # the last two hours
edc changes -v                  # all rows, not only the newest 50
edc changes --json -            # all changes in metrics.events
```

The output starts with one result for each source. A merged table follows, with the newest change first.

| Result | Source |
|---|---|
| `changes.packages` | `/var/log/dpkg.log`, `/var/log/dnf.rpm.log`, `/var/log/yum.log`, and their uncompressed rotated files |
| `changes.etc` | the mtime of each file and symlink under `/etc` |
| `changes.services` | the latest start of each service unit and its restart count |

If a boot is in the window, the table shows one `boot` row. That row holds the services that started during the boot.

If edc cannot read a source, the result is `skip` with the reason. A `skip` does not mean that nothing changed. Run it as root to read all sources. Without root, edc cannot read some directories under `/etc` and some package logs.

`edc changes` does not decompress rotated logs. If a compressed log is newer than the window start, edc shows a warning. It reads the mtime under `/etc`, not the file content, so it does not show the change inside a file. systemd keeps one start time for each unit, so a unit that restarted several times shows its latest start and the restart count. The `/etc` walk stops after 20000 entries and shows `warn`. edc does not run `dnf history` or `journalctl`.

## Kernel and systemd events (Linux)

`edc events` reads the kernel log and the systemd journal for a time window. It finds failure signals and puts them in time order. It changes nothing, and it sends nothing over the network.

```bash
sudo edc events                 # the last 6 hours
sudo edc events --since 48h
sudo edc events -v              # add the events behind each result and the timeline
sudo edc events --json -        # one result for each category
```

| Result | Fail | Warn |
|---|---|---|
| `events.window` | | |
| `events.oom` | the kernel killed a process for memory | |
| `events.disk_io` | a disk or a file system reported an I/O error | |
| `events.fs_readonly` | a file system went read-only or shut down | |
| `events.lockup` | a CPU stayed in a soft or hard lockup, RCU reported a stall, or a workqueue locked up | |
| `events.oops` | the kernel reported a `BUG`, an `Oops`, a general protection fault, or a panic | the kernel issued a `WARNING`, or the kernel is tainted `D`, `L`, `M`, or `B` with no oops in the window |
| `events.link` | a network link went down and did not come back up | a link went down and came back up |
| `events.units` | the last event of a unit is a failure result | a unit failed and restarted, or restarted 3 times or more |
| `events.failed_units` | a unit is in the failed state now, also when it failed before the window | |
| `events.journal_errors` | | the journal has error lines (priority `err` or higher) in the window. The evidence counts them by unit and shows the latest line of the 10 busiest units |
| `events.segfault` | | a process stopped on a segfault or a general protection fault |
| `events.hung_task` | | a task stayed blocked for longer than the hung task limit, 120 seconds by default |
| `events.clock` | | the wall clock jumped 1 second or more |

`events.window` names the window and the sources. With `-v`, it also shows the 50 most recent signals of all categories in time order. Each other result shows its count, its latest event, and up to 20 recent events.

`events.failed_units` asks `systemctl list-units --state=failed` for the state now, so a unit that failed two days ago and stays failed appears with the default 6-hour window. `events.journal_errors` reads `journalctl -p err` for the window and groups the lines by `_SYSTEMD_UNIT`, or by the syslog identifier when a line has no unit. It leaves out kernel lines, because the kernel categories read them. An error line is a clue, not a failure, so this result warns. Without root or the `adm` or `systemd-journal` group, journalctl shows only your own lines, and the result shows `skip`. On a host without systemd as PID 1, `events.failed_units` shows `skip`.

`events.oops` also reads `/proc/sys/kernel/tainted`. The flags `D`, `W`, `L`, `M`, and `B` say that the kernel had an oops, a warning, a soft lockup, a machine check, or a bad page since boot, also when the log of that moment lies outside the window. The result shows the flags as evidence. Module flags such as `P`, `O`, and `E` appear in the evidence, but they do not change the result.

The kernel signals come from the journal entries of the kernel. If the journal has no kernel entry in the window, edc reads `/dev/kmsg`. `/dev/kmsg` holds the current boot only. The unit and clock signals come from the journal entries of PID 1. A clock jump counts only when the wall clock and the monotonic clock differ by 1 second or more, and by more than 0.1% of the time between two entries. Thus NTP slew does not count.

Run it as root, or as a member of the `adm` or `systemd-journal` group. Without that access, journalctl shows only your own entries, and `kernel.dmesg_restrict` can block `/dev/kmsg`. These results show `skip`, not `pass`. If journalctl is not installed, `events.units` and `events.clock` show `skip`.

edc reads at most 20000 journal entries, 32 MiB of journal output, and 100000 `/dev/kmsg` records. If it hits a limit, `events.window` gives a warning.

`edc events` does not show the full log. It does not follow new entries, and it does not read application logs.


## Current scope

`top`, `info`, `doctor`, and the single network probes support Linux and macOS.

On Linux, `edc` reads `/proc`, `/sys`, `ip`, `ss`, `ping`, `traceroute` or `tracepath`, and `/etc/resolv.conf`. If `resolvectl` exists, `edc` adds `resolvectl status` as evidence.

On macOS, `edc` uses a system command adapter. Linux and macOS run `capture`. `quality` runs `networkQuality` on macOS and a built-in responsiveness test on Linux. Both report `download_bps`, `upload_bps`, `responsiveness_rpm`, and `base_rtt_ms` when the run measured them; a missing value is left out. The config URL defaults to Apple's `https://mensura.cdn-apple.com/api/v1/gm/config`; `--server` or `defaults.quality.server` replaces it. An empty `server` keeps the default.

Every diagnostic command keeps to read-only inspection. `edc` runs no automatic repair, such as a DNS flush, an interface reset, or a firewall change. `edc log` writes its output, rotation archives, and lock file. `edc top --write` writes a SQLite database and its WAL files. `edc ai` writes `ai-resets.jsonl` and `ai-claude.json`, and it calls the Claude usage API.


## Listening ports

Use `listen` to inspect open ports separately from the host overview. Add `--unix` or `--all` for unix sockets, or repeat the observation with `--watch`.

```bash
edc listen
edc listen --watch -i 0.5 --duration 10s
```
