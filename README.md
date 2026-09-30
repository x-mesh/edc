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

The script installs `edc` in `/usr/local/bin` on Linux and in `~/.local/bin` on macOS. On Linux, `trace` and `capture` need root. `sudo` finds commands only in its own `PATH`, and that `PATH` includes `/usr/local/bin` but not `~/.local/bin`. If you cannot write to the install directory, the script uses `sudo` to copy the binary. Set `BINDIR` for another directory, for example `~/.local/bin` to install without `sudo`. Set `EDC_VERSION` for an earlier version.

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

`edc` writes the new file next to the old one and renames it. A failed download leaves the earlier binary in place. If the directory needs a privilege, `edc` stops with exit code `3` before it downloads anything. If `edc` is in `/usr/local/bin`, run `sudo edc update`.

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

[defaults.log]
stream = "stderr"
output = "/absolute/path/to/edc.log"
command_display = "full"
```

Command-specific values override `defaults.common`. Positional targets, URLs, hosts, and remote groups are never stored, nor are action options such as `yes`, `force`, `dry-run`, `list`, and `check`. Persisted remote inventory and recipe paths must be absolute. Empty path values disable that default.

The setup wizard recommends `~/Library/Logs/edc.log` on macOS and `${XDG_STATE_HOME:-~/.local/state}/edc/edc.log` on Linux. `edc log` creates the parent directory only for this generated default; custom output paths must already have a parent directory.

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

# repeat a page check; omit --duration to run until Ctrl-C
./bin/edc watch -i 0.1 --duration 10s https://example.com

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

### Name lookup

![edc dns lookup example.com prints the address list and edc dns config prints the resolver setup, both as PASS](docs/media/dns.gif)

Terminal and JSON output show actual IP addresses by default. Add `--redact` to hide them as `<ip:...>`.

`edc dns compare example.com` compares the system resolver with `1.1.1.1`. Repeat `--resolver IP[:port]` to choose other DNS servers. It compares A, AAAA, CNAME, and response status; TTL is shown for explicit resolvers but not used to mark a mismatch. A different answer is WARN, and a failed system lookup is FAIL.

### Connection check

![edc tcp check connects to example.com:443 and then fails on a closed port with a timeout phase](docs/media/tcp.gif)

A failed probe shows the phase and the cause in an ERROR block. It returns exit code `1`.

`edc watch -i 0.1 https://example.com` checks the page repeatedly until Ctrl-C. `-i` accepts seconds as a decimal (minimum `0.1`) or a duration such as `100ms`; `--duration 1m` stops automatically. Every sample prints HTTP status, body bytes read (up to 10 MiB), elapsed time, and available DNS/TCP/TLS/TTFB timings. The resolved IP set appears when it changes. The final summary includes min/avg/p95/max latency and the longest continuous failure. `--json <path|->` emits JSON Lines with a final summary. A failed sample makes the final exit code `1`.

`edc listen --watch` prints the current listeners once, then reports sockets opening, closing, or changing process. Change lines use reverse video in a terminal. It accepts the same interval and duration options; `--json` emits a snapshot, events, and a summary as JSON Lines.

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

`hot core` shows the usage of one core, so it gets a warning at 90 and no risk level. A host with many cores keeps room when one core is full.

The dashboard gives no color to `iops`, `busy%`, `swap/s`, the byte and packet rates, and the `signal` column. These values have no threshold, or they show the level without a color. Aggregate `busy%` goes above 100 on a host with more than one busy disk, so a fixed threshold gives a wrong signal. The `cores` bar shows the level with `.`, `:`, `*`, and `#`.

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
| `s` | switch to Linux pressure columns |
| `↑`, `↓`, `PgUp`, `PgDn`, `End` | select an earlier row, move one screen, or return to the live row |
| `Enter` | show details for the selected time |
| `h` | show the load, CPU, iowait, and memory peaks from the last 60 seconds, each with its time |

The default table has a `signal` column. It shows the highest-priority warning among load, CPU, iowait, memory, disk await, and network errors or drops, followed by the number of other warnings. Network errors and drops count as a warning from 1 per second. Use the network view for packet counts. Sampling continues while an earlier row is selected; press `End` to follow the latest row again. A temporary sampling error keeps the last row and retries on the next interval.

The default table follows the terminal width. If the terminal is wider than 80 columns, the table adds columns in this order:

| terminal width | added columns |
|---|---|
| 84 | hot core |
| 96 | disk IOPS and `await` |
| 110 | packet in/out |
| 120 | network errors and drops |
| 125 | disk `busy` |

The `signal` column gets 13 to 16 characters. That is room for at least one warning and the number of the other warnings. The other columns share the remaining width, so the table fills the terminal. The title line also adds the OS name, the memory size, and the CPU model when the terminal has room. The right edge of the title shows the view and `live` or `history`. It adds the edc version when the terminal has room. If the terminal becomes narrower, the table removes those columns immediately.

