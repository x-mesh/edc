[English](remote.md) | **한국어** · [README로 돌아가기](../README.ko.md)

# 원격 작업과 SSH 터널

[Remote recipe](#remote-recipe) · [SSH 터널](#ssh-터널)

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

![edc remote daily --dry-run이 SSH 연결 없이 host x step 계획 표를 출력하고, tags가 맞지 않는 step을 –로 표시하는 화면](media/remote.gif)

데모는 recipe의 tags와 `--dry-run`이 출력하는 계획 표를 보여 줍니다.

### 명령 형태

group은 위치 인자입니다. `--group` flag는 같은 값을 받는 별칭입니다. 둘 중 하나만 씁니다.

```bash
cp examples/remote/inventory.yaml ./inventory.yaml
edc remote              # group을 고른 다음 확인
edc remote daily        # group을 지정한 다음 확인
edc remote daily -f     # group을 지정하고 확인을 생략
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
edc remote daily --dry-run
edc remote daily --dry-run --json -
```

`-l`이나 `--list`를 쓰면 inventory의 group과 host를 출력합니다. group을 지정하면 그 group만 나옵니다.

```bash
edc remote --list
edc remote daily --list --json -
```

`--dry-run`과 `--list`는 `-f`와 함께 쓰지 못합니다. `--redact`를 주면 출력에서 IP 주소를 가립니다.

### 인벤토리와 레시피 편집

터미널에서 `edc setup remote`를 실행하면 한 화면에서 인벤토리와 레시피를 편집할 수 있습니다.

```bash
edc setup remote
edc setup remote --inventory ./.edc/inventory.yaml --recipe ./.edc/recipe.yaml
```

명령은 `edc remote`가 찾는 파일을 엽니다. 파일이 없으면 처음 저장할 때 `./.edc/inventory.yaml`이나 `./.edc/recipe.yaml`을 만듭니다. 다른 파일은 `--inventory`와 `--recipe`로 지정합니다. `defaults.remote.inventory`와 `defaults.remote.recipe` 설정으로도 파일을 지정할 수 있고, 둘 다 있으면 flag가 우선합니다.

화면에는 host 목록과 그룹 목록이 있습니다. `a`로 추가, `e`로 수정, `d`로 삭제하고, `g`나 `h`로 목록을 바꾸며, `s`로 저장하고 `q`로 종료합니다. 그룹 목록에서 `e`를 누르면 스페이스바로 그룹의 host를 고릅니다. `p`는 `parallel`을, `o`는 선택한 그룹의 `parallel`을 설정합니다.

`r`을 누르면 step 목록이 열립니다. `a`로 추가, `e`로 수정, `d`로 삭제하고, `K`와 `J`로 순서를 위아래로 옮깁니다. `n`은 레시피 이름을 지정합니다. step에는 명령이나 업로드 중 하나만 지정합니다.

저장하기 전에 `edc remote`와 같은 규칙으로 결과를 검사합니다. 저장은 바뀐 파일만 씁니다. 저장해도 파일의 주석은 남지만, 이름을 바꾼 host나 step의 주석은 사라집니다. 새 파일의 권한은 `0600`입니다. `edc`가 읽은 뒤에 파일이 바뀌었다면 저장을 멈추고 아무것도 쓰지 않습니다. 저장하지 않은 변경을 버리고 종료하면 종료 코드 `4`를 반환합니다.

비밀번호와 키는 저장하지 않습니다. `target`에는 SSH alias를 쓰십시오.

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
edc remote daily \
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

## SSH 터널

`edc tunnel`은 SSH 터널을 시작, 종료하고 목록으로 보여 줍니다. 로컬 OpenSSH `ssh`를 백그라운드로 실행하므로 OpenSSH 설정, agent, known host 확인을 그대로 사용합니다.

터미널에서 `edc tunnel`을 실행하면 터널 화면이 열립니다. 화면은 터널마다 상태를 보여 주고 2초마다 갱신합니다.

| 키 | 동작 |
|---|---|
| `enter` | 고른 터널을 시작합니다. 실행 중이면 종료합니다. |
| `n` | 새 터널 입력 폼을 엽니다. |
| `r` | 고른 터널을 다시 시작합니다. 저장된 터널에서만 쓸 수 있습니다. |
| `x` | 확인한 뒤 고른 터널의 정의를 삭제합니다. 저장된 터널에서만 쓸 수 있습니다. |
| `q` | 화면을 닫습니다. 실행 중인 터널은 계속 실행됩니다. |

폼에서는 `tab`으로 다음 항목에 이동하고 `←`, `→`로 호스트와 종류를 고릅니다. `ctrl+t`는 연결을 시험하고 `enter`는 터널을 시작합니다. 폼에는 실행될 `ssh` 명령과 지금 선택한 항목의 도움말이 함께 보입니다.

`저장` 항목은 터널을 보관할지 정합니다. 기본값은 `예`이며, `tunnels.yaml`에 정의를 쓰고 저장 경로를 항목 아래에 보여 줍니다. 파일이 없으면 `./.edc/tunnels.yaml`을 새로 만듭니다. `아니오`를 고르면 한 번만 시작하고, 터널을 종료하면 목록에서 사라집니다. 파일에 같은 이름이 있으면 덮어쓰지 않고 막습니다.

자주 쓰는 터널은 `tunnels.yaml`에 정의합니다. `edc`는 `./.edc`, 현재 디렉터리, 설정 디렉터리 순으로 찾고 `--file`로 경로를 지정할 수 있습니다.

```yaml
tunnels:
  db:
    host: jump1                  # inventory의 호스트 이름 또는 ~/.ssh/config 별칭
    local: 5432                  # 포트 또는 auto
    remote: db.internal:5432     # ssh -L
  api-dev:
    host: dev
    reverse: 8080:localhost:3000 # ssh -R, port:host:port
  proxy:
    host: jump1
    socks: 1080                  # ssh -D, 포트 또는 auto
```

터널마다 `remote`, `reverse`, `socks` 중 정확히 하나를 지정해야 합니다. 로컬 포트는 `127.0.0.1`에만 바인딩하고, `auto`는 비어 있는 포트를 고릅니다.

```bash
edc tunnel                                      # 터널 화면, 터미널이 아니면 목록
edc tunnel list --json -                        # 목록을 JSON으로 출력
edc tunnel up db                                # 정의된 터널 시작
edc tunnel up -L auto:db.internal:5432 jump1    # 정의 없이 즉석으로 시작
edc tunnel up -L 5432:db.internal:5432 --name db jump1 --save   # 시작하고 정의도 저장
edc tunnel test db                              # 터널은 시작하지 않고 연결만 시험
edc tunnel down db
edc tunnel down --all
```

### 연결 시험

폼의 `ctrl+t`와 `edc tunnel test <name>`은 같은 항목을 확인하며 터널은 시작하지 않습니다. 확인 순서는 다음과 같습니다.

| 항목 | 방법 |
|---|---|
| 로컬 포트 | `127.0.0.1`에 바인딩해 보고, 같은 포트를 쓰는 실행 중인 터널이 있는지 찾습니다. |
| SSH 로그인 | `BatchMode`로 `ssh host true`를 실행합니다. |
| 목적지 (`remote`) | `ssh -W 목적지 host`를 실행합니다. 호스트가 목적지에 연결하므로 호스트에서 본 결과입니다. |
| 원격 포트 (`reverse`) | `ExitOnForwardFailure`를 켠 `ssh -R`로 호스트가 포트를 열었다가 바로 닫습니다. |
| 로컬 대상 (`reverse`) | 이 컴퓨터의 대상으로 TCP 연결을 열어 봅니다. 실패해도 경고로만 표시합니다. |

SSH 로그인에 실패하면 호스트가 필요한 항목은 건너뜁니다. 항목이 실패하면 `edc tunnel test`는 종료 코드 `1`을 반환합니다. 서비스를 나중에 띄울 수 있으므로, 폼에서는 시험이 실패해도 터널을 시작할 수 있습니다.

목록의 `saved` 열은 정의가 있는 터널을 `●`, 한 번만 실행 중인 터널을 `–`로 표시합니다. 상태는 다음과 같습니다.

| 상태 | 의미 |
|---|---|
| `up` | SSH master 프로세스가 실행 중입니다. |
| `down` | 정의는 있지만 실행 중이 아닙니다. |
| `stale` | 상태 파일은 남아 있지만 SSH 프로세스가 없습니다. 화면에서 `enter`를 누르거나 `edc tunnel down`을 실행하면 정리됩니다. |

`reach` 열은 터널 입구로 TCP 연결을 열어 확인합니다. 목적지가 연결을 유지하면 `ok`, 곧바로 끊으면 `refused`입니다. 역방향 터널은 `-`로 표시합니다.

터널마다 상태 파일 하나를 history 데이터베이스와 같은 위치의 `tunnels` 디렉터리에 둡니다. 비밀번호와 키는 저장하지 않습니다. 터널은 `BatchMode`로 연결하므로 SSH agent나 암호 없는 키를 사용해야 합니다.

