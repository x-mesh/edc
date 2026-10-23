[English](network.md) | **한국어** · [README로 돌아가기](../README.ko.md)

# 네트워크 점검

[네트워크 점검](#네트워크-점검) · [어디에 있는가](#어디에-있는가) · [Probe 임계값](#probe-임계값) · [Probe 진행 줄](#probe-진행-줄) · [빠진 인자](#빠진-인자) · [Doctor 실시간 화면](#doctor-실시간-화면) · [네트워크 품질](#네트워크-품질)

## 네트워크 점검

필요한 계층을 개별 점검하거나, `doctor`로 종합 진단합니다.

```bash
edc dns lookup example.com
edc tcp check example.com:443
edc tls check --min-days 14 example.com:443
edc http check --expect-status 200 https://example.com
edc net route example.com
edc doctor https://example.com
```

redaction을 적용한 report를 저장하고 나중에 열어 봅니다.

```bash
edc doctor --redact --json report.json https://example.com
edc report show report.json
edc report diff before.json after.json
```

`report diff`는 저장한 report 두 개를 비교하며, 악화된 probe가 있으면 exit code `1`을 반환합니다.

공통 option은 `--timeout`, `--json <path|->`, `--verbose`, `--redact`입니다. Go `flag` 규칙에 따라 option은 target 앞에 둡니다.

### 이름 조회

![edc dns lookup example.com이 주소 목록을, edc dns config가 resolver 설정을 각각 PASS로 출력하는 화면](media/dns.gif)

기본적으로 터미널과 JSON에 실제 IP 주소를 보여줍니다. `--redact`를 주면 `<ip:...>`로 가립니다.

`edc dns compare example.com`은 system resolver와 `1.1.1.1`을 비교합니다. 다른 DNS 서버를 쓰려면 `--resolver IP[:port]`를 반복해서 지정합니다. A, AAAA, CNAME, 응답 상태를 비교하고, 직접 지정한 resolver의 TTL도 보여 주되 TTL 차이만으로 불일치 처리하지는 않습니다. 응답이 다르면 WARN, system 조회가 실패하면 FAIL입니다.

### 연결 확인

![edc tcp check가 example.com:443 연결에 성공하고, 닫힌 port에서는 timeout phase와 함께 FAIL을 출력하는 화면](media/tcp.gif)

실패한 probe는 phase와 cause를 ERROR 블록으로 보여 주고 exit code `1`을 돌려줍니다.

`edc watch http -i 0.1 https://example.com`은 Ctrl-C까지 페이지를 반복 확인합니다. `-i`는 초 단위 소수(최소 `0.1`)나 `100ms` 같은 duration을 받으며, `--duration 1m`으로 종료 시각을 정할 수 있습니다. 매 sample에 HTTP status, 읽은 body byte(최대 10 MiB), 소요 시간과 수집된 DNS/TCP/TLS/TTFB 시간을 표시합니다. DNS의 IP 집합이 바뀌면 새 목록을 보여 줍니다. 완료한 표본이 있으면 마지막에 min/avg/p95/max 지연과 최장 연속 실패 시간을 요약합니다. `--json <path|->`는 마지막 요약을 포함한 JSON Lines를 출력합니다. 요약에는 `observation_status`(`observed` 또는 `no_samples`)와 `stop_reason`(`duration` 또는 `cancelled`)이 추가됩니다.

완료한 표본이 없으면 텍스트 요약은 관측 종료 이유를 설명하고 지연 통계를 생략합니다. 이때 대상의 상태를 판단할 수 없습니다. 완료한 표본 없이 관측 시간이 끝나면 exit code는 `2`, 취소하면 `4`입니다. 완료한 표본이 있으면 기존 정책을 유지합니다. 실패한 표본이 있으면 `1`, 없으면 취소한 경우에도 `0`입니다.

`edc listen --watch`는 현재 listener를 한 번 출력한 뒤 socket의 생성·종료·소유 process 변경을 알려 줍니다. 변경 줄은 터미널에서 반전 표시합니다. 같은 interval·duration 옵션을 받으며, `--json`은 초기 snapshot·event·요약을 JSON Lines로 출력합니다.

기본·상세 표의 범위 열과 watch 변경 줄은 소켓이 바인딩한 주소를 loopback, 모든 인터페이스, 특정 주소, Unix, 알 수 없음으로 구분합니다.

wildcard의 모든 인터페이스는 관측한 주소 계열의 바인딩을 뜻합니다. 외부 접근 가능 여부, 방화벽 규칙, IPv6 dual-stack 동작을 판단하는 값은 아닙니다.

### 파일 감시와 action (Linux·macOS)

`edc watch fs`는 기본으로 현재 디렉터리 바로 아래의 변경을 감시하고 출력합니다. `-r`이나 `--recursive`로 하위 디렉터리까지 감시하며, 새로 생성되거나 들어온 디렉터리도 등록합니다. 시작할 때 이미 있던 파일은 create 이벤트로 출력하지 않습니다. 디렉터리 symlink는 따라가지 않습니다.

```bash
edc watch fs
edc watch fs ./src -r
edc watch fs --event create --match text.txt --exec 'git-kit pull'
edc watch fs ./src --recursive --event modify --match '**/*.go' --exec 'go test ./...'
edc watch fs --rules docs/examples/watch.yaml --dry-run
edc watch fs --duration 1m --json events.jsonl
```

이벤트는 `create`, `modify`, `remove`, `rename`입니다. `rename`의 path는 원래 이름이며 감시 범위 안의 새 이름은 create로 나타날 수 있습니다. 파일 읽기(access)와 metadata-only 변경은 감지 대상이 아닙니다. `--event`는 쉼표로 여러 이벤트를 받으며 `--match`는 감시 루트 기준 glob입니다. `*.go`는 바로 아래 파일, `**/*.go`는 모든 깊이의 파일에 일치합니다. 하위 경로를 실제로 감시하려면 `--recursive`도 지정합니다.

터미널에서 `edc watch fs`는 한 변경의 이벤트를 한 줄로 합쳐 보여 줍니다. 이것이 `--smart`입니다. 이벤트를 200ms 동안 모았다가 결과만 출력합니다. 임시 파일에 쓰고 이름을 바꿔 교체하는 저장은 `CREATE "dir/final"  ← "final.tmp"`처럼 한 줄이 됩니다. 같은 200ms 안에 만들었다가 지운 파일은 `TEMP`로, 한 파일의 `MODIFY`가 여러 번이면 `×N`으로 보입니다. 이름 바꿈과 그 뒤의 create는 둘 사이에 다른 이벤트가 없을 때만 한 줄이 됩니다. 200ms의 끝에 걸친 변경은 두 줄로 보일 수 있습니다. pipe로 보낼 때, `--json`, `--no-smart`는 이벤트를 오는 대로 모두 출력합니다. `--smart`는 pipe에서도 합치기를 켜지만 `--json`과 함께 쓸 수 없습니다. 규칙과 요약은 원본 이벤트를 쓰므로, 요약의 이벤트 수가 화면에 보이는 줄보다 많을 수 있습니다.

같은 줄이 5초 안에 다시 나오면 새 줄을 쓰지 않고, 상태줄 바로 위의 줄을 고쳐 써서 `×N`을 붙입니다. 상태줄을 그리는 터미널에서만 동작합니다. 두 줄 사이에 안내나 action 같은 다른 출력이 끼면 새 줄로 시작합니다.

같은 모드에서 시끄러운 디렉터리도 알립니다. 한 디렉터리가 10초 동안 이벤트를 30건 이상 내고 그 시간 전체 이벤트의 절반 이상이면, stderr에 `힌트:`로 시작하는 한 줄을 출력합니다. 이 줄은 그 디렉터리를 빼는 `--exclude` 옵션을 보여 줍니다(예: `--exclude 'logs/**'`). 같은 디렉터리는 한 번만 알리고, 한 번 실행에서 최대 3개 디렉터리까지 알립니다. 감시 루트와 이름에 glob 문자가 든 디렉터리는 알리지 않습니다. stderr가 터미널이어야 하고, `--no-smart`로 끌 수 있습니다.

감시를 멈추면 요약 줄 아래에 표를 출력합니다. 이벤트가 가장 많은 디렉터리 5개를 횟수와 전체에서 차지하는 비율과 함께 보여 주므로, 무엇을 제외할지 고르는 데 쓸 수 있습니다. 이벤트가 10건 이상일 때만 출력합니다. `--json`에서는 출력하지 않고, `--no-smart`로 끌 수 있습니다.

`fanotify` 드라이버에서는 줄 끝에 파일을 바꾼 process의 이름이 `주체 syslog-ng`처럼 붙습니다. 커널이 process를 알려 주고, edc는 이벤트가 도착할 때 그 이름을 읽습니다. 이미 끝난 process는 이름이 없고, `mv` 같은 짧은 명령은 이벤트를 읽기 전에 끝나는 일이 많아 이름이 나오지 않습니다. 한 줄에 합친 이벤트가 서로 다른 process에서 왔으면 이름을 한 번씩 모두 보이고(예: `주체 editor, formatter`), 3개를 넘으면 `+N`을 붙입니다. `inotify`는 process를 알려 주지 않고, `--no-smart`는 이름을 붙이지 않습니다. JSON 기록에는 드라이버가 알면 `pid`가 들어가고(이름을 몰라도), 이름을 알 때 `process`가 들어갑니다. 커널이 한 경로의 이벤트를 합치면 process는 첫 이벤트의 것입니다.

기본으로 `.git`, `node_modules`, `.venv`, `venv`, `__pycache__`, `.mypy_cache`, `.pytest_cache`, `.ruff_cache`, `.tox`, `.next`, `.gradle`을 모든 깊이에서 제외합니다. 빌드 결과물을 감시할 수 있도록 `build`와 `dist`는 제외하지 않습니다. `--exclude 'build/**'`처럼 제외 glob을 반복해서 추가할 수 있습니다. 기본 제외 디렉터리도 감시하려면 `--no-default-exclude`를 쓰며, 이때도 `--exclude`로 준 패턴은 적용됩니다.

Linux에서는 `--driver`로 감시 방식을 고릅니다. 기본값은 `auto`입니다.

- `fanotify`: 파일시스템 전체에 mark 하나를 겁니다. 시작할 때 트리를 훑지 않고, inotify watch 한도도 받지 않습니다. root(`CAP_SYS_ADMIN`)와 Linux 5.17 이상이 필요합니다.
- `inotify`: 디렉터리마다 감시를 등록한 뒤에 출력을 시작합니다. 트리가 크면 시간이 걸리며, stderr가 터미널이면 이 동안 안내를 출력합니다. 디렉터리 수가 `fs.inotify.max_user_watches`를 넘으면 오류로 멈춥니다.

`--recursive`이면 `auto`는 `fanotify`를 먼저 시도하고, 시작할 수 없으면 `inotify`를 씁니다. stderr가 터미널이면 그 이유를 한 줄로 알립니다. `--recursive`가 없으면 감시가 하나뿐이므로 `inotify`를 씁니다. 실제로 쓴 방식은 `ready` 기록의 `driver`에 나옵니다. `auto`가 `inotify`를 고르면 시작 줄과 `ready` 기록의 `driver_reason`에 이유(`not_recursive` 또는 `fanotify_unavailable`)가 나옵니다. `--driver`로 직접 고른 방식과 `fanotify`에는 이유가 없습니다. `--driver fanotify`를 지정했는데 `fanotify`를 시작할 수 없으면 이유를 출력하고 멈춥니다.

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

![edc net interfaces, edc net route example.com, edc net ping example.com이 각각 PASS와 결과 한 줄을 출력하는 화면](media/net.gif)

## 어디에 있는가

`edc where`는 두 질문에 한 번에 답합니다. 이 host에서 어느 클라우드 지역이 가까운지, 그리고 이 망이 어떤 모습인지.

```bash
edc where
edc where --provider aws        # 사업자 하나만
edc where --count 5 -v          # 더 여러 번 재고 사업자별로 모두 보기
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

![edc where가 public IP와 ASN, Cloudflare PoP, route, NAT 모습, 왕복 시간 순으로 가까운 지역을 보여 주는 화면](media/where.gif)

데모 화면은 public IP를 `222.XXX.XXX.XXX`로 가리고, route 주소를 사설 예시 주소로 바꿨습니다.

`edc`는 확인한 것만 밝힙니다. 지터로 회선 종류를 짐작하지 않습니다. 지터는 숫자로 남기고 판단은 사용자에게 맡깁니다.

## Probe 임계값

`edc tls check`는 인증서 만료가 30일보다 적게 남으면 warning을 냅니다. `--min-days`를 지정하면 그보다 이른 만료를 실패로 처리합니다.

`edc http check`는 4xx 응답에 warning을, 5xx 응답에 실패를 냅니다. `--expect-status`를 지정하면 그 code만 통과하고 나머지는 실패입니다.

대상에 scheme이 없으면 `edc http check`는 `http://`를 붙입니다. `edc http check naver.com`은 `http://naver.com`을 요청하고 redirect를 따라갑니다. `url` metric에는 실제로 요청한 주소가 남습니다. TLS endpoint를 바로 확인하려면 `https://`를 씁니다.

서버가 redirect를 보내면 `edc http check`는 요청마다 status, URL, 시간 값, peer를 차례로 보여 줍니다. `-v`를 붙이면 응답마다 header가, 마지막 응답에는 본문 앞부분이 더해집니다. `Authorization`, `Cookie`, `Set-Cookie` 같은 인증 header의 값은 가립니다. JSON에는 요청마다 기록이 `hops` metric에 남습니다. `edc doctor -v`도 같은 목록을 보여 줍니다.

`curl -H`처럼 `-H 'Name: value'`로 요청 header를 넣을 수 있고, 여러 번 줄 수 있습니다. `-H 'Host: name'`은 Host header를 바꿉니다. redirect가 다른 host로 가면 그 요청에는 `Authorization`과 `Cookie`가 실리지 않습니다. 요청마다 보낸 header는 `-v`로 확인합니다.

`--body page.html`은 마지막 응답의 본문 전체를 파일로 저장합니다. `--body -`는 본문을 stdout에 쓰고, 결과는 stderr로 보냅니다. stdout이 터미널이면 본문의 제어 문자를 escape해서 씁니다. 본문은 10 MiB까지만 읽고, 그보다 길면 잘랐다고 알려 줍니다. 같은 이름의 파일이 있으면 덮어씁니다. option은 URL 앞이나 뒤에 둘 수 있습니다.

```bash
edc http check http://example.com
edc http check -v -H 'Authorization: Bearer <token>' https://example.com/api
edc http check https://example.com/api --body - | jq .
```

```bash
edc tls check --min-days 14 example.com:443
edc http check --expect-status 200 https://example.com/health
```

실패는 exit code `1`을 돌려줍니다. 이 option을 cron에 넣으면 synthetic check가 됩니다.

![edc tls check가 PASS를 준 뒤, --min-days 90이 인증서 남은 일수를 기준 미달로 판정해 FAIL과 ERROR 블록을 출력하는 화면](media/tls.gif)

![edc http check가 HTTP 200으로 PASS를 준 뒤, --expect-status 404가 기대값 불일치로 FAIL을 출력하는 화면](media/http.gif)

두 데모 모두 통과 한 번과 임계값 실패 한 번을 보여 줍니다.

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

![edc doctor가 네트워크 probe를 실행하고 결과를 요약하는 화면](media/doctor.gif)

DNS·TCP·TLS·HTTP 점검에서 warning이나 실패가 나오면 수동 조사 명령의 템플릿을 순서대로 보여 줍니다. 각 점검 결과는 독립적이며, 표시 순서가 인과관계를 뜻하지는 않습니다. JSON report에는 제안을 넣지 않습니다. 제안한 명령을 실행할 때는 placeholder를 실제 대상으로 바꾸세요.

데모는 기본 언어인 영어입니다. `EDC_LANG=ko`로 한국어를 선택합니다. 데모 소스는 [`docs/tape/`](tape)에 있으며, `vhs docs/tape/doctor.tape`로 다시 만듭니다.

stdin과 stdout이 모두 terminal이면 `edc doctor`는 probe마다 줄 하나를 보여 주고 probe가 끝날 때 그 줄을 갱신합니다. 끝난 줄은 화면에 남습니다. 그 뒤에 상세와 요약이 따라옵니다.

`edc doctor --all-ips example.com`은 DNS가 반환한 IP마다 직접 접속해 TCP·TLS·HTTP 결과를 보여 줍니다. Host와 TLS SNI는 원래 hostname을 유지합니다. 이 HTTP 검사는 응답 header까지만 읽고 redirect를 따라가거나 proxy를 사용하지 않습니다. 공개 IP의 origin ASN과 등록된 ASN holder는 RIPEstat에서 조회하며, 조회 실패는 endpoint 상태에 영향을 주지 않습니다. IP 하나라도 실패하면 `endpoints.check`가 실패합니다. 이 검사로 LB 존재 여부나 서버의 실제 물리적 위치를 확정할 수는 없습니다.

취소하려면 Ctrl-C를 누릅니다. `edc`는 돌던 probe를 멈추고 exit code `4`를 돌려줍니다.

stdin이나 stdout이 terminal이 아니거나, `--json`을 지정했거나, `NO_COLOR`가 설정되면 `edc`는 기다렸다가 마지막에 모든 줄을 출력합니다.


## 네트워크 품질

`quality`는 처리량과 응답성을 측정합니다. macOS에서는 `networkQuality`를, Linux에서는 내장 응답성 측정을 사용합니다. 설정한 서버에 측정용 트래픽을 발생시킵니다.

```bash
edc quality --timeout 60s
edc quality --server https://example.com/.well-known/nq
```

`--server`에는 일반 웹페이지가 아니라 응답성 측정 설정 URL을 지정합니다. 기본값은 Apple의 `https://mensura.cdn-apple.com/api/v1/gm/config`이며, 설정 파일의 `defaults.quality.server`로도 바꿀 수 있습니다. 측정한 경우 download·upload 처리량, RPM 응답성, 기본 RTT를 표시하고, 측정하지 못한 값은 생략합니다.