On macOS and Linux, the disk view also shows IOPS and average `await` across physical disks. On Linux, the disk view also shows aggregate `busy%`, and the memory view shows memory pressure next to `mem%`. Aggregate `busy%` can exceed 100 when multiple disks are busy at once. macOS does not report the time that a disk is busy, so macOS shows `—` for these Linux-only values. On macOS and Linux, the network view shows interface errors and drops. On macOS, `edc` reads them from the interface statistics of the kernel (`net.link.generic.ifdata`). On macOS and Linux, the memory view shows `swap/s`, the bytes per second that the kernel moves out to swap. A value above zero shows that the host is short of memory.

Press `s` for Linux pressure. It shows CPU, memory, and I/O `some avg10`: the percentage of the last ten seconds during which at least some tasks waited for that resource. The CPU view shows the hottest core and an ASCII bar; on machines with more than 24 cores, the bar shows the first 24.

The detail view also lists the top three processes by CPU. The list refreshes in the background at most once a second, so it does not lengthen the observation interval. Only the dashboard collects it; the table and `--json` output skip it. On Linux, `edc` compares the CPU ticks in `/proc/<pid>/stat` with the previous refresh, so the value covers the time since that refresh. On macOS, it uses the recent decaying average that `ps` reports.

On macOS and Linux, a process at 80% CPU or more appears first in the default `signal` column with its name and CPU value, followed by the number of other warnings. CPU is measured as 100% per core, so `node 185%` means roughly 1.85 cores in use.

The interval moves between 200ms, 500ms, 1s, 2s, 5s, 10s, 30s, and 1m. Resuming first creates a new baseline, and later rows show rates.

The dashboard quits to the previous screen and leaves no rows behind. Use `--json` to keep the values.

`edc top` prints the earlier table instead of the dashboard in these cases:

- The command uses `--count` or `--json`.
- stdin or stdout is not a terminal.
- `NO_COLOR` is set.

On macOS, `edc` reads the CPU ticks of each core from the kernel with the Mach `host_processor_info` call. On Linux, `edc` reads `/proc/stat`. On both systems, every column follows the interval.

## Top JSON output

Use `--json` to write one JSON object for each sample. Use `-` for stdout. A path gets a new file with mode 0600.

```bash
./bin/edc top --count 5 --json -
```

Each line has `time`, `hostname`, `cores`, the network and disk rates in bytes per second, the CPU values in percent, `load1`, `memory_pct`, and `swap_out_bytes_per_s`. macOS and Linux emit network errors and drops, and disk IOPS and await values. Linux additionally emits disk busy values and PSI `some avg10`; `*_health_supported`, `disk_busy_supported`, and `psi_supported` tell consumers whether those values are supported. The `--json` option removes the table and the header.

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

A command group asks which command to run. `edc dns`, `edc net`, `edc report`, `edc route`, `edc change`, and `edc completion` show their commands. `edc report show` and `edc report diff` then list the reports in the current directory.

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
In the full-screen terminal view, press `s` for source rows, `t` for target rows, `p` for port rows, `c` for process rows, `e` for event rows, or `g` for scrolling events. Press `Tab` to show the next view. Press `Shift+Tab` to show the previous view. If the terminal is wide, the first column becomes wider and shows the full group value. If the terminal is narrow, the byte columns use short units, for example `195K` for 195 KiB.
The full-screen view keeps the last 10,000 events. The live rates use the time that these events cover. The summary after Ctrl-C uses all events.
Grouped rows in the full-screen view show the groups with the most traffic first. If the rows do not fit the terminal, the view shows the top rows.

In the event list, press Up or Down (or `k` and `j`) to select an event. While an event is selected, the list stops and does not follow new events. The header shows the number of newer events. Press Enter to see all the fields of the event and the whole payload. The detail view wraps long lines. Press Up, Down, PgUp (or `b`), PgDn (or Space), `g`, or `G` to scroll, and press Esc or `q` to go back. Press `l`, Esc, or End in the list to follow new events again. Many Mac keyboards have no End key. In the list, `b` and Space also move the selection by a page. If the selected event becomes the oldest one in the list, the list shows newer events below it. The list keeps the first 4 KiB of each payload. The detail view shows the whole payload of recent events, up to 64 MiB in total.

Press `f` to open the detail view on the newest event and follow new events. Press `f` again to stop at the event on the screen. In `trace http`, the full screen collects the first 4 KiB of each message also without `--payload`. The list hides the payload lines until you press `v`. With `--payload`, the list shows them from the start. Press `m` to show or hide the values of the `Authorization`, `Proxy-Authorization`, `Cookie`, and `Set-Cookie` headers. While they are visible, the header shows `secrets shown`. In the detail view, press `z` to decode a gzip body. edc decodes up to 1 MiB and shows how many bytes it decoded.

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

Use `trace http` on Linux 6.4 or later to print plain HTTP/1.x requests and responses as they arrive. edc reads the first 512 bytes of each TCP read and write in the kernel, or the first 4 KiB with `--payload`. It keeps the method, the `Host` header, the path, and the status code. It removes the query from the path, because the query can contain tokens. Without `--payload`, it also drops the other headers and the body.

