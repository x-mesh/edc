[English](README.md) | **한국어**

# edc

`edc`는 **everyday carry**의 줄임말입니다. everyday carry는 주머니에 넣고 다니면서 가장 먼저 꺼내 쓰는 작은 도구 모음을 뜻합니다. `edc`는 SE와 SRE가 terminal에서 그렇게 쓰는 도구입니다.

장애가 나면 첫 질문은 하나입니다. 원인이 내 쪽인지, 네트워크인지, 상대편인지. `edc`는 명령 하나로 답합니다. DNS, TCP, TLS, HTTP, route, ping, interface, socket을 한 번에 확인하고 결과를 모두 같은 형식으로 출력합니다. Linux와 macOS의 host resource와 host 정보도 함께 보여 주며, 네트워크 응답성(RPM)과 처리량도 잽니다. macOS는 `networkQuality`를 실행하고, Linux는 IETF responsiveness draft를 따르는 내장 측정을 씁니다.

진단과 관측 command는 read-only입니다. `watch fs`는 `--exec`나 `--rules`로 지정한 커맨드를 실행할 수 있습니다. 기본 관측은 원인을 찾는 데서 멈춥니다. DNS flush, interface reset, firewall 변경 같은 자동 복구를 하지 않으므로 운영 중인 host에서도 그대로 씁니다.

host를 바꾸는 command는 `host changes` 그룹의 `edc route`, `edc disk`, `edc change`뿐입니다. 하위 command를 직접 입력하고, root로 실행하고, 확인을 받아야 바뀝니다. `edc`는 `sudo`를 붙이지 않습니다.

