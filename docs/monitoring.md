**English** | [한국어](monitoring.ko.md) · [Back to README](../README.md)

# Live resource monitoring

[Top thresholds](#top-thresholds) · [Top dashboard](#top-dashboard) · [Top events](#top-events) · [Top JSON output](#top-json-output) · [Filter processes in top](#filter-processes-in-top) · [AI tool usage](#ai-tool-usage) · [Read logs](#read-logs) · [Search logs](#search-logs)

![Live resource dashboard](media/top.png)

Screen rendered with synthetic demo data using the current UI renderer.

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
| `1`, `c`, `m`, `d`, `n` | switch to all, CPU, memory, disk, or network columns. `1` is the first view. |
| `s` | switch to pressure columns |
| `o` | rank the process panel by CPU, `MEM`, or I/O. Press again to change the rank. The fourth press, or a view key, returns to the rank of the view. |
| `v` | Return to the box screen. |
| `f` | Open the signal view. Press again to select a process candidate. If a filter is active, open the process view. |
| `Tab` | Switch between history and process selection. |
| `?` | Open help. Use arrows to scroll. Press `Esc` to return. |
| `/` | type a process filter: a command name or a PID, comma-separated |
| `i` | Open a box with the interfaces and their traffic. Press `Space` to check an interface, `a` for all, or `p` for physical. `Enter` applies the selection from the next sample. While `-w` records, the key is off. |
| `Esc` | Go back one step. Esc closes help, a list, or process selection. Then it closes the detail or peaks panel, returns to the live row, drops the rank from `o`, and returns to the first view. Last, it clears the process filter. |
| `↑`, `↓`, `PgUp`, `PgDn`, `End` | select an earlier row, move one screen, or return to the live row |
| `Enter` | Show time details. If process selection is active, focus the selected PID. |
| `h` | show the load, CPU, iowait, and memory peaks from the last 60 seconds, each with its time |
| `e` | Open the event list. Use arrows to select an event. Press `Enter` to go to its start. Press `e` or `Esc` to return. |
| `[`, `]` | Go to the start of the previous or next event. |

The `signal` column shows the highest-priority host warning and the number of other warnings.

On Linux, the `signal` column shows `blocked N` when `i/o` is at the warning level and 4 or more tasks wait for I/O. macOS has no `i/o` value. The `U` state also covers page-in waits, so macOS shows `blocked N` when 4 or more processes are in the `U` state and `await` is at the warning level or the memory pressure level is `warn` or `critical`. On macOS, the `signal` column also shows `mem pressure warn` or `mem pressure critical` when the kernel raises the memory pressure level. A process group also gets a warning when its CPU total is 80% or more, or its I/O total is 50 MB/s or more. The warning shows the name and the process count, for example `gm (200) 100%`. Only the disk view calculates the I/O total, so the I/O warning of a group shows only in the disk view.

Network errors and drops count as a warning from one per second. Use the network view for packet counts.

The network values add the counters of all interfaces except loopback. On Linux, edc reads the interfaces of its own network namespace. On macOS, it reads the interfaces that are up. A bridge, a veth pair, or a tunnel also counts, so one packet can count on two or more interfaces. edc subtracts the counters of each interface separately. An interface that appears or disappears between two samples adds nothing to that interval.

Use `--interface` to add only some interfaces. Write interface names, glob patterns, or `physical`, separated by commas. `physical` selects the interfaces with a device under them. On Linux, `/sys/class/net/<name>/device` exists for these interfaces. `physical` leaves out bridges, veth pairs, bonds, VLANs, and tunnels, so a packet counts once on the NIC. `all` adds every interface, and it is the default.

```bash
edc top --interface physical
edc top --interface eth0,ens5
edc top --interface 'en*' --json -
```

The selection applies to the table, the dashboard, JSON, `--write`, and the device-side drops in the `n` view. If an interface name does not exist when edc starts, edc exits with code 2. If a glob or `physical` matches no interface, edc shows a warning. Until an interface that matches appears, the byte and packet rates stay at 0, `err` and `drop` show `—`, and JSON sets `network_health_supported` to `false`. With `--interface`, each JSON sample adds `interface_filter` and `interfaces`. `interfaces` holds the values of each selected interface with traffic in that interval. `edc history list` shows the selection in the `INTERFACES` column.

Set `defaults.top.interface` in the config file to use the same selection every time. `physical` is not available on macOS. There, edc shows a warning and adds every interface, so one config file can serve Linux and macOS hosts.

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

Use `--view` to choose the first view. The names are `all`, `cpu`, `memory`, `disk`, `network`, `pressure`, and `split`. `Esc` returns to this view. Set `defaults.top.view` to start with the same view every time.

Use `--panels` to keep panels below the table. The list takes `cpu`, `mem`, `disk`, `net`, `psi`, `ai`, and `events`, comma-separated. `none` removes all panels.

- `cpu`, `mem`, `disk`, and `psi` show three lines for the selected row: a summary, the peaks of the last 60 seconds, and the three processes that use the resource most. The process list on the right uses one order only, so these panels show the top processes of each resource. The `disk` panel makes edc read the I/O of each process. The `psi` panel shows the processes that wait for I/O. On macOS, edc does not collect PSI, so the `psi` panel has one line.
- `net` shows the network limits of the selected row. On macOS, edc does not collect these values, so the panel has one line.
- `ai` shows the Claude and Codex usage limits, as `edc ai` does. edc checks the limits every minute.
- `events` shows the newest events.

The first panel in the list gets space first. If a panel does not fit, the dashboard hides the full panel and shows `+net hidden` in the status line. A smaller panel later in the list can use the rows that are left. The table keeps at least 5 rows, and each box keeps at least 3 data rows.

Press `a` to show or hide the `ai` panel. edc reads the Claude and Codex data only after you turn on the panel. While `Enter` or `h` is open, that panel uses the panel area.

Set `defaults.top.panels` to start with the same list every time. If you do not set a list, the dashboard keeps its usual layout: the network view shows the `net` panel, and the events use the free space.

Set `defaults.top.process_sort` to `cpu`, `mem`, or `io` to choose the first order of the process list. `o` still changes the order.

Use `--columns` to choose the column groups of the all view. The list takes `net`, `cpu`, `mem`, `disk`, and `psi`, comma-separated, in the order that you want. `all` shows the five groups. Each group always shows its first columns, and the dashboard adds more columns while the width allows. If you remove a group, the other groups get its width. Add `+` after a name to add the columns of that group before the columns of other groups. For example, `--columns net+,cpu,mem` shows packets and network errors before `hot core`. Set `defaults.top.columns` to start with the same list every time.

Add `ai` to the list to show the Claude and Codex requests in a `claude` group and a `codex` group. Each row counts the requests that arrived after the previous row, up to its own time. Thus you can compare them with the other columns of the same row. In each group, `req` is the requests, and `tok` is the new input and output tokens together, the same value as `total` in `edc ai`. The columns do not include the tokens that the tool read from the prompt cache. `·` means 0. Claude Code writes a response to its log when the response ends, and edc reads the logs every 2 seconds, so the newest rows get their values 2 to 3 seconds later. `all` does not include `ai`. edc reads the Claude and Codex logs only when the list includes `ai`.

```toml
[defaults.top]
split = "net,disk"
view = "split"
panels = "net,ai,events"
process_sort = "io"
columns = "net+,cpu,mem"
```

`--view`, `--panels`, and `--columns` work only in the dashboard, as `--split` does. If you use `--view` with `--process`, the command exits with code 2. The config values do not stop runs that print the table.

Run `edc top setup` to choose these values with a live preview. The upper part lists the view, the column groups, the boxes, the panels, the process order, and the interval. The lower part shows the dashboard with real samples, and changes when you change a value.

- Use `↑` and `↓` to choose a row, and `←` and `→` to change a value or to choose a box or a panel.
- Press `Space` to turn a column group, a box, or a panel on or off. Press `[` and `]` to move it. On the columns row, press `+` to show more columns of a group first.
- Press `Tab` to show the preview at the full terminal size. The layout is the same as `edc top` after you save. Only the first status line shows the setup keys.
- Press `z` to show the preview at 80×24, then 120×30. edc skips sizes that do not fit the terminal. If a panel hides at the preview size, the panel row shows `+net hidden at 80×24`.
- Press `Enter` to see the `[defaults.top]` section, then `y` to save it.

`edc top setup` changes only the keys that you change. Other keys and sections keep their values. edc writes the whole file again and does not keep comments in the file. If the file changes while the setup is open, edc does not save and exits with code 2. If you quit with changes that you did not save, the command exits with code 4.

On macOS and Linux, the disk view shows IOPS and average `await` across physical disks.

On Linux, the disk view shows aggregate `busy%`, and the memory view shows memory pressure. Aggregate `busy%` can exceed 100 across multiple disks.

On Linux, some views also show optional columns. The CPU view shows `steal%` and `blocked`. `steal%` is the CPU time that the hypervisor gave to other guests. `blocked` is the number of tasks that wait for I/O now. The disk view shows `queue`, the average number of I/O requests in progress, added across physical disks. The network view shows TCP retransmitted segments (`retr/s`), sent resets (`rst/s`), and failed connection attempts (`fail/s`) per second. Optional columns appear only if all other columns fit and `signal` keeps at least 13 columns.

On macOS, the dashboard omits unsupported iowait, PSI, disk busy, file descriptor, listen overflow, softnet drop, conntrack, and I/O latency columns. The help page lists these limits. The memory and pressure views show `mem lvl`, the memory pressure level of the kernel (`kern.memorystatus_vm_pressure_level`): `normal`, `warn`, or `critical`. The CPU and pressure views show `blocked`, the number of processes in the `U` state. Linux counts tasks, so the two values are not the same unit.

On macOS and Linux, the network view shows interface errors and drops. On macOS, `edc` reads kernel interface statistics (`net.link.generic.ifdata`).

The memory view shows `swap/s`, the bytes per second that the kernel moves out to swap.

Press `s` for pressure. On macOS, it shows `mem lvl`, `blocked`, `load`, and `mem%`. On Linux, it shows CPU, memory, and I/O `some avg10`: the percentage of the last ten seconds during which at least some tasks waited for that resource. The `mem full` and `io full` columns show memory and I/O `full avg10`. This is the percentage of the last ten seconds during which all non-idle tasks waited at the same time. The Linux pressure view also shows `blocked`, the tasks that wait for I/O now, next to `io psi`. The CPU view shows the hottest core and an ASCII bar; on machines with more than 24 cores, the bar shows the first 24.

The detail view also lists the top three processes by CPU. The list refreshes in the background at most once a second, so it does not lengthen the observation interval. Without `--write`, only the dashboard collects it; the table and unfiltered `--json` output skip it. On Linux, `edc` compares the CPU ticks in `/proc/<pid>/stat` with the previous refresh, so the value covers the time since that refresh. On macOS, it uses the recent decaying average that `ps` reports.

The process panel shows `PID`, `COMMAND`, `STATE`, `CPU%`, and `MEM`. If the panel has room, it also shows `RSS` after `MEM`. `STATE` shows `run`, `sleep`, `iowait`, `zombie`, `stop`, `trace`, `idle`, `dead`, or `park`. `iowait` is the Linux `D` state: the process waits for I/O, and a signal cannot wake it. An `iowait` process waits for the disk. It does not prove that the process uses the disk.

If the terminal has 60 or more columns, the panel adds `READ` and `WRITE`. The disk view and `--process` always add them. Other views add them only when a value is available. These values are the bytes per second that reach the storage device, from `/proc/<pid>/io`. The panel shows `—` if `edc` cannot read the value. To read the processes of other users, run `edc top` as root.

`MEM` is the memory that the process holds. On macOS, it is the physical footprint, which includes compressed memory. On Linux, it is RSS plus swap (`VmSwap`). RSS does not count compressed or swapped pages, so a process with a memory leak can have a small RSS. If `edc` cannot read `MEM`, the panel shows RSS in a dim color. On macOS, `edc` cannot read this value for processes of other users, such as root or `_windowserver`. If you run `edc` with `sudo`, it reads them.

If the lowest `MEM` of each minute rises for 15 minutes, the `signal` column names the process, for example `node +360.0M/12m`. `edc` compares the lowest value of the first three minutes with the lowest value of the last three minutes, and the time is the gap between those two minutes. The rise must be 256 MiB or more, and at least half of the minutes must rise. A GC sawtooth or a single step does not count. In the memory view, the process that grows the most comes first, and the cell of each growing process takes the warning color. A dim RSS value stays dim. The warning becomes a danger if this rate fills the free memory in less than one hour. The first verdict needs about 15 minutes of samples. A pause or an `--interval` longer than one minute leaves a gap, and `edc` does not judge growth across a gap.

The panel ranks candidates by CPU in most views, by `MEM` in the memory view, and by I/O in the disk view. Press `o` to choose the rank without a change of the host columns. The title then shows the rank with `(o)`, for example `MEM rank (o)`. The I/O rank reads the I/O of every process from the next sample, so a process with low CPU and high I/O appears. The other ranks read the I/O of the candidates only. Before the list limit, the panel keeps five leaders by CPU, by `MEM`, and by `iowait` state. The I/O rank also keeps five leaders by I/O. The panel shows three candidates. If the terminal has 40 or more rows, the panel shows five.

The panel also shows up to two process groups above the candidates. A group is two or more processes with the same executable name, for example `gm (200 procs, 150 iowait)`. The groups use the rank of the panel: CPU, `MEM`, or I/O. A group must reach the minimum total of that rank: 10% CPU, 100 MB `MEM`, or 1 MB/s I/O. In the I/O rank, a group with `iowait` processes also appears. The memory total can count a shared page one time for each process, so `≤` marks it as an upper bound. A group line has no PID, and you cannot select it.

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

On Linux, the `n` view adds conntrack occupancy (`ct%`), listen overflows/s (`listen/s`), and softnet drops/s (`soft/s`). Its panel shows the selected sample's conntrack entries, TCP socket counts, and rates for listen drops, SYN cookies, conntrack drops, UDP receive-buffer errors, and softnet budget exhaustion. It also shows TCP memory pages against the `tcp_mem` ceiling, orphan sockets, and the rates of TCP memory pressure entries, aborts for memory, and pruned receive queues, plus the device-side drops that `/proc/net/dev` does not show: `rx_missed_errors` for a full receive ring and `rx_fifo_errors` and `tx_fifo_errors` for FIFO overruns, summed over the interfaces of the namespace except loopback. Use `↑`/`↓` for history, `Enter` for the five busiest interfaces and the settings at that sample, and `h` for peaks in the last 60 seconds. Conntrack occupancy warns at 90% and marks risk at 98%; neither proves a connection failure.

Collection uses the current network namespace, but softnet counters and TCP TIME_WAIT can be host-wide. TCP `CurrEstab` includes ESTABLISHED and CLOSE_WAIT. Socket counts are not local port utilization. Missing baselines, failed reads, or counter resets show `—` for rates. Conntrack statistics require an exposed `/proc/net/stat/nf_conntrack`; missing statistics do not prevent other collection.

JSON samples add `network_limits` with the namespace, `settings`, `gauges`, cumulative `counters`, and per-second `rates`. The counters and rates include `tcp_retrans_segs`, `tcp_out_rsts`, `tcp_attempt_fails`, `tcp_memory_pressures`, `tcp_abort_on_memory`, `tcp_rcv_pruned`, `nic_rx_missed`, `nic_rx_fifo`, and `nic_tx_fifo`. The gauges include `tcp_orphans` and `tcp_memory_pages`, and the settings include `net.ipv4.tcp_mem`. Readings include `status` and, where needed, `reason`; unobserved numbers are omitted. Save JSON Lines to analyze trends after the command exits.

## Top events

`edc top` keeps a warning from the `signal` column as an event when the warning stays on for 5 seconds. A warning that stops and starts again within 5 seconds stays in the same event. The dashboard keeps the last 100 events. Events stay in memory, and they disappear when the dashboard quits.

Each event keeps its worst value and the leading group and process at that sample. The candidates come from the view of the warning: I/O for disk and pressure warnings, `MEM` for memory warnings, and CPU for other warnings. Network warnings get no candidate. A growth warning names the growing process. A candidate does not prove the cause.

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
edc top --count 5 --json -
```

Each line has `time`, `hostname`, `cores`, the network and disk rates in bytes per second, the CPU values in percent, `load1`, `memory_pct`, and `swap_out_bytes_per_s`. macOS and Linux emit network errors and drops, and disk IOPS and await values. Linux additionally emits disk busy values, PSI `some avg10`, and memory and I/O PSI `full avg10`; `*_health_supported`, `disk_busy_supported`, and `psi_supported` tell consumers whether those values are supported. `memory_pressure_supported` tells whether the host reported a memory pressure level, and macOS then adds `memory_pressure` (`normal`, `warn`, or `critical`). The `--json` option removes the table and the header.

## Filter processes in top

`--process <filter>` narrows the process list to the processes you name. The filter is a comma-separated list.

A number must equal a PID. Other terms match part of the command name without case sensitivity.

A process that matches any term stays in the list.

On Linux, the command name is `comm`, which the kernel cuts at 15 characters. On macOS, it is the executable path.

```bash
# dashboard: opens the process view, and Enter shows the busiest matches
edc top --process output-mesh
# JSON lines: add a processes array to each sample
edc top --process 4321,worker --json /tmp/edc-host.jsonl
```

With `--process`, the dashboard shows a highlighted `PROCESS` bar under the title. It names the filter and the current values of the matched processes. When the terminal is narrow, the bar drops the last values first. The dashboard also opens the process view. Each row shows the matched processes at one sample. It shows the number of matches, their CPU%, memory (`mem`), threads, open files, and disk read and write per second. One core is 100 CPU%. With `--ebpf`, the row also shows the average run-queue wait and the average block I/O latency in milliseconds. A value that `edc` cannot read shows `—`. Press `1` for the host columns and `f` to return to the process view. `Enter` shows the busiest matches for the selected row.

Select a row and press `f`. If the first signal is a host value, `f` opens its view.

Press `f` again to select a process candidate. Use arrows to choose a process. Press `Enter` to focus its PID.

Press `/` to edit the filter. Press `Esc` to close process selection, then press `Esc` again to clear the filter.

Rows from a different filter show `—` for process values. They do not represent the current filter.

The filter runs before the list is cut to the busiest processes. A quiet process that matches stays in the list.

The detail view starts with totals over every match: count, CPU, memory, threads, open file descriptors, and disk I/O. Next, it lists the three busiest matches and `+N` for the rest. The remaining lines show process resource limits. The `signal` column then reflects only the matches.

With `--json`, a sample adds two fields. `processes` holds up to 50 matches, busiest first. `process_total` is the sum over every match: `count`, `cpu_pct`, `rss_bytes`, `memory_bytes`, and `threads`. CPU is 100% per core. `processes` is `[]` when nothing matches. Each process has these fields:

| Field | Meaning |
|---|---|
| `pid`, `started` | `started` is the start time in UTC. A PID can be reused, so `(pid, started)` names one process. |
| `command`, `cpu_pct`, `rss_bytes` | name, CPU since the last refresh, resident memory |
| `memory_bytes` | the memory that the process holds: physical footprint on macOS, RSS plus `VmSwap` on Linux. In `process_total`, it is absent if one match has no value. |
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
sudo edc top --process output-mesh -d
sudo edc top --process output-mesh -d --json /tmp/edc-host.jsonl
```

The dashboard adds a third line to the detail view: `ebpf 1s · runq 7584 avg 5.80ms p95 <16.384ms · io 704 avg 0.07ms p95 <0.256ms`. On Linux, the line ends with the three system calls with the longest waits, for example `· wait futex 1.20s epoll_wait 800.00ms read 12.00ms`. With `--json`, each process and `process_total` get an `ebpf` object:

| Field | Meaning |
|---|---|
| `window_s` | seconds the counts cover, since the previous sample |
| `io_supported`, `p95_supported` | whether the source can count block I/O and keep a distribution. Both are `true` for `ebpf` and `false` for `libproc`. |
| `runq_count`, `runq_avg_ms`, `runq_p95_ms` | On Linux, times a thread of the process became runnable and then got a CPU, and the wait in between. On macOS, the context switches in the window and the average wait per switch, with no p95. A high wait with a low `cpu_pct` means the CPU is oversubscribed. |
| `io_ops`, `io_bytes`, `io_avg_ms`, `io_p95_ms` | block I/O requests the process issued, their bytes, and the time from issue to completion |
| `waits` | Linux only: up to five system calls with the longest waits in the window. A wait starts when a thread of the process sleeps and leaves the CPU, and it ends when the thread runs again. Each entry has `syscall`, `count`, and `total_ms`. `-` is a wait outside a system call, for example in a page fault. |
| `context_switches`, `unreadable` | macOS only: the context switches in the window, which `runq_count` repeats, and the matches that macOS refused to read |

`p95` is the upper edge of the power-of-two bucket that holds the 95th percentile, so the real value is below it. A latency is left out when there were no events. `process_total` merges every match, and its p95 comes from the merged distribution. `edc` watches the 4096 busiest matches.

- A block I/O request belongs to the task that issues it. Synchronous reads, direct I/O, and `fsync` land on the process. Buffered writes are issued later by a kernel flusher, so they land on `kworker`. Use `disk_write_bytes_per_s` for those bytes.
- `edc` translates the kernel's PIDs into the PID namespace it runs in, so the filter matches inside a container as well.
- The first sample after a process appears has no `ebpf` object, because the counts start when `edc` begins to watch it.
- `source` in the `ebpf` object names how the values were counted: `ebpf` on Linux, `libproc` on macOS.
- A wait counts in the sample in which it ends. A thread that sleeps for the whole window does not add to that window. A wait includes the run-queue wait after the wakeup, so do not add `waits` to `runq_avg_ms`. After `edc` starts to watch a process, it counts the waits of a thread only after the thread wakes once.

macOS has no public hook for scheduler or block I/O events. On macOS, `-d` therefore counts only the CPU wait, without root, from libproc's cumulative counters. The wait is the runnable time minus the CPU time, and `runq_count` is the number of context switches in the window, so `runq_avg_ms` is the average wait per switch. There is no distribution, so `runq_p95_ms` is left out, and I/O cannot be counted, so the `io_*` fields are absent. The dashboard shows `n/a` in those cells. Without root, macOS refuses to read the processes of other users. `edc` counts them in `unreadable`, leaves out the `runq_*` fields when it read none of the matches, and shows `root` in the dashboard. The x86_64 build stops `-d` under Rosetta, because the translated clock may not match the kernel counters; use the arm64 build.

```bash
edc top --process Safari -d
```

On macOS, a process in the `U` state (uninterruptible wait) shows as `iowait` like Linux `D` and joins the blocked candidates.

## AI tool usage

`edc ai` shows the tokens that Claude Code and Codex use on this host. It also shows the account limits of both tools and their reset times.

```bash
edc ai                      # dashboard (press q to quit)
edc ai --count 3            # three samples as a table
edc ai --count 1 --json -   # one JSON line
```

In a terminal, `edc ai` opens a dashboard. Each row of the table is one interval: 10s, 1m, 5m, or 1h. To change the interval, press `+` or `-`. The `▸` row is still open. To move through 24 hours of history, use `↑`, `↓`, PgUp, PgDn, and End. The `Σ` rows sum the last 10 minutes, the last hour, and the last 24 hours.

edc counts the tokens from the local logs. Every 2 seconds, it reads only the new lines.

- Claude Code: `~/.claude/projects/**/*.jsonl`. edc counts each message once.
- Codex: the growth of the cumulative token count in `~/.codex/sessions`.

The table counts this host only. The tokens that other machines or claude.ai use show only in the limit percentages.

`in` is new input, and `cache` is input that the tool read from the prompt cache. `total` is `in + out`. It does not include `cache`, because the cache reads grow with the length of a conversation and hide the work that the tool did. `hit` is `cache / (in + cache)`, rounded down. For Claude, `in` includes the tokens that Claude Code wrote to the cache, so these tokens count as misses. If an interval has no input, `hit` shows `-`. The `--json` output has `cache_hit_percent` with one decimal place, and it omits the field for an interval without input. The table needs 88 columns.

The limit boxes show each window with its use, its reset time, and the time left.

- Codex: edc starts `codex app-server` and asks it. The `codex` command must be in `PATH`.
- Claude on Linux: edc calls `https://api.anthropic.com/api/oauth/usage`, the API behind `/usage` in Claude Code. It sends the token in `~/.claude/.credentials.json`. edc does not print, store, or refresh this token.
- Claude on macOS: edc reads the usage that the Claude Code status line gives to `edc ai statusline`. See the end of this section.

Anthropic does not document the Claude usage API, and the API answers frequent calls with HTTP 429. So edc calls it at most every 5 minutes. After a 429, edc waits 10 minutes, then 20 minutes, then at most 30 minutes. Until the next call, the box shows the saved values and their age.

`--poll` sets the time between limit calls. The default is 60s, and the minimum is 30s. On Linux, Claude keeps its own interval of 5 minutes or more.

Without a terminal, or with `--count` or `--json`, edc prints samples in place of the dashboard. `--count N` stops after N samples, and 0 runs until you stop edc. `--json <path|->` writes one JSON object for each sample. A file gets mode 0600.

edc writes each reset that it detects to `ai-resets.jsonl`. It keeps the last Claude values in `ai-claude.json`. Both files are in the directory of the history database. For that directory, see [Top recordings and history](history.md#top-recordings-and-history).

On macOS, Claude Code keeps the token in the Keychain. edc does not read the Keychain. Claude Code gives the 5-hour and 7-day limits to its status line command. `edc ai statusline` saves these limits to `ai-claude-statusline.json` in the same directory.

To connect the status line, install edc in its permanent location first. Then run this command:

```sh
edc ai statusline install
```

The command changes only `statusLine.command` in `~/.claude/settings.json`. It keeps the old command, so `cship` becomes `edc ai statusline -- cship`. The old file stays in `settings.json.edc-backup`. To restore the old command, run `edc ai statusline uninstall`.

The values come from the last Claude Code request. The box shows their age. The text output adds an `updated` line when the values are 1 minute old or more. The macOS box does not show the plan name. The status line input does not include it.

## Read logs

`edc view <file>` reads a log with color. In a terminal, it opens a screen at the end of the file. The screen follows new lines as the file grows. You do not need `-f`.

```bash
# screen: the last 10,000 lines, then each new line
edc view /var/log/nginx/access.log
# keep more lines on the screen and in the stats
edc view -n 50000 app.log
# read another command's output
kubectl logs -f deploy/api | edc view
# pipe: the bytes stay unchanged, and -f follows like tail -F
edc view -f app.log | grep timeout
```

`edc view` reads three formats. It selects the format from the first lines.

| Format | Example | Color |
| --- | --- | --- |
| access | nginx and Apache common and combined logs | status class, time in gray |
| json | one JSON object on each line | level, time in gray; other keys as `key=value` |
| level | `2026-10-26 10:00:00 ERROR message` | level word, time in gray |

The level must be an uppercase word such as `ERROR` or `WARN`, or a `level=` value. A lowercase "error" in the message is not a level. In the level format, a line without a level belongs to the line above it. This keeps a stack trace with its error.

Press Up or PgUp to stop the list. The header then shows `paused` and counts the new lines. Press End, `l`, or Esc to follow again. Press `/` to filter the lines by text. Press Enter to show the fields of a line, the whole line, and the stack trace lines below it.

The stats panel shows the lines per second, the errors, the status classes or the levels, the last error, and the top paths and clients. It counts only the lines that the screen keeps, 10,000 by default. If the terminal has 120 columns or more, the panel is on the right. If the terminal is narrower, the panel is below the list. Press `i` to hide the panel.

If logrotate moves the file, `edc view` reads the rest of the old file. Then it marks the place and reads the new file from the start. If the file becomes smaller, or if the bytes it last read are no longer there because the file was rewritten in place, it reads the file from the start again.

If the output is not a terminal, or if `NO_COLOR` is set, `edc view` writes the bytes unchanged, like `cat`. `-n N` writes the last N lines and keeps at most 64 MB of one line; a longer line keeps its first part, and stderr says how many lines were cut. `-f` follows the file like `tail -F`. If you give `-f` without `-n`, it starts with the last 10 lines.

## Search logs

`edc grep <pattern> [path...]` searches logs. It prints lines like `grep`: the path when you give several files, the line number with `-n`, and context lines with `-C N`. It searches a directory recursively. It skips binary files and reads `.gz` files in order.

```bash
# plain text in a directory
edc grep timeout /var/log/app/
# 5xx responses in the last 10 minutes
edc grep --since 10m --ordered --status 5xx /var/log/nginx/access.log
# the newest 20 errors, with their stack traces
edc grep --level error --last 20 app.log
# regular expression, any case, two lines of context
edc grep -E -i -C 2 'order=[0-9]+' app.log
# another command's output
kubectl logs deploy/api | edc grep -i refund
```

With `--level`, `--status`, `--since`, or `--until`, you can leave out the pattern when the first argument is an existing file or directory.

`edc grep` reads less than `grep` in three ways.

| Option | What it skips |
| --- | --- |
| `--since` with `--ordered` | `--since` and `--until` filter lines by their timestamps. Add `--ordered` when the log is in time order. Then `edc grep` checks the order at 16 points, skips a file whose last timestamp is before `--since` (a `.gz` file by its modification time), and bisects the timestamps to start near `--since`. A recent window of a multi-GB log reads a few MB. Without `--ordered` it reads the file whole, because a few probes cannot see a short backward run between them. |
| `--last N` | It reads the file from the end and stops after N matches. |
| index | On the second search of a file of 64 MB or more within 24 hours, it builds an index in the cache directory. Later searches read only the blocks that can hold the pattern, the time, the level, or the status. The time ranges in the index are exact, so this works without `--ordered`. |

The index records, for each 1 MB block, the letter trigrams, the time range, the levels, and the status classes. It takes 0.8% of the file. The first search of a file reads the file whole, like `grep`, and leaves a marker. `--index` builds the index now, and `--no-index` ignores it. `edc grep` discards the index if the file shrinks or if its first or last indexed block changes. If a block does not match its checksum, the search drops the index and reads the file whole. When more than 20% of the blocks could match, the file is read in order instead.

The index assumes a log that only grows. A file edited in place keeps its index until it shrinks or its first or last indexed block changes. Use `--no-index` or `--verify-index` for such a file. When stderr is a terminal, every search reports whether it used, built, or skipped the index. Use `--verify-index` to compare the index result with a whole read.

`--level warn` matches lines at warn or above. Lines of a stack trace carry the level and the time of their `ERROR` line. `--status` accepts `5xx`, `404`, and `500-503`, separated by commas. `--since` and `--until` accept `10m`, `2h`, `1d`, `2026-10-27 09:00`, and `09:00`.

Exit codes: 0 when a line matched, 1 when no line matched, 2 on an error.