```bash
./bin/edc trace http
./bin/edc trace http --group-by target
./bin/edc trace http --side server --process nginx
./bin/edc trace http --payload
./bin/edc trace http --side server --port 8080
./bin/edc trace http --port 8080 --payload=all
```

A request is an `http_request` event. A response is one of `http_1xx` to `http_5xx`, and `status` has the code. HTTP/1.x answers the requests on one connection in order. So a response matches the oldest request on the same connection that has no answer. `latency_ms` starts when the client sends the request and stops when the client reads the response. A `1xx` response does not end the request.

The `target` of an HTTP event is the `Host` header. If there is no `Host` header, the target is the server address. Use `--side server` to watch a local HTTP server. On the server side, `latency_ms` starts when the server reads the request and stops when the server writes the response.

edc finds HTTP by the start of the data, not by the port, so it sees HTTP on any port. The `source` column is always this host, and `destination` is the peer. Use `--port` to watch one HTTP server port. On the client side, it is the port of the server that this host calls. With `--side server`, it is the port of the local server. edc checks the port in the kernel, so it does not read the data of other connections.

Use `--payload` to see the data of each message. Under each event, edc prints the body. If there is no body, it prints the headers. With `--raw`, the `payload` field has all the data. The data is the first 4 KiB (4,096 bytes) of the read or the write, so edc cuts a longer body. If a program writes the headers and the body in two writes, edc does not see the body. Each record is larger with `--payload`, so a busy server can cause lost events. The summary shows the number of lost events. `--payload` keeps the query, but it hides the values of the `Authorization`, `Proxy-Authorization`, `Cookie`, and `Set-Cookie` headers. edc shows the other headers, the query, and the body as they are, and they can contain tokens and passwords. Before you share the output, check it for tokens and passwords. edc changes control characters to `\xNN`, so the data cannot change the terminal. `--payload` does not work with `--json`, because `--json` writes only the summary.

Use `--payload=all` to see each whole message, up to 1 MiB. edc follows the message into the next reads and writes, and into the other buffers of a `writev`. edc prints the event when the message ends. The end is the last byte of `Content-Length`, the last chunk of a chunked body, or one second without data. So the event can come out later than with `--payload`, but the latency does not change. The plain output prints the whole message under the event. The full screen still shows one line. If edc cuts the message at 1 MiB, loses a part, or stops before the end, the event has `"payload_truncated": true`. Write `--payload=all` without a space. `--payload all` is an error. `--payload=all` uses more CPU than `--payload`, so a busy server causes lost events sooner.

Use `--show-secrets` with `--payload` to show the values of the `Authorization`, `Proxy-Authorization`, `Cookie`, and `Set-Cookie` headers. Other people can use an account with these values. Do not share output that has them. In the full screen, press `m` instead.

The summary after Ctrl-C shows one row for each method, host, and path. Grouped rows show the requests, the responses, the 4xx and 5xx responses, the unanswered requests, and the average and maximum latency.

`trace http` does not show HTTPS, HTTP/2, or HTTP/3, because the kernel sees only encrypted data or binary frames. edc finds a message only at the start of a read or a write. If one read has the end of a response and the start of the next response, edc misses the next response. If a program writes one message from several buffers, edc reads only the first buffer. So the `Host` header must be in the first buffer and in the first 512 bytes. If it is not, the target is the server address. The kernel field that edc reads came in Linux 6.4, so older kernels stop with an error.

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

`edc log` appends either `stdout` or `stderr` from a command to a file. The selected stream goes only to the file; the other stream and `stdin` stay connected to cron or the calling terminal. This makes a quiet cron failure visible without requiring logging support in the application.

```cron
* * * * * /usr/local/bin/edc log -- /usr/local/bin/job --daily
```

The short form uses `defaults.log.stream`, `output`, and `command_display`. Each value can still be overridden with its CLI option. Without a configured or explicit stream and output, `edc log` keeps reporting them as required.

Each run gets ASCII start and end markers with its time, duration, and exit status. A new file is mode `0600`; an existing file keeps its contents and mode. Runs targeting the same file wait for each other, so their blocks do not mix. The child exit code and signal status pass through `edc`.

The default `--command-display full` puts the complete argument list in the start marker. Use `--command-display name` or `none` when arguments may contain credentials. `edc log` only appends: it does not rotate, compress, or remove logs. Configure retention with the system log rotation service.

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

## Current scope

`top`, `info`, `doctor`, and the single network probes support Linux and macOS.

On Linux, `edc` reads `/proc`, `/sys`, `ip`, `ss`, `ping`, `traceroute` or `tracepath`, and `/etc/resolv.conf`. If `resolvectl` exists, `edc` adds `resolvectl status` as evidence.

On macOS, `edc` uses a system command adapter. Linux and macOS run `capture`. Only macOS runs `quality`.

Every diagnostic command keeps to read-only inspection. `edc` runs no automatic repair, such as a DNS flush, an interface reset, or a firewall change. `edc log` only writes its explicit output file.

## License

MIT. See [LICENSE](LICENSE).
