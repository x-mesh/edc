# Browse listening sockets

`edc listen` opens the socket browser when stdin and stdout are interactive and `TERM` is not `dumb`. In pipes it prints the current sockets once and exits. Use `-B` (`--no-browse`) for one-shot output in a terminal, or `-b` (`--browse`) to require the browser. The browser refreshes when you press `r`, and one second after `S` sends a signal.

```sh
edc listen
edc listen -B
edc listen -b
edc listen -b --tcp
edc listen -b --all
edc listen -b --full
edc listen -b --redact
```

The browser requires interactive stdin and stdout, UTF-8, a terminal other than `TERM=dumb`, and at least 48 columns by 8 rows. Resizing below this size displays a size notice; `q` and Ctrl-C still exit. With `NO_COLOR`, the browser retains keyboard navigation and uses a plain selection marker without colors. The list then has a SCOPE column after ADDRESS. The column shows the binding scope as `all`, `specific`, or `loopback`. For screen-reader use, run `edc listen -B` or `edc listen --json -`.

| Key | Socket list | Socket details |
| --- | --- | --- |
| Up/Down, k/j | Select a socket | Scroll |
| PgUp/PgDn | Move one page | Scroll one page |
| Home/End, g/G | First/last socket | Beginning/end |
| Enter | Read socket and process details | — |
| t | Open `edc top --interactive --process <pid>` | Open the selected process in top |
| x | Show a CPU trace command | Show a CPU trace command |
| S | Ask for confirmation, then send SIGTERM to the selected PID | Ask for confirmation, then send SIGTERM to the selected PID |
| l | Hide or show loopback rows | — |
| p | Select the next PID of a row with two or more PIDs | Select the next PID |
| r | Refresh the socket list | Reload selected socket's process details |
| Esc, Backspace | — | Return to the same selection and viewport |
| q, Ctrl-C | Exit | Exit |

The list puts sockets that differ only by address or PID in one row, as `edc listen -B` does. The header shows the number of sockets and the number of rows. The ADDRESS column shows the addresses of the row. In color, the address is yellow for all addresses, cyan for a specific address, and gray for loopback. The notice line starts with this legend. The port is bold, the PID is gray, and the container after `→` is cyan. The ADDRESS column uses at most half of the free width. A longer address list is cut, and the footer shows all addresses. If a row has two or more PIDs, the PID column shows the selected PID and the number of other PIDs, for example `1682348 +1`. At the start, the browser selects the first PID. `p` selects the next PID, and `t` and `x` use the selected PID. The footer shows the selected PID, its position, and the `p` key, for example `PID 1682348 (1/2) · p next PID`. The details screen shows the same text below the key line. If a refresh removes the selected process, the browser selects the first PID of the row again. The footer shows all addresses of the selected row.

Refresh preserves the selected row by protocol, port, process name, binding scope, and container. If it disappears, selection moves to the first remaining row. `l` hides the loopback rows until you press `l` again, also after a refresh. If the selected row is a loopback row, the selection stays at the same position. A failed refresh retains the last successful snapshot and shows an error. It does not imply that the old snapshot is current.

Enter shows the addresses, owner PIDs, receive/send queue of each socket, container association when available, and the same process facts as `edc proc <pid> -v` for each PID. An unknown owner or an exited process leaves the socket details accessible with an explanation. Process details are read on demand and remain a snapshot until reloaded. Esc returns to the socket list without automatically refreshing it.

For node, python, ruby, perl, and php, the PROCESS column adds the script file name and the name of the working directory in gray, for example `node · vite (web)`. If the process replaced its title, for example `Raycast Backend`, the column shows that title. The column does not show other arguments, because they can contain secrets. Use `--full` to see the full command.

`--full` adds the selected process's command and working directory to the list footer. For a row with two or more PIDs, they are those of the PID that `p` selected. The details screen wraps long content and can be scrolled. Terminal display of Arabic and other bidirectional text depends on the terminal emulator.

`--browse` cannot be combined with `--watch`, `--json`, `--interval`/`-i`, or `--duration`. Use `edc listen --watch` for a continuous stream of socket changes. Browse does not change the existing TCP/UDP/Unix filters or the one-shot/JSON output modes. Binding scope describes where a socket is bound; it does not establish external reachability.

## Choose the default

Run `edc setup` and select the listen section, or set a single value:

```sh
edc setup listen.browse auto
edc setup listen.browse always
edc setup listen.browse never
edc setup listen.browse --clear
```

The default is `auto`. `always` requires an interactive terminal and reports an error in a pipe or `TERM=dumb`; `never` prints one snapshot. Clearing the setting restores `auto`.

In the TOML configuration:

```toml
[defaults.listen]
browse = "auto"
```

Legacy YAML configuration is also supported:

```yaml
defaults:
  listen:
    browse: auto
```

`-b` and `-B` override the stored preference and cannot be combined. `--browse=false` also forces one-shot output. `--watch` and JSON output override configured browsing, including `always`; explicitly combining `-b` with either is an error. `NO_COLOR` changes only the colors, not the choice between TUI and one-shot output. `--interval`/`--duration` are not browser refresh settings.

Process navigation uses the current edc executable and passes the PID as a separate argument. Leaving top returns to the same socket selection and detail offset. The list remains a snapshot until manually refreshed. The trace key shows a command without running it or elevating privileges. Navigation is disabled with `--redact` so a child command cannot silently reveal unredacted data.

`top --interactive` explicitly requests a terminal dashboard even with `NO_COLOR`; it preserves monochrome rendering and rejects JSON, counted batch output, pipes, and TERM=dumb. Normal top defaults are unchanged.

The process-navigation child explicitly clears saved top count and JSON defaults so the selected process always opens as a dashboard rather than a batch export.

## Stop a process

`S` shows a confirmation box in the middle of the screen. While the box is open, the rest of the screen is gray. The box names the process, the PID, and up to three ports that this PID holds. If the PID holds more ports, `+N` gives the number of other ports. The default answer is no. Press `y` to send SIGTERM. You can also select yes with Left, Right, or Tab and then press Enter. Any other key cancels, so a navigation key cannot send the signal. If the window becomes smaller than 48 columns by 8 rows, edc cancels the confirmation.

In a row with two or more PIDs, `S` stops only the PID that `p` selected. The confirmation box tells you that the row has other PIDs. If you stop a worker process, its parent process can start a new one.

Before edc shows the confirmation box, it reads the process name and start time again. If the name is different, or the process started after edc began to read the list, edc does not send the signal. Press `r` to read the list again. On Linux, edc keeps the `/proc/<pid>` directory open and sends the signal through it with `pidfd_send_signal`. Thus the signal cannot reach a different process that gets the same PID later. On Linux before 5.1 and on macOS, edc examines the process again just before `kill`. A small gap remains between that examination and the signal.

After the signal, edc reads the socket list again after one second. The notice line tells you if the sockets of the PID are gone. If the sockets remain, the process did not stop yet, or it ignores SIGTERM. edc does not send SIGKILL.

edc does not stop PID 1 or itself. `S` is not available with `--redact`. For a port that a container publishes, the socket owner is usually `docker-proxy`. If you stop it, the container continues to run and only the port mapping stops. Use `docker stop` for these rows. To stop a process of a different user, run edc with sudo.
