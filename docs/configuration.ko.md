[English](configuration.md) | **한국어** · [README로 돌아가기](../README.ko.md)

# 설치와 설정

[설치](#설치) · [업데이트](#업데이트) · [빌드](#빌드) · [언어](#언어) · [Command 기본값](#command-기본값) · [Shell completion](#shell-completion)

## 설치

script로 최신 release를 설치합니다. script는 운영체제와 architecture를 읽고, SHA-256을 확인한 다음 실행 파일을 설치합니다.

```bash
curl -fsSL https://raw.githubusercontent.com/x-mesh/edc/main/install.sh | sh
```

script는 Linux에서는 `edc`를 `/usr/local/bin`에, macOS에서는 `~/.local/bin`에 설치합니다. Linux의 패킷 캡처와 일부 trace는 root 또는 해당 capability가 필요하고, `sudo`의 `PATH`에는 보통 `/usr/local/bin`만 있기 때문입니다. 일반 사용자가 설치 디렉터리에 쓸 수 없으면 script는 복사할 때만 `sudo`를 씁니다. root가 `/usr/local/bin`에 쓸 수 없으면 `~/.local/bin`으로 전환합니다. 다른 디렉터리를 반드시 사용하려면 `BINDIR`을 지정합니다. 이전 버전을 받으려면 `EDC_VERSION`을 지정합니다.

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

[defaults.du]
workers = 4

[defaults.history]
limit = 200

[defaults.changes]
since = "24h"

[defaults.snapshot]
since = "6h"

[defaults.events]
since = "6h"

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
history_limit = 20
```

command별 값은 `defaults.common`을 덮습니다. positional target, URL, host, remote group과 `yes`, `force`, `dry-run`, `list`, `check` 같은 action option은 저장하지 않습니다. 저장하는 remote inventory와 recipe는 absolute path여야 합니다. 빈 path 값은 해당 기본값을 사용하지 않는다는 뜻입니다.

Setup wizard는 `edc log`의 저장 경로를 비워두고 실행별 파일을 자동 생성하도록 추천합니다. 직접 지정한 경로의 상위 디렉터리는 이미 있어야 합니다. 이전 추천값인 `edc.log` 경로는 상위 디렉터리 자동 생성을 계속 지원합니다.

## Shell completion

`edc completion`은 completion script를 출력합니다. script는 command, option, 그리고 `edc`가 찾은 inventory의 group 이름을 완성합니다.

```bash
source <(edc completion zsh)
source <(edc completion bash)
```

zsh에서는 script를 `fpath`의 디렉터리에 `_edc`라는 이름으로 저장해도 됩니다.

`edc completion groups`는 inventory의 group 이름을 한 줄에 하나씩 출력합니다. script가 이 command를 호출합니다.