![edc doctor https://example.com이 probe 9개를 차례로 실행하고 9 pass 요약을 출력하는 화면](docs/media/doctor.gif)

한 번 실행에 3초쯤 걸립니다. 각 줄은 probe 이름, 대상, 결과를 같은 열에 맞추므로 열 하나만 따라 내려가면 실패한 probe가 보입니다.

데모 화면은 기본 언어인 영어입니다. `EDC_LANG=ko`를 지정하면 한국어로 나옵니다. [언어](#언어)를 보십시오.

각 데모의 소스는 [`docs/tape/`](docs/tape) 아래 `.tape` 파일입니다. `vhs docs/tape/doctor.tape`처럼 실행하면 다시 만듭니다.

## 설치

script로 최신 release를 설치합니다. script는 운영체제와 architecture를 읽고, SHA-256을 확인한 다음 실행 파일을 설치합니다.

```bash
curl -fsSL https://raw.githubusercontent.com/x-mesh/edc/main/install.sh | sh
```

script는 Linux에서는 `edc`를 `/usr/local/bin`에, macOS에서는 `~/.local/bin`에 설치합니다. Linux의 `trace`와 `capture`는 root가 필요하고, `sudo`의 `PATH`에는 보통 `/usr/local/bin`만 있기 때문입니다. 일반 사용자가 설치 디렉터리에 쓸 수 없으면 script는 복사할 때만 `sudo`를 씁니다. root가 `/usr/local/bin`에 쓸 수 없으면 `~/.local/bin`으로 전환합니다. 다른 디렉터리를 반드시 사용하려면 `BINDIR`을 지정합니다. 이전 버전을 받으려면 `EDC_VERSION`을 지정합니다.

```bash
curl -fsSL https://raw.githubusercontent.com/x-mesh/edc/main/install.sh | BINDIR="$HOME/.local/bin" sh
curl -fsSL https://raw.githubusercontent.com/x-mesh/edc/main/install.sh | EDC_VERSION=0.1.0 sh
```

이전 script는 Linux에서도 `edc`를 `~/.local/bin`에 설치했습니다. Ubuntu는 `PATH`에서 `~/.local/bin`을 `/usr/local/bin`보다 앞에 두므로, 이 옛 파일이 남아 있으면 계속 옛 버전이 실행됩니다. 그래서 script는 옛 파일이 `edc`이면 지웁니다. script를 실행한 사용자의 home과, `sudo`로 실행했다면 `sudo`를 실행한 사용자의 home을 확인합니다. 다른 사용자의 home에 있는 파일은 `sudo rm /home/<user>/.local/bin/edc`처럼 직접 지웁니다.

설치 디렉터리가 `PATH`에 없으면 script가 사용 중인 shell을 판단해 그 디렉터리를 추가하는 명령을 출력합니다. `EDC_MODIFY_PATH=1`을 지정하면 script가 `~/.zshrc`, `~/.bashrc` 같은 shell 시작 파일에 직접 한 줄을 추가하며, 이미 있으면 다시 넣지 않습니다. 지금 열려 있는 shell에는 새 `PATH`가 반영되지 않으므로, 새 shell을 열거나 script가 출력한 명령을 실행합니다.

```bash
curl -fsSL https://raw.githubusercontent.com/x-mesh/edc/main/install.sh | EDC_MODIFY_PATH=1 sh
```

release에는 Linux와 macOS의 `amd64`, `arm64` 실행 파일이 들어 있습니다.

## 업데이트

`edc update`는 최신 release를 읽고 SHA-256을 확인한 다음 실행 중인 파일을 바꿉니다.

```bash
edc update           # 확인한 다음 교체
edc update --check   # 두 버전만 출력
edc update --yes     # 확인 생략
```

`edc`는 새 파일을 기존 파일 옆에 쓰고 이름을 바꿉니다. 내려받기가 실패하면 기존 실행 파일이 그대로 남습니다. 디렉터리에 쓸 권한이 없으면 install script처럼 새 파일의 복사와 이름 바꾸기만 `sudo`로 합니다. 내려받기와 checksum 확인은 사용자 권한으로 합니다. 확인 화면에 `권한 sudo`가 나오고, 암호가 필요하면 `sudo`가 한 번 묻습니다. `sudo`가 없거나 실패하면 내려받기 전에 exit code `3`으로 멈춥니다.

## 빌드

Go 1.25 이상이 필요합니다. 실시간 화면은 다음 dependency를 씁니다.

- `charm.land/bubbletea/v2`
- `charm.land/bubbles/v2`
- `charm.land/lipgloss/v2`

```bash
make build VERSION=0.1.0-dev
./bin/edc version
```

`edc`를 `~/.local/bin`에 설치합니다.

```bash
make install VERSION=0.1.0-dev
~/.local/bin/edc version
```

다른 경로에 설치하려면 `PREFIX`나 `BINDIR`을 지정합니다.

```bash
make install PREFIX=/usr/local
```

## 언어

`edc`는 기본으로 영어를 씁니다. 한국어와 일본어도 함께 담고 있습니다.

언어는 설정 파일에서 정합니다. Linux와 macOS 모두 `$XDG_CONFIG_HOME/edc/config.toml`이 설정되어 있으면 그 경로를, 아니면 `~/.config/edc/config.toml`을 읽습니다.

```toml
lang = "ko"
```

`config.toml`이 없으면 Linux는 `~/.config/edc/config.yaml`, macOS는 `~/Library/Application Support/edc/config.yaml`을 계속 읽습니다. `edc setup`은 기존 값을 유지한 채 새 TOML 파일을 저장하며 YAML 파일은 그대로 둡니다.

한 번만 다른 언어로 보려면 `EDC_LANG`을 씁니다. 설정 파일보다 우선합니다.

```bash
EDC_LANG=ja edc where
```

`en`, `ko`, `ja`를 받습니다. `ko_KR.UTF-8` 같은 locale 이름을 주면 앞의 언어 부분만 봅니다. 모르는 값은 영어로 내려가고, 어떤 언어에 빠진 메시지도 영어로 내려갑니다.

## Command 기본값

같은 config file에 반복 실행해도 안전한 command 기본값을 둘 수 있습니다. 우선순위는 built-in, config, 명시한 CLI option 순서입니다. 모르는 key, 잘못된 type이나 범위가 있으면 조용히 무시하지 않고 command 실행 전에 exit code `2`로 멈춥니다.

terminal에서 `edc setup`을 실행하면 file을 만들거나 수정하는 wizard가 열립니다. section별로 설정하고 Enter로 기존 값을 유지하며, optional 값은 `!clear`로 제거합니다. 전체 config를 미리 보여 준 뒤 확인을 받아 mode `0600`으로 atomic 저장합니다. config directory는 mode `0700`이며 취소하면 exit code `4`입니다.

아래 TOML 예시는 Linux와 macOS에 공통으로 적용됩니다.

```toml
lang = "ko"

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

[defaults.quality]
timeout = "30s"
server = ""

[defaults.log]
stream = "both"
output = ""
command_display = "full"
max_size_mb = 10
keep_files = 3
restart = "never"
max_restarts = 3
restart_delay = "5s"
timeout = "0s"
kill_after = "5s"
```

command별 값은 `defaults.common`을 덮습니다. positional target, URL, host, remote group과 `yes`, `force`, `dry-run`, `list`, `check` 같은 action option은 저장하지 않습니다. 저장하는 remote inventory와 recipe는 absolute path여야 합니다. 빈 path 값은 해당 기본값을 사용하지 않는다는 뜻입니다.

Setup wizard는 `edc log`의 저장 경로를 비워두고 실행별 파일을 자동 생성하도록 추천합니다. 직접 지정한 경로의 상위 디렉터리는 이미 있어야 합니다. 이전 추천값인 `edc.log` 경로는 상위 디렉터리 자동 생성을 계속 지원합니다.

## 빠른 시작

```bash
# 실시간 host resource 대시보드 (q로 종료)
./bin/edc top
# 표를 흘려 보내는 기존 출력
./bin/edc top --interval 2s --count 10
# sample당 한 줄 JSON
./bin/edc top --count 5 --json -

# Claude Code와 Codex의 token 사용량과 계정 한도
./bin/edc ai

# system/network/disk 정보와 public IP
./bin/edc info
# 외부 public IP 조회 없이 실행
./bin/edc info --public=false

# 디렉터리 아래 디스크 사용량, 큰 항목부터 (Linux)
./bin/edc du /var

# 알려진 개발 캐시 사용량, 읽기 전용 (Linux, macOS)
./bin/edc clean

# 기본 종합 진단
./bin/edc doctor https://example.com

# redaction을 적용한 machine-readable report 저장 (파일 mode 0600)
./bin/edc doctor --redact --json report.json https://example.com
./bin/edc report show report.json
# 두 report 비교 (악화된 probe가 있으면 exit 1)
./bin/edc report diff before.json after.json

# bandwidth/responsiveness 측정을 포함한 종합 진단
./bin/edc doctor --profile full --timeout 60s example.com

# 개별 probe
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
./bin/edc listen              # 열려 있는 포트, --unix나 --all로 unix socket 포함
./bin/edc listen --watch -i 0.5 --duration 10s
sudo ./bin/edc audit           # 노출 상태와 설정 점검, 읽기 전용 (Linux)
sudo ./bin/edc svc nginx       # systemd service 하나를 한 화면에 (Linux)
./bin/edc svc                  # systemd service 목록, 실패한 것이 먼저 (Linux)
sudo ./bin/edc ctr             # docker container 목록, 실패한 것이 먼저 (Linux)
./bin/edc changes --since 2h   # 최근 패키지, /etc, service 변경 (Linux)
sudo ./bin/edc events --since 6h   # 커널과 systemd의 장애 신호를 시간 순서로 정리 (Linux)
./bin/edc quality --timeout 60s
./bin/edc quality --server https://example.com/.well-known/nq   # 응답성 config URL

# 페이지 상태 반복 확인; --duration을 생략하면 Ctrl-C까지 실행
./bin/edc watch http -i 0.1 --duration 10s https://example.com

# 어느 지역이 가깝고 이 망은 어떤 모습인지
./bin/edc where
./bin/edc where --provider aws --count 5

# shell completion
source <(./bin/edc completion zsh)

# 최신 release로 업데이트
./bin/edc update --check
```

공통 option은 `--timeout`, `--json <path|->`, `--verbose`, `--redact`입니다. Go `flag` 규칙에 따라 option은 target 앞에 둡니다.

`edc info`는 기본으로 public IP를 조회합니다. `ns1.google.com`에 `o-o.myaddr.l.google.com` DNS 질의 하나를 IPv4로 보냅니다. `-v`를 주면 ipinfo.io에도 물어 기관, 도시, 지역, 국가, 시간대를 함께 보여 줍니다. 3초 안에 응답이 없으면 조회를 멈추고 그 줄을 빼고 출력합니다. 조회를 끄려면 `--public=false`를, 제한 시간을 바꾸려면 `--timeout`을, 실패 원인을 보려면 `-v`를 씁니다. 외부 DNS 서버로 가는 질의를 막는 네트워크에서는 기본 출력에 public IP가 나오지 않습니다.

`edc info`는 메모리 상태, 프로세스 요약, 진단 기능의 지원 여부도 표시합니다. Linux의 사용량은 `MemTotal - MemAvailable`이며, 제공되는 캐시·buffer·회수 가능한 slab 값을 함께 보여 줍니다. macOS의 회수 가능량은 Mach의 free·inactive page를 기준으로 추정하며, 파일 기반 메모리와 압축된 물리 메모리를 구분합니다. 세부 카운터는 서로 겹칠 수 있으므로 합산하지 않습니다. 이 값만으로 메모리 압박을 판정하지 않습니다.

프로세스 요약에는 관측한 프로세스 수, thread 수와 그 수집 범위, RSS가 높은 상위 3개를 표시합니다. 프로세스별 공유 page가 중복될 수 있으므로 RSS를 합산해 host 메모리 사용량으로 해석하지 않습니다.

`edc info`는 다음 신호도 기본으로 표시합니다. 모두 파일, syscall, sysctl에서 읽으며 외부 명령은 실행하지 않습니다.

- `Platform`: DMI의 제조사와 제품 이름입니다(예: `Amazon EC2 m5.large`). Oracle Cloud에서는 chassis asset tag로 `Oracle Cloud`를 덧붙입니다. Linux에서는 CPU가 hypervisor를 알리거나 DMI가 QEMU나 가상 머신을 가리키면 `virtual machine`을, container runtime의 표식이 있으면 그 이름을 덧붙입니다. macOS에서는 모델 식별자를 표시합니다.
- `Clock`(Linux): `adjtimex`가 알려 주는 커널의 시간 동기화 상태입니다. 오차는 표시하지 않습니다. 커널의 오차 값은 갱신 사이에 늘어나기만 하는 상한이라 실제 오차가 아닙니다.
- `DNS`: `/etc/resolv.conf`의 nameserver와 search domain입니다. 파일이 systemd-resolved stub을 가리키면 실제 upstream 서버를 보여 줍니다.
- `Pressure`와 `OOM kills since boot`(Linux): CPU, 메모리, I/O의 PSI some avg10과 `/proc/vmstat`의 `oom_kill` counter입니다.
- 마운트가 읽기 전용이거나 inode 사용률이 80% 이상이면 디스크 줄 끝에 `(read-only)`나 `(inodes N%)`를 붙입니다. macOS의 서명된 system volume은 원래 읽기 전용이므로 표시하지 않습니다.
- Linux에서는 지워진 파일을 process가 아직 열고 있으면 디스크 줄 끝에 `(deleted-open 12.0 GB in 3 files, java)`도 붙입니다. 이 공간은 `du`에는 보이지 않지만 `df`에는 잡히고, 마지막 process가 파일을 닫아야 풀립니다. edc는 `/proc/<pid>/fd`를 읽어 inode마다 한 번만 세고, 가장 큰 파일을 잡은 process 이름을 적습니다. `-v`는 가장 큰 파일 5개를 PID, process, 경로와 함께 보여 줍니다. root가 아니면 다른 사용자의 파일을 읽지 못해 값이 하한이고 `≥`를 붙입니다. file descriptor 200000개에서 멈춥니다.

`-v`는 시스템 전체의 열린 파일 수, IPv6 기본 경로(Linux), 인터페이스별 MTU·MAC·링크 속도(Linux)·IPv6 주소, 마운트 옵션을 더합니다. 인터페이스는 IPv4 주소나 링크 로컬이 아닌 IPv6 주소를 가진 것만 보여 줍니다.

`Listening Ports`는 연결을 기다리는 TCP·UDP 소켓의 수를 표시합니다. loopback 주소에 바인드한 소켓은 개수에만 포함하고, 나머지는 포트·주소·프로세스를 최대 5줄까지 나열합니다. `-v`는 TCP·UDP 소켓을 모두 나열하며, 전체 표는 `edc listen`으로 봅니다. macOS에서 root가 아니면 다른 계정의 소켓은 목록에 나오지 않습니다.

기본 출력은 관련 값을 한 행에 묶습니다. `-v`를 주면 긴 커널 정보, 메모리 계산 기준, 공유 page 주의사항, Python 버전도 표시합니다. Python 버전 확인은 pyenv 같은 shim을 거치면 수백 ms가 걸리므로 기본 출력에서는 하지 않습니다.

진단 지원 여부는 현재 프로세스의 I/O 접근, PSI, eBPF 상세 조건을 확인합니다. 다른 PID의 접근 권한은 다를 수 있습니다. Linux의 eBPF 확인은 kernel BTF와 effective capability만 읽으며 program을 불러오거나 attach하지 않습니다. `prerequisites met`는 실제 attach 성공을 보장하지 않습니다. 미지원·권한 부족·확인 실패는 이유와 함께 표시합니다.

메모리나 프로세스 수집에 실패하면 오류를 표시하고 종료 코드 `1`을 반환합니다.

Linux에서는 `edc info`의 `Network Limits`에 conntrack 사용량과 한도, `tcp_mem` 상한 대비 TCP memory page와 orphan socket 수, 임시 port 범위와 예약 port, accept/SYN 대기열 한도, socket buffer 상한, 패킷 처리 backlog/budget, neighbor 한도, forwarding과 `rp_filter`를 요약합니다. `-v`는 TCP buffer 설정과 개별 sysctl 이름, 읽지 못한 이유도 표시합니다. 값이 없거나 권한이 부족하면 `unavailable`로 표시하며, macOS에서는 이 항목을 `unsupported`로 표시합니다. 설정값만으로 연결 실패를 판정하지 않습니다.

### 이름 조회

![edc dns lookup example.com이 주소 목록을, edc dns config가 resolver 설정을 각각 PASS로 출력하는 화면](docs/media/dns.gif)

기본적으로 터미널과 JSON에 실제 IP 주소를 보여줍니다. `--redact`를 주면 `<ip:...>`로 가립니다.

`edc dns compare example.com`은 system resolver와 `1.1.1.1`을 비교합니다. 다른 DNS 서버를 쓰려면 `--resolver IP[:port]`를 반복해서 지정합니다. A, AAAA, CNAME, 응답 상태를 비교하고, 직접 지정한 resolver의 TTL도 보여 주되 TTL 차이만으로 불일치 처리하지는 않습니다. 응답이 다르면 WARN, system 조회가 실패하면 FAIL입니다.

### 연결 확인

![edc tcp check가 example.com:443 연결에 성공하고, 닫힌 port에서는 timeout phase와 함께 FAIL을 출력하는 화면](docs/media/tcp.gif)

실패한 probe는 phase와 cause를 ERROR 블록으로 보여 주고 exit code `1`을 돌려줍니다.

`edc watch http -i 0.1 https://example.com`은 Ctrl-C까지 페이지를 반복 확인합니다. `-i`는 초 단위 소수(최소 `0.1`)나 `100ms` 같은 duration을 받으며, `--duration 1m`으로 종료 시각을 정할 수 있습니다. 매 sample에 HTTP status, 읽은 body byte(최대 10 MiB), 소요 시간과 수집된 DNS/TCP/TLS/TTFB 시간을 표시합니다. DNS의 IP 집합이 바뀌면 새 목록을 보여 줍니다. 완료한 표본이 있으면 마지막에 min/avg/p95/max 지연과 최장 연속 실패 시간을 요약합니다. `--json <path|->`는 마지막 요약을 포함한 JSON Lines를 출력합니다. 요약에는 `observation_status`(`observed` 또는 `no_samples`)와 `stop_reason`(`duration` 또는 `cancelled`)이 추가됩니다.

완료한 표본이 없으면 텍스트 요약은 관측 종료 이유를 설명하고 지연 통계를 생략합니다. 이때 대상의 상태를 판단할 수 없습니다. 완료한 표본 없이 관측 시간이 끝나면 exit code는 `2`, 취소하면 `4`입니다. 완료한 표본이 있으면 기존 정책을 유지합니다. 실패한 표본이 있으면 `1`, 없으면 취소한 경우에도 `0`입니다.

`edc listen --watch`는 현재 listener를 한 번 출력한 뒤 socket의 생성·종료·소유 process 변경을 알려 줍니다. 변경 줄은 터미널에서 반전 표시합니다. 같은 interval·duration 옵션을 받으며, `--json`은 초기 snapshot·event·요약을 JSON Lines로 출력합니다.

기본·상세 표의 범위 열과 watch 변경 줄은 소켓이 바인딩한 주소를 loopback, 모든 인터페이스, 특정 주소, Unix, 알 수 없음으로 구분합니다.

wildcard의 모든 인터페이스는 관측한 주소 계열의 바인딩을 뜻합니다. 외부 접근 가능 여부, 방화벽 규칙, IPv6 dual-stack 동작을 판단하는 값은 아닙니다.

### 파일 감시와 action (Linux·macOS)

`edc watch fs`는 기본으로 현재 디렉터리 바로 아래의 변경을 감시하고 출력합니다. `--recursive`로 하위 디렉터리까지 감시하며, 새로 생성되거나 들어온 디렉터리도 등록합니다. 시작할 때 이미 있던 파일은 create 이벤트로 출력하지 않습니다. 디렉터리 symlink는 따라가지 않습니다.

```bash
edc watch fs
edc watch fs ./src --recursive
edc watch fs --event create --match text.txt --exec 'git-kit pull'
edc watch fs ./src --recursive --event modify --match '**/*.go' --exec 'go test ./...'
edc watch fs --rules docs/examples/watch.yaml --dry-run
edc watch fs --duration 1m --json events.jsonl
```

이벤트는 `create`, `modify`, `remove`, `rename`입니다. `rename`의 path는 원래 이름이며 감시 범위 안의 새 이름은 create로 나타날 수 있습니다. 파일 읽기(access)와 metadata-only 변경은 감지 대상이 아닙니다. `--event`는 쉼표로 여러 이벤트를 받으며 `--match`는 감시 루트 기준 glob입니다. `*.go`는 바로 아래 파일, `**/*.go`는 모든 깊이의 파일에 일치합니다. 하위 경로를 실제로 감시하려면 `--recursive`도 지정합니다.

기본으로 `.git`, `node_modules`, `.venv`, `venv`, `__pycache__`, `.mypy_cache`, `.pytest_cache`, `.ruff_cache`, `.tox`, `.next`, `.gradle`을 모든 깊이에서 제외합니다. 빌드 결과물을 감시할 수 있도록 `build`와 `dist`는 제외하지 않습니다. `--exclude 'build/**'`처럼 제외 glob을 반복해서 추가할 수 있습니다. 기본 제외 디렉터리도 감시하려면 `--no-default-exclude`를 쓰며, 이때도 `--exclude`로 준 패턴은 적용됩니다.

Linux에서는 `--driver`로 감시 방식을 고릅니다. 기본값은 `auto`입니다.

- `fanotify`: 파일시스템 전체에 mark 하나를 겁니다. 시작할 때 트리를 훑지 않고, inotify watch 한도도 받지 않습니다. root(`CAP_SYS_ADMIN`)와 Linux 5.17 이상이 필요합니다.
- `inotify`: 디렉터리마다 감시를 등록한 뒤에 출력을 시작합니다. 트리가 크면 시간이 걸리며, stderr가 터미널이면 이 동안 안내를 출력합니다. 디렉터리 수가 `fs.inotify.max_user_watches`를 넘으면 오류로 멈춥니다.

`--recursive`이면 `auto`는 `fanotify`를 먼저 시도하고, 시작할 수 없으면 `inotify`를 씁니다. stderr가 터미널이면 그 이유를 한 줄로 알립니다. `--recursive`가 없으면 감시가 하나뿐이므로 `inotify`를 씁니다. 실제로 쓴 방식은 `ready` 기록의 `driver`에 나옵니다. `--driver fanotify`를 지정했는데 `fanotify`를 시작할 수 없으면 이유를 출력하고 멈춥니다.

`fanotify`는 다음 경우에 시작할 수 없습니다.

- root로 실행하지 않았습니다.
- 커널이 Linux 5.17보다 오래되었습니다.
- 감시 루트가 있는 파일시스템이 `fanotify`에 필요한 file handle을 지원하지 않습니다. btrfs subvolume이 그 예입니다.

mark 하나는 파일시스템 하나만 감시합니다. 그래서 `--recursive`이면 감시 루트 아래에 마운트된 파일시스템마다 mark를 겁니다. sysfs나 devpts처럼 mark를 받지 않는 파일시스템이 있고, 감시 루트 밖의 디렉터리를 bind mount한 지점은 그 변경을 감시 루트 안의 경로로 알릴 수 없습니다. `edc`는 이런 마운트를 건너뛰고 stderr에 한 줄로 알리며, 건너뛴 마운트 아래의 변경은 알리지 않습니다. 이 변경이 필요하면 `--driver inotify`를 씁니다. `--exclude`에 걸린 마운트에는 mark를 걸지 않습니다.

`fanotify`는 파일시스템 전체의 이벤트를 받고, `edc`가 감시 루트 밖의 이벤트를 버립니다. 그래서 파일시스템이 바쁘면 CPU를 씁니다. 이벤트가 적게 올 때는 읽기 사이에 5ms를 기다립니다. CPU 사용이 부담되면 `--driver inotify`를 씁니다.

macOS에서는 `watch fs`가 FSEvents를 사용합니다. stream 하나가 트리 전체를 감시하므로 파일 수만큼 file descriptor를 쓰지 않습니다. FSEvents는 변경을 최대 50ms 동안 모아서 보냅니다. macOS가 이벤트를 유실하면 `watch fs`는 그 디렉터리를 다시 훑어서 달라진 점을 `create`, `modify`, `remove`로 알립니다. 이때 이동은 `remove`와 `create`로 나옵니다. 한 번에 최대 200,000개 항목을 훑으며, 다시 훑었다는 사실과 상한에서 멈췄는지를 stderr에 한 줄로 알립니다. 제외한 디렉터리의 변경도 FSEvents가 보내지만 `edc`가 버립니다.

stdout이 터미널이고 `--json`을 쓰지 않으면 화면 맨 아래 줄에 통계를 보여 줍니다. 경과 시간, 이벤트 종류별 개수, 최근 10초 동안의 초당 이벤트 수, rule이 있을 때의 action 수와 실패 수, 마지막 이벤트 이후 지난 시간이 나오고, 1초마다 갱신합니다. 터미널이 좁으면 마지막 이벤트 이후 시간을 먼저 빼고, 그다음 초당 이벤트 수를 뺍니다. 감시가 끝나면 이 줄은 사라지고 요약 줄이 남습니다. pipe, 파일, JSON 출력에는 이 줄이 나오지 않습니다.

JSON 출력 파일과 stdout으로 연결된 일반 파일도 감시에서 제외하여 출력이 다음 이벤트를 만들지 않게 합니다. `--event`와 `--match`는 출력과 action에 공통으로 적용됩니다. rule 파일을 생략하면 기본 action은 없습니다.

rule 파일은 알 수 없는 key를 거부하는 YAML 문서 하나입니다. 아래 예시는 현재 디렉터리를 감시합니다. `directory`를 지정하면 rule 파일이 있는 디렉터리 기준으로 해석하며, CLI의 directory 인자가 우선합니다. rule의 `cwd`는 감시 루트 기준이고 기본값은 감시 루트입니다.

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

`--exec`는 `/bin/sh -c`로 실행하고 YAML의 `command`는 shell 없이 argv 그대로 실행합니다. shell alias는 불러오지 않으므로 실행 가능한 command 이름을 사용합니다. 파일명은 command 문자열에 자동 삽입하지 않으며 `EDC_WATCH_ROOT`(절대 경로), `EDC_WATCH_PATH`(상대 경로), `EDC_WATCH_EVENT`, `EDC_WATCH_RULE` 환경변수로 전달합니다. shell에서 파일명을 사용할 때는 `"$EDC_WATCH_PATH"`처럼 인용합니다.

전체 action은 한 번에 하나씩 실행합니다. 기본 debounce는 200ms이며 rule별 마지막 이벤트를 기준으로 연속 이벤트를 묶습니다. 실행 중 추가 이벤트는 rule당 한 건으로 묶어서 실행 종료 후 다시 실행합니다. `--dry-run`은 일치한 action을 표시하되 실행하지 않습니다. 커맨드가 감시 대상 파일을 변경하면 다시 조건에 맞을 수 있으므로 출력 경로를 제외하거나 match 범위를 좁힙니다.

action의 stdin은 연결하지 않습니다. 기본 timeout은 30s이며 `--debounce`와 `--timeout`은 rule에 값이 없을 때의 기본값입니다. stdout·stderr를 합쳐 최대 64 KiB까지 결과에 표시합니다. 실패나 timeout 뒤에도 감시는 계속하며, 실패한 action이 있으면 감시 종료 코드가 `1`입니다. `Ctrl-C`나 `--duration` 만료 시 실행 중인 action의 process group을 종료하고 대기 중인 action은 실행하지 않습니다. 변경이 없는 정상 감시는 종료 코드 `0`, 옵션·출력 오류는 `2`입니다. 감시 루트 삭제·이름 변경, watcher 오류나 이벤트 overflow는 관측이 불완전하므로 오류로 종료합니다.

`--json`은 `ready`, `event`, `action_start`, `action_result`, `summary`를 JSON Lines로 씁니다. action 출력은 `action_result.output` 안에 있어 JSON stream을 섞지 않습니다. 감시는 로컬 filesystem을 대상으로 하며 저장 방식에 따라 이벤트가 합쳐지거나 여러 번 발생할 수 있습니다. 모든 파일 작업을 기록하는 audit 기능은 아닙니다.

### 경로와 interface

![edc net interfaces, edc net route example.com, edc net ping example.com이 각각 PASS와 결과 한 줄을 출력하는 화면](docs/media/net.gif)

## 어디에 있는가

`edc where`는 두 질문에 한 번에 답합니다. 이 host에서 어느 클라우드 지역이 가까운지, 그리고 이 망이 어떤 모습인지.

```bash
./bin/edc where
./bin/edc where --provider aws        # 사업자 하나만
./bin/edc where --count 5 -v          # 더 여러 번 재고 사업자별로 모두 보기
```

`edc`는 지역마다 공개 endpoint에 TCP handshake만 열고 끊습니다. 요청을 보내지 않고 본문도 읽지 않으므로 값에는 왕복 시간만 남습니다. 이름은 한 번만 풀고 그 주소로 연결해 DNS 시간이 거리 값에 섞이지 않게 합니다.

표는 endpoint를 도시로 묶고 도시마다 가장 빠른 사업자를 남깁니다. 사업자별 값을 모두 보려면 `-v`를 씁니다.

| 사업자 | endpoint |
|---|---|
| `aws` | `s3.<region>.amazonaws.com` |
| `gcp` | `storage.<region>.rep.googleapis.com` |

Azure는 넣지 않았습니다. 리전 이름이 붙은 공개 주소가 실제로 그 리전에서 끝나지 않아 값이 거리를 따르지 않습니다.

`edc where`는 public IP와 ASN, anycast가 고른 Cloudflare PoP, 그리고 로컬 망의 모습도 함께 보여 줍니다.

- NAT 뒤인지, 아니면 interface가 public 주소를 직접 쓰는지
- carrier-grade NAT 뒤인지. `100.64.0.0/10` 대역이 그 표시입니다
- tunnel을 지나는지. 기본 경로 interface가 그 표시입니다

terminal에서는 몇 곳을 확인했는지 진행 줄로 보여 줍니다. `q`로 취소합니다.

기본 화면에는 주소를 그대로 씁니다. `--redact`를 주면 화면과 JSON 출력에서 가립니다.

![edc where가 public IP와 ASN, Cloudflare PoP, route, NAT 모습, 왕복 시간 순으로 가까운 지역을 보여 주는 화면](docs/media/where.gif)

데모 화면은 public IP를 `222.XXX.XXX.XXX`로 가리고, route 주소를 사설 예시 주소로 바꿨습니다.

`edc`는 확인한 것만 밝힙니다. 지터로 회선 종류를 짐작하지 않습니다. 지터는 숫자로 남기고 판단은 사용자에게 맡깁니다.

## 호스트 점검 (Linux)

`edc audit`는 Linux 호스트의 노출 상태와 설정을 읽습니다. 파일, sysctl 값, listen 소켓만 읽고, 아무것도 바꾸지 않으며 외부로 보내지 않습니다. 1초 안에 끝납니다.

```bash
sudo edc audit
sudo edc audit -v               # 결과마다 근거가 된 값과 규칙 추가
sudo edc audit --json -         # 점검마다 결과 하나
```

| 점검 | fail | warn |
|---|---|---|
| `net.exposed` | DB·제어 포트가 loopback 밖에 열려 있고 호스트 방화벽이 들어오는 트래픽을 허용함 | 같은 포트가 열려 있지만 방화벽이 기본으로 차단하거나, 방화벽을 읽지 못함 |
| `net.firewall` | | input chain에 규칙이 없음 |
| `net.ip_forward` | | forwarding이 켜져 있는데 컨테이너 런타임이 없음 |
| `ssh.root_login` | | `PermitRootLogin yes` |
| `ssh.password_auth` | 비밀번호 로그인이 켜져 있고 `PermitEmptyPasswords yes` | 비밀번호 로그인이 켜져 있음 |
| `ssh.key_files` | 다른 사용자가 `~/.ssh`나 `authorized_keys`에 쓸 수 있음 | |
| `user.uid0` | root 외에 UID 0인 계정이 있음 | |
| `user.empty_pass` | 빈 비밀번호 계정이 있음 | |
| `user.sudo_nopass` | | 비밀번호 없이 모든 명령을 허용하는 sudo 규칙이 있음 |
| `user.root_groups` | | `docker`나 `lxd` 그룹에 멤버가 있음 |
| `file.sensitive` | 다른 사용자가 `/etc/shadow`를 읽거나 `/etc/passwd`, `/etc/sudoers`, `docker.sock`에 쓸 수 있음 | |
| `file.path_writable` | 다른 사용자가 root `PATH`의 디렉터리에 쓸 수 있거나 `PATH`에 상대 경로가 있음 | |
| `kernel.sysctl` | | 하드닝 값이 권장 값과 다름 |
| `patch.reboot` | | 패키지가 재부팅을 요구하거나, `/boot`에 실행 중인 것보다 새 커널이 있음 |
| `ctr.privileged` | edc가 모든 capability를 가져 컨테이너가 privileged임 | |
| `ctr.docker_sock` | 컨테이너 안의 `/var/run/docker.sock`이나 `/run/docker.sock`이 socket임 | |
| `ctr.root_user` | | edc가 컨테이너 안에서 UID 0으로 실행됨 |
| `ctr.host_ns` | 컨테이너가 호스트의 PID namespace를 씀 | |

DB·제어 포트는 2375, 2379, 3306, 5432, 6379, 9200, 10250, 11211, 27017입니다.

`ctr.*` 점검은 컨테이너 안에서만 실행됩니다. 컨테이너인지는 `edc info`와 같은 표식(`KUBERNETES_SERVICE_HOST`, `/.dockerenv`, `/run/.containerenv`)으로 판단하고, 호스트에서는 보고서에 나오지 않습니다. 컨테이너가 호스트의 PID namespace를 쓰면 `ctr.host_ns`는 network namespace도 PID 1의 것과 비교합니다.

root로 실행하세요. root가 아니면 방화벽, `/etc/shadow`, `/etc/sudoers`, 다른 사용자의 홈 디렉터리를 읽지 못합니다. 이 점검은 `pass`가 아니라 `skip`으로 나옵니다.

방화벽 점검은 `nft list ruleset`을 읽고, nft에 input chain이 없으면 `iptables-save`를 읽습니다. input 규칙의 수는 세지만 규칙 순서는 해석하지 않습니다. 이 호스트만 보므로 클라우드 보안 그룹이 포트를 막고 있을 수 있습니다. `fail`은 호스트 자체가 그 포트를 막지 않는다는 뜻입니다.

SSH 점검은 `sshd_config`와 `Include`한 파일을 읽고, sshd처럼 처음 나온 값을 씁니다. `Match` 블록은 일부 연결에만 적용되므로 해석하지 않습니다. 실제 적용 값은 `sshd -T`로 확인하세요.

이미 확인한 결과를 받아들이려면 `config.toml`에 `[audit]` 표를 넣으세요.

```toml
[audit]
accept = ["net.ip_forward", "user.root_groups"]
allow_ports = [5432]
```

`accept`에는 위 표의 점검 이름을 씁니다. `allow_ports`에 넣은 포트는 `net.exposed`가 판정하지 않고 경고로 알립니다. 없는 점검 이름이나 1~65535 밖의 포트가 있으면 설정이 잘못된 것으로 처리됩니다.

받아들인 결과는 보고서에서 사라지지 않습니다. `skip`으로 나오고 요약 앞에 `받아들임 (fail)` 같은 표시가 붙습니다. 표시는 언어 설정을 따릅니다. JSON에서는 `metrics.accepted`가 `true`이고 `metrics.accepted_status`에 원래 상태가 남습니다. 받아들인 결과는 종료 코드를 바꾸지 않습니다.

`sudo edc audit`로 실행하면 root의 설정 파일(예: `/root/.config/edc/config.toml`)을 읽습니다. `[audit]` 표는 그 파일에 넣으세요.

`edc audit`는 패키지 버전을 CVE와 대조하지 않습니다. 파일 시스템 전체를 훑지 않고, 찾은 문제를 고치지도 않습니다.

## 서비스 보기 (Linux)

`edc svc <unit>`은 systemd service 하나를 한 화면에 보여 줍니다. unit 상태, process, fd, 소켓, journal을 읽기만 하고, unit을 시작·중지·reload하지 않습니다.

```bash
sudo edc svc nginx
sudo edc svc -v nginx.service   # process 행, fd 행, 소켓, journal 줄 추가
sudo edc svc --json - nginx     # 보기마다 결과 하나
```

option은 unit 이름 앞에 둡니다. `nginx`와 `nginx.service`는 같은 unit입니다. 이름에는 영문자, 숫자, `:_.@-`만 쓸 수 있고, `.timer`나 `.socket` 같은 다른 unit 종류는 받지 않습니다.

| 결과 | pass | warn | fail |
|---|---|---|---|
| `svc.state` | unit이 active이고 재시작이 없음 | active지만 재시작했거나, 다른 상태임 | unit이 failed이거나 systemd가 unit을 불러오지 못함 |
| `svc.processes` | unit cgroup의 process를 CPU%, RSS와 함께 보여 줌 | | |
| `svc.fd` | 모든 process가 soft fd 한계의 80% 미만을 씀 | 한계의 80% 이상을 쓰는 process가 있음 | |
| `svc.listen` | unit process가 가진 TCP·UDP 소켓을 보여 줌 | | |
| `svc.journal` | 이 unit의 journal에 오류 줄이 없음 | 오류 줄이 있고, 마지막 5줄을 보여 줌 | |

process 목록은 main process를 맨 앞에 두고 최대 10행까지 보여 줍니다. 개수와 RSS는 unit cgroup의 모든 process를 셉니다.

root로 실행하세요. root가 아니면 다른 사용자의 fd, 소켓 주인, 전체 journal을 읽지 못합니다. 이 결과는 `pass`가 아니라 `skip`으로 나옵니다. process가 없는 unit은 process 관련 결과가 `skip`입니다.

systemd만 쥔 소켓(socket activation)은 unit process의 것이 아니어서 보여 주지 않습니다.

macOS나 systemd가 없는 Linux에서는 `skip` 하나를 내고 0으로 끝납니다. unit이 없으면 오류를 쓰고 1로 끝납니다. `fail` 결과가 있어도 1, 사용법 오류는 2입니다.

### 서비스 목록

unit 없이 `edc svc`를 실행하면 service 목록을 보여 줍니다. 실패한 service가 먼저 오고, 그다음 시작 중이거나 재시작을 기다리는 service, 재시작 기록이 있는 service 순입니다. 실행 중이 아닌 service는 `--all`을 줄 때만 보입니다.

```bash
edc svc               # 목록. 터미널에서는 화면으로 열림
edc svc --all         # 실행 중이 아닌 service도 포함
edc svc --json -      # 목록이 metrics.services에 들어감
```

터미널에서는 목록이 2초마다 갱신됩니다. `↑`/`↓`로 고르고, `Enter`로 열고, `Esc`로 돌아옵니다. `/`는 이름 필터, `f`는 실패한 service만 보기, `a`는 `--all` 전환, `r`은 목록 다시 읽기, `q`는 종료입니다. 연 service에는 `edc svc -v <unit>`과 같은 결과가 나오고, `r`로 다시 읽습니다.

pipe나 `--json`에서는 `svc.list` 결과 하나와 표를 출력합니다. 실패한 service가 있으면 결과는 `fail`, exit code는 1입니다. 시작 중이거나 재시작을 기다리는 service가 있으면 `warn`입니다. 목록은 root 없이 읽지만, 연 service는 위와 같이 root가 필요합니다.

## Container 보기 (Linux)

`edc ctr <container>`는 docker container 하나를 한 화면에 보여 줍니다. `edc svc`가 unit을 보여 주는 것과 같습니다. 상태는 `docker inspect`에 묻고, container의 process, fd, 소켓, cgroup 한도를 읽고, `docker logs`의 마지막 줄을 읽습니다. container를 시작·중지·재시작하지 않습니다.

```bash
sudo edc ctr web
sudo edc ctr -v 0123456789ab      # process 행, fd 행, 소켓, 로그 줄 추가
sudo edc ctr --json - web         # 보기마다 결과 하나
```

이름이나 ID에는 영문자, 숫자, `_.-/`만 쓸 수 있습니다. option은 이름 앞이나 뒤에 둡니다.

| 결과 | pass | warn | fail |
|---|---|---|---|
| `ctr.state` | container가 실행 중이고 재시작과 OOM kill이 없음 | 실행 중이지만 재시작했거나, OOM kill됐거나, unhealthy이거나, paused·created·restarting 상태임. unhealthy이면 마지막으로 실패한 health check의 출력을 warning으로 보여 줌 | container가 exited이거나 dead임. 요약에 종료 코드를 보여 줌. `on-failure` 한도에 닿아 더 재시작하지 않으면 warning으로 알림 |
| `ctr.image` | 이미지 참조가 container가 실행 중인 이미지를 가리키거나, 그 참조가 이 호스트에 없음 | 이미지 참조가 다른 이미지를 가리킴. 반영하려면 container를 다시 만들어야 함 | |
| `ctr.processes` | container cgroup의 process를 CPU%, RSS와 함께 보여 줌 | | |
| `ctr.limits` | memory, OOM kill, CPU throttle, pids가 한도 아래임 | memory 사용량이 한도의 90% 이상, 커널이 container의 process를 메모리 부족으로 죽임, CPU 한도가 period의 25% 이상을 throttle함, pids가 한도의 90% 이상 | |
| `ctr.fd` | 모든 process가 soft fd 한계의 80% 미만을 씀 | 한계의 80% 이상을 쓰는 process가 있음 | |
| `ctr.listen` | container process가 가진 TCP·UDP 소켓을 보여 주고, publish한 port마다 listen하는 process가 있음 | publish한 port를 container 안에서 listen하는 process가 없거나, loopback 주소에서만 listen함 | |
| `ctr.log` | 마지막 로그 50줄에 오류 줄이 없음 | 오류 줄이 있고, 마지막 5줄을 보여 줌 | |
| `ctr.disk` | log 파일과 writable layer의 크기를 보여 줌 | json-file log에 `max-size`가 없고 1 GiB 이상이거나, writable layer가 10 GiB 이상임 | |

`ctr.state` 요약에는 재시작 정책이 함께 나옵니다(예: `재시작 1번(on-failure:5)`). `-v`를 주면 docker가 남긴 health check를 exit code, 출력 첫 줄과 함께 보여 줍니다. health check 기록은 healthcheck가 있는 container에서만 읽습니다.

`ctr.image`는 container의 이미지 참조가 지금 가리키는 이미지를 `docker image inspect`로 읽습니다. `docker pull`이나 build는 tag를 새 이미지로 옮길 수 있지만, 실행 중인 container는 다시 만들기 전까지 예전 이미지로 돕니다.

`ctr.limits`는 container의 cgroup v2를 읽습니다. `memory.current`와 `memory.max`, OOM kill은 `memory.events.local`, throttle된 period는 container가 시작된 뒤의 `cpu.stat`, 그리고 `pids.current`와 `pids.max`입니다. 오류 줄은 `error`, `fatal`, `panic`, `exception`, `traceback`, `oom`이 든 줄입니다. 로그는 `docker logs --tail 50`으로 읽으므로, 다른 곳에 쓰는 container는 오류 줄이 보이지 않습니다.

`ctr.listen`은 container의 network namespace에서 소켓을 읽습니다. docker는 publish한 port로 들어온 연결을 container 주소로 넘기므로, container 안에서 `127.0.0.1`이나 `::1`에서만 listen하는 process에는 연결이 닿지 않습니다. 막 시작해 아직 listen하지 않은 container도 이 warn을 낼 수 있습니다.

`ctr.disk`는 writable layer 크기와 log option을 `docker inspect --size`에 묻고, log 파일 크기를 읽습니다. `max-size`가 없는 json-file log는 rotation하지 않습니다. writable layer의 데이터는 container를 다시 만들면 사라집니다. root가 아니면 log 파일을 읽지 못해 log 크기가 `-`로 나옵니다.

root로 실행하거나, docker 그룹으로 실행하고 fd는 root로 읽으세요. docker daemon에 접근하지 못하면 `skip` 하나를 냅니다. root가 아니면 다른 사용자의 fd와 소켓 주인을 읽지 못해 그 결과가 `skip`으로 나옵니다. 멈춘 container는 process 관련 결과가 `skip`이고 로그는 그대로 보여 줍니다. `docker` 명령과 cgroup v2가 필요합니다. macOS에서는 `skip` 하나를 내고 0으로 끝납니다. container가 없으면 오류를 쓰고 1로 끝납니다.

### Container 목록

container 없이 `edc ctr`를 실행하면 container 목록을 보여 줍니다. 실패한 container가 먼저 옵니다. 0이 아닌 코드로 끝났거나, OOM으로 죽었거나, `dead` 상태인 container입니다. 그다음 재시작 중, 일시 중지, unhealthy, 생성만 된 container가 오고, 그다음 재시작 기록이 있는 container가 옵니다. 코드 0으로 끝난 container는 `--all`을 줄 때만 보입니다.

```bash
sudo edc ctr          # 목록. 터미널에서는 화면으로 열림
sudo edc ctr --all    # 코드 0으로 끝난 container도 포함
sudo edc ctr --json - # 목록이 metrics.containers에 들어감
```

container가 몇 개든 docker를 두 번만 부르고, 실행 중인 container의 메모리는 cgroup에서 읽습니다. 터미널 화면의 키는 `edc svc`와 같습니다. `Enter`를 누르면 `edc ctr -v <container>`와 같은 결과가 나오고, `Esc`로 돌아옵니다. 80열에서는 `IMAGE` 열이 먼저 빠집니다.

pipe나 `--json`에서는 `ctr.list` 결과 하나와 표를 출력합니다. 실패한 container가 있으면 결과는 `fail`, exit code는 1입니다. 재시작 중, 일시 중지, unhealthy, 생성만 된 container가 있으면 `warn`입니다. docker daemon에 접근하지 못하면 목록은 `skip` 하나를 냅니다.

## 최근 호스트 변경 (Linux)

`edc changes`는 지정한 시간 범위 안에서 Linux 호스트에 일어난 변경을 보여 줍니다. 패키지 로그, `/etc` 아래 파일의 mtime, systemd unit 상태만 읽고, 아무것도 바꾸지 않으며 외부로 보내지 않습니다. 1초 안에 끝납니다.

```bash
edc changes                     # 최근 24시간
edc changes --since 2h          # 최근 2시간
edc changes -v                  # 최근 50건에서 자르지 않고 모두
edc changes --json -            # 모든 변경을 metrics.events에 담음
```

원천마다 결과 한 줄을 먼저 출력하고, 이어서 모든 변경을 합친 표를 최근 것부터 출력합니다.

| 결과 | 원천 |
|---|---|
| `changes.packages` | `/var/log/dpkg.log`, `/var/log/dnf.rpm.log`, `/var/log/yum.log`와 압축되지 않은 회전 로그 |
| `changes.etc` | `/etc` 아래 파일과 symlink의 mtime |
| `changes.services` | service unit마다 마지막 시작 시각과 재시작 횟수 |

부팅이 시간 범위 안에 있으면 표에 `boot` 한 줄이 나옵니다. 부팅 중에 시작한 service는 이 줄로 묶습니다.

원천을 읽지 못하면 결과는 이유와 함께 `skip`입니다. `skip`은 바뀐 것이 없다는 뜻이 아닙니다. 모든 원천을 읽으려면 root로 실행하세요. root가 아니면 `/etc` 아래 일부 디렉터리와 일부 패키지 로그를 읽지 못합니다.

`edc changes`는 회전된 압축 로그를 풀지 않습니다. 압축 로그가 시간 범위 시작보다 새로우면 경고로 알립니다. `/etc`는 파일 내용이 아니라 mtime만 읽으므로 파일 안에서 무엇이 바뀌었는지는 보이지 않습니다. systemd는 unit마다 시작 시각을 하나만 남기므로, 여러 번 재시작한 unit은 마지막 시작 시각과 재시작 횟수로 보입니다. `/etc`는 항목 20000개에서 멈추고 `warn`으로 알립니다. `dnf history`와 `journalctl`은 실행하지 않습니다.

## 디렉터리 디스크 사용량 (Linux)

`edc du`는 디렉터리 아래의 디스크 사용량을 보여 줍니다. `du`처럼 할당된 block을 세고, hard link가 여러 개인 파일은 한 번만 셉니다. 아무것도 바꾸지 않으며 외부로 보내지 않습니다.

```bash
edc du                          # 현재 디렉터리
edc du /var                     # /var 아래에서 큰 항목
edc du --top 40 /var            # 40줄
edc du --workers 1 /var         # 디렉터리를 하나씩 읽음
edc du --cross-mounts /var      # /var 아래의 마운트도 셈
edc du --json - /var            # 항목을 metrics.rows에 담음
```

터미널에서는 탐색 화면을 엽니다. 읽는 동안에는 읽은 디렉터리 수를 보여 주고, `q`를 누르면 멈추고 그때까지 읽은 결과를 보여 주며, `Ctrl-C`는 멈춘 뒤 화면을 닫습니다. 화면은 지금 디렉터리의 항목을 큰 것부터 나열합니다. `Enter`나 `→`로 디렉터리를 열고, `←`, `Backspace`, `Esc`로 위로 올라갑니다. `↑`, `↓`, `PgUp`, `PgDn`, `g`, `G`로 커서를 옮기고, `q`로 화면을 닫으면 결과를 출력합니다. 화면에서는 파일을 바꾸거나 지우지 않습니다. pipe로 넘기거나 `--json`을 쓰거나 `NO_COLOR`를 설정하면 화면 없이 결과만 출력합니다. 화면을 열 때는 가장 큰 파일 10,000개를 남기고, 남기지 않은 파일은 디렉터리마다 `(N other files)` 한 줄로 보여 줍니다.

출력은 `du.scan` 결과 한 줄로 시작합니다. 이 줄에는 전체 크기, 항목 수, 걸린 시간, worker 수가 있고, 그 아래에 항목이 이어집니다. edc는 가장 큰 항목부터 고르고, 디렉터리를 고르면 그 안의 항목도 후보에 올립니다. 디렉터리 줄은 `/`로 끝나고, 마지막 줄은 보이지 않은 나머지 항목의 합입니다. 화면을 닫은 뒤의 출력은 터미널 높이에 맞춰 줄 수를 정하고, pipe로 넘기면 20줄을 보여 줍니다. `--top`은 화면이 아니라 이 출력의 줄 수를 정합니다.

기본으로 한 파일 시스템 안에서만 셉니다. 경로 아래의 마운트에는 들어가지 않고, 건너뛴 마운트마다 파일 시스템 종류와 `statfs`가 알려 준 사용량을 보여 줍니다. bind mount와 overlay mount는 다른 경로와 파일 시스템을 공유하므로 사용량을 `-`로 표시합니다. 디렉터리를 자기 자신 위에 bind mount한 경우에는 그 파일이 다른 곳에 없으므로 건너뛰지 않고 셉니다.

바쁜 호스트에서도 CPU를 적게 쓰도록 기본으로 디렉터리 4개를 동시에 읽습니다. `--workers`나 `-w`로 이 수를 바꿀 수 있습니다. 디렉터리마다 기록 하나와 가장 큰 파일 몇 개만 남기므로, 메모리 사용량은 파일 수가 아니라 디렉터리 수에 따라 늘어납니다.

제한 시간(기본 60초)이 지나면 탐색을 멈춥니다. 이때 결과는 `warn`이고 `metrics.partial`은 `true`이며, 크기는 읽은 디렉터리만 센 값입니다. 제한 시간이나 `q`로 멈추면 텍스트 출력의 마지막 줄에도 부분 결과라고 표시하고, 제한 시간에 멈췄을 때는 `--timeout`을 늘린 명령을 함께 보여 줍니다. 읽지 못한 디렉터리가 있으면 결과는 `warn`이고 `-v`로 그 목록을 볼 수 있습니다. 모든 디렉터리를 읽으려면 root로 실행합니다. macOS에서는 결과가 `skip`입니다.

항목 아래에는 경로가 속한 파일 시스템을 보여 줍니다. 마운트 지점, 종류, `df`와 같은 사용량과 전체 크기가 나옵니다. 그 파일 시스템에서 지워졌지만 process가 아직 열고 있는 파일도 보여 줍니다. 이 파일은 `df`에는 잡히지만 `du`로는 보이지 않습니다. 줄마다 크기, process, 지우기 전 경로가 나오며, 그 process를 재시작하거나 파일을 다시 열게 하면 공간이 돌아옵니다. 이 값은 경로 아래만이 아니라 파일 시스템 전체 기준입니다. root가 아니면 모든 process의 파일을 읽지 못하므로 값 앞에 `≥`를 붙입니다. 지워졌지만 열린 파일이 파일 시스템의 10% 이상이면 결과는 `warn`입니다.

경로가 파일 시스템 전체의 마운트 지점이면 숫자를 맞춰 봅니다. `not found`는 사용량에서 센 크기와 지워졌지만 열린 파일을 뺀 값이고, 마운트 아래에 가려진 파일이나 파일 시스템 metadata가 이 공간을 쓸 수 있습니다. 모든 디렉터리와 process를 읽었고 제한 시간에 멈추지 않았을 때만 계산합니다. bind mount나 하위 디렉터리를 재면 이 비교는 나오지 않습니다. JSON에서는 `metrics.filesystem`, `metrics.deleted_open`, `metrics.not_found_bytes`에 이 값이 들어갑니다.

어느 디렉터리가 늘었는지 보려면 두 번의 실행을 `--json`으로 저장하고 `edc report diff`로 비교합니다. `metrics.largest_dirs`에는 깊이와 관계없이 가장 큰 디렉터리 200개가 들어갑니다. `report diff`는 이 목록을 경로로 맞춰 변화가 가장 큰 디렉터리를 최대 10개 보여 줍니다. 하위 디렉터리 하나가 상위 디렉터리 변화의 90% 이상을 차지하면 상위 대신 그 하위 디렉터리를 보여 줍니다. 한쪽 목록에만 있는 디렉터리는 반대쪽 값을 그 목록의 가장 작은 크기 이하(`≤`)로 표시합니다. 같은 경로를 끝까지 읽은 두 결과만 비교합니다. hard link 이름이 두 디렉터리에 있는 파일은 먼저 읽은 디렉터리에 세므로, 실행마다 그 두 디렉터리 사이에서 크기가 옮겨 갈 수 있습니다.

`PATH`에 `docker`가 있으면 `docker ps`와 `docker inspect`로 container 목록도 읽습니다. 형식은 container마다 이름, 로그 경로, 마운트, layer 경로만 읽습니다. 이 정보로 로그 디렉터리, writable layer, 실행 중인 container의 root 파일 시스템, volume, bind mount 원본에 `volume pgdata (db-1 사용)`처럼 container 이름을 붙입니다. JSON에서는 `label`에 들어갑니다. containerd image store를 쓰면 멈춘 container의 writable layer는 마운트되어 있지 않아 이름이 붙지 않습니다. docker가 3초 안에 답하지 않으면 이름 없이 출력합니다.

## 알려진 개발 캐시 사용량 (Linux와 macOS)

`edc clean`은 알려진 개발 캐시의 디스크 사용량을 보여 줍니다. 읽기만 하며 파일을 지우거나 바꾸지 않습니다.

```bash
edc clean                       # 알려진 캐시를 잼
edc clean -v                    # 모든 후보, 더 많은 묶음, 이유, 정리 명령 추가
edc clean --timeout 2m          # 제한 시간을 늘림
edc clean --project ~/work      # ~/work에서 빌드 폴더도 찾음
edc clean --project ~/a --project ~/b   # 루트를 더 지정하려면 옵션을 반복
edc clean --json -              # 후보를 metrics.candidates에 담음
```

후보의 위치는 세 가지 방법 중 하나로 찾습니다. `go-build`, `go-mod`, `npm`, `pnpm`, `uv`, `homebrew`, `pip`는 그 캐시를 가진 도구에 물어서 찾습니다. 도구가 `PATH`에 없으면 상태는 `tool_absent`입니다.

나머지 후보는 표의 순서대로 환경변수를 읽습니다. 처음으로 설정된 변수가 경로를 정하고, 변수가 하나도 없으면 기본 경로를 씁니다. `located_by`에 어떤 방법을 썼는지 나옵니다.

처음으로 설정된 변수가 비었거나 절대 경로가 아니면 상태는 `unknown_location`이고 이유는 `invalid_env`입니다. 다음 변수나 기본 경로로 넘어가지 않습니다.

`bunfig.toml`, `.npmrc`, `.puppeteerrc` 같은 도구 설정 파일은 읽지 않습니다. 설정 파일로 캐시를 옮겼다면 환경변수를 지정하십시오.

| 후보 | 위치를 찾는 방법 |
| --- | --- |
| `go-build`, `go-mod` | `go env GOCACHE GOMODCACHE` |
| `npm` | `npm config get cache` |
| `pnpm` | `pnpm store path` |
| `uv` | `uv cache dir` |
| `homebrew` | `brew --cache` |
| `pip` | `pip3 cache dir` |
| `xcode-derived-data` (macOS) | `~/Library/Developer/Xcode/DerivedData` |
| `swiftpm` (macOS) | `~/Library/Caches/org.swift.swiftpm` |
| `cargo-registry`, `cargo-git` | `$CARGO_HOME/registry`, `$CARGO_HOME/git`, 없으면 `~/.cargo/registry`, `~/.cargo/git` |
| `zig` | `$ZIG_GLOBAL_CACHE_DIR`, `$XDG_CACHE_HOME/zig`, 없으면 `~/.cache/zig` |
| `bun` | `$BUN_INSTALL_CACHE_DIR`, `$BUN_INSTALL/install/cache`, `$XDG_CACHE_HOME/.bun/install/cache`, 없으면 `~/.bun/install/cache` |
| `playwright` | `$PLAYWRIGHT_BROWSERS_PATH`, 없으면 `~/Library/Caches/ms-playwright` (macOS) 또는 `$XDG_CACHE_HOME/ms-playwright`, `~/.cache/ms-playwright` (Linux) |
| `puppeteer` | `$PUPPETEER_CACHE_DIR`, 없으면 `~/.cache/puppeteer` |
| `node-gyp` | `$npm_config_devdir`, 없으면 `~/Library/Caches/node-gyp` (macOS) 또는 `$XDG_CACHE_HOME/node-gyp`, `~/.cache/node-gyp` (Linux) |
| `electron` (macOS) | `$electron_config_cache`, 없으면 `~/Library/Caches/electron` |
| `xcode-device-support` (macOS) | `~/Library/Developer/Xcode`의 `*DeviceSupport` 폴더 |
| `coresimulator-caches` (macOS) | `~/Library/Developer/CoreSimulator/Caches` |
| `app-updates` (macOS) | `~/Library/Caches`의 `*.ShipIt` 폴더 |
| `app-logs` (macOS) | `~/Library/Logs` |
| `huggingface` | `$HF_HUB_CACHE`, `$HUGGINGFACE_HUB_CACHE`, `$HF_HOME/hub`, `$XDG_CACHE_HOME/huggingface/hub`, 없으면 `~/.cache/huggingface/hub` |
| `rustup-toolchains` | `$RUSTUP_HOME/toolchains`, 없으면 `~/.rustup/toolchains` |
| `ollama` (macOS) | `$OLLAMA_MODELS`, 없으면 `~/.ollama/models` |
| `app-caches` (macOS) | `~/Library/Caches`의 나머지 폴더 |
| `dot-cache` (macOS) | `~/.cache`의 나머지 폴더 |

Linux에서는 표에 macOS라고 적지 않은 후보만 잽니다. Linux의 `~/.cache` 나머지 폴더는 재지 않습니다.

후보마다 우선순위가 있습니다. P1은 도구 캐시, P2는 내려받은 브라우저·바이너리, Xcode 부가 데이터, 앱 캐시, 로그이고, P3는 모델과 툴체인입니다. 텍스트 출력은 P1부터 나열하고, 같은 우선순위 안에서는 큰 후보부터 보여 줍니다.

크기는 `edc du`의 측정 코드를 재사용해 후보를 하나씩 잽니다. 경로 아래의 마운트에는 들어가지 않습니다. symlink 경로는 보고된 경로와 실제 경로를 함께 보여 주고 실제 경로를 잽니다. DerivedData는 폴더를 앱 이름별로, `rustup-toolchains`와 `app-logs`는 폴더 이름별로 묶어 보여 줍니다.

`xcode-device-support`, `app-updates`, `app-caches`, `dot-cache` 네 후보는 폴더의 1단계 항목을 하나씩 잽니다. 앞의 둘은 이름이 `DeviceSupport` 또는 `.ShipIt`으로 끝나는 항목만 잽니다. 그런 이름의 항목이 없으면 상태는 `absent`입니다.

`app-caches`와 `dot-cache`는 나머지 항목을 모두 잽니다. 앞선 후보가 잰 경로는 건너뛰고, 그 경로를 품은 항목은 품지 않은 부분만 잽니다. 건너뛴 후보는 `excludes`에 나옵니다. 결과는 항목 이름별 묶음입니다.

이 네 후보는 symlink를 따라가지 않습니다. 다른 장치에 있는 항목은 재지 않고 결과를 `partial`로 표시합니다. hard link는 edc가 재는 항목마다 한 번씩 셉니다.

앱이 폴더에 무엇을 두는지는 edc가 알 수 없습니다. `app-caches`, `dot-cache`, `app-updates`, `app-logs`는 `regenerable`이 `false`이고 다시 만드는 비용은 `unknown` 또는 `lost`입니다.

도구의 캐시 폴더만 잽니다. Hugging Face는 `hub`, Cargo는 `registry`와 `git`, Ollama는 `models`입니다. 토큰이나 자격 증명 파일은 재지 않습니다.

Xcode Archives, 시뮬레이터 기기, `Application Support`, `Containers`는 캐시로 분류하지 않습니다.

후보마다 상태가 하나 있습니다.

| 상태 | 뜻 |
| --- | --- |
| `measured` | 경로 전체를 읽었습니다. 크기가 완전합니다. |
| `partial` | 크기가 하한입니다. 읽지 못한 디렉터리가 있거나, 마운트를 건너뛰었거나, 시간이 끝났습니다. |
| `absent` | 경로가 없거나, 후보가 찾는 이름의 항목이 없습니다. |
| `tool_absent` | 도구가 `PATH`에 없습니다. |
| `unknown_location` | 도구가 실패했거나, 시간이 초과됐거나, 예상과 다른 답을 냈습니다. 환경변수가 절대 경로가 아니어도 이 상태입니다. |
| `unreadable` | 권한 오류 등으로 읽지 못했습니다. |
| `rejected` | 경로가 `/`, 홈 디렉터리, 그 상위를 덮습니다. 재지 않습니다. |
| `duplicate` | 앞선 후보와 실제 경로가 같거나 앞선 후보가 이 경로를 품고 있습니다. |
| `not_scanned` | edc가 닿기 전에 시간이 끝났거나 `Ctrl-C`를 눌렀습니다. |

`usage_bytes`는 `measured`와 `partial`에만 있고, 다른 상태에서는 `0 B`를 표시하지 않습니다. 다른 후보 안에 있는 후보는 합계에 두 번 더하지 않습니다.

텍스트 출력은 크기가 있거나 확인이 필요한 후보만 한 줄씩 보여 줍니다. `absent`, `tool_absent`, `duplicate`는 상태별로 한 줄에 접습니다. 묶음은 기본 3개를 보여 주고, `-v`는 모든 후보와 묶음 30개를 보여 줍니다. `--json`은 모두 보여 줍니다. 텍스트 출력은 홈 디렉터리 아래 경로를 `~`로 줄여 쓰고, JSON에는 전체 경로가 있습니다.

파일 시스템별 합계는 사용량이며, 정리하면 비워지는 양이 아닙니다. 텍스트 출력은 이 합계를 맨 끝에 보여 줍니다. APFS clone과 snapshot 때문에 합계가 비워지는 양보다 클 수 있습니다. hard link는 후보 하나 안에서는 한 번만 세지만, 두 후보에 걸친 hard link는 두 번 셉니다. pnpm 저장소와 uv 캐시가 hard link를 씁니다. 후보에는 회수 가능량이 없고, Docker 섹션만 엔진이 보고한 회수 가능량을 보여 줍니다.

정리 명령은 도구가 문서로 제공할 때만 보여 줍니다. `bun`, `pip`, `huggingface`, `rustup-toolchains`는 명령을 보여 주고, `cargo-registry`, `zig`, `app-caches` 등은 보여 주지 않습니다. edc는 명령을 실행하지 않습니다.

후보 뒤에 섹션 네 개가 더 있습니다. 섹션마다 `metrics`에 따로 키가 있고, 섹션은 후보가 아닙니다.

| `metrics` 키 | 내용 |
| --- | --- |
| `candidates` | 위 표의 캐시 |
| `filesystems` | 파일 시스템별 사용량 합계 |
| `project_build_dirs` | `--project`로 지정한 루트의 빌드 폴더 |
| `docker` | Docker 엔진이 보고한 집계 |
| `docker_root` | 엔진이 더 이상 쓰지 않는 옛 Docker 데이터 디렉터리(Linux) |
| `vm_disks` | 가상 머신의 디스크 파일(예: OrbStack) |
| `worktrees` | `~/.gk/worktree`의 git-kit worktree |
| `not_measured` | 재지 않는 유형과 이유 |

섹션은 후보, 빌드 폴더, Docker, 옛 Docker 데이터, VM 디스크, worktree 순서로 실행하고 제한 시간 하나를 나눠 씁니다.

파일 시스템 합계에는 다시 만들 수 있는 빌드 폴더만 더합니다. 엔진 집계, 옛 Docker 데이터, VM 디스크, worktree는 어떤 합계에도 더하지 않습니다.

### 프로젝트 빌드 폴더

`--project DIR`은 찾을 루트를 지정합니다. 루트를 더 지정하려면 옵션을 반복합니다. `--project`가 없으면 찾지 않고 `project_build_dirs`의 상태는 `no_project_root`입니다. 이 상태는 정상입니다.

루트 아래 폴더를 깊이 6까지 읽습니다. symlink는 따라가지 않고, 다른 장치의 마운트에는 들어가지 않습니다. `.git`, 찾은 빌드 폴더, 후보가 재는 경로 아래로도 내려가지 않습니다.

폴더 300,000개 또는 빌드 폴더 256개에서 멈추고, 섹션은 이유 `project_limit`인 `partial`입니다. 닿지 못한 루트의 상태는 `not_scanned`입니다.

없는 루트의 상태는 `absent`, 파일인 루트는 `rejected`입니다. 같은 루트나 다른 루트 안의 루트는 한 번만 걷고 상태는 `duplicate`입니다.

빌드 폴더는 같은 부모 폴더에 표지 파일이 있어야 합니다.

| 폴더 | 표지 파일 |
| --- | --- |
| `.zig-cache`, `zig-out` | `build.zig` |
| `target` | `Cargo.toml` |
| `node_modules` | `package.json` |
| `.build` | `Package.swift` |
| `build` | `build.gradle` 또는 `build.gradle.kts` |

이름만 같고 표지 파일이 없는 폴더는 소스일 수 있어 재지 않고 루트의 `unmarked`로 셉니다.

표지가 있는 폴더마다 git에 물어봅니다. git 저장소 밖의 폴더는 표지 파일만으로 다시 만들 수 있다고 봅니다. `git` 필드는 `none`입니다.

저장소 안에서는 폴더에 추적 파일이 없어야 합니다. git이 추적하는 파일이 있으면 `regenerable`은 `false`이고 이유는 `tracked_files`입니다. 무시 규칙이 있어도 추적 파일이 먼저입니다. `git` 필드는 `ignored` 또는 `not_ignored`입니다.

git이 실패하거나 시간이 초과되면 `regenerable`은 `false`, 이유는 `git_unknown`, `git` 필드는 `unknown`입니다. 추측하지 않습니다.

```json
{
  "kind": "cargo_target",
  "path": "/Users/me/work/app/target",
  "marker": "Cargo.toml",
  "git": "ignored",
  "regenerable": true,
  "status": "measured",
  "usage_bytes": 4831838208
}
```

`regenerable`이 `false`인 빌드 폴더는 합계에 넣지 않습니다. 후보와 경로가 같은 빌드 폴더의 상태는 `duplicate`이고, 후보 안에 있는 빌드 폴더는 `inside`를 표시해 두 번 세지 않습니다.

worktree 안의 빌드 폴더에는 `inside_worktree`가 있고, worktree 항목의 `build_dirs`에 그 수가 나옵니다. 빌드 폴더를 찾지 않았으면 `build_dirs`는 `null`입니다.

텍스트 출력은 큰 빌드 폴더 10개와 나머지 수를 보여 주고 `-v`는 모두 보여 줍니다. 루트마다 읽은 폴더 수와 `unmarked` 폴더 수가 나옵니다.

### Docker 엔진

엔진에는 API socket으로 `GET`만 보냅니다. 다른 메서드는 보내지 않고 `prune` 명령도 실행하지 않습니다.

socket 경로는 `docker context inspect`에 물어서 `DOCKER_HOST`와 현재 context가 반영됩니다. `unix://` 끝점만 지원하고, 다른 끝점은 `unsupported`입니다.

`docker`가 `PATH`에 없으면 상태는 `tool_absent`, socket 파일이 없으면 `not_running`입니다. 두 상태는 정상으로 봅니다.

엔진은 API 1.52 이상이어야 합니다. 더 낮은 버전은 이유 `api_version`인 `unsupported`이고, 목록에서 집계를 계산하지 않습니다.

`types`에는 `images`, `containers`, `build_cache`, `volumes`가 하나씩 있고, 각각 `total_bytes`, `reclaimable_bytes`, `total_count`, `active_count`를 담습니다. 값은 엔진이 보고한 그대로입니다.

요청 하나가 실패하면 그 유형만 `failed`이고 섹션은 `partial`입니다. 볼륨 요청은 최대 20초까지 기다립니다.

`reclaimable_bytes`는 정리하면 비워진다고 엔진이 알려 준 양이며, 출력에서 회수 가능량을 내는 값은 이것뿐입니다. 정리 명령(예: `docker image prune -a`)은 문자열로만 보여 주고 실행하지 않습니다.

볼륨은 사용자 데이터입니다. `volumes` 항목은 `user_data`가 `true`이고 `reclaimable_bytes`와 정리 명령이 없습니다.

`docker system df`는 크기를 반올림하고 1000바이트 단위를 씁니다. edc 텍스트 출력은 1024바이트 단위를 써서 두 수치가 다릅니다. JSON에는 정확한 바이트 수가 있습니다.

### 옛 Docker 데이터 디렉터리

Linux에서는 `/var/lib/docker`를 확인합니다. 엔진이 다른 데이터 디렉터리를 쓰면 이 디렉터리가 옛 데이터 후보입니다. 엔진이 쓰는 디렉터리는 `docker.root_dir`에 있고, API 1.52보다 낮은 엔진에서도 읽습니다.

edc는 지워도 되는지 판정하지 않고, 판단에 도움이 되는 사실만 보여 줍니다. 파일을 읽기만 하며 `docker` 명령은 실행하지 않습니다.

`docker_root.roots`의 항목마다 아래 사실이 있습니다.

- `usage_bytes`, `dirs`: 디렉터리 전체와 안의 폴더별 크기
- `mounted`, `open_files`: 디렉터리 아래에 마운트나 열린 파일이 있는지. `open_files_complete`가 `false`이면 읽지 못한 process가 있습니다.
- `last_container_stop`, `newest_volume_write`: 마지막으로 활동한 날짜
- `containers`: 디렉터리에 기록된 컨테이너의 이미지, Compose 프로젝트, 종료 시각. `in_engine`, `project_in_engine`은 엔진에 같은 이름이 있는지 나타냅니다.
- `volumes`: 볼륨별 크기, 가장 최근 파일의 날짜, 그 볼륨을 마운트한 컨테이너 `used_by`. `in_engine`은 엔진에 같은 이름의 볼륨이 있는지 나타냅니다.

항목에는 디렉터리를 지우는 `rm -rf --one-file-system` 명령이 `cleanup`으로 있고, `-v`에서 텍스트로 보여 줍니다. edc는 이 명령을 실행하지 않습니다. 디렉터리 아래에 마운트나 열린 파일이 있거나, 마운트 표나 `/proc` 전체를 읽지 못하면 `cleanup`은 비어 있습니다.

이 볼륨에는 `docker volume rm`을 쓰지 마세요. 엔진은 옛 디렉터리를 모르므로, 자기 디렉터리에 있는 같은 이름의 볼륨을 지웁니다.

`used_by`가 빈 볼륨은 이 볼륨을 쓴 컨테이너 기록이 없는 것이고, 사용자 데이터일 수 있습니다. 지우기 전에 내용을 확인하세요.

볼륨은 큰 순서로 20개까지 보이고, 나머지는 `volumes_omitted`와 `omitted_bytes`로 셉니다. `-v`가 없으면 텍스트 출력은 이름 없는 볼륨을 한 줄로 묶고 컨테이너 목록을 내지 않습니다.

컨테이너의 이름, 이미지, 라벨, 마운트, 시각은 `containers/*/config.v2.json`에서 읽습니다. 환경 변수는 담지도 출력하지도 않습니다.

엔진이 데이터 디렉터리를 알려 주지 않으면 상태는 `unknown_location`입니다. `docker`가 없거나 엔진이 꺼져 있으면 텍스트 출력에는 이 섹션이 나오지 않고 `docker_root.status`는 비어 있습니다.

### VM 디스크

macOS에서는 `~/Library/Group Containers/HUAQ24HBR6.dev.orbstack/data`의 OrbStack 데이터 디스크를 잽니다. 이 파일은 sparse 파일이라 논리 크기가 아니라 할당된 block을 잽니다.

이 디스크에는 Docker 엔진 데이터와 Linux 머신이 들어 있습니다. 엔진이 OrbStack이면 `docker.inside`가 `orbstack`입니다.

엔진 집계와 디스크 크기는 같은 데이터를 다른 방법으로 본 값이라 서로 더하지 않고 어떤 합계에도 넣지 않습니다. 디스크는 사용자 데이터이고 `user_data`가 `true`입니다.

Docker Desktop이나 다른 VM의 디스크는 재지 않습니다. Linux에는 VM 디스크 항목이 없습니다.

### worktree

git-kit의 폴더인 `~/.gk/worktree`를 읽습니다. 깊이 5까지 `.git`이 있는 폴더를 찾고 그 아래로는 내려가지 않습니다. 폴더가 없으면 상태는 `absent`이고 정상입니다.

루트를 한 번 재고 worktree별 크기는 그 트리에서 읽습니다. `leftover`는 루트의 그 밖의 파일 크기입니다.

worktree를 쓰는지는 파일 수정 시각이 아니라 git 상태로 판단합니다. git은 읽기 하위 명령만 `GIT_OPTIONAL_LOCKS=0`과 `core.fsmonitor=false`로 실행해 `.git`의 파일을 바꾸지 않습니다. `git status`는 바뀐 파일을 git-lfs 같은 저장소의 clean filter로 읽으므로 쓰지 않습니다. 대신 파일의 stat을 index와 비교하므로, touch만 한 파일도 바뀐 것으로 셉니다.

worktree마다 `verdict`가 하나 있고, 아래 순서에서 처음 맞는 값을 고릅니다.

| verdict | 뜻 |
| --- | --- |
| `unlinked` | worktree의 git 디렉터리가 없음 |
| `has_changes` | 변경된 파일이나 추적하지 않는 파일이 있음 |
| `state_unknown` | git 명령이 실패해 알 수 없음 |
| `locked` | worktree가 잠겨 있음 |
| `not_merged` | 기준에 없는 커밋이 브랜치에 있음 |
| `base_unknown` | 비교할 기준 브랜치를 찾지 못함 |
| `merged_clean` | 기준에 브랜치가 들어 있고 변경이 없음 |

기준은 git 설정의 브랜치 `gk-parent`이고, 없으면 `origin/HEAD`입니다. `main`이라고 가정하지 않습니다.

`merged_clean`은 git 명령이 모두 성공해야 나옵니다. edc는 worktree를 정리하지 않으며, 실행할 명령으로 `git-kit worktree cleanup`을 보여 줍니다.

### 재지 않는 유형

출력은 재지 않는 유형을 항상 나열하며, `-v`로 유형마다 이유를 볼 수 있습니다.

- OrbStack 외의 VM 디스크, DB
- `~/.gk/worktree` 밖의 worktree
- 휴지통과 다운로드
- 시뮬레이터 기기
- SDK
- 앱 데이터
- 위치를 확인하지 못한 그 밖의 도구 캐시

제한 시간은 기본 60초이며 모든 후보와 섹션이 나눠 씁니다. P1 후보부터 재고 나머지 묶음 두 후보는 마지막에 잽니다. 시작하지 못한 섹션의 상태는 `not_scanned`입니다. 시간이 끝나면 결과는 `warn`이고 `next`에 더 긴 제한 시간의 명령을 보여 줍니다. `Ctrl-C`는 실행을 멈추고 그때까지의 결과를 남깁니다.

## 커널과 systemd 이벤트 (Linux)

`edc events`는 정한 기간의 커널 로그와 systemd journal을 읽어 장애 신호를 시간 순서로 정리합니다. 아무것도 바꾸지 않고 외부로 보내지 않습니다.

```bash
sudo edc events                 # 최근 6시간
sudo edc events --since 48h
sudo edc events -v              # 결과마다 근거가 된 기록과 시간순 기록 추가
sudo edc events --json -        # 범주마다 결과 하나
```

| 결과 | fail | warn |
|---|---|---|
| `events.window` | | |
| `events.oom` | 커널이 메모리 부족으로 프로세스를 죽임 | |
| `events.disk_io` | 디스크나 파일 시스템이 I/O 오류를 보고함 | |
| `events.fs_readonly` | 파일 시스템이 읽기 전용으로 바뀌거나 멈춤 | |
| `events.lockup` | CPU가 soft lockup이나 hard lockup에 머물렀거나, RCU가 stall을 보고했거나, workqueue가 멈춤 | |
| `events.oops` | 커널이 `BUG`, `Oops`, general protection fault, panic을 보고함 | 커널이 `WARNING`을 냈거나, 기간 안에 oops가 없는데 커널이 `D`, `L`, `M`, `B`로 tainted됨 |
| `events.link` | 네트워크 링크가 끊긴 뒤 다시 연결되지 않음 | 링크가 끊겼다가 다시 연결됨 |
| `events.units` | unit의 마지막 기록이 실패 결과임 | unit이 실패한 뒤 재시작했거나 3번 이상 재시작함 |
| `events.failed_units` | 지금 failed 상태인 unit이 있음. 기간 전에 실패한 unit도 포함 | |
| `events.journal_errors` | | 기간 안 journal에 오류 줄(priority `err` 이상)이 있음. 근거는 unit별 개수와 가장 많은 unit 10개의 마지막 줄 |
| `events.segfault` | | 프로세스가 segfault나 general protection fault로 멈춤 |
| `events.hung_task` | | task가 hung task 한도(기본 120초)보다 오래 막힘 |
| `events.clock` | | 벽시계가 1초 이상 점프함 |

`events.window`는 기간과 원천을 밝히고, `-v`를 주면 모든 범주의 최근 신호 50개를 시간 순서로 보여 줍니다. 다른 결과는 횟수와 마지막 기록, 최근 기록 20개까지를 보여 줍니다.

`events.failed_units`는 `systemctl list-units --state=failed`로 지금 상태를 묻습니다. 그래서 이틀 전에 실패해 아직 failed인 unit도 기본 6시간 창에서 보입니다. `events.journal_errors`는 기간 안의 `journalctl -p err`를 읽어 `_SYSTEMD_UNIT`으로, unit이 없는 줄은 syslog 이름으로 묶습니다. 커널 줄은 커널 범주가 이미 읽으므로 뺍니다. 오류 줄은 실패가 아니라 단서라 이 결과는 warn입니다. root나 `adm`, `systemd-journal` 그룹이 아니면 journalctl은 자기 기록만 보여 주므로 결과가 `skip`입니다. systemd가 PID 1이 아닌 호스트에서는 `events.failed_units`가 `skip`입니다.

`events.oops`는 `/proc/sys/kernel/tainted`도 읽습니다. `D`, `W`, `L`, `M`, `B` flag는 부팅 뒤 커널에 oops, warning, soft lockup, machine check, bad page가 있었다는 표시라, 그 시각의 로그가 기간 밖이어도 남습니다. 결과는 flag를 근거로 보여 줍니다. `P`, `O`, `E` 같은 module flag는 근거에만 나오고 결과를 바꾸지 않습니다.

커널 신호는 journal의 커널 기록에서 읽습니다. 그 기간에 journal의 커널 기록이 없으면 `/dev/kmsg`를 읽는데, `/dev/kmsg`에는 현재 부팅의 기록만 있습니다. unit과 시계 신호는 journal의 PID 1 기록에서 읽습니다. 시계 점프는 두 기록 사이에서 벽시계와 monotonic 시계가 1초 이상, 그리고 간격의 0.1%보다 크게 다를 때만 셉니다. 그래서 NTP slew는 세지 않습니다.

root나 `adm`, `systemd-journal` 그룹으로 실행하세요. 권한이 없으면 journalctl은 자기 기록만 보여 주고, `kernel.dmesg_restrict`가 `/dev/kmsg`를 막을 수 있습니다. 이때 결과는 `pass`가 아니라 `skip`으로 나옵니다. journalctl이 없으면 `events.units`와 `events.clock`이 `skip`입니다.

journal 기록은 20000개, journal 출력은 32 MiB, `/dev/kmsg` 기록은 100000개까지 읽습니다. 상한에 닿으면 `events.window`가 경고를 냅니다.

`edc events`는 로그 전체를 보여 주지 않습니다. 새 기록을 따라가지 않고, 애플리케이션 로그도 읽지 않습니다.

## Probe 임계값

`edc tls check`는 인증서 만료가 30일보다 적게 남으면 warning을 냅니다. `--min-days`를 지정하면 그보다 이른 만료를 실패로 처리합니다.

`edc http check`는 4xx 응답에 warning을, 5xx 응답에 실패를 냅니다. `--expect-status`를 지정하면 그 code만 통과하고 나머지는 실패입니다.

대상에 scheme이 없으면 `edc http check`는 `http://`를 붙입니다. `edc http check naver.com`은 `http://naver.com`을 요청하고 redirect를 따라갑니다. `url` metric에는 실제로 요청한 주소가 남습니다. TLS endpoint를 바로 확인하려면 `https://`를 씁니다.

서버가 redirect를 보내면 `edc http check`는 요청마다 status, URL, 시간 값, peer를 차례로 보여 줍니다. `-v`를 붙이면 응답마다 header가, 마지막 응답에는 본문 앞부분이 더해집니다. `Authorization`, `Cookie`, `Set-Cookie` 같은 인증 header의 값은 가립니다. JSON에는 요청마다 기록이 `hops` metric에 남습니다. `edc doctor -v`도 같은 목록을 보여 줍니다.

`curl -H`처럼 `-H 'Name: value'`로 요청 header를 넣을 수 있고, 여러 번 줄 수 있습니다. `-H 'Host: name'`은 Host header를 바꿉니다. redirect가 다른 host로 가면 그 요청에는 `Authorization`과 `Cookie`가 실리지 않습니다. 요청마다 보낸 header는 `-v`로 확인합니다.

`--body page.html`은 마지막 응답의 본문 전체를 파일로 저장합니다. `--body -`는 본문을 stdout에 쓰고, 결과는 stderr로 보냅니다. stdout이 터미널이면 본문의 제어 문자를 escape해서 씁니다. 본문은 10 MiB까지만 읽고, 그보다 길면 잘랐다고 알려 줍니다. 같은 이름의 파일이 있으면 덮어씁니다. option은 URL 앞이나 뒤에 둘 수 있습니다.

```bash
./bin/edc http check http://example.com
./bin/edc http check -v -H 'Authorization: Bearer <token>' https://example.com/api
./bin/edc http check https://example.com/api --body - | jq .
```

```bash
./bin/edc tls check --min-days 14 example.com:443
./bin/edc http check --expect-status 200 https://example.com/health
```

실패는 exit code `1`을 돌려줍니다. 이 option을 cron에 넣으면 synthetic check가 됩니다.

![edc tls check가 PASS를 준 뒤, --min-days 90이 인증서 남은 일수를 기준 미달로 판정해 FAIL과 ERROR 블록을 출력하는 화면](docs/media/tls.gif)

![edc http check가 HTTP 200으로 PASS를 준 뒤, --expect-status 404가 기대값 불일치로 FAIL을 출력하는 화면](docs/media/http.gif)

두 데모 모두 통과 한 번과 임계값 실패 한 번을 보여 줍니다.

## 장애 snapshot

`edc snapshot`은 이 호스트의 상태를 edc report 하나로 저장합니다. 변경이나 장애 전에 한 번, 후에 한 번 찍고 `edc report diff`로 비교합니다.

읽기만 하는 명령입니다. 출력 파일만 쓰고 호스트의 다른 것은 바꾸지 않습니다. 외부로 아무것도 보내지 않으므로 공인 IP도 조회하지 않습니다.

```bash
edc snapshot                              # ./edc-snapshot-<host>-<UTC 시각>.json
sudo edc snapshot --output before.json    # root는 모든 audit와 events 원천을 읽음
sudo edc snapshot --output after.json
sudo edc snapshot --du /var --output after.json   # /var의 edc du 결과도 함께 (Linux)
sudo edc report diff before.json after.json
edc snapshot --since 2h --output -        # report JSON만 stdout으로
```

report에는 다음 결과가 들어갑니다.

| 결과 | 내용 |
|---|---|
| `info.host` | OS, 커널 release, CPU, uptime, load, swap, 열린 파일 한도 |
| `info.memory` | 전체, 가용, 사용 메모리. 사용률 90% 이상이면 `warn` |
| `info.pressure` | PSI some avg10과 부팅 뒤 OOM kill 횟수. PSI 10% 이상이거나 OOM kill이 있으면 `warn` |
| `info.disk:<mount>` | `edc info`가 보여 주는 mount마다 결과 하나. 사용률 90% 이상, 읽기 전용 mount, inode 사용률 80% 이상, 또는 process가 아직 열고 있는 지워진 파일이 mount의 10% 이상을 잡고 있으면 `warn`. 가장 큰 deleted-open 파일이 근거로 남습니다 |
| `info.processes` | process와 thread 수, RSS가 큰 process 5개 |
| `listen` | `edc listen`이 보여 주는 대기 소켓 |
| `audit.*` | `edc audit`의 점검 |
| `events.*` | `--since` 기간(기본 6h)의 `edc events` 신호 |
| `changes.*` | `--since` 기간의 `edc changes` 원천 |
| `du.scan` | `--du <path>`를 줄 때만: 그 경로의 `edc du` 결과와 `report diff`용 큰 디렉터리 목록 |

큰 트리에 `--du`를 쓰면 `--timeout`을 늘려야 할 수 있습니다. 제한 시간에 걸리면 `du.scan` 결과는 일부만 센 값이 되고, `report diff`는 그 디렉터리를 비교하지 않습니다.

모든 항목은 `--timeout` 예산(기본 15s) 하나 안에서 동시에 돌아갑니다. 예산에 2초를 더해도 끝나지 않은 항목은 이유와 함께 `skip`이 됩니다. `audit`, `events`, `changes`는 점검마다 `skip`을 하나씩 남기므로 `report diff`가 같은 ID끼리 비교합니다. `info.disk`는 mount 목록을 알 수 없어서 `info.disk` skip 하나로 남습니다. 수집기가 실패하면 오류와 함께 `fail`이 됩니다. 어느 항목도 report에서 빠지지 않습니다.

macOS에서는 `audit`, `events`, `changes`, `info.pressure`가 `skip`으로 나옵니다. Linux에서 root가 아니면 일부 audit와 events 원천이 `pass`가 아니라 `skip`으로 나옵니다.

파일에 계정 이름, sudo 규칙, process 이름이 들어가므로 권한은 0600입니다. 출력 경로에 이미 파일이 있으면 바꾸지 않고, 그 자리의 symlink도 따라가지 않습니다. 이때 exit code는 `2`입니다. 바꾸려면 `--force`를 붙이세요. 그러면 새 0600 파일을 쓴 뒤 그 경로로 옮깁니다.

`--redact`는 host 이름, 홈 경로, IP 주소만 가립니다. 기본 파일 이름에서도 host 이름을 뺍니다. 계정 이름, sudo 규칙, process 이름은 report에 그대로 남습니다.

파일을 쓰고 나면 경로, 결과 개수, `report diff` 안내를 출력합니다. `fail` 결과가 하나라도 있으면 exit code는 `1`이고, 없으면 `0`입니다. 사용법 오류와 쓰기 오류는 `2`를 돌려줍니다.

## Report 비교

`edc report`는 edc 진단 명령이 `--json`으로 저장한 **edc 진단 결과**를 읽고 비교합니다. `doctor`뿐 아니라 같은 report 형식을 사용하는 다른 edc 진단 명령의 결과도 지원합니다. 일반 JSON 파일이나 `report diff`가 생성한 비교 JSON은 지원하지 않습니다. `edc log`가 저장한 명령 출력 텍스트와는 별개입니다. `edc report list [directory]`는 지정한 디렉터리(기본값: 현재 디렉터리)의 유효한 JSON report를 실행 시각이 최신인 순서로 나열하며, 실행 시각과 PASS/WARN/FAIL/SKIP 개수를 보여 줍니다. 하위 디렉터리는 검색하지 않습니다.

```bash
./bin/edc doctor --json report.json https://example.com
./bin/edc report list
./bin/edc report show report.json
```

경로 없이 `edc report show`나 `edc report diff`를 실행하면 최근 report 최대 20개 중에서 선택하거나 다른 JSON 파일 경로를 직접 입력할 수 있습니다. report가 없으면 저장 예시를 안내합니다. `edc report diff`는 report를 한 화면에 보여 주고 가장 최근 두 개를 미리 골라 둡니다. Space로 선택을 바꾸고 Enter로 비교합니다. 시간이 이른 쪽이 언제나 이전이 됩니다. 반대 순서로 비교하려면 두 경로를 직접 주세요.

`edc report diff`는 두 JSON report를 probe 이름으로 맞춰 비교합니다. probe마다 status 변화와 scalar metric 차이를 보여 줍니다.

```bash
./bin/edc doctor --json before.json https://example.com
./bin/edc doctor --json after.json https://example.com
./bin/edc report diff before.json after.json
./bin/edc report diff --json diff.json before.json after.json
```

보고서에는 schema version `1.0`, tool name `edc`, 비어 있지 않은 tool version과 run ID, 유효한 0이 아닌 실행 시작 시각이 필요합니다.

`results`와 summary 객체가 있어야 합니다. 빈 결과는 `null`과 `[]`를 모두 허용하며, 알 수 없는 확장 필드도 허용합니다.

각 결과에는 probe 이름과 `pass`, `warn`, `fail`, `skip` 중 하나의 status가 필요합니다. 실행 시간과 summary 개수는 음수일 수 없으며, summary는 결과와 일치해야 합니다. 잘못된 보고서 입력의 exit code는 `2`입니다.

`STATUS SAME`과 `STATUS CHANGED`는 상태만 비교합니다. JSON의 `same`과 `changed`도 같은 기준을 유지합니다.

status가 pass에서 warn이나 fail로, 또는 warn에서 fail로 바뀌면 그 probe를 `WORSE`로 표시합니다. 악화된 probe가 하나라도 있으면 exit code는 `1`입니다.

두 보고서에 모두 있는 scalar metric과 실행 시간의 차이는 별도로 표시합니다. 배열, 객체, 추가되거나 삭제된 metric 키는 비교하지 않으므로, 차이가 표시되지 않아도 모든 측정값이 같다는 뜻은 아닙니다. 접힌 뷰어에는 scalar와 실행 시간의 차이 개수가 나옵니다.

머리말에는 양쪽 target URL, target host, 수집 hostname이 나옵니다. 서로 다르면 안내를 표시하고 비교를 계속합니다. 없는 식별 정보는 확인 불가로 표시합니다. JSON 비교 결과의 각 측에는 수집 `hostname`과 별도로 선택 필드 `target_url`과 `target_host`가 추가됩니다.

## Report 뷰어

stdin과 stdout이 모두 terminal이면 `edc report show`와 `edc report diff`는 전체 화면 뷰어를 엽니다.

| key | 동작 |
|---|---|
| `f` | 필터 변경 |
| `e` | 상세 펼치기와 접기 |
| `↑` `↓` `PgUp` `PgDn` | 스크롤 |
| `q` | 종료 |

`edc report show`의 필터는 전체, 실패와 경고, 실패만 순서로 바뀝니다. `edc report diff`의 필터는 전체, 상태가 바뀐 것, 악화된 것 순서로 바뀝니다.

뷰어는 화면에 출력을 남기지 않습니다. exit code는 그대로입니다. 파이프, 파일, `--json`은 기존 출력을 받습니다.

![edc report show 뷰어에서 f로 필터를 바꾸고 e로 상세를 펼친 뒤, edc report diff가 probe별 duration_ms 차이를 보여 주는 화면](docs/media/report.gif)

데모는 뷰어를 열어 필터를 바꾸고 상세를 펼친 다음, 두 report를 비교합니다.

## Top 임계값

`edc top`은 색을 둘만 씁니다. 노란색은 경고, 빨간색은 위험입니다. 정상 값은 terminal 기본색으로 두므로 색이 붙은 값만 살피면 됩니다.

표는 load, `usr%`, `sys%`, `i/o`, `mem_%`에 색을 넣습니다. 대시보드는 이 값에 더해 `hot core`, `await`, `err`, `drop`, pressure 값에도 색을 넣습니다.

load 임계값은 host의 core 수를 따릅니다.

| 값 | 경고 | 위험 |
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

`hot core`는 core 하나의 사용률이므로 90부터 경고만 주고 위험 단계가 없습니다. core가 여럿이면 하나가 포화해도 host 전체에는 여유가 있습니다.

process 패널은 `READ`와 `WRITE`에 위 임계값으로 색을 넣고, `STATE` 열의 `iowait`는 노란색으로 표시합니다. process 묶음의 CPU 합은 core 하나가 바쁜 수준인 80%부터 빨간색입니다.

대시보드는 `iops`, `busy%`, `swap/s`, `steal%`, `blocked`, `queue`, 바이트·패킷·TCP 속도, `signal` 열에는 색을 넣지 않습니다. 임계값이 없거나 색 없이도 수준이 드러나는 값입니다. 집계 `busy%`는 바쁜 disk가 여럿이면 100을 넘으므로 고정 임계값이 잘못된 신호를 줍니다. `cores` 막대는 `.`, `:`, `*`, `#`로 수준을 보여 줍니다.

색을 끄려면 `NO_COLOR`를 설정합니다. 파이프와 파일에는 색이 들어가지 않습니다.

`edc info`는 disk마다 막대를 그립니다. 막대는 `mem_%`와 같은 임계값을 쓰고 한 칸이 5퍼센트입니다. 막대는 색 없이도 수준을 보여 주므로 파이프에서도 정보가 남습니다.

`edc info`는 disk 사용량을 전체 크기에서 남은 공간을 뺀 값으로 셉니다. macOS에서는 여러 APFS volume이 container 하나를 나눠 쓰므로, volume 하나의 `Used` 열만 보면 다른 volume이 차지한 공간이 빠집니다.

`edc info`는 저장 장치를 쓰는 file system만 보여 줍니다. `tmpfs`, `overlay`, `snap`이 붙이는 읽기 전용 image는 숨깁니다. 램을 쓰거나 같은 disk를 두 번 세기 때문입니다. loop device 자체는 숨기지 않으므로 직접 붙인 disk image는 그대로 나옵니다.

macOS에서 `edc`는 memory 사용량을 Mach `host_statistics64` 호출로 읽습니다. free, speculative, inactive page를 빼는데, 이는 Linux의 `MemAvailable`과 같은 기준입니다. `top`의 `PhysMem` 줄은 cache를 포함하므로 97퍼센트 위에 머뭅니다.

## Top 대시보드

stdin과 stdout이 모두 terminal이면 `edc top`은 전체 화면 대시보드를 엽니다. 대시보드에는 `--count` 제한이 필요 없습니다.

| key | 동작 |
|---|---|
| `q` | 종료 |
| `p` | 일시정지와 재개 |
| `+` | interval 늘리기 |
| `-` | interval 줄이기 |
| `1`, `c`, `m`, `d`, `n` | 전체, CPU, memory, disk, network 열로 전환. `1`이 처음 보기 |
| `s` | pressure 열로 전환 |
| `o` | process 패널을 CPU, `MEM`, I/O 순으로 정렬. 누를 때마다 기준이 바뀌고, 네 번째로 누르거나 보기를 바꾸면 보기 기준으로 돌아감 |
| `v` | 박스 화면으로 돌아가기 |
| `f` | 선택한 행의 signal 보기로 이동하고, 한 번 더 누르면 process 후보 선택. 필터가 있으면 process 보기로 전환 |
| `Tab` | 이력 탐색과 process 후보 선택 사이 이동 |
| `?` | 도움말 표시. 화살표로 스크롤하고 `Esc`로 닫기 |
| `/` | process 필터 입력: command 이름이나 PID, 쉼표로 구분 |
| `i` | interface 선택 상자: 목록과 traffic을 보고 `Space`로 체크, `a` 전체, `p` physical. `Enter`로 다음 sample부터 적용하며, `-w`로 기록하는 동안에는 쓸 수 없음 |
| `Esc` | 한 단계 뒤로: 도움말, 목록, 후보 선택을 닫고, 그다음 상세나 최고치 패널 닫기, 최신 행으로 복귀, `o`로 고른 순위 해제, 처음 보기로 복귀, 마지막으로 process 필터 해제 |
| `↑`, `↓`, `PgUp`, `PgDn`, `End` | 과거 행 선택, 한 화면씩 이동, 실시간 행 추적 재개 |
| `Enter` | 선택한 시점의 상세 표시. 후보 선택 중에는 선택한 PID에 초점 |
| `h` | 최근 60초의 load, CPU, iowait, memory 최고치를 각각 시각과 함께 표시 |
| `e` | 이벤트 목록 열기. 화살표로 고르고 `Enter`로 그 시작 시점으로 이동, `e`나 `Esc`로 닫기 |
| `[`, `]` | 이전·다음 이벤트의 시작 시점으로 이동 |

기본 표의 `signal` 열은 load, CPU, iowait, memory, disk await, network errors·drops 중 가장 심각한 항목과 추가 개수를 보여 줍니다. network errors·drops는 초당 1개부터 경고로 셉니다. 패킷 수는 network 보기에서 확인합니다. network 값은 loopback을 뺀 모든 interface의 counter를 더한 값입니다. Linux에서는 edc가 있는 network namespace의 interface를, macOS에서는 켜진 interface를 읽습니다. bridge, veth, tunnel도 더하므로 같은 packet이 둘 이상의 interface에서 셀 수 있습니다. counter는 interface마다 따로 빼므로, 두 sample 사이에 생기거나 사라진 interface는 그 구간에 더하지 않습니다. 과거 행을 선택해도 수집은 계속되며 `End`로 최신 행을 다시 따라갑니다. 수집이 잠시 실패하면 마지막 행을 유지하고 다음 interval에 다시 시도합니다.

일부 interface만 더하려면 `--interface`를 씁니다. interface 이름, glob, `physical`을 쉼표로 구분해 적습니다. `physical`은 장치가 뒤에 있는 interface를 고릅니다. Linux에서는 `/sys/class/net/<이름>/device`가 있는 interface입니다. bridge, veth, bond, VLAN, tunnel이 빠지므로 같은 packet을 NIC에서 한 번만 셉니다. `all`은 모든 interface를 더하며 기본값입니다.

```bash
edc top --interface physical
edc top --interface eth0,ens5
edc top --interface 'en*' --json -
```

선택은 표, 대시보드, JSON, `--write`, `n` 보기의 장치 쪽 drop에 모두 적용됩니다. 적은 interface 이름이 시작할 때 없으면 exit code 2로 끝납니다. glob이나 `physical`에 맞는 interface가 없으면 경고를 보입니다. 맞는 interface가 생길 때까지 byte와 packet 값은 0으로 남고, `err`와 `drop`은 `—`로 보이며, JSON의 `network_health_supported`는 `false`입니다. `--interface`를 쓰면 JSON sample마다 `interface_filter`와 `interfaces`가 붙습니다. `interfaces`에는 고른 interface 중 그 구간에 traffic이 있는 interface의 값이 들어갑니다. `edc history list`는 `INTERFACES` 열에 선택을 보입니다.

매번 같은 선택을 쓰려면 설정 파일에 `defaults.top.interface`를 둡니다. macOS에서는 `physical`을 쓸 수 없습니다. 이때는 경고를 보이고 모든 interface를 더하므로, 설정 파일 하나를 Linux와 macOS host가 함께 쓸 수 있습니다.

Linux에서는 `i/o`가 경고 수준이고 I/O를 기다리는 작업이 4개 이상이면 `signal` 열에 `blocked N`을 표시합니다. macOS에는 `i/o` 값이 없습니다. `U` 상태는 page-in 대기에서도 생기므로, macOS는 `U` 상태 process가 4개 이상이고 `await`가 경고 수준이거나 memory 압박 단계가 `warn`이나 `critical`일 때 `blocked N`을 표시합니다. macOS에서는 kernel이 memory 압박 단계를 올리면 `signal` 열에 `mem pressure warn`이나 `mem pressure critical`도 표시합니다. process 묶음도 CPU 합이 80% 이상이거나 I/O 합이 50 MB/s 이상이면 `gm (200) 100%`처럼 이름과 process 수로 경고합니다. I/O 합은 disk 보기에서만 계산하므로 묶음의 I/O 경고도 disk 보기에서만 나옵니다.

기본 표는 terminal 폭에 맞춰 열을 늘립니다. 80열보다 넓으면 다음 순서로 열을 추가합니다.

| terminal 폭 | 추가되는 열 |
|---|---|
| 84 | hot core |
| 96 | disk IOPS, `await` |
| 110 | packet in/out |
| 120 | network errors·drops |
| 125 | disk `busy` |
| 143 | CPU·memory·I/O pressure(`psi`) |
| 149 | memory swap out(`swap`) |
| 161 | listen queue overflow(`listen`), softnet drop(`soft`) |
| 167 | conntrack 사용률(`ct%`) |

`signal` 열은 13~16칸을 씁니다. 경고를 적어도 하나와 나머지 경고의 개수를 표시할 수 있는 폭입니다. 남는 폭은 다른 열이 나눠 가져서, 표가 terminal 오른쪽 끝까지 찹니다. 제목 줄도 폭에 여유가 있으면 OS 이름, memory 크기, CPU 모델을 함께 표시합니다. 제목 오른쪽 끝에는 보기와 `live` 또는 `history`를 표시하고, 폭에 여유가 있으면 edc 버전도 붙입니다. 폭이 줄면 추가한 열을 바로 뺍니다.

80열보다 좁으면 전체 보기에는 CPU, memory, load, signal을 남깁니다. 다른 보기에서도 폭에 맞지 않는 열은 뺍니다. 최소 크기는 24열·8행이며, 작은 terminal에서는 도움말을 스크롤할 수 있습니다.

`--split`을 쓰면 여러 보기를 한 화면에 동시에 봅니다. 보기마다 박스가 하나씩 생기고, 박스 안에는 그 보기의 최근 sample 표가 들어갑니다.

`--split`은 `cpu`, `mem`, `disk`, `net`, `psi` 이름을 쉼표로 이어 받으며, 적은 순서대로 박스를 배치합니다. 목록 없이 `--split`만 쓰면 다섯 박스를 모두 보이고, `--split none`은 박스 없이 단일 보기로 시작합니다. 빈 값, 모르는 이름, 중복 이름은 exit 2로 끝납니다.

설정 파일의 `defaults.top.split`에 같은 문법으로 목록을 적으면 대시보드를 매번 그 목록으로 시작합니다. 명령줄에 직접 준 `--split`이 설정 값보다 우선합니다.

박스는 적은 순서를 지킵니다. terminal 폭에 들어가는 가장 적은 줄 수를 쓰고, 그 줄 수 안에서 가장 넓은 줄이 가장 좁아지도록 박스를 나눕니다. 박스 줄들은 높이를 똑같이 나눠 가집니다. 모든 박스에서 `signal` 열을 빼고, 선택한 시각의 signal을 박스 아래 한 줄로 한 번만 보입니다. cpu 박스가 화면에 있으면 mem과 psi 박스에서 `load`를 빼고, mem 박스가 있으면 psi 박스에서 `mem%`를 뺍니다. 상세, 최고치, process 패널은 단일 보기와 같은 방식으로 그 아래에 놓입니다. 같은 줄의 박스는 같은 시각의 행을 나란히 보이므로 `time` 열은 줄의 첫 박스에만 있습니다.

선택한 행은 모든 박스가 공유합니다. 방향키와 `End`는 모든 박스를 함께 움직이고, `PgUp`과 `PgDn`은 박스 하나의 데이터 행 수만큼 이동합니다. `1`, `c`, `m`, `d`, `n`, `s`를 누르면 박스 화면을 떠나고, `v`를 누르면 돌아옵니다.

박스 하나에는 데이터 행이 3개 이상 필요합니다. 박스의 데이터 행이 그보다 적으면 첫 박스를 단일 보기로 보이고 상태 줄에 이유를 안내합니다. terminal보다 넓은 박스는 뒤쪽 열을 빼고 들어갑니다. `steal%`, `retr/s` 같은 선택 열 때문에 박스 줄이 늘어나면 박스에서 선택 열을 뺍니다. 80열·24행 terminal에서 `--split`은 CPU 보기만 보이고, 80열에서 박스 다섯 개를 모두 보이려면 약 33행이 필요합니다.

macOS에서는 `psi` 박스를 생략하고 안내를 보입니다. macOS의 memory 압박 단계는 mem 박스에 있습니다. 설정 파일에 `psi`를 적어도 오류가 아니므로 Mac과 Linux가 같은 파일을 쓸 수 있습니다.

`--split`은 대시보드에서만 동작합니다. 표를 출력하는 실행(`--count`, `--json`, 파이프, `NO_COLOR`)이나 `--process`와 함께 직접 주면 exit 2로 끝납니다. 설정 값은 그런 실행을 막지 않고, `--process`와 함께면 process 보기가 유지됩니다.

`--view`로 처음 보기를 고릅니다. `all`, `cpu`, `memory`, `disk`, `network`, `pressure`, `split`을 쓸 수 있고, `Esc`를 누르면 이 보기로 돌아옵니다. 설정 파일의 `defaults.top.view`에 적으면 매번 그 보기로 시작합니다.

`--panels`로 표 아래에 늘 보일 패널을 고릅니다. `cpu`, `mem`, `disk`, `net`, `psi`, `ai`, `events`를 쉼표로 이어 적고, `none`은 패널을 모두 뺍니다.

- `cpu`, `mem`, `disk`, `psi`는 선택한 행의 요약, 최근 60초 최고치, 그 자원을 가장 많이 쓴 process 셋을 세 줄로 보입니다. 오른쪽 process 목록은 기준 하나로만 줄 세우므로, 자원마다 상위 process를 이 패널에서 봅니다. `disk` 패널을 켜면 process별 I/O도 읽습니다. `psi` 패널은 I/O를 기다리는 process를 보이고, macOS는 PSI를 수집하지 않으므로 한 줄 안내만 보입니다.
- `net`은 선택한 행의 network limit입니다. macOS는 이 값을 수집하지 않으므로 한 줄 안내만 보입니다.
- `ai`는 `edc ai`와 같은 Claude·Codex 사용량 한도입니다. 1분마다 다시 조회합니다.
- `events`는 최근 이벤트입니다.

목록 앞의 패널이 먼저 자리를 받습니다. 패널이 들어가지 않으면 패널을 자르지 않고 통째로 숨긴 뒤 상태 줄에 `+net hidden`처럼 알립니다. 뒤의 작은 패널은 남은 줄에 들어가면 보입니다. 표는 적어도 5행, 박스는 데이터 행을 적어도 3개 남깁니다.

`a`를 누르면 `ai` 패널을 켜고 끕니다. 패널을 켜기 전에는 Claude와 Codex 정보를 읽지 않습니다. `Enter` 상세나 `h` 최고치를 연 동안에는 그 패널이 패널 자리를 씁니다.

설정 파일의 `defaults.top.panels`에 같은 문법으로 적으면 매번 그 목록으로 시작합니다. 목록을 정하지 않으면 지금 배치를 그대로 씁니다. network 보기에서 `net` 패널이 보이고, 빈자리에 이벤트가 보입니다.

`defaults.top.process_sort`에 `cpu`, `mem`, `io`를 적으면 process 목록의 처음 정렬이 됩니다. `o`로 바꾸는 동작은 그대로입니다.

`--columns`로 all 보기의 열 그룹을 고릅니다. `net`, `cpu`, `mem`, `disk`, `psi`를 보이고 싶은 순서대로 쉼표로 이어 적고, `all`은 다섯 그룹을 모두 보입니다. 그룹마다 기본 열은 늘 보이고, 폭이 남으면 열을 더 붙입니다. 그룹을 빼면 그 폭을 남은 그룹이 씁니다. 이름 뒤에 `+`를 붙이면 그 그룹의 열을 다른 그룹보다 먼저 붙입니다. 예를 들어 `--columns net+,cpu,mem`은 `hot core`보다 packet과 network 오류 열을 먼저 보입니다. 설정 파일의 `defaults.top.columns`에 적으면 매번 그 목록으로 시작합니다.

목록에 `ai`를 넣으면 `claude` 그룹과 `codex` 그룹에 두 도구의 요청이 보입니다. 행마다 직전 행 뒤부터 그 행 시각까지 들어온 요청을 세므로, 같은 줄의 다른 열과 견줄 수 있습니다. 그룹마다 `req`는 요청 수이고, `tok`은 새 입력과 출력을 더한 token으로 `edc ai`의 `total`과 같은 값입니다. prompt cache에서 읽은 token은 넣지 않고, 0은 `·`로 보입니다. Claude Code는 응답이 끝난 뒤 기록을 쓰고 edc는 2초마다 읽으므로, 가장 최근 행은 2~3초 뒤에 값이 채워집니다. `all`에는 `ai`가 들지 않고, 목록에 `ai`가 있을 때만 Claude와 Codex 대화 기록을 읽습니다.

```toml
[defaults.top]
split = "net,disk"
view = "split"
panels = "net,ai,events"
process_sort = "io"
columns = "net+,cpu,mem"
```

`--view`, `--panels`, `--columns`도 `--split`처럼 대시보드에서만 동작하고, `--view`를 `--process`와 함께 주면 exit 2로 끝납니다. 설정 값은 표를 출력하는 실행을 막지 않습니다.

`edc top setup`을 실행하면 이 값들을 실제 화면을 미리 보며 고를 수 있습니다. 위쪽에는 보기, 열 그룹, 박스, 패널, process 정렬, interval 줄이 있고, 아래쪽에는 실제 sample로 그린 대시보드가 값을 바꿀 때마다 바로 바뀝니다.

- `↑`·`↓`로 줄을 고르고, `←`·`→`로 값을 바꾸거나 박스·패널 항목을 고릅니다.
- `Space`로 열 그룹, 박스, 패널을 켜고 끄고, `[`·`]`로 순서를 옮깁니다. 열 그룹 줄에서 `+`를 누르면 그 그룹의 열을 먼저 더 보입니다.
- `Tab`을 누르면 미리보기를 터미널 전체 크기로 그립니다. 배치는 저장한 뒤의 `edc top`과 같고, 상태 줄 첫 줄에만 설정 화면의 키 안내가 보입니다.
- `z`를 누르면 80×24, 120×30 크기로 바꿔 봅니다. 터미널에 들어가지 않는 크기는 건너뜁니다. 그 크기에서 숨는 패널은 패널 줄 옆에 `+net hidden at 80×24`처럼 보입니다.
- `Enter`를 누르면 저장할 `[defaults.top]` 내용을 보여 주고, `y`를 누르면 저장합니다.

`edc top setup`은 바꾼 키만 고치고 다른 키와 섹션은 그대로 둡니다. 파일 전체를 다시 쓰므로 파일 안의 주석은 남지 않습니다. 설정 화면을 연 사이에 파일이 바뀌면 저장하지 않고 exit 2로 끝나고, 저장하지 않은 변경이 있는 채로 나가면 exit 4로 끝납니다.

disk 보기에는 macOS와 Linux 모두 물리 disk의 IOPS와 평균 `await`가 추가됩니다. Linux에서는 모든 물리 disk의 합산 `busy%`와, memory 보기의 `mem%` 옆 memory pressure도 추가됩니다. 합산 `busy%`는 여러 disk가 동시에 바쁘면 100%를 넘을 수 있습니다. macOS 대시보드는 수집하지 않는 iowait, PSI, disk busy, file descriptor, listen overflow, softnet drop, conntrack, I/O 지연 열을 숨기고 도움말에 제한을 설명합니다. macOS의 memory와 pressure 보기에는 kernel의 memory 압박 단계(`kern.memorystatus_vm_pressure_level`)인 `mem lvl`이 `normal`, `warn`, `critical`로 나옵니다. CPU와 pressure 보기의 `blocked`는 `U` 상태 process 수입니다. Linux는 작업(thread)을 세므로 두 값의 단위가 다릅니다. network 보기의 interface errors·drops는 macOS와 Linux 모두 표시하며, macOS에서는 kernel의 interface 통계(`net.link.generic.ifdata`)에서 읽습니다. memory 보기의 `swap/s`는 kernel이 초당 swap으로 내보낸 byte입니다.

Linux에서는 일부 보기에 선택 열이 더 있습니다. CPU 보기의 `steal%`는 hypervisor가 다른 guest에 CPU를 내준 시간의 비율이고, `blocked`는 지금 I/O를 기다리며 멈춘 작업 수입니다. disk 보기의 `queue`는 진행 중인 I/O 요청의 평균 개수를 물리 disk마다 더한 값입니다. network 보기에는 초당 TCP 재전송 segment(`retr/s`), 보낸 RST(`rst/s`), 실패한 연결 시도(`fail/s`)가 있습니다. 선택 열은 다른 열이 모두 들어가고 `signal`에 13열 이상이 남을 때만 보입니다.

`s`는 pressure 보기입니다. macOS에서는 `mem lvl`, `blocked`, `load`, `mem%`를 보입니다. Linux에서는 CPU, memory, I/O의 `some avg10`을 퍼센트로 표시하며, 최근 10초 동안 일부 작업이 그 자원을 기다린 시간의 비율입니다. `mem full`과 `io full` 열은 memory와 I/O의 `full avg10`으로, 최근 10초 동안 실행할 수 있는 작업이 모두 동시에 기다린 시간의 비율입니다. Linux pressure 보기에는 지금 I/O를 기다리는 작업 수인 `blocked`도 `io psi` 옆에 나옵니다. CPU 보기의 `hot core`와 ASCII 막대는 코어별 사용률을 보여 주고, 24개보다 많은 코어는 앞 24개만 막대로 표시합니다.

상세 보기에는 CPU 사용률 기준 상위 세 process도 표시합니다. 목록은 관측 주기를 늘리지 않도록 최대 1초마다 백그라운드에서 갱신하며, `--write`가 없으면 대시보드에서만 수집하고 표와 필터 없는 `--json` 출력에서는 수집하지 않습니다. Linux에서는 `/proc/<pid>/stat`의 CPU tick을 직전 갱신과 비교하므로 값은 그 사이 구간의 사용률입니다. macOS에서는 `ps`가 제공하는 최근 감쇠 평균을 씁니다.

process 패널은 `PID`, `COMMAND`, `STATE`, `CPU%`, `MEM`을 보여 줍니다. 패널 폭이 넉넉하면 `MEM` 뒤에 `RSS`도 보여 줍니다. `STATE`는 `run`, `sleep`, `iowait`, `zombie`, `stop`, `trace`, `idle`, `dead`, `park` 중 하나입니다. `iowait`는 Linux의 `D` 상태로, I/O가 끝나기를 기다리며 signal로도 깨울 수 없는 상태입니다. `iowait`는 disk를 기다린다는 뜻이지, 그 process가 disk를 많이 쓴다는 증거는 아닙니다.

terminal이 60열 이상이면 `READ`와 `WRITE` 열을 붙입니다. disk 보기와 `--process`에서는 항상 붙이고, 다른 보기에서는 값이 있을 때만 붙입니다. `/proc/<pid>/io`에서 읽은, 저장 장치에 닿은 초당 byte입니다. 값을 읽지 못하면 `—`로 표시합니다. 다른 사용자의 process까지 보려면 `edc top`을 root로 실행합니다.

`MEM`은 process가 차지한 메모리입니다. macOS는 압축된 메모리까지 포함하는 physical footprint, Linux는 RSS에 swap으로 나간 양(`VmSwap`)을 더한 값입니다. RSS는 압축되거나 swap으로 나간 page를 세지 않으므로 누수로 커진 process도 RSS는 작게 보일 수 있습니다. `MEM`을 읽지 못하면 그 자리에 RSS를 흐린 색으로 보여 줍니다. macOS에서는 root나 `_windowserver`처럼 다른 사용자 소유인 process의 값을 읽지 못합니다. `sudo`로 실행하면 읽습니다.

분마다 가장 낮은 `MEM` 값이 15분 동안 오르면 `signal` 열에 `node +360.0M/12m`처럼 그 process를 표시합니다. 처음 3분의 최저값과 마지막 3분의 최저값을 비교하고, 시간은 그 두 최저값이 나온 분 사이의 간격입니다. 이 사이에 256 MiB 이상 늘어야 하고, 절반 이상의 분에서 실제로 늘어나야 합니다. 그래서 GC가 만드는 톱니나 한 번에 오른 계단은 누수로 보지 않습니다. memory 보기에서는 가장 많이 늘어난 process 하나가 크기와 상관없이 맨 앞에 오고, 늘어나는 process의 `MEM` 칸에 색이 칠해집니다. RSS로 대신한 흐린 값은 흐린 색 그대로 둡니다. 이 속도로 남은 메모리를 1시간 안에 다 쓰면 경고가 위험으로 바뀝니다. 첫 판정에는 약 15분 동안의 기록이 필요합니다. 일시정지하거나 `--interval`을 1분보다 길게 주면 기록이 비고, `edc`는 빈 구간을 건너 증가를 판정하지 않습니다.

후보 순위는 기본이 CPU, memory 보기는 `MEM`, disk 보기는 I/O입니다. `o`를 누르면 위쪽 host 열은 그대로 두고 패널 순위만 고를 수 있고, 이때 제목에 `MEM rank (o)`처럼 `(o)`가 붙습니다. I/O 순위는 다음 표본부터 모든 process의 I/O를 읽으므로 CPU는 낮고 I/O가 큰 process도 후보에 오릅니다. 다른 순위는 후보의 I/O만 읽습니다. 목록을 자르기 전에 CPU, `MEM`, `iowait` 상태 기준 상위 5개를 각각 보존하고, I/O 순위는 I/O 상위 5개도 보존합니다. 후보는 3개를 표시하고, terminal이 40행 이상이면 5개를 표시합니다.

후보 위에는 process 묶음을 최대 2줄 표시합니다. 묶음은 실행 파일 이름이 같은 process 2개 이상이며 `gm (200 procs, 150 iowait)`처럼 씁니다. 묶음 순위는 패널 순위와 같은 기준의 합(CPU, `MEM`, I/O)을 따릅니다. 묶음은 그 기준의 합이 최소값(CPU 10%, `MEM` 100 MB, I/O 1 MB/s)을 넘어야 보이고, I/O 순위에서는 `iowait` process가 있는 묶음도 보입니다. 메모리 합은 공유 page를 process마다 셀 수 있으므로 상한값이라는 뜻으로 `≤`를 붙입니다. 묶음 줄에는 PID가 없고 고를 수 없습니다.

Linux kernel은 부모가 끝난 자식을 회수할 때 자식의 I/O를 부모의 값에 더합니다. 짧게 사는 자식을 많이 띄우는 부모는 자식의 I/O까지 자기 값으로 보일 수 있으므로, 부모를 원인으로 보기 전에 자식을 확인합니다.

패널과 키 안내는 화면 맨 아래에 고정됩니다. 안내 두 줄이 한 줄에 들어가면 한 줄로 합칩니다. terminal이 145열 이상이면 process 패널은 오른쪽으로 가고, 상세·최고치 패널과 키 안내는 왼쪽에 놓입니다. 왼쪽을 쓰는 다른 패널이 없으면 왼쪽에 이벤트를 놓습니다. 89~144열에서는 이벤트를 process 패널 오른쪽에 놓습니다. `--process`를 쓰면 process 줄이 길어지므로 위아래로 쌓습니다. `Tab`으로 후보 선택에 들어가 화살표로 고른 뒤 `Enter`로 해당 PID에 초점을 맞춥니다. 후보를 고르는 동안에는 선택한 시점을 유지하고, `End`로 실시간 이력으로 돌아갑니다.

`signal` 열은 host 경고를 process CPU 후보보다 먼저 보여 줍니다. 후보가 host 경고의 원인이라고 단정하지 않습니다. process CPU는 core 하나가 100%이고, host CPU는 전체 core를 기준으로 합니다.

process 패널과 `PROCESS` 막대에는 선택한 시각을 표시합니다. host 수집이 실패하면 마지막 성공 시각을 함께 표시합니다.

interval은 200ms, 500ms, 1s, 2s, 5s, 10s, 30s, 1m 사이를 오갑니다. 일시정지를 풀면 먼저 새 기준점을 만들고, 그 다음 행부터 rate를 표시합니다.

대시보드는 이전 화면으로 빠져나가며 행을 남기지 않습니다. 대시보드를 유지하며 기록하려면 `--write <DB>`, JSON Lines로 남기려면 `--json`을 씁니다.

다음 경우에는 대시보드 대신 기존 표를 출력합니다.

- `--count`나 `--json`을 지정한 경우
- stdin이나 stdout이 terminal이 아닌 경우
- `NO_COLOR`가 설정된 경우

macOS에서 `edc`는 Mach `host_processor_info` 호출로 kernel에서 core별 CPU tick을 직접 읽고, Linux에서는 `/proc/stat`을 읽습니다. 두 운영체제 모두 모든 열이 interval을 따릅니다.

Linux에서는 `n` 화면에 conntrack 사용률(`ct%`), listen overflow/s(`listen/s`), softnet drop/s(`soft/s`)를 추가합니다. 아래 패널은 선택한 시점의 conntrack entry, TCP socket 수, listen drop·SYN cookie·conntrack drop·UDP 수신 buffer 오류와 softnet budget 초과의 초당 증가량을 보여 줍니다. `tcp_mem` 상한 대비 TCP memory page, orphan socket, TCP memory pressure 진입·메모리 부족 abort·수신 대기열 prune의 초당 증가량도 보여 주고, `/proc/net/dev`에 없는 장치 쪽 drop도 보여 줍니다. 수신 ring이 가득 차 버린 `rx_missed_errors`와 FIFO overrun인 `rx_fifo_errors`, `tx_fifo_errors`를 loopback을 뺀 namespace의 interface 전체로 더한 값입니다. `↑`/`↓`로 이전 sample을 보고, `Enter`로 그때 가장 바쁜 interface 다섯 개와 당시 설정을 펼치고, `h`로 최근 60초 최대값을 봅니다. `ct%`는 90%부터 경고, 98%부터 위험으로 표시하며 연결 실패가 확인됐다는 뜻은 아닙니다.

수집은 현재 network namespace를 기준으로 하지만 softnet 카운터와 TCP TIME_WAIT 수는 host 전체 값일 수 있습니다. TCP `CurrEstab`는 ESTABLISHED와 CLOSE_WAIT를 포함합니다. socket 수는 임시 port 사용률이 아닙니다. 카운터 읽기 실패·초기화·기준점 부재 시 rate는 `—`로 표시합니다. conntrack 상세 통계는 `/proc/net/stat/nf_conntrack`이 노출될 때 수집하며, 없으면 해당 값만 빠집니다.

`--json`의 `network_limits`에는 namespace, 설정(`settings`), 현재값(`gauges`), 누적값(`counters`), 초당 증가량(`rates`)이 들어갑니다. `counters`와 `rates`에는 `tcp_retrans_segs`, `tcp_out_rsts`, `tcp_attempt_fails`, `tcp_memory_pressures`, `tcp_abort_on_memory`, `tcp_rcv_pruned`, `nic_rx_missed`, `nic_rx_fifo`, `nic_tx_fifo`도 있습니다. `gauges`에는 `tcp_orphans`와 `tcp_memory_pages`가, `settings`에는 `net.ipv4.tcp_mem`이 있습니다. 각 값에는 `status`와 필요한 경우 `reason`이 있으며, 관측하지 못한 숫자는 생략됩니다. 파일에 저장하면 외부 도구로 실행 후 추이를 분석할 수 있습니다.

## Top 이벤트

`edc top`은 `signal` 열의 경고가 5초 이상 이어지면 이벤트로 남깁니다. 꺼졌다가 5초 안에 다시 켜진 경고는 같은 이벤트로 이어집니다. 이벤트는 최근 100개까지 메모리에만 보관하며, 대시보드를 닫으면 사라집니다.

이벤트마다 가장 나빴던 값과 그 시점에 앞선 묶음과 process를 저장합니다. 후보는 경고를 자세히 보이는 보기의 기준을 따릅니다. disk와 pressure 경고는 I/O, memory 경고는 `MEM`, 나머지는 CPU이고, network 경고에는 후보를 붙이지 않습니다. 증가 경고는 늘어나는 process를 후보로 적습니다. 후보가 원인이라고 단정하지 않습니다.

장애 하나가 여러 경고를 동시에 켜는 일이 많습니다. disk가 포화하면 `await`, `blocked`, `load`, `i/o`, `psi io`가 함께 오릅니다. 첫 이벤트에서 5초 안에 시작한 이벤트는 한 줄로 합치고, 가장 심한 경고와 나머지 개수를 `await 2034ms +4`처럼 표시합니다.

| 표시 | 뜻 |
|---|---|
| `●` | 아직 이어지는 이벤트 |
| 시각 앞의 `≤` | 첫 sample에서 이미 켜져 있던 경고. 실제 시작은 더 이를 수 있음 |
| 이력 행 시각 뒤의 `!` | 그 행에서 이벤트가 시작됨 |

아래 영역에 최근 이벤트를 표시합니다. 자리가 없으면 패널 위 한 줄에 아직 이어지는 이벤트 중 가장 심한 것을 표시합니다.

`e`를 누르면 이벤트 목록이 열립니다. 항목마다 가장 심한 경고, 함께 켜진 경고의 최고값, 후보를 보여 줍니다. `Enter`를 누르면 그 항목의 시작 시점으로 이동합니다. 이력은 500행만 보관하므로 오래된 이벤트는 요약만 남습니다. `[`와 `]`는 이전·다음 항목의 시작 시점으로 이동합니다.

## Top JSON 출력

`--json`을 쓰면 sample마다 JSON 객체를 한 줄씩 씁니다. stdout으로 보내려면 `-`를 씁니다. 경로를 주면 mode 0600으로 새 파일을 만듭니다.

```bash
./bin/edc top --count 5 --json -
```

각 줄에는 `time`, `hostname`, `cores`, 초당 byte 단위 network·disk rate, 퍼센트 단위 CPU 값, `load1`, `memory_pct`, 초당 byte 단위 `swap_out_bytes_per_s`가 들어갑니다. macOS와 Linux 모두 network errors·drops와 disk IOPS·await를 내보냅니다. Linux에서는 disk busy, PSI `some avg10`, memory·I/O PSI `full avg10`도 추가되며, `*_health_supported`, `disk_busy_supported`, `psi_supported`가 지원 여부를 표시합니다. `memory_pressure_supported`는 host가 memory 압박 단계를 알려 줬는지를 표시하고, 그때 macOS는 `memory_pressure`(`normal`, `warn`, `critical`)를 추가합니다. `--json`은 표와 헤더를 없앱니다.

## Top 저장과 이력 조회

`--write [path]` 또는 `-w [path]`로 관측값을 로컬 SQLite DB에 누적합니다. 대시보드는 유지하며, `--count`, 파이프, `--json`의 출력 방식은 그대로입니다. `--write`와 함께 쓰면 표에서도 `--process`를 사용할 수 있습니다. `-w`만 주면 Linux는 `~/.local/state/edc/history.db`, macOS는 `~/Library/Application Support/edc/history.db`를 사용합니다. 두 플랫폼 모두 절대 경로인 `XDG_STATE_HOME`이 있으면 `$XDG_STATE_HOME/edc/history.db`를 사용합니다. 기본 디렉터리는 권한 `0700`으로 자동 생성합니다. `-w`나 `--write`를 생략하면 저장하지 않습니다.

```bash
./bin/edc top -w
./bin/edc history list
./bin/edc top -w incident.db
./bin/edc top --process nginx --count 60 -w incident.db
./bin/edc top --count 10 -w incident.db --json samples.jsonl
./bin/edc history list incident.db
./bin/edc history top --metric memory_pct --min 90 incident.db
./bin/edc history top --from 2026-10-05T09:00:00+09:00 --to 2026-10-05T10:00:00+09:00 incident.db
./bin/edc history process --process nginx --metric cpu_pct --min 100 incident.db
./bin/edc history top --run <run-id> --limit 0 --json - incident.db
```

같은 DB를 지정해도 이전 값을 덮어쓰지 않고 별도 실행으로 추가합니다. 실행마다 host 정보, edc version, 수집 조건, 시작·종료 시각, 상태, sample 수를 남깁니다. `unfinished`는 종료 기록이 없는 실행으로, 실행 중이거나 강제 종료된 상태일 수 있습니다. 정상 종료는 대기 중인 기록을 모두 저장하고, 저장 실패는 관측을 중단하며 종료 코드 `1`을 반환합니다. 강제 종료 시 이미 commit된 transaction은 보존됩니다. 새 DB 권한은 `0600`이며 자동 삭제나 회전은 없습니다.

host 지표와 core별 CPU, 지원 여부, 실제 관측 구간, 당시 process 필터를 저장합니다. 필터가 없으면 CPU 상위 5개와 `MEM` 상위 5개의 합집합(최대 10개), 필터가 있으면 최대 50개와 전체 합계를 저장합니다. 전체 process를 기록하는 기능은 아닙니다. `p`로 일시정지하면 기록도 멈추고, 재개 시 새 기준점 다음부터 rate를 기록합니다. process는 host보다 덜 자주 갱신될 수 있으며 `process_observed_at`으로 수집 시각을 구분합니다. 시작 시각을 읽을 수 있으면 PID와 정밀한 시작 시각으로 PID 재사용을 구분합니다.

`history list`는 실행 목록, `history top`과 `history process`는 sample을 최신순으로 보여 줍니다. DB 경로를 생략하면 같은 기본 DB를 읽으며, 없는 DB를 생성하지는 않습니다. `--run`은 실행 ID, `--from`과 `--to`는 RFC3339 시각으로 범위를 좁힙니다. 시작 시각은 포함하고 종료 시각은 제외합니다. `--limit`은 기본 200이며 `0`은 전체입니다. `--json <path|->`는 결과당 JSON 한 줄입니다. Go flag 규칙에 따라 옵션은 DB 경로 앞에 둡니다.

`top`과 `process` 조회는 `--metric <field>`와 `--min`, `--max`를 지원하며 경계값을 포함합니다. `memory_pct`, `disk_await_ms`, `rss_bytes`, `disk_read_bytes_per_s`처럼 JSON 최상위 숫자 필드 이름을 씁니다. `cpu_pct`는 host에서는 user+system 합, process에서는 기존의 core 하나가 100%인 값입니다. 미지원·미측정 SQL 값은 NULL로 저장하여 숫자 조건에 맞지 않게 하고, JSON에는 지원 상태를 보존합니다. 이름·PID 검색은 기존 `top --process` 규칙을 따릅니다.

WAL을 사용하므로 기록 중에도 조회할 수 있습니다. DB는 로컬 파일시스템에 둡니다. 외부 SQLite 도구로 `runs`, `top_samples`, `top_process_samples`도 조회할 수 있습니다. 숫자 지표는 column, cgroup·eBPF 등 상세 정보는 `payload` JSON에 보존합니다. 조회는 기존 DB를 읽으며 생성이나 migration을 하지 않습니다. 다른 DB와 지원하지 않는 schema version은 거부합니다. JSON 출력은 DB나 journal 파일을 덮어쓸 수 없습니다.

## Top process 필터

`--process <filter>`는 process 목록을 지정한 process로 좁힙니다. filter는 쉼표로 구분한 목록입니다. 숫자는 PID와 같아야 하고, 그 밖의 항목은 command 이름의 일부와 대소문자를 가리지 않고 맞아야 합니다. 항목 하나라도 맞으면 목록에 남습니다. command 이름은 Linux에서는 kernel이 15자로 자르는 `comm`, macOS에서는 실행 파일 경로입니다.

```bash
# 대시보드: process 보기로 열리고, Enter로 가장 바쁜 process를 봅니다
./bin/edc top --process output-mesh
# JSON 줄: sample마다 processes 배열을 더합니다
./bin/edc top --process 4321,worker --json /tmp/edc-host.jsonl
```

`--process`를 주면 대시보드 제목 아래에 강조된 `PROCESS` 막대가 나옵니다. 막대에는 필터와 맞은 process들의 현재 값이 나오고, 터미널이 좁으면 뒤쪽 값부터 뺍니다. 대시보드는 process 보기로 열립니다. 행마다 한 시점에 맞은 process 묶음의 값을 보여 줍니다. 맞은 개수, CPU% 합(core 하나가 100), 메모리(`mem`), thread 수, 열린 파일 수, 초당 디스크 읽기와 쓰기입니다. `--ebpf`를 주면 run-queue 평균 대기와 block I/O 평균 지연(ms)도 보입니다. 읽지 못한 값은 `—`로 나옵니다. `1`을 누르면 host 열로, `f`를 누르면 process 보기로 돌아갑니다. `Enter`는 선택한 행에서 가장 바쁜 process를 보여 줍니다.

대시보드를 실행하는 중에도 process를 고를 수 있습니다. 행을 고르고 `f`를 누릅니다. 첫 signal이 `await 65ms`처럼 host 값이면 해당 보기를 엽니다. `f`를 한 번 더 누르거나 process signal에서 누르면 후보 선택으로 이동합니다. 화살표로 고르고 `Enter`로 해당 PID에 초점을 맞춥니다. `/`로 필터를 입력합니다. `Esc`는 후보 선택을 닫고, 선택 중이 아닐 때는 필터를 지웁니다. 다른 필터로 수집한 행의 process 값은 `—`로 나옵니다.

filter는 CPU 상위로 자르기 전에 적용하므로 CPU가 낮은 process도 맞으면 목록에 남습니다.

대시보드 상세 보기는 두 줄입니다. 첫 줄은 맞은 process 전체의 합으로, 개수, CPU, 메모리, thread, 열린 file descriptor, 디스크 I/O입니다. 둘째 줄은 CPU가 높은 세 개와 나머지 개수 `+N`입니다. 이때 `signal` 열은 맞는 process만 반영합니다.

`--json`에서는 sample에 필드 두 개가 더해집니다. `processes`는 맞는 process를 CPU가 높은 순으로 최대 50개 담습니다. `process_total`은 맞은 process 전체의 합으로 `count`, `cpu_pct`, `rss_bytes`, `memory_bytes`, `threads`입니다. CPU는 core 하나가 100%이고, 맞는 process가 없으면 `processes`는 `[]`입니다. process마다 들어가는 필드는 다음과 같습니다.

| 필드 | 뜻 |
|---|---|
| `pid`, `started` | `started`는 UTC 시작 시각입니다. PID는 재사용될 수 있어 `(pid, started)`가 process 하나를 가리킵니다. |
| `command`, `cpu_pct`, `rss_bytes` | 이름, 직전 refresh 이후 CPU, 상주 memory |
| `memory_bytes` | process가 차지한 메모리. macOS는 physical footprint, Linux는 RSS와 `VmSwap`의 합. `process_total`에서는 하나라도 읽지 못하면 빠짐 |
| `threads` | thread 수 (Linux·macOS) |
| `fds` | 열린 file descriptor 수 (Linux) |
| `disk_read_bytes_per_s`, `disk_write_bytes_per_s` | 초당 disk I/O byte. Linux는 `/proc/<pid>/io`, macOS는 libproc 기준 |

`edc`가 읽지 못한 필드는 0이 아니라 빠집니다. Linux에서 `fds`와 디스크 필드는 같은 사용자거나 root여야 읽습니다. 이 값은 목록에 남은 process만 읽으므로 많이 맞는 filter도 읽는 process는 최대 50개입니다. macOS는 libproc으로 thread 수와 disk I/O를 수집하고, `fds`는 수집하지 않습니다. macOS CPU 값은 기존 `ps`의 최근 감쇠 평균을 유지합니다. 디스크 rate는 같은 process를 두 번 읽어야 나오며, 대시보드는 기준점 대기나 libproc 오류를 표시합니다. `--process`가 없으면 `--json` 출력에 이 필드들이 없습니다.

`no ev`는 eBPF 관측기가 켜져 있지만 해당 구간에 이벤트가 없었다는 뜻입니다. 지연이 0이라는 뜻은 아닙니다.

`--process`는 대시보드나 `--json`, `--write`에서 쓸 수 있습니다. 표에는 process 열이 없으므로 `edc top --process x --count 5`는 종료 코드 `2`로 멈춥니다.

### 프로세스 자원 한도 (Linux)

`--process`의 프로세스 패널은 선택한 프로세스의 FD 사용량과 soft limit을 표시합니다. 이력 행에서 `Enter`를 누르면 목록에 남은 프로세스의 자원 한도를 확인할 수 있습니다.

cgroup v2에서는 그룹 메모리 사용량과 로컬 한도, 로컬 OOM·OOM kill 횟수, 관측 구간의 CPU throttling 횟수와 시간을 표시합니다. 같은 그룹은 상세 화면에 한 번만 표시하며, 그룹 값을 프로세스별 값으로 합산하지 않습니다.

메모리 사용량에는 하위 그룹이 포함되지만 표시한 로컬 한도에는 상위 그룹의 한도가 반영되지 않습니다. OOM 횟수는 해당 그룹이 만들어진 뒤의 누적값이며 하위 그룹의 이벤트는 제외합니다. CPU throttling 값은 그룹 자체의 CPU 한도에서 발생한 값이며 상위 그룹 한도에서 발생한 값은 포함하지 않습니다.

CPU 첫 표본은 비교 기준만 수집합니다. 카운터가 감소하거나 관측이 끊기면 기준을 다시 수집합니다.

JSON에는 프로세스별 `limits.fd`와 `limits.cgroup`이 추가됩니다. 각 지표는 `status`와 필요한 경우 `reason`을 제공하며, 읽지 못한 수치는 0 대신 생략합니다. 무제한 FD·메모리 한도는 별도의 boolean 필드로 구분합니다.

미지원, 권한 부족, namespace에서 접근할 수 없는 경로, 읽기 오류를 구분합니다. cgroup v1과 macOS의 자원 한도는 미지원으로 표시합니다. 수집 기준은 [Linux cgroup v2 인터페이스](https://docs.kernel.org/admin-guide/cgroup-v2.html)를 따릅니다.

### `-d`로 CPU 대기와 I/O 지연 보기

`-d` 또는 `--detail`은 `/proc`으로는 얻을 수 없는 값을 더합니다. 맞은 process가 CPU를 기다린 시간과 block I/O에 걸린 시간입니다. `--process`가 필요합니다. Linux에서는 root나 `CAP_BPF`와 `CAP_PERFMON`, 커널 BTF도 있어야 합니다. 없으면 `edc top`은 종료 코드 `3`으로 멈추고, 지원하지 않는 호스트인지 capability가 빠졌는지 알려 줍니다. Linux 5.15와 6.17에서 시험했습니다. Linux 5.15의 `sched_switch` tracepoint는 `prev_state`를 넘기지 않으므로, 그 커널에서는 `edc`가 task 상태를 직접 읽습니다.

`--ebpf`도 같은 옵션입니다. 이전 이름을 쓰는 스크립트를 위해 남겨 두었습니다. 대시보드에서는 `--process` 없이 `-d`만 줘도 됩니다. 이때 값은 `f`나 `/`로 process를 고른 뒤부터 나옵니다. 이 옵션을 주면 `PROCESS` 막대와 process 보기에 run-queue 대기와 I/O 지연이 함께 나옵니다.

```bash
sudo ./bin/edc top --process output-mesh -d
sudo ./bin/edc top --process output-mesh -d --json /tmp/edc-host.jsonl
```

대시보드는 상세 보기에 세 번째 줄을 더합니다. 예: `ebpf 1s · runq 7584 avg 5.80ms p95 <16.384ms · io 704 avg 0.07ms p95 <0.256ms`. Linux에서는 이 줄 끝에 대기 시간이 긴 syscall 세 개가 붙습니다. 예: `· wait futex 1.20s epoll_wait 800.00ms read 12.00ms`. `--json`에서는 process마다, 그리고 `process_total`에 `ebpf` 객체가 붙습니다.

| 필드 | 뜻 |
|---|---|
| `window_s` | 이 개수가 다루는 시간(초). 직전 sample 이후입니다. |
| `io_supported`, `p95_supported` | 이 source가 block I/O를 세고 분포를 남기는지입니다. `ebpf`는 둘 다 `true`, `libproc`은 둘 다 `false`입니다. |
| `runq_count`, `runq_avg_ms`, `runq_p95_ms` | Linux에서는 process의 thread가 실행 가능 상태가 된 뒤 CPU를 받기까지의 횟수와 대기 시간입니다. macOS에서는 이 시간 동안의 context switch 수와 switch 한 번당 평균 대기이고 p95는 없습니다. `cpu_pct`가 낮은데 대기가 길면 CPU가 모자란 것입니다. |
| `io_ops`, `io_bytes`, `io_avg_ms`, `io_p95_ms` | process가 낸 block I/O 요청 수, byte, 요청부터 완료까지의 시간 |
| `waits` | Linux 전용입니다. 이 시간 동안 대기가 가장 긴 syscall을 최대 5개 보여 줍니다. 대기는 process의 스레드가 잠들며 CPU를 떠날 때 시작해 다시 실행될 때 끝납니다. 항목마다 `syscall`, `count`, `total_ms`가 있습니다. `-`는 page fault처럼 syscall 밖에서 기다린 대기입니다. |
| `context_switches`, `unreadable` | macOS 전용입니다. 이 시간 동안의 context switch 수(`runq_count`와 같은 값)와 macOS가 읽지 못하게 막은 process 수입니다. |

`p95`는 95번째 백분위가 든 2의 거듭제곱 구간의 위쪽 경계라서 실제 값은 그 아래입니다. 이벤트가 없으면 지연 값은 빠집니다. `process_total`은 맞은 process 전체를 합치고, p95도 합친 분포에서 구합니다. `edc`는 맞은 process 중 가장 바쁜 4096개까지 감시합니다.

- block I/O 요청은 그것을 낸 task에 속합니다. 동기 읽기, direct I/O, `fsync`는 그 process로 잡힙니다. 버퍼 쓰기는 나중에 커널 flusher가 내므로 `kworker`로 잡힙니다. 그 byte는 `disk_write_bytes_per_s`를 쓰세요.
- `edc`가 커널의 PID를 자신이 속한 PID namespace 기준으로 바꾸므로 컨테이너 안에서도 필터가 맞습니다.
- process가 나타난 뒤 첫 sample에는 `ebpf` 객체가 없습니다. `edc`가 그 process를 감시하기 시작할 때부터 세기 때문입니다.
- `ebpf` 객체의 `source`는 값을 센 방법입니다. Linux는 `ebpf`, macOS는 `libproc`입니다.
- 대기는 끝난 sample에서 셉니다. window 내내 잠든 스레드는 그 window에 더해지지 않습니다. 대기에는 깨어난 뒤 CPU를 기다린 run-queue 대기도 들어 있으므로 `waits`와 `runq_avg_ms`를 더하지 마세요. `edc`가 process를 감시하기 시작한 뒤에는, 스레드가 한 번 깨어난 다음부터 그 스레드의 대기를 셉니다.

macOS에는 scheduler와 block I/O event를 주는 공개 hook이 없습니다. 그래서 `-d`는 root 없이 libproc의 누적 counter로 CPU 대기만 셉니다. 대기 시간은 실행 가능했던 시간에서 CPU를 쓴 시간을 뺀 값이고, `runq_count`는 그동안의 context switch 수입니다. 따라서 `runq_avg_ms`는 switch 한 번당 평균 대기입니다. 분포가 없어 `runq_p95_ms`는 빠지고, I/O 값은 셀 수 없으므로 `io_*` 필드가 없습니다. 대시보드는 그 칸을 `n/a`로 보입니다. root가 아니면 macOS는 다른 사용자의 process를 읽지 못하게 막습니다. `edc`는 그 수를 `unreadable`에 세고, 맞은 process를 하나도 읽지 못했으면 `runq_*` 필드를 빼며, 대시보드에는 `root`를 보입니다. x86_64 build는 Rosetta에서 `-d`를 멈춥니다. 번역된 시계가 kernel counter와 맞지 않을 수 있기 때문이니 arm64 build를 쓰세요.

```bash
./bin/edc top --process Safari -d
```

macOS의 process `STATE`에서 `U`(uninterruptible wait)는 Linux의 `D`처럼 `iowait`로 보이고 blocked 후보에 들어갑니다.

## AI 도구 사용량

`edc ai`는 이 host에서 Claude Code와 Codex가 쓴 token을 보여 줍니다. 두 도구의 계정 한도와 reset 시각도 함께 보여 줍니다.

```bash
./bin/edc ai                      # 대시보드 (q로 종료)
./bin/edc ai --count 3            # sample 3개를 표로 출력
./bin/edc ai --count 1 --json -   # 한 줄 JSON
```

터미널에서는 대시보드를 엽니다. 표의 한 행은 한 구간이고, 구간은 10s, 1m, 5m, 1h 중 하나입니다. `+`와 `-`로 구간을 바꿉니다. `▸` 행은 아직 끝나지 않은 구간입니다. `↑`, `↓`, PgUp, PgDn, End로 24시간 기록을 오갑니다. `Σ` 행은 최근 10분, 1시간, 24시간의 합계입니다.

token은 이 host의 로그에서 셉니다. 2초마다 새로 붙은 줄만 읽습니다.

- Claude Code: `~/.claude/projects/**/*.jsonl`. 같은 메시지는 한 번만 셉니다.
- Codex: `~/.codex/sessions`에 쌓이는 누적 token 수의 증가분입니다.

표는 이 host만 셉니다. 다른 기기나 claude.ai에서 쓴 양은 한도 비율에만 나타납니다.

`in`은 새로 보낸 입력이고, `cache`는 prompt cache에서 읽은 입력입니다. `total`은 `in + out`이고 `cache`는 넣지 않습니다. cache 읽기는 대화가 길수록 커져서 실제로 처리한 양을 가리기 때문입니다. `hit`은 `cache / (in + cache)`를 내림한 값입니다. Claude의 `in`에는 cache에 새로 쓴 token도 들어가므로, 이 token은 miss로 셉니다. 입력이 없는 구간의 `hit`은 `-`로 표시합니다. `--json`에는 소수점 한 자리의 `cache_hit_percent`가 들어가고, 입력이 없는 구간에서는 이 필드를 뺍니다. 표는 88열이 필요합니다.

한도 상자는 한도 구간마다 사용률, reset 시각, 남은 시간을 보여 줍니다.

- Codex: `codex app-server`를 띄워 묻습니다. `codex` 명령이 `PATH`에 있어야 합니다.
- Linux의 Claude: Claude Code의 `/usage`가 쓰는 API인 `https://api.anthropic.com/api/oauth/usage`를 부릅니다. 이때 `~/.claude/.credentials.json`의 token을 보냅니다. edc는 이 token을 출력하거나 저장하거나 갱신하지 않습니다.
- macOS의 Claude: Claude Code의 상태 줄이 `edc ai statusline`에 넘긴 사용량을 읽습니다. 이 절의 끝을 봅니다.

Claude 사용량 API는 공개 문서가 없고, 자주 부르면 HTTP 429로 거절합니다. 그래서 edc는 최소 5분 간격으로 부릅니다. 429를 받으면 10분, 20분, 최대 30분으로 간격을 늘립니다. 다음 조회 전까지는 저장한 값과 그 값이 얼마나 오래됐는지를 보여 줍니다.

`--poll`은 한도 조회 간격입니다. 기본값은 60s이고 30s 이상이어야 합니다. Linux의 Claude는 이와 별도로 5분 이상 간격을 지킵니다.

터미널이 아니거나 `--count` 또는 `--json`을 주면 대시보드 대신 sample을 출력합니다. `--count N`은 sample N개를 출력하고 끝나며, 0이면 멈출 때까지 계속합니다. `--json <경로|->`는 sample마다 JSON 객체 하나를 씁니다. 파일은 mode 0600으로 만듭니다.

감지한 reset은 `ai-resets.jsonl`에, 마지막 Claude 값은 `ai-claude.json`에 남깁니다. 두 파일은 history DB와 같은 디렉터리에 있습니다. 위치는 [Top 저장과 이력 조회](#top-저장과-이력-조회)를 봅니다.

macOS의 Claude Code는 token을 Keychain에 둡니다. edc는 Keychain을 읽지 않습니다. 대신 Claude Code가 상태 줄 명령에 넘기는 5시간·7일 한도를 씁니다. `edc ai statusline`이 이 값을 같은 디렉터리의 `ai-claude-statusline.json`에 남깁니다.

상태 줄을 연결하려면 edc를 계속 쓸 위치에 먼저 설치한 뒤 다음 명령을 실행합니다. 이 명령은 실행한 edc의 경로를 설정에 넣습니다.

```sh
edc ai statusline install
```

이 명령은 `~/.claude/settings.json`의 `statusLine.command`만 바꿉니다. 원래 명령은 그대로 실행되므로, `cship`은 `edc ai statusline -- cship`이 됩니다. 원래 파일은 `settings.json.edc-backup`에 남습니다. 되돌리려면 `edc ai statusline uninstall`을 실행합니다.

값은 Claude Code가 마지막으로 요청했을 때의 값입니다. 상자에는 경과 시간을 늘 보여 주고, 텍스트 출력은 1분 이상 지난 값에만 `updated` 줄을 더합니다. 상태 줄 입력에 요금제가 없으므로 macOS의 상자에는 요금제 이름이 나오지 않습니다.

## Remote recipe

`edc remote <group>`은 inventory group에 YAML recipe를 실행합니다. 로컬 OpenSSH 설정, agent, known host 검사를 그대로 씁니다.

각 command는 원격 계정의 기본 shell을 대화형으로 띄웁니다. shell 시작 출력과 prompt hook은 화면에 나오지 않습니다.

inventory와 recipe 파일에는 비밀번호와 개인 키를 넣지 않습니다. SSH alias는 `~/.ssh/config`에 설정합니다.

step마다 `name`과 `command` 또는 `upload` 중 하나가 필요합니다. `verify`는 선택입니다. `verify`가 없으면 action 결과가 step 결과를 정합니다.

파일을 전송하려면 `command` 대신 `upload`를 씁니다. `source`는 로컬 파일이고 `destination`은 원격 경로입니다. `mode`는 선택적인 octal 권한입니다.

`upload`는 OpenSSH의 `scp`를 실행합니다. `mode`를 지정하면 전송 후 원격에서 `chmod`를 실행합니다.

group에서 참조하려면 `name`을 유지합니다. `target`이 없으면 `edc`는 `name`을 SSH target으로 씁니다.

```yaml
name: deploy
steps:
  - name: upload-config
    upload:
      source: ./config/app.yaml
      destination: /etc/myapp/app.yaml
      mode: "0644"
    verify: test -f /etc/myapp/app.yaml
```

![edc remote daily --dry-run이 SSH 연결 없이 host x step 계획 표를 출력하고, tags가 맞지 않는 step을 –로 표시하는 화면](docs/media/remote.gif)

데모는 recipe의 tags와 `--dry-run`이 출력하는 계획 표를 보여 줍니다.

### 명령 형태

group은 위치 인자입니다. `--group` flag는 같은 값을 받는 별칭입니다. 둘 중 하나만 씁니다.

```bash
cp examples/remote/inventory.yaml ./inventory.yaml
./bin/edc remote              # group을 고른 다음 확인
./bin/edc remote daily        # group을 지정한 다음 확인
./bin/edc remote daily -f     # group을 지정하고 확인을 생략
```

`edc`는 다음 디렉터리에서 차례로 `inventory.yaml`을 찾습니다.

1. `./.edc/`
2. `./`
3. `os.UserConfigDir()/edc/`. 이 경로는 운영체제를 따릅니다.

`recipe.yaml`도 같은 순서로 찾습니다. 파일 선택기도 같은 디렉터리의 YAML 파일을 같은 순서로 나열합니다.

프로젝트의 remote 파일을 `./.edc/`에 모으면 프로젝트 루트에 YAML이 쌓이지 않습니다. 이 디렉터리를 git에서 빼려면 안에 `.gitignore`를 둡니다. `edc`는 이 파일을 만들지 않습니다.

```bash
mkdir -p .edc
printf '*\n' > .edc/.gitignore
```

파일을 커밋하려면 `.edc/.gitignore`를 지웁니다.

recipe의 `upload.source`에 쓴 상대 경로는 recipe 파일이 있는 디렉터리가 아니라 `edc`를 실행한 디렉터리를 기준으로 합니다.

`-v`를 더하면 탐색 순서를 보여 줍니다. 없는 디렉터리와 `.gitignore`가 없는 `./.edc/`에는 표시가 붙습니다. `--list -v`도 같은 줄을 보여 줍니다.

```
inventory  ./.edc/inventory.yaml      recipe  ./.edc/recipe.yaml
search     ./.edc/  →  ./  →  /home/me/.config/edc/ (없음)
```

`--inventory`와 `--recipe`는 찾은 파일보다 우선합니다.

group을 지정하면 `edc`는 경로를 묻지 않습니다. 계획을 보여 주고 확인만 받습니다.

group을 지정하지 않으면 `edc`는 group부터 고릅니다. 그다음 inventory 경로를 보여 주고, recipe를 고르고, 확인을 받습니다.

선택을 마치면 계획 맨 위에 `edc remote` 명령이 나옵니다. 이 명령에는 고른 group, inventory, recipe와 직접 입력한 flag가 들어갑니다. 이 명령을 실행하면 같은 선택으로 확인만 받습니다.

대화형 선택기는 목록을 그 자리에 그립니다. 위아래 방향키나 `j`, `k`로 움직입니다. Enter로 선택하고, `q`나 Esc로 취소합니다. 취소하면 exit code `4`를 돌려줍니다.

고르는 동안 `edc`는 커서가 놓인 행의 색을 반전하고 왼쪽에 `▌` 막대를 붙입니다. 반전 덕분에 흑백 terminal에서도 행이 분명합니다.

선택한 뒤에도 목록은 화면에 남습니다. 막대는 고른 항목에 반전 없이 남고, 첫 줄에는 질문 이름이 그대로 있습니다.

```
inventory 파일
  lab-hosts.yaml  ·  group 1개, host 1개
▌ prod-hosts.yaml  ·  group 2개, host 2개
```

`inventory.yaml`을 찾지 못하면 `edc`는 탐색 디렉터리의 YAML 파일 중 inventory로 읽히는 것을 나열합니다. 목록에는 group과 host 개수가 나옵니다. recipe 목록에는 recipe 이름과 step 개수가 나옵니다. inventory도 recipe도 아닌 YAML 파일은 숨깁니다. 목록이 비면 `edc`는 경로를 묻습니다.

확인 질문은 질문, 두 답, key 안내를 한 줄에 놓습니다. 좌우 방향키로 옮기고 Enter로 답하거나, `y`나 `n`으로 바로 답합니다. 가리킨 답에는 `▌` 막대와 반전 색이 붙습니다. 기본 답은 아니오입니다.

`-f`나 `--force`를 쓰면 확인을 생략합니다. `-v`와 함께 쓰면 출력을 흘려보냅니다. group을 지정하지 않은 채 `-f`를 쓰려면 inventory에 group이 정확히 하나 있어야 합니다.

recipe의 command는 terminal 없이 실행되고 입력이 비어 있습니다. step이 `host changes` command를 `--yes` 없이 실행하면 확인을 받을 수 없습니다. `edc disk grow`는 exit code `4`로 멈추고, `edc route switch`는 경로를 되돌리고, `edc change apply`는 타이머가 실행될 때 되돌립니다.

step에 `--yes`를 넣으면 `edc disk grow`와 `edc change apply`는 group의 모든 host에서 질문 없이 실행됩니다. `edc route switch`는 출구 신원이 일치할 때 그렇습니다. 확인하기 전에 plan을 읽고, 이런 recipe는 `-f`로 실행하지 마세요.

`run`, `list`, `plan`, `hosts`, `groups`는 앞으로 쓸 subcommand 이름이라 group 이름으로 예약돼 있습니다. 이 중 하나를 쓴 inventory는 로드에 실패합니다.

기존 `edc remote run` 형태는 없어졌습니다. `edc remote <group>`을 씁니다.

### Dry run과 inventory 목록

`-n`이나 `--dry-run`을 쓰면 계획만 출력하고 끝냅니다. `edc`는 SSH 연결을 열지 않습니다. `--json`을 더하면 계획을 JSON으로 받습니다.

```bash
./bin/edc remote daily --dry-run
./bin/edc remote daily --dry-run --json -
```

`-l`이나 `--list`를 쓰면 inventory의 group과 host를 출력합니다. group을 지정하면 그 group만 나옵니다.

```bash
./bin/edc remote --list
./bin/edc remote daily --list --json -
```

`--dry-run`과 `--list`는 `-f`와 함께 쓰지 못합니다. `--redact`를 주면 출력에서 IP 주소를 가립니다.

### Host tag

host와 step에 `tags`를 답니다. `tags`가 없는 step은 group의 모든 host에서 돕니다. `tags`가 있는 step은 같은 tag를 가진 host에서만 돕니다. 이렇게 하면 recipe 하나로 macOS와 Linux host에 다른 작업을 보냅니다.

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
  - name: git-kit          # tags가 없으므로 모든 host가 이 step을 실행합니다
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

step과 맞지 않는 host는 그 step의 결과를 받지 않습니다. report는 실패한 host에 대해서만 skip 개수를 남깁니다.

group의 어떤 host도 step의 tag와 맞지 않으면 `edc`는 stderr에 경고를 찍고 계속 진행합니다. tag 철자를 확인합니다.

### 표 하나

`edc remote`는 계획과 결과에 표 하나를 씁니다. 행은 host, 열은 step입니다. 표는 모든 칸을 `·`로 시작하고, 그 host의 step이 끝날 때마다 칸이 바뀝니다. step의 tag가 host와 맞지 않으면 칸에 `–`가 나옵니다.

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

표 아래 줄은 지금 도는 host, step, phase를 알려 줍니다. 그 아래 줄들은 step마다의 command를 보여 줍니다.

stdin과 stdout이 모두 terminal이면 확인 질문이 이 표 바로 아래, command 목록 위에 나옵니다. 가리킨 답에는 `▌` 막대와 반전 색이 붙습니다. 좌우 방향키와 Enter로 답하거나 `y`, `n`으로 답합니다. 그 줄은 곧 진행 줄로 바뀌고 표가 채워집니다. 질문을 건너뛰려면 `-f`를 씁니다.

```
host   uname  uptime
alpha  ·      ·

실행할까요?     예   ▌ 아니오       ←/→ 이동   Enter 선택   y/n 바로 답하기
```

표는 terminal 너비에 맞춥니다. `edc`는 열 이름을 먼저 줄이고, 그다음 `PASS`와 `FAIL`을 범례와 함께 `✓`, `✗`로 바꾸고, 마지막에 host 이름을 줄입니다.

`-v`를 더하면 표 아래에 마지막 출력 줄들을 보여 줍니다.

취소하려면 Ctrl-C를 누릅니다. `edc`는 돌던 command를 멈추고, 남은 step을 `SKIP`으로 표시하고, 요약을 출력하고, exit code `4`를 돌려줍니다. Ctrl-C를 한 번 더 누르면 화면을 즉시 닫습니다.

stdin이나 stdout이 terminal이 아니거나, `--json`을 지정했거나, `NO_COLOR`가 설정되면 표 대신 기존 결과 줄을 출력합니다.

### 자동화

cron이나 launchd에서는 flag를 모두 지정합니다. terminal이 아닌 실행은 입력을 기다리지 않고 확인도 건너뜁니다.

```bash
./bin/edc remote daily \
  --inventory ./inventory.yaml \
  --recipe ./examples/remote/daily-update.yaml \
  --parallel 2 \
  --json ./remote-report.json
```

terminal이 아닌 실행에는 group이 필요합니다. 위치 인자나 `--group`으로 지정합니다.

원격 command를 그대로 흘려보려면 `-v`나 `--verbose`를 씁니다.

`edc`는 step이 끝날 때마다 PASS나 FAIL 줄을 출력합니다. 마지막 출력은 실패 목록과, 개수·경과 시간을 담은 요약 한 줄입니다.

host를 동시에 돌리려면 inventory에 `parallel`을 설정합니다. group 하나에만 적용하려면 `group_options.<group>.parallel`을 씁니다.

`--parallel` option은 두 inventory 값보다 우선합니다. host 안에서 step은 여전히 순서대로 돕니다.

host는 inventory 순서로 돕니다. step은 recipe 순서로 command와 verify command를 실행합니다.

step이 실패하면 `edc`는 그 host의 나머지 step을 건너뜁니다. 다음 host는 그대로 진행합니다. 실패가 하나라도 있으면 exit code `1`을 돌려줍니다.

## Route 전환

`edc route`는 gateway VM의 기본 경로를 바꾸고, 실패하면 되돌립니다. Linux에서만 동작합니다. `switch`와 `rollback`은 root 권한이 필요하고 `edc`가 `sudo`를 붙이지 않습니다.

이 명령은 경로를 바꿀 gateway VM에서 직접 실행합니다. 원격에서 조종하는 방식이 아닙니다. `edc`가 `SSH_CONNECTION`을 읽어 그 변경이 지금 세션을 끊는지 판정하기 때문입니다.

```bash
edc route check                 # 현재 상태만 살펴봅니다. 아무것도 바꾸지 않습니다
edc route switch --to <name>    # 전환하고, 롤백 타이머를 무장하고, 검증하고, 확정합니다
edc route status                # 진행 중이거나 끝난 전환을 보여 줍니다
edc route rollback --state <f>  # 상태 파일 하나를 되돌립니다
```

`rollback`이 별도 명령인 이유는 무장한 타이머가 이 명령을 부르기 때문입니다. 타이머와 사람이 같은 코드 경로를 씁니다.

### 출구 목록

`switch --to`는 `exits.yaml`에 적힌 이름만 받습니다. next-hop 주소를 직접 입력하면 오타가 나도 커널이 exit code `0`을 돌려주기 때문에 실수가 드러나지 않습니다. 이름 목록이 그 실수를 막습니다.

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

`edc`는 `inventory.yaml`과 같은 순서로 `exits.yaml`을 찾습니다. `--exits <file>`로 다른 경로를 지정할 수 있습니다.

`expect_public_ip`는 그 출구가 인터넷에 내보이는 주소입니다. `edc`가 전환 뒤 공인 주소를 읽어 이 값과 대조합니다. 이 항목이 없으면 신원 확인을 건너뛰고 사람이 확정하게 남겨 둡니다.

### 안전장치

`edc`는 세 겹으로 막습니다. 각각 다른 실패를 잡습니다.

1. **프리플라이트.** next-hop의 이웃 항목을 읽습니다. `INCOMPLETE`나 `FAILED`면 바꿔도 반드시 실패하므로 거부하고 아무것도 바꾸지 않습니다. 항목이 아예 없는 것은 실패가 아니라 미확인이므로 막지 않습니다. `--force`로 거부를 넘길 수 있습니다.
2. **롤백 타이머.** 경로를 바꾸기 **전에** `edc route rollback`을 실행할 systemd 타이머를 무장합니다. 타이머는 PID 1이 들고 있어서 `edc` 프로세스가 죽어도 발화합니다. 무장에 실패하면 경로를 건드리지 않습니다.
3. **검증.** 변경 전후의 경로 개수를 비교하고, 목적지가 실제로 바뀐 경로를 타는지 대조하고, 새 출구의 공인 주소를 확인합니다.

`--seconds`로 롤백 유예를 정합니다. 10에서 900 사이이고 기본값은 120입니다.

```bash
edc route switch --to lab-nat-02 --dry-run   # 계획만 출력하고 아무것도 바꾸지 않습니다
edc route switch --to lab-nat-02 --seconds 60
```

`--dry-run`은 어떤 경로를 바꾸는지, 실행할 명령이 무엇인지, 어떤 타이머를 무장하는지, 출구가 닿는지를 보여 줍니다. root 권한이 없어도 됩니다.

전환은 기존 연결을 끊습니다. NAT 상태가 이전 출구에 남아 있어 이미 맺어진 흐름은 멈춥니다. 새로 맺는 흐름만 새 출구를 씁니다.

## 보호된 호스트 변경

접속을 막을 수 있는 호스트 변경에는 `edc change`를 씁니다. Linux에서만 동작합니다. 대상 호스트에서 root로 실행합니다.

`edc`는 대상을 바꾸기 전에 현재 상태를 저장하고 systemd 타이머를 무장합니다. 확정이 오지 않으면 타이머가 rollback 명령을 실행합니다.

SSH 공개 키 파일을 바꿉니다.

```bash
edc change apply \
  --kind authorized-keys \
  --path /home/<user>/.ssh/authorized_keys \
  --content-file /path/to/authorized_keys \
  --seconds 120
```

IPv4 방화벽 규칙을 바꿉니다.

```bash
edc change apply \
  --kind iptables \
  --rules-file /path/to/rules.v4 \
  --seconds 120
```

apply 결과의 run ID로 변경을 확정합니다.

```bash
edc change confirm --state /run/edc/change-<run-id>.json
```

타이머가 실행되기 전에 되돌리려면 같은 상태 파일 경로를 씁니다.

```bash
edc change rollback --state /run/edc/change-<run-id>.json
```

대기 중인 변경과 남은 시간을 봅니다.

```bash
edc change status
```

`authorized-keys`는 공개 키가 담긴 일반 파일을 받습니다. `iptables`는 타이머를 무장하기 전에 `iptables-restore --test`로 규칙을 검사합니다.

보호된 변경은 한 번에 하나만 실행하세요. apply 뒤에 대상이 바뀌면 `edc`는 rollback할 때 새 상태를 덮어쓰지 않고 거부합니다.

`--yes`를 쓰면 바로 확정합니다. 이 경우 rollback 유예가 사라집니다.

## 디스크 증설

AWS EBS 볼륨이나 OCI 블록 볼륨처럼 클라우드 볼륨을 키운 뒤에 `edc disk`를 실행합니다. Linux에서만 동작합니다. `grow`는 root 권한이 필요하고 `edc`가 `sudo`를 붙이지 않습니다.

```bash
edc disk check                 # 늘릴 수 있는 마운트를 보여 줍니다. 아무것도 바꾸지 않습니다
edc disk check /data           # 마운트 하나 아래의 층을 보여 줍니다
edc disk grow /data -n         # 계획만 출력하고 아무것도 바꾸지 않습니다
edc disk grow /data            # 계획을 보여 주고, 확인을 받은 뒤 늘립니다
```

`edc`는 `/proc/self/mountinfo`와 `/sys/class/block`에서 디스크, 파티션, LVM 물리 볼륨(PV), 논리 볼륨(LV), 파일시스템 층을 읽습니다. 그리고 공간을 늘리는 단계만 실행합니다.

1. 디스크가 SCSI면 `rescan`합니다. OCI 문서의 순서대로 블록 하나를 직접 읽은 뒤 `/sys/class/block/<disk>/device/rescan`에 씁니다. NVMe와 virtio 디스크에는 rescan 파일이 없고, 커널이 새 크기를 바로 봅니다.
2. 파티션이 디스크보다 짧으면 `growpart <disk> <number>`를 실행합니다.
3. PV가 파티션보다 짧으면 `pvresize <pv>`를 실행합니다.
4. 볼륨 그룹에 남은 공간이 있으면 `lvextend -l +100%FREE <lv>`를 실행합니다. 남은 공간을 모두 LV에 줍니다.
5. ext3와 ext4는 `resize2fs <device>`, xfs는 `xfs_growfs -d <mount>`로 파일시스템을 늘립니다.

단계마다 층을 다시 읽어 그 층이 실제로 늘었는지 확인합니다. 어떤 단계가 실패하면 같은 명령을 다시 실행하세요. 이미 늘어난 층은 계획에서 빠지므로 실패한 단계부터 이어서 진행합니다.

시간 제한은 계획을 세우기 전의 읽기에만 적용합니다. 디스크를 바꾸는 단계는 시간 제한으로 끊지 않습니다. `edc`가 명령을 죽여도 커널은 확장을 끝까지 하기 때문입니다. Ctrl+C를 누르면 지금 단계를 끝낸 뒤 다음 단계 앞에서 멈춥니다.

늘린 크기는 되돌릴 수 없습니다. 파티션과 파일시스템은 다시 줄일 수 없고, xfs는 아예 줄일 수 없습니다. 늘리기 전에 볼륨 스냅샷을 만드세요.

다음 경우에는 거부하고 아무것도 바꾸지 않습니다.

- 파일시스템이 ext3, ext4, xfs가 아닌 경우
- 그 파티션 뒤에 다른 파티션이 있는 경우. 마지막 파티션은 번호가 아니라 시작 위치로 판정합니다. Ubuntu 클라우드 이미지는 루트인 `1`번 파티션을 디스크 맨 뒤에 둡니다.
- MBR 디스크에서 파티션이 MBR의 2 TiB 한계에 닿은 경우. 먼저 GPT로 바꾸세요.
- LV가 PV 여러 개에 걸쳐 있거나, device-mapper 디바이스가 LVM 볼륨이 아닌 경우
- `growpart`가 없는 경우. Debian과 Ubuntu에서는 `cloud-guest-utils`, RHEL에서는 `cloud-utils-growpart`를 설치하세요.

늘린 SCSI 디스크는 rescan하기 전까지 커널이 옛 크기를 봅니다. `check`와 `grow -n`은 커널 상태를 바꾸는 rescan을 하지 않습니다. 대신 아무것도 바꾸지 않는 SCSI `READ CAPACITY` 명령으로 디스크에 크기를 묻습니다. 디스크가 커널이 아는 크기보다 크면 새 크기로 계획하고, 디스크 줄에 `sda · 100.00 GB → 200.00 GB`처럼 보이며, `edc disk grow`를 제안합니다. root가 아닌 경우처럼 디스크에 묻지 못하면 `grow`가 먼저 rescan하므로 공간이 더 보일 수 있다고 알립니다. 실제 `grow`는 rescan한 뒤 커널이 보는 크기로만 계획합니다. rescan 뒤에도 디스크가 커널보다 크다고 답하면 다 쓴 디스크라고 하지 않고 오류로 멈춥니다.

늘릴 수 있는 마운트는 늘리기 전과 뒤의 크기를 보이고, 실행할 명령을 따로 `next:` 줄에 보입니다. JSON에서는 `next`에 명령이 들어갑니다.

```text
WARN  disk.check                /를 99.00 GB에서 199.00 GB로 늘릴 수 있습니다(+100.00 GB)
      next: edc disk grow /
```

OCI와 Ubuntu 클라우드 이미지의 `/boot`처럼 마지막이 아닌 파티션의 마운트는, 마지막 파티션 뒤의 빈 공간으로 다른 마운트를 늘릴 수 있으면 `check`에서 통과로 나옵니다. 그 파일시스템의 줄이 grow를 제안합니다.

`check`는 root 없이도 실행되지만, 모든 크기는 root로 실행해야 보입니다. root가 아니면 ext 슈퍼블록과 LVM 정보를 읽지 못합니다. 마운트를 주지 않으면 블록 디바이스 위에 마운트된 ext3, ext4, xfs를 모두 보여 줍니다.

`grow`는 디스크를 바꾸기 전에 확인을 받습니다. 스크립트에서는 `--yes`로 질문을 건너뜁니다. 거절하면 exit code `4`로 끝납니다.

## Probe 진행 줄

stdin과 stdout이 모두 terminal이면 단일 probe command는 진행 줄 하나를 보여 줍니다. 그 줄에는 probe 이름, target, 경과 시간, command의 마지막 출력 줄이 들어갑니다.

```
⠋     net.trace                 example.com  2.9s  ·   4  <ip:778fad8d>  5.573 ms
```

`edc`는 probe가 300밀리초보다 오래 걸릴 때만 이 줄을 띄웁니다. 빨리 끝나는 probe는 결과만 출력합니다.

취소하려면 Ctrl-C를 누릅니다. `edc`는 command를 멈추고 exit code `4`를 돌려줍니다.

이 줄은 언제나 한 줄입니다. 긴 출력은 terminal 너비에서 잘립니다.

## 빠진 인자

인자 없이 command를 실행하고 stdin이 terminal이면 `edc`는 값을 묻습니다. 그 뒤에 값을 넣은 명령을 출력하므로 다음부터는 바로 칠 수 있습니다.

```bash
$ edc doctor
대상 (host 또는 URL): example.com
→ edc doctor example.com
```

`edc doctor`와 개별 probe command가 대상을 묻습니다. 질문에는 usage에 나오는 인자 형태를 그대로 씁니다. `edc tcp check`는 `host:port`를, `edc http check`는 `URL`을 묻습니다.

`edc capture`는 `--interface`가 필요합니다. 없으면 주소가 붙은 interface를 나열하고 기본 경로가 쓰는 것을 표시합니다. 이름을 치는 대신 줄을 고릅니다.

```bash
$ edc capture
어느 interface를 잡을까요?
> en0        192.168.1.92   기본 경로
  bridge100  192.168.139.3
→ edc capture --interface en0
```

command 묶음은 어떤 command를 실행할지 묻습니다. `edc dns`, `edc net`, `edc report`, `edc route`, `edc change`, `edc disk`, `edc history`, `edc completion`이 각자의 command를 짧은 설명과 함께 보여 줍니다. `edc report show`와 `edc report diff`는 이어서 현재 디렉터리의 report를 나열합니다.

`edc log`는 묻지 않습니다. 인자 없이 실행하면 사용 형태와 예시를 출력하고 exit code `2`를 돌려줍니다.

`edc change confirm`, `edc change rollback`, `edc route rollback`은 아직 대기 중인 변경을 나열합니다. run id를 옮겨 적는 대신 고릅니다.

stdin이 terminal이 아니면 `edc`는 usage를 출력하고 exit code `2`를 돌려줍니다. script의 동작은 그대로입니다. `edc`는 command에 꼭 필요한 값만 묻고, `--duration`이나 `--filter` 같은 선택 설정은 묻지 않습니다.

`edc change apply`와 `edc route switch`는 예외입니다. host를 바꾸므로 값을 모두 flag로 받습니다. 되돌릴 수 없는 command는 다시 칠 수 있는 형태로 남아야 합니다.

## Doctor 실시간 화면

stdin과 stdout이 모두 terminal이면 `edc doctor`는 probe마다 줄 하나를 보여 주고 probe가 끝날 때 그 줄을 갱신합니다. 끝난 줄은 화면에 남습니다. 그 뒤에 상세와 요약이 따라옵니다.

`edc doctor --all-ips example.com`은 DNS가 반환한 IP마다 직접 접속해 TCP·TLS·HTTP 결과를 보여 줍니다. Host와 TLS SNI는 원래 hostname을 유지합니다. 이 HTTP 검사는 응답 header까지만 읽고 redirect를 따라가거나 proxy를 사용하지 않습니다. 공개 IP의 origin ASN과 등록된 ASN holder는 RIPEstat에서 조회하며, 조회 실패는 endpoint 상태에 영향을 주지 않습니다. IP 하나라도 실패하면 `endpoints.check`가 실패합니다. 이 검사로 LB 존재 여부나 서버의 실제 물리적 위치를 확정할 수는 없습니다.

취소하려면 Ctrl-C를 누릅니다. `edc`는 돌던 probe를 멈추고 exit code `4`를 돌려줍니다.

stdin이나 stdout이 terminal이 아니거나, `--json`을 지정했거나, `NO_COLOR`가 설정되면 `edc`는 기다렸다가 마지막에 모든 줄을 출력합니다.

## Packet capture

`capture`만 privileged 작업이며, `doctor`는 `sudo`를 사용하지 않습니다. `capture`는 Linux와 macOS를 지원합니다. Capture에는 강제 상한(duration 60초, packet 10,000개)이 있고 기존 파일을 덮어쓰지 않습니다. `tcpdump`는 `/usr/sbin`, `/usr/bin`, `/sbin`, `/bin` 중 한 곳에 설치되어 있어야 합니다. `capture`는 `tcpdump`와 `sudo`를 `PATH`에서 찾지 않습니다.

Linux에서 `--mode events`를 사용하면 process metadata와 함께 TCP socket state, retransmission, reset, destroy event를 JSONL로 저장합니다. 이 mode는 BTF와 eBPF capability가 필요하며 PCAP 파일을 만들지 않습니다. 각 event의 `timestamp_ns`는 요약 줄과 같은 Unix epoch 기준 나노초이고, `boot_time_ns`는 부팅 후 kernel monotonic 시간입니다. TCP event의 `pid`와 `process`는 kernel이 packet을 처리할 때 CPU에서 돌던 task가 아니라 그 socket을 쓰는 process입니다. `Ctrl-C`를 누르면 수집을 멈추고, 그때까지 모은 event와 summary 줄을 저장합니다.

Linux와 macOS에서 `trace tcp` 또는 `trace udp`를 사용하면 network event를 발생 즉시 출력합니다. 기본 terminal 화면은 event를 스크롤합니다. `Ctrl-C`를 누르면 수집을 종료하고 summary를 출력합니다.

```bash
./bin/edc trace tcp
./bin/edc trace tcp --duration 15s
./bin/edc trace tcp --process slackbot --destination 100.66.11.194:443 --json trace.json
./bin/edc trace tcp --raw
./bin/edc trace show trace.jsonl       # 저장한 --raw 파일의 요약을 root 없이, 어느 호스트에서나
./bin/edc trace udp --duration 15s
./bin/edc trace udp --group-by target
./bin/edc trace udp --group-by source
./bin/edc trace udp --group-by port
./bin/edc trace tcp --group-by process
./bin/edc trace tcp --group-by event
./bin/edc trace tcp -d
```

`--raw`는 JSONL event를 발생 즉시 출력합니다. `--json`은 `Ctrl-C` 후 connection summary를 저장합니다.
끝에 출력하는 텍스트 요약은 process와 상대별로 행을 묶습니다. client 행은 목적지를, server 행은 `127.0.0.1:2379 (server)`처럼 local 서비스를 표시합니다. client마다 포트가 달라서 서버 쪽은 서비스로 묶습니다. TCP 행은 연결 수, 결과별 연결 수, 평균 연결 시간과 traffic을 표시하고, UDP 행은 datagram 수와 traffic을 표시합니다. 연결이나 UDP flow마다 한 행을 보려면 `-d` 또는 `--detail`을 사용합니다. JSON 출력은 항상 연결이나 flow마다 한 행입니다. `trace dns`의 요약은 항상 이름과 record 종류마다 한 행이라 `-d`로 달라지지 않습니다.
연결별 상세 행은 socket이 생긴 때부터 없어질 때까지를 한 행으로 표시합니다. kernel은 닫힌 socket의 주소를 새 socket에 다시 쓸 수 있으므로, socket이 없어지면 다음 event부터 새 행으로 셉니다. 행은 열려 있는 연결과 최근에 닫힌 연결 1,000개만 남기고, 합계는 모든 연결로 셉니다. JSON 출력의 `connections_omitted`는 행을 남기지 않은 닫힌 연결의 수입니다.
각 행의 결과는 다음 중 하나입니다.

- `established`: trace가 connect나 accept를 봤습니다. 나중에 reset이 와도 결과는 바뀌지 않고, reset 여부는 `RESET` 열에 표시합니다.
- `failed`: 연결되지 못했습니다. 경로가 없는 경우처럼 핸드셰이크 전에 connect가 실패했거나, 핸드셰이크 중에 닫히거나 reset을 받았습니다.
- `incomplete`: trace가 끝날 때까지 핸드셰이크가 끝나지 않았습니다.
- `existing`: trace를 시작하기 전부터 열려 있던 연결이라 핸드셰이크를 보지 못했습니다.

`Attempts`는 trace 중에 핸드셰이크를 본 연결의 수이고, `Established`와 `Incomplete`의 합입니다. `Incomplete`는 `failed`와 `incomplete` 행을 셉니다. `Existing`은 `existing` 행을 셉니다. listen socket과 connect하지 않고 닫힌 socket은 연결이 아니므로 행을 만들지 않습니다.
`--duration 15s`를 지정하면 15초 후 종료합니다. `--live`는 호환성을 위해 계속 허용합니다.
`--group-by source`, `--group-by target`, `--group-by port`, `--group-by process`, `--group-by event`를 사용하면 선택한 기준별 live 행을 표시합니다. TCP 행에는 connect, retransmission, reset과 traffic 값을 표시하고 UDP 행에는 TX, RX traffic 값을 표시합니다. `EVENT/s`는 초당 event 수입니다. TX와 RX byte는 socket payload byte입니다. B/s는 byte rate이며 bps와 Mbps는 bit rate입니다. Mbps는 `bps / 1,000,000`의 decimal 단위를 사용합니다.
`--group-by source`는 source host별로 event를 묶습니다. OS가 연결마다 새 port를 배정하므로 source port는 무시합니다.
`--group-by event`는 `tcp_connect`, `tcp_retransmit` 같은 event 이름별로 묶습니다.
`--group-by port`는 상대 port별로 묶습니다. 서버 socket은 `53 (server)`처럼 로컬 port로 묶습니다. 한 port 행에 상대가 여럿이면 `3478 (65 peers)`처럼 상대 수를 표시합니다.
`--group-by process`는 process 이름별로 묶습니다. 이름이 같은 process는 PID가 달라도 한 행에 표시하며, `--process` 필터와 같은 이름을 기준으로 합니다. Linux에서는 event를 만든 thread의 이름이 아니라 `ps`가 보여 주는 process 이름을 씁니다. kernel은 이름의 앞 15 byte만 보관합니다. edc가 event의 process를 찾지 못하면 `-` 행에 표시합니다.
`--group-by target`은 target이 없는 서버 socket의 event를 `127.0.0.53:53 (server)`처럼 로컬 서비스마다 한 행으로 묶습니다. 로컬 port가 ephemeral port 범위 밖이고 상대 port가 범위 안이면 서버 socket으로 봅니다.
event의 target은 같은 process가 trace 중에 받은 DNS 응답, process의 명령줄, 그 주소에 대해 다른 DNS 응답이나 systemd-resolved 캐시에서 마지막으로 본 이름 순서로 정합니다. JSON event의 `target_source` 필드에 `dns`, `command`, `resolver-cache` 중 어디서 얻었는지 나옵니다. 한 주소를 여러 이름이 함께 쓸 수 있어서 다른 조회에서 얻은 이름은 틀릴 수 있습니다.
재전송, reset, 일부 상태 변화는 kernel이 socket을 가진 프로세스 밖에서 기록합니다. trace 중에 그 socket을 쓰는 프로세스를 보지 못했으면 이 event의 process는 `-`로 표시합니다. 예를 들어 trace 전에 연결한 socket은 데이터를 주고받기 전까지 주인을 알 수 없습니다.

Linux에서 `--container <name|id>`를 주면 docker container 하나의 event만 보여 줍니다. edc는 `docker inspect`로 container의 첫 process를 찾고 그 process의 cgroup v2 디렉터리를 읽은 뒤, 그 cgroup과 하위 cgroup의 event만 남깁니다. `docker exec`로 띄운 process도 같은 cgroup에 들어갑니다. cgroup은 trace를 시작할 때 한 번만 읽으므로, container가 다시 시작되면 trace도 다시 시작합니다. 주인을 모르는 event에는 cgroup이 없어서 `--process`처럼 `--container`에서도 빠집니다. `trace arp`와 `trace ndp`의 event에는 process가 없으므로 `--container`를 받지 않습니다. 전체 화면의 머리글에는 container 이름이 표시됩니다.

```bash
./bin/edc trace http --side server --container web
./bin/edc trace tcp --container 5c9e77b3f470 --raw
```

bridge network에서는 주소와 port가 container 안의 값입니다. 예를 들어 `-p 8080:80`으로 publish한 서버는 port 80으로 보입니다.

전체 화면 terminal에서는 `s`로 source 행, `t`로 target 행, `p`로 port 행, `c`로 process 행, `e`로 event 행, `g`로 event 스크롤을 표시합니다. `trace http`에서는 `u`로 path 행을 표시합니다. `Tab`은 다음 보기, `Shift+Tab`은 이전 보기로 바꿉니다. terminal 폭이 넓으면 첫 열을 넓혀 group 값을 자르지 않고 표시합니다. 폭이 좁으면 byte 열을 `195K`(195 KiB)처럼 짧은 단위로 표시합니다.
전체 화면은 최근 event 10,000개를 유지하고, live rate는 이 event들이 걸친 시간으로 계산합니다. `Ctrl-C` 후 summary는 모든 event를 사용합니다.
전체 화면의 group 행은 traffic이 많은 group부터 표시합니다. 행이 terminal에 다 들어가지 않으면 위쪽 행을 표시합니다.

event 목록에서 Up이나 Down(또는 `k`, `j`)을 누르면 event를 고릅니다. event를 고른 동안에는 목록이 멈추고 새 event를 따라가지 않으며, 머리글에 더 새로운 event 수가 표시됩니다. Enter를 누르면 event의 모든 필드와 payload 전체를 보여 주는 상세 보기가 열립니다. 상세 보기는 긴 줄을 화면 폭에 맞춰 나눕니다. Up, Down, PgUp(또는 `b`), PgDn(또는 Space), `g`, `G`로 스크롤하고, Esc나 `q`로 돌아갑니다. 목록에서 `l`, Esc, End 중 하나를 누르면 다시 새 event를 따라갑니다. Mac 자판에는 End 키가 없는 경우가 많습니다. 목록에서도 `b`와 Space는 한 화면씩 고른 위치를 옮깁니다. 고른 event가 목록에서 가장 오래된 event가 되면, 그 아래에 더 새 event를 보여 줍니다. 목록은 payload마다 앞 4KiB만 보관하고, 상세 보기는 최근 event의 payload 전체를 합계 64MiB까지 보여 줍니다.

`f`를 누르면 가장 최근 event의 상세 보기를 열고 새 event를 따라갑니다. `f`를 다시 누르면 화면의 event에서 멈춥니다. `trace http`의 전체 화면은 `--payload`가 없어도 각 message의 앞 4KiB를 모읍니다. 목록의 payload 줄은 `v`를 누를 때까지 숨기고, `--payload`로 시작하면 처음부터 보여 줍니다. `m`은 `--payload`가 가리는 header 값을 보이거나 가리고, 보이는 동안 머리글에 `secrets shown`이 표시됩니다. 상세 보기에서 `z`를 누르면 gzip 본문을 풉니다. 최대 1MiB까지 풀고, 푼 크기를 함께 보여 줍니다.

`i`를 누르면 화면을 나눕니다. 위쪽 절반은 목록이고, 아래쪽 절반은 message 하나의 미리 보기입니다. 미리 보기는 고른 event를 보여 주고, 고른 event가 없으면 가장 최근 event를 보여 주며 새 event가 오면 바뀝니다. event에 payload가 있으면 payload를, 없으면 event의 필드를 보여 줍니다. `J`와 `K`로 미리 보기를 스크롤하고, Enter로 전체 상세 보기를 엽니다. `trace http`에서는 `m`과 `z`도 미리 보기에 적용됩니다. terminal이 9줄보다 작으면 목록만 보입니다.

미리 보기와 상세 보기의 제목 줄은 `09:05:07.123`처럼 event 시각으로 시작하며, 시각은 이 host의 시간대를 따릅니다. 미리 보기 제목 줄의 남은 폭은 `─`로 채워서, 색이 없어도 목록과 미리 보기의 경계가 보입니다. terminal 폭이 120칸 이상이면 event 목록의 `PROCESS` 앞에 `TIME` 열이 생깁니다.

macOS에서 `trace`는 kernel의 network 통계 interface(`com.apple.network.statistics`)에서 socket별 counter를 읽습니다. `nettop`도 같은 interface를 사용합니다. `edc`는 1초마다 counter를 읽고 직전 값과의 차이로 event를 만듭니다. 이 interface는 공개되지 않은 interface라서 macOS 업데이트로 형식이 바뀔 수 있고, 형식이 바뀌면 trace는 오류를 내고 멈춥니다.

macOS에서는 다음 값을 관측할 수 없어서 텍스트 출력에는 `-`로, JSON 출력에는 `null`로 표시합니다.

- `connect_ms`: counter에 연결 시간이 없습니다.
- `reset`, `resets`: counter로는 RST packet을 볼 수 없습니다.
- `retransmissions`: counter는 재전송한 packet 수가 아니라 byte 수를 줍니다. 이 byte 수는 `--raw` 출력에서 `tcp_retransmit` event의 `bytes` 필드에 담깁니다.
- 연결하지 않은 UDP socket의 목적지: kernel이 datagram마다의 목적지를 기록하지 않으므로 목적지를 비워 두고, 표에는 `-`로 표시합니다.

macOS의 event 하나는 직전에 읽은 값 이후의 변화를 나타냅니다. `bytes` 필드에는 byte 수를, `packets` 필드에는 packet 수를 담습니다. UDP의 `sent`와 `received`는 packet 수를 셉니다. `EVENTS`와 `EVENT/s`는 socket 호출이 아니라 이렇게 만든 event의 수를 셉니다. byte 합계와 traffic rate는 정확합니다.

macOS의 process 이름은 32 byte까지 보관됩니다. ephemeral port 범위는 `net.inet.ip.portrange.first`와 `net.inet.ip.portrange.last`에서 읽습니다.

root 권한 없이 실행하면 macOS trace는 현재 사용자의 kernel socket만 표시합니다. macOS의 사용자 공간 network stack이 처리하는 연결은 표시하지 않으며, Network.framework는 이 stack으로 traffic을 보낼 수 있습니다. `target` hostname도 표시하지 않으므로 이때 `--group-by target`은 목적지 주소로 event를 묶습니다.

`sudo`로 실행하면 모든 사용자의 process와 사용자 공간 network stack의 연결을 표시하고, macOS가 연결마다 기록한 domain 이름을 `target`으로 표시합니다. 이때 `target_source`는 `system`입니다. domain 이름이 없는 연결은 `--group-by target`에서 목적지 주소로 묶습니다. QUIC는 UDP를 사용하므로, 사용자 공간 network stack의 QUIC 연결은 `trace udp`에 표시합니다.

Linux에서 `trace dns`를 사용하면 DNS 질의와 응답을 발생 즉시 출력합니다. UDP port 53의 DNS message를 읽고, port 53으로 가는 TCP 연결도 표시합니다.

```bash
./bin/edc trace dns
./bin/edc trace dns --duration 15s --json dns.json
./bin/edc trace dns --group-by target
./bin/edc trace dns --process curl --destination 127.0.0.53:53
```

질의는 `dns_query` event입니다. 응답은 결과 코드를 이름으로 사용합니다. 예를 들어 `dns_noerror`, `dns_nxdomain`, `dns_servfail`입니다. record 없이 성공한 응답은 `dns_nodata`입니다.

TC bit가 있는 응답은 `dns_truncated`입니다. 응답이 UDP에 다 들어가지 않았다는 뜻이며, client는 같은 서버에 TCP로 다시 묻습니다. port 53으로 가는 TCP 연결은 `dns_tcp_connect`이고, 연결에 실패하면 `dns_tcp_fail`입니다. TCP 연결 안의 DNS message는 보통의 질의와 응답 event로 표시하며, TCP로 오간 event에는 `"transport": "tcp"`가 붙습니다. 같은 process가 그 서버에서 `dns_truncated` 응답을 받았으면 TCP event에 그 질의의 이름을 붙입니다.

`Errors`는 `dns_noerror`, `dns_nodata`, `dns_truncated`가 아닌 응답과 `dns_tcp_fail`을 셉니다. summary의 `TCP connections`는 `dns_tcp_connect`와 `dns_tcp_fail`을 셉니다.

DNS event의 `target`은 질의한 이름이고 `destination`은 DNS 서버입니다. 스크롤 화면의 `DESTINATION` 열은 이름, record 종류, 서버를, `EVENT` 열은 결과와 응답 시간을 표시합니다. 따라서 `--group-by target`은 이름별로 묶고, `--destination`은 서버로 거릅니다. DNS 서버 port는 항상 53이므로 `trace dns`에는 port 보기가 없습니다.

edc는 로컬 port, 서버, transaction ID가 같은 질의와 응답을 짝짓습니다. `latency_ms`는 kernel이 질의를 보낸 때부터 process가 응답을 읽은 때까지입니다.

edc는 이 시간을 둘로 나눕니다. `network_ms`는 응답이 socket 수신 큐에 들어간 때까지이고, `read_delay_ms`는 그때부터 process가 응답을 읽은 때까지입니다. `read_delay_ms`가 길면 DNS 서버가 아니라 프로그램이 바쁘거나 느린 것입니다.

응답이 오기 전에 같은 질의를 다시 보냈으면 응답 하나가 그 질의에 모두 답한 것으로 보고, 응답 시간은 처음 보낸 질의부터 잽니다.

`Unanswered`는 trace가 끝날 때까지 응답이 없는 질의 수입니다. 전체 화면의 `NOANS`는 아직 응답을 기다리는 질의 수입니다.

`Ctrl-C` 후 summary는 이름과 record 종류마다 한 행을 표시합니다. group 행은 byte 대신 질의, 응답, 오류, 응답 없음을 표시하고, 평균·최대 응답 시간과 평균 network 시간(`NETms`), 평균 읽기 지연(`RDms`)도 표시합니다.

systemd-resolved가 있는 host에서는 조회 하나가 두 번 보일 수 있습니다. 프로그램과 `127.0.0.53` 사이, systemd-resolved와 상위 서버 사이입니다. 프로그램 쪽 행만 있으면 systemd-resolved가 캐시에서 답한 것입니다.

`--side server`를 사용하면 systemd-resolved, dnsmasq, CoreDNS 같은 로컬 DNS 서버를 관측합니다. 서버가 port 53으로 받은 질의와 보낸 응답을 표시하며, event 이름은 client 쪽과 같습니다. JSON event와 요약에는 `"side": "server"`가 붙습니다. 서버 쪽 event의 `destination`은 질의한 client이고 `process`는 DNS 서버입니다. `--group-by source`는 서버 쪽 event를 서버가 받는 주소별로 묶습니다.

```bash
./bin/edc trace dns --side server
./bin/edc trace dns --side server --group-by target
```

서버 쪽 응답 시간은 서버가 질의를 읽은 때부터 응답을 보낸 때까지입니다. 서버 쪽 질의의 `read_delay_ms`는 질의가 서버의 수신 큐에서 기다린 시간입니다. 응답 시간이 짧으면 대개 캐시에서 답한 것이고, 길면 상위 서버에 물어본 것입니다. 서버 쪽에서는 서버가 port 53으로 받은 TCP 연결을 `dns_tcp_accept`로 표시하고, 서버가 그 client에게 잘린 응답을 보냈으면 그 질의의 이름을 붙입니다. edc는 서버가 `accept()`를 부를 때 listen socket의 process를 알게 되므로, trace 전부터 떠 있던 서버의 첫 연결에는 process가 없을 수 있습니다.

`trace dns`는 port 53만 관측합니다. DNS over TCP는 message 앞에 2바이트 길이를 붙입니다. 길이와 message를 두 버퍼로 나눠 쓰거나 두 번에 나눠 읽는 program이 많아, edc는 쓰기의 앞 두 버퍼를 읽고, 2바이트만 읽은 조각은 같은 연결의 다음 읽기와 이어 붙입니다. 한 번의 읽기 중간에서 시작하는 message는 찾지 못합니다. DNS over TLS, DNS over HTTPS, mDNS, LLMNR은 표시하지 않습니다. BPF는 DNS message의 앞 1,024 byte만 읽습니다. 이보다 긴 응답은 header에서 결과 코드를 읽고 이름은 질의에서 가져옵니다. macOS는 socket의 payload를 주지 않으므로 `trace dns`는 Linux에서만 동작합니다.

Linux와 macOS에서 `trace arp`를 사용하면 IPv4 neighbor table(ARP 캐시)의 변화를 출력합니다. root와 eBPF가 필요 없습니다. Linux에서는 kernel 알림을 netlink로 받아 발생 즉시 출력합니다. macOS에서는 `arp -an`과 같은 방법으로 ARP table을 1초마다 읽고 직전 table과 비교합니다.

```bash
./bin/edc trace arp
./bin/edc trace arp --group-by target
./bin/edc trace arp --json arp.json --duration 30s
```

trace를 시작할 때 이미 있던 항목은 event로 표시하지 않고, 그 뒤의 변화를 하나씩 표시합니다.

- `arp_new`: 새 항목입니다.
- `arp_state`: 상태가 바뀌었습니다. 예를 들어 `REACHABLE`에서 `STALE`로 바뀐 경우이며, `old_state`와 `new_state`에 상태가 나옵니다.
- `arp_mac_change`: IP의 MAC 주소가 바뀌었습니다. `old_mac`과 `mac`에 주소가 나옵니다. gateway 전환, IP 충돌, 위조된 ARP 응답이 원인일 수 있습니다.
- `arp_failed`: kernel이 그 IP의 응답을 받지 못했습니다.
- `arp_delete`: kernel이 항목을 지웠습니다.

ARP event의 `target`은 IP 주소이고 `source`는 interface입니다. process와 port가 없으므로 `trace arp`에는 process 보기와 port 보기가 없습니다. `--destination`은 IP 주소로 거릅니다. ARP event에는 process가 없으므로 `--process`를 주면 ARP event가 하나도 남지 않습니다. `Ctrl-C` 후 summary는 interface와 IP마다 한 행을 표시하고, group 행은 MAC 주소 수(`MACS`), MAC 변경(`CHG`), 실패(`FAIL`)를 표시합니다.

kernel은 주소 확인을 시작할 때 알리지 않으므로, 실패한 확인은 `arp_failed`만 표시합니다. ARP를 쓰지 않는 항목(`NOARP`)은 표시하지 않습니다. `trace arp`는 ARP 패킷이 아니라 neighbor table을 보므로, 다른 host의 요청처럼 table을 바꾸지 않는 ARP 패킷은 표시하지 않습니다. macOS에서는 두 번 읽는 사이에 생겼다 사라진 변화와, 상태가 바뀌지 않은 채 다시 실패한 확인은 표시하지 않습니다. macOS에는 `STALE` 같은 neighbor 상태가 없으므로, MAC 주소가 있는 항목은 `COMPLETE`, 없는 항목은 `INCOMPLETE`, 고정 항목은 `PERMANENT`, macOS가 거부 표시(`RTF_REJECT`)를 한 항목은 `FAILED`로 표시합니다.

Linux에서 `trace arp`는 netlink 수신 buffer를 8MB로 요청합니다. table flush처럼 kernel이 많은 변화를 한꺼번에 알리면 buffer가 넘치고, kernel은 넘친 알림을 버립니다. `Lost events`는 버려진 변화의 수가 아니라 buffer가 넘친 횟수입니다. 한 번 넘칠 때 여러 변화를 잃을 수 있으므로, 요약은 놓친 변화의 수를 알 수 없다고 함께 표시합니다. root가 아니면 kernel이 buffer를 `net.core.rmem_max`까지만 허용합니다. root 없이 실행했는데 유실이 보이면 `net.core.rmem_max`를 늘리거나 root로 실행합니다.

IPv6 neighbor table(NDP)은 `trace ndp`로 봅니다. Linux와 macOS에서 `trace arp`와 같은 방식으로 동작하며, event는 `ndp_new`, `ndp_state`, `ndp_mac_change`, `ndp_failed`, `ndp_delete`이고 요약 제목은 `NDP trace`입니다.

```bash
./bin/edc trace ndp
./bin/edc trace ndp --group-by target
```

macOS kernel은 table의 link-local 주소에 interface 번호를 넣어 둡니다. `trace ndp`는 이 번호를 지우므로 link-local 주소는 `fe80::1`처럼 보이고, interface는 `source` 열에 나옵니다.

Linux 5.15 이상에서 `trace http`를 사용하면 평문 HTTP/1.x 요청과 응답을 발생 즉시 출력합니다. edc는 kernel에서 TCP로 읽고 쓰는 data마다 앞 512 byte(`--payload`를 쓰면 앞 4KiB)를 읽고, method, `Host` header, path, 상태 코드만 남깁니다. path의 query에는 token이 들어 있을 수 있어 뺍니다. `--payload`를 쓰지 않으면 다른 header와 body도 버립니다.

```bash
./bin/edc trace http
./bin/edc trace http --group-by target
./bin/edc trace http --group-by path
./bin/edc trace http --side server --process nginx
./bin/edc trace http --payload
./bin/edc trace http --side server --port 8080
./bin/edc trace http --port 8080 --payload=all
```

요청은 `http_request` event입니다. 응답은 `http_1xx`부터 `http_5xx`까지이고 `status`에 코드가 나옵니다. HTTP/1.x는 한 연결에서 요청 순서대로 응답하므로, 응답은 같은 연결에서 아직 응답이 없는 가장 오래된 요청과 짝짓습니다. `latency_ms`는 client가 요청을 보낸 때부터 응답을 읽은 때까지입니다. `1xx` 응답은 요청을 끝내지 않습니다.

HTTP event의 `target`은 `Host` header이고, `Host`가 없으면 서버 주소입니다. 서버 쪽 `latency_ms`는 서버가 요청을 읽은 때부터 응답을 쓴 때까지입니다.

`trace http`는 이 host에서 일어나는 HTTP를 두 쪽으로 나눠 보여 줍니다. event 행 앞에 쪽이 붙습니다.

- `client:`는 이 host가 보낸 요청입니다. proxy가 backend로 보내는 요청이나 program이 API를 호출하는 요청이 여기에 속합니다.
- `server:`는 로컬 서버가 받은 요청입니다.

JSON event에는 `"side": "client"`나 `"side": "server"`가 붙습니다. 한 쪽만 보려면 `--side client`나 `--side server`를 씁니다. 전체 화면에서는 `/`를 누르고 `server`를 입력하면 server 쪽 행만 남습니다. `trace dns`는 기본으로 client 쪽만 봅니다.

proxy를 거치는 요청은 구간마다 한 번씩 보입니다. 예를 들어 같은 host에서 nginx가 port 9900으로 요청을 받아 port 9000의 backend로 보내면, 사용자 요청 하나가 요청 행 세 개로 나옵니다. port 9900에서 받은 nginx의 `server:`, port 9000으로 보낸 nginx의 `client:`, port 9000에서 받은 backend의 `server:`입니다. group 보기는 server 쪽을 `nginx (server)`처럼 따로 묶습니다.

`--group-by path`를 사용하면 요청 path별로 event를 묶습니다. 전체 화면에서는 `u`를 누릅니다. path 보기는 `trace http`에만 있습니다. path에는 query가 없고, host와 method가 달라도 path가 같으면 한 행에 묶습니다. 응답은 짝지은 요청의 행에 들어갑니다. `tls_hello` event와 짝이 없는 응답은 `-` 행에 들어갑니다. `/users/123`처럼 path에 ID가 들어 있으면 ID마다 행이 따로 생깁니다.

| 보려는 것 | 명령 |
| --- | --- |
| 이 host의 HTTP 전부 | `./bin/edc trace http` |
| 로컬 서버가 받은 요청 | `./bin/edc trace http --side server` |
| 이 host가 보낸 요청(backend나 외부 API 호출) | `./bin/edc trace http --side client` |
| port 9000 연결의 양 끝 | `./bin/edc trace http --port 9000` |
| port 9900의 proxy가 받은 요청만 | `./bin/edc trace http --side server --port 9900` |
| 쪽마다 process별 응답 시간 | `./bin/edc trace http --port 9000 --group-by process` |
| path별 요청, 오류, 응답 시간 | `./bin/edc trace http --group-by path` |
| HTTPS 안의 HTTP/1.1과 HTTP/2 요청 | `./bin/edc trace http --tls` |

한 구간의 client 응답 시간과 서버 응답 시간은 서로 다른 시간을 잽니다. client 응답 시간에는 network와, 서버가 요청을 읽기 전까지 기다린 시간이 들어갑니다. 서버 응답 시간에는 서버가 처리한 시간만 들어갑니다. client 응답 시간이 서버 응답 시간보다 훨씬 길면 network와 서버의 대기열을 확인합니다.

edc는 port가 아니라 data의 앞부분으로 HTTP를 찾으므로, 어느 port의 HTTP든 봅니다. `source`는 항상 이 host 쪽 주소이고 `destination`은 상대 주소입니다. `--port`를 사용하면 이 host나 상대가 그 port를 쓰는 연결만 봅니다. `--side server`와 함께 쓰면 로컬 서버 하나를, `--side client`와 함께 쓰면 이 host가 그 port의 서버로 보낸 요청을 봅니다. port는 kernel에서 확인하므로 다른 연결의 data는 읽지 않습니다.

`--payload`를 사용하면 각 message의 data를 볼 수 있습니다. edc는 event마다 그 아래 줄에 body를 출력하고, body가 없으면 header를 출력합니다. `--raw`에서는 `payload` 필드에 data 전체가 들어 있습니다. data는 한 번의 읽기나 쓰기에서 앞 4KiB(4,096 byte)라서 더 긴 body는 잘립니다. program이 header와 body를 두 번에 나눠 쓰면 body는 보이지 않습니다. `--payload`를 쓰면 레코드가 커져서, 요청이 많은 서버에서는 event가 유실될 수 있습니다. 유실된 event 수는 요약에 표시됩니다. `--payload`는 query를 그대로 두지만 `Authorization`, `Proxy-Authorization`, `Cookie`, `Set-Cookie`, `X-Api-Key`, `X-Goog-Api-Key`, `Api-Key`, `X-Amz-Security-Token` header 값은 가립니다. 다른 header, query, body는 그대로 출력하므로 token이나 비밀번호가 보일 수 있습니다. 출력을 공유하기 전에 token이나 비밀번호가 없는지 확인합니다. 제어 문자는 `\xNN`으로 바꾸므로 data가 terminal을 조작하지 못합니다. `--json`은 요약만 기록하므로 `--payload`와 함께 사용할 수 없습니다.

`--payload=all`을 사용하면 message 하나를 1MiB까지 전부 볼 수 있습니다. edc는 message의 다음 읽기와 쓰기, `writev`의 다른 버퍼까지 따라갑니다. event는 message가 끝날 때 출력합니다. `Content-Length`만큼 body를 받았거나, chunked body의 마지막 조각을 받았거나, 1초 동안 data가 없으면 끝난 것으로 봅니다. 그래서 `--payload`보다 event가 늦게 나올 수 있지만 응답 시간은 같습니다. 줄 단위 출력에서는 event 아래에 message 전체를 출력하고, 전체 화면은 여전히 한 줄로 보여 줍니다. 1MiB에서 잘렸거나, 조각을 잃었거나, 끝나기 전에 trace가 끝나면 event에 `"payload_truncated": true`가 붙습니다. `--payload=all`은 띄우지 않고 붙여 씁니다. `--payload all`은 오류입니다. `--payload=all`은 `--payload`보다 CPU를 더 쓰므로, 요청이 많은 서버에서는 event가 더 일찍 유실될 수 있습니다.

`--payload`와 함께 `--show-secrets`를 사용하면 `--payload`가 가리는 header 값도 그대로 보여 줍니다. 이 값이 있으면 다른 사람이 그 계정을 쓸 수 있으므로, 이 출력은 공유하지 않습니다. 전체 화면에서는 대신 `m`을 누릅니다.

`Ctrl-C` 후 summary는 쪽, method, host, path마다 한 행을 표시합니다. 두 쪽이 모두 있으면 쪽별 합계를 따로 보여 주고 `SIDE` 칸을 더합니다. JSON에는 쪽별 합계를 담은 `client`와 `server` 객체가 붙습니다. group 행은 요청, 응답, 4xx·5xx 응답, 응답 없음, 평균·최대 응답 시간을 표시합니다.

HTTPS는 암호문이라 method, path, 상태 코드를 읽을 수 없습니다. 연결을 시작할 때 보내는 TLS ClientHello는 평문이므로, edc는 이를 읽어 `tls_hello` event로 보여 줍니다. `target`은 서버 이름(SNI)이고, `alpn` 필드에는 client가 제안한 protocol이 `h2`, `http/1.1`처럼 들어 있습니다. event 행에는 첫 protocol만 표시합니다. ClientHello를 보낸 client는 `client:` 행으로, 받은 로컬 서버는 `server:` 행으로 보입니다. 요약은 이 연결을 별도의 `TLS connections` 표로 세고, JSON에는 `tls_connections`와 `tls`가 붙습니다. port 443의 HTTPS 연결만 보려면 `--port 443`을 씁니다.

서버의 답도 키가 정해지기 전까지는 평문입니다. edc는 ServerHello를 `tls_server_hello` event로 보여 주며 `tls_version`과 `cipher`가 붙습니다. 예를 들어 `TLS1.3 TLS_AES_128_GCM_SHA256`입니다. `latency_ms`는 같은 연결의 ClientHello부터 잰 시간이라, client에서는 network와 서버가, 로컬 서버에서는 서버만 들어갑니다. TLS 1.2 서버는 고른 protocol도 `alpn`에 적습니다. TLS 1.3은 그 선택을 암호문으로 보냅니다. 한쪽이 handshake를 거절하면 alert record를 보냅니다. edc는 평문 alert를 `tls_alert` event로 보여 줍니다. 예를 들어 `fatal handshake_failure from server`, `fatal protocol_version from server`, `fatal unrecognized_name from server`, `fatal unknown_ca from client`입니다. `alert_from`은 보낸 쪽입니다. 요약의 TLS 표에는 `VERSION`과 `ALERTS` 열이, 그 위에는 alert 종류별 수를 적은 `TLS alerts` 줄이 붙고, JSON에는 `versions`, `alerts`, `tls_alerts`가 붙습니다.

TLS 1.3은 ServerHello 뒤의 alert를 암호화할 수 있습니다. OpenSSL은 client가 서버 인증서를 거절하는 alert를 자기 암호화를 켜기 전에 보내므로, `curl`은 TLS 1.3에서도 TLS 1.2에서도 `fatal unknown_ca from client`를 보입니다. alert를 암호화하는 client는 `tls_server_hello` 뒤에 끝나는 연결만 보입니다. edc는 handshake 머리 뒤 방향마다 처음 8번의 읽기와 쓰기에서만 alert를 찾고, program이 5 byte record 머리와 2 byte alert를 두 번에 나눠 읽어도 찾으며, trace 전에 시작한 연결은 보지 못합니다.

trace를 시작하기 전에 맺은 TLS 연결은 보이지 않습니다. client가 Encrypted Client Hello(ECH)를 쓰면 SNI는 서비스 제공자의 공개 이름입니다. HTTPS의 요청을 보려면 `--tls`를 씁니다. TLS를 푸는 곳 뒤의 평문 HTTP를 trace해도 됩니다. 예를 들어 backend로 평문 HTTP를 보내는 proxy가 있으면 그 구간을 봅니다.

`--tls`를 사용하면 HTTPS 안의 HTTP/1.1과 HTTP/2 요청을 볼 수 있습니다. edc는 OpenSSL, GnuTLS, NSS, wolfSSL, Mbed TLS, rustls-ffi, Go TLS 또는 지원하는 BoringSSL 빌드에서 암호화하기 전과 복호화한 뒤의 평문을 읽으므로, 인증서나 key가 필요 없습니다.

```bash
./bin/edc trace http --tls
./bin/edc trace http --tls --side server --port 443
./bin/edc trace http --tls=/usr/local/bin/node
```

값 없이 쓰면 trace를 시작할 때 다음 파일을 찾습니다.

- 표준 library 디렉터리에 있는 이 host의 `libssl`, `libgnutls`, `libssl3`, `libnspr4`, `libwolfssl`, `libmbedtls`, `librustls`
- 실행 중인 process가 적재한 이 library들(container 안의 것도 포함)
- OpenSSL을 실행 파일 안에 넣고 `SSL_read`를 내보내는 process의 실행 파일(예: `node`)
- 지원하는 GNU build ID를 가진 심볼 없는 BoringSSL 실행 파일

edc는 process가 실제로 적재한 파일을 열므로, 패키지를 업데이트한 뒤에도 예전 `libssl`을 쓰고 있는 process가 보입니다. 이 파일을 열려면 `CAP_SYS_ADMIN`이나 `CAP_CHECKPOINT_RESTORE`가 필요하고, probe를 붙일 때도 `CAP_SYS_ADMIN`이 필요할 수 있습니다. root는 이 권한이 있고, container에서는 `--cap-add`로 `SYS_ADMIN`을 더하거나 `--privileged`를 씁니다. 이 파일을 열 수 없으면 안내를 표시합니다.

trace가 도는 동안에도 계속 찾습니다. process가 program을 실행하면 그 뒤 3초 동안 그 process를 몇 번 다시 확인하고, 모든 process도 2초 이상의 간격으로 다시 확인합니다. process가 많은 host에서는 이 간격이 길어집니다. 새 파일을 쓰는 process를 찾으면 그 파일에 probe를 붙이며, 그 전에 보낸 요청은 보이지 않습니다. kernel은 program 실행 알림을 처음 network namespace에만 보냅니다. host network나 `--pid=host`를 쓰지 않는 container처럼 다른 network namespace나 PID namespace에서 edc를 실행하면 안내를 표시하고, 모든 process를 다시 확인하는 방법만 씁니다. trace 도중에 kernel이 실행 알림을 멈추면 trace가 끝난 뒤 알려 줍니다.

파일 하나만 보려면 `--tls=<경로>`로 지정합니다. 이때는 다른 파일을 찾지 않습니다. 경로는 `--payload=all`처럼 띄우지 않고 붙여 씁니다.

probe는 지정한 파일에 붙습니다. program이 같은 내용의 다른 파일(복사본)을 적재하면 그 program은 보이지 않습니다. process가 적재한 파일은 `/proc/<pid>/maps`에서 확인합니다.

NSS는 TLS 상태를 확인할 `libssl3`와 평문 I/O를 읽을 `libnspr4`가 모두 필요합니다. 둘 중 하나를 지정하면 같은 디렉터리나 표준 library 디렉터리에서 다른 하나도 함께 선택합니다.

```bash
./bin/edc trace http --tls=/usr/lib/x86_64-linux-gnu/libssl3.so
```

NSS 프로그램을 시작하기 전에 trace를 시작합니다. TLS와 일반 파일·socket을 구분하려면 SSL 설정 호출을 관찰해야 합니다.

NSS 평문은 `PR_Read`, `PR_Recv`, `PR_Write`, `PR_Send`에서 읽습니다. NSPR은 이 함수로 일반 파일과 socket도 읽고 쓰므로, TLS를 쓰지 않는 program에서도 NSPR의 읽기와 쓰기마다 probe가 실행됩니다. `SSL_SECURITY`와 기본값 변경, model 복사, accept한 연결, `PR_Close`도 추적합니다. `SSL_SECURITY`를 끈 연결과 `PR_MSG_PEEK`로 읽은 내용은 TLS event로 표시하지 않습니다.

Go TLS는 Go 함수 표와 반환 위치의 probe로 평문을 읽습니다. 검증한 범위는 Linux amd64와 arm64의 Go 1.26.8과 1.27.1입니다.

```bash
./bin/edc trace http --tls=my-go-program
```

바이너리 경로나 command 이름을 지정합니다. 일반·stripped·PIE 바이너리를 지원하며, 자동 library 탐색으로 Go 바이너리를 고르지는 않습니다.

이 event에는 socket 주소가 없어 `--port` 필터를 사용하면 제외됩니다. 다른 Go 버전과 아키텍처는 지원하지 않습니다.

Go client의 응답 시간은 `Write` 진입부터 `Read` 반환까지입니다. 서버에서는 응답의 `Write` 진입까지 잽니다.

Go HTTP/2는 한 TLS 연결의 동시 stream 16개, 반복 header의 HPACK 처리, 64KiB body의 조각 결합을 검증했습니다.

rustls-ffi는 C 함수 `rustls_connection_read`와 `rustls_connection_write`에서 평문을 읽고, `rustls_connection_free`에서 연결 상태를 지웁니다.

```bash
./bin/edc trace http --tls=/usr/local/lib/librustls.so
```

이 event에는 socket 주소가 없습니다. `--port` 필터를 사용하면 제외됩니다. native Rust API는 지원하지 않습니다.

Mbed TLS는 `mbedtls_ssl_read`, `mbedtls_ssl_write`, `mbedtls_ssl_read_early_data`, `mbedtls_ssl_write_early_data`에서 평문을 읽습니다. `mbedtls_ssl_session_reset`과 `mbedtls_ssl_free`에서 연결 상태를 지웁니다. server 쪽 `mbedtls_ssl_read_early_data`는 handshake가 이미 받은 data를 돌려주므로, 이 event에는 socket 주소가 없고 `--port`를 쓰면 빠집니다.

```bash
./bin/edc trace http --tls=/usr/local/lib/libmbedtls.so
```

DTLS는 지원하지 않습니다.

wolfSSL은 `wolfSSL_read`와 `wolfSSL_write` 또는 `_ex` 변형에서 평문을 읽고, `wolfSSL_free`에서 연결 상태를 지웁니다.

```bash
./bin/edc trace http --tls=/usr/local/lib/libwolfssl.so
```

`--tls=claude`처럼 실행 파일 이름만 지정하면 PATH에서 찾습니다. 현재 디렉터리에 같은 이름의 파일이 있으면 그 파일을 먼저 사용합니다.

심볼 없는 BoringSSL은 Claude Code 2.1.291에 포함된 amd64 Bun 1.4.3 런타임을 지원합니다. GNU build ID는 `ca2032b38650b44e05b2074617d524c7475c80f0`입니다.

edc는 build ID와 함수 코드가 등록된 값과 일치해야 probe를 붙입니다. 다른 심볼 없는 BoringSSL 빌드는 확인한 오프셋을 별도로 등록해야 합니다.

```bash
./bin/edc trace http --tls=claude
```

`sudo`로 실행하면 `PATH`에 `~/.local/bin`이 없는 경우가 많습니다. edc가 실행 파일을 찾지 못하면 현재 shell에서 찾은 경로를 넘깁니다.

```bash
sudo ./bin/edc trace http --tls="$(command -v claude)"
```

OpenSSL, GnuTLS, NSS, wolfSSL, Mbed TLS, rustls-ffi, Go TLS, BoringSSL에서 읽은 요청과 응답 event에는 `"tls": true`가 붙고, event 행에는 event 이름 뒤에 `tls`가 표시됩니다. 목적지는 `https://`로 시작하고, 평문 HTTP의 목적지는 `http://`로 시작합니다. path, 상태 코드, 응답 시간, group 보기, `--payload`, 요약은 평문 HTTP와 같게 동작하고, `--payload`는 같은 header 값을 가립니다. HTTPS의 body에는 token이 들어 있는 경우가 많으므로, 출력을 공유하기 전에 확인합니다.

TLS 위의 HTTP/2는 h2c처럼 해석해서 stream마다 method, path, 상태 코드, 응답 시간을 표시합니다. frame과 header 표를 따라가야 하므로 HTTP/2 연결의 평문은 모두 읽고, 그래서 바쁜 HTTP/2 연결은 HTTP/1보다 비용이 크고 다른 연결의 event를 잃게 할 수 있습니다. 비용을 줄이려면 `--port`를 씁니다. HTTP/2 연결의 평문을 잃으면 그 방향은 더 읽지 않고 잃은 event로 셉니다. trace를 시작하기 전에 맺은 HTTP/2 연결은 해석하지 않습니다. HTTP/2에서는 `--payload`가 시작 줄과 body를 표시합니다. 시작 줄은 method와 path, 또는 상태 코드로 만들고, header는 표시하지 않습니다. HTTP/2 event는 body가 끝날 때 출력합니다. `--payload`는 body의 앞 4KiB를, `--payload=all`은 1MiB까지 담습니다. 상한에서 body를 잘랐거나, stream이나 trace가 끝나기 전에 body가 끝나지 않으면 event에 `"payload_truncated": true`가 붙습니다. 기다리는 body가 4096개나 64MiB를 넘으면 가장 오래된 event를 먼저 이 표시와 함께 출력하고, 어느 방향을 더 읽지 않을 때도 그렇게 합니다. `--payload` 없이 연 전체 화면은 HTTP/2 body를 보여 주지 않고, header가 오면 바로 event를 보여 줍니다.

Bun, `node`, Python `asyncio`처럼 TLS 함수 안에서 socket을 쓰지 않는 program은 어느 연결인지 알 수 없습니다. 이런 event에는 process는 있지만 `source`와 `destination`이 없고, `target`은 `Host` header입니다. 요약은 이 event 수를 `TLS plaintext without an address`로 표시하고, JSON에는 `tls_unmapped`가 붙습니다. `--port`를 쓰면 이런 평문은 port를 확인할 수 없어 표시하지 않고 같은 수에 더합니다.

`--tls`는 OpenSSL이 내보내는 `SSL_read`와 `SSL_write`(또는 `SSL_read_ex`와 `SSL_write_ex`), GnuTLS의 `gnutls_record_recv`와 `gnutls_record_send`, 앞서 설명한 NSS, wolfSSL, Mbed TLS, rustls-ffi, BoringSSL을 봅니다. Debian과 Ubuntu의 `wget`과 `git`은 GnuTLS를 씁니다. Java는 지원하지 않습니다. Go는 앞서 설명한 범위만 지원합니다. 심볼이 없는 program은 지원하는 Go 바이너리와 등록된 BoringSSL 빌드를 지원합니다. `--tls=<경로>`로 지정한 파일은 symbol table도 읽으므로, strip하지 않은 정적 program도 보입니다. `openssl s_server -www`처럼 OpenSSL의 SSL BIO로 읽고 쓰는 program도 보이지 않습니다.

edc는 page fault 없이 평문을 읽습니다. 그 순간 kernel이 그 page를 바꾸고 있으면(예: transparent huge page를 쪼개거나 합칠 때) 그 호출의 평문을 읽지 못합니다. 드문 일이며, edc는 이를 잃은 event로 셉니다.

이 파일을 쓰는 모든 process에서 함수가 불릴 때마다 probe가 실행되며, `--process`로 가린 process도 마찬가지입니다. 끝날 때 kernel이 probe를 하나씩 지우므로, Ctrl-C를 누른 뒤 몇 초 지나서 끝날 수 있습니다. `--tls`가 요구하는 kernel 버전은 `trace http`와 같습니다.

amd64의 Linux 6.11, 6.12.14 전의 6.12, 6.13.3 전의 6.13에서는 Docker container처럼 seccomp filter 아래에서 도는 process가 TLS 호출에서 돌아올 때 종료될 수 있습니다. 이런 kernel에서는 probe를 붙이기 전에 경고를 표시하고, 전체 화면을 열기 전에 Enter를 기다립니다. 멈추려면 Ctrl-C를 누릅니다. 배포판 kernel에는 수정이 따로 들어 있을 수 있습니다.

`--tls`가 없으면 HTTPS의 요청은 kernel에서 암호문으로만 보이므로 표시하지 않습니다. TLS 없는 HTTP/2(h2c, 예: cluster 안의 gRPC)는 해석해서 stream마다 method, path, 상태 코드, 응답 시간을 표시합니다. frame과 header 표를 따라가야 하므로 h2c 연결은 모든 byte를 읽고, 그래서 바쁜 h2c 연결은 HTTP/1보다 비용이 큽니다. 이 byte는 다른 HTTP 레코드와 같은 buffer를 쓰므로, 바쁜 h2c 연결이 있으면 다른 연결의 event도 잃을 수 있습니다. 비용을 줄이려면 `--port`를 씁니다. h2c 연결의 byte를 잃으면 그 방향은 더 읽지 않고 잃은 event로 셉니다. HTTP/3의 요청은 binary frame이라 표시하지 않습니다. HTTP/3은 UDP를 쓰므로 `tls_hello` event도 없습니다. edc는 한 번의 읽기나 쓰기가 시작되는 곳에서만 message를 찾습니다. 그래서 한 번의 읽기에 앞 응답의 끝과 다음 응답의 시작이 함께 들어 있으면 다음 응답을 놓칩니다. 프로그램이 message 하나를 여러 버퍼로 나눠 쓰면 첫 버퍼만 읽으므로, `Host` header는 첫 버퍼의 앞 512 byte 안에 있어야 합니다. 없으면 target은 서버 주소입니다. edc는 Linux 5.15 이상에서 이 field를 지원합니다.

Linux 5.15 이상에서 `trace mysql`을 사용하면 평문 MySQL 명령과 결과를 발생 즉시 출력합니다. edc는 kernel에서 MySQL port의 TCP 읽기와 쓰기마다 앞부분을 읽습니다. 기본 port는 3306이고, 다른 port는 `--port`로 지정합니다.

```bash
./bin/edc trace mysql
./bin/edc trace mysql --side server
./bin/edc trace mysql --port 3307
./bin/edc trace mysql --slow 250ms
./bin/edc trace mysql --group-by process
./bin/edc trace mysql --show-secrets
./bin/edc trace mysql --raw
```

`trace mysql`은 `trace http`처럼 두 쪽을 나눠 보여 줍니다. event 행 앞에 쪽이 붙습니다.

- `client:`는 이 host가 MySQL 서버로 보낸 명령입니다.
- `server:`는 로컬 MySQL 서버가 받은 명령입니다.

JSON event에는 `"side": "client"`나 `"side": "server"`가 붙습니다. 한 쪽만 보려면 `--side client`나 `--side server`를 씁니다. 로컬 port가 MySQL port이면 서버 쪽 socket이고, 상대 port가 MySQL port이면 client 쪽 socket입니다.

`--slow <duration>`은 `trace mysql`, `trace clickhouse`, `trace postgres`, `trace redis`에서만 씁니다. `250ms`, `1.5s` 같은 Go duration 문법을 받습니다. 묶지 않은 대화형 화면에서는 첫 paired response의 latency가 지정 시간 이상인 command 행만 남깁니다. 응답 없는 command, 짝이 없는 response, latency가 없는 response, TLS 행은 제외합니다. raw JSON, 비대화형 출력, 요약, group 보기, `--group-by`, `--side`에는 영향을 주지 않습니다.

| 보려는 것 | 명령 |
| --- | --- |
| 이 host의 MySQL 전부 | `./bin/edc trace mysql` |
| 로컬 서버가 받은 명령 | `./bin/edc trace mysql --side server` |
| 이 host가 보낸 명령 | `./bin/edc trace mysql --side client` |
| port 3307의 MySQL | `./bin/edc trace mysql --port 3307` |
| 250ms 이상 걸린 paired command | `./bin/edc trace mysql --slow 250ms` |
| 쪽마다 process별 응답 시간 | `./bin/edc trace mysql --group-by process` |
| 가리지 않은 SQL 원문 | `./bin/edc trace mysql --show-secrets` |

명령은 아래 event 중 하나입니다. 필드는 JSON event의 `mysql` 객체에 들어 있습니다.

- `mysql_connect`는 로그인입니다. `user`, `database`, `server_version`이 있습니다.
- `mysql_query`는 텍스트 query입니다. `sql`이 있습니다.
- `mysql_prepare`는 prepared statement 준비입니다. `sql`이 있습니다.
- `mysql_execute`는 prepared statement 실행입니다. `statement_id`와, edc가 본 prepare의 `sql`이 있습니다. binary parameter 값은 읽지 않습니다.

응답은 아래 event 중 하나입니다.

- `mysql_ok`에는 `affected_rows`가 있고, prepare의 응답에는 `statement_id`가 있습니다. 로그인의 성공 응답도 `mysql_ok`입니다.
- `mysql_error`에는 `error_code`, `sql_state`, `message`가 있습니다.
- `mysql_result`에는 column 수인 `columns`가 있습니다. 행은 세지도 읽지도 않습니다.

응답 event에는 답한 명령의 `command`와 `sql`이 함께 들어 있습니다. `mysql_tls`는 명령이 아닙니다. 연결이 TLS를 쓴다는 표시이며 `tls: true`가 있습니다. edc는 그 연결을 더 읽지 않습니다.

MySQL은 한 번에 명령 하나에 답하므로, 명령 뒤에 오는 첫 응답 packet이 그 명령의 응답입니다. `latency_ms`는 client가 명령을 보낸 때부터 그 packet을 읽은 때까지입니다. 서버 쪽에서는 서버가 명령을 읽은 때부터 그 packet을 쓴 때까지입니다. client 응답 시간에는 network가 들어가고, 서버 응답 시간에는 서버가 처리한 시간만 들어갑니다. 첫 packet 뒤의 행은 읽지 않으므로 `latency_ms`는 마지막 행까지 걸린 시간이 아닙니다.

`COM_QUIT`, `COM_STMT_CLOSE`, `COM_STMT_SEND_LONG_DATA`는 응답이 없어서 event가 없습니다. ping이나 기본 database 변경 같은 다른 명령도 event가 없고, edc는 그 응답을 건너뜁니다.

edc는 기본으로 SQL의 작은따옴표와 큰따옴표 안 문자열을 `?`로 가립니다. 예를 들어 `INSERT INTO t VALUES (1,'?')`로 보입니다. 가리기는 scroll 보기, 전체 화면, `--raw`, 요약에 모두 적용됩니다. 숫자, 이름, 주석은 가리지 않으므로 주석이나 숫자에 비밀을 쓰지 않습니다. 서버가 `ANSI_QUOTES`를 쓰면 따옴표로 감싼 이름도 가려집니다.

`--show-secrets`를 사용하면 SQL 원문을 보냈던 그대로 봅니다. `--payload`는 필요 없습니다. `CREATE USER ... IDENTIFIED BY 'password'` 같은 문장에서는 password가 보이므로 이런 출력은 공유하지 않습니다. 전체 화면의 `m` 키는 `trace http`에서만 동작합니다. `trace mysql`에서는 명령줄의 `--show-secrets`를 사용합니다.

`mysql_error` event의 `message`는 가리지 않으며, 값이 들어 있을 수 있습니다. 예를 들어 `Duplicate entry '1' for key 't.PRIMARY'`에는 key 값이 있고, 접속 오류에는 user와 host가 있습니다. 출력을 공유하기 전에 값이 들어 있는지 확인합니다. edc는 로그인의 인증 data를 건너뛰고 저장하지 않습니다.

Ctrl-C 뒤의 요약은 쪽, 명령, SQL shape마다 한 행을 보여 줍니다. shape는 문자열 값과 숫자를 `?`로 바꾸고 공백을 한 칸으로 합칩니다. 그래서 `WHERE id = 7`과 `WHERE id = 8`은 한 행에 합쳐집니다. `IN` 목록의 항목 수가 달라도 합치지 않습니다. 행에는 명령 수, 오류 수, 응답이 없는 명령 수, 평균과 최대 응답 시간, process가 나옵니다. 두 쪽이 섞이면 쪽별 합계를 보여 주고 `SIDE` 열을 더합니다. 연결 수, TLS 연결 수, 압축 연결 수도 나옵니다. JSON에도 같은 내용이 들어 있습니다. `--group-by`를 쓰면 행에 `CMD`, `RSP`, `ERR`, `NOANS`, `AVGms`, `MAXms`가 나오고, group 보기는 server 쪽을 별도 행으로 둡니다.

`trace mysql`에는 다음 제한이 있습니다.

- TCP만 봅니다. `mysql -h localhost`는 unix socket을 쓰므로 `trace mysql`에는 아무것도 보이지 않습니다. `mysql -h 127.0.0.1`은 TCP입니다. unix socket의 raw payload는 `trace socket`으로 봅니다.
- TLS는 읽지 않습니다. MySQL 8.4의 `mysql` client는 TCP에서 기본으로 TLS를 쓰므로 많은 연결에 `mysql_tls`만 나옵니다. test에서는 `--ssl-mode=DISABLED`를 씁니다.
- 압축 연결은 읽지 않습니다. `compressed: true`인 `mysql_connect`와 로그인 결과까지만 보이고, 그 뒤에는 event가 없습니다.
- port 33060의 X Protocol은 읽지 않습니다.
- 명령에 query attribute 값이 있으면 SQL을 찾지 못해 `sql` 없이 명령만 보입니다.
- client가 한 번의 `writev`로 packet header와 payload를 서로 다른 buffer에서 쓰면 edc는 header만 읽어 명령을 보지 못합니다.
- 16MiB 이상의 명령은 packet 여러 개로 옵니다. edc는 앞부분만 보여 주고 응답을 짝짓습니다.
- `LOCAL INFILE`은 특별한 응답을 씁니다. edc는 짝짓지 못하므로 그 명령은 응답 없음으로 셉니다.
- 연결 중간에 trace를 시작하면 그 연결의 `mysql_connect`가 없습니다. edc는 이후 명령에서 packet 경계를 찾으므로 처음 명령 몇 개는 빠질 수 있습니다. prepare를 보지 못했으면 `mysql_execute`에는 `statement_id`만 보입니다.
- 명령은 읽기나 쓰기마다 앞 4KiB, 응답은 읽기나 쓰기마다 앞 1KiB만 가져옵니다. event의 SQL은 1,024 byte까지이고, 잘리면 `sql_truncated`가 true입니다.
- 잃어버린 event나 부분 송신 때문에 packet 경계가 어긋나면 그 연결의 다음 명령이 빠질 수 있습니다. 요약에서 lost event 수를 확인합니다.
- `docker run -p`는 `docker-proxy`를 거치므로 query 하나가 구간마다 한 번씩 보입니다. container 주소로 접속하면 피할 수 있습니다.

Linux 5.15 이상에서 `trace clickhouse`를 사용하면 평문 ClickHouse 질의를 발생 즉시 출력합니다. 한 번의 trace가 native TCP protocol과 HTTP interface를 함께 봅니다. edc는 kernel에서 두 port의 TCP 읽기와 쓰기마다 앞부분을 읽습니다. 기본 port는 native TCP 9000, HTTP 8123입니다.

native port는 `--port`로, HTTP port는 `--http-port`로 바꿉니다. `--http-port`는 `trace clickhouse`에서만 쓸 수 있고, 값은 1부터 65535까지입니다. 한쪽 interface만 보는 option은 없습니다.

두 port는 서로 달라야 합니다. 같으면 종료 코드 2로 끝납니다. 다른 option의 기본값과 겹치는 경우도 마찬가지입니다. 예를 들어 HTTP port의 기본값이 8123이므로 `--port 8123`만 주면 실패합니다.

```bash
./bin/edc trace clickhouse
./bin/edc trace clickhouse --side server
./bin/edc trace clickhouse --port 9001
./bin/edc trace clickhouse --port 9440 --http-port 18123
./bin/edc trace clickhouse --slow 500ms
./bin/edc trace clickhouse --group-by process
./bin/edc trace clickhouse --show-secrets
./bin/edc trace clickhouse --raw
```

`trace clickhouse`는 `trace mysql`처럼 두 쪽을 나눠 보여 줍니다. event 행 앞에 쪽이 붙습니다.

- `client:`는 이 host가 ClickHouse 서버로 보낸 질의입니다.
- `server:`는 로컬 ClickHouse 서버가 받은 질의입니다.

JSON event에는 `"side": "client"`나 `"side": "server"`가 붙습니다. 한 쪽만 보려면 `--side client`나 `--side server`를 씁니다.

`--slow <duration>`은 묶지 않은 대화형 화면에서 씁니다. `250ms`, `1.5s` 같은 Go duration 문법을 받습니다. 짝지은 응답의 latency가 지정 시간 이상인 질의 행만 남기고, 응답 없는 질의, 짝이 없는 응답, latency가 없는 응답은 제외합니다. `--raw`, `--json`, `--group-by`나 terminal이 아닌 출력과 함께 쓰면 종료 코드 2로 끝납니다.

| 보려는 것 | 명령 |
| --- | --- |
| 이 host의 ClickHouse 전부 | `./bin/edc trace clickhouse` |
| 로컬 서버가 받은 질의 | `./bin/edc trace clickhouse --side server` |
| 이 host가 보낸 질의 | `./bin/edc trace clickhouse --side client` |
| native port 9001의 ClickHouse | `./bin/edc trace clickhouse --port 9001` |
| HTTP port 18123의 ClickHouse | `./bin/edc trace clickhouse --http-port 18123` |
| 500ms 이상 걸린 paired 질의 | `./bin/edc trace clickhouse --slow 500ms` |
| 쪽마다 process별 응답 시간 | `./bin/edc trace clickhouse --group-by process` |
| 가리지 않은 SQL 원문 | `./bin/edc trace clickhouse --show-secrets` |

요청은 아래 event 중 하나입니다. 필드는 JSON event의 `clickhouse` 객체에 들어 있습니다.

- `clickhouse_connect`는 로그인입니다. `user`, `database`, `client_name`, `client_version`, `revision`이 있습니다.
- `clickhouse_query`는 질의입니다. `sql`, `query_id`, `user`, `database`, `compressed`가 있습니다.

응답은 아래 event 중 하나입니다.

- `clickhouse_hello`는 로그인의 응답입니다. `server_name`과 `server_version`이 있습니다.
- `clickhouse_end`는 질의의 끝입니다. edc가 data block을 읽었으면 `rows`가 있습니다.
- `clickhouse_exception`은 실패한 질의입니다. `error_code`, `error_name`, `message`가 있습니다.

응답 event에는 답한 요청의 `command`, `sql`, `query_id`가 함께 들어 있습니다. edc가 스트림의 끝(EndOfStream)을 보지 못했지만 질의가 끝났다고 판단할 수 있으면 `clickhouse_end`에 `end_inferred: true`가 붙습니다. `clickhouse_chunked`는 요청이 아니고, 연결이 chunked protocol을 쓴다는 표시입니다. edc는 그 연결을 더 읽지 않습니다.

native 질의의 latency는 `Query` packet부터 `EndOfStream` 또는 `Exception`까지입니다. 첫 응답 packet에서 끝나는 `trace mysql`과 다르므로, `latency_ms`는 결과의 끝까지 걸린 시간입니다. 서버 쪽에서는 서버가 질의를 읽은 때부터 마지막 packet을 쓴 때까지이고, client 쪽에서는 client가 마지막 packet을 읽은 때까지입니다. client 응답 시간에는 network가 들어갑니다.

HTTP 질의도 같은 event와 필드를 씁니다. `clickhouse` 객체마다 `interface`가 있고, 값은 `native`나 `http`입니다. HTTP 질의의 응답 event에는 `http_status`도 붙습니다. scroll 보기에서 HTTP 행은 쪽 뒤에 `http:`가 붙습니다. 예를 들어 `server: http: SELECT 1`입니다. 전체 화면의 상세 보기에는 `interface` 줄이 나오고, 응답이면 `http_status` 줄도 나옵니다. HTTP에서 `compressed`는 요청 body가 압축됐다는 뜻입니다. HTTP에는 `clickhouse_connect`와 `clickhouse_hello` event가 없고 `rows`도 채우지 않습니다.

edc는 다음 HTTP 요청을 질의로 봅니다.

- 경로가 `?`, `/?`, `/query?`로 시작하는 `GET`이나 `HEAD` 요청
- `/`나 위 경로로 보낸 `POST`나 `OPTIONS` 요청

`/ping`, `/replicas_status`, `/play` 같은 다른 endpoint는 event가 없습니다. 사용자 정의 `http_handlers` 경로도 질의로 보지 않으므로 event가 없습니다.

SQL은 URL의 `query` parameter, 줄바꿈, 요청 body의 앞부분을 이은 것입니다. body는 앞 4KiB만 가져옵니다. multipart body는 읽지 않습니다. 압축 body(`Content-Encoding`이나 `decompress=1`)도 읽지 않으므로 SQL이 `query` parameter만일 수 있습니다. `INSERT`는 data가 시작하기 전인 `VALUES`나 `FORMAT <이름>`까지만 남기고, insert data는 보이지 않습니다.

edc는 한 연결의 요청과 응답을 순서대로 짝짓습니다. 요청 body는 크기(`Content-Length`나 chunked)로 건너뛰므로 keep-alive 연결의 다음 요청을 찾습니다. `Expect: 100-continue`도 처리합니다.

HTTP 질의의 latency는 요청의 첫 record부터 응답이 끝나는 곳까지입니다. 끝은 마지막 chunk, `Content-Length`만큼의 byte, 또는 `__exception__` block입니다. edc는 `X-ClickHouse-Summary` header의 `elapsed_ns`를 쓰지 않습니다. 서버는 결과를 모두 보내기 전에 이 header를 쓰므로, 값이 마지막 chunk까지 걸린 시간보다 훨씬 작을 수 있습니다. 서버 쪽에서는 서버가 질의를 실행한 시간이고, client 쪽에서는 network가 들어갑니다.

기본값은 `wait_for_async_insert=1`입니다. async insert는 서버가 buffer를 flush한 뒤에 응답하므로, latency에 flush를 기다린 시간(`async_insert_busy_timeout_ms`)이 들어갑니다. `wait_for_async_insert=0`이면 서버가 insert를 접수할 때까지만 잽니다.

서버가 출력을 보내기 전에 질의가 실패하면 HTTP status가 오류를 알려 줍니다. edc는 `X-ClickHouse-Exception-Code`를 `error_code`로 하는 `clickhouse_exception`을 냅니다. 출력이 시작된 뒤에 실패하면 status는 200으로 남고 body 끝에 `__exception__` block이 붙습니다. edc는 이 block을 읽어 `clickhouse_exception`을 냅니다. 응답이 압축돼 있으면 block을 보지 못할 수 있고, 그러면 응답 없음으로 셉니다. edc가 읽지 못하면 `error_name`은 비어 있습니다.

edc는 기본으로 SQL의 문자열 literal 안 글자를 `?`로 가립니다. 가리기는 scroll 보기, 전체 화면, `--raw`, 요약에 모두 적용됩니다. 숫자, 이름, 주석은 가리지 않으므로 주석이나 숫자에 비밀을 쓰지 않습니다.

`--show-secrets`를 사용하면 SQL 원문을 보냈던 그대로 봅니다. `--payload`는 필요 없습니다. 이런 출력은 공유하지 않습니다. 로그인의 password는 `--show-secrets`를 써도 출력에 나오지 않습니다. setting과 query parameter의 값도 저장하지 않습니다.

HTTP에서는 `--show-secrets`를 써도 password, `X-ClickHouse-Key`, `Authorization: Basic`의 password, 다른 scheme의 `Authorization`, `X-ClickHouse-Quota`, `quota_key`, `session_id`, cookie, `param_*` 값, setting 값을 보이지 않습니다. `user`, `database`, `query_id`는 보입니다.

`clickhouse_exception` event의 `message`는 가리지 않으며, 값이 들어 있을 수 있습니다. 예를 들어 table 이름이나 key 값이 들어갑니다. 출력을 공유하기 전에 값이 들어 있는지 확인합니다.

Ctrl-C 뒤의 요약은 쪽, 명령, SQL shape마다 한 행을 보여 줍니다. shape는 문자열 값과 숫자를 `?`로 바꾸고 공백을 한 칸으로 합칩니다. 행에는 질의 수, 오류 수, 응답이 없는 질의 수, 평균과 최대 응답 시간, process가 나옵니다. 두 쪽이 섞이면 쪽별 합계를 보여 주고 `SIDE` 열을 더합니다. JSON에도 같은 내용이 들어 있습니다. `--group-by`를 쓰면 행에 `CMD`, `RSP`, `ERR`, `NOANS`, `AVGms`, `MAXms`가 나오고, group 보기는 server 쪽을 별도 행으로 둡니다. native와 HTTP 질의가 섞이면 요약에 `SIDE` 열 다음에 `INTERFACE` 열이 붙습니다. JSON 행에는 항상 `interface`가 있습니다. `Connections` 합계는 native 연결만 셉니다.

`trace clickhouse`에는 다음 제한이 있습니다.

- native TCP protocol과 HTTP/1.x를 읽습니다. TLS(native 9440, HTTPS 8443)와 HTTP/2는 읽지 않으며 event도 나오지 않습니다. chunked native 연결도 읽지 않습니다.
- HTTP/1.0 응답과 길이가 없는 응답은 edc가 끝을 볼 수 없습니다. 이런 질의는 응답 없음으로 셉니다.
- client가 HTTP 요청 두 개를 한 번에 쓰면 서버는 첫 요청만 답합니다. 그 연결의 짝이 어긋날 수 있습니다.
- TCP만 봅니다. unix socket 연결은 보이지 않습니다.
- 요청과 응답 모두 읽기나 쓰기마다 앞 4KiB만 가져옵니다. event의 SQL은 1,024 byte까지이고, 잘리면 `sql_truncated`가 true입니다.
- client 쪽에서 큰 결과는 압축 여부와 관계없이 마지막 읽기가 data block이나 HTTP chunk 중간에서 시작할 수 있습니다. 이때 edc는 질의의 끝을 보지 못해 응답 없음으로 셉니다. 서버 쪽에서는 끝이 보입니다.
- 무거운 질의의 비압축 `ProfileEvents` block은 thread와 counter 수에 따라 커집니다. 마지막 쓰기가 4KiB를 넘으면 edc는 끝을 보지 못합니다.
- 연결의 `Hello`를 보지 못하면 그 연결은 건너뜁니다. 연결 중간에 trace를 시작하면 그렇게 됩니다. client가 다시 접속해야 그 연결이 보입니다.
- 잃어버린 event나 부분 송신 때문에 packet 경계가 어긋나면 그 연결의 다음 질의가 빠질 수 있습니다. 요약에서 lost event 수를 확인합니다.
- host에 publish한 port로 보낸 질의는 client 행과 container 안 서버 행으로 보입니다. `docker-proxy` 자체의 행은 나오지 않고, 서버 행의 상대 주소는 Docker bridge gateway입니다. `--process clickhouse-serv`로 서버 process의 행만 남길 수 있습니다.

Linux 5.15 이상에서 `trace postgres`를 사용하면 평문 PostgreSQL 요청과 결과를 발생 즉시 출력합니다. edc는 kernel에서 PostgreSQL port의 TCP 읽기와 쓰기마다 앞부분을 읽습니다. 기본 port는 5432이고, 다른 port는 `--port`로 지정합니다.

```bash
./bin/edc trace postgres
./bin/edc trace postgres --side server
./bin/edc trace postgres --port 6432
./bin/edc trace postgres --slow 250ms
./bin/edc trace postgres --group-by process
./bin/edc trace postgres --show-secrets
./bin/edc trace postgres --raw
```

`trace postgres`는 `trace mysql`처럼 두 쪽을 나눠 보여 줍니다. event 행은 `client:`나 `server:`로 시작하고, JSON event에는 `"side": "client"`나 `"side": "server"`가 붙습니다. 한 쪽만 보려면 `--side client`나 `--side server`를 씁니다. `--slow`는 `trace mysql`과 같게 동작합니다.

| 보려는 것 | 명령 |
| --- | --- |
| 이 host의 PostgreSQL 전부 | `./bin/edc trace postgres` |
| 로컬 서버가 받은 요청 | `./bin/edc trace postgres --side server` |
| port 6432의 PostgreSQL | `./bin/edc trace postgres --port 6432` |
| 250ms 이상 걸린 paired 요청 | `./bin/edc trace postgres --slow 250ms` |
| 가리지 않은 SQL 원문 | `./bin/edc trace postgres --show-secrets` |

요청은 아래 event 중 하나입니다. 필드는 JSON event의 `postgres` 객체에 들어 있습니다.

- `postgres_connect`는 로그인입니다. `user`와 `database`가 있습니다.
- `postgres_query`는 요청 단위 하나입니다. `command`는 `query`, `prepare`, `execute`, `function` 중 하나입니다.
- `query`는 simple Query message 하나이고 `sql`이 있습니다.
- `prepare`와 `execute`는 Sync로 끝나는 extended batch이고 `sql`, `statement`, `executes`가 있습니다.

응답은 아래 event 중 하나입니다.

- `postgres_result`에는 CommandComplete의 `tag`(예: `SELECT 3`)와 `rows`가 있습니다. 로그인의 응답에는 `server_version`이 있습니다.
- `postgres_error`에는 `severity`, `sql_state`, `message`가 있습니다.

응답 event에는 답한 요청의 `command`와 `sql`이 함께 들어 있습니다. `postgres_tls`는 요청이 아닙니다. 그 연결이 TLS나 GSSAPI로 암호화되었다는 뜻이고, edc는 그 연결을 더 읽지 않습니다.

응답 시간은 요청 단위의 첫 message부터 ReadyForQuery까지입니다. client 쪽 응답 시간에는 network가 들어가고, 서버 쪽 응답 시간에는 서버의 처리와 결과 쓰기만 들어갑니다.

기본으로 SQL의 문자열 상수와 dollar quote 본문은 `?`로 바꿉니다. `--show-secrets`를 쓰면 보낸 SQL 그대로와 오류의 `detail`을 보입니다. password, SASL data, Bind parameter 값, COPY data, function call 인자는 `--show-secrets`를 써도 표시하지 않습니다. 오류의 `message`는 가리지 않으므로, 공유하기 전에 값이 들어 있는지 확인하세요.

Ctrl-C 뒤의 요약은 쪽, 명령, SQL shape마다 한 행을 씁니다. `--group-by`를 쓰면 행에 `CMD`, `RSP`, `ERR`, `NOANS`, `AVGms`, `MAXms`가 나옵니다.

`trace postgres`에는 다음 제한이 있습니다.

- TLS와 GSSAPI 암호화는 읽지 않습니다. 암호화한 연결마다 `postgres_tls` 행 하나만 나옵니다.
- TCP만 봅니다. unix socket 연결은 보이지 않습니다. `-h` 없는 `psql`은 unix socket을 쓰므로, 시험할 때는 `psql -h 127.0.0.1`을 씁니다.
- 응답 쓰기나 읽기마다 앞 4 KiB만 남깁니다. 서버는 큰 결과를 8 KiB씩 나눠 쓰므로 message 경계를 잃습니다.
- 큰 결과에서는 응답 쓰기나 읽기마다 끝 256 byte도 남깁니다. 그 byte가 온전한 CommandComplete나 ErrorResponse와 ReadyForQuery로 끝나고 기다리는 요청이 하나면 그 시각에 그 요청을 닫습니다. tag와 오류 field는 그 byte에서, `rows`는 `SELECT`나 `FETCH` tag에서 읽습니다.
- DataRow도 ReadyForQuery와 같은 6 byte로 끝날 수 있어서 그 6 byte만으로는 닫지 않습니다. ErrorResponse가 끝 256 byte보다 길면 그 요청은 응답 없음으로 셉니다.
- 경계를 잃을 때 기다리던 요청이 둘 이상이면 짐작하지 않고 응답 없음으로 셉니다. 다음 요청부터 다시 짝을 찾습니다.
- 읽기의 끝이 첫 buffer 밖에 있으면 그 읽기의 끝부분은 남기지 않습니다. 쓰기의 끝부분은 마지막 buffer에서만 읽습니다.
- 연결 중간에 trace를 시작하면 그 연결의 `postgres_connect`가 없습니다. edc는 다음 요청에서 경계를 찾습니다.
- event의 SQL은 1,024 byte까지 보이고, 잘랐으면 `sql_truncated`가 true입니다.

Linux 5.15 이상에서 `trace redis`를 사용하면 평문 Redis 명령과 응답을 발생 즉시 출력합니다. edc는 kernel에서 Redis port의 TCP 읽기와 쓰기마다 앞부분을 읽습니다. 기본 port는 6379이고, 다른 port는 `--port`로 지정합니다.

```bash
./bin/edc trace redis
./bin/edc trace redis --side server
./bin/edc trace redis --port 6380
./bin/edc trace redis --slow 5ms
./bin/edc trace redis --group-by process
./bin/edc trace redis --show-secrets
./bin/edc trace redis --raw
```

`trace redis`도 `trace mysql`처럼 두 쪽을 나눠 보여 주고, event 행은 `client:`나 `server:`로 시작합니다. `--slow`는 `trace mysql`과 같게 동작합니다.

JSON event의 `redis` 객체에는 아래 event의 필드가 들어 있습니다.

- `redis_command`는 명령입니다. `command`, `key`, `args`가 있습니다. pipeline은 명령마다 event 하나를 냅니다.
- `redis_reply`는 가장 오래 기다린 명령의 응답입니다. 응답 종류인 `reply`와 `elements`나 `bulk_length`가 있습니다.
- `redis_error`는 오류 응답입니다. `error_class`(예: `WRONGTYPE`)가 있습니다.
- `redis_push`는 pub/sub message나 RESP3 push입니다. 어느 명령의 응답도 아닙니다.

edc는 한 연결의 명령과 응답을 순서대로 짝짓습니다. MULTI 안의 명령은 `queued` 응답을 받고, EXEC의 응답은 결과 array입니다. 응답 시간은 명령이 도착한 때부터 그 응답이 도착한 때까지입니다. pipeline에서는 한 번에 쓴 명령들이 시작 시각을, 한 번에 쓴 응답들이 끝 시각을 함께 씁니다.

기본으로 인자 값, 응답 값, 오류 message는 표시하지 않습니다. `--show-secrets`를 쓰면 그 값의 짧은 preview를 보입니다. key 이름은 두 경우 모두 보입니다. AUTH, HELLO, MIGRATE, CONFIG GET, CONFIG SET, ACL, SENTINEL의 값은 `--show-secrets`를 써도 표시하지 않습니다.

Ctrl-C 뒤의 요약은 쪽과 명령 이름마다 한 행을 쓰고, 명령 수, 오류 수, 응답 없는 명령 수, 평균과 최대 응답 시간을 보입니다.

`trace redis`에는 다음 제한이 있습니다.

- TLS는 읽지 않습니다. TLS 연결에서는 RESP 경계를 찾지 못해 event가 나오지 않습니다.
- TCP만 봅니다. unix socket 연결은 보이지 않습니다.
- 요청은 앞 4 KiB, 응답 쓰기나 읽기는 앞 1 KiB만 남깁니다. 5,000개 항목의 `LRANGE`처럼 아주 큰 array 응답은 경계를 잃습니다. 그 명령은 응답 없음으로 세고, 다음 명령에서 경계를 다시 맞춥니다.
- bulk 값은 크기만큼 건너뛰므로 10 KB `SET`도 응답과 짝을 찾습니다. 5,000개 값을 넣는 `RPUSH`처럼 인자 header가 캡처 밖에 있는 명령은 `truncated: true`로 표시하고 응답과 짝짓지 않습니다.
- `CLIENT REPLY OFF`나 `CLIENT REPLY SKIP` 뒤에는 서버가 답하지 않습니다. 이 명령에는 `no_reply: true`가 붙고, 응답 없음으로 세지 않습니다.
- PSYNC나 SYNC 뒤의 복제 흐름은 응답으로 읽지 않습니다. replica는 master에 1초마다 `REPLCONF ACK`를 보냅니다. master에서 `--side server`로 보면 replica마다 1초에 하나씩 `no_reply: true`인 `REPLCONF ACK`가 보입니다.
- 연결 중간에 trace를 시작하면 다음 명령에서 경계를 찾습니다.

Linux에서 `trace socket`을 사용하면 unix domain socket 파일 하나에서 일어나는 일을 출력합니다. socket 파일의 경로를 지정하면 그 socket의 connect, accept, send, recv, data 끝(`eof`), close를 표시합니다. option은 경로 앞이나 뒤에 씁니다.

```bash
./bin/edc trace socket /run/docker.sock
./bin/edc trace socket /run/docker.sock --payload
./bin/edc trace socket /run/php/php-fpm.sock --payload=all --raw
```

목적지는 socket 경로입니다. event 칸에는 호출과 byte 수가 나오고, 호출이 실패하면 `connect ECONNREFUSED`처럼 errno 이름이 나옵니다. 출처(source)는 상대 process입니다. 서버 쪽에서는 연결한 process입니다. 클라이언트 쪽에서는 연결을 accept한 process이고, 그 뒤 서버 쪽에서 data를 주고받은 process가 있으면 가장 최근의 그 process입니다. accept 전에는 `listen()`을 호출한 process라서, systemd가 여는 socket에서는 `systemd`로 나옵니다.

서버 쪽에는 `accept` event가 나옵니다. 이 event는 `accept 120µs`처럼 연결이 backlog에서 기다린 시간을 보여 줍니다. 이 시간이 길면 서버가 연결을 늦게 받는 것이고, worker가 모두 바쁜 경우가 그 예입니다. trace를 시작하기 전에 이미 시작된 `accept` 호출은 보이지 않지만, 그 연결의 다음 서버 쪽 호출부터는 상대가 바르게 나옵니다.

`--payload`를 사용하면 send와 recv마다 data의 앞 4KiB를 볼 수 있고, `--payload=all`을 사용하면 호출마다 1MiB까지 볼 수 있습니다. edc는 호출 하나의 data를 16KiB 조각으로 읽어 event 하나로 합칩니다. stream socket에는 message 경계가 없으므로 event 하나는 message가 아니라 호출 하나입니다. edc는 payload의 형식을 모르므로 어떤 값도 가리지 않고, 그래서 `--show-secrets`는 쓸 수 없습니다. `docker.sock`의 `X-Registry-Auth` header처럼 token이나 비밀번호가 보일 수 있으므로, 출력을 공유하기 전에 확인합니다. 전체 화면에서는 `--payload`가 없어도 호출마다 앞 4KiB를 모읍니다. `v`를 누르면 payload 줄이 보이고, Enter를 누르면 payload 전체가 보입니다.

서버가 재시작하면서 socket 파일을 다시 만들면 edc는 1초 안에 새 파일을 찾습니다. 이전 파일로 맺은 연결도 계속 표시합니다. `trace socket`은 stream socket만 지원합니다. datagram과 seqpacket socket, abstract socket, socketpair는 볼 수 없습니다. `sendfile`과 `splice`로 옮긴 data는 보이지 않습니다. 실패한 connect는 program이 명령과 같은 경로를 쓸 때만 표시합니다.

Linux에서 `trace priv`를 사용하면 process가 권한을 어떻게 쓰는지 볼 수 있습니다. edc는 세 가지를 보고 끝날 때 요약을 한 번 출력합니다.

- capability 검사: `capable()`과 `ns_capable()`의 결과를 kernel 안에서 process, capability, 결과별로 셉니다.
- 자격 변경: thread의 uid, euid, gid, egid가 바뀔 때마다 기록합니다. 예를 들어 `setuid()`, `setresgid()`, `sudo` 같은 setuid program의 exec입니다.
- module 적재: process가 적재한 kernel module과 그 이름을 기록합니다.

```bash
./bin/edc trace priv --duration 30s
./bin/edc trace priv --denied --container web
./bin/edc trace priv --raw
```

capability 표는 거부된 검사를 먼저 보여 줍니다. `denied` 뒤의 `(noaudit)`는 kernel이 무엇을 할지 고르려고 process에 capability가 있는지만 물어본 것이라는 뜻입니다. 이런 검사는 동작을 막지 않으며, 요약의 `denied`에도 들어가지 않습니다. 예를 들어 database process가 이렇게 `CAP_SYS_ADMIN`을 1초에 수백 번 물을 수 있습니다. `--denied`를 주면 허용된 검사를 숨깁니다.

`DENIED`, `CREDENTIALS`, `MODULES`는 event를 하나씩 보여 줍니다.

```
17:54:46.615953  sudo 3969/3969  cred  euid 1000→0
17:54:46.648788  modprobe 3970/3970  module  dummy
```

거부된 검사는 CPU마다 1초에 최대 1,000개를 출력하고 나머지는 `sampled_out`으로 셉니다. capability 집합만 바뀌는 것처럼 uid, euid, gid, egid가 그대로인 자격 변경은 출력하지 않습니다. `--raw`는 event를 하나씩 JSON으로 출력하고 마지막 줄에 요약을 둡니다.

Linux 6.17과 Linux 5.15에서 edc의 집계를 맞춰 보았습니다. 시험 thread가 자기 effective 집합에서 `CAP_SYS_NICE`를 지우고 nice 값을 낮추려 100번 시도하자, edc는 `CAP_SYS_NICE` 거부 100번을 셌습니다. egid를 12345로 바꿨다가 되돌린 thread는 `egid 0→12345`와 `egid 12345→0`으로 나왔습니다. 5.15 VM에서는 `sudo`가 `euid 1000→0`과 `uid 1000→0`으로, `modprobe dummy`가 module 적재로 나왔습니다.

Linux 6.17, CPU 12개 host에서 capability program은 1초에 약 670번 돌았고 검사 한 번에 0.6µs가 걸렸습니다. 자격 program은 1초에 24번 돌았고 0.75µs가 걸렸습니다.

BTF와 `cap_capable`, `commit_creds` 함수, `module_load` tracepoint가 필요합니다. 하나가 없으면 알림을 출력하고 나머지를 봅니다.

Linux에서 `trace drop`을 사용하면 kernel이 패킷을 버린 이유를 볼 수 있습니다. 버린 패킷마다 이유, kernel 함수, 주소와 port, 크기를 보여 줍니다. 패킷이 local socket에 속하면 process도 보여 줍니다.

```bash
./bin/edc trace drop
./bin/edc trace drop --reason NO_SOCKET,SOCKET_RCVBUFF
./bin/edc trace drop --container web --raw
```

이유는 kernel이 쓰는 이름입니다. 예를 들어 `NO_SOCKET`은 그 port를 쓰는 socket이 없는 경우, `SOCKET_RCVBUFF`는 socket의 수신 buffer가 가득 찬 경우, `NETFILTER_DROP`은 방화벽 규칙이 버린 경우입니다. 이유는 Linux 5.17 이상에서 나옵니다. 그 전 kernel에서는 이유가 `unknown`이고 함수만 보입니다. `--reason`에 쉼표로 나눈 이름을 주면 그 이유만 봅니다. 이름은 대소문자를 가리지 않습니다.

kernel은 정상 동작 중에도 패킷을 해제합니다. 예를 들어 program이 읽지 않은 data가 남은 socket을 닫으면 `QUEUE_PURGE`나 `TCP_ABORT_ON_DATA`가 나옵니다. 이런 버림도 program이 data를 읽지 않았다는 뜻이라 함께 보여 줍니다.

요약의 이유별 합계는 정확합니다. 한 CPU에서 1초에 1,000건이 넘게 버려지면 그 1초 동안은 event를 1,000건만 보내고 나머지는 세기만 합니다. 이 수는 요약에 `Sampled out`으로 나옵니다. process는 socket이 있는 패킷에만 붙습니다. 닫힌 port로 온 패킷에는 socket이 없습니다.

Linux에서 `trace exec`를 사용하면 process가 뜨고 끝나는 것, 누가 신호를 보냈는지, OOM killer가 어느 process를 끝냈는지 볼 수 있습니다. `exec` event는 PID, 부모, 사용자, 명령줄의 처음 256 byte를 보여 줍니다. `exit` event는 종료 코드, process를 끝낸 신호, core dump 여부, 산 시간을 보여 줍니다. 산 시간은 task의 시작 시각에서 재므로 trace 전에 뜬 process에도 나옵니다. `signal` event는 신호, 보낸 쪽, 받는 쪽, kernel이 전달했는지 무시했는지를 보여 줍니다.

```bash
./bin/edc trace exec --duration 15s
./bin/edc trace exec --process cron,sh --group-by parent
./bin/edc trace exec --signal all --raw
./bin/edc trace exec --group-by event --json exec.json
```

요약은 process 이름으로 행을 묶습니다. exec 수, exit 수, 0이 아닌 코드로 끝난 수, 신호로 끝난 수, 1초를 못 살고 끝난 수를 세고 평균과 가장 긴 수명을 보여 줍니다. `EXEC`와 `SHORT`가 함께 높으면 짧은 process가 쏟아지는 것입니다. 예를 들어 1초마다 shell을 띄우는 health check가 그렇습니다. 누가 띄우는지는 `--group-by parent`로 봅니다. 두 번째 표는 신호를 보낸 쪽과 받는 쪽으로 묶습니다. 결과 뒤의 `(kernel)`은 kernel이 만든 신호라는 뜻입니다. 예를 들어 fault 뒤의 `SIGSEGV`나 OOM killer의 `SIGKILL`이고, 이때 보낸 쪽은 그 순간 실행 중이던 process입니다.

`oom` event는 OOM killer가 고른 process를 보여 줍니다. `TRIGGER` 칸은 메모리가 모자랄 때 메모리를 할당하려던 task입니다. Linux 6.x에서는 victim의 이름, anon·file·shmem RSS, `oom_score_adj`도 보여 줍니다. Linux 5.15의 kernel은 victim의 PID만 줍니다. victim의 `exit` event에는 `oom-killed`가 붙고, 요약의 `OOM` 칸이 이 종료를 셉니다. 이 표시는 process의 kernel 상태에서 읽으므로 Linux 5.15에서도 정확합니다. `oom` event에서 `--container`는 TRIGGER task의 cgroup으로 맞춥니다.

기본으로는 process를 끝내거나 core를 남기는 신호만 기록합니다. `HUP`, `INT`, `QUIT`, `ILL`, `TRAP`, `ABRT`, `BUS`, `FPE`, `KILL`, `USR1`, `SEGV`, `USR2`, `PIPE`, `TERM`, `STKFLT`, `XCPU`, `XFSZ`, `SYS`입니다. `CHLD`나 `ALRM`처럼 늘 오가는 신호는 뺍니다. `--signal`에 쉼표로 나눈 이름이나 번호, `all`, `none`을 줍니다. 필터가 버린 신호 수는 요약의 `filtered_signals`에 나옵니다. `--process`는 이름이나 PID를 받고, 신호는 받는 쪽으로 맞춥니다. `--container`는 container 하나의 event만 남깁니다.

명령줄은 exec 시점에 새 process의 메모리에서 읽으므로 program이 받은 인자이고, 나중에 바뀐 값은 아닙니다. 읽지 못하면 실행 파일 경로를 보여 줍니다. 256 byte보다 긴 줄은 `…`로 끝나고 `args_truncated`에 셉니다. 스레드의 종료는 `exit` event가 아니고, process의 마지막 스레드만 `exit`를 냅니다. kernel thread는 보이지 않습니다. `sched_process_exec`, `sched_process_exit`, `signal_generate` tracepoint와 BTF가 필요합니다. `oom` event에는 tracefs의 `oom/mark_victim` tracepoint도 필요합니다. container처럼 tracefs가 없으면 알림을 출력하고 나머지 event를 봅니다.

Linux에서 `trace lock`을 사용하면 thread가 lock을 기다리는 곳을 볼 수 있습니다. edc는 두 가지 기다림을 재고 끝날 때 요약을 한 번 출력합니다.

- kernel lock 기다림은 kernel 안의 spinlock, rwlock, mutex, rwsem, per-CPU semaphore 경합입니다. Linux 5.19 이상에 있는 `lock:contention_begin`과 `lock:contention_end` tracepoint를 씁니다. mutex 기다림은 task가 spin을 시작할 때 시작하고, spin 뒤에 잠든 시간까지 포함합니다.
- futex 기다림은 사용자 공간에서 부른 `futex_wait`입니다. 경합한 `pthread_mutex`, condition variable, Go runtime이 모두 이렇게 기다리므로 futex 기다림이 늘 경합은 아닙니다.

```bash
./bin/edc trace lock --duration 10s
./bin/edc trace lock --process api --kind futex
./bin/edc trace lock --container db --json lock.json
```

`KERNEL LOCK` 표는 lock, lock 종류, 부른 함수로 기다림을 묶습니다. 정적 lock은 `tasklist_lock`이나 `rtnl_mutex`처럼 이름을, 나머지는 주소를 보여 줍니다. 종류는 `perf lock`의 이름을 씁니다: `spinlock`, `rwlock:R`, `rwlock:W`, `rwsem:R`, `rwsem:W`, `mutex`, `rtmutex`, `pcpu-sem:R`, `pcpu-sem:W`. 부른 함수는 stack에서 lock 함수가 아닌 첫 kernel 함수입니다. `PROCESS`는 가장 자주 기다린 process입니다.

`FUTEX` 표는 process와 futex 주소로 기다림을 묶습니다. 주소가 전역 변수이면 ELF 심볼에서 `main.hotWord`나 `runtime.sched+0x120`처럼 이름을 찾습니다. 나머지 주소는 `[anon] 0x7f3a1c000960`처럼 mapping과 주소를 보여 줍니다. `-s`로 build한 Go program처럼 심볼표가 없는 program은 file 이름만 나옵니다. `CODE`는 기다린 시간이 가장 긴 stack의 안쪽 두 사용자 frame입니다.

edc의 집계를 맞춰 보았습니다. 시험 program이 전역 futex 하나에서 800번 기다렸고, edc는 Linux 6.17과 Linux 5.15에서 모두 `main.hotWord`의 기다림 800번을 셌습니다. thread 8개가 메모리를 mapping하고 해제하는 program에서 `perf stat`은 `lock:contention_end`를 359,516번, edc는 359,506번 셌습니다. `perf stat`은 program이 이름을 얻기 전인 fork부터 셉니다.

trace가 도는 동안 host의 모든 kernel lock 경합과 futex 기다림이 BPF program을 거칩니다. Linux 6.17, CPU 12개 host에서 고른 process의 경합 한 번은 두 program에서 1.57µs와 0.34µs가 걸렸고, 대부분은 stack을 읽는 시간입니다. 필터가 고르지 않은 process는 0.26µs와 0.07µs였습니다. futex 기다림은 1.59µs와 0.42µs, 고르지 않으면 0.29µs와 0.08µs였습니다. 시험 program은 trace가 있을 때와 없을 때 같은 시간에 같은 양의 일을 했습니다. trace가 경합 한 번을 길게 만들어 경합 횟수는 바뀔 수 있습니다. 같은 program이 아무것도 고르지 않는 필터에서는 358,687번, trace 대상일 때는 203,922번 경합했습니다.

Linux 5.15에는 contention tracepoint가 없습니다. 그러면 알림을 출력하고 futex 기다림만 보여 줍니다. BTF와 task local storage가 필요합니다.

Linux에서 `trace syscall`을 사용하면 process별 system call 수와 시간을 볼 수 있습니다. edc는 kernel 안에서 세고, 끝날 때 요약을 한 번 출력합니다. `--slow`나 `--errors`를 주면 조건에 맞는 호출도 하나씩 출력합니다.

```bash
./bin/edc trace syscall --duration 10s
./bin/edc trace syscall --process nginx,1234 --duration 30s
./bin/edc trace syscall --group-by syscall --json syscall.json
./bin/edc trace syscall --container web --group-by cgroup
./bin/edc trace syscall --process api --errors ENOENT,EACCES
./bin/edc trace syscall --syscall openat,statx --slow 10ms --raw
```

행 하나는 process 이름 하나와 system call 하나입니다. 행마다 호출 수, 오류 수와 가장 많은 errno, 호출 안에 머문 시간의 합계, 평균, p95, 최댓값을 보여 줍니다. 시간은 CPU 시간이 아니라 호출 안에 머문 실제 시간입니다. 그래서 `epoll_wait`나 `futex`처럼 기다리는 호출이 위에 옵니다. 합계는 모든 스레드의 시간을 더하므로 trace 시간보다 길 수 있습니다. 행은 합계 시간 순서입니다. 표에는 20행이 나오고, `--json`에는 모든 행이 들어갑니다. p95는 `top -d`와 같은 histogram bucket의 위쪽 경계라서 `<1.05s`처럼 보입니다.

`--group-by syscall`, `--group-by process`, `--group-by cgroup`은 한 열로 묶어 더합니다. `--process`는 쉼표로 나눈 PID와 command 이름을 받습니다. kernel 안에서 비교하므로 이름은 process의 command 이름과 정확히 같아야 하고, 15 byte까지만 봅니다. PID는 그 process의 모든 스레드에 맞습니다. `--container`는 container 하나의 호출만 셉니다.

`--syscall`에 쉼표로 나눈 이름을 주면 그 system call만 셉니다. kernel 안에서 번호를 확인하므로 다른 호출은 map에 들어가지 않습니다. 이름은 CPU 아키텍처의 system call 표에서 찾습니다. amd64에서는 같은 이름의 32비트 호출도 함께 고릅니다.

`--slow <시간>`을 주면 그 시간 이상 걸린 호출을 하나씩 출력합니다. `--errors all`은 실패한 모든 호출을, `--errors ENOENT,EACCES`는 그 오류로 실패한 호출만 출력합니다. 두 조건 모두 kernel 안에서 확인합니다. 한 줄에는 시각, process, PID와 스레드 ID, system call, 반환값이나 errno, 호출 안에 머문 시간, 경로가 나옵니다.

```
15:04:05.000123  nginx 1234/1240  openat  ENOENT  12.3us  /etc/nginx/missing.conf
```

경로는 출력하는 호출에서만, 호출이 돌아올 때 읽습니다. 256 byte까지 읽고, 더 긴 경로는 `…`로 끝납니다. 경로는 `openat`, `statx`, `unlinkat`, `mkdirat`, `execve` 같은 약 50개 호출의 첫 경로 인자입니다. 32비트 호출의 경로는 읽지 않습니다. 성공한 `execve`의 경로도 읽지 않습니다. 새 program이 register와 memory를 바꿨기 때문입니다. 호출 중에 다른 스레드가 경로 buffer를 바꾸면 바뀐 글자가 나올 수 있습니다. 경로의 제어 문자는 `\xNN`으로 보여 줍니다.

edc는 CPU마다 1초에 1,000줄까지만 출력합니다. 넘친 줄은 요약의 `sampled_out`으로 세고, 표는 모든 호출을 계속 셉니다. event buffer에 들어가지 못한 줄은 `ring_lost`로 셉니다. `--json`을 주면 줄 수만 세고 출력하지 않습니다. `--raw`를 주면 줄마다 JSON으로 출력하고, 마지막 줄에 요약을 붙입니다.

`IN FLIGHT AT STOP` 표는 trace 중에 시작해 끝날 때까지 돌아오지 않은 호출입니다. trace 내내 기다린 스레드는 호출이 돌아오지 않아 위의 표에 나오지 않습니다. trace 전에 시작한 호출은 시작 시각 없이 돌아옵니다. 요약은 이 복귀를 `fork`나 `clone`으로 생긴 스레드의 첫 복귀와 함께 `unmatched`로 셉니다. `--process`, `--container`, `--syscall`을 주면 세지 않고 `-`로 보여 줍니다. `exit`와 `exit_group`은 돌아오지 않으므로 시간 없이 횟수만 셉니다.

edc 자신의 호출은 뺍니다. edc가 자기 PID namespace가 있는 container 안에서 실행되면 자신의 호출을 찾을 수 없어서, 요약 뒤에 이 사실을 알립니다. amd64에서 32비트 program은 다른 system call 번호를 씁니다. 이 호출은 이름 뒤에 `(32-bit)`를 붙여 보여 줍니다. 신호로 끊긴 호출은 kernel 안에서만 쓰는 재시작 값으로 돌아올 수 있습니다. 예를 들어 `ERESTARTSYS`입니다. 표는 이 값도 이름으로 보여 줍니다.

집계는 크기가 정해진 per-CPU map에 쌓습니다. edc는 map을 모두 합쳐 32 MiB, 최대 16,384개 항목으로 잡습니다. 항목 하나는 process 이름 하나와 system call 하나입니다. map이 가득 차면 넘친 호출은 세지 않습니다. 요약은 그 수를 `map_full`로 보여 주고, edc는 경고를 출력합니다. `--group-by cgroup`은 항목마다 cgroup을 더하므로 항목을 더 씁니다. 예를 들어 SSH login마다 새 cgroup이 생길 수 있습니다.

trace가 도는 동안 host의 모든 system call이 BPF program 두 개를 거칩니다. 필터에 걸리지 않는 process도 이 비용을 냅니다. 12 CPU x86_64 host(Linux 6.17)에서 `perf bench syscall basic`으로 잰 호출당 비용은 다음과 같습니다.

| 조건 | `getppid` | 추가 비용 |
|---|---|---|
| trace 없음 | 0.577 µs | - |
| trace 중, 필터에 걸리지 않는 process | 0.735 µs | 0.16 µs |
| trace 중, 대상 process | 0.809 µs | 0.23 µs |

이 host에서는 초당 약 32,500건의 호출이 전체 CPU 시간의 0.1% 미만을 더했습니다. 바쁜 server에서 오래 trace하려면 먼저 그 host에서 비용을 재 보세요.

`--syscall`을 주면 다른 호출의 비용이 줄어듭니다. `--syscall openat`일 때 `getppid`는 0.697 µs가 걸렸습니다. 같은 측정에서 trace가 없을 때는 0.585 µs였습니다. 실패한 호출은 errno도 세므로 약 0.1 µs가 더 듭니다. 출력하는 줄 하나는 약 0.6 µs, 상한을 넘어 세기만 하는 줄은 약 0.05 µs가 더 듭니다. 상한이 있으므로 12 CPU host는 1초에 최대 12,000줄을 출력합니다.

`sys_enter`, `sys_exit` tracepoint와 BTF, task local storage, task iterator, BPF ring buffer가 필요합니다.

Linux에서 `trace reclaim`을 사용하면 task가 메모리를 기다린 시간을 볼 수 있습니다. edc는 네 가지 정지를 kernel 안에서 세고, 끝날 때 요약을 한 번 출력합니다.

| 종류 | task가 멈추는 때 |
|---|---|
| `direct` | zone의 남은 메모리가 모자라, 할당하던 task가 직접 메모리를 회수합니다. |
| `memcg` | cgroup이 `memory.max`나 `memory.high`에 닿아, task가 그 cgroup의 메모리를 회수합니다. |
| `throttle` | cgroup이 `memory.high`를 넘었습니다. task가 사용자 공간으로 돌아가기 전에 kernel이 회수하고 task를 재웁니다. |
| `compact` | task가 큰 page가 필요해 직접 메모리를 compaction합니다. |

```bash
./bin/edc trace reclaim --duration 30s
./bin/edc trace reclaim --container web --slow 10ms
./bin/edc trace reclaim --group-by cgroup --json reclaim.json
```

행 하나는 process 이름 하나와 종류 하나입니다. 정지 횟수, 합계·평균·p95·최대 시간, 그리고 `direct`와 `memcg`가 회수한 메모리를 보여 줍니다. 합계는 모든 thread의 정지를 더하므로 trace보다 길 수 있습니다. `throttle` 정지에는 그 안에서 돈 `memcg` 회수가 들어 있으므로 두 행을 더하지 마세요. `--group-by process`, `--group-by cgroup`, `--group-by kind`로 한 칸에 대해 행을 합칩니다. `--process`와 `--container`는 `trace syscall`과 같습니다.

`--slow <duration>`을 주면 그 시간 이상 멈춘 정지를 하나씩 출력합니다.

```
02:13:24.479122  api 1456919/1456920  throttle  588.57ms  cgroup 15040023
```

CPU마다 1초에 최대 1,000줄을 출력하고, 나머지는 `sampled_out`으로 셉니다. `--slow`와 함께 `--raw`를 주면 줄마다 JSON으로 출력합니다.

`kswapd`와 `kcompactd`의 작업은 세지 않습니다. 이 kernel thread는 뒤에서 회수와 compaction을 하고, 할당하던 task를 멈추지 않습니다. 메모리 cgroup이 없는 kernel처럼 어떤 종류의 hook이 없으면 알림을 출력하고 나머지 종류를 셉니다.

edc의 집계를 kernel과 맞춰 보았습니다. 메모리 압박을 건 Linux 5.15 VM에서 tracefs는 시험 program의 `mm_vmscan_direct_reclaim_end`를 187번, `mm_compaction_end`를 166번 셌고, edc는 `direct` 187번과 `compact` 166번을 셌습니다. `/proc/vmstat`의 `allocstall`은 둘보다 많았습니다. 회수 한 번이 다시 시도하면 kernel이 `allocstall`을 또 세기 때문입니다. `memory.max` 16MiB 시험 cgroup에서 64MiB file을 세 번 읽자 `memcg` 정지가 584번 생겼고 182.5MiB를 회수했습니다. `memory.high` 16MiB에 swap이 없는 시험 cgroup에서는 40MiB를 쓰는 program이 한 번에 최대 1.28초 멈췄습니다.

kernel은 task가 처리할 일을 안고 사용자 공간으로 돌아갈 때마다 `memory.high` 처리 함수를 부릅니다. Linux 6.17, CPU 12개 host에서 1초에 약 2,600번이었습니다. edc는 task가 `memory.high`를 넘었을 때만 정지를 시작합니다. 두 BPF program은 호출마다 214ns와 267ns가 걸렸습니다. 나머지 hook은 task가 회수나 compaction을 할 때만 돕니다.

BTF, task local storage와 다음 hook 중 하나 이상이 필요합니다: `mm_vmscan_direct_reclaim`, `mm_vmscan_memcg_reclaim`, `mm_compaction` tracepoint, `mem_cgroup_handle_over_high` 함수.
Linux에서 `trace cpu`를 사용하면 CPU를 쓰는 함수를 찾을 수 있습니다. edc는 CPU마다 실행 중인 task의 stack을 기본 1초에 99번 샘플링합니다. 샘플은 kernel 안에서 세고, 끝날 때 요약을 한 번 출력합니다.

```bash
./bin/edc trace cpu --duration 30s
./bin/edc trace cpu --process api,1234 --duration 10s
./bin/edc trace cpu --container web --folded cpu.folded
./bin/edc trace cpu --frequency 499 --folded - | flamegraph.pl > cpu.svg
```

첫 줄은 샘플 수와 CPU가 바빴던 비율입니다. 쉬는 CPU에서는 idle task가 샘플되고, edc는 이 샘플을 `idle`로 셉니다. 바빴던 비율은 idle이 아닌 샘플을, 그 시간 동안 모든 CPU에서 frequency대로 받았어야 할 샘플 수로 나눈 값입니다. 어떤 VM은 쉬는 CPU에서 샘플을 덜 줍니다. CPU 4개인 GitHub runner에서 쉬는 CPU는 받아야 할 샘플의 약 57%를 받았고, 바쁜 CPU는 모두 받았습니다. `PROCESS` 표는 process별 샘플 수입니다. `FUNCTION` 표의 `SELF`는 그 함수가 가장 안쪽 frame이었던 샘플이고, `TOTAL`은 그 함수가 stack 어디에든 있던 샘플입니다. kernel 함수는 이름 뒤에 `[k]`가 붙습니다. 비율은 필터를 통과한 샘플에 대한 값입니다. 표는 함수 20개를 보여 주고, `--json`에는 모든 함수가 들어갑니다.

`--folded <path>`는 stack마다 바깥 frame부터 가장 안쪽 frame까지 한 줄로 쓰고, 끝에 샘플 수를 붙입니다. `flamegraph.pl`이나 speedscope 같은 flame graph 도구가 이 형식을 읽습니다. kernel frame은 `_[k]`로 끝납니다. `--folded -`로 stdout에 쓰면 표는 출력하지 않습니다. `--process`와 `--container`는 `trace syscall`과 같습니다. kernel 안에서 비교하므로 나머지 샘플은 map을 쓰지 않습니다.

kernel 함수 이름은 `/proc/kallsyms`에서 찾습니다. 사용자 함수 이름은 mapping된 file의 ELF 심볼표에서 찾습니다. Go program은 `.gopclntab`도 읽으므로 `-s -w`로 심볼표를 지운 Go program도 함수 이름이 나옵니다. JIT runtime은 그 process의 mount namespace 안 `/tmp/perf-<pid>.map`을 읽습니다. 예를 들어 `node --perf-basic-prof`가 이 file을 씁니다. file은 `/proc/<pid>/map_files`로 열므로 container 안의 file과 지워진 file도 읽습니다. 심볼이 없는 frame은 `[libc.so.6]`처럼 file 이름으로, 어느 mapping에도 없는 주소는 `[unknown]`으로 나옵니다. C++과 Rust 이름은 demangle하지 않습니다.

kernel은 frame pointer를 따라 사용자 stack을 읽습니다. frame pointer 없이 build한 program은 stack의 일부만 보이고, 끊긴 뒤의 주소는 `[unknown]`으로 나올 수 있습니다. 예를 들어 같은 C program을 `-O2 -fomit-frame-pointer`로 build하면 `_start;__libc_start_main;leaf_work`만 보였고, `-fno-omit-frame-pointer`로 build하면 `main;outer_work;leaf_work`가 보였습니다. 자기 frame을 만들지 않는 함수, 흔히 짧은 leaf 함수는 자신을 부른 함수를 가립니다. 이 예에서 `middle_work`가 빠진 이유입니다. `-O0`에서는 stack 전체가 보였습니다. Go program은 frame pointer를 유지합니다. 샘플의 절반이 넘게 사용자 stack이 frame 하나뿐이면 edc가 알림을 출력합니다.

몇 ms 안에 끝나는 process는 edc가 mapping을 읽기 전에 끝날 수 있고, 그러면 사용자 frame이 `[unknown]`으로 나옵니다. edc는 BPF program이 새 process를 처음 볼 때 mapping을 읽고, 20ms 뒤와 200ms 뒤에 다시 읽습니다.

Linux 6.17, CPU 12개 x86_64 host에서 비용을 측정했습니다. CPU를 쓰는 program을 CPU 12개에 모두 5초 동안 돌렸고, 조건마다 세 번 쟀습니다.

| 조건 | 처리한 일 | 변화 | 샘플당 BPF 시간 |
|---|---|---|---|
| trace 없음 | 2,649,850 | - | - |
| `--frequency 99` | 2,652,236 | +0.09% | 1.5 µs |
| `--frequency 999` | 2,639,584 | -0.39% | 1.1 µs |

99Hz에서 edc 자신은 샘플링하는 6초 동안 CPU 120ms를 썼습니다. 8초 trace 전체로는 약 1초를 썼고, 대부분은 시작과 끝의 심볼 읽기입니다. 메모리는 최대 131MiB였습니다.

BTF, online CPU마다 여는 `perf_event_open`의 cpu-clock event, perf event용 BPF link가 필요합니다. Linux 5.15에는 셋 다 있습니다.

`trace cpu --off-cpu`를 사용하면 thread가 도는 곳이 아니라 잠드는 곳을 찾을 수 있습니다. edc는 thread가 잠들 때 stack을 기록하고, 그 thread가 다시 돌 때 잠든 시간을 그 stack에 더합니다. scheduler가 선점한 thread는 runnable이므로 세지 않습니다.

```bash
./bin/edc trace cpu --off-cpu --process api --duration 10s
./bin/edc trace cpu --off-cpu --container db --folded db-offcpu.folded
```

`PROCESS` 표는 process별로 잠든 횟수와 시간을 보여 줍니다. `CODE`와 `KERNEL` 표는 그 시간이 어디서 쓰였는지 보여 줍니다. `CODE`는 사용자 stack의 안쪽 두 frame입니다. `KERNEL`은 `futex`나 `epoll_wait`처럼 잠든 system call이거나 `page_fault`입니다. 시간은 모든 thread의 잠든 시간을 더하므로 trace보다 길 수 있습니다. `--folded` 출력의 수는 잠든 시간의 마이크로초입니다. `--off-cpu`와 `--frequency`를 함께 주면 오류로 끝납니다.

trace 중에 시작하고 끝난 잠만 셉니다. trace 전에 시작했거나 끝날 때까지 깨지 않은 잠은 결과에 없습니다. 그래서 trace 내내 기다리는 thread는 보이지 않습니다.

같은 host에서 `perf bench sched pipe`로 비용을 쟀습니다. 한 번의 작업에 context switch가 두 번 있습니다.

| 조건 | 작업당 시간 | 늘어난 비용 |
|---|---|---|
| trace 없음 | 3.471 µs | - |
| `--off-cpu --process no-such-process` | 3.495 µs | 0.02 µs |
| `--off-cpu --process sched-pipe` | 4.379 µs | 0.91 µs |
| `--off-cpu`, 필터 없음 | 4.475 µs | 1.00 µs |

BPF program은 host의 모든 context switch에서 돕니다. 필터가 고르지 않은 process에는 191ns, 고른 process에는 1.25~1.38µs가 걸렸고, 대부분은 두 stack을 읽는 시간입니다. 바쁜 host에서는 `--process`나 `--container`를 쓰세요.

필터가 없으면 stack map의 hash 칸 하나를 두 stack이 쓰려 할 수 있습니다. host 전체를 8초 기록하자 잠 25,402번 중 180번이 이렇게 stack을 잃었습니다. edc는 이 수를 `collisions`로 세고 알림을 출력합니다. `--process`를 주자 같은 시험에서 충돌이 없었습니다.

off-CPU trace에는 `sched_switch` tracepoint와 BTF, task local storage도 필요합니다.

kernel이 모든 버림을 이유와 함께 알리지는 않습니다. 예를 들어 listen socket의 accept 대기열이 가득 차면 kernel은 SYN을 정상 패킷처럼 해제합니다. 이 경우를 위해 요약에는 trace 동안 `/proc/net/netstat`의 `ListenOverflows`와 `ListenDrops`가 늘어난 수가 나옵니다. 이 값은 edc가 있는 network namespace의 모든 listen socket을 합한 것이라, `--container`를 써도 container만의 값이 아닙니다.

```bash
./bin/edc capture \
  --interface en0 \
  --duration 15s \
  --count 500 \
  --filter 'host 203.0.113.10 and port 443' \
  --output incident.pcap
```

PCAP에는 credential과 개인정보가 포함될 수 있습니다. JSON redaction은 PCAP payload에 적용되지 않습니다.

`edc capture`는 시작 전에 계획을 보여 줍니다. 계획에는 interface, duration, packet 상한, filter, 출력 경로, `edc`가 쓰는 권한이 들어갑니다. 좌우 방향키와 Enter로 답하거나 `y`, `n`으로 답합니다. 계획은 `tcpdump` 출력 위에 그대로 남습니다.

질문을 건너뛰려면 `--yes`를 씁니다. terminal이 아닌 실행은 계획을 출력하고 stdin에서 `y`나 `n`을 읽습니다.

## Cron과 애플리케이션 로그

`edc log`는 별도 설정 없이 stdout과 stderr를 모두 기록합니다. 실행마다 다른 파일을 쓰므로 독립적인 작업이 서로 기다리지 않습니다.

```cron
* * * * * /usr/local/bin/edc log -- /usr/local/bin/job --daily
```

Linux는 `${XDG_STATE_HOME:-~/.local/state}/edc/log/<command>/`, macOS는 `~/Library/Logs/edc/<command>/` 아래에 저장합니다. 파일 이름은 UTC 시각, wrapper PID, 고유 접미사로 구성하며 터미널에서 실행하면 생성한 경로를 안내합니다.

각 실행 시도에는 명령, 작업 디렉터리, 프로세스 PID, 시작·종료 시각, 실행 시간, 종료 상태를 기록합니다. 명령 시작에 실패하면 원인도 파일에 남기므로, 에러 출력 없이 비정상 종료한 작업도 종료 기록으로 확인할 수 있습니다.

기본 `--command-display full`은 인자 전체를 기록합니다. 인자에 인증 정보가 있으면 `name`이나 `none`을 사용하십시오.

`--output`을 지정하면 해당 파일에 이어 쓰며, `--stream stdout` 또는 `stderr`로 기록할 출력을 한쪽으로 제한할 수 있습니다. 선택하지 않은 출력과 stdin은 호출한 환경에 그대로 연결합니다. 같은 저장 파일을 지정한 실행은 `.lock` 파일로 순서대로 실행하므로 회전 중에도 잠금이 유지됩니다.

새 로그 파일은 mode `0600`, 자동 생성한 명령 디렉터리는 `0700`입니다. 기존 로그 파일의 권한은 유지합니다.

### 명령 실행 이력

`edc log history`는 자동 저장된 로그의 명령과 전체 인자를 키로 묶어 나열합니다. 기본 영문 화면의 목록은 명령·최근 실행·실행 수·실패 수를 정렬해서 보여 줍니다. 불명 건수가 있을 때만 해당 열을 추가하며 hash와 반복 설명은 목록에서 숨깁니다. 터미널에서는 ↑/↓로 키를 선택하고 Enter로 해당 명령의 실행 이력을 엽니다. 이력 화면에서는 실행을 선택한 뒤 Enter로 작업 디렉터리·전체 키·원본 경로를 확인합니다. Esc 또는 b는 이전 화면으로 돌아가며 q는 종료합니다. 선택 행은 청록색, 성공은 초록, 실패는 빨강, 불명과 안내는 노랑으로 표시합니다. 파이프이거나 `NO_COLOR`가 설정되면 키 목록을 정적으로 출력합니다.

```bash
edc log -- ls -l /tmp
edc log history
edc log history ls
edc log history --command ls
edc log history ls -l /tmp
edc log history --failed ls -l /tmp
edc log history --key <key>
edc log history --file /tmp/job.log
edc log history --dir /path/to/logs
```

`history ls`는 인자 없는 `ls`의 이력을 바로 출력합니다. `history ls -l /tmp`는 정확히 같은 argv의 이력을 조회합니다. `history --command ls`는 `ls`의 인자 조합별 키 목록을 보여 줍니다. 첫 명령 토큰 전까지 history 옵션을 해석하고, 이후 `--failed`나 `--`를 포함한 모든 토큰은 argv로 보존합니다. 명령 앞의 선택적 `--`는 옵션 해석만 끝내며 조회 의미를 바꾸지 않습니다. `--command`, `--key`, 직접 argv 조회는 함께 지정할 수 없습니다. 조회는 명령을 다시 실행하지 않습니다. 인자 순서와 경계를 보존하므로 `echo "a b"`와 `echo a b`는 다른 키입니다. 동일 argv를 다른 디렉터리에서 실행한 경우 키는 같지만 통계는 작업 디렉터리별로 구분합니다.

실행별 시작 시각, 소요 시간, 성공·실패·시간 초과·시그널 종료·오류·완료 불명, exit code와 재시도 번호를 표시합니다. 기본 최근 20건이며 `--limit 1`부터 `--limit 1000`까지 지정할 수 있습니다. 실행 소요 시간에는 출력 배출과 프로세스 정리가 포함됩니다. 요약은 표시한 실행 기준이며 성공한 실행만 평균·최소·최대 시간에 포함합니다. 종료 기록이 없는 항목은 실패로 단정하지 않으며 `--failed`에서도 제외합니다.

기본 로그 디렉터리와 하위 한 단계의 `.log` 파일, 설정한 `defaults.log.output`을 조회하며 회전 조각도 함께 읽습니다. 임의 위치에 저장한 로그는 `--file` 또는 `--dir`로 지정합니다. 조회는 최대 1,000개 파일 묶음, 10,000개 디렉터리 항목, 128 MiB, 10,000개 실행 시도로 제한합니다. 한도 초과·읽는 동안 변경·손상된 기록은 안내와 함께 부분 결과를 반환하고 exit code `2`를 돌려줍니다. 과거 실패가 있어도 정상적인 조회는 `0`입니다.

새 로그는 전체 argv의 SHA-256 키와 표시 방식을 기록합니다. `command-display=name`은 인자를 숨기지만 키로 구분할 수 있으며, `none`은 키도 기록하지 않습니다. 기존 로그는 전체 argv가 확실한 경우 키를 재구성합니다. 기존 단일 basename 기록은 인자 없는 실행인지 name 모드인지 구별할 수 없어 키 목록에서 제외하고 건수를 안내합니다. 숨겨진 인자는 복원하지 않습니다.

이력은 텍스트 로그의 메타데이터를 읽은 결과입니다. 자식 출력이 동일한 메타데이터 형식을 흉내 낸 경우의 진위까지 보장하지는 않습니다.

### 로그 회전

파일당 기본 최대 크기는 10 MiB이며 `--max-size`에 MiB 단위 정수를 지정합니다. `0`은 회전을 끕니다. 기본으로 회전 파일 3개를 보관하며 `--keep-files`는 1-100을 받습니다.

회전 파일은 `<output>.edc.1`부터 `<output>.edc.N`까지이며, 이 이름은 해당 로그의 회전용으로 예약합니다. 가장 오래된 회전 파일만 정리하고 다른 실행의 로그는 삭제하거나 압축하지 않습니다. 실행 횟수에 따른 전체 보관량은 별도로 관리해야 합니다.

저장 경로에는 일반 파일을 사용해야 합니다. 로그나 회전 파일 경로가 symlink이거나 일반 파일이 아니면 거부합니다. 기록에 실패하면 자식 프로세스 그룹을 종료하고 종료 코드 `2`를 반환합니다.

### 재시작과 timeout

재시작은 기본으로 꺼져 있습니다. `--restart on-failure`는 비정상 종료, 자식의 signal 종료, timeout을 재시도합니다. `always`는 정상 종료도 다시 실행하며, `never`는 재시작하지 않습니다. 명령 시작 실패와 로그 기록 오류는 재시도하지 않습니다. 재시작 대상인 종료에서는 남아 있는 그룹 구성원을 종료한 뒤 다음 시도를 시작하거나 wrapper를 종료합니다.

기본 최대 재시작 횟수는 3회, 대기 시간은 5초입니다. `--max-restarts`와 `--restart-delay`로 바꿀 수 있습니다. 외부 SIGINT·SIGTERM을 받으면 실행 중인 작업이나 재시작 대기를 중단하고 다시 실행하지 않습니다.

시간 제한은 기본으로 없습니다. `--timeout`은 출력 수집까지 포함해 시도별 시간을 제한하며, 공통 timeout 기본값은 적용하지 않습니다. 제한 시간이 지나면 자식 프로세스 그룹에 SIGTERM을 보내고, `--kill-after`의 유예 시간(기본 5초)이 지나면 SIGKILL을 보냅니다.

시간 초과는 `status=timeout exit=124`로 기록합니다. 이후 재시도가 성공하면 `0`, 그렇지 않으면 마지막 시도의 종료 코드를 반환합니다.

```bash
edc log --timeout 10m --restart on-failure --max-restarts 3 -- /path/to/job
edc log --max-size 20 --keep-files 5 --output /tmp/job.log -- /path/to/job
```

새 옵션은 `[defaults.log]`에도 저장할 수 있으며 명시한 CLI 옵션이 우선합니다. wrapper 자체가 SIGKILL로 종료되면 종료 기록을 남길 수 없습니다. 별도 프로세스 그룹으로 이동한 자손 프로세스는 그룹 종료 범위에 포함되지 않습니다.

## Shell completion

`edc completion`은 completion script를 출력합니다. script는 command, option, 그리고 `edc`가 찾은 inventory의 group 이름을 완성합니다.

```bash
source <(edc completion zsh)
source <(edc completion bash)
```

zsh에서는 script를 `fpath`의 디렉터리에 `_edc`라는 이름으로 저장해도 됩니다.

`edc completion groups`는 inventory의 group 이름을 한 줄에 하나씩 출력합니다. script가 이 command를 호출합니다.

## Exit code

- `0`: 성공 또는 warning만 존재
- `1`: 하나 이상의 probe 실패, 또는 `report diff`에서 악화된 probe 존재
- `2`: argument, config, log 시작, report parse 등 실행 오류
- `3`: privileged 작업의 권한 부족
- `4`: 사용자 취소 (선택 취소, `remote`와 `doctor`의 Ctrl-C 포함)

## 현재 범위

`top`, `info`, `doctor`와 개별 network probe는 Linux와 macOS를 지원합니다. Linux에서는 `/proc`, `/sys`, `ip`, `ss`, `ping`, `traceroute` 또는 `tracepath`, `/etc/resolv.conf`를 읽고, `resolvectl`이 있으면 `resolvectl status`를 evidence로 덧붙입니다. macOS에서는 system command adapter를 사용합니다. `capture`는 Linux와 macOS를 지원하고 `quality`는 macOS에서 `networkQuality`를, Linux에서 내장 응답성 측정을 실행하며 둘 다 측정한 경우에 `download_bps`, `upload_bps`, `responsiveness_rpm`, `base_rtt_ms`를 남기고, 측정하지 못한 값은 뺍니다. config URL 기본값은 Apple의 `https://mensura.cdn-apple.com/api/v1/gm/config`이고, `--server`나 `defaults.quality.server`로 바꿉니다. `server`를 비우면 기본값을 씁니다. 진단 command는 read-only 관측에 집중하며, DNS flush, interface reset, firewall 변경 같은 자동 복구는 하지 않습니다. `edc log`는 로그 파일, 회전 파일, 잠금 파일을 씁니다. `edc top --write`는 SQLite DB와 WAL 파일을 씁니다. `edc ai`는 `ai-resets.jsonl`과 `ai-claude.json`을 쓰고 Claude 사용량 API를 부릅니다.

## 라이선스

MIT입니다. [LICENSE](LICENSE)를 보십시오.
