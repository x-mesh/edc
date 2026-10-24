[English](tracing.md) | **한국어** · [README로 돌아가기](../README.ko.md)

# 동작 추적과 패킷 캡처

요청, 연결, 프로세스 동작과 커널 대기를 관측합니다. trace마다 지원 OS와 권한이 다릅니다. HTTP trace는 Linux 5.15 이상과 필요한 eBPF 권한이 있어야 합니다. 각 절에서 실행 조건과 측정 한계를 확인하세요.

![데모 HTTP 요청·응답 목록과 message 미리 보기](media/trace-http.png)

현재 UI 렌더러에 데모 event를 넣어 만든 화면입니다. `i`로 화면을 나누어 message 미리 보기를 연 상태입니다. [캡처 재현 방법](capture/README.md)을 참고하세요.

[패킷 캡처](#패킷-캡처) · [trace tcp / udp](#trace-tcp--udp) · [trace dns](#trace-dns) · [trace arp](#trace-arp) · [trace http](#trace-http) · [trace mysql](#trace-mysql) · [trace clickhouse](#trace-clickhouse) · [trace postgres](#trace-postgres) · [trace redis](#trace-redis) · [trace socket](#trace-socket) · [trace priv](#trace-priv) · [trace drop](#trace-drop) · [trace exec](#trace-exec) · [trace lock](#trace-lock) · [trace syscall](#trace-syscall) · [trace reclaim](#trace-reclaim) · [trace cpu](#trace-cpu) · [PCAP 파일](#pcap-파일) · [trace io](#trace-io) · [trace sched](#trace-sched) · [trace show](#trace-show)

## 패킷 캡처

`capture`는 패킷 수집에 필요한 권한을 사용합니다. `capture`는 Linux와 macOS를 지원합니다. Capture에는 강제 상한(duration 60초, packet 10,000개)이 있고 기존 파일을 덮어쓰지 않습니다. `tcpdump`는 `/usr/sbin`, `/usr/bin`, `/sbin`, `/bin` 중 한 곳에 설치되어 있어야 합니다. `capture`는 `tcpdump`와 `sudo`를 `PATH`에서 찾지 않습니다.

Linux에서 `--mode events`를 사용하면 process metadata와 함께 TCP socket state, retransmission, reset, destroy event를 JSONL로 저장합니다. 이 mode는 BTF와 eBPF capability가 필요하며 PCAP 파일을 만들지 않습니다. 각 event의 `timestamp_ns`는 요약 줄과 같은 Unix epoch 기준 나노초이고, `boot_time_ns`는 부팅 후 kernel monotonic 시간입니다. TCP event의 `pid`와 `process`는 kernel이 packet을 처리할 때 CPU에서 돌던 task가 아니라 그 socket을 쓰는 process입니다. `Ctrl-C`를 누르면 수집을 멈추고, 그때까지 모은 event와 summary 줄을 저장합니다.

## trace tcp / udp

Linux와 macOS에서 `trace tcp` 또는 `trace udp`를 사용하면 network event를 발생 즉시 출력합니다. 기본 terminal 화면은 event를 스크롤합니다. `Ctrl-C`를 누르면 수집을 종료하고 summary를 출력합니다.

```bash
edc trace tcp
edc trace tcp --duration 15s
edc trace tcp --process slackbot --destination 100.66.11.194:443 --json trace.json
edc trace tcp --raw
edc trace show trace.jsonl       # 저장한 --raw 파일의 요약을 root 없이, 어느 호스트에서나
edc trace udp --duration 15s
edc trace udp --group-by target
edc trace udp --group-by source
edc trace udp --group-by port
edc trace tcp --group-by process
edc trace tcp --group-by event
edc trace tcp -d
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
edc trace http --side server --container web
edc trace tcp --container 5c9e77b3f470 --raw
```

bridge network에서는 주소와 port가 container 안의 값입니다. 예를 들어 `-p 8080:80`으로 publish한 서버는 port 80으로 보입니다.

전체 화면 terminal에서는 `s`로 source 행, `t`로 target 행, `p`로 port 행, `c`로 process 행, `e`로 event 행, `g`로 event 스크롤을 표시합니다. `trace http`에서는 `u`로 path 행을 표시합니다. `Tab`은 다음 보기, `Shift+Tab`은 이전 보기로 바꿉니다. terminal 폭이 넓으면 첫 열을 넓혀 group 값을 자르지 않고 표시합니다. 폭이 좁으면 byte 열을 `195K`(195 KiB)처럼 짧은 단위로 표시합니다.
전체 화면은 최근 event 10,000개를 유지하고, live rate는 이 event들이 걸친 시간으로 계산합니다. `Ctrl-C` 후 summary는 모든 event를 사용합니다.
전체 화면의 group 행은 traffic이 많은 group부터 표시합니다. 행이 terminal에 다 들어가지 않으면 위쪽 행을 표시합니다.

event 목록에서 Up이나 Down(또는 `k`, `j`)을 누르면 event를 고릅니다. event를 고른 동안에는 목록이 멈추고 새 event를 따라가지 않으며, 머리글에 더 새로운 event 수가 표시됩니다. Enter를 누르면 event의 모든 필드와 payload 전체를 보여 주는 상세 보기가 열립니다. 상세 보기는 긴 줄을 화면 폭에 맞춰 나눕니다. Up, Down, PgUp(또는 `b`), PgDn(또는 Space), `g`, `G`로 스크롤하고, Esc나 `q`로 돌아갑니다. 목록에서 `l`, Esc, End 중 하나를 누르면 다시 새 event를 따라갑니다. Mac 자판에는 End 키가 없는 경우가 많습니다. 목록에서도 `b`와 Space는 한 화면씩 고른 위치를 옮깁니다. 고른 event가 목록에서 가장 오래된 event가 되면, 그 아래에 더 새 event를 보여 줍니다. 목록은 payload마다 앞 4KiB만 보관하고, 상세 보기는 최근 event의 payload 전체를 합계 64MiB까지 보여 줍니다.

`f`를 누르면 가장 최근 event의 상세 보기를 열고 새 event를 따라갑니다. `f`를 다시 누르면 현재 event에서 멈춥니다. `trace http` 전체 화면은 기본으로 message마다 최대 1MiB를 수집하고, 목록에는 4KiB snippet을 둡니다.

목록의 payload 행은 처음에 숨깁니다. `v`를 누르면 표시합니다. `--payload`나 `--payload=all`을 명시하면 시작할 때부터 행을 표시합니다. `m`은 알려진 secret header 값을 보이거나 가립니다. 값을 보일 때는 머리글에 `secrets shown`이 표시됩니다. 상세 보기에서 `z`를 누르면 gzip data를 최대 1MiB까지 풀고, 푼 크기를 표시합니다.

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

## trace dns

Linux에서 `trace dns`를 사용하면 DNS 질의와 응답을 발생 즉시 출력합니다. UDP port 53의 DNS message를 읽고, port 53으로 가는 TCP 연결도 표시합니다.

```bash
edc trace dns
edc trace dns --duration 15s --json dns.json
edc trace dns --group-by target
edc trace dns --process curl --destination 127.0.0.53:53
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
edc trace dns --side server
edc trace dns --side server --group-by target
```

서버 쪽 응답 시간은 서버가 질의를 읽은 때부터 응답을 보낸 때까지입니다. 서버 쪽 질의의 `read_delay_ms`는 질의가 서버의 수신 큐에서 기다린 시간입니다. 응답 시간이 짧으면 대개 캐시에서 답한 것이고, 길면 상위 서버에 물어본 것입니다. 서버 쪽에서는 서버가 port 53으로 받은 TCP 연결을 `dns_tcp_accept`로 표시하고, 서버가 그 client에게 잘린 응답을 보냈으면 그 질의의 이름을 붙입니다. edc는 서버가 `accept()`를 부를 때 listen socket의 process를 알게 되므로, trace 전부터 떠 있던 서버의 첫 연결에는 process가 없을 수 있습니다.

`trace dns`는 port 53만 관측합니다. DNS over TCP는 message 앞에 2바이트 길이를 붙입니다. 길이와 message를 두 버퍼로 나눠 쓰거나 두 번에 나눠 읽는 program이 많아, edc는 쓰기의 앞 두 버퍼를 읽고, 2바이트만 읽은 조각은 같은 연결의 다음 읽기와 이어 붙입니다. 한 번의 읽기 중간에서 시작하는 message는 찾지 못합니다. DNS over TLS, DNS over HTTPS, mDNS, LLMNR은 표시하지 않습니다. BPF는 DNS message의 앞 1,024 byte만 읽습니다. 이보다 긴 응답은 header에서 결과 코드를 읽고 이름은 질의에서 가져옵니다. macOS는 socket의 payload를 주지 않으므로 `trace dns`는 Linux에서만 동작합니다.

## trace arp

Linux와 macOS에서 `trace arp`를 사용하면 IPv4 neighbor table(ARP 캐시)의 변화를 출력합니다. root와 eBPF가 필요 없습니다. Linux에서는 kernel 알림을 netlink로 받아 발생 즉시 출력합니다. macOS에서는 `arp -an`과 같은 방법으로 ARP table을 1초마다 읽고 직전 table과 비교합니다.

```bash
edc trace arp
edc trace arp --group-by target
edc trace arp --json arp.json --duration 30s
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
edc trace ndp
edc trace ndp --group-by target
```

macOS kernel은 table의 link-local 주소에 interface 번호를 넣어 둡니다. `trace ndp`는 이 번호를 지우므로 link-local 주소는 `fe80::1`처럼 보이고, interface는 `source` 열에 나옵니다.

## trace http

Linux 5.15 이상에서 `edc trace http`를 실행하면 HTTP/1.x와 HTTP/2를 client·server 양쪽에서 봅니다. 기본 TLS 자동 탐지도 시작하며, TLS 탐색이나 attach가 실패해도 HTTP 수집을 계속합니다. HTTP/1 행은 header가 완성되면 출력하고, HTTP/2 행은 header block을 해석한 뒤 출력합니다.

```bash
edc trace http
edc trace http --group-by target
edc trace http --group-by path
edc trace http --side server --process nginx
edc trace http --payload
edc trace http --side server --port 8080
edc trace http --port 8080 --payload=all
```

일반 text와 `--raw`는 기본으로 message마다 최대 1MiB를 수집합니다. JSON 요약과 `--group-by`는 payload를 기본 수집하지 않습니다. 값 없는 `--payload`는 기존대로 앞 4KiB를, `--payload=all`은 최대 1MiB를 수집합니다. `--payload=false`는 수집하지 않습니다.

전체 화면은 full message를 수집하지만 payload 행은 처음에 숨깁니다. Enter로 상세 보기를 열고 `i`로 미리 보기를 엽니다. `v`는 payload 행을 표시합니다. `z`는 gzip body를 풉니다. 목록은 event마다 4KiB snippet을 두고 최대 10,000개 event를 보관합니다. 전체 payload와 gzip 원본은 합계 64MiB까지 보관합니다. 이 한도에서 message를 내보내면 상세 보기에 전체 payload가 eviction됐다고 표시합니다.

열린 HTTP/1·HTTP/2 message는 합계 4096개와 64MiB까지 보관합니다. message 하나의 상한은 1MiB입니다. 불완전하거나 eviction된 message에는 `payload_truncated`가 표시됩니다.

HTTP/1은 나뉜 header를 합친 뒤 body가 끝나기 전에 metadata를 출력합니다. data가 바뀌어도 250ms보다 자주 같은 행을 갱신하지 않습니다. `Content-Length`와 chunked framing의 끝을 따릅니다. 끝을 알 수 없는 stream은 연결 종료나 trace 종료까지 유지합니다. 강제로 닫은 마지막 snapshot은 truncated로 표시합니다.

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
| 이 host의 HTTP 전부 | `edc trace http` |
| 로컬 서버가 받은 요청 | `edc trace http --side server` |
| 이 host가 보낸 요청(backend나 외부 API 호출) | `edc trace http --side client` |
| port 9000 연결의 양 끝 | `edc trace http --port 9000` |
| port 9900의 proxy가 받은 요청만 | `edc trace http --side server --port 9900` |
| 쪽마다 process별 응답 시간 | `edc trace http --port 9000 --group-by process` |
| path별 요청, 오류, 응답 시간 | `edc trace http --group-by path` |
| HTTPS 안의 HTTP/1.1과 HTTP/2 요청 | `edc trace http` |

한 구간의 client 응답 시간과 서버 응답 시간은 서로 다른 시간을 잽니다. client 응답 시간에는 network와, 서버가 요청을 읽기 전까지 기다린 시간이 들어갑니다. 서버 응답 시간에는 서버가 처리한 시간만 들어갑니다. client 응답 시간이 서버 응답 시간보다 훨씬 길면 network와 서버의 대기열을 확인합니다.

edc는 port가 아니라 data의 앞부분으로 HTTP를 찾으므로, 어느 port의 HTTP든 봅니다. `source`는 항상 이 host 쪽 주소이고 `destination`은 상대 주소입니다. `--port`를 사용하면 이 host나 상대가 그 port를 쓰는 연결만 봅니다. `--side server`와 함께 쓰면 로컬 서버 하나를, `--side client`와 함께 쓰면 이 host가 그 port의 서버로 보낸 요청을 봅니다. port는 kernel에서 확인하므로 다른 연결의 data는 읽지 않습니다.

text, `--raw`, 전체 화면은 알려진 secret header 값을 기본으로 가립니다. 전체 화면은 원문을 보관하고 상세 보기와 미리 보기를 그릴 때 가립니다. `--show-secrets`나 `m`을 명시하면 값을 볼 수 있습니다. query와 body는 redaction하지 않습니다. 출력에 token이나 비밀번호가 있는지 확인한 뒤 공유합니다. 제어 문자는 `\xNN` 문자로 바꿉니다.

가리는 header는 `Authorization`, `Proxy-Authorization`, `Cookie`, `Set-Cookie`, `X-Api-Key`, `X-Goog-Api-Key`, `Api-Key`, `X-Amz-Security-Token`입니다. `--payload`는 query를 유지합니다. `--json`은 요약만 쓰므로 `--payload`와 함께 사용할 수 없습니다.

`--payload=all`은 다음 read/write와 `writev`의 다른 buffer도 따라갑니다. Header가 완성되면 metadata 행을 만들고, body가 바뀔 때마다 같은 message 행을 갱신합니다. 일반 text는 누적 snapshot을 update로 표시합니다. body가 끝났다고 확인하지 못한 stream은 idle 시간만으로 닫지 않습니다. 종료, 상한, trace 중단에서 끝까지 받지 못한 body는 `"payload_truncated": true`로 표시합니다. `--raw`는 update를 쓰지 않고 message마다 최종 event 한 개를 씁니다. 기존 JSON schema는 유지합니다.

TLS를 포함한 HTTP/2 event에는 시작 줄, 일반 header, body, trailer가 들어갑니다. `:authority`는 `Host`로 표시하고 payload의 원래 path와 query는 유지합니다. 구조화된 `path`에서는 query를 뺍니다. Header가 도착하면 metadata를 출력하고 `END_STREAM` 전에도 body snapshot을 갱신합니다. 1MiB 상한은 header·body·trailer 합계입니다. Decoded header와 compressed header block은 각각 64KiB로 제한합니다. 열린 HTTP/1·HTTP/2 message는 앞서 설명한 4096개·64MiB 한도를 함께 씁니다.

`Ctrl-C` 후 summary는 쪽, method, host, path마다 한 행을 표시합니다. 두 쪽이 모두 있으면 쪽별 합계를 따로 보여 주고 `SIDE` 칸을 더합니다. JSON에는 쪽별 합계를 담은 `client`와 `server` 객체가 붙습니다. group 행은 요청, 응답, 4xx·5xx 응답, 응답 없음, 평균·최대 응답 시간을 표시합니다.

HTTPS는 암호문이라 method, path, 상태 코드를 읽을 수 없습니다. 연결을 시작할 때 보내는 TLS ClientHello는 평문이므로, edc는 이를 읽어 `tls_hello` event로 보여 줍니다. `target`은 서버 이름(SNI)이고, `alpn` 필드에는 client가 제안한 protocol이 `h2`, `http/1.1`처럼 들어 있습니다. event 행에는 첫 protocol만 표시합니다. ClientHello를 보낸 client는 `client:` 행으로, 받은 로컬 서버는 `server:` 행으로 보입니다. 요약은 이 연결을 별도의 `TLS connections` 표로 세고, JSON에는 `tls_connections`와 `tls`가 붙습니다. port 443의 HTTPS 연결만 보려면 `--port 443`을 씁니다.

서버의 답도 키가 정해지기 전까지는 평문입니다. edc는 ServerHello를 `tls_server_hello` event로 보여 주며 `tls_version`과 `cipher`가 붙습니다. 예를 들어 `TLS1.3 TLS_AES_128_GCM_SHA256`입니다. `latency_ms`는 같은 연결의 ClientHello부터 잰 시간이라, client에서는 network와 서버가, 로컬 서버에서는 서버만 들어갑니다. TLS 1.2 서버는 고른 protocol도 `alpn`에 적습니다. TLS 1.3은 그 선택을 암호문으로 보냅니다. 한쪽이 handshake를 거절하면 alert record를 보냅니다. edc는 평문 alert를 `tls_alert` event로 보여 줍니다. 예를 들어 `fatal handshake_failure from server`, `fatal protocol_version from server`, `fatal unrecognized_name from server`, `fatal unknown_ca from client`입니다. `alert_from`은 보낸 쪽입니다. 요약의 TLS 표에는 `VERSION`과 `ALERTS` 열이, 그 위에는 alert 종류별 수를 적은 `TLS alerts` 줄이 붙고, JSON에는 `versions`, `alerts`, `tls_alerts`가 붙습니다.

TLS 1.3은 ServerHello 뒤의 alert를 암호화할 수 있습니다. OpenSSL은 client가 서버 인증서를 거절하는 alert를 자기 암호화를 켜기 전에 보내므로, `curl`은 TLS 1.3에서도 TLS 1.2에서도 `fatal unknown_ca from client`를 보입니다. alert를 암호화하는 client는 `tls_server_hello` 뒤에 끝나는 연결만 보입니다. edc는 handshake 머리 뒤 방향마다 처음 8번의 읽기와 쓰기에서만 alert를 찾고, program이 5 byte record 머리와 2 byte alert를 두 번에 나눠 읽어도 찾으며, trace 전에 시작한 연결은 보지 못합니다.

trace를 시작하기 전에 맺은 TLS 연결은 보이지 않습니다. client가 Encrypted Client Hello(ECH)를 쓰면 SNI는 서비스 제공자의 공개 이름입니다. 기본 명령이 TLS를 자동 탐지합니다. TLS를 푸는 곳 뒤의 평문 HTTP도 trace할 수 있습니다. 예를 들어 backend로 평문 HTTP를 보내는 proxy를 봅니다.

TLS 자동 탐지는 암호화 전과 복호화 후의 평문을 읽습니다. OpenSSL, GnuTLS, NSS, wolfSSL, Mbed TLS, rustls-ffi, 지원 Go TLS 바이너리와 등록된 BoringSSL build를 지원합니다. 인증서와 key 파일은 필요하지 않습니다.

TLS probe는 Linux amd64와 arm64에서만 지원합니다. 지원하지 않는 architecture나 ABI, 권한 부족, attach 실패는 이유를 표시하고 HTTP 수집을 계속합니다.

```bash
edc trace http --tls
edc trace http --tls --side server --port 443
edc trace http --tls=/usr/local/bin/node
```

bare 명령과 `--tls`는 TLS를 자동 탐지합니다. `--tls=false`는 TLS 관측을 끕니다. `--tls=<경로>`는 지정한 파일만 봅니다. 위험 kernel에서 bare 명령은 TLS probe를 건너뛰고 이유를 표시하지만, 명시한 `--tls`는 기존 경고와 확인 절차를 유지합니다.

bare 명령과 값 없는 `--tls`는 시작할 때와 trace 중에 다음 대상을 찾습니다.

- 표준 library 디렉터리에 있는 이 host의 `libssl`, `libgnutls`, `libssl3`, `libnspr4`, `libwolfssl`, `libmbedtls`, `librustls`
- 실행 중인 process가 적재한 이 library들(container 안의 것도 포함)
- OpenSSL을 실행 파일 안에 넣고 `SSL_read`를 내보내는 process의 실행 파일(예: `node`)
- TLS library를 적재한 경우에도 `.gopclntab` section이 있는 지원 Go 바이너리
- 지원하는 GNU build ID를 가진 심볼 없는 BoringSSL 실행 파일

edc는 process가 실제로 적재한 파일을 열므로, 패키지를 업데이트한 뒤에도 예전 `libssl`을 쓰는 process를 봅니다. 파일을 열 때 `CAP_SYS_ADMIN`이나 `CAP_CHECKPOINT_RESTORE`가 필요할 수 있습니다. probe를 붙일 때도 `CAP_SYS_ADMIN`이 필요할 수 있습니다. 접근할 수 없으면 이유를 표시하고 HTTP 수집을 계속합니다.

trace 중에도 계속 탐색합니다. 새 process는 시작 후 3초 동안 몇 번 다시 확인합니다. edc는 전체 process도 2초 이상 간격으로 확인합니다. host에 process가 많으면 간격이 길어집니다. 새 파일을 발견하면 probe를 붙입니다. 그 전에 보낸 요청은 놓칠 수 있습니다. 다른 network namespace나 PID namespace에서는 kernel 알림을 받지 못할 수 있습니다. 이때는 주기적인 전체 탐색만 하고 이유를 표시합니다.

파일 하나만 보려면 `--tls=<경로>`로 지정합니다. 이때는 다른 파일을 찾지 않습니다. 경로는 `--payload=all`처럼 띄우지 않고 붙여 씁니다.

probe는 지정한 파일에 붙습니다. program이 같은 내용의 다른 파일(복사본)을 적재하면 그 program은 보이지 않습니다. process가 적재한 파일은 `/proc/<pid>/maps`에서 확인합니다.

NSS는 TLS 상태를 확인할 `libssl3`와 평문 I/O를 읽을 `libnspr4`가 모두 필요합니다. 둘 중 하나를 지정하면 같은 디렉터리나 표준 library 디렉터리에서 다른 하나도 함께 선택합니다.

```bash
edc trace http --tls=/usr/lib/x86_64-linux-gnu/libssl3.so
```

NSS 프로그램을 시작하기 전에 trace를 시작합니다. TLS와 일반 파일·socket을 구분하려면 SSL 설정 호출을 관찰해야 합니다.

NSS 평문은 `PR_Read`, `PR_Recv`, `PR_Write`, `PR_Send`에서 읽습니다. NSPR은 이 함수로 일반 파일과 socket도 읽고 쓰므로, TLS를 쓰지 않는 program에서도 NSPR의 읽기와 쓰기마다 probe가 실행됩니다. `SSL_SECURITY`와 기본값 변경, model 복사, accept한 연결, `PR_Close`도 추적합니다. `SSL_SECURITY`를 끈 연결과 `PR_MSG_PEEK`로 읽은 내용은 TLS event로 표시하지 않습니다.

Go TLS는 Go 함수 표를 읽고 함수 반환 지점에 probe를 붙여 평문을 읽습니다. edc는 Go 1.21.5, 1.23.7, 1.23.8, 1.23.12, 1.25.0, 1.26.5, 1.26.8, 1.27.1, 1.27.3을 지원합니다. 이 버전의 실시간 캡처를 Linux amd64에서 검증했습니다. Linux amd64와 arm64의 normal, stripped, PIE ELF metadata도 확인했습니다. arm64 실시간 캡처는 검증하지 않았습니다. 현재 형식의 유효한 Go 함수 표에서 TLS Read, Write, Close 메서드가 모두 없으면 자동 탐지에서 제외하고 오류를 내지 않습니다. 메서드가 일부만 있거나 함수 표가 손상되면 자동 탐지 안내를 표시합니다. `--tls`로 TLS 메서드가 없는 Go 바이너리를 지정하면 오류를 반환합니다. 이전 형식의 Go 함수 표는 지원하지 않습니다.

```bash
edc trace http --tls=my-go-program
```

bare 명령은 `.gopclntab` section을 확인해 지원 Go 바이너리를 자동 탐지합니다. 바이너리 경로나 command 이름도 지정할 수 있습니다. 일반·stripped·PIE 바이너리를 지원합니다.

이 event에는 socket 주소가 없어 `--port` 필터를 사용하면 제외됩니다. 다른 Go 버전은 지원하지 않습니다.

Go client의 응답 시간은 `Write` 진입부터 `Read` 반환까지입니다. 서버에서는 응답의 `Write` 진입까지 잽니다.

Go HTTP/2는 한 TLS 연결의 동시 stream 16개, 반복 header의 HPACK 처리, 64KiB body의 조각 결합을 검증했습니다.

rustls-ffi는 C 함수 `rustls_connection_read`와 `rustls_connection_write`에서 평문을 읽고, `rustls_connection_free`에서 연결 상태를 지웁니다.

```bash
edc trace http --tls=/usr/local/lib/librustls.so
```

이 event에는 socket 주소가 없습니다. `--port` 필터를 사용하면 제외됩니다. native Rust API는 지원하지 않습니다.

Mbed TLS는 `mbedtls_ssl_read`, `mbedtls_ssl_write`, `mbedtls_ssl_read_early_data`, `mbedtls_ssl_write_early_data`에서 평문을 읽습니다. `mbedtls_ssl_session_reset`과 `mbedtls_ssl_free`에서 연결 상태를 지웁니다. server 쪽 `mbedtls_ssl_read_early_data`는 handshake가 이미 받은 data를 돌려주므로, 이 event에는 socket 주소가 없고 `--port`를 쓰면 빠집니다.

```bash
edc trace http --tls=/usr/local/lib/libmbedtls.so
```

DTLS는 지원하지 않습니다.

wolfSSL은 `wolfSSL_read`와 `wolfSSL_write` 또는 `_ex` 변형에서 평문을 읽고, `wolfSSL_free`에서 연결 상태를 지웁니다.

```bash
edc trace http --tls=/usr/local/lib/libwolfssl.so
```

`--tls=claude`처럼 실행 파일 이름만 지정하면 PATH에서 찾습니다. 현재 디렉터리에 같은 이름의 파일이 있으면 그 파일을 먼저 사용합니다.

심볼 없는 BoringSSL은 Claude Code 2.1.291에 포함된 amd64 Bun 1.4.3 런타임을 지원합니다. GNU build ID는 `ca2032b38650b44e05b2074617d524c7475c80f0`입니다.

edc는 build ID와 함수 코드가 등록된 값과 일치해야 probe를 붙입니다. 다른 심볼 없는 BoringSSL 빌드는 확인한 오프셋을 별도로 등록해야 합니다.

```bash
edc trace http --tls=claude
```

`sudo`로 실행하면 `PATH`에 `~/.local/bin`이 없는 경우가 많습니다. edc가 실행 파일을 찾지 못하면 현재 shell에서 찾은 경로를 넘깁니다.

```bash
sudo edc trace http --tls="$(command -v claude)"
```

OpenSSL, GnuTLS, NSS, wolfSSL, Mbed TLS, rustls-ffi, Go TLS, BoringSSL에서 읽은 요청과 응답 event에는 `"tls": true`가 붙고, event 행에는 event 이름 뒤에 `tls`가 표시됩니다. 목적지는 `https://`로 시작하고, 평문 HTTP의 목적지는 `http://`로 시작합니다. path, 상태 코드, 응답 시간, group 보기, `--payload`, 요약은 평문 HTTP와 같게 동작하고, `--payload`는 같은 header 값을 가립니다. HTTPS의 body에는 token이 들어 있는 경우가 많으므로, 출력을 공유하기 전에 확인합니다.

TLS 위의 HTTP/2도 h2c처럼 stream마다 method, path, 상태 코드, 응답 시간을 표시합니다. frame과 HPACK table을 따라가므로 연결의 평문을 모두 읽습니다. 바쁜 연결은 다른 HTTP event를 유실시킬 수 있으니 비용을 줄이려면 `--port`를 씁니다. byte를 잃으면 그 방향 해석을 멈추고 유실을 셉니다. trace 전에 맺은 HTTP/2 연결은 해석하지 않습니다. HTTP/2 metadata는 header가 도착하면 출력합니다. `END_STREAM` 전에도 같은 행에 body snapshot을 갱신합니다. payload에는 시작 줄, 일반 header, body, trailer가 들어갑니다. `:authority`는 `Host`로 표시합니다. 1MiB message 한도에는 header, body, trailer가 모두 포함됩니다. Decoded header와 compressed header block은 각각 64KiB로 제한합니다. 열린 HTTP/1·HTTP/2 message는 합계 4096개와 64MiB 한도를 함께 씁니다. `--payload`는 최대 4KiB, `--payload=all`은 최대 1MiB를 담습니다. 잘렸거나 eviction된 message에는 `"payload_truncated": true`가 붙습니다.

Bun, `node`, Python `asyncio`처럼 TLS 함수 안에서 socket을 쓰지 않는 program은 어느 연결인지 알 수 없습니다. 이런 event에는 process는 있지만 `source`와 `destination`이 없고, `target`은 `Host` header입니다. 요약은 이 event 수를 `TLS plaintext without an address`로 표시하고, JSON에는 `tls_unmapped`가 붙습니다. `--port`를 쓰면 이런 평문은 port를 확인할 수 없어 표시하지 않고 같은 수에 더합니다.

`--tls`는 OpenSSL이 내보내는 `SSL_read`와 `SSL_write`(또는 `SSL_read_ex`와 `SSL_write_ex`), GnuTLS의 `gnutls_record_recv`와 `gnutls_record_send`, 앞서 설명한 NSS, wolfSSL, Mbed TLS, rustls-ffi, BoringSSL을 봅니다. Debian과 Ubuntu의 `wget`과 `git`은 GnuTLS를 씁니다. Java는 지원하지 않습니다. Go는 앞서 설명한 범위만 지원합니다. 심볼이 없는 program은 지원하는 Go 바이너리와 등록된 BoringSSL 빌드를 지원합니다. `--tls=<경로>`로 지정한 파일은 symbol table도 읽으므로, strip하지 않은 정적 program도 보입니다. `openssl s_server -www`처럼 OpenSSL의 SSL BIO로 읽고 쓰는 program도 보이지 않습니다.

edc는 page fault 없이 평문을 읽습니다. 그 순간 kernel이 그 page를 바꾸고 있으면(예: transparent huge page를 쪼개거나 합칠 때) 그 호출의 평문을 읽지 못합니다. 드문 일이며, edc는 이를 잃은 event로 셉니다.

이 파일을 쓰는 모든 process에서 함수가 불릴 때마다 probe가 실행되며, `--process`로 가린 process도 마찬가지입니다. 끝날 때 kernel이 probe를 하나씩 지우므로, Ctrl-C를 누른 뒤 몇 초 지나서 끝날 수 있습니다. `--tls`가 요구하는 kernel 버전은 `trace http`와 같습니다.

amd64의 Linux 6.11, 6.12.14 전의 6.12, 6.13.3 전의 6.13에서는 seccomp filter 아래의 process가 TLS 호출에서 돌아올 때 종료될 수 있습니다. Bare 명령은 TLS probe를 건너뛰고 이유를 표시하며 HTTP 수집을 계속합니다. 명시한 `--tls`는 경고와 전체 화면 확인 절차를 유지합니다. 배포판 kernel에는 수정이 따로 들어 있을 수 있습니다.

`--tls`가 없으면 HTTPS의 요청은 kernel에서 암호문으로만 보이므로 표시하지 않습니다. TLS 없는 HTTP/2(h2c, 예: cluster 안의 gRPC)는 해석해서 stream마다 method, path, 상태 코드, 응답 시간을 표시합니다. frame과 header 표를 따라가야 하므로 h2c 연결은 모든 byte를 읽고, 그래서 바쁜 h2c 연결은 HTTP/1보다 비용이 큽니다. 이 byte는 다른 HTTP 레코드와 같은 buffer를 쓰므로, 바쁜 h2c 연결이 있으면 다른 연결의 event도 잃을 수 있습니다. 비용을 줄이려면 `--port`를 씁니다. h2c 연결의 byte를 잃으면 그 방향은 더 읽지 않고 잃은 event로 셉니다. HTTP/3의 요청은 binary frame이라 표시하지 않습니다. HTTP/3은 UDP를 쓰므로 `tls_hello` event도 없습니다. edc는 한 번의 읽기나 쓰기가 시작되는 곳에서만 message를 찾습니다. 그래서 한 번의 읽기에 앞 응답의 끝과 다음 응답의 시작이 함께 들어 있으면 다음 응답을 놓칩니다. 프로그램이 message 하나를 여러 버퍼로 나눠 쓰면 첫 버퍼만 읽으므로, `Host` header는 첫 버퍼의 앞 512 byte 안에 있어야 합니다. 없으면 target은 서버 주소입니다. edc는 Linux 5.15 이상에서 이 field를 지원합니다.

## trace mysql

Linux 5.15 이상에서 `trace mysql`을 사용하면 평문 MySQL 명령과 결과를 발생 즉시 출력합니다. edc는 kernel에서 MySQL port의 TCP 읽기와 쓰기마다 앞부분을 읽습니다. 기본 port는 3306이고, 다른 port는 `--port`로 지정합니다.

```bash
edc trace mysql
edc trace mysql --side server
edc trace mysql --port 3307
edc trace mysql --slow 250ms
edc trace mysql --group-by process
edc trace mysql --show-secrets
edc trace mysql --raw
```

`trace mysql`은 `trace http`처럼 두 쪽을 나눠 보여 줍니다. event 행 앞에 쪽이 붙습니다.

- `client:`는 이 host가 MySQL 서버로 보낸 명령입니다.
- `server:`는 로컬 MySQL 서버가 받은 명령입니다.

JSON event에는 `"side": "client"`나 `"side": "server"`가 붙습니다. 한 쪽만 보려면 `--side client`나 `--side server`를 씁니다. 로컬 port가 MySQL port이면 서버 쪽 socket이고, 상대 port가 MySQL port이면 client 쪽 socket입니다.

`--slow <duration>`은 `trace mysql`, `trace clickhouse`, `trace postgres`, `trace redis`에서만 씁니다. `250ms`, `1.5s` 같은 Go duration 문법을 받습니다. 묶지 않은 대화형 화면에서는 첫 paired response의 latency가 지정 시간 이상인 command 행만 남깁니다. 응답 없는 command, 짝이 없는 response, latency가 없는 response, TLS 행은 제외합니다. raw JSON, 비대화형 출력, 요약, group 보기, `--group-by`, `--side`에는 영향을 주지 않습니다.

| 보려는 것 | 명령 |
| --- | --- |
| 이 host의 MySQL 전부 | `edc trace mysql` |
| 로컬 서버가 받은 명령 | `edc trace mysql --side server` |
| 이 host가 보낸 명령 | `edc trace mysql --side client` |
| port 3307의 MySQL | `edc trace mysql --port 3307` |
| 250ms 이상 걸린 paired command | `edc trace mysql --slow 250ms` |
| 쪽마다 process별 응답 시간 | `edc trace mysql --group-by process` |
| 가리지 않은 SQL 원문 | `edc trace mysql --show-secrets` |

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

## trace clickhouse

Linux 5.15 이상에서 `trace clickhouse`를 사용하면 평문 ClickHouse 질의를 발생 즉시 출력합니다. 한 번의 trace가 native TCP protocol과 HTTP interface를 함께 봅니다. edc는 kernel에서 두 port의 TCP 읽기와 쓰기마다 앞부분을 읽습니다. 기본 port는 native TCP 9000, HTTP 8123입니다.

native port는 `--port`로, HTTP port는 `--http-port`로 바꿉니다. `--http-port`는 `trace clickhouse`에서만 쓸 수 있고, 값은 1부터 65535까지입니다. 한쪽 interface만 보는 option은 없습니다.

두 port는 서로 달라야 합니다. 같으면 종료 코드 2로 끝납니다. 다른 option의 기본값과 겹치는 경우도 마찬가지입니다. 예를 들어 HTTP port의 기본값이 8123이므로 `--port 8123`만 주면 실패합니다.

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

`trace clickhouse`는 `trace mysql`처럼 두 쪽을 나눠 보여 줍니다. event 행 앞에 쪽이 붙습니다.

- `client:`는 이 host가 ClickHouse 서버로 보낸 질의입니다.
- `server:`는 로컬 ClickHouse 서버가 받은 질의입니다.

JSON event에는 `"side": "client"`나 `"side": "server"`가 붙습니다. 한 쪽만 보려면 `--side client`나 `--side server`를 씁니다.

`--slow <duration>`은 묶지 않은 대화형 화면에서 씁니다. `250ms`, `1.5s` 같은 Go duration 문법을 받습니다. 짝지은 응답의 latency가 지정 시간 이상인 질의 행만 남기고, 응답 없는 질의, 짝이 없는 응답, latency가 없는 응답은 제외합니다. `--raw`, `--json`, `--group-by`나 terminal이 아닌 출력과 함께 쓰면 종료 코드 2로 끝납니다.

| 보려는 것 | 명령 |
| --- | --- |
| 이 host의 ClickHouse 전부 | `edc trace clickhouse` |
| 로컬 서버가 받은 질의 | `edc trace clickhouse --side server` |
| 이 host가 보낸 질의 | `edc trace clickhouse --side client` |
| native port 9001의 ClickHouse | `edc trace clickhouse --port 9001` |
| HTTP port 18123의 ClickHouse | `edc trace clickhouse --http-port 18123` |
| 500ms 이상 걸린 paired 질의 | `edc trace clickhouse --slow 500ms` |
| 쪽마다 process별 응답 시간 | `edc trace clickhouse --group-by process` |
| 가리지 않은 SQL 원문 | `edc trace clickhouse --show-secrets` |

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

## trace postgres

Linux 5.15 이상에서 `trace postgres`를 사용하면 평문 PostgreSQL 요청과 결과를 발생 즉시 출력합니다. edc는 kernel에서 PostgreSQL port의 TCP 읽기와 쓰기마다 앞부분을 읽습니다. 기본 port는 5432이고, 다른 port는 `--port`로 지정합니다.

```bash
edc trace postgres
edc trace postgres --side server
edc trace postgres --port 6432
edc trace postgres --slow 250ms
edc trace postgres --group-by process
edc trace postgres --show-secrets
edc trace postgres --raw
```

`trace postgres`는 `trace mysql`처럼 두 쪽을 나눠 보여 줍니다. event 행은 `client:`나 `server:`로 시작하고, JSON event에는 `"side": "client"`나 `"side": "server"`가 붙습니다. 한 쪽만 보려면 `--side client`나 `--side server`를 씁니다. `--slow`는 `trace mysql`과 같게 동작합니다.

| 보려는 것 | 명령 |
| --- | --- |
| 이 host의 PostgreSQL 전부 | `edc trace postgres` |
| 로컬 서버가 받은 요청 | `edc trace postgres --side server` |
| port 6432의 PostgreSQL | `edc trace postgres --port 6432` |
| 250ms 이상 걸린 paired 요청 | `edc trace postgres --slow 250ms` |
| 가리지 않은 SQL 원문 | `edc trace postgres --show-secrets` |

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

## trace redis

Linux 5.15 이상에서 `trace redis`를 사용하면 평문 Redis 명령과 응답을 발생 즉시 출력합니다. edc는 kernel에서 Redis port의 TCP 읽기와 쓰기마다 앞부분을 읽습니다. 기본 port는 6379이고, 다른 port는 `--port`로 지정합니다.

```bash
edc trace redis
edc trace redis --side server
edc trace redis --port 6380
edc trace redis --slow 5ms
edc trace redis --group-by process
edc trace redis --show-secrets
edc trace redis --raw
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

## trace socket

Linux에서 `trace socket`을 사용하면 unix domain socket 파일 하나에서 일어나는 일을 출력합니다. socket 파일의 경로를 지정하면 그 socket의 connect, accept, send, recv, data 끝(`eof`), close를 표시합니다. option은 경로 앞이나 뒤에 씁니다.

```bash
edc trace socket /run/docker.sock
edc trace socket /run/docker.sock --payload
edc trace socket /run/php/php-fpm.sock --payload=all --raw
```

목적지는 socket 경로입니다. event 칸에는 호출과 byte 수가 나오고, 호출이 실패하면 `connect ECONNREFUSED`처럼 errno 이름이 나옵니다. 출처(source)는 상대 process입니다. 서버 쪽에서는 연결한 process입니다. 클라이언트 쪽에서는 연결을 accept한 process이고, 그 뒤 서버 쪽에서 data를 주고받은 process가 있으면 가장 최근의 그 process입니다. accept 전에는 `listen()`을 호출한 process라서, systemd가 여는 socket에서는 `systemd`로 나옵니다.

서버 쪽에는 `accept` event가 나옵니다. 이 event는 `accept 120µs`처럼 연결이 backlog에서 기다린 시간을 보여 줍니다. 이 시간이 길면 서버가 연결을 늦게 받는 것이고, worker가 모두 바쁜 경우가 그 예입니다. trace를 시작하기 전에 이미 시작된 `accept` 호출은 보이지 않지만, 그 연결의 다음 서버 쪽 호출부터는 상대가 바르게 나옵니다.

`--payload`를 사용하면 send와 recv마다 data의 앞 4KiB를 볼 수 있고, `--payload=all`을 사용하면 호출마다 1MiB까지 볼 수 있습니다. edc는 호출 하나의 data를 16KiB 조각으로 읽어 event 하나로 합칩니다. stream socket에는 message 경계가 없으므로 event 하나는 message가 아니라 호출 하나입니다. edc는 payload의 형식을 모르므로 어떤 값도 가리지 않고, 그래서 `--show-secrets`는 쓸 수 없습니다. `docker.sock`의 `X-Registry-Auth` header처럼 token이나 비밀번호가 보일 수 있으므로, 출력을 공유하기 전에 확인합니다. 전체 화면에서는 `--payload`가 없어도 호출마다 앞 4KiB를 모읍니다. `v`를 누르면 payload 줄이 보이고, Enter를 누르면 payload 전체가 보입니다.

서버가 재시작하면서 socket 파일을 다시 만들면 edc는 1초 안에 새 파일을 찾습니다. 이전 파일로 맺은 연결도 계속 표시합니다. `trace socket`은 stream socket만 지원합니다. datagram과 seqpacket socket, abstract socket, socketpair는 볼 수 없습니다. `sendfile`과 `splice`로 옮긴 data는 보이지 않습니다. 실패한 connect는 program이 명령과 같은 경로를 쓸 때만 표시합니다.

## trace priv

Linux에서 `trace priv`를 사용하면 process가 권한을 어떻게 쓰는지 볼 수 있습니다. edc는 세 가지를 보고 끝날 때 요약을 한 번 출력합니다.

- capability 검사: `capable()`과 `ns_capable()`의 결과를 kernel 안에서 process, capability, 결과별로 셉니다.
- 자격 변경: thread의 uid, euid, gid, egid가 바뀔 때마다 기록합니다. 예를 들어 `setuid()`, `setresgid()`, `sudo` 같은 setuid program의 exec입니다.
- module 적재: process가 적재한 kernel module과 그 이름을 기록합니다.

```bash
edc trace priv --duration 30s
edc trace priv --denied --container web
edc trace priv --raw
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

## trace drop

Linux에서 `trace drop`을 사용하면 kernel이 패킷을 버린 이유를 볼 수 있습니다. 버린 패킷마다 이유, kernel 함수, 주소와 port, 크기를 보여 줍니다. 패킷이 local socket에 속하면 process도 보여 줍니다.

```bash
edc trace drop
edc trace drop --reason NO_SOCKET,SOCKET_RCVBUFF
edc trace drop --container web --raw
```

이유는 kernel이 쓰는 이름입니다. 예를 들어 `NO_SOCKET`은 그 port를 쓰는 socket이 없는 경우, `SOCKET_RCVBUFF`는 socket의 수신 buffer가 가득 찬 경우, `NETFILTER_DROP`은 방화벽 규칙이 버린 경우입니다. 이유는 Linux 5.17 이상에서 나옵니다. 그 전 kernel에서는 이유가 `unknown`이고 함수만 보입니다. `--reason`에 쉼표로 나눈 이름을 주면 그 이유만 봅니다. 이름은 대소문자를 가리지 않습니다.

kernel은 정상 동작 중에도 패킷을 해제합니다. 예를 들어 program이 읽지 않은 data가 남은 socket을 닫으면 `QUEUE_PURGE`나 `TCP_ABORT_ON_DATA`가 나옵니다. 이런 버림도 program이 data를 읽지 않았다는 뜻이라 함께 보여 줍니다.

요약의 이유별 합계는 정확합니다. 한 CPU에서 1초에 1,000건이 넘게 버려지면 그 1초 동안은 event를 1,000건만 보내고 나머지는 세기만 합니다. 이 수는 요약에 `Sampled out`으로 나옵니다. process는 socket이 있는 패킷에만 붙습니다. 닫힌 port로 온 패킷에는 socket이 없습니다.

## trace exec

Linux에서 `trace exec`를 사용하면 process가 뜨고 끝나는 것, 누가 신호를 보냈는지, OOM killer가 어느 process를 끝냈는지 볼 수 있습니다. `exec` event는 PID, 부모, 사용자, 명령줄의 처음 256 byte를 보여 줍니다. `exit` event는 종료 코드, process를 끝낸 신호, core dump 여부, 산 시간을 보여 줍니다. 산 시간은 task의 시작 시각에서 재므로 trace 전에 뜬 process에도 나옵니다. `signal` event는 신호, 보낸 쪽, 받는 쪽, kernel이 전달했는지 무시했는지를 보여 줍니다.

```bash
edc trace exec --duration 15s
edc trace exec --process cron,sh --group-by parent
edc trace exec --signal all --raw
edc trace exec --group-by event --json exec.json
```

요약은 process 이름으로 행을 묶습니다. exec 수, exit 수, 0이 아닌 코드로 끝난 수, 신호로 끝난 수, 1초를 못 살고 끝난 수를 세고 평균과 가장 긴 수명을 보여 줍니다. `EXEC`와 `SHORT`가 함께 높으면 짧은 process가 쏟아지는 것입니다. 예를 들어 1초마다 shell을 띄우는 health check가 그렇습니다. 누가 띄우는지는 `--group-by parent`로 봅니다. 두 번째 표는 신호를 보낸 쪽과 받는 쪽으로 묶습니다. 결과 뒤의 `(kernel)`은 kernel이 만든 신호라는 뜻입니다. 예를 들어 fault 뒤의 `SIGSEGV`나 OOM killer의 `SIGKILL`이고, 이때 보낸 쪽은 그 순간 실행 중이던 process입니다.

`oom` event는 OOM killer가 고른 process를 보여 줍니다. `TRIGGER` 칸은 메모리가 모자랄 때 메모리를 할당하려던 task입니다. Linux 6.x에서는 victim의 이름, anon·file·shmem RSS, `oom_score_adj`도 보여 줍니다. Linux 5.15의 kernel은 victim의 PID만 줍니다. victim의 `exit` event에는 `oom-killed`가 붙고, 요약의 `OOM` 칸이 이 종료를 셉니다. 이 표시는 process의 kernel 상태에서 읽으므로 Linux 5.15에서도 정확합니다. `oom` event에서 `--container`는 TRIGGER task의 cgroup으로 맞춥니다.

기본으로는 process를 끝내거나 core를 남기는 신호만 기록합니다. `HUP`, `INT`, `QUIT`, `ILL`, `TRAP`, `ABRT`, `BUS`, `FPE`, `KILL`, `USR1`, `SEGV`, `USR2`, `PIPE`, `TERM`, `STKFLT`, `XCPU`, `XFSZ`, `SYS`입니다. `CHLD`나 `ALRM`처럼 늘 오가는 신호는 뺍니다. `--signal`에 쉼표로 나눈 이름이나 번호, `all`, `none`을 줍니다. 필터가 버린 신호 수는 요약의 `filtered_signals`에 나옵니다. `--process`는 이름이나 PID를 받고, 신호는 받는 쪽으로 맞춥니다. `--container`는 container 하나의 event만 남깁니다.

명령줄은 exec 시점에 새 process의 메모리에서 읽으므로 program이 받은 인자이고, 나중에 바뀐 값은 아닙니다. 읽지 못하면 실행 파일 경로를 보여 줍니다. 256 byte보다 긴 줄은 `…`로 끝나고 `args_truncated`에 셉니다. 스레드의 종료는 `exit` event가 아니고, process의 마지막 스레드만 `exit`를 냅니다. kernel thread는 보이지 않습니다. `sched_process_exec`, `sched_process_exit`, `signal_generate` tracepoint와 BTF가 필요합니다. `oom` event에는 tracefs의 `oom/mark_victim` tracepoint도 필요합니다. container처럼 tracefs가 없으면 알림을 출력하고 나머지 event를 봅니다.

## trace lock

Linux에서 `trace lock`을 사용하면 thread가 lock을 기다리는 곳을 볼 수 있습니다. edc는 두 가지 기다림을 재고 끝날 때 요약을 한 번 출력합니다.

- kernel lock 기다림은 kernel 안의 spinlock, rwlock, mutex, rwsem, per-CPU semaphore 경합입니다. Linux 5.19 이상에 있는 `lock:contention_begin`과 `lock:contention_end` tracepoint를 씁니다. mutex 기다림은 task가 spin을 시작할 때 시작하고, spin 뒤에 잠든 시간까지 포함합니다.
- futex 기다림은 사용자 공간에서 부른 `futex_wait`입니다. 경합한 `pthread_mutex`, condition variable, Go runtime이 모두 이렇게 기다리므로 futex 기다림이 늘 경합은 아닙니다.

```bash
edc trace lock --duration 10s
edc trace lock --process api --kind futex
edc trace lock --container db --json lock.json
```

`KERNEL LOCK` 표는 lock, lock 종류, 부른 함수로 기다림을 묶습니다. 정적 lock은 `tasklist_lock`이나 `rtnl_mutex`처럼 이름을, 나머지는 주소를 보여 줍니다. 종류는 `perf lock`의 이름을 씁니다: `spinlock`, `rwlock:R`, `rwlock:W`, `rwsem:R`, `rwsem:W`, `mutex`, `rtmutex`, `pcpu-sem:R`, `pcpu-sem:W`. 부른 함수는 stack에서 lock 함수가 아닌 첫 kernel 함수입니다. `PROCESS`는 가장 자주 기다린 process입니다.

`FUTEX` 표는 process와 futex 주소로 기다림을 묶습니다. 주소가 전역 변수이면 ELF 심볼에서 `main.hotWord`나 `runtime.sched+0x120`처럼 이름을 찾습니다. 나머지 주소는 `[anon] 0x7f3a1c000960`처럼 mapping과 주소를 보여 줍니다. `-s`로 build한 Go program처럼 심볼표가 없는 program은 file 이름만 나옵니다. `CODE`는 기다린 시간이 가장 긴 stack의 안쪽 두 사용자 frame입니다.

edc의 집계를 맞춰 보았습니다. 시험 program이 전역 futex 하나에서 800번 기다렸고, edc는 Linux 6.17과 Linux 5.15에서 모두 `main.hotWord`의 기다림 800번을 셌습니다. thread 8개가 메모리를 mapping하고 해제하는 program에서 `perf stat`은 `lock:contention_end`를 359,516번, edc는 359,506번 셌습니다. `perf stat`은 program이 이름을 얻기 전인 fork부터 셉니다.

trace가 도는 동안 host의 모든 kernel lock 경합과 futex 기다림이 BPF program을 거칩니다. Linux 6.17, CPU 12개 host에서 고른 process의 경합 한 번은 두 program에서 1.57µs와 0.34µs가 걸렸고, 대부분은 stack을 읽는 시간입니다. 필터가 고르지 않은 process는 0.26µs와 0.07µs였습니다. futex 기다림은 1.59µs와 0.42µs, 고르지 않으면 0.29µs와 0.08µs였습니다. 시험 program은 trace가 있을 때와 없을 때 같은 시간에 같은 양의 일을 했습니다. trace가 경합 한 번을 길게 만들어 경합 횟수는 바뀔 수 있습니다. 같은 program이 아무것도 고르지 않는 필터에서는 358,687번, trace 대상일 때는 203,922번 경합했습니다.

Linux 5.15에는 contention tracepoint가 없습니다. 그러면 알림을 출력하고 futex 기다림만 보여 줍니다. BTF와 task local storage가 필요합니다.

## trace syscall

Linux에서 `trace syscall`을 사용하면 process별 system call 수와 시간을 볼 수 있습니다. edc는 kernel 안에서 셉니다. terminal에서는 표를 1초마다 다시 그리고, Ctrl-C를 누르면 최종 표를 출력합니다. 출력을 파이프로 넘기거나 `--json`을 쓰면 끝날 때 요약을 한 번 출력합니다. `--slow`나 `--errors`를 주면 조건에 맞는 호출도 하나씩 출력합니다.

```bash
edc trace syscall --duration 10s
edc trace syscall -- curl -s https://example.com
edc trace syscall --process nginx,1234 --duration 30s
edc trace syscall --group-by syscall --json syscall.json
edc trace syscall --container web --group-by cgroup
edc trace syscall --process api --errors ENOENT,EACCES
edc trace syscall --syscall openat,statx --slow 10ms --raw
```

행 하나는 process 이름 하나와 system call 하나입니다. 행마다 호출 수, 오류 수와 가장 많은 errno, 호출 안에 머문 시간의 합계, 평균, p95, 최댓값을 보여 줍니다. 시간은 CPU 시간이 아니라 호출 안에 머문 실제 시간입니다. 그래서 `epoll_wait`나 `futex`처럼 기다리는 호출이 위에 옵니다. 합계는 모든 스레드의 시간을 더하므로 trace 시간보다 길 수 있습니다. 행은 합계 시간 순서입니다. 표에는 20행이 나오고, `--all`을 주면 모든 행이 나옵니다. `--json`에는 늘 모든 행이 들어갑니다. p95는 `top -d`와 같은 histogram bucket의 위쪽 경계라서 `<1.05s`처럼 보입니다.

`--group-by syscall`, `--group-by process`, `--group-by cgroup`은 한 열로 묶어 더합니다. `--process`는 쉼표로 나눈 PID와 command 이름을 받습니다. kernel 안에서 비교하므로 이름은 process의 command 이름과 정확히 같아야 하고, 15 byte까지만 봅니다. PID는 그 process의 모든 스레드에 맞습니다. `--container`는 container 하나의 호출만 셉니다.

빨리 끝나는 명령은 `--` 뒤에 적어 trace할 수 있습니다. edc는 trace를 켠 뒤에 새 cgroup 안에서 명령을 시작하므로 첫 호출부터 셉니다. 명령이 시작한 process의 호출도 함께 셉니다. 명령이 끝나면 trace도 끝나고, edc는 명령의 종료 코드를 돌려줍니다. root로 실행해야 하며, `--process`, `--container`, `--duration`은 명령과 함께 쓸 수 없습니다. cgroup v2를 쓸 수 없으면 명령 자신의 호출만 세고 경고를 출력합니다.

`FILES` 표도 함께 출력하며, 실시간 화면에도 보입니다. 행은 경로 하나이고, 열은 읽기로 연 횟수(`READ`), 쓰기로 연 횟수(`WRITE`), `mkdir`, `link`, `symlink` 호출과 `O_EXCL`로 연 횟수(`CREATE`), 삭제(`DELETE`), 이름 변경(`RENAME`), 그 밖의 변경(`OTHER`)입니다. 이름 변경 행에는 원래 경로가 나옵니다. kernel은 `O_CREAT`로 연 호출이 파일을 새로 만들었는지 알려 주지 않으므로, 파일을 만드는 open도 `WRITE`에 들어갑니다. 실패한 `stat`과 `access` 호출도 `OTHER`에 세므로, process가 찾았지만 없었던 파일을 표에서 볼 수 있습니다. `openat2`는 flags를 읽지 않으므로 `OTHER`에 들어갑니다. `ERRORS` 열은 실패한 호출 수와 가장 많은 errno입니다. 실패한 open도 해당 열에 세어 넣습니다.

파일을 바꾼 행이 먼저 나오고, 실패한 호출이 있는 행, 읽기만 한 행 순서입니다. 표에는 20행이 보이고, `--all`을 주면 모든 행을 보여 줍니다. `--json`의 `files`에도 모든 행이 들어 있습니다. 표를 빼려면 `--no-files`를 쓰십시오. `--slow`나 `--errors`를 주면 `--files`를 함께 줄 때만 파일 표가 나옵니다. edc는 파일 event를 CPU마다 1초에 최대 10,000건, 경로를 최대 10,000개까지 기록합니다. 놓친 event가 있으면 표 아래에 알립니다.

`trace syscall`도 `edc top`처럼 event를 남깁니다. 조건이 5초 이상 이어지면 event가 되고, 꺼졌다가 5초 안에 다시 켜지면 같은 event입니다. 실시간 화면에는 최근 event 3개가 보이고, 끝난 뒤의 출력에는 모든 event가 시작 시각, 지속 시간, 초당 최댓값, 횟수와 함께 나옵니다. `●`는 조건이 지금 켜져 있다는 뜻입니다. `--json`의 `events`에도 들어갑니다. 종류는 세 가지입니다.

- `error loop`: 한 process가 같은 system call을 같은 errno로 초당 100번 이상 실패합니다. 바쁜 프로그램에서 정상인 오류는 세지 않습니다: `EAGAIN`, `EINTR`, `EINPROGRESS`, 재시작 errno, `futex`의 `ETIMEDOUT`, `wait4`의 `ECHILD`.
- `missing path`: 한 경로에 대한 호출이 `ENOENT`나 `ENOTDIR`로 초당 50번 이상 실패합니다. 예를 들어 없는 파일을 계속 `stat`하는 경우입니다. 있는 경로의 권한 오류는 없는 경로가 아닙니다. 이 종류는 파일 event를 쓰므로 파일 표가 켜져 있어야 하고, 파일 event의 상한이 적용됩니다. 그래서 최댓값이 실제보다 낮을 수 있습니다.
- `call spike`: 한 process가 한 system call을 초당 1,000번 이상, 그리고 최근 30초 평균의 10배 이상 부릅니다. 늘 많이 부르는 process는 event가 아닙니다. 처음 5초는 평균이 없어 event를 만들지 않습니다.

기준값은 고정입니다. `--slow`, `--errors`, `--raw`를 쓰면 호출을 하나씩 출력하므로 event를 만들지 않습니다.

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

## trace reclaim

Linux에서 `trace reclaim`을 사용하면 task가 메모리를 기다린 시간을 볼 수 있습니다. edc는 네 가지 정지를 kernel 안에서 세고, 끝날 때 요약을 한 번 출력합니다.

| 종류 | task가 멈추는 때 |
|---|---|
| `direct` | zone의 남은 메모리가 모자라, 할당하던 task가 직접 메모리를 회수합니다. |
| `memcg` | cgroup이 `memory.max`나 `memory.high`에 닿아, task가 그 cgroup의 메모리를 회수합니다. |
| `throttle` | cgroup이 `memory.high`를 넘었습니다. task가 사용자 공간으로 돌아가기 전에 kernel이 회수하고 task를 재웁니다. |
| `compact` | task가 큰 page가 필요해 직접 메모리를 compaction합니다. |

```bash
edc trace reclaim --duration 30s
edc trace reclaim --container web --slow 10ms
edc trace reclaim --group-by cgroup --json reclaim.json
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
## trace cpu

Linux에서 `trace cpu`를 사용하면 CPU를 쓰는 함수를 찾을 수 있습니다. edc는 CPU마다 실행 중인 task의 stack을 기본 1초에 99번 샘플링합니다. 샘플은 kernel 안에서 세고, 끝날 때 요약을 한 번 출력합니다.

```bash
edc trace cpu --duration 30s
edc trace cpu --process api,1234 --duration 10s
edc trace cpu --container web --folded cpu.folded
edc trace cpu --frequency 499 --folded - | flamegraph.pl > cpu.svg
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
edc trace cpu --off-cpu --process api --duration 10s
edc trace cpu --off-cpu --container db --folded db-offcpu.folded
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

## PCAP 파일

```bash
edc capture \
  --interface en0 \
  --duration 15s \
  --count 500 \
  --filter 'host 203.0.113.10 and port 443' \
  --output incident.pcap
```

PCAP에는 credential과 개인정보가 포함될 수 있습니다. JSON redaction은 PCAP payload에 적용되지 않습니다.

`edc capture`는 시작 전에 계획을 보여 줍니다. 계획에는 interface, duration, packet 상한, filter, 출력 경로, `edc`가 쓰는 권한이 들어갑니다. 좌우 방향키와 Enter로 답하거나 `y`, `n`으로 답합니다. 계획은 `tcpdump` 출력 위에 그대로 남습니다.

질문을 건너뛰려면 `--yes`를 씁니다. terminal이 아닌 실행은 계획을 출력하고 stdin에서 `y`나 `n`을 읽습니다.


## trace io

Linux에서 block I/O의 queue·service·전체 지연을 측정합니다. 기본으로 1ms 이상의 요청을 표시하며, `--slow`로 기준을 바꿉니다. request insert 때 process와 cgroup을 기록합니다. insert가 없는 요청은 issue 시점의 소유자를 사용하고 `queue_ms`를 만들지 않습니다.

```bash
edc trace io --duration 15s
edc trace io --device 8:0 --group-by device
edc trace io --process postgres --raw
```

kernel BTF, block request tracepoint, `CAP_BPF`와 `CAP_PERFMON`이 필요합니다. tracefs mount와 `CAP_NET_ADMIN`은 필요하지 않습니다. 요약에는 손실·짝을 찾지 못한 요청·threshold 미만의 요청 수가 포함됩니다. [I/O 측정과 attribution 상세](tracing.md#trace-io)를 참고하세요.

## trace sched

Linux에서 잠든 task의 wakeup-to-run, preemption 후 runnable queue, off-CPU 시간을 측정합니다. off-CPU에는 앞의 지연이 포함되므로 더해서 해석하지 않습니다. 기본 기준은 1ms이며 `--slow`로 바꿉니다.

```bash
edc trace sched --duration 15s
edc trace sched --process api --group-by syscall
edc trace sched --raw
```

thread가 CPU에서 내려갈 때의 system call을 표시합니다. `futex`, `epoll_wait`, `read` 등으로 대기 유형을 좁힐 수 있으며, system call 안에서 기다리지 않았으면 `-`입니다. Linux 5.15에서 동작하며 필요한 BTF 필드를 확인한 뒤 program을 load합니다. [스케줄러 지연과 손실 카운터 상세](tracing.md#trace-sched)를 참고하세요.

## trace show

저장한 `--raw` JSONL을 다른 호스트에서 root 없이 요약합니다.

```bash
edc trace show trace.jsonl
```

실시간 trace와 같은 grouping과 필터를 사용할 수 있습니다. [저장 파일 조회 상세](tracing.md#trace-show)를 참고하세요.
