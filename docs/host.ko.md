[English](host.md) | **한국어** · [README로 돌아가기](../README.ko.md)

# 호스트 상태와 점검

[호스트 정보](#호스트-정보) · [호스트 점검 (Linux)](#호스트-점검-linux) · [서비스 보기 (Linux)](#서비스-보기-linux) · [Container 보기 (Linux)](#container-보기-linux) · [최근 호스트 변경 (Linux)](#최근-호스트-변경-linux) · [커널과 systemd 이벤트 (Linux)](#커널과-systemd-이벤트-linux) · [현재 범위](#현재-범위) · [Listening port](#listening-port)

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


## 현재 범위

`top`, `info`, `doctor`와 개별 network probe는 Linux와 macOS를 지원합니다. Linux에서는 `/proc`, `/sys`, `ip`, `ss`, `ping`, `traceroute` 또는 `tracepath`, `/etc/resolv.conf`를 읽고, `resolvectl`이 있으면 `resolvectl status`를 evidence로 덧붙입니다. macOS에서는 system command adapter를 사용합니다. `capture`는 Linux와 macOS를 지원하고 `quality`는 macOS에서 `networkQuality`를, Linux에서 내장 응답성 측정을 실행하며 둘 다 측정한 경우에 `download_bps`, `upload_bps`, `responsiveness_rpm`, `base_rtt_ms`를 남기고, 측정하지 못한 값은 뺍니다. config URL 기본값은 Apple의 `https://mensura.cdn-apple.com/api/v1/gm/config`이고, `--server`나 `defaults.quality.server`로 바꿉니다. `server`를 비우면 기본값을 씁니다. 진단 command는 read-only 관측에 집중하며, DNS flush, interface reset, firewall 변경 같은 자동 복구는 하지 않습니다. `edc log`는 로그 파일, 회전 파일, 잠금 파일을 씁니다. `edc top --write`는 SQLite DB와 WAL 파일을 씁니다. `edc ai`는 `ai-resets.jsonl`과 `ai-claude.json`을 쓰고 Claude 사용량 API를 부릅니다.


## Listening port

`listen`으로 호스트 개요와 별도로 열린 port를 확인합니다. unix socket을 포함하려면 `--unix`나 `--all`을, 반복 관측하려면 `--watch`를 사용합니다.

```bash
edc listen
edc listen --watch -i 0.5 --duration 10s
```
