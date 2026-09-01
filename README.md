# edc

`edc`는 SE/SRE 네트워크·시스템 진단 CLI입니다. Linux/macOS resource monitoring과 host 정보를 제공하며, DNS, TCP, TLS, HTTP, route, interface, socket과 macOS `networkQuality`를 공통 결과 형식으로 실행합니다.

## 빌드

Go 1.23 이상이 필요합니다. 외부 Go dependency는 없습니다.

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

## 빠른 시작

```bash
# 실시간 host resource 모니터 (Ctrl-C로 종료)
./bin/edc top
./bin/edc top --interval 2s --count 10
# sample당 한 줄 JSON
./bin/edc top --count 5 --json -

# system/network/disk 정보
./bin/edc info
# public IP/지역/ASN은 외부 ipinfo.io 요청을 명시적으로 허용할 때만 조회
./bin/edc info --public

# 기본 종합 진단
./bin/edc doctor https://example.com

# machine-readable report 저장 (기본 redaction, 파일 mode 0600)
./bin/edc doctor --json report.json https://example.com
./bin/edc report show report.json
# 두 report 비교 (악화된 probe가 있으면 exit 1)
./bin/edc report diff before.json after.json

# bandwidth/responsiveness 측정을 포함한 종합 진단
./bin/edc doctor --profile full --timeout 60s example.com

# 개별 probe
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
```

공통 option은 `--timeout`, `--json <path|->`, `--verbose`, `--redact=true|false`입니다. Go `flag` 규칙에 따라 option은 target 앞에 둡니다.

## Probe thresholds

`edc tls check` gives a warning when the certificate expires in less than 30 days. Set `--min-days` to make an earlier expiry a failure.

`edc http check` gives a warning for a 4xx response and a failure for a 5xx response. Set `--expect-status` to accept one status code only. A different code is a failure.

```bash
./bin/edc tls check --min-days 14 example.com:443
./bin/edc http check --expect-status 200 https://example.com/health
```

A failure returns exit code `1`. Use these options in cron to get a synthetic check.

## Report diff

`edc report diff` compares two JSON reports by probe name. It shows the status change and the scalar metric differences of each probe.

```bash
./bin/edc doctor --json before.json https://example.com
./bin/edc doctor --json after.json https://example.com
./bin/edc report diff before.json after.json
./bin/edc report diff --json diff.json before.json after.json
```

The output marks a probe as `WORSE` when the status changes from pass to warn or fail, or from warn to fail. If one probe gets worse, the exit code is `1`. Arrays and objects in `metrics` do not appear in the diff.

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

On macOS, `edc` reads the memory usage from `vm_stat`. It subtracts the free, speculative, and inactive pages. This matches `MemAvailable` on Linux. The `PhysMem` line of `top` includes the cache, so it stays above 97 percent.

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

The interactive group selector uses the up and down arrow keys. Press Enter to select a group.

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

`edc` prints the PASS or FAIL line at the end of each step. The final output shows the failures and the summary.

Set `parallel` in the inventory to run hosts concurrently. Use `group_options.<group>.parallel` for one group.

The `--parallel` option overrides both inventory values. Each host still runs its steps in order.

Each host runs in inventory order. Each step runs its command and verify command in recipe order.

If a step fails, `edc` skips later steps on that host. The next host still runs. Any failure returns exit code `1`.

## Packet capture

`capture`만 privileged 작업이며, `doctor`는 `sudo`를 사용하지 않습니다. Capture에는 강제 상한(duration 60초, packet 10,000개)이 있고 기존 파일을 덮어쓰지 않습니다.

```bash
./bin/edc capture \
  --interface en0 \
  --duration 15s \
  --count 500 \
  --filter 'host 203.0.113.10 and port 443' \
  --output incident.pcap
```

PCAP에는 credential과 개인정보가 포함될 수 있습니다. JSON redaction은 PCAP payload에 적용되지 않습니다.

## Shell completion

`edc completion` prints a completion script. The script completes commands, options, and the group names of the inventory that `edc` finds.

```bash
source <(edc completion zsh)
source <(edc completion bash)
```

For zsh, you can also save the script as `_edc` in a directory of `fpath`.

`edc completion groups` prints the group names of the inventory, one per line. The scripts call it.

## Exit code

- `0`: 성공 또는 warning만 존재
- `1`: 하나 이상의 probe 실패, 또는 `report diff`에서 악화된 probe 존재
- `2`: argument, config, report parse 등 실행 오류
- `3`: privileged 작업의 권한 부족
- `4`: 사용자 취소

## 현재 범위

`top`, `info`, `doctor`와 개별 network probe는 Linux와 macOS를 지원합니다. Linux에서는 `/proc`, `/sys`, `ip`, `ss`, `ping`, `traceroute` 또는 `tracepath`, `/etc/resolv.conf`를 읽고, `resolvectl`이 있으면 `resolvectl status`를 evidence로 덧붙입니다. macOS에서는 system command adapter를 사용합니다. `quality`와 `capture`는 macOS 전용입니다. 모든 command는 read-only 진단에 집중하며, DNS flush, interface reset, firewall 변경 같은 자동 복구는 하지 않습니다.
