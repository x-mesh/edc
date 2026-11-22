# Browse listening sockets

`edc listen` opens the socket browser when stdin and stdout are interactive and `TERM` is not `dumb`. In pipes it prints the current sockets once and exits. Use `-B` (`--no-browse`) for one-shot output in a terminal, or `-b` (`--browse`) to require the browser. The browser refreshes only when you press `r`.

```sh
edc listen
edc listen -B
edc listen -b
edc listen -b --tcp
edc listen -b --all
edc listen -b --full
edc listen -b --redact
```

The browser requires interactive stdin and stdout, UTF-8, a terminal other than `TERM=dumb`, and at least 48 columns by 8 rows. Resizing below this size displays a size notice; `q` and Ctrl-C still exit. With `NO_COLOR`, the browser retains keyboard navigation and uses a plain selection marker without colors. For screen-reader use, run `edc listen -B` or `edc listen --json -`.

| Key | Socket list | Socket details |
| --- | --- | --- |
| Up/Down, k/j | Select a socket | Scroll |
| PgUp/PgDn | Move one page | Scroll one page |
| Home/End, g/G | First/last socket | Beginning/end |
| Enter | Read socket and process details | — |
| r | Refresh the socket list | Reload selected socket's process details |
| Esc, Backspace | — | Return to the same selection and viewport |
| q, Ctrl-C | Exit | Exit |

Refresh preserves the selected socket by protocol, address, port, process name, and PID. If it disappears, selection moves to the first remaining socket. A failed refresh retains the last successful snapshot and shows an error. It does not imply that the old snapshot is current.

Enter shows the address, owner PID, receive/send queue, container association when available, and the same process facts as `edc proc <pid> -v`. An unknown owner or an exited process leaves the socket details accessible with an explanation. Process details are read on demand and remain a snapshot until reloaded. Esc returns to the socket list without automatically refreshing it.

`--full` adds the selected process's command and working directory to the list footer. The details screen wraps long content and can be scrolled. Terminal display of Arabic and other bidirectional text depends on the terminal emulator.

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
