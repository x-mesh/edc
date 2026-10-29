[English](host.md) | **한국어** · [README로 돌아가기](../README.ko.md)

# 호스트 상태와 점검

[호스트 정보](#호스트-정보) · [호스트 점검 (Linux)](#호스트-점검-linux) · [서비스 보기 (Linux)](#서비스-보기-linux) · [Container 보기 (Linux와 macOS)](#container-보기-linux와-macos) · [최근 호스트 변경 (Linux)](#최근-호스트-변경-linux) · [커널과 systemd 이벤트 (Linux)](#커널과-systemd-이벤트-linux) · [현재 범위](#현재-범위) · [Listening port](#listening-port)

## 호스트 정보

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

## 상태 점검 (Linux와 macOS)

`edc check`는 Linux나 macOS 호스트에서 지금 고장 난 곳을 찾습니다. service, process, 메모리, 디스크, 커널 로그, 시간, 네트워크, container, 파일 핸들, 재부팅 필요 여부를 읽습니다. 기본 점검은 아무것도 바꾸지 않고 외부로 보내지 않습니다. [루틴](#점검-루틴)은 설정 파일에 적은 셸 명령도 실행합니다. 문제마다 이어서 볼 edc 명령을 알려 줍니다.

```bash
edc check
edc check --json -                  # 점검마다 결과 하나
edc check --group daily             # edc remote 그룹의 호스트마다 edc check 실행
```

| 점검 | fail | warn |
|---|---|---|
| `service` | unit이 실패했거나 `require_units`의 unit이 inactive이거나 없음 | unit의 상태가 바뀌는 중이거나 `auto-restart`에 머묾 |
| `process` | | defunct process나 D 상태 process가 있음 |
| `memory` | 가용 메모리가 5% 미만 | 가용 메모리가 10% 미만 |
| `disk` | mount의 사용률이나 inode 사용률이 95% 이상 | 같은 값이 90% 이상이거나 mount가 읽기 전용 |
| `kernel` | 최근 1시간에 OOM kill, 디스크 I/O 오류, 읽기 전용 전환, lockup, oops가 있음 | 최근 1시간에 segfault, hung task, 커널 WARNING이 있음 |
| `time` | | 시계가 동기화되지 않음 |
| `network` | default route가 없거나 conntrack·TCP 메모리 사용률이 90% 이상 | `/etc/resolv.conf`나 nameserver가 없거나 같은 사용률이 80% 이상 |
| `container` | restart policy가 있는 container가 0이 아닌 코드나 메모리 부족으로 멈춤 | restart policy가 없는 container가 최근 24시간 안에 오류로 멈췄거나, container가 재시작·일시 정지 중이거나 unhealthy |
| `filehandles` | `fs.file-max`의 90% 이상을 사용 | `fs.file-max`의 80% 이상을 사용 |
| `reboot` | | 패키지가 재부팅을 요구하거나 `/boot`에 실행 중인 커널보다 새 커널이 있음 |

fail이 하나라도 있으면 종료 코드가 1입니다. NIC drop처럼 부팅 뒤 늘어나기만 하는 카운터는 판정하지 않습니다. 한 번 읽어서는 언제 늘었는지 알 수 없기 때문입니다.

macOS에서는 다음 원천을 읽습니다.

| 점검 | macOS 원천 | Linux와 다른 점 |
|---|---|---|
| `service` | `launchctl list` | 실행 중이 아니고 양수 종료 코드로 끝난 job을 실패로 봅니다. 쉬고 있는 agent의 `-9`처럼 signal로 끝난 job은 실패가 아닙니다. 사용자로 실행하면 자기 세션의 agent를, `sudo`로 실행하면 system daemon을 봅니다. `next`는 `launchctl list <label>`입니다. |
| `process` | `ps` | defunct process만 셉니다. macOS에는 Linux와 같은 뜻의 D 상태가 없습니다. |
| `memory` | `kern.memorystatus_vm_pressure_level` | pressure 단계로 판정합니다. `warn`이면 warn, `critical`이면 fail입니다. macOS는 원래 가용 메모리가 적게 보입니다. |
| `disk` | `df`와 `statfs` | 읽기 전용 mount는 판정하지 않습니다. 서명된 system volume이나, mount한 설치 파일처럼 원래 가득 찬 disk image이기 때문입니다. |
| `network` | `route -n get default`, `/etc/resolv.conf` | conntrack과 TCP 메모리는 보지 않습니다. |
| `kernel` | `/Library/Logs/DiagnosticReports`, `~/Library/Logs/DiagnosticReports` | 최근 1시간에 쓰인 보고서를 셉니다. kernel panic과 `JetsamEvent`(메모리가 모자라 커널이 process를 끝냄)는 fail, 앱 crash와 hang 보고서는 warn입니다. CPU·wakeup 보고서는 세지 않습니다. |
| `container`, `filehandles` | Docker socket, `kern.num_files`와 `kern.maxfiles` | Linux와 같습니다. |
| `thermal` | `notifyutil -g com.apple.system.thermalpressurelevel` | macOS에만 있습니다. `heavy`는 warn, `trapping`과 `sleeping`은 fail입니다. `moderate`는 부하가 걸리면 흔히 나오는 단계라 경고하지 않습니다. |
| `backup` | `tmutil destinationinfo`, `tmutil latestbackup` | macOS에만 있습니다. 마지막 Time Machine 백업이 7일보다 오래되면 warn입니다. 백업 대상이 없으면 `skip`입니다. |

macOS에서 `time`과 `reboot`은 `skip`입니다. Linux 결과에는 `thermal`과 `backup`이 없습니다.

`--group`은 `edc remote`의 inventory와 OpenSSH 설정을 씁니다. 각 호스트에 `check`가 있는 edc가 있어야 합니다. 표에는 호스트마다 한 줄이 나옵니다. 결과를 받지 못한 호스트는 SSH 실패나 `check`가 없는 edc처럼 그 이유를 보여 줍니다.

이미 확인한 결과를 받아들이려면 `config.toml`에 `[check]` 표를 넣으세요.

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

`accept`에는 위 표의 점검 이름을 씁니다. 받아들인 점검은 `skip`으로 나오고 요약 앞에 `받아들임 (warn)` 같은 표시가 붙습니다. JSON의 `metrics.accepted_status`에는 원래 상태가 남습니다.

`accept_units`, `accept_containers`, `accept_mounts`에는 glob 패턴을 씁니다. 패턴에 맞는 이름은 판정하지 않습니다. 대신 요약 끝에 받아들인 개수가 붙고, `metrics.accepted`에 이름이 남습니다.

`[check.limits]`의 값은 모두 `[warn, fail]` 백분율입니다. `usage`는 conntrack, TCP 메모리, 파일 핸들에 적용됩니다. `memory_available`은 남은 비율이므로 warn 값이 fail 값보다 크거나 같아야 합니다.

`--group`으로 실행하면 각 호스트가 자기 설정 파일을 읽습니다. `[check]` 표는 결과가 나온 호스트에 넣으세요.

### 점검 루틴

루틴은 `config.toml`에 이름을 붙여 둔 규칙 묶음입니다. `edc check <루틴>`으로 실행하고, 그룹의 호스트마다 실행하려면 `edc check --group <이름> --routine <루틴>`을 씁니다.

```toml
[check.routines.web]
checks = ["service", "disk", "kernel", "network"]
require_units = ["nginx.service", "app-api.service"]

[[check.routines.web.commands]]
name = "app-health"
run = "curl -fsS http://localhost:8080/health"
timeout = "5s"
level = "fail"
```

`checks`에 적은 점검만 결과에 나옵니다. 비워 두면 모든 점검이 나옵니다. accept 목록과 `require_units`는 `[check]`의 값에 더해집니다. 루틴의 `limits` 표에 적은 값은 `[check.limits]`의 같은 값을 대신합니다.

명령은 `sh -c`로 실행하고, 결과 이름은 `check.cmd.<name>`입니다. 종료 코드가 0이면 `pass`이고, 다른 코드면 `fail`입니다. `level = "warn"`이면 `warn`이 됩니다. `timeout`(기본 10s) 안에 끝나지 않으면 `fail`이고, edc가 명령과 그 자식 process를 멈춥니다. 결과에는 출력의 마지막 10줄이 4 KiB까지 남습니다. 명령의 결과를 받아들이려면 `accept`에 `cmd.<name>`을 넣으세요.

다른 사용자가 설정 파일을 바꿀 수 없을 때만 명령을 실행합니다. 읽은 설정 파일과 그 상위 디렉터리를 모두 검사해서, 그중 하나라도 그룹이나 다른 사용자가 쓸 수 있거나 주인이 다른 사용자이면 명령마다 `skip`으로 나옵니다. `/tmp`처럼 sticky bit가 있는 디렉터리는 다른 사용자가 쓸 수 있어도 괜찮습니다. `sudo edc check`는 root의 설정 파일을 읽으므로, 이 규칙이 다른 사용자가 root 권한으로 명령을 실행하는 것을 막습니다.

루틴과 명령의 이름에는 `a-z`, `0-9`, `_`, `-`만 씁니다.

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

<a id="container-보기-linux"></a>
<a id="container-보기-linux와-macos"></a>

## Container 보기 (Linux와 macOS)

`edc ctr <container>`는 Unix socket으로 Docker Engine API를 읽어 container 하나를 보여 줍니다. Docker CLI를 실행하거나 container 상태를 바꾸지 않습니다.

```bash
edc ctr web
edc ctr -v 0123456789ab
edc ctr --json - web
edc ctr --collector-image edc-collector:amd64 web
```

다른 socket을 쓰려면 `DOCKER_HOST`에 절대 경로의 Unix socket URL을 지정합니다. `DOCKER_CONTEXT=default`는 `DOCKER_HOST`를 무시합니다. edc는 Docker context나 credential 파일을 읽지 않습니다. default가 아닌 context와 TCP·SSH·TLS endpoint는 오류를 냅니다. Linux는 `/var/run/docker.sock`을 씁니다. macOS는 `~/.docker/run/docker.sock`이 있으면 그 경로를 쓰고, 없으면 `/var/run/docker.sock`을 씁니다.

기본 상세 조회는 읽기 전용 GET 요청만 보냅니다. Docker API 1.41부터 1.51까지 협상합니다. 개별 요청은 5초로 제한하고, 기본 전체 제한은 15초입니다.

| 결과 | 기본 API 조회 | collector opt-in |
|---|---|---|
| `ctr.state` | 상태, 재시작, health, 종료 코드 | 기존 결과를 유지합니다. |
| `ctr.image` | 현재 이미지 참조와 image ID | 기존 결과를 유지합니다. |
| `ctr.processes` | Docker top API의 process, CPU, RSS | 기존 결과를 유지합니다. daemon이 요청한 열을 거부하면 오류를 표시합니다. |
| `ctr.limits` | API 필드가 있으면 memory, 한도, CPU throttle, pids | container cgroup 수명 동안의 OOM·oom_kill 누적 횟수 |
| `ctr.fd` | 제공하지 않음 | 대상 cgroup process별 FD 수와 soft limit |
| `ctr.listen` | publish한 port 설정 | 대상 process FD가 소유한 TCP·UDP listener의 주소와 port |
| `ctr.log` | 마지막 50줄과 그중 마지막 오류 5줄 | 기존 결과를 유지합니다. |
| `ctr.disk` | writable layer 크기와 log rotation 설정 | 허용된 log 파일의 크기 |

기본 조회는 Docker Engine API 정보만 사용합니다. `--collector-image <local-image>`를 상세 요청에 주면 추가 Linux 지표를 수집합니다. 선택한 Docker daemon에 이미지가 있어야 합니다. edc는 immutable image ID, Linux platform, daemon architecture, protocol label을 확인합니다. 이미지를 자동으로 pull·build·publish하지 않습니다. TUI 목록 갱신은 helper를 만들지 않습니다. `Enter`로 상세를 열거나 상세에서 `r`로 다시 읽을 때 실행합니다.

collector는 daemon의 Linux host에서 실행됩니다. macOS client도 Unix socket을 통해 Linux daemon VM을 조회할 수 있습니다. daemon은 rootful Linux와 cgroup v2를 사용해야 합니다. helper는 UID 0으로 실행하고 모든 capability를 제거한 뒤 `SYS_PTRACE`, `DAC_READ_SEARCH`만 추가합니다. host PID·cgroup namespace, `network none`, read-only root filesystem을 사용하고 Docker log driver를 `none`으로 설정합니다. `SYS_ADMIN`, privileged mode, Docker socket mount, host root mount는 사용하지 않습니다.

helper는 `/sys/fs/cgroup`을 read-only로 mount합니다. log driver가 `json-file` 또는 `local`이고 경로 검사를 통과하면 해당 container의 log 파일 하나만 read-only로 mount합니다. helper는 `fstat`으로 파일 metadata만 읽고 log 내용은 읽지 않습니다. rootless daemon, user namespace mode, cgroup v1은 지원하지 않습니다. LSM이나 Enhanced Container Isolation 정책은 권한을 거부할 수 있습니다. edc는 권한을 넓혀 재시도하지 않습니다.

측정 JSON은 attach stream으로만 전달합니다. metric을 DB, 파일, Docker log에 저장하지 않습니다. Docker Engine에는 helper image와 임시 container metadata·event가 남을 수 있습니다. 정리가 성공하면 helper container를 제거합니다. 정리에 실패하면 report에 helper 이름과 ID를 표시합니다. 직접 제거하기 전에 그 ID를 확인하세요.

각 collector metric은 상태를 담고, 수집이 불완전하거나 불가능하면 사유도 담습니다. collector가 실패해도 기존 API 결과는 유지합니다. PID가 0인 container는 FD·listener·OOM을 읽을 수 없지만, 경로가 허용된 log 파일 크기는 읽을 수 있습니다. IPv6 wildcard의 coverage나 accept queue 한도는 추정하지 않습니다.

repository root에서 daemon architecture에 맞는 helper binary를 만듭니다.

```bash
make collector-build COLLECTOR_ARCH=amd64
make collector-build COLLECTOR_ARCH=arm64
```

이 target은 local binary만 만듭니다. image는 자동으로 build·pull·publish하지 않습니다. `collector.Dockerfile`은 `scratch`와 선택한 binary만 사용합니다. 아래 명령은 Docker build context에 Dockerfile과 binary만 넣습니다. `.env`와 credential 파일은 context에 들어가지 않습니다.

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

arm64 daemon이면 `arch=arm64`로 바꾸세요. daemon과 architecture가 같은 image tag를 상세 요청에 지정하세요.

```bash
edc ctr --collector-image edc-collector:amd64 web
edc ctr --collector-image edc-collector:amd64 --json - web
```

최소 capability와 mount의 실제 동작은 Docker daemon에서 검증하지 않았습니다. live 검증 전 사용자 확인을 받으세요. Unix socket, local image ID와 protocol label, 대상 container ID, capability, 정확한 mount source, 고유 helper 이름과 cleanup 대상을 먼저 제시하세요.

누락된 API 필드의 이유를 표시하고 값을 0으로 바꾸지 않습니다. 일부 한도 정보만 있어도 그 값을 표시합니다. 자원 한도 필드가 모두 없으면 `ctr.limits`는 `skip`이고, 수집한 OOM 값은 계속 표시합니다.

목록은 실패한 container, 주의가 필요한 container, 재시작 기록이 있는 container 순서로 표시합니다. 코드 0으로 끝난 container는 `--all`을 줄 때만 나옵니다.

```bash
edc ctr
edc ctr --all
edc ctr --json -
```

목록은 version 요청 1회와 목록 요청 1회를 보냅니다. 표시 대상마다 inspect를 1회, 실행 중 container마다 stats를 2회 요청합니다. 각 stats 단계는 최대 4개 요청을 병렬 처리합니다. CPU를 계산하려고 두 단계 사이에 전체에서 한 번 1초 기다립니다. 로그, process, writable layer 크기는 조회하지 않습니다. `--collector-image`가 있어도 목록이나 refresh는 helper를 만들지 않습니다.

목록에는 CPU%, raw 메모리 사용량과 설정 한도, 메모리 비율, 현재 PIDs가 표시됩니다. 알 수 없는 값은 `n/a`, 중지된 container는 `—`, 설정상 한도가 없는 메모리는 `∞`로 표시됩니다. 메모리 사용량은 Docker의 `memory_stats.usage` 원본이며 cache를 빼지 않습니다. 터미널 폭이 넓어지면 남은 공간에 따라 NET I/O, BLOCK I/O, 재시작 횟수, 가동 시간, image 열을 개별 추가합니다. 터미널 화면의 키는 `edc svc`와 같습니다. `Enter`로 container를 열고 `Esc`로 돌아옵니다.

pipe나 `--json`에서는 `ctr.list` 결과 하나와 표를 출력합니다. 실패한 container가 있으면 결과는 `fail`, exit code는 1입니다. 재시작 중, 일시 중지, unhealthy, 생성만 된 container가 있으면 `warn`입니다. socket 권한 오류는 `skip`입니다.

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


### macOS

macOS에서 `edc events`는 `/Library/Logs/DiagnosticReports`와 그 아래 `Retired` 폴더의 jetsam·패닉 보고서를 읽습니다. 이 보고서는 ReportCrash가 남깁니다. 보고서는 파일 이름이 아니라 첫 줄의 `bug_type`으로 고릅니다.

| 결과 | Fail | Warn |
|---|---|---|
| `events.window` | | |
| `events.panic` | 커널 패닉이 있었음 | |
| `events.jetsam` | | jetsam이 process를 종료함. 근거에 이유, 상주 메모리, 가장 큰 process를 보입니다 |

이유가 `per-process-limit`인 종료는 process 하나가 자기 메모리 한도를 넘은 것으로, macOS에서 흔하므로 warn으로 둡니다. 관리자 계정이면 보고서를 읽을 수 있고, 폴더를 읽지 못하면 `events.window`가 `skip`입니다. unified log에는 커널의 `memorystatus` 줄이 남지 않아 읽지 않습니다.

## 현재 범위

`top`, `info`, `doctor`와 개별 network probe는 Linux와 macOS를 지원합니다. Linux에서는 `/proc`, `/sys`, `ip`, `ss`, `ping`, `traceroute` 또는 `tracepath`, `/etc/resolv.conf`를 읽고, `resolvectl`이 있으면 `resolvectl status`를 evidence로 덧붙입니다. macOS에서는 system command adapter를 사용합니다. `capture`는 Linux와 macOS를 지원하고 `quality`는 macOS에서 `networkQuality`를, Linux에서 내장 응답성 측정을 실행하며 둘 다 측정한 경우에 `download_bps`, `upload_bps`, `responsiveness_rpm`, `base_rtt_ms`를 남기고, 측정하지 못한 값은 뺍니다. config URL 기본값은 Apple의 `https://mensura.cdn-apple.com/api/v1/gm/config`이고, `--server`나 `defaults.quality.server`로 바꿉니다. `server`를 비우면 기본값을 씁니다. 진단 command는 read-only 관측에 집중하며, DNS flush, interface reset, firewall 변경 같은 자동 복구는 하지 않습니다. `edc log`는 로그 파일, 회전 파일, 잠금 파일을 씁니다. `edc top --write`는 SQLite DB와 WAL 파일을 씁니다. `edc ai`는 `ai-resets.jsonl`과 `ai-claude.json`을 쓰고 Claude 사용량 API를 부릅니다.


## Listening port

`listen`으로 호스트 개요와 별도로 열린 port를 확인합니다. unix socket을 포함하려면 `--unix`나 `--all`을, 반복 관측하려면 `--watch`를 사용합니다.

```bash
edc listen
edc listen --full
edc listen --watch -i 0.5 --duration 10s
```

`--full`은 행마다 아래에 프로세스의 작업 디렉터리와 명령줄을 회색 줄로 붙입니다. 같은 `node`나 `python` 서버 여럿을 이 줄로 구분할 수 있습니다. `node`, `python`, `ruby`, `perl`, `php`는 스크립트 이름을 앞에 둡니다. 터미널에서는 화면 폭에 맞춰 줄을 자르므로 명령줄의 끝이 먼저 잘립니다. `--password`, `--token` 같은 옵션의 값과 URL의 암호는 `***`로 가립니다. `edc listen --json`은 읽을 수 있는 프로세스의 `args`와 `cwd`를 담습니다. root가 아니면 macOS에서는 다른 계정의 프로세스를, Linux에서는 다른 계정의 작업 디렉터리를 읽을 수 없습니다. `--full`은 `--watch`와 함께 쓸 수 없습니다.
