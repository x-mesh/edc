**English** | [한국어](README.ko.md)

# edc

`edc` is short for **everyday carry**. An everyday carry is the small kit you keep in a pocket and reach for first. `edc` is that kit for SE and SRE work in a terminal.

An incident starts with one question: is the fault here, in the network, or at the far end? `edc` answers it with one command. It runs DNS, TCP, TLS, HTTP, route, ping, interface, and socket probes in one pass, and every probe prints the same result format. It also reports host resources and host information on Linux and macOS, and it runs macOS `networkQuality`.

Every command is read-only. `edc` finds the fault and stops there. It runs no DNS flush, no interface reset, and no firewall change, so it stays safe on a production host.

![edc doctor https://example.com runs nine probes in order and prints a 9 pass summary](docs/media/doctor.gif)

The whole run takes about three seconds. Each line keeps the probe name, the target, and the result in the same columns, so you read down one column to find the failure.

The source of each demo is a `.tape` file under [`docs/tape/`](docs/tape). To build one again, run `vhs docs/tape/doctor.tape`.

## Install

Install the latest release with the script. It reads the operating system and the architecture, checks the SHA-256, and installs the binary.

```bash
curl -fsSL https://raw.githubusercontent.com/x-mesh/edc/main/install.sh | sh
```

The script installs `edc` in `~/.local/bin`. Set `BINDIR` for another directory. Set `EDC_VERSION` for an earlier version.

```bash
curl -fsSL https://raw.githubusercontent.com/x-mesh/edc/main/install.sh | BINDIR=/usr/local/bin sh
curl -fsSL https://raw.githubusercontent.com/x-mesh/edc/main/install.sh | EDC_VERSION=0.1.0 sh
```

A release holds binaries for Linux and macOS on `amd64` and `arm64`.

## Update

`edc update` reads the latest release, checks the SHA-256, and replaces the running binary.

```bash
edc update           # confirm, then replace
edc update --check   # print the two versions only
edc update --yes     # skip the confirmation
```

`edc` writes the new file next to the old one and renames it. A failed download leaves the earlier binary in place. If the directory needs a privilege, `edc` stops with exit code `3` before it downloads anything.

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

## Quick start

```bash
# live host resource dashboard (press q to quit)
./bin/edc top
# the earlier table output
./bin/edc top --interval 2s --count 10
# one JSON line for each sample
./bin/edc top --count 5 --json -

# system, network, and disk information, with the public IP
./bin/edc info
# skip the ipinfo.io request
./bin/edc info --public=false

# default diagnosis
./bin/edc doctor https://example.com

# save a machine-readable report (redaction on, file mode 0600)
./bin/edc doctor --json report.json https://example.com
./bin/edc report show report.json
# compare two reports (exit 1 if one probe gets worse)
./bin/edc report diff before.json after.json

# full diagnosis with bandwidth and responsiveness
./bin/edc doctor --profile full --timeout 60s example.com

# single probes
./bin/edc dns lookup example.com
./bin/edc tcp check example.com:443
./bin/edc tls check example.com:443
./bin/edc tls check --min-days 14 example.com:443
./bin/edc http check https://example.com
./bin/edc http check --expect-status 200 https://example.com
./bin/edc net route example.com
./bin/edc net ping example.com
./bin/edc net trace example.com
./bin/edc net interfaces
./bin/edc sockets
./bin/edc quality --timeout 60s

# shell completion
source <(./bin/edc completion zsh)

# update to the latest release
./bin/edc update --check
```

The common options are `--timeout`, `--json <path|->`, `--verbose`, and `--redact=true|false`. Go `flag` rules put an option before the target.

`edc info` asks ipinfo.io for the public IP by default. The request stops after 3 seconds and the line disappears. Use `--public=false` to skip the request, `--timeout` to change the limit, and `-v` to print the cause of a failure.

### Name lookup

![edc dns lookup example.com prints the address list and edc dns config prints the resolver setup, both as PASS](docs/media/dns.gif)

`--redact` is on by default, so `edc` hides an IP address as `<ip:...>`.

### Connection check

![edc tcp check connects to example.com:443 and then fails on a closed port with a timeout phase](docs/media/tcp.gif)

A failed probe shows the phase and the cause in an ERROR block. It returns exit code `1`.

### Route and interfaces

![edc net interfaces, edc net route example.com, and edc net ping example.com each print PASS and one result line](docs/media/net.gif)

## Probe thresholds

`edc tls check` gives a warning when the certificate expires in less than 30 days. Set `--min-days` to make an earlier expiry a failure.

`edc http check` gives a warning for a 4xx response and a failure for a 5xx response. Set `--expect-status` to accept one status code only. A different code is a failure.

```bash
./bin/edc tls check --min-days 14 example.com:443
./bin/edc http check --expect-status 200 https://example.com/health
```

A failure returns exit code `1`. Use these options in cron to get a synthetic check.

![edc tls check passes and then --min-days 90 fails because the certificate expiry is under the threshold](docs/media/tls.gif)

![edc http check passes with HTTP 200 and then --expect-status 404 fails on the status mismatch](docs/media/http.gif)

The demo shows one pass and one threshold failure for each command.

## Report diff

`edc report diff` compares two JSON reports by probe name. It shows the status change and the scalar metric differences of each probe.

```bash
./bin/edc doctor --json before.json https://example.com
./bin/edc doctor --json after.json https://example.com
./bin/edc report diff before.json after.json
./bin/edc report diff --json diff.json before.json after.json
```

The output marks a probe as `WORSE` when the status changes from pass to warn or fail, or from warn to fail. If one probe gets worse, the exit code is `1`. Arrays and objects in `metrics` do not appear in the diff.

## Report viewer

If stdin and stdout are terminals, `edc report show` and `edc report diff` open a full screen viewer.

| key | action |
|---|---|
| `f` | change the filter |
| `e` | show or hide the details |
| `↑` `↓` `PgUp` `PgDn` | scroll |
| `q` | quit |

`edc report show` filters by 전체, 실패와 경고, then 실패만. `edc report diff` filters by 전체, 바뀐 것, then 악화된 것.

The viewer leaves no output on the screen. The exit code stays the same. A pipe, a file, or `--json` gets the earlier output.

![the edc report show viewer changes the filter with f and shows the details with e, then edc report diff shows the duration_ms difference of each probe](docs/media/report.gif)

The demo opens the viewer, changes the filter, shows the details, and then compares two reports.

## Top thresholds

`edc top` gives a color to the load, CPU, iowait, and memory values. White is normal. Orange is a warning. Red is a risk.

The load thresholds follow the core count of the host.

| value | warning | risk |
|---|---|---|
| load | 0.7 × cores | 1.0 × cores |
| usr%, sys% | 70 | 90 |
| i/o | 10 | 25 |
| mem_% | 90 | 95 |

To remove the colors, set `NO_COLOR`. A pipe or a file gets no colors.

`edc info` draws a bar for each disk. The bar uses the same thresholds as `mem_%`. One block is 5 percent. The bar shows the level without color, so a pipe keeps the information.

`edc info` counts the disk usage as the total minus the available space. On macOS, several APFS volumes share one container, so the `Used` column of a single volume misses the space that the other volumes take.

On macOS, `edc` reads the memory usage from `vm_stat`. It subtracts the free, speculative, and inactive pages. This matches `MemAvailable` on Linux. The `PhysMem` line of `top` includes the cache, so it stays above 97 percent.

## Top dashboard

If stdin and stdout are terminals, `edc top` opens a full-screen dashboard. The dashboard needs no `--count` limit.

| key | action |
|---|---|
| `q` | quit |
| `p` | pause and resume |
| `+` | make the interval longer |
| `-` | make the interval shorter |

The interval moves between 200ms, 500ms, 1s, 2s, 5s, 10s, 30s, and 1m. While the dashboard is paused, the arrow and page keys scroll the earlier rows.

The dashboard quits to the previous screen and leaves no rows behind. Use `--json` to keep the values.

`edc top` prints the earlier table instead of the dashboard in these cases:

- The command uses `--count` or `--json`.
- stdin or stdout is not a terminal.
- `NO_COLOR` is set.

The first row after a pause shows the average rate of the paused period.

On macOS, `edc` reads the CPU values from `top`, which needs about one second for each sample. `edc` runs `top` in the background, so a shorter interval still updates the network, disk, memory, and load values on time. The CPU columns hold the same value until the next `top` sample arrives. On Linux, `edc` reads `/proc/stat` directly and every column follows the interval.

## Top JSON output

Use `--json` to write one JSON object for each sample. Use `-` for stdout. A path gets a new file with mode 0600.

```bash
./bin/edc top --count 5 --json -
```

Each line has `time`, `hostname`, `cores`, the network and disk rates in bytes per second, the CPU values in percent, `load1`, and `memory_pct`. The `--json` option removes the table and the header.

## Remote recipes

`edc remote <group>` executes a YAML recipe on an inventory group. It uses local OpenSSH configuration, agents, and known host checks.

Each command loads the remote account default shell in interactive mode. Shell startup output and prompt hooks stay hidden.

Store no passwords or private keys in inventory and recipe files. Configure SSH aliases in `~/.ssh/config`.

Each step needs `name` and `command`. The `verify` field is optional. If `verify` is absent, the exit code of `command` decides the step result.

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

`edc` finds `./inventory.yaml` first. The fallback path is `os.UserConfigDir()/edc/inventory.yaml`. This path follows the operating system. The `recipe.yaml` file uses the same order.

The `--inventory` and `--recipe` flags win over the found files.

If you name a group, `edc` asks no path questions. It shows the plan and requests the confirmation only.

If you name no group, `edc` selects the group first. It then shows the inventory path, selects the recipe, and requests confirmation.

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

The `--dry-run` and `--list` options do not combine with `-f`. The JSON output hides IP addresses when `--redact` is on. Use `--redact=false` to keep them.

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

## Probe live line

A single probe command shows one progress line if stdin and stdout are terminals. The line has the probe name, the target, the elapsed time, and the last output line of the command.

```
⠋     net.trace                 example.com  2.9s  ·   4  <ip:778fad8d>  5.573 ms
```

`edc` starts this line only if the probe runs longer than 300 milliseconds. A fast probe prints the result and nothing else.

Press Ctrl-C to cancel. `edc` stops the command and returns exit code `4`.

The line is always one line. Long output gets a cut at the terminal width.

## Doctor live screen

If stdin and stdout are terminals, `edc doctor` shows one line for each probe and updates the line when the probe ends. The finished lines stay on the screen. The details and the summary follow.

Press Ctrl-C to cancel. `edc` stops the running probes and returns exit code `4`.

`edc` waits and prints all lines at the end if stdin or stdout is not a terminal, if `--json` is set, or if `NO_COLOR` is set.

## Packet capture

Only `capture` uses a privilege. `doctor` does not use `sudo`. Capture keeps hard limits of 60 seconds and 10,000 packets. Capture does not overwrite an existing file.

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
- `2`: a run error, such as an argument, a config, or a report parse error
- `3`: not enough privilege for a privileged task
- `4`: user cancel, which includes a cancelled selection and Ctrl-C in `remote` and `doctor`

## Current scope

`top`, `info`, `doctor`, and the single network probes support Linux and macOS.

On Linux, `edc` reads `/proc`, `/sys`, `ip`, `ss`, `ping`, `traceroute` or `tracepath`, and `/etc/resolv.conf`. If `resolvectl` exists, `edc` adds `resolvectl status` as evidence.

On macOS, `edc` uses a system command adapter. Only macOS runs `quality` and `capture`.

Every command keeps to a read-only diagnosis. `edc` runs no automatic repair, such as a DNS flush, an interface reset, or a firewall change.

## License

MIT. See [LICENSE](LICENSE).
