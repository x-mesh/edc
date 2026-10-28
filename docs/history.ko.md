[English](history.md) | **한국어** · [README로 돌아가기](../README.ko.md)

# 기록·이력·report

[장애 snapshot](#장애-snapshot) · [Report 비교](#report-비교) · [Report 뷰어](#report-뷰어) · [Top 저장과 이력 조회](#top-저장과-이력-조회) · [Cron과 애플리케이션 로그](#cron과-애플리케이션-로그)

![Recorded resource trends](media/history.png)

현재 UI 렌더러에 데모 데이터를 넣어 캡처한 화면입니다.

## 장애 snapshot

`edc snapshot`은 이 호스트의 상태를 기본 history DB에 저장합니다. `--output <file>`을 지정하면 JSON 보고서도 저장합니다. 변경이나 장애 전에 한 번, 후에 한 번 찍고 `edc report diff`로 비교합니다.

진단 데이터를 수집해 history DB와 지정한 출력 파일에 저장합니다. 외부로 아무것도 보내지 않으므로 공인 IP도 조회하지 않습니다.

```bash
edc snapshot                              # 기본 history DB에 저장, JSON 파일 없음
sudo edc snapshot --output before.json    # root는 모든 audit와 events 원천을 읽음
sudo edc snapshot --output after.json
sudo edc snapshot --du /var --output after.json   # /var의 edc du 결과도 함께 (Linux와 macOS)
sudo edc snapshot -w --du /var --du /home         # 파일 없이 history DB에 기록
sudo edc report diff before.json after.json
edc snapshot --no-write --since 2h --output -  # DB 기록 없이 JSON만 stdout으로
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
| `du.scan` | `--du <path>`를 줄 때만: 그 경로의 `edc du` 결과와 `report diff`용 큰 디렉터리 목록. 경로를 둘 이상 주면 결과 이름은 `du.scan:<path>`입니다. |

큰 트리에 `--du`를 쓰면 `--timeout`을 늘려야 할 수 있습니다. 제한 시간에 걸리면 `du.scan` 결과는 일부만 센 값이 되고, `report diff`는 그 디렉터리를 비교하지 않습니다.

모든 항목은 `--timeout` 예산(기본 15s) 하나 안에서 동시에 돌아갑니다. 예산에 2초를 더해도 끝나지 않은 항목은 이유와 함께 `skip`이 됩니다. `audit`, `events`, `changes`는 점검마다 `skip`을 하나씩 남기므로 `report diff`가 같은 ID끼리 비교합니다. `info.disk`는 mount 목록을 알 수 없어서 `info.disk` skip 하나로 남습니다. 수집기가 실패하면 오류와 함께 `fail`이 됩니다. 어느 항목도 report에서 빠지지 않습니다.

macOS에서는 `audit`, `events`, `changes`, `info.pressure`가 `skip`으로 나옵니다. Linux에서 root가 아니면 일부 audit와 events 원천이 `pass`가 아니라 `skip`으로 나옵니다.

파일에 계정 이름, sudo 규칙, process 이름이 들어가므로 권한은 0600입니다. 출력 경로에 이미 파일이 있으면 바꾸지 않고, 그 자리의 symlink도 따라가지 않습니다. 이때 exit code는 `2`입니다. 바꾸려면 `--force`를 붙이세요. 그러면 새 0600 파일을 쓴 뒤 그 경로로 옮깁니다.

`--redact`는 host 이름, 홈 경로, IP 주소만 가립니다. 계정 이름, sudo 규칙, process 이름은 report에 그대로 남습니다.

저장 후 DB 또는 파일 경로, 수집 항목별 상태와 저장 데이터를 조회할 명령을 출력합니다. `fail` 결과가 하나라도 있으면 exit code는 `1`이고, 없으면 `0`입니다. 사용법 오류와 쓰기 오류는 `2`를 돌려줍니다.

### 스냅샷을 history DB에 기록

snapshot은 기본으로 [Top 저장과 이력 조회](history.ko.md#top-저장과-이력-조회)의 SQLite DB에 기록합니다. 기존 `--write [path]`(`-w [path]`)도 지원하며 경로를 주면 다른 DB에 저장합니다. `--output`을 지정하면 JSON도 만듭니다. DB 기록 없이 JSON만 만들려면 `--no-write --output <file|->`을 사용합니다. `--no-write`에는 `--output`이 필요하고 `-w` 또는 `--write`와 함께 쓸 수 없습니다. 기록은 직접 삭제할 때까지 유지합니다. 정리 전 확인은 `edc history prune --older-than 90d --dry-run`으로 합니다.

```bash
sudo edc snapshot -w --du /var                              # 기본 DB에 한 번 기록
edc history list                                            # COMMAND 열에 snapshot 또는 top
edc history export --run <run-id> --output before.json      # 스냅샷 파일과 같은 report로 꺼냄
edc report diff before.json after.json
```

매시간 기록하려면 root의 cron에 한 줄을 추가합니다.

```
0 * * * * /usr/local/bin/edc snapshot -w --du /var --timeout 2m >/dev/null
```

결과는 `snapshot_results`에 그대로 남으므로 `history export`가 같은 report를 다시 씁니다. 터미널에서 `--run` 없이 `history export`를 실행하면 최근 스냅샷 50개 중에서 고릅니다. 기간별 조회를 위해 표 두 개를 더 둡니다. `snapshot_metrics`는 결과의 숫자 지표를 `probe`, `key`, `value`로 펼친 표이고, `du_dirs`는 `--du` 경로마다 큰 디렉터리를 `root`, `path`, `bytes`로 남긴 표입니다. 스냅샷마다 계정 이름, sudo 규칙, process 이름이 DB에 쌓입니다. `--redact`는 파일과 같이 기록에도 적용됩니다.

## Report 비교

`edc report`는 edc 진단 명령이 `--json`으로 저장한 **edc 진단 결과**를 읽고 비교합니다. `doctor`뿐 아니라 같은 report 형식을 사용하는 다른 edc 진단 명령의 결과도 지원합니다. 일반 JSON 파일이나 `report diff`가 생성한 비교 JSON은 지원하지 않습니다. `edc log`가 저장한 명령 출력 텍스트와는 별개입니다. `edc report list [directory]`는 지정한 디렉터리(기본값: 현재 디렉터리)의 유효한 JSON report를 실행 시각이 최신인 순서로 나열하며, 실행 시각과 PASS/WARN/FAIL/SKIP 개수를 보여 줍니다. 하위 디렉터리는 검색하지 않습니다.

```bash
edc doctor --json report.json https://example.com
edc report list
edc report show report.json
```

경로 없이 `edc report show`나 `edc report diff`를 실행하면 최근 report 최대 20개 중에서 선택하거나 다른 JSON 파일 경로를 직접 입력할 수 있습니다. report가 없으면 저장 예시를 안내합니다. `edc report diff`는 report를 한 화면에 보여 주고 가장 최근 두 개를 미리 골라 둡니다. Space로 선택을 바꾸고 Enter로 비교합니다. 시간이 이른 쪽이 언제나 이전이 됩니다. 반대 순서로 비교하려면 두 경로를 직접 주세요.

`edc report diff`는 두 JSON report를 probe 이름으로 맞춰 비교합니다. probe마다 status 변화와 scalar metric 차이를 보여 줍니다.

```bash
edc doctor --json before.json https://example.com
edc doctor --json after.json https://example.com
edc report diff before.json after.json
edc report diff --json diff.json before.json after.json
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

![edc report show 뷰어에서 f로 필터를 바꾸고 e로 상세를 펼친 뒤, edc report diff가 probe별 duration_ms 차이를 보여 주는 화면](media/report.gif)

데모는 뷰어를 열어 필터를 바꾸고 상세를 펼친 다음, 두 report를 비교합니다.

## Top 저장과 이력 조회

`--write [path]` 또는 `-w [path]`로 관측값을 로컬 SQLite DB에 누적합니다. 대시보드는 유지하며, `--count`, 파이프, `--json`의 출력 방식은 그대로입니다. `--write`와 함께 쓰면 표에서도 `--process`를 사용할 수 있습니다. `-w`만 주면 Linux는 `~/.local/state/edc/history.db`, macOS는 `~/Library/Application Support/edc/history.db`를 사용합니다. 두 플랫폼 모두 절대 경로인 `XDG_STATE_HOME`이 있으면 `$XDG_STATE_HOME/edc/history.db`를 사용합니다. 기본 디렉터리는 권한 `0700`으로 자동 생성합니다. `-w`나 `--write`를 생략하면 기록을 끈 상태로 시작합니다. 대시보드에서 `w`로 기록을 시작·중지할 수 있습니다. 시작 시점 이후의 온전한 관측 구간만 저장하며, 다시 시작하면 같은 DB에 새 실행을 만듭니다. `p`로 일시정지하면 기록도 멈춥니다. 기록 중에는 `REC`와 저장한 관측값 개수가 표시됩니다.


터미널에서 `edc history top`과 `edc history process`는 인터랙티브 탐색 화면을 엽니다. ↑↓ 선택, Enter 전체 상세, Home 최신 관측 추적, `p` 자동 갱신 일시정지를 지원합니다. top 표는 너비에 따라 디스크·네트워크·swap 등 지원되는 지표의 컬럼을 추가하고, 넓은 화면에서는 선택한 관측값의 상세 패널을 표시합니다. top의 파이프 출력은 디스크·네트워크·swap 컬럼을 포함하는 표이며, `--json`은 기존 JSONL 출력입니다. process 탐색 화면은 PID·CPU·RSS·명령어를 기본으로 표시하고 공간과 기록된 데이터에 따라 MEM·디스크 I/O·thread·FD 컬럼을 추가합니다. 상세에는 process 시작 시각과 수집된 한도도 표시합니다. 기존 `--process`, `--run`, 시간 범위와 지표 필터를 그대로 사용할 수 있습니다.

```bash
edc top -w
edc history list
edc top -w incident.db
edc top --process nginx --count 60 -w incident.db
edc top --count 10 -w incident.db --json samples.jsonl
edc history list incident.db
edc history top --metric memory_pct --min 90 incident.db
edc history top --from 2026-10-05T09:00:00+09:00 --to 2026-10-05T10:00:00+09:00 incident.db
edc history process --process nginx --metric cpu_pct --min 100 incident.db
edc history top --run <run-id> --limit 0 --json - incident.db
edc history trend --metric memory_pct --by hour incident.db
edc history trend --probe info.disk:/ --metric used_bytes --by day
edc history growth --root /var
edc top --process postgres -w
edc history trend --process postgres --metric rss_bytes --by day
edc history trend --process postgres --metric cpu_pct --each
edc history growth --process postgres
edc history prune --older-than 90d --dry-run
edc history prune --older-than 90d --command snapshot --yes --vacuum
```

같은 DB를 지정해도 이전 값을 덮어쓰지 않고 별도 실행으로 추가합니다. 실행마다 host 정보, edc version, 수집 조건, 시작·종료 시각, 상태, sample 수를 남깁니다. `unfinished`는 종료 기록이 없는 실행으로, 실행 중이거나 강제 종료된 상태일 수 있습니다. 정상 종료는 대기 중인 기록을 모두 저장하고, 저장 실패는 관측을 중단하며 종료 코드 `1`을 반환합니다. 강제 종료 시 이미 commit된 transaction은 보존됩니다. 새 DB 권한은 `0600`이며 자동으로 지우지 않습니다. 오래된 실행은 `history prune`으로 지웁니다.

host 지표와 core별 CPU, 지원 여부, 실제 관측 구간, 당시 process 필터를 저장합니다. 필터가 없으면 CPU 상위 5개와 `MEM` 상위 5개의 합집합(최대 10개), 필터가 있으면 최대 50개와 전체 합계를 저장합니다. 전체 process를 기록하는 기능은 아닙니다. `p`로 일시정지하면 기록도 멈추고, 재개 시 새 기준점 다음부터 rate를 기록합니다. process는 host보다 덜 자주 갱신될 수 있으며 `process_observed_at`으로 수집 시각을 구분합니다. 시작 시각을 읽을 수 있으면 PID와 정밀한 시작 시각으로 PID 재사용을 구분합니다.

`history list`는 실행 목록, `history top`과 `history process`는 sample을 최신순으로 보여 줍니다. DB 경로를 생략하면 같은 기본 DB를 읽으며, 없는 DB를 생성하지는 않습니다. `--run`은 실행 ID, `--from`과 `--to`는 RFC3339 시각으로 범위를 좁힙니다. 시작 시각은 포함하고 종료 시각은 제외합니다. `--limit`은 기본 200이며 `0`은 전체입니다. `--json <path|->`는 결과당 JSON 한 줄입니다. Go flag 규칙에 따라 옵션은 DB 경로 앞에 둡니다.

`top`과 `process` 조회는 `--metric <field>`와 `--min`, `--max`를 지원하며 경계값을 포함합니다. `memory_pct`, `disk_await_ms`, `rss_bytes`, `disk_read_bytes_per_s`처럼 JSON 최상위 숫자 필드 이름을 씁니다. `cpu_pct`는 host에서는 user+system 합, process에서는 기존의 core 하나가 100%인 값입니다. 미지원·미측정 SQL 값은 NULL로 저장하여 숫자 조건에 맞지 않게 하고, JSON에는 지원 상태를 보존합니다. 이름·PID 검색은 기존 `top --process` 규칙을 따릅니다.

`trend`는 지표 하나의 값을 `--by hour`, `day`(기본값), `week` 구간으로 묶습니다. 구간은 로컬 시간의 정시, 자정, 월요일 자정에서 시작합니다. 행마다 개수, 최솟값, 평균, 95번째 백분위수(nearest rank), 최댓값을 보이고, 오래된 구간부터 나열합니다. 값이 없는 구간은 보이지 않습니다. `--probe`가 없으면 `--metric`은 top의 host 지표이고, `--probe`를 주면 그 스냅샷 결과의 숫자 지표입니다. 예를 들어 `info.disk:/`의 `used_bytes`, `info.memory`의 `used_percent`, `du.scan`의 `bytes`를 볼 수 있습니다. 없는 결과나 지표를 주면 기록된 이름을 보여 줍니다. 터미널에서 `--metric` 없이 `trend`를 실행하면 탐색 화면이 열립니다. 왼쪽에서 호스트, 기록된 프로세스, 스냅샷 결과 가운데 대상을 고르면, 오른쪽에 구간 표와 구간마다 평균을 나타내는 막대가 보입니다. `m`은 지표를, `b`는 구간을 바꾸고, `e`는 프로세스마다 한 줄씩 보여 줍니다. 프로세스를 고르면 표 아래에 최근 7일 동안의 하루 RSS 변화와 누수 의심이 `growth --process`와 같은 기준으로 나옵니다. 화면을 끝내면 마지막으로 본 표를 내는 명령을 보여 줍니다. `NO_COLOR`를 설정했거나 `--json <file>`을 주면 탐색 화면 대신 기록, 대상, 지표를 차례로 묻습니다. `--run`, `--from`, `--to`, `--json`은 `top`과 같게 동작합니다.

`trend --process <filter>`는 top 기록의 프로세스 표본을 씁니다. 필터 규칙은 `top --process`와 같습니다. `--metric`에는 프로세스 지표인 `cpu_pct`, `rss_bytes`, `threads`, `fds`, `disk_read_bytes_per_s`, `disk_write_bytes_per_s` 가운데 하나를 지정합니다. 표본마다 일치하는 프로세스의 값을 모두 더하므로, worker가 여럿인 서비스도 합계 하나로 보입니다. `--each`를 주면 PID와 시작 시각으로 구분한 프로세스마다 한 줄씩 보여 줍니다. `SEEN`은 프로세스가 기록된 표본 가운데 일치한 프로세스가 있던 비율입니다. `PROCS`는 구간에 나타난 서로 다른 프로세스 수이고, `RESTARTS`는 일치하던 프로세스가 사라진 자리를 새 프로세스가 대신한 횟수입니다. 종료와 시작이 서로 다른 표본에 잡혀도 셉니다. 사라진 프로세스 없이 새 프로세스만 늘었다면 재시작으로 세지 않습니다. 필터 없이 기록한 표본에서는 프로세스가 상위 목록에서 밀려나기만 해도 사라진 것처럼 보이므로, 그런 표본이 섞인 구간의 `RESTARTS`는 `-`로 표시합니다. `--process` 없이 기록한 실행의 표본이 섞여 있으면 stderr에 안내를 씁니다. 모든 표본을 남기려면 `top -w --process <filter>`로 기록하세요.

터미널에서 `history growth`는 ↑↓ 선택, Enter 상세, 자동 갱신, `r` 새로고침, `p` 일시정지를 제공하는 탐색 화면을 엽니다. 디스크, 저장한 du 루트와 변경된 디렉터리를 표시하며 `--process`는 process RSS 증가량을 표시합니다. 넓은 화면에는 컬럼과 상세 패널을 추가합니다. 파이프와 `--json`의 텍스트·JSONL 출력은 유지합니다.

`growth`는 최근 7일, 또는 `--from`과 `--to` 범위의 스냅샷을 읽습니다. 마운트마다 마지막 스냅샷의 사용량과 전체 크기, 그리고 `used_bytes`의 최소제곱 기울기로 구한 하루 증가량을 보여 줍니다. 사용량이 늘고 있으면 그 속도로 가득 차기까지 걸리는 시간을 추정합니다. 직선으로 구한 추정치이며 예측을 보장하지 않습니다. 스냅샷이 3개보다 적거나 첫 스냅샷과 마지막 스냅샷의 간격이 1시간보다 짧으면 추정하지 않습니다. `--du` 경로마다 범위 안의 첫 번째와 마지막 완전한 스캔을 `report diff`와 같은 규칙으로 비교해, 크게 바뀐 디렉터리를 최대 10개 보여 줍니다. 범위가 1일 이상이면 하루 증가량도 함께 씁니다. 그 위의 줄은 두 스캔에서 경로 전체의 크기를 보여 주므로, 경로 바로 아래 파일의 변화도 여기에 반영됩니다. `--root <path>`는 그 경로의 디렉터리만 보여 주고, `--json`은 마운트와 경로마다 한 줄씩 씁니다.

`growth --process <filter>`는 스냅샷 대신 top 기록을 읽습니다. 일치하는 프로세스마다 마지막 RSS와, `rss_bytes`의 최소제곱 기울기로 구한 하루 변화량을 보여 줍니다. 표본이 6시간 이상에 걸쳐 12개 이상 있고, RSS가 첫 값보다 10% 이상 늘었고, 직선 적합도(R²)가 0.7 이상이면 누수 의심으로 표시합니다. 누수 의심은 살펴볼 이유일 뿐 진단이 아닙니다. 천천히 채워지는 캐시도 같은 모양을 보일 수 있습니다. 표는 누수 의심을 먼저 두고 최대 20개 프로세스를 보여 주며, `--json`은 프로세스마다 한 줄씩 씁니다. `--process`와 `--root`는 함께 쓸 수 없습니다.

터미널에서 `history prune`을 기간 없이 실행하면 마지막 활동이 90일 이전인 실행을 미리 봅니다. 다른 기간은 `--older-than`으로 지정합니다. 현재 기간과 후보의 마지막 활동, 호스트, 관측값 수를 표시합니다. `t`로 1h·1d·7d·30d·90d·180d·365d를 고르거나 `12h`, `2w`처럼 직접 입력할 수 있습니다. 기간을 바꾸면 후보를 다시 조회하고 삭제 시에도 선택한 기간으로 조건을 재확인합니다. Space로 체크, `a`로 전체 전환, Enter로 상세를 봅니다. `d` 다음 `y`로 체크한 실행 삭제를 확인하고, `q`는 취소합니다. `--dry-run`은 삭제가 비활성인 미리보기를 열고, `--yes`는 기간을 명시해 기존 명령 출력으로 실행합니다. 삭제 직전 같은 transaction에서 기간 조건을 다시 확인하며 최근 활동이 생긴 실행은 제외합니다.

`prune --older-than <기간>`은 마지막 활동이 그 기간보다 오래된 실행을 표본·스냅샷 행과 함께 한 transaction으로 지웁니다. 마지막 활동은 시작, 종료, 마지막 표본 중 가장 늦은 시각이라, 오래전에 시작해 아직 기록 중인 `top -w`는 남습니다. 기간에는 Go duration과 `d`, `w` 단위를 씁니다(예: `90d`). `--command top`이나 `--command snapshot`으로 대상을 좁힙니다. 먼저 지울 실행과 표본 수를 보여 주고, 터미널에서는 삭제를 확인한 뒤에만 지우며, 터미널이 없으면 `--yes`나 `--dry-run`이 필요합니다. `--dry-run`은 개수만 세고 파일을 바꾸지 않습니다. 지운 행의 자리는 파일 안에 빈 page로 남아 새 기록에 다시 쓰이며, 그 크기를 알려 줍니다. `--vacuum`은 파일을 다시 써서 공간을 돌려받습니다. DB 크기만큼 여유 공간이 필요하고, 그동안 다른 기록은 기다립니다. 없는 DB는 만들지 않습니다.

WAL을 사용하므로 기록 중에도 조회할 수 있습니다. DB는 로컬 파일시스템에 둡니다. 외부 SQLite 도구로 `runs`, `top_samples`, `top_process_samples`도 조회할 수 있습니다. 숫자 지표는 column, cgroup·eBPF 등 상세 정보는 `payload` JSON에 보존합니다. 스냅샷 기록은 `snapshot_results`, `snapshot_metrics`, `du_dirs`에 들어갑니다. 조회는 기존 DB를 읽으며 생성이나 migration을 하지 않습니다. `--dry-run` 없는 `prune`만 DB에 씁니다. 다른 DB와 지원하지 않는 schema version은 거부합니다.

DB schema는 버전 1을 유지합니다. `top -w`나 `snapshot -w`가 스냅샷 표가 없는 DB를 열면 그 표만 추가하고 다른 것은 바꾸지 않습니다. top 기록은 그대로 남고, 이전 edc도 스냅샷 표를 무시한 채 같은 DB를 계속 읽고 씁니다. JSON 출력은 DB나 journal 파일을 덮어쓸 수 없습니다.

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
edc log history --follow
edc log history --follow --json - --command make
```

`history ls`는 인자 없는 `ls`의 이력을 바로 출력합니다. `history ls -l /tmp`는 정확히 같은 argv의 이력을 조회합니다. `history --command ls`는 `ls`의 인자 조합별 키 목록을 보여 줍니다. 첫 명령 토큰 전까지 history 옵션을 해석하고, 이후 `--failed`나 `--`를 포함한 모든 토큰은 argv로 보존합니다. 명령 앞의 선택적 `--`는 옵션 해석만 끝내며 조회 의미를 바꾸지 않습니다. `--command`, `--key`, 직접 argv 조회는 함께 지정할 수 없습니다. 조회는 명령을 다시 실행하지 않습니다. 인자 순서와 경계를 보존하므로 `echo "a b"`와 `echo a b`는 다른 키입니다. 동일 argv를 다른 디렉터리에서 실행한 경우 키는 같지만 통계는 작업 디렉터리별로 구분합니다.

실행별 시작 시각, 소요 시간, 성공·실패·시간 초과·시그널 종료·오류·완료 불명, exit code와 재시도 번호를 표시합니다. 기본 최근 20건이며 `--limit 1`부터 `--limit 1000`까지 지정할 수 있습니다. 실행 소요 시간에는 출력 배출과 프로세스 정리가 포함됩니다. 요약은 표시한 실행 기준이며 성공한 실행만 평균·최소·최대 시간에 포함합니다. 종료 기록이 없는 항목은 실패로 단정하지 않으며 `--failed`에서도 제외합니다.

실행마다 자원 사용량도 종료 표식에 기록합니다. 끝날 때는 자식 프로세스의 rusage에서 user·system CPU 시간과 가장 큰 단일 프로세스의 상주 메모리를 읽습니다. 실행하는 동안에는 시작 0.2초 뒤와 그 뒤 1초마다 프로세스 그룹을 표본으로 읽어, 그룹 전체의 메모리 최고값과 평균(Linux는 RSS와 swap, macOS는 footprint), CPU 최고 사용률, 디스크 읽기·쓰기 바이트를 기록합니다. Linux에서 `docker run`과 `docker exec`는 컨테이너 안을 잽니다. `docker run`은 실행 중에 새로 생긴 컨테이너의 cgroup을, `docker exec`는 그 컨테이너에서 argv가 같은 프로세스를 따라갑니다. 그 프로세스를 찾지 못하면 컨테이너 전체를 기록하고 측정 범위를 `container`로 표시합니다. 컨테이너 안의 프로세스는 edc의 자식이 아니라서 rusage를 읽을 수 없습니다. 그래서 마지막 표본 뒤의 CPU 시간은 빠지고, 0.2초보다 짧은 실행은 자원 값을 남기지 않습니다.

실행 목록에는 메모리 최고값을 표시하고, 터미널 폭이 84칸 이상이면 CPU 시간과 디스크 바이트도 표시합니다. CPU 시간이 중앙값의 2배 이상이어도 노랑으로 칠합니다. 소요 시간도 같은 기준을 씁니다. 실행 중인 항목은 경과 시간으로 비교하므로 끝나기 전에도 노랑으로 바뀌고, 키 목록에는 `실행 중 · 지연`으로 표시합니다. 실행 상세에는 CPU 시간, 평균·최고 CPU, 메모리, 디스크 바이트와 측정 범위를 표시합니다. 같은 키에 같은 디렉터리의 이전 성공 실행이 3건 이상 있으면 그 중앙값 대비 배율도 표시하며, 메모리가 2배 이상이면 노랑으로 칠합니다. 표본 값은 근사치입니다. 두 표본 사이에 시작하고 끝난 프로세스는 보이지 않고, 공유 메모리는 프로세스마다 따로 더해지며, `setsid`로 프로세스 그룹을 벗어난 프로세스는 세지 않습니다.

이력 화면은 2초마다 로그를 다시 읽습니다. r은 즉시 갱신하고 p는 갱신을 멈추거나 다시 시작합니다. 로그 잠금을 잡고 있는 실행은 실행 중과 경과 시간으로 표시하며 실패로 세지 않습니다.

`edc history log`도 같은 화면을 열고 같은 옵션을 받으므로, `top -w` 기록과 같은 자리에서도 실행 이력을 볼 수 있습니다.

d로 지울 수 있습니다. 명령 목록에서는 고른 명령의 실행을 모두, 실행 목록에서는 고른 실행 하나를 지웁니다. 지울 실행 수, 파일 수, 크기를 보여 주고 y를 누를 때만 지웁니다. edc는 로그 파일을 `.edc.N` 순환 파일과 잠금 파일까지 통째로 지우며, 실행 하나를 빼려고 파일을 고쳐 쓰지 않습니다. 그래서 고르지 않은 실행이 같은 파일에 있으면(예: `--output`으로 같이 쓴 파일) 그 파일은 남고, 실행 중인 항목도 남습니다. 지우기 직전에 파일을 다시 읽어, 화면이 읽은 뒤 바뀐 파일은 지우지 않습니다. 기본 로그 디렉터리 아래의 명령 폴더가 비면 함께 지웁니다.

`edc log prune --older-than <기간>`은 그 기간보다 먼저 시작한 종료된 실행을 같은 규칙으로 지웁니다. 기간에는 Go duration과 `d`, `w` 단위를 쓸 수 있습니다(예: `30d`, `2w`). `--command`와 `--key`로 대상을 좁히고, `--dir`나 `--file`로 로그 위치를 정합니다. `--dir`이나 `--file`을 쓰면 prune은 오래된 로그부터 읽으므로, 파일 한도를 넘는 폴더에서도 오래된 실행까지 닿습니다. `--dry-run`은 지우지 않고 지울 대상만 보여 줍니다. 명령마다 한 줄로 실행 수, 처음과 마지막 시작 날짜, 크기, 폴더를 적고, 로그 디렉터리는 첫 줄에 한 번만 적습니다. `-v`를 붙이면 파일을 모두 나열합니다. 터미널이 없으면 `--yes`가 있을 때만 지웁니다.

```bash
edc log prune --older-than 30d --dry-run
edc log prune --older-than 2w --command make --yes
```

`--follow`는 실행이 시작할 때와 끝날 때 한 줄씩 출력합니다. 시작하면 지금 실행 중인 항목부터 출력합니다. `--command`, `--key`, argv, `--failed`, `--dir`, `--file` 조건을 그대로 적용하고 `--limit`는 무시합니다. `--json <path|->`는 `start`, `end`, `slow`, `error`, `summary` 이벤트를 JSONL로 기록합니다. `end` 이벤트에는 중앙값 대비 `duration_ratio`, `memory_ratio`, `cpu_ratio`가 있고, 2배 이상인 항목을 `alerts`에 적습니다. 실행 중인 항목이 평소 소요 시간의 2배를 넘으면 `slow` 이벤트를 한 번 냅니다. 텍스트 모드의 상태 줄은 stderr로 출력합니다. Ctrl-C로 끝냅니다.

기본 로그 디렉터리와 하위 한 단계의 `.log` 파일, 설정한 `defaults.log.output`을 조회하며 회전 조각도 함께 읽습니다. 임의 위치에 저장한 로그는 `--file` 또는 `--dir`로 지정합니다. 조회는 최대 1,000개 파일 묶음, 200,000개 디렉터리 항목, 128 MiB, 10,000개 실행 시도로 제한하며, 한도에 걸리면 수정 시각이 최신인 파일부터 남기고 오래된 것을 뺍니다.

기본 로그 디렉터리에는 실행 메타데이터 인덱스 `.edc-index.db`(권한 `0600`)를 둡니다. 이력 화면, `--follow`, prune은 이 인덱스를 읽으므로 위의 파일 한도에 묶이지 않고 최대 100,000건까지 보여 주며, 읽을 때마다 바뀐 로그만 다시 파싱합니다. `edc log` 자체는 인덱스를 열지 않습니다. 원본은 로그 파일이고 인덱스는 캐시입니다. 인덱스를 쓸 수 없으면 안내를 한 번 출력하고 로그 파일을 직접 읽습니다. 잠금이 0.2초 넘게 풀리지 않을 때, 손상되었을 때, 다른 버전의 형식일 때, 디렉터리가 읽기 전용일 때, 처음 만드는 데 1.5초가 넘게 걸릴 때가 여기에 해당하며, 종료 코드는 바뀌지 않습니다. 손상된 인덱스는 `.edc-index.db.corrupt-<시각>`으로 옮기고 다시 만듭니다. `--dir`과 `--file`은 항상 파일을 직접 읽습니다.

실행 목록과 `--key` 출력에는 그 명령의 추세 줄이 있습니다. 최근 14일을 하루 한 칸의 막대로 보여 주고, 실행 수, 실패 수, 성공한 실행의 평균 소요 시간을 함께 적습니다. 기본 로그 디렉터리에서 prune이 실행을 지우면, 실제로 지운 실행을 `.edc-stats.db`(권한 `0600`)의 일 단위 요약에 더합니다(실행 수, 실패 수, 소요 시간 합과 최대, 최대 메모리, CPU 시간). 요약은 지운 뒤에 쓰므로, 바뀌어서 남긴 로그가 두 번 세어지지 않습니다. 대신 두 단계 사이에 edc가 멈추면 그 prune의 요약은 남지 않습니다. 추세는 이 요약과 아직 로그에 남은 실행을 합쳐서 계산하므로 prune 뒤에도 빠짐없이 보입니다. `d`는 요약을 남기지 않고 지웁니다. 요약은 인덱스와 달리 로그로 다시 만들 수 없으므로 edc가 지우거나 바꾸지 않습니다. 요약을 쓰지 못하면 prune은 경고만 하고 그대로 지우고, 다른 버전의 형식이거나 손상되었으면 안내를 한 번 출력하고 추세에서 보관된 날짜만 뺍니다. 한도 초과·읽는 동안 변경·손상된 기록은 안내와 함께 부분 결과를 반환하고 exit code `2`를 돌려줍니다. 과거 실패가 있어도 정상적인 조회는 `0`입니다.

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


## 코드와 파일 경로 검색

터미널에서 `edc grep ValidateToken`은 현재 디렉터리의 코드 내용에서 경로와 줄 번호를 출력합니다. `edc grep -f auth`(또는 `--files`)는 파일 경로를 찾고, `--code`는 코드 검색을 명시적으로 선택합니다. 두 검색 모두 현재 디렉터리를 기본으로 하고 저장소와 하위 디렉터리의 `.gitignore`를 적용하며 숨김 항목과 `.git`을 제외합니다. `--hidden`은 숨김 항목을 포함하고 `--no-ignore`는 `.gitignore`를 무시합니다. `.git`은 계속 제외합니다. 직접 지정한 파일은 그대로 검색합니다.

`-i`는 대소문자를 무시하고 `-E`는 정규식을 사용합니다. `--files`는 각 검색 루트의 상대 경로에 일치시키며, `--code -l`은 내용이 일치한 파일을 나열합니다. 코드 검색은 로그 index 없이 현재 파일을 읽습니다. 종료 코드는 일치 0, 일치 없음 1, 오류 2입니다.

파이프나 리다이렉트된 stdin에서는 패턴만 주면 stdin을 읽으며, 경로 또는 로그 전용 옵션을 지정하면 기존 로그 검색을 사용합니다.

### 코드 키워드 순위 검색

`edc grep --ask --keyword 'remote inventory config'`는 질의에 쓰인 단어를 기준으로 로컬 코드 조각을 순위화합니다. 키워드 검색이므로 코드와 다른 표현을 쓴 질문, 특히 영문 식별자를 한국어로 묻는 경우 관련 결과를 놓칠 수 있습니다. Go 선언은 최대 32줄로 나누고 다른 텍스트는 32줄씩 나눕니다. 결과에는 경로, 조각의 줄 범위와 관련된 최대 세 줄을 출력합니다.

검색 범위는 `--code`와 같은 `.gitignore`, 숨김 파일, 명시 경로 규칙을 따릅니다. 네트워크를 사용하거나 index·cache를 만들지 않습니다. `--limit N`은 최대 100개 결과를 선택하며 기본값은 10개입니다. `--json`은 `method`, 점수를 포함한 `results`, `skipped_files`를 출력합니다. binary, 잘못된 UTF-8, 2 MiB 초과 파일은 건너뜁니다. 입력 32 MiB 또는 코드 조각 50,000개에 도달하면 불완전한 결과임을 stderr에 알리고 종료 코드 2를 반환합니다. 키워드가 일치하지 않으면 종료 코드 1입니다.

저장소 검색 평가: `EDC_EVAL_GREP=1 go test ./internal/edc -run '^TestGrepAskEvaluation$' -count=1 -v`. 질문은 `internal/edc/testdata/grep-ask/queries.json`에 있습니다.

개발용 오프라인 embedding 비교에만 `sentence-transformers`와 선택한 모델을 미리 설치한 뒤 다음을 실행합니다. edc 실행에는 Python이 필요하지 않습니다.

```sh
EDC_EVAL_GREP=1 EDC_EVAL_DUMP=/tmp/edc-grep-corpus.json EDC_EVAL_SCORES=/tmp/edc-grep-scores.json go test ./internal/edc -run '^TestGrepAskEvaluation$' -count=1 -v
python3 scripts/eval-grep-embedding.py /tmp/edc-grep-corpus.json internal/edc/testdata/grep-ask/queries.json --keyword-scores /tmp/edc-grep-scores.json
```

내보낸 파일에는 로컬 소스가 들어 있으므로 실험 후 지웁니다. 평가 스크립트는 캐시된 모델만 사용합니다. `--ask` 질의는 최대 32단어입니다.

### 로컬 semantic 검색

고정된 출처는 [MiniLM 모델](https://huggingface.co/sentence-transformers/all-MiniLM-L6-v2/tree/c9745ed1d9f207416be6d2e6f8de32d1f16199bf)과 [ONNX Runtime 1.23.2](https://github.com/microsoft/onnxruntime/releases/tag/v1.23.2)입니다. runtime 라이선스 안내는 모델 cache에 보관합니다.

semantic 검색은 영문 `all-MiniLM-L6-v2` 모델과 native ONNX Runtime을 사용하며 edc 실행에 Python이 필요하지 않습니다. 처음 초기화할 때 다운로드 크기와 경로를 보여 주고 확인을 받은 뒤 Hugging Face와 Microsoft에서 고정된 모델과 runtime을 받습니다. 플랫폼에 따라 다운로드는 약 94–128 MiB이며, 압축을 푼 파일과 index에는 추가 공간이 필요합니다. 설치 전에 고정된 SHA-256을 검증합니다.

```sh
edc grep --semantic-init
edc grep --ask 'Where is authentication configuration loaded?'
edc grep --semantic 'Where is authentication configuration loaded?'
edc grep --semantic-status
edc grep --semantic-files
edc grep --semantic-clear
edc grep --model-clear
```

`--semantic`은 `--ask`와 같습니다. `--ask`는 기본으로 semantic 검색을 사용합니다. `--semantic-init --yes`는 터미널 확인 없이 다운로드를 명시적으로 허용합니다. 모델과 runtime은 사용자 cache에 공유되며 프로젝트 index는 초기화할 때만 만듭니다. 다른 프로젝트에서는 모델을 재사용하고 index만 새로 만듭니다. `--semantic-clear`는 현재 프로젝트 index만, `--model-clear`는 공유 모델과 runtime만 삭제합니다. 작은 lock 파일은 남을 수 있습니다. `--semantic-status`는 모델과 저장된 index 상태를 보여 주며 파일 변경 여부는 검사하지 않습니다. `--semantic-files`는 현재 파일을 다시 읽어 파일마다 `index에 든 청크/전체 청크`와 경로를 출력합니다. 경로에 탭, 줄바꿈 같은 제어 문자나 큰따옴표가 있으면 Go 문자열 형식으로 따옴표를 붙여 출력합니다. 두 수가 다른 파일은 다음 검색 때 다시 벡터화합니다.

디렉터리를 검색할 때 생성 파일, lock 파일, 테스트 fixture는 index에 넣지 않고 순위에서도 뺍니다. 생성 파일은 첫 줄 주석에 `Code generated ... DO NOT EDIT` 또는 `auto-generated`가 있는 파일입니다. lock 파일은 `go.sum`, `package-lock.json`, `yarn.lock`, `Cargo.lock` 등입니다. 테스트 fixture는 `testdata/` 아래 파일이며, `.md`와 `.txt`는 남깁니다. 이런 파일도 경로로 직접 지정하면 검색합니다.

검색은 오프라인으로 실행합니다. 현재 파일을 확인해 변경된 조각만 다시 벡터화하며, 변경이 없으면 index를 다시 쓰지 않습니다. 벡터화 중 파일이 바뀌면 오래된 결과를 저장하지 않고 오류를 반환합니다. `--semantic-init`은 이전 다국어 모델로 만들었거나 이전 형식으로 저장한 index도 다시 만듭니다. index는 벡터를 16비트 실수로 저장합니다. 실패하거나 중단된 다운로드는 사용 가능한 모델로 취급하지 않습니다. 검색 질의는 영어를 사용합니다. 에이전트가 사용자 질문을 영어로 바꿔 호출할 수 있으며, 한국어로만 된 주석·문서는 영문 모델이 놓칠 수 있습니다.

점수는 cosine similarity 95%와 정규화된 키워드 점수 5%를 합친 값이며 확률은 아닙니다. 조각마다 최대 3,000자와 모델 토큰 128개를 벡터화합니다. 파일 크기와 전체 검색 한도는 키워드 검색과 같습니다. Linux·macOS의 amd64·arm64를 지원합니다. 공식 Linux runtime은 glibc와 libstdc++가 필요하므로 Alpine 같은 musl 전용 환경은 호환 라이브러리 없이 지원하지 않습니다.
