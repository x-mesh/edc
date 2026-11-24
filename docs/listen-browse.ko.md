# Listening socket 탐색

stdin과 stdout이 대화형이고 `TERM`이 `dumb`가 아니면 `edc listen`은 socket 브라우저를 엽니다. pipe에서는 지금의 socket을 한 번 출력하고 끝납니다. 터미널에서 한 번만 출력하려면 `-B`(`--no-browse`)를, 반드시 브라우저를 열려면 `-b`(`--browse`)를 사용합니다. 브라우저는 `r`을 누를 때와, `S`로 시그널을 보낸 1초 뒤에 새로 읽습니다.

```sh
edc listen
edc listen -B
edc listen -b
edc listen -b --tcp
edc listen -b --all
edc listen -b --full
edc listen -b --redact
```

브라우저에는 대화형 stdin과 stdout, UTF-8, `TERM=dumb`가 아닌 터미널, 최소 48열 8행이 필요합니다. 창을 이보다 작게 줄이면 크기 안내가 나오고, `q`와 Ctrl-C로는 그대로 끝낼 수 있습니다. `NO_COLOR`에서도 키보드 이동은 그대로이고, 선택 표시는 색 없는 기호로 나옵니다. 이때 목록에는 ADDRESS 뒤에 SCOPE 열이 생겨 바인딩 범위를 `전체`, `특정`, `loopback`으로 적습니다. 화면 낭독기를 쓴다면 `edc listen -B`나 `edc listen --json -`을 사용합니다.

| 키 | socket 목록 | socket 상세 |
| --- | --- | --- |
| Up/Down, k/j | socket 선택 | 스크롤 |
| PgUp/PgDn | 한 페이지 이동 | 한 페이지 스크롤 |
| Home/End, g/G | 첫·마지막 socket | 처음·끝 |
| Enter | socket과 process 상세 읽기 | — |
| t | `edc top --interactive --process <pid>` 열기 | 선택한 process를 top에서 열기 |
| x | CPU trace 명령 보기 | CPU trace 명령 보기 |
| S | 확인을 받은 뒤 고른 PID에 SIGTERM 보내기 | 확인을 받은 뒤 고른 PID에 SIGTERM 보내기 |
| l | loopback 행 숨기기·보이기 | — |
| p | PID가 둘 이상인 행에서 다음 PID 고르기 | 다음 PID 고르기 |
| r | socket 목록 새로 읽기 | 선택한 socket의 process 상세 다시 읽기 |
| Esc, Backspace | — | 같은 선택과 화면 위치로 돌아가기 |
| q, Ctrl-C | 끝내기 | 끝내기 |

목록은 `edc listen -B`처럼 주소나 PID만 다른 socket을 한 행으로 묶습니다. 머리줄에는 socket 수와 행 수가 나옵니다. ADDRESS 열은 행에 묶인 주소를 보여 줍니다. 색을 쓰면 주소를 모든 주소는 노랑, 특정 주소는 청록, loopback은 회색으로 칠하고, 안내 줄 앞에 이 범례가 나옵니다. port는 굵게, PID는 회색, `→` 뒤의 container는 청록으로 표시합니다. ADDRESS 열은 남는 폭의 절반까지만 쓰고, 더 긴 주소 목록은 잘립니다. 목록 아래쪽에는 주소가 모두 나옵니다. 한 행에 PID가 둘 이상이면 PID 열은 고른 PID와 나머지 PID 수를 `1682348 +1`처럼 보여 줍니다. 처음에는 첫 PID가 골라져 있습니다. `p`는 다음 PID를 고르고, `t`와 `x`는 고른 PID를 씁니다. 목록 아래쪽에는 고른 PID와 그 순서, `p` 키가 `PID 1682348 (1/2) · p 다음 PID`처럼 나오고, 상세 화면에서는 키 안내 줄 아래에 같은 문구가 나옵니다. 새로 읽었을 때 고른 process가 사라졌으면 그 행의 첫 PID를 다시 고릅니다. 목록 아래쪽에는 선택한 행의 주소가 모두 나옵니다.

새로 읽을 때는 프로토콜, port, process 이름, 바인딩 범위, container로 선택한 행을 유지합니다. 그 행이 사라지면 남은 행 중 첫 번째를 선택합니다. `l`은 다시 누를 때까지 loopback 행을 숨기고, 새로 읽어도 숨김을 유지합니다. loopback 행을 선택한 채 숨기면 선택은 같은 위치에 머뭅니다. 새로 읽기에 실패하면 마지막으로 성공한 snapshot을 그대로 두고 오류를 보여 줍니다. 이 snapshot이 지금도 맞다는 뜻은 아닙니다.

Enter는 주소, 소유 PID, socket마다의 수신·송신 queue, 알 수 있을 때는 연결된 container, 그리고 PID마다 `edc proc <pid> -v`와 같은 process 정보를 보여 줍니다. 소유자를 모르거나 process가 끝났어도 socket 상세는 설명과 함께 볼 수 있습니다. process 상세는 필요할 때 읽고, 다시 읽기 전까지 snapshot으로 남습니다. Esc는 socket 목록으로 돌아가며, 목록을 자동으로 새로 읽지 않습니다.

`--full`은 선택한 process의 명령과 작업 디렉터리를 목록 아래쪽에 더합니다. PID가 둘 이상인 행에서는 `p`로 고른 PID의 것을 보여 줍니다. 상세 화면은 긴 내용을 줄바꿈하고 스크롤할 수 있습니다. 아랍어 같은 양방향 문자의 표시는 터미널 에뮬레이터에 따라 다릅니다.

