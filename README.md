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

# system/network/disk 정보
./bin/edc info
# public IP/지역/ASN은 외부 ipinfo.io 요청을 명시적으로 허용할 때만 조회
./bin/edc info --public

# 기본 종합 진단
./bin/edc doctor https://example.com

# machine-readable report 저장 (기본 redaction, 파일 mode 0600)
./bin/edc doctor --json report.json https://example.com
./bin/edc report show report.json

# bandwidth/responsiveness 측정을 포함한 종합 진단
./bin/edc doctor --profile full --timeout 60s example.com

# 개별 probe
./bin/edc dns lookup example.com
./bin/edc tcp check example.com:443
./bin/edc tls check example.com:443
./bin/edc http check https://example.com
./bin/edc net route example.com
./bin/edc net ping example.com
./bin/edc net trace example.com
./bin/edc net interfaces
./bin/edc sockets
./bin/edc quality --timeout 60s
```

공통 option은 `--timeout`, `--json <path|->`, `--verbose`, `--redact=true|false`입니다. Go `flag` 규칙에 따라 option은 target 앞에 둡니다.

## Remote recipes

`remote run` executes a YAML recipe on an inventory group. It uses local OpenSSH configuration, agents, and known host checks.

Each command loads the remote account default shell in interactive mode. Shell startup output and prompt hooks stay hidden.

Store no passwords or private keys in inventory and recipe files. Configure SSH aliases in `~/.ssh/config`.

Copy the inventory example into the current directory. The interactive command finds `./inventory.yaml` before the user configuration directory.

The fallback path is `os.UserConfigDir()/edc/inventory.yaml`. This path follows the operating system.

```bash
cp examples/remote/inventory.yaml ./inventory.yaml
./bin/edc remote run
```

The interactive command selects the group first. It then shows the inventory path, selects the recipe, and requests confirmation.

Use all flags for cron or launchd. A non-terminal command never waits for input.

```bash
./bin/edc remote run \
  --inventory ./inventory.yaml \
  --recipe ./examples/remote/daily-update.yaml \
  --group daily \
  --parallel 2 \
  --json ./remote-report.json
```

Use `-v` or `--verbose` to stream each remote command. These global flags keep the interactive selector when target flags are absent.

The interactive group selector uses the up and down arrow keys. Press Enter to select a group.

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

## Exit code

- `0`: 성공 또는 warning만 존재
- `1`: 하나 이상의 probe 실패
- `2`: argument, config, report parse 등 실행 오류
- `3`: privileged 작업의 권한 부족
- `4`: 사용자 취소

## 현재 범위

`top`과 `info`는 Linux와 macOS를 지원합니다. Linux에서는 `/proc`과 `/sys`, macOS에서는 system command adapter를 사용합니다. 나머지 상세 network command는 현재 macOS 우선이며 read-only 진단에 집중합니다. DNS flush, interface reset, firewall 변경 같은 자동 복구는 하지 않습니다.
