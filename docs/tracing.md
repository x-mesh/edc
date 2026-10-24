**English** | [한국어](tracing.ko.md) · [Back to README](../README.md)

# Tracing and packet capture

Inspect requests, connections, process activity, and kernel waits. Support and privileges vary by trace; HTTP tracing requires Linux 5.15 or later with the required eBPF capabilities. See each section for its requirements and measurement limits.

![HTTP trace event list with message preview, rendered from synthetic demo data](media/trace-http.png)

The current UI renderer uses synthetic demo events. The screen shows the split preview opened with `i`. [Reproduce the capture](capture/README.md).

[Capture](#capture) · [trace tcp / udp](#trace-tcp--udp) · [trace io](#trace-io) · [trace priv](#trace-priv) · [trace sched](#trace-sched) · [trace exec](#trace-exec) · [trace lock](#trace-lock) · [trace syscall](#trace-syscall) · [trace reclaim](#trace-reclaim) · [trace cpu](#trace-cpu) · [trace show](#trace-show) · [trace dns](#trace-dns) · [trace arp](#trace-arp) · [trace ndp](#trace-ndp) · [trace http](#trace-http) · [trace mysql](#trace-mysql) · [trace clickhouse](#trace-clickhouse) · [trace postgres](#trace-postgres) · [trace redis](#trace-redis) · [trace socket](#trace-socket) · [trace drop](#trace-drop) · [PCAP file](#pcap-file)

## Capture

`capture` requires packet capture privileges. Capture supports Linux and macOS. Capture keeps hard limits of 60 seconds and 10,000 packets. Capture does not overwrite an existing file. Install `tcpdump` in `/usr/sbin`, `/usr/bin`, `/sbin`, or `/bin`. Capture does not search `PATH` for `tcpdump` or `sudo`.

Use `--mode events` on Linux to record TCP socket state, retransmission, reset, and destroy events with process metadata. The mode writes JSONL and requires BTF and eBPF capabilities. It does not create a PCAP file. In each event, `timestamp_ns` is Unix epoch time in nanoseconds, the same clock as the summary line. `boot_time_ns` is the kernel monotonic time since boot. In a TCP event, `pid` and `process` show the process that uses the socket, not the task that ran on the CPU when the kernel handled the packet. If you press Ctrl-C, capture stops collection. It writes the events that it collected and the summary line.

## trace tcp / udp

Use `trace tcp` or `trace udp` on Linux or macOS to print network events as they arrive. The default terminal view scrolls through events. The command runs until you press Ctrl-C. It then prints a summary.

On Linux, `trace tcp` also reports change-based `tcp_sample` events. These events show RTT, RTT variation, congestion window, slow-start threshold, unacknowledged packets, lost packets, and zero-window state. A valid field can contain `0`. An absent field is unavailable. RTT and RTT variation use microseconds. Linux stores `srtt_us` with three fixed-point bits and `rttvar_us` with two fixed-point bits. edc shifts these values before output. Zero-window means that an ESTABLISHED TCP socket has `snd_wnd` equal to zero. It does not mean that the accept queue is full. `tcp_sample` does not run on each send or receive event. It runs when a connection reaches ESTABLISHED or retransmits, and only emits changed values.

`tcp_accept` records TCP handshake completion. `tcp_app_accept` records a successful return from `inet_csk_accept`. It reports the time from child ESTABLISHED to application accept and the listener accept queue values. Failed accepts do not produce `tcp_app_accept`. `ListenDrops` and `ListenOverflows` are network namespace counters. edc does not assign them to one listener.

Use Enter in the terminal view to see raw event JSON. The TCP connection JSON includes the last observed diagnostic values. The text summary shows mean RTT, lost packets, zero-window state, and mean application accept time for each process and peer. Linux 5.15 load and attach verification is not complete.

## trace io

Use `trace io` on Linux to measure block I/O latency. It records queue latency from request insert to issue, service latency from issue to complete, and total latency. It keeps the submitter process and cgroup at request insert. The completion context does not change that attribution. A device with the `none` I/O scheduler sends most requests to the driver without an insert. For these requests, `attribution` is `issue`, and the event has no `queue_ms`. The process and cgroup then come from the issue context, and this context can be a kernel worker.

```bash
edc trace io --duration 15s
edc trace io --device 8:0 --group-by device
edc trace io --process postgres --raw
edc trace io --container database --json io.json
```

`trace io` reports read and write operations, bytes, and average, p95, and maximum queue, service, and total latency. By default, it emits requests at or above 1ms. Use `--slow <duration>` to change this threshold, for example `--slow 200us` on a fast NVMe device. The summary reports ring loss, pending-map insertion failures, unmatched completions, incomplete requests, and requeues. `Below threshold` is the number of requests that completed faster than `--slow`, on all devices and before the `--process`, `--device`, and `--container` filters. A device with a write-back cache can complete all writes in less than 1ms. In this case `Writes` is 0, and the writes are in `Below threshold`. A request without saved issue state has no invented latency or process owner.

Use `--group-by device`, `--group-by process`, `--group-by cgroup`, or `--group-by event` for I/O reports. `--raw` writes each completed request and then the summary as JSONL. In each event, `timestamp_ns` is Unix epoch time in nanoseconds, the same clock as the summary line. `boot_time_ns` is the kernel monotonic time since boot. `--json` writes the summary report. `trace io` requires kernel BTF, block request tracepoints, `CAP_BPF`, and `CAP_PERFMON`. It does not require `CAP_NET_ADMIN`. It finds the tracepoints in kernel BTF and does not require tracefs, so it runs in a container without a tracefs mount.

```bash
edc trace tcp
edc trace tcp --duration 15s
edc trace tcp --process slackbot --destination 100.66.11.194:443 --json trace.json
edc trace tcp --raw
edc trace udp --duration 15s
edc trace udp --group-by target
edc trace udp --group-by source
edc trace udp --group-by port
edc trace tcp --group-by process
edc trace tcp --group-by event
edc trace tcp -d
```

## trace priv

Use `trace priv` on Linux to see how processes use privileges. edc watches three things and prints one summary at the end.

- Capability checks: each `capable()` and `ns_capable()` result, counted in the kernel by process, capability, and result.
- Credential changes: each change of the uid, euid, gid, or egid of a thread, for example from `setuid()`, `setresgid()`, or the exec of a setuid program such as `sudo`.
- Module loads: each kernel module that a process loads, with the module name.

```bash
edc trace priv --duration 30s
edc trace priv --denied --container web
edc trace priv --raw
```

The capability table shows denied checks first. `(noaudit)` after `denied` means that the kernel only asked whether the process has the capability, to choose what to do. Such a check does not refuse an operation, and the `denied` count in the summary does not include it. For example, a database process can ask for `CAP_SYS_ADMIN` hundreds of times per second in this way. Use `--denied` to hide the allowed checks.

The `DENIED`, `CREDENTIALS`, and `MODULES` sections list each event:

```
17:54:46.615953  sudo 3969/3969  cred  euid 1000→0
17:54:46.648788  modprobe 3970/3970  module  dummy
```

edc prints at most 1,000 denied checks per second on each CPU and counts the others as `sampled_out`. It does not print the credential changes that keep the same uid, euid, gid, and egid, such as a change of the capability sets. `--raw` prints each event as JSON, with the summary as the last line.

edc checked the counts on Linux 6.17 and on Linux 5.15. A test thread removed `CAP_SYS_NICE` from its effective set and asked for a lower nice value 100 times. edc counted 100 denied `CAP_SYS_NICE` checks. A thread that set its egid to 12345 and back showed `egid 0→12345` and `egid 12345→0`. In the 5.15 VM, `sudo` showed `euid 1000→0` and `uid 1000→0`, and `modprobe dummy` showed the module load.

On a 12-CPU host with Linux 6.17, the capability program ran about 670 times per second and took 0.6 µs per check. The credential program ran 24 times per second and took 0.75 µs.

The trace needs BTF and the `cap_capable` and `commit_creds` functions and the `module_load` tracepoint. If one is missing, edc prints a note and watches the others.

## trace sched

Use `trace sched` on Linux to measure scheduler delays. The trace reports wakeup-to-run after sleep, runnable queue after preemption, and off-CPU spans. The off-CPU spans include the two delay types as subsets. It reports voluntary and preempted off-CPU spans separately.

```bash
edc trace sched --duration 15s
edc trace sched --process api --group-by process
edc trace sched --group-by event --json sched.json
edc trace sched --process api --group-by syscall
edc trace sched --raw
```

Each span also shows the system call that the thread was in when it left the CPU. The name tells the type of wait. For example, `futex` is usually a lock or a condition variable, `epoll_wait` and `poll` wait for events, `read` and `recvfrom` wait for data, and `nanosleep` is a sleep. The wakeup span has the same system call as the off-CPU span before it. `-` means that the thread did not wait in a system call, for example in a page fault, or that it is a kernel thread. Use `--group-by syscall` to add the spans by system call. In the full-screen view, press `s`. Raw events have a `syscall` field. edc reads the number from the saved user registers when the thread leaves the CPU, so the trace does not attach to the system call tracepoints.

By default, the trace emits spans of at least 1 ms. Use `--slow <duration>` to change this threshold, for example `--slow 100us`. It uses fixed pending maps and a fixed ring buffer. The summary and raw JSON summary show ring loss, map full, unmatched, repeated wakeup, and omitted span counts. On Linux kernels that expose the required BTF fields, raw events include the cgroup v2 ID and `--container` filters this ID. Use `--group-by cgroup` to group rows by this ID. edc verifies these fields before it loads the program. The trace works on Linux 5.15. In each raw event, `timestamp_ns` is Unix epoch time in nanoseconds, the same clock as the summary line. `boot_time_ns` is the kernel monotonic time since boot.

## trace exec

Use `trace exec` on Linux to see processes start and end, to see who sends a signal, and to see which process the OOM killer stops. Each `exec` event shows the PID, the parent, the user, and the first 256 bytes of the command line. Each `exit` event shows the exit code, the signal that ended the process, whether it dumped core, and how long the process lived. The lifetime comes from the start time of the task, so a process that started before the trace also shows it. Each `signal` event shows the signal, the sender, the target, and whether the kernel delivered or ignored it.

```bash
edc trace exec --duration 15s
edc trace exec --process cron,sh --group-by parent
edc trace exec --signal all --raw
edc trace exec --group-by event --json exec.json
```

The summary groups the rows by process name. It counts the execs, the exits, the exits with a non-zero code, the exits by a signal, and the processes that lived less than 1 second, with the average and the longest lifetime. A high `SHORT` count with a high `EXEC` count is a storm of short-lived processes, for example a health check that forks a shell every second. Use `--group-by parent` to see which process starts them. A second table lists the signals by sender and target. `(kernel)` after the result means that the kernel made the signal, for example `SIGSEGV` after a fault or `SIGKILL` from the OOM killer, and the sender is then the process that was running at that moment.

An `oom` event shows each process that the OOM killer selects. The `TRIGGER` column shows the task that tried to allocate memory when the kernel ran out. On Linux 6.x, the event also shows the name of the victim, its anonymous, file, and shmem RSS, and its `oom_score_adj`. On Linux 5.15, the kernel gives only the PID of the victim. The `exit` event of the victim shows `oom-killed`, and the `OOM` column counts these exits. This mark comes from the kernel state of the process, so it is also correct on Linux 5.15. The `--container` filter matches the cgroup of the trigger task for an `oom` event.

By default, edc records the signals that end a process or dump core: `HUP`, `INT`, `QUIT`, `ILL`, `TRAP`, `ABRT`, `BUS`, `FPE`, `KILL`, `USR1`, `SEGV`, `USR2`, `PIPE`, `TERM`, `STKFLT`, `XCPU`, `XFSZ`, and `SYS`. It leaves out the signals that run all the time, such as `CHLD` and `ALRM`. Use `--signal` with a comma-separated list of names or numbers, `--signal all`, or `--signal none`. The summary counts the signals that the filter dropped as `filtered_signals`. `--process` accepts names or PIDs, and a signal matches on the target. `--container` keeps the events of one container.

edc reads the command line from the memory of the new process at the time of the exec, so it holds the arguments that the program received, not later changes. If edc cannot read it, the event shows the path of the executable. Lines longer than 256 bytes end with `…`, and `args_truncated` counts them. A thread that exits does not make an `exit` event, only the last thread of a process does. Kernel threads do not appear. The trace needs the `sched_process_exec`, `sched_process_exit`, and `signal_generate` tracepoints with BTF. The `oom` event also needs the `oom/mark_victim` tracepoint in tracefs. If tracefs is not available, for example in a container, edc prints a notice and traces the other events.

## trace lock

Use `trace lock` on Linux to see where threads wait for locks. edc measures two kinds of wait and prints one summary at the end.

- A kernel lock wait is a contended spinlock, rwlock, mutex, rwsem, or per-CPU semaphore in the kernel. edc uses the `lock:contention_begin` and `lock:contention_end` tracepoints, which Linux 5.19 and later have. A mutex wait starts when the task starts to spin and includes the sleep after the spin.
- A futex wait is a call to `futex_wait` from user space. A contended `pthread_mutex`, a condition variable, and the Go runtime all wait this way, so a futex wait is not always contention.

```bash
edc trace lock --duration 10s
edc trace lock --process api --kind futex
edc trace lock --container db --json lock.json
```

The `KERNEL LOCK` table groups the waits by lock, lock type, and caller. A static lock shows its name, for example `tasklist_lock` or `rtnl_mutex`, and other locks show their address. The type uses the names of `perf lock`: `spinlock`, `rwlock:R`, `rwlock:W`, `rwsem:R`, `rwsem:W`, `mutex`, `rtmutex`, `pcpu-sem:R`, and `pcpu-sem:W`. The caller is the first kernel function in the stack that is not a lock function. `PROCESS` is the process that waited most often.

The `FUTEX` table groups the waits by process and futex address. If the address is a global variable, edc shows its name from the ELF symbols, for example `main.hotWord` or `runtime.sched+0x120`. Other addresses show the mapping and the address, for example `[anon] 0x7f3a1c000960`. A program without a symbol table, such as a Go program built with `-s`, shows only the file name. `CODE` is the two innermost user frames of the stack with the most wait time.

edc checked the counts. A test program waited 800 times on one global futex. edc counted 800 waits on `main.hotWord`, on Linux 6.17 and on Linux 5.15. For a program that maps and unmaps memory from 8 threads, `perf stat` counted 359,516 `lock:contention_end` events, and edc counted 359,506 waits. `perf stat` also counts from the fork, before the program has its name.

When the trace runs, each kernel lock contention and each futex wait on the host goes through a BPF program. On a 12-CPU host with Linux 6.17, a contention took 1.57 µs and 0.34 µs in the two programs for a selected process, mostly to read the stack. A process that the filter does not select took 0.26 µs and 0.07 µs. A futex wait took 1.59 µs and 0.42 µs, or 0.29 µs and 0.08 µs. The test program did the same work in the same time with and without the trace. Because the trace makes each contention longer, the number of contentions can change: the same program had 358,687 contentions with a filter that selected nothing and 203,922 with the trace on it.

On Linux 5.15, the kernel has no contention tracepoints. edc prints a note and shows only the futex waits. The trace needs BTF and task local storage.

## trace syscall

Use `trace syscall` on Linux to count the system calls of each process. edc counts in the kernel. On a terminal, the table updates every second, and Ctrl-C prints the final table. If you pipe the output or use `--json`, edc prints one summary at the end. With `--slow` or `--errors`, it also prints each call that matches.

```bash
edc trace syscall --duration 10s
edc trace syscall -- curl -s https://example.com
edc trace syscall --process nginx,1234 --duration 30s
edc trace syscall --group-by syscall --json syscall.json
edc trace syscall --container web --group-by cgroup
edc trace syscall --process api --errors ENOENT,EACCES
edc trace syscall --syscall openat,statx --slow 10ms --raw
```

Each row is one process name and one system call. The row shows the calls, the errors with the most frequent errno, and the time in the call: total, average, p95, and maximum. The time is wall time inside the call, not CPU time. Calls that wait, such as `epoll_wait` and `futex`, are therefore at the top. The total adds the time of all threads, so it can be longer than the trace. The rows are in order of total time. The table shows 20 rows. Use `--all` to show every row, or use `--json` to get all rows in a file. P95 is the upper bound of a histogram bucket, the same as in `top -d`. The table shows it as `<1.05s`.

Use `--group-by syscall`, `--group-by process`, or `--group-by cgroup` to add the rows over one column. `--process` takes PIDs and command names, separated by commas. edc compares them in the kernel, so a name must be the exact command name of the process, up to 15 bytes. A PID matches all threads of the process. `--container` keeps the calls of one container.

To trace a command that ends quickly, put it after `--`. edc starts the command in a new cgroup after the trace is on, so the first call is counted. The calls of the processes that the command starts are counted too. The trace ends when the command ends, and edc returns the exit code of the command. Run it as root. You cannot use `--process`, `--container`, or `--duration` with a command. If cgroup v2 is not available, edc counts only the command itself and prints a warning.

edc also prints a `FILES` table, and the live screen shows it too. Each row is one path. The columns count the opens for reading (`READ`) and writing (`WRITE`), the `mkdir`, `link`, and `symlink` calls and the opens with `O_EXCL` (`CREATE`), the deletes (`DELETE`), the renames (`RENAME`), and the other changes (`OTHER`). A rename row shows the source path. `WRITE` includes the opens that create the file, because the kernel does not tell if an open with `O_CREAT` made the file. `OTHER` also counts the `stat` and `access` calls that failed, so the table shows the files that the process looked for and did not find. The `openat2` calls are in `OTHER`, because edc does not read their flags. The `ERRORS` column shows the number of failed calls and the most frequent errno. A failed open still counts in its column.

The rows with changes are first. The rows with failed calls are next, and the rows that were only read are last. The table has 20 rows. `--all` shows every row, and `--json` holds all rows in `files`. Use `--no-files` to leave the table out. A trace with `--slow` or `--errors` has no file table unless you add `--files`. edc records at most 10,000 file events for each CPU in each second, and it records at most 10,000 paths. If edc misses events, the table says so.

`trace syscall` also keeps events, as `edc top` does. A condition that holds for 5 seconds becomes an event, and a condition that stops and starts again within 5 seconds stays in the same event. The live screen shows the latest 3 events, and the final output lists all of them with the start time, the duration, the peak per second, and the count. A `●` mark means the condition is still on. `--json` holds them in `events`. There are three kinds:

- `error loop`: one process fails the same system call with the same errno at least 100 times each second. Errors that are normal in busy programs do not count: `EAGAIN`, `EINTR`, `EINPROGRESS`, the restart errors, `ETIMEDOUT` of `futex`, and `ECHILD` of `wait4`.
- `missing path`: calls on one path fail with `ENOENT` or `ENOTDIR` at least 50 times each second, for example a `stat` on a file that does not exist. A permission error on a path that exists is not a missing path. This kind uses the file events, so it needs the file table, and the limit of file events applies. The peak can be lower than the real rate.
- `call spike`: one process calls one system call at least 1,000 times each second, and at least 10 times the average of the last 30 seconds. A process that always calls a lot has no event. The first 5 seconds have no average, so they have no event.

The limits are fixed. A trace with `--slow`, `--errors`, or `--raw` prints each call, and it has no events.

Use `--syscall` with names separated by commas to count only these system calls. edc checks the number in the kernel, so the other calls do not reach the map. The names come from the system call table of the CPU architecture. On amd64, a name also selects the 32-bit call with that name.

Use `--slow <duration>` to print each call that takes at least that time. Use `--errors all` to print each failed call, or `--errors ENOENT,EACCES` to print only these errors. edc checks both conditions in the kernel. Each line shows the time, the process, the PID and the thread ID, the system call, the return value or the errno, the time in the call, and the path:

```
15:04:05.000123  nginx 1234/1240  openat  ENOENT  12.3us  /etc/nginx/missing.conf
```

edc reads the path only for the calls that it prints, when the call returns. It reads up to 256 bytes, and a longer path ends with `…`. The path is the first path argument of about 50 calls, such as `openat`, `statx`, `unlinkat`, `mkdirat`, and `execve`. edc does not read the path of a 32-bit call. It does not read the path of an `execve` that succeeded, because the new program replaced the registers and the memory. If another thread changes the path buffer during the call, the line can show the new text. Control characters in the path show as `\xNN`.

edc prints at most 1,000 lines per second on each CPU. The summary counts the other lines as `sampled_out`, and the table still counts all calls. `ring_lost` counts the lines that did not fit in the event buffer. With `--json`, edc counts the lines but does not print them. Use `--raw` to print each line as JSON, with the summary as the last line.

The `IN FLIGHT AT STOP` table lists the calls that started during the trace and did not return before the end. A thread that waits for the whole trace is not in the main table, because its call does not return. A call that started before the trace returns without a start time. The summary counts these returns as `unmatched`, together with the first return of a new thread after `fork` or `clone`. With `--process`, `--container`, or `--syscall`, edc does not count them, and the summary shows `-`. `exit` and `exit_group` do not return, so edc counts them without a time.

edc leaves out its own calls. If edc runs in a container with its own PID namespace, it cannot find its own calls, and it prints a note after the summary. On amd64, a 32-bit program uses other system call numbers. edc shows these calls with `(32-bit)` after the name. A call that a signal interrupts can return a restart code that only the kernel uses, for example `ERESTARTSYS`. The table shows these codes by name.

The counts use a per-CPU map with a fixed size. edc sizes the map to 32 MiB in total, with at most 16,384 entries. Each entry is one process name and one system call. If the map is full, edc does not count the extra calls. The summary shows them as `map_full`, and edc prints a warning. `--group-by cgroup` adds the cgroup to each entry, so it uses more entries. For example, each SSH login can make a new cgroup.

When the trace runs, each system call on the host goes through two BPF programs. The cost applies also to the processes that the filter does not select. `perf bench syscall basic` measured these costs per call on a 12-CPU x86_64 host with Linux 6.17:

| Condition | `getppid` | Added cost |
|---|---|---|
| No trace | 0.577 µs | - |
| Trace runs, process not selected | 0.735 µs | 0.16 µs |
| Trace runs, process selected | 0.809 µs | 0.23 µs |

On that host, about 32,500 calls per second added less than 0.1% of the total CPU time. On a busy server, measure the cost on that host before a long trace.

`--syscall` lowers the cost of the other calls. With `--syscall openat`, `getppid` took 0.697 µs. Without the trace, it took 0.585 µs in the same measurement. A call that fails costs about 0.1 µs more, because edc also counts its errno. A printed line costs about 0.6 µs more, and a line over the limit costs about 0.05 µs more. With the limit, a 12-CPU host prints at most 12,000 lines per second.

The trace needs the `sys_enter` and `sys_exit` tracepoints with BTF, task local storage, the task iterator, and the BPF ring buffer.

## trace reclaim

Use `trace reclaim` on Linux to see how long tasks wait for memory. edc measures four kinds of stall, counts them in the kernel, and prints one summary at the end.

| Kind | When a task stalls |
|---|---|
| `direct` | The zone has too little free memory, and the task that allocates reclaims memory itself. |
| `memcg` | The cgroup reaches `memory.max` or `memory.high`, and the task reclaims memory in that cgroup. |
| `throttle` | The cgroup is above `memory.high`. Before the task returns to user space, the kernel reclaims and then puts the task to sleep. |
| `compact` | The task needs a large page and compacts memory itself. |

```bash
edc trace reclaim --duration 30s
edc trace reclaim --container web --slow 10ms
edc trace reclaim --group-by cgroup --json reclaim.json
```

Each row is one process name and one kind. The row shows the stalls, the total, average, p95, and maximum time, and the memory that `direct` and `memcg` reclaimed. The total adds the stalls of all threads, so it can be longer than the trace. A `throttle` stall includes the `memcg` reclaim that runs in it, so do not add these two rows. Use `--group-by process`, `--group-by cgroup`, or `--group-by kind` to add the rows over one column. `--process` and `--container` work as in `trace syscall`.

Use `--slow <duration>` to print each stall that takes at least that time:

```
02:13:24.479122  api 1456919/1456920  throttle  588.57ms  cgroup 15040023
```

edc prints at most 1,000 lines per second on each CPU, and counts the other lines as `sampled_out`. Use `--raw` with `--slow` to print each line as JSON.

edc does not count the work of `kswapd` and `kcompactd`. These kernel threads reclaim and compact in the background, and they do not stop the task that allocates. If a kernel has no hook for a kind, for example a kernel without memory cgroups, edc prints a note and counts the other kinds.

edc checked the counts against the kernel. In a Linux 5.15 VM under memory pressure, tracefs counted 187 `mm_vmscan_direct_reclaim_end` events for the test program and 166 `mm_compaction_end` events. edc counted 187 `direct` and 166 `compact` stalls. `/proc/vmstat` showed more `allocstall` than either, because the kernel counts `allocstall` again when one reclaim retries. In a test cgroup with `memory.max` 16 MiB, reading a 64 MiB file three times gave 584 `memcg` stalls that reclaimed 182.5 MiB. In a test cgroup with `memory.high` 16 MiB and no swap, a program that used 40 MiB stalled for up to 1.28 s at a time.

The kernel calls the `memory.high` handler each time a task returns to user space with pending work. On a 12-CPU host with Linux 6.17, this was about 2,600 calls per second. edc checks whether the task is above `memory.high`, and it starts a stall only then. The two BPF programs took 214 ns and 267 ns per call. The other hooks run only when a task reclaims or compacts.

The trace needs BTF, task local storage, and at least one of these hooks: the `mm_vmscan_direct_reclaim`, `mm_vmscan_memcg_reclaim`, and `mm_compaction` tracepoints, and the `mem_cgroup_handle_over_high` function.
## trace cpu

Use `trace cpu` on Linux to find the functions that use the CPU. edc samples the stack of the running task on each CPU, 99 times per second by default. It counts the samples in the kernel and prints one summary at the end.

```bash
edc trace cpu --duration 30s
edc trace cpu --process api,1234 --duration 10s
edc trace cpu --container web --folded cpu.folded
edc trace cpu --frequency 499 --folded - | flamegraph.pl > cpu.svg
```

The first line shows the samples and the busy share of the CPUs. An idle CPU gives a sample of the idle task, and edc counts these samples as `idle`. The busy share is the samples that are not idle, divided by the samples that the frequency gives in that time on all CPUs. Some virtual machines give fewer samples on an idle CPU. On a 4-CPU GitHub runner, the idle CPUs gave about 57% of their samples, and the busy CPU gave all of its samples. The `PROCESS` table shows the samples of each process. The `FUNCTION` table shows `SELF`, the samples where the function was the innermost frame, and `TOTAL`, the samples where the function was anywhere in the stack. A kernel function has `[k]` after the name. The percentages are of the samples that the filter kept. The table shows 20 functions, and `--json` holds all of them.

`--folded <path>` writes one line for each stack, from the outer frame to the innermost frame, with the count at the end. Flame graph tools, such as `flamegraph.pl` and speedscope, read this format. Kernel frames end with `_[k]`. If `--folded -` writes to standard output, edc does not print the tables. `--process` and `--container` work as in `trace syscall`. edc compares them in the kernel, so the other samples do not use the maps.

edc finds the kernel names in `/proc/kallsyms`. It finds the user names in the ELF symbol table of each mapped file. For a Go program, it also reads `.gopclntab`, so a Go program that `-s -w` stripped still shows its function names. For a JIT runtime, it reads `/tmp/perf-<pid>.map` in the mount namespace of the process. For example, `node --perf-basic-prof` writes this file. edc opens the files through `/proc/<pid>/map_files`, so the files of a container and deleted files also work. A frame without a symbol shows the file name, for example `[libc.so.6]`. A frame outside all mappings shows `[unknown]`. C++ and Rust names are not demangled.

The kernel follows the frame pointers to read the user stack. A program built without frame pointers shows only part of its stack, and an address after the break can show as `[unknown]`. For example, a C program built with `-O2 -fomit-frame-pointer` showed `_start;__libc_start_main;leaf_work`, but the same program with `-fno-omit-frame-pointer` showed `main;outer_work;leaf_work`. A function that does not make its own frame, often a short leaf function, hides its caller. In the example, `middle_work` is missing for this reason. With `-O0`, the full stack showed. Go programs keep frame pointers. If more than half of the samples have a user stack of one frame, edc prints a note.

A process that ends within a few milliseconds can end before edc reads its mappings, and its user frames show as `[unknown]`. edc reads the mappings of a new process when the BPF program first sees it, and again after 20 ms and 200 ms.

edc measured these costs on a 12-CPU x86_64 host with Linux 6.17. The test ran a CPU-bound program on all 12 CPUs for 5 seconds, three times for each condition:

| Condition | Work done | Change | BPF time per sample |
|---|---|---|---|
| No trace | 2,649,850 | - | - |
| `--frequency 99` | 2,652,236 | +0.09% | 1.5 µs |
| `--frequency 999` | 2,639,584 | -0.39% | 1.1 µs |

At 99 Hz, edc itself used 120 ms of CPU time in 6 seconds of sampling. It used about 1 second in total for an 8-second trace, mostly to start and to read the symbols at the end, and its memory peak was 131 MiB.

The trace needs BTF, a `perf_event_open` cpu-clock event on each online CPU, and a BPF link for perf events. Linux 5.15 has all three.

Use `trace cpu --off-cpu` to find where threads sleep instead of where they run. edc records the stack of a thread when it goes to sleep, and adds the sleep time to that stack when the thread runs again. A thread that the scheduler preempts is runnable, so edc does not count it.

```bash
edc trace cpu --off-cpu --process api --duration 10s
edc trace cpu --off-cpu --container db --folded db-offcpu.folded
```

The `PROCESS` table shows the sleeps and the sleep time of each process. The `CODE` and `KERNEL` table shows where the time goes. `CODE` is the two innermost user frames. `KERNEL` is the system call that slept, such as `futex` or `epoll_wait`, or `page_fault`. The time adds the sleeps of all threads, so it can be longer than the trace. In `--folded` output, the count is the sleep time in microseconds. Do not use `--frequency` with `--off-cpu`. edc stops with an error.

edc counts only the sleeps that start and end during the trace. A sleep that started before the trace, or that does not end before the end, is not in the result. A thread that waits for the whole trace therefore does not show.

edc measured these costs with `perf bench sched pipe` on the same host. Each operation has two context switches:

| Condition | Time per operation | Added cost |
|---|---|---|
| No trace | 3.471 µs | - |
| `--off-cpu --process no-such-process` | 3.495 µs | 0.02 µs |
| `--off-cpu --process sched-pipe` | 4.379 µs | 0.91 µs |
| `--off-cpu`, no filter | 4.475 µs | 1.00 µs |

The BPF program ran on each context switch of the host. It took 191 ns for a process that the filter does not select and 1.25 µs to 1.38 µs for a selected process, mostly to read the two stacks. On a busy host, use `--process` or `--container`.

Without a filter, the stack map can fill one hash slot with two stacks. In an 8-second trace of the host, 180 of 25,402 sleeps lost a stack in this way. edc counts them as `collisions` and prints a note. With `--process`, the same test had no collisions.

The off-CPU trace also needs the `sched_switch` tracepoint with BTF and task local storage.

Use `--raw` to print JSONL events as they arrive. Use `--json` to write the connection summary after Ctrl-C.

## trace show

Use `trace show <raw.jsonl>` to print the summary of a saved `--raw` file later, on any host and without root. It takes the same `--group-by`, `--process`, `--destination`, `-d`, and `--json` options as the live trace, and it prints the same tables. `-` reads standard input. The file holds one trace: the network, DNS, HTTP, database, socket, and drop traces, and `trace exec`. `trace io`, `trace sched`, and `trace syscall` files are not supported. Each `--raw` event carries a `socket` number, and `trace show` uses it to join the events of one connection, so a file from an earlier edc without that number groups the UDP flows and the database commands less precisely.

```bash
sudo edc trace http --raw --duration 30s > http.jsonl
edc trace show http.jsonl
edc trace show http.jsonl --group-by path -d
edc trace show exec.jsonl --group-by parent --json exec.json
```
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
edc trace http --side server --container web
edc trace tcp --container 5c9e77b3f470 --raw
```

In a bridge network, the addresses and ports are the addresses and ports inside the container. For example, a server published with `-p 8080:80` shows port 80.

In the full-screen terminal view, press `s` for source rows, `t` for target rows, `p` for port rows, `c` for process rows, `e` for event rows, or `g` for scrolling events. In `trace http`, press `u` for path rows. Press `Tab` to show the next view. Press `Shift+Tab` to show the previous view. If the terminal is wide, the first column becomes wider and shows the full group value. If the terminal is narrow, the byte columns use short units, for example `195K` for 195 KiB.
The full-screen view keeps the last 10,000 events. The live rates use the time that these events cover. The summary after Ctrl-C uses all events.
Grouped rows in the full-screen view show the groups with the most traffic first. If the rows do not fit the terminal, the view shows the top rows.

In the event list, press Up or Down (or `k` and `j`) to select an event. While an event is selected, the list stops and does not follow new events. The header shows the number of newer events. Press Enter to see all the fields of the event and the whole payload. The detail view wraps long lines. Press Up, Down, PgUp (or `b`), PgDn (or Space), `g`, or `G` to scroll, and press Esc or `q` to go back. Press `l`, Esc, or End in the list to follow new events again. Many Mac keyboards have no End key. In the list, `b` and Space also move the selection by a page. If the selected event becomes the oldest one in the list, the list shows newer events below it. The list keeps the first 4 KiB of each payload. The detail view shows the whole payload of recent events, up to 64 MiB in total.

Press `f` to open the newest event and follow new events. Press `f` again to stop at the current event. In `trace http`, the full screen collects up to 1 MiB per message by default. The list keeps a 4 KiB snippet.

The list hides payload rows at first. Press `v` to show them. `--payload` or `--payload=all` shows rows at startup. Press `m` to reveal or mask known secret headers. The header shows `secrets shown` while values appear. Press `z` in the detail view to decode gzip data. edc decodes up to 1 MiB and shows the decoded size.

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

## trace dns

Use `trace dns` on Linux to print DNS queries and answers as they arrive. It reads the DNS messages of UDP port 53 and also shows TCP connections to port 53.

```bash
edc trace dns
edc trace dns --duration 15s --json dns.json
edc trace dns --group-by target
edc trace dns --process curl --destination 127.0.0.53:53
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
edc trace dns --side server
edc trace dns --side server --group-by target
```

On the server side, the latency starts when the server reads the query and stops when the server sends the answer. The `read_delay_ms` of a server query is the time that the query waited in the receive queue of the server. A short latency usually shows an answer from the cache. A long latency usually shows a query to an upstream server. On the server side, `dns_tcp_accept` shows a TCP connection that the server accepts on port 53. If the server sent a truncated answer to that client, the event gets the name of the query. edc learns the process of a listen socket when the server calls `accept()`. So for a server that started before the trace, the first accepted connection can have no process.

`trace dns` watches only port 53. DNS over TCP puts a 2-byte length before each message. Many programs write the length and the message as two buffers, or read them in two reads. edc reads the first two buffers of a write, and it joins a 2-byte read with the next read on the same connection. It does not find a message that starts in the middle of a read. It does not show DNS over TLS, DNS over HTTPS, mDNS, or LLMNR. BPF reads the first 1,024 bytes of a DNS message. For a longer answer, edc reads the result code from the header and takes the name from the query. macOS does not give the payload of a socket, so `trace dns` requires Linux.

## trace arp

Use `trace arp` on Linux or macOS to print the changes of the IPv4 neighbor table (the ARP cache). It needs no root and no eBPF. On Linux, it reads kernel notifications through netlink as they arrive. On macOS, it reads the ARP table each second, with the same query as `arp -an`, and compares it with the previous table.

```bash
edc trace arp
edc trace arp --group-by target
edc trace arp --json arp.json --duration 30s
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

## trace ndp

Use `trace ndp` for the IPv6 neighbor table (NDP). It works the same way as `trace arp` on Linux and macOS. Its events are `ndp_new`, `ndp_state`, `ndp_mac_change`, `ndp_failed`, and `ndp_delete`, and its summary title is `NDP trace`.

```bash
edc trace ndp
edc trace ndp --group-by target
```

On macOS, the kernel puts the interface number into the link-local addresses of the table. `trace ndp` removes it, so a link-local address shows as, for example, `fe80::1`, and the `source` column shows the interface.

## trace http

Use `edc trace http` on Linux 5.15 or later to inspect HTTP/1.x and HTTP/2 traffic on both sides.

The bare command enables TLS auto discovery. It continues HTTP capture if TLS discovery or attach fails.

HTTP/1 rows appear after the headers complete. HTTP/2 rows appear after each header block.

```bash
edc trace http
edc trace http --group-by target
edc trace http --group-by path
edc trace http --side server --process nginx
edc trace http --payload
edc trace http --side server --port 8080
edc trace http --port 8080 --payload=all
```

Text output and `--raw` collect up to 1 MiB per message by default. JSON summaries and `--group-by` default to no payload.

`--payload` keeps the first 4 KiB. `--payload=all` keeps up to 1 MiB. `--payload=false` disables payload. A bare `--payload` keeps its existing 4 KiB limit.

The full screen collects complete messages by default. It hides payload rows at first.

Press Enter for details, `i` for a preview, and `v` to show payload rows.

Press `z` to decode gzip data. The list keeps 4 KiB per event and 10,000 events.

Full payload and gzip data share a 64 MiB budget. The detail view says when this budget evicts a message.

HTTP/1 and HTTP/2 share limits of 4096 open messages and 64 MiB. Each message has a 1 MiB limit.

edc marks an incomplete or evicted message with `payload_truncated`.

HTTP/1 combines split headers, then emits metadata before the body ends.

It updates the same row no more than once every 250 ms when data changes.

HTTP/1 follows `Content-Length` and chunked framing. Unknown-length streams stay open until close or trace stop. A forced final snapshot is truncated.

A request has the event name `http_request`. A response has a name from `http_1xx` to `http_5xx`. The status code is in `status`.

HTTP/1.x answers requests in order. A response matches the oldest unanswered request on its connection.

On the client, `latency_ms` starts at request send and ends at response read. A `1xx` response does not end the request.

The `target` is the `Host` header. If no `Host` header exists, the target is the server address.

On the server, `latency_ms` starts when the server reads the request and ends when it writes the response.

`trace http` shows two sides of HTTP on this host. Each event row starts with the side:

- `client:` is a request that this host sent. For example, a proxy sends requests to its backend, or a program calls an API.
- `server:` is a request that a local server received.

JSON events have `"side": "client"` or `"side": "server"`. Use `--side client` or `--side server` to keep one side.

In the full-screen view, press `/` and type `server` to keep server rows. `trace dns` shows the client side by default.

A proxy shows one request on each hop. For example, nginx receives requests on port 9900 and sends them to a backend on port 9000.

One user request then gives three rows: `server:` for nginx on port 9900, `client:` for nginx to port 9000, and `server:` for the backend.

Grouped views keep server rows separate, for example `nginx (server)`.

Use `--group-by path` to group events by request path. In the full-screen view, press `u`. Only `trace http` has this view.

The path has no query. One row includes all hosts and methods. A response goes into its request row.

A `tls_hello` event and an unmatched response go into the `-` row.

If the path contains an ID, for example `/users/123`, each ID gets a different row.

| To see | Command |
| --- | --- |
| All HTTP on this host | `edc trace http` |
| The requests that local servers received | `edc trace http --side server` |
| The requests that this host sent, for example to a backend or an API | `edc trace http --side client` |
| Both ends of the connections to port 9000 | `edc trace http --port 9000` |
| Only the requests that the proxy on port 9900 received | `edc trace http --side server --port 9900` |
| The latency of each process on each side | `edc trace http --port 9000 --group-by process` |
| The requests, errors, and latency of each path | `edc trace http --group-by path` |
| The HTTP/1.1 and HTTP/2 requests in HTTPS | `edc trace http` |

Client and server latency measure different times. Client latency includes the network and the wait before the server reads the request.

Server latency includes only server work. If client latency is much larger, check the network and the server queue.

edc finds HTTP by the start of the data, not by the port. It can see HTTP on any port.

`source` is this host, and `destination` is the peer.

Use `--port` to keep connections that use one port on this host or its peer.

With `--side server`, it shows one local server. With `--side client`, it shows requests from this host to servers on that port.

edc checks the port in the kernel, so it does not read data from other connections.

Known secret headers stay masked in text, `--raw`, and the full screen. The full screen masks the source value in details and previews.

Use `--show-secrets` or press `m` to reveal these values. These are explicit options. They do not redact queries or bodies.

Check output before you share it. Control characters become `\xNN` text.

Secret headers include `Authorization`, `Proxy-Authorization`, `Cookie`, `Set-Cookie`, `X-Api-Key`, `X-Goog-Api-Key`, `Api-Key`, and `X-Amz-Security-Token`.

`--payload` keeps query data. It cannot be used with `--json`, because JSON writes only the summary.

`--payload=all` follows message bytes across reads, writes, and `writev` buffers. It emits metadata after the headers complete.

Text and the full screen update the same message every 250 ms when bytes change.

Final, limit, gap, close, and trace-stop updates take effect at once.

A stream stays open until its framing ends or edc stops or loses it. An unfinished body has `"payload_truncated": true`.

Raw output keeps one final event per message and its existing JSON schema.

HTTP/2 payload includes the start line, regular headers, and trailers. edc maps `:authority` to `Host`.

The payload keeps the original path and query. The structured `path` field omits the query.

HTTP/2 metadata appears when headers arrive. Body snapshots update the same row before `END_STREAM`.

The 1 MiB message cap includes headers, body, and trailers. Decoded and compressed headers have separate 64 KiB limits.

Use `--show-secrets` to reveal values in text or raw output. HTTP captures payload by default, so this option needs no `--payload`.

In the full screen, press `m` to reveal the values.

After Ctrl-C, the summary shows one row for each side, method, host, and path.

If both sides appear, it shows each side's totals and adds a `SIDE` column.

JSON adds `client` and `server` objects with the side totals.

Grouped rows show requests, responses, 4xx and 5xx responses, unanswered requests, and average and maximum latency.

HTTPS is encrypted, so edc cannot read the method, path, or status without TLS probes.

The ClientHello at each connection start is plain text. edc shows it as a `tls_hello` event.

The `target` is the server name (SNI). `alpn` lists protocols offered by the client, for example `h2` and `http/1.1`. The row shows the first protocol.

A client that sends ClientHello makes a `client:` row. A local server that receives it makes a `server:` row.

The summary has a separate `TLS connections` table. JSON adds `tls_connections` and `tls`. Use `--port 443` to keep HTTPS connections on port 443.

The server's ServerHello is plain text before it sets the keys. edc shows it as a `tls_server_hello` event with `tls_version` and `cipher`.

For example, it can show `TLS1.3 TLS_AES_128_GCM_SHA256`.

`latency_ms` starts at ClientHello on the same connection. On the client, it includes network and server time.

On a local server, it includes server time only.

A TLS 1.2 server names its chosen protocol in `alpn`. TLS 1.3 encrypts that choice. A side can refuse the handshake with an alert record.

edc shows a plain-text alert as `tls_alert`, for example `fatal handshake_failure from server` or `fatal unknown_ca from client`. `alert_from` names the sender.

The summary adds `VERSION` and `ALERTS` to the TLS table. It adds a `TLS alerts` line and JSON fields `versions`, `alerts`, and `tls_alerts`.

TLS 1.3 can encrypt alerts after the ServerHello. In OpenSSL, a client that rejects a certificate sends its alert before encryption starts.

So `curl` can show `fatal unknown_ca from client` in TLS 1.2 and TLS 1.3.

Other clients can encrypt the alert. edc then shows only a connection that ends after `tls_server_hello`.

edc checks the first 8 reads and writes in each direction after the handshake header.

edc also joins a split 5-byte record header and 2-byte alert. It cannot see a connection that started before the trace.

edc does not see a TLS connection that started before the trace.

If a client uses Encrypted Client Hello (ECH), the SNI is the provider's public name.

Bare `edc trace http` also finds TLS by default.

You can trace plain HTTP behind a TLS end point, for example a proxy that sends plain HTTP to its backend.

TLS auto discovery reads plaintext before encryption and after decryption.

It supports OpenSSL, GnuTLS, NSS, wolfSSL, Mbed TLS, rustls-ffi, supported Go TLS binaries, and registered BoringSSL builds.

It needs no certificate or key file.

TLS probes support only Linux amd64 and arm64. Unsupported architecture or ABI, missing access, and attach failures show a reason. HTTP capture continues.

Use `--tls=false` to disable TLS observation. Use `--tls=<path>` to probe only one file. An explicit `--tls` keeps its existing warning and confirmation on risky kernels.

```bash
edc trace http --tls
edc trace http --tls --side server --port 443
edc trace http --tls=/usr/local/bin/node
```

The bare command and `--tls` find these targets at trace start and during the trace:

- the `libssl`, `libgnutls`, `libssl3`, `libnspr4`, `libwolfssl`, `libmbedtls`, and `librustls` of this host in the standard library directories
- each of these libraries that a process loads, also in a container
- the program file of a process, if the file contains OpenSSL and exports `SSL_read`, for example `node`
- supported Go binaries with a `.gopclntab` section, also when they load a TLS library
- a stripped BoringSSL program with a supported GNU build ID

edc opens the file that each process loaded. This also shows a process that still uses an old `libssl` after a package update.

Opening these files can need `CAP_SYS_ADMIN` or `CAP_CHECKPOINT_RESTORE`. Probes can also need `CAP_SYS_ADMIN`. If access fails, edc shows the reason and continues HTTP capture.

edc continues the search during the trace. After a process starts, edc checks it several times for 3 seconds.

It also checks all processes every 2 seconds or more.

On a busy host, the interval grows. edc probes a new target after it finds the file. Earlier requests can be missed.

Library, ABI, or attach failures show a reason and do not stop HTTP capture.

The full screen keeps watcher and namespace notices. Other outputs write notices to standard error.

The kernel sends process-start events only in the first network namespace. Other namespaces use periodic checks and can miss early requests.

To watch only one file, use `--tls=<path>`. Then edc does not search for other files. Write the path without a space, as with `--payload=all`.

edc attaches probes to this file. If a program loads a library copy from another file, edc does not see that program.

Read `/proc/<pid>/maps` to find the file that a process loads.

NSS needs `libssl3` for TLS state and `libnspr4` for plaintext I/O.

If you specify either library, edc selects its companion from the same directory or a standard library directory.

```bash
edc trace http --tls=/usr/lib/x86_64-linux-gnu/libssl3.so
```

Start the trace before the NSS program starts. edc needs the SSL setup calls to distinguish TLS from plain files and sockets.

NSS uses `PR_Read`, `PR_Recv`, `PR_Write`, and `PR_Send` for plaintext. edc follows `SSL_SECURITY`, its default value, model copies, accepted connections, and `PR_Close`.

NSPR also uses these functions for plain files and sockets. So each NSPR read and write runs a probe, also in a program without TLS.

An NSS connection with `SSL_SECURITY` off produces no TLS event. `PR_Recv` with `PR_MSG_PEEK` also produces no TLS event.

Go TLS uses the Go function table and probes at function returns.

edc supports Go 1.21.5, 1.23.7, 1.23.8, 1.23.12, 1.25.0, 1.26.5, 1.26.8, 1.27.1, and 1.27.3.

Live capture tests pass on Linux amd64 for these versions. Metadata tests cover Linux amd64 and arm64. Native arm64 capture remains unverified.

A valid modern Go function table with no TLS Read, Write, or Close method produces no TLS target. A partial or corrupt table produces a notice in automatic mode. The `--tls` option reports an error when a Go binary has no TLS methods. edc rejects legacy Go function tables with an unsupported ABI error.

```bash
edc trace http --tls=my-go-program
```

The bare command discovers supported Go binaries by their Go function table.

You can also specify a binary path or command name. edc supports normal, stripped, and PIE binaries.

These events have no socket addresses. A `--port` filter excludes them. Other Go versions have no Go TLS support.

For Go clients, latency starts at `Write` entry and ends at `Read` return. For servers, latency ends at `Write` entry.

The capture tests cover Go HTTP/1.1 and HTTP/2, including concurrent streams, repeated headers and fragmented bodies.

rustls-ffi uses the C functions `rustls_connection_read` and `rustls_connection_write`. edc clears the connection state at `rustls_connection_free`.

```bash
edc trace http --tls=/usr/local/lib/librustls.so
```

These events have no socket addresses. A `--port` filter excludes them. `--tls` does not see programs that use the native Rust API of rustls.

Mbed TLS uses `mbedtls_ssl_read`, `mbedtls_ssl_write`, `mbedtls_ssl_read_early_data`, and `mbedtls_ssl_write_early_data`.

edc clears the connection state at `mbedtls_ssl_session_reset` and `mbedtls_ssl_free`.

On a server, `mbedtls_ssl_read_early_data` returns data that the handshake already read. Its events have no socket address, so `--port` excludes them.

```bash
edc trace http --tls=/usr/local/lib/libmbedtls.so
```

edc does not read DTLS connections.

wolfSSL uses `wolfSSL_read` and `wolfSSL_write`, or their `_ex` variants. edc clears the connection state at `wolfSSL_free`.

```bash
edc trace http --tls=/usr/local/lib/libwolfssl.so
```

Use `--tls=claude` to find an executable in PATH. A file in the current directory takes priority over PATH.

edc supports the stripped amd64 Bun 1.4.3 runtime in Claude Code 2.1.291. Its GNU build ID is `ca2032b38650b44e05b2074617d524c7475c80f0`.

edc checks the build ID and the function code before it attaches probes. Other stripped BoringSSL builds need a separate entry with checked offsets.

```bash
edc trace http --tls=claude
```

With `sudo`, `PATH` often does not include `~/.local/bin`. If edc does not find the program, give the path from your shell:

```bash
sudo edc trace http --tls="$(command -v claude)"
```

An HTTPS request or response from a supported TLS library has `"tls": true`. The event row shows `tls` after the event name.

The destination starts with `https://`. A plain HTTP destination starts with `http://`. Path, status, latency, grouped views, and payload work as with plain HTTP.

`--payload` masks the same header values. HTTPS bodies can contain tokens. Check output before you share it.

edc reads HTTP/2 over TLS as it reads h2c. It shows the method, path, status, and latency of each stream.

edc reads each connection's plaintext. Busy HTTP/2 traffic can cause lost events. Use `--port` to reduce this cost.

If edc loses bytes, it stops reading that direction. It does not read a connection that started before the trace.

HTTP/2 metadata appears when headers arrive. Each snapshot updates the same message before `END_STREAM`.

Long streams stay open until `END_STREAM`, reset, connection close, limit, or trace stop.

Payload includes a start line, regular headers, body, and trailers. edc maps `:authority` to `Host`.

The 1 MiB message limit includes headers, body, and trailers. Decoded and compressed headers have separate 64 KiB limits.

HTTP/1 and HTTP/2 share limits of 4096 open messages and 64 MiB. `--payload` keeps up to 4 KiB. `--payload=all` keeps up to 1 MiB.

Truncated and evicted messages carry `"payload_truncated": true`.

Some programs do not use the socket inside the TLS call, for example Bun, `node`, and Python `asyncio`. edc then cannot identify the connection.

The event has the process but no `source` or `destination`. The `target` is the `Host` header.

The summary labels these events `TLS plaintext without an address`. JSON adds `tls_unmapped`.

With `--port`, edc cannot check this plaintext's port. It hides these events and adds them to the same count.

`--tls` sees OpenSSL calls to `SSL_read`, `SSL_write`, `SSL_read_ex`, and `SSL_write_ex`.

It sees GnuTLS calls to `gnutls_record_recv` and `gnutls_record_send`, for example in Debian and Ubuntu `wget` and `git`.

NSS, wolfSSL, Mbed TLS, and rustls-ffi programs also work, as described above.

`--tls` does not see Java programs. Go support has the limits above.

edc also misses programs without the exported functions of these libraries, except supported Go binaries and the supported BoringSSL build.

With `--tls=<path>`, edc also reads the symbol table, so a static program with symbols can work.

`--tls` does not see a program that reads through the SSL BIO of OpenSSL, for example `openssl s_server -www`.

edc reads plaintext without a page fault. If the kernel changes the page at that moment, edc cannot read that call's plaintext.

For example, the kernel can split or merge a transparent huge page. This is rare, and edc counts it as a lost event.

Each call runs a probe in each process that uses these files, including processes that `--process` hides.

At the end, the kernel removes each probe. The trace can stop a few seconds after Ctrl-C. `--tls` needs no newer kernel than `trace http`.

On amd64, seccomp can stop a process when a TLS call returns. This affects Linux 6.11, 6.12 before 6.12.14, and 6.13 before 6.13.3.

Bare-command auto discovery skips TLS probes on these kernels. It shows the reason and continues HTTP capture.

Explicit `--tls` keeps its warning and full-screen confirmation. A distribution kernel can include the fix.

Without TLS probes, edc sees HTTPS as encrypted data. It still parses HTTP/2 without TLS (h2c), for example gRPC inside a cluster.

h2c rows show the method, path, status, and latency of each stream.

edc reads all bytes of an h2c connection to follow its frames and header tables.

Busy h2c traffic can cause lost events. Use `--port` to reduce this cost.

If edc loses h2c bytes, it stops reading that direction and counts lost events.

edc does not parse HTTP/3 requests. HTTP/3 uses binary frames over UDP, so it has no `tls_hello` event.

edc finds messages only at read or write start.

If one read has the end of a response and the start of the next, edc misses the next response.

edc reads only the first buffer of a multi-buffer write.

Keep `Host` in the first 512 bytes of that buffer. Otherwise, the target is the server address.

edc supports this field on Linux 5.15 or later.

## trace mysql

Use `trace mysql` on Linux 5.15 or later to print plain MySQL commands and results as they arrive. edc reads the start of each TCP read and write on the MySQL port in the kernel. The default port is 3306. Use `--port` for another port.

```bash
edc trace mysql
edc trace mysql --side server
edc trace mysql --port 3307
edc trace mysql --slow 250ms
edc trace mysql --group-by process
edc trace mysql --show-secrets
edc trace mysql --raw
```

`trace mysql` shows two sides, like `trace http`. Each event row starts with the side:

- `client:` is a command that this host sent to a MySQL server.
- `server:` is a command that a local MySQL server received.

JSON events have `"side": "client"` or `"side": "server"`. Use `--side client` or `--side server` to keep one side. If the local port is the MySQL port, the socket is on the server side. If the peer port is the MySQL port, the socket is on the client side.

Use `--slow <duration>` only with `trace mysql`, `trace clickhouse`, `trace postgres`, `trace redis`, `trace io`, `trace sched`, `trace syscall`, or `trace reclaim`. It accepts Go duration syntax, such as `250ms` and `1.5s`. For `trace io`, it sets the minimum request latency. For `trace sched`, it sets the minimum span latency. The default for both is 1ms. For `trace syscall`, it prints each call that takes at least the duration, and it has no default. For `trace mysql`, in the ungrouped interactive view, it keeps a command row only when its first paired response has latency at or above the duration. It excludes unanswered commands, unmatched responses, responses without latency, and TLS rows. It does not change raw JSON, noninteractive output, summaries, grouped views, `--group-by`, or `--side`.

| To see | Command |
| --- | --- |
| All MySQL on this host | `edc trace mysql` |
| The commands that local servers received | `edc trace mysql --side server` |
| The commands that this host sent | `edc trace mysql --side client` |
| MySQL on port 3307 | `edc trace mysql --port 3307` |
| Paired commands at or above 250ms | `edc trace mysql --slow 250ms` |
| The latency of each process on each side | `edc trace mysql --group-by process` |
| The SQL text without the mask | `edc trace mysql --show-secrets` |

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

## trace clickhouse

Use `trace clickhouse` on Linux 5.15 or later to print plain ClickHouse queries as they arrive. One trace reads the native TCP protocol and the HTTP interface. edc reads the start of each TCP read and write on the two ports in the kernel. The default ports are 9000 for native TCP and 8123 for HTTP.

Use `--port` for another native port. Use `--http-port` for another HTTP port. `--http-port` works only with `trace clickhouse`, and its value must be from 1 to 65535. No option reads only one interface.

The two ports must differ. If they are the same, edc exits with code 2. This includes the default of the other option. For example, `--port 8123` fails, because the HTTP port is 8123 by default.

```bash
edc trace clickhouse
edc trace clickhouse --side server
edc trace clickhouse --port 9001
edc trace clickhouse --port 9440 --http-port 18123
edc trace clickhouse --slow 500ms
edc trace clickhouse --group-by process
edc trace clickhouse --show-secrets
edc trace clickhouse --raw
```

`trace clickhouse` shows two sides, like `trace mysql`. Each event row starts with the side:

- `client:` is a query that this host sent to a ClickHouse server.
- `server:` is a query that a local ClickHouse server received.

JSON events have `"side": "client"` or `"side": "server"`. Use `--side client` or `--side server` to keep one side.

Use `--slow <duration>` in the ungrouped interactive view. It accepts Go duration syntax, such as `250ms` and `1.5s`. It keeps a query row only when the latency of its paired response is at or above the duration. It excludes unanswered queries, unmatched responses, and responses without latency. If you use `--slow` with `--raw`, `--json`, `--group-by`, or output that is not a terminal, edc exits with code 2.

| To see | Command |
| --- | --- |
| All ClickHouse on this host | `edc trace clickhouse` |
| The queries that local servers received | `edc trace clickhouse --side server` |
| The queries that this host sent | `edc trace clickhouse --side client` |
| ClickHouse with native port 9001 | `edc trace clickhouse --port 9001` |
| ClickHouse with HTTP port 18123 | `edc trace clickhouse --http-port 18123` |
| Paired queries at or above 500ms | `edc trace clickhouse --slow 500ms` |
| The latency of each process on each side | `edc trace clickhouse --group-by process` |
| The SQL text without the mask | `edc trace clickhouse --show-secrets` |

A request is one of these events. The `clickhouse` object of the JSON event has the fields.

- `clickhouse_connect` is a login. It has `user`, `database`, `client_name`, `client_version`, and `revision`.
- `clickhouse_query` is a query. It has `sql`, `query_id`, `user`, `database`, and `compressed`.

A response is one of these events:

- `clickhouse_hello` is the answer to a login. It has `server_name` and `server_version`.
- `clickhouse_end` is the end of a query. It has `rows` when edc read the data blocks.
- `clickhouse_exception` is a failed query. It has `error_code`, `error_name`, and `message`.

A response event has the `command`, the `sql`, and the `query_id` of the request that it answers. If edc did not see the end of the stream but can tell that the query ended, `clickhouse_end` has `end_inferred: true`. `clickhouse_chunked` is not a request. It shows that the connection uses the chunked protocol. edc reads no more of that connection.

The latency of a native query starts at the `Query` packet and stops at `EndOfStream` or `Exception`. This differs from `trace mysql`, which stops at the first response packet. So `latency_ms` is the time to the end of the result. On the server side, it starts when the server reads the query and stops when the server writes the last packet. On the client side, it stops when the client reads the last packet. The client latency includes the network.

HTTP queries use the same events and fields. Each `clickhouse` object has `interface`, which is `native` or `http`. A response event of an HTTP query also has `http_status`. In the scroll view, an HTTP row has `http:` after the side, for example `server: http: SELECT 1`. The detail view of the full screen shows `interface`, and `http_status` for a response. For HTTP, `compressed` means that the request body is compressed. HTTP has no `clickhouse_connect` or `clickhouse_hello` event, and `rows` is not set.

edc shows an HTTP request as a query in these cases:

- A `GET` or `HEAD` request whose path starts with `?`, `/?`, or `/query?`.
- A `POST` or `OPTIONS` request to `/`, or to one of those paths.

Other endpoints, such as `/ping`, `/replicas_status`, and `/play`, have no event. A custom `http_handlers` path is not a query, so it has no event either.

The SQL is the `query` parameter of the URL, a newline, and the start of the request body. edc keeps the first 4 KiB of the body. edc does not read a multipart body. edc does not read a compressed body (`Content-Encoding` or `decompress=1`) either, so the SQL can be only the `query` parameter. edc cuts an `INSERT` before its data, at `VALUES` or `FORMAT <name>`. The insert data is never shown.

edc pairs the requests and responses of one connection in order. It skips the request body by its size (`Content-Length` or chunked), so it finds the next request on a keep-alive connection. It also handles `Expect: 100-continue`.

The latency of an HTTP query starts at the first record of the request. It stops at the end of the response: the last chunk, the `Content-Length` bytes, or an `__exception__` block. edc does not use `elapsed_ns` of the `X-ClickHouse-Summary` header. The server writes that header before it sends all of the result. So the value can be much smaller than the time to the last chunk. On the server side, the latency is the time that the server takes to run the query. On the client side, it includes the network.

By default, `wait_for_async_insert` is 1. For an async insert, the server answers after the flush of the buffer. So the latency includes the wait for the flush (`async_insert_busy_timeout_ms`). If `wait_for_async_insert` is 0, the latency stops when the server accepts the insert.

If a query fails before the server sends output, the HTTP status shows the error. edc emits `clickhouse_exception` with `error_code` from `X-ClickHouse-Exception-Code`. If a query fails after the output starts, the status stays 200 and the body ends with an `__exception__` block. edc reads this block and emits `clickhouse_exception`. If the response is compressed, edc can fail to see the block. Then the query counts as unanswered. `error_name` is empty if edc cannot read it.

By default, edc replaces the text of string literals in the SQL with `?`. The mask applies to the scroll view, the full screen, `--raw`, and the summary. Numbers, names, and comments are not hidden. Do not write a secret in a comment or a number.

Use `--show-secrets` to show the SQL as sent. It needs no `--payload`. Do not share output that has it. The output never has the password of a login, also with `--show-secrets`. edc does not keep the values of settings and query parameters.

For HTTP, edc never shows these values, also with `--show-secrets`: the password, `X-ClickHouse-Key`, the password of `Authorization: Basic`, other `Authorization` schemes, `X-ClickHouse-Quota`, `quota_key`, `session_id`, cookies, `param_*` values, and setting values. edc shows `user`, `database`, and `query_id`.

edc does not hide the `message` of a `clickhouse_exception` event. The message can hold values, for example a table name or a key value. Check the output for values before you share it.

The summary after Ctrl-C shows one row for each side, command, and SQL shape. The shape replaces the string values and the numbers with `?` and joins the spaces to one space. A row shows the number of queries, errors, and unanswered queries, the average and maximum latency, and the process. If the trace has both sides, the summary shows the totals of each side and adds a `SIDE` column. JSON has the same content. With `--group-by`, the rows show `CMD`, `RSP`, `ERR`, `NOANS`, `AVGms`, and `MAXms`. The grouped views keep the server side in separate rows. If the trace has both native and HTTP queries, the summary adds an `INTERFACE` column after `SIDE`. JSON rows always have `interface`. The `Connections` total counts native connections only.

`trace clickhouse` has these limits:

- It reads the native TCP protocol and HTTP/1.x. It does not read TLS (native port 9440, HTTPS port 8443) or HTTP/2, and shows no event for them. It does not read a chunked native connection.
- An HTTP/1.0 response and a response with no length have an end that edc cannot see. These queries count as unanswered.
- If a client sends two HTTP requests in one write, the server answers only the first. The pairs of that connection can be wrong.
- It reads TCP only. A connection to a unix socket is not visible.
- edc keeps the first 4 KiB of each read or write, for requests and for responses. An event shows up to 1,024 bytes of SQL, and `sql_truncated` is true if edc cuts it.
- On the client side, the last read of a large result can start in the middle of a data block or an HTTP chunk. This occurs with and without compression. Then edc cannot see the end of the query, and the query counts as unanswered. The server side still sees the end.
- If a heavy query makes the last write more than 4 KiB, edc cannot see the end. The uncompressed `ProfileEvents` block grows with the number of threads and counters.
- If edc did not see the `Hello` of a connection, edc skips that connection. This happens when the trace starts in the middle of a connection. edc sees the connection after the client connects again.
- If a lost event or a partial send moves the packet boundary, edc can miss the next queries of that connection. The summary shows the lost events.
- At a published host port, edc shows the client row and the row of the server in the container. `docker-proxy` itself shows no rows. The server row shows the Docker bridge gateway as the peer. Use `--process clickhouse-serv` to keep the rows of the server process.

## trace postgres

Use `trace postgres` on Linux 5.15 or later to print plain PostgreSQL requests and results as they arrive. edc reads the start of each TCP read and write on the PostgreSQL port in the kernel. The default port is 5432. Use `--port` for another port.

```bash
edc trace postgres
edc trace postgres --side server
edc trace postgres --port 6432
edc trace postgres --slow 250ms
edc trace postgres --group-by process
edc trace postgres --show-secrets
edc trace postgres --raw
```

`trace postgres` shows two sides, like `trace mysql`. Each event row starts with `client:` or `server:`. JSON events have `"side": "client"` or `"side": "server"`. Use `--side client` or `--side server` to keep one side. `--slow` works as in `trace mysql`.

| To see | Command |
| --- | --- |
| All PostgreSQL on this host | `edc trace postgres` |
| The requests that local servers received | `edc trace postgres --side server` |
| PostgreSQL on port 6432 | `edc trace postgres --port 6432` |
| Paired requests at or above 250ms | `edc trace postgres --slow 250ms` |
| The SQL text without the mask | `edc trace postgres --show-secrets` |

A request is one of these events. The `postgres` object of the JSON event has the fields.

- `postgres_connect` is a login. It has `user` and `database`.
- `postgres_query` is one request unit. Its `command` is `query`, `prepare`, `execute`, or `function`.
- A `query` is one simple Query message. It has `sql`.
- A `prepare` or an `execute` is an extended batch that ends with Sync. It has `sql`, `statement`, and `executes`.

A response is one of these events:

- `postgres_result` has the `tag` of CommandComplete, for example `SELECT 3`, and `rows`. The answer to a login has `server_version`.
- `postgres_error` has `severity`, `sql_state`, and `message`.

A response event has the `command` and the `sql` of the request that it answers. `postgres_tls` is not a request. It shows that the connection uses TLS or GSSAPI encryption. edc reads no more of that connection.

The latency starts at the first message of the request unit and stops at ReadyForQuery. On the client side, the latency includes the network. On the server side, it includes the work of the server and the write of the result.

By default, edc replaces string constants and dollar-quoted text in the SQL with `?`. Use `--show-secrets` to show the SQL as sent and the `detail` of an error. edc never shows the password, the SASL data, the Bind parameter values, the COPY data, or the arguments of a function call. edc does not hide the `message` of an error. Check it for values before you share it.

The summary after Ctrl-C shows one row for each side, command, and SQL shape. With `--group-by`, the rows show `CMD`, `RSP`, `ERR`, `NOANS`, `AVGms`, and `MAXms`.

`trace postgres` has these limits:

- It does not read TLS or GSSAPI encryption. Each encrypted connection shows one `postgres_tls` row.
- It reads TCP only. A unix socket connection is not visible. `psql` without `-h` uses a unix socket. Use `psql -h 127.0.0.1` in a test.
- edc keeps the first 4 KiB of each response write or read. The server writes a large result in parts of 8 KiB, so edc loses the message boundary.
- For a large result, edc also keeps the last 256 bytes of each response write or read. If they end with a whole CommandComplete or ErrorResponse and then ReadyForQuery, and one request waits, edc closes that request at that time. The tag or the error fields come from those bytes. `rows` comes from a `SELECT` or `FETCH` tag.
- edc does not accept the six bytes of ReadyForQuery alone, because a data row can end with the same bytes. If an ErrorResponse is longer than the last 256 bytes, the request counts as unanswered.
- If two or more requests wait when edc loses the boundary, edc does not guess. These requests count as unanswered. The next request gets its pair again.
- If the end of a read is not in its first buffer, edc keeps no tail of that read. The tail of a write comes only from its last buffer.
- If the trace starts in the middle of a connection, there is no `postgres_connect` for it. edc finds the boundary at a later request.
- An event shows up to 1,024 bytes of SQL. `sql_truncated` is true if edc cuts it.

## trace redis

Use `trace redis` on Linux 5.15 or later to print plain Redis commands and replies as they arrive. edc reads the start of each TCP read and write on the Redis port in the kernel. The default port is 6379. Use `--port` for another port.

```bash
edc trace redis
edc trace redis --side server
edc trace redis --port 6380
edc trace redis --slow 5ms
edc trace redis --group-by process
edc trace redis --show-secrets
edc trace redis --raw
```

`trace redis` shows two sides, like `trace mysql`. Each event row starts with `client:` or `server:`. `--slow` works as in `trace mysql`.

The `redis` object of the JSON event has the fields of these events:

- `redis_command` is a command. It has `command`, `key`, and `args`. A pipeline shows one event for each command.
- `redis_reply` answers the oldest waiting command. It has `reply`, the type of the reply, and `elements` or `bulk_length`.
- `redis_error` is an error reply. It has `error_class`, for example `WRONGTYPE`.
- `redis_push` is a pub/sub message or a RESP3 push. It answers no command.

edc pairs the commands and replies of one connection in order. A command inside MULTI gets the reply `queued`, and the reply to EXEC is the array of the results. The latency starts when the command arrives and stops when its reply arrives. In a pipeline, the commands of one write share the start, and the replies of one write share the end.

By default, edc does not show argument values, reply values, or error messages. Use `--show-secrets` to show short previews of them. edc shows key names in both cases. edc never shows the values of AUTH, HELLO, MIGRATE, CONFIG GET, CONFIG SET, ACL, or SENTINEL, also with `--show-secrets`.

The summary after Ctrl-C shows one row for each side and command name. A row shows the number of commands, errors, and unanswered commands, and the average and maximum latency.

`trace redis` has these limits:

- It does not read TLS. edc finds no RESP framing in a TLS connection, so it shows no events for it.
- It reads TCP only. A unix socket connection is not visible.
- edc keeps the first 4 KiB of each request and the first 1 KiB of each reply write or read. A very large array reply, for example `LRANGE` of 5,000 items, loses the framing. That command counts as unanswered, and the next command re-syncs.
- edc skips bulk values by their size, so a 10 KB `SET` still gets its reply. If the argument headers of a command are outside the capture, for example `RPUSH` with 5,000 values, the command shows `truncated: true` and gets no reply.
- After `CLIENT REPLY OFF` or `CLIENT REPLY SKIP`, the server does not reply. These commands have `no_reply: true`, and edc does not count them as unanswered.
- After PSYNC or SYNC, edc does not read the replication stream as replies. A replica sends `REPLCONF ACK` to its master each second. On the master, `--side server` shows one `REPLCONF ACK` with `no_reply: true` for each replica each second.
- If the trace starts in the middle of a connection, edc finds the framing at a later command.

## trace socket

Use `trace socket` on Linux to follow one unix domain socket file. Give the path of the socket file. edc prints each connect, accept, send, recv, end of data (`eof`), and close on that socket. The options can come before or after the path.

```bash
edc trace socket /run/docker.sock
edc trace socket /run/docker.sock --payload
edc trace socket /run/php/php-fpm.sock --payload=all --raw
```

The destination is the socket path. The event shows the call and its byte count. If a call fails, the event shows the errno name, for example `connect ECONNREFUSED`. The source is the peer process. On the server side, the peer is the process that connected. On the client side, the peer is the process that accepted the connection, or the last server process that sent or received data on it. Before the accept, the peer is the process that called `listen()`, for example `systemd` for a socket that systemd opens.

The server side shows an `accept` event. It shows how long the connection waited in the backlog, for example `accept 120µs`. A long wait means that the server accepts connections too slowly, for example because all workers are busy. edc does not see an `accept` call that started before the trace, but the next server call on that connection still shows the correct peer.

Use `--payload` to see the first 4 KiB of the data of each send and recv. Use `--payload=all` to see up to 1 MiB of each call. edc reads the data of one call in 16 KiB parts and joins them into one event. A stream socket has no message boundaries, so one event is one call, not one message. edc does not know the format of the payload, so it hides nothing. `--show-secrets` is not available. The payload can contain tokens and passwords, for example the `X-Registry-Auth` header on `docker.sock`. Before you share the output, check it for tokens and passwords. In the full screen, edc collects the first 4 KiB of each call also without `--payload`. Press `v` to show the payload lines. Press Enter to see the whole payload.

If the server makes the socket file again, for example after a restart, edc finds the new file within one second. Connections on the old file stay in the trace. `trace socket` supports stream sockets only. Datagram and seqpacket sockets, abstract sockets, and socket pairs are not available. edc does not see the data of `sendfile` and `splice`. A failed connect is in the trace only if the program uses the same path as the command.

## trace drop

Use `trace drop` on Linux to see why the kernel drops packets. For each drop, edc shows the reason, the kernel function, the addresses and ports, and the size. If the packet belongs to a local socket, edc also shows the process.

```bash
edc trace drop
edc trace drop --reason NO_SOCKET,SOCKET_RCVBUFF
edc trace drop --container web --raw
```

The reason is the name from the kernel, for example `NO_SOCKET` (no socket uses the port), `SOCKET_RCVBUFF` (the receive buffer of the socket is full), or `NETFILTER_DROP` (a firewall rule dropped the packet). The reasons need Linux 5.17 or later. On an earlier kernel, the reason is `unknown`, and only the function shows. Use `--reason` with names separated by commas to see only some reasons. The names are not case-sensitive.

The kernel also frees packets as part of normal work, for example when a program closes a socket with unread data (`QUEUE_PURGE` and `TCP_ABORT_ON_DATA`). edc shows these too, because they tell you that a program did not read the data.

The totals per reason in the summary are exact. If one CPU drops more than 1,000 packets in one second, edc sends only 1,000 events for that second and counts the rest. The summary shows this number as `Sampled out`. A process shows only for a packet that has a socket. A packet to a closed port has no socket.

The kernel does not report every drop with a reason. For example, if the accept queue of a listening socket is full, the kernel frees the SYN as a normal packet. For that case, the summary shows how much `ListenOverflows` and `ListenDrops` in `/proc/net/netstat` increased during the trace. These counters cover all listening sockets in the network namespace of edc, also with `--container`.

## PCAP file

```bash
edc capture \
  --interface en0 \
  --duration 15s \
  --count 500 \
  --filter 'host 203.0.113.10 and port 443' \
  --output incident.pcap
```

A PCAP file can hold credentials and personal data. The JSON redaction does not apply to the PCAP payload.

`edc capture` shows the plan before it starts. The plan has the interface, the duration, the packet limit, the filter, the output path, and the privilege that `edc` uses. Answer the question with the left and right arrow keys and Enter, or with `y` or `n`. The plan stays on the screen above the `tcpdump` output.

Use `--yes` to skip the question. A non-terminal command prints the plan and reads `y` or `n` from stdin.