`--browse`는 `--watch`, `--json`, `--interval`/`-i`, `--duration`과 함께 쓸 수 없습니다. socket 변화를 계속 보려면 `edc listen --watch`를 사용합니다. 브라우저는 기존 TCP/UDP/Unix 필터와 한 번 출력·JSON 출력 방식을 바꾸지 않습니다. 바인딩 범위는 socket이 어디에 bind되었는지를 나타낼 뿐, 외부에서 실제로 닿는지를 확인하지는 않습니다.

## 기본값 고르기

`edc setup`을 실행해 listen 항목을 고르거나, 값 하나만 지정합니다.

```sh
edc setup listen.browse auto
edc setup listen.browse always
edc setup listen.browse never
edc setup listen.browse --clear
```

기본값은 `auto`입니다. `always`는 대화형 터미널을 요구하며, pipe나 `TERM=dumb`에서는 오류를 냅니다. `never`는 snapshot 하나를 출력합니다. 설정을 지우면 `auto`로 돌아갑니다.

TOML 설정에서는 다음과 같이 씁니다.

```toml
[defaults.listen]
browse = "auto"
```

이전 YAML 설정도 지원합니다.

```yaml
defaults:
  listen:
    browse: auto
```

`-b`와 `-B`는 저장한 설정보다 우선하며, 둘을 함께 쓸 수 없습니다. `--browse=false`도 한 번 출력을 강제합니다. `--watch`와 JSON 출력은 설정한 브라우저 사용(`always` 포함)보다 우선하고, `-b`를 이 둘 중 하나와 직접 함께 쓰면 오류입니다. `NO_COLOR`는 색만 바꾸고, TUI와 한 번 출력 중 무엇을 쓸지는 바꾸지 않습니다. `--interval`/`--duration`은 브라우저의 새로 읽기 설정이 아닙니다.

process로 이동할 때는 지금 실행 중인 edc 실행 파일을 쓰고, PID를 별도 인자로 넘깁니다. top을 나오면 같은 socket 선택과 상세 위치로 돌아옵니다. 목록은 직접 새로 읽기 전까지 snapshot으로 남습니다. trace 키는 명령을 보여 줄 뿐, 실행하거나 권한을 올리지 않습니다. `--redact`에서는 자식 명령이 가리지 않은 데이터를 몰래 드러내지 않도록 이동을 끕니다.

`top --interactive`는 `NO_COLOR`에서도 터미널 대시보드를 명시적으로 요청합니다. 흑백 표시는 그대로 두고, JSON, 횟수를 정한 batch 출력, pipe, TERM=dumb는 거부합니다. 일반 top의 기본 동작은 바뀌지 않습니다.

process 이동으로 띄운 top은 저장된 top의 횟수와 JSON 기본값을 명시적으로 비웁니다. 그래서 선택한 process는 batch 출력이 아니라 언제나 대시보드로 열립니다.

## process 멈추기

`S`를 누르면 화면 가운데에 확인 상자가 뜨고, 상자가 떠 있는 동안 나머지 화면은 회색으로 옅어집니다. 상자에는 process 이름과 PID, 그 PID가 연 포트가 세 개까지 나옵니다. 포트가 더 있으면 나머지 수를 `+N`으로 적습니다. 기본 답은 아니오입니다. `y`를 누르거나, 왼쪽·오른쪽 방향키나 Tab으로 예를 고른 뒤 Enter를 누르면 SIGTERM을 보냅니다. 그 밖의 키는 모두 취소이므로 이동 키를 잘못 눌러도 시그널이 나가지 않습니다. 창이 48열·8행보다 작아지면 확인을 취소합니다.

PID가 둘 이상인 행에서 `S`는 `p`로 고른 PID 하나만 멈추고, 확인 상자에 그 행에 다른 PID가 있다고 알립니다. worker process를 멈추면 부모 process가 새 worker를 띄울 수 있습니다.

확인 상자를 띄우기 전에 process 이름과 시작 시각을 다시 읽습니다. 이름이 다르거나, 목록을 읽기 시작한 뒤에 시작한 process라면 시그널을 보내지 않습니다. 이때는 `r`로 목록을 다시 읽으세요. Linux에서는 `/proc/<pid>` 디렉터리를 열어 두고 그 fd로 `pidfd_send_signal`을 부르므로, 나중에 같은 PID를 받은 다른 process에는 시그널이 닿지 않습니다. Linux 5.1 이전과 macOS에서는 `kill` 직전에 process를 다시 견주며, 그 확인과 시그널 사이에 짧은 틈이 남습니다.

시그널을 보내고 1초 뒤 socket 목록을 다시 읽어, 그 PID의 socket이 사라졌는지 안내 줄에 알립니다. socket이 남아 있으면 process가 아직 끝나지 않았거나 SIGTERM을 무시한 것입니다. edc는 SIGKILL을 보내지 않습니다.

PID 1과 edc 자신은 멈추지 않습니다. `--redact`에서는 `S`를 쓸 수 없습니다. container가 publish한 포트의 socket은 대개 `docker-proxy`가 엽니다. 이 process를 멈추면 container는 계속 돌고 포트 연결만 끊기므로, 이런 행에서는 `docker stop`을 쓰세요. 다른 사용자의 process를 멈추려면 sudo로 edc를 실행하세요.
