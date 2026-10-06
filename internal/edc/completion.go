package edc

import (
	"fmt"
	"io"
	"os"
	"strings"
)

func runCompletion(args []string) int {
	usage := T("cli.usage", "edc completion <zsh|bash|groups>")
	if len(args) == 0 {
		choice, ok := promptMissingChoice("edc completion", []string{"zsh", "bash", "groups"})
		if !ok {
			fmt.Fprintln(os.Stderr, usage)
			return 2
		}
		args = []string{choice}
	}
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "zsh":
		fmt.Fprint(os.Stdout, renderCompletion(zshCompletion, zshCommandList()))
	case "bash":
		fmt.Fprint(os.Stdout, renderCompletion(bashCompletion, bashCommandList()))
	case "groups":
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		configDir, err := os.UserConfigDir()
		if err != nil {
			configDir = ""
		}
		return writeCompletionGroupsWithPath(os.Stdout, cwd, configDir, configuredString(activeConfig.Defaults.Remote.Inventory, ""))
	default:
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	return 0
}

// renderCompletion은 script의 명령 목록 자리를 commandDocs에서 만든 목록으로 채운다.
// help 출력과 completion이 같은 표를 읽으므로 설명이 서로 어긋나지 않는다.
func renderCompletion(script, commands string) string {
	return strings.Replace(script, "@@COMMANDS@@", commands, 1)
}

func zshCommandList() string {
	var builder strings.Builder
	for _, doc := range commandSummaries() {
		fmt.Fprintf(&builder, "        '%s:%s'\n", doc.name, doc.summary())
	}
	builder.WriteString("        'help:" + T("help.help_command_summary") + "'")
	return builder.String()
}

func bashCommandList() string {
	return strings.Join(append(commandNames(), "help"), " ")
}

// writeCompletionGroups는 shell completion이 읽도록 inventory group 이름을 한 줄에 하나씩 쓴다.
func writeCompletionGroups(writer io.Writer, cwd, configDir string) int {
	return writeCompletionGroupsWithPath(writer, cwd, configDir, "")
}

func writeCompletionGroupsWithPath(writer io.Writer, cwd, configDir, configuredPath string) int {
	path := configuredPath
	found := path != ""
	if !found {
		path, found = discoverRemoteInventory(cwd, configDir)
	}
	if !found {
		fmt.Fprintln(os.Stderr, remoteInventoryNotFound(cwd))
		return 2
	}
	inventory, err := loadRemoteInventory(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	for _, group := range remoteGroupNames(inventory) {
		fmt.Fprintln(writer, group)
	}
	return 0
}

const zshCompletion = `#compdef edc
compdef _edc edc

# edc completion zsh 출력. source <(edc completion zsh)로 읽거나 fpath에 _edc 파일로 둔다.

_edc_remote_groups() {
  local -a groups
  groups=(${(f)"$(edc completion groups 2>/dev/null)"})
  (( ${#groups} )) && _describe -t groups 'inventory group' groups
}

# exits.yaml은 로그인마다 바뀌지 않는 로컬 파일이라, remote group처럼 edc를 부르지 않고 grep으로
# 이름만 뽑는다. .edc/exits.yaml을 cwd/exits.yaml보다 먼저 본다(discoverRemoteFile과 같은 순서).
_edc_route_exits() {
  local -a exits
  local file
  for file in .edc/exits.yaml exits.yaml; do
    [[ -f $file ]] || continue
    exits=(${(f)"$(grep -E '^[[:space:]]*-?[[:space:]]*name:' "$file" 2>/dev/null | sed -E 's/.*name:[[:space:]]*//')"})
    break
  done
  (( ${#exits} )) && _describe -t exits 'exit name' exits
}

_edc() {
  local curcontext="$curcontext" state line
  typeset -A opt_args
  local -a common
  common=(
    '--timeout[실행 제한 시간]:duration'
    '--json[JSON 출력 경로, stdout은 -]:path:_files'
    '(-v --verbose)'{-v,--verbose}'[상세 evidence 출력]'
    '--redact[민감정보 redaction]'
  )
  _arguments -C '1:command:->command' '*::arg:->args' && return
  case $state in
    command)
      local -a commands
      commands=(
@@COMMANDS@@
      )
      _describe -t commands 'edc command' commands
      ;;
    args)
      case $words[1] in
        top)
          _arguments '--interval[sampling interval]:duration' '--count[출력 row 수]:count' '--no-header[header 생략]' '--process[process filter]:filter' '(-d --detail)'{-d,--detail}'[eBPF details]' '(-w --write)'{-w,--write}'[SQLite 기록, 경로 생략 시 기본 DB]::path:_files' '--json[sample당 한 줄 JSON 출력 경로]:path:_files'
          ;;
        history)
          _arguments '1:subcommand:(list top process)' '--run[실행 ID]:id' '--from[시작 시각]:RFC3339' '--to[종료 시각]:RFC3339' '--process[process filter]:filter' '--metric[지표 이름]:metric' '--min[최솟값]:number' '--max[최댓값]:number' '--limit[최대 결과 수]:count' '--json[JSONL 출력 경로]:path:_files' '*:database:_files'
          ;;
        watch)
          if [[ $words[2] == fs ]]; then
            _arguments '1:subcommand:(http fs)' '--recursive[하위 디렉터리 감시]' '--event[파일 이벤트]:events:(create modify remove rename)' '--match[감시 루트 기준 glob]:glob' '--exec[실행할 shell command]:command' '--rules[YAML rule 파일]:path:_files' '--exclude[제외 glob]:glob' '--debounce[이벤트 묶음 시간]:duration' '--timeout[action 제한 시간]:duration' '--duration[감시 시간]:duration' '--json[JSONL 출력 경로]:path:_files' '--dry-run[action 미실행]' '2:directory:_files -/'
          else
            _arguments $common '(-i --interval)'{-i,--interval}'[sample 간격(초)]:seconds' '--duration[관측 시간]:duration' '--expect-status[기대 HTTP status]:code' '1:subcommand:(http fs)' '2:host or URL:_hosts'
          fi
          ;;
        info)
          _arguments '--public[public IP, 지역, ASN 조회. --public=false로 끕니다]' '--timeout[public 조회 제한 시간]:duration' '(-v --verbose)'{-v,--verbose}'[조회 실패 원인 출력]'
          ;;
        doctor)
          _arguments $common '--profile[default 또는 full]:profile:(default full)' '--all-ips[DNS 응답 IP별 직접 연결 검사]' '1:host or URL:_hosts'
          ;;
        dns)
          _arguments $common '--resolver[비교할 DNS 서버]:IP' '1:subcommand:(lookup compare config)' '2:host:_hosts'
          ;;
        tcp)
          _arguments $common '1:subcommand:(check)' '2:host\:port'
          ;;
        tls)
          _arguments $common '--min-days[인증서 남은 일수 하한, 미만이면 fail]:days' '1:subcommand:(check)' '2:host\:port'
          ;;
        http)
          _arguments $common '--expect-status[기대 HTTP status code]:code' '1:subcommand:(check)' '2:URL'
          ;;
        net)
          _arguments $common '1:subcommand:(interfaces route ping trace)' '2:host:_hosts'
          ;;
        route)
          case $words[2] in
            switch)
              _arguments \
                '--to[전환할 출구 이름]:name:_edc_route_exits' \
                '--seconds[롤백 유예(초), 10-900]:seconds' \
                '--exits[exits.yaml 경로]:path:_files' \
                '--force[자기 차단 위험이 높고 안전장치가 미확인이어도 전환]' \
                '--yes[신원 확인이 일치하면 확인 생략]' \
                '(-n --dry-run)'{-n,--dry-run}'[아무것도 바꾸지 않고 계획만 출력]'
              ;;
            rollback)
              _arguments '--state[route 상태 파일 경로]:path:_files'
              ;;
            check|status)
              _arguments $common
              ;;
            *)
              _arguments '1:subcommand:(check switch status rollback)'
              ;;
          esac
          ;;
        change)
          case $words[2] in
            apply)
              _arguments '--kind[change adapter]:kind:(authorized-keys iptables)' '--path[authorized_keys path]:path:_files' '--content-file[new authorized_keys file]:path:_files' '--rules-file[iptables-save rules file]:path:_files' '--seconds[rollback grace in seconds]:seconds' '--yes[confirm after apply]' $common
              ;;
            confirm|rollback)
              _arguments '--state[change state file]:path:_files' $common
              ;;
            status)
              _arguments $common
              ;;
            *)
              _arguments '1:subcommand:(apply status confirm rollback)'
              ;;
          esac
          ;;
        listen)
          # listen의 -v는 evidence를 펼치지 않고 열을 늘린다. 공용 설명을 빼고 이 명령의 것을 쓴다.
          _arguments ${common:#*verbose*} '--tcp[TCP socket만 봅니다]' '(-u --udp)'{-u,--udp}'[바인드된 UDP socket만 봅니다]' '--unix[unix domain socket만 봅니다]' '--all[TCP와 UDP, unix domain socket을 모두 봅니다]' '--watch[포트 변화 관측]' '(-i --interval)'{-i,--interval}'[관측 간격(초)]:seconds' '--duration[관측 시간]:duration' '(-v --verbose)'{-v,--verbose}'[계정과 descriptor, 큐 열을 함께 엽니다]'
          ;;
        quality)
          _arguments $common '--server[응답성 측정 config URL]:url'
          ;;
        capture)
          _arguments '--mode[캡처 방식]:mode:(pcap events)' '--interface[capture할 interface]:interface' '--duration[capture 시간]:duration' '--count[packet 수]:count' '--filter[BPF filter]:filter' '--output[pcap 저장 경로]:path:_files' '--yes[확인 생략]'
          ;;
        trace)
          _arguments '1:subcommand:(tcp udp dns arp ndp http mysql)'
          case $words[2] in
            tcp|udp) _arguments '--duration[trace 시간]:duration' '--json[JSON 저장 경로]:path:_files' '--raw[raw event JSONL 출력]' '--live[실시간 event 출력]' '--group-by[그룹 기준]:group:(source target port process event)' '--process[process filter]:process' '--destination[destination filter]:host:port' '(-d --detail)'{-d,--detail}'[연결별 상세 요약]' '--yes[확인 생략]' ;;
            dns) _arguments '--duration[trace 시간]:duration' '--json[JSON 저장 경로]:path:_files' '--raw[raw event JSONL 출력]' '--live[실시간 event 출력]' '--group-by[그룹 기준]:group:(source target process event)' '--process[process filter]:process' '--destination[DNS server filter]:host:port' '--side[DNS 관측 쪽]:side:(client server)' '--yes[확인 생략]' ;;
            http) _arguments '--duration[trace 시간]:duration' '--json[JSON 저장 경로]:path:_files' '--raw[raw event JSONL 출력]' '--live[실시간 event 출력]' '--group-by[그룹 기준]:group:(source target port process event path)' '--process[process filter]:process' '--destination[destination filter]:host:port' '--side[관측 쪽]:side:(client server)' '--payload=-[message 출력, =all은 전체]::mode:(all)' '--show-secrets[가린 header 값 출력]' '--tls=-[OpenSSL HTTPS 평문, =경로는 그 파일만]::path:_files' '--port[HTTP 서버 port]:port' '--yes[확인 생략]' ;;
            mysql) _arguments '--duration[trace 시간]:duration' '--json[JSON 저장 경로]:path:_files' '--raw[raw event JSONL 출력]' '--live[실시간 event 출력]' '--group-by[그룹 기준]:group:(source target port process event)' '--process[process filter]:process' '--destination[destination filter]:host:port' '--side[관측 쪽]:side:(client server)' '--show-secrets[SQL 문자열 출력]' '--port[MySQL 서버 port]:port' '--yes[확인 생략]' ;;
            arp|ndp) _arguments '--duration[trace 시간]:duration' '--json[JSON 저장 경로]:path:_files' '--raw[raw event JSONL 출력]' '--live[실시간 event 출력]' '--group-by[그룹 기준]:group:(source target event)' '--destination[IP filter]:ip' ;;
          esac
          ;;
        log)
          if [[ $words[2] == history ]]; then
            local -a words=("${words[@]:1}")
            local CURRENT=$((CURRENT-1))
            _arguments -S -A "-*" '--dir[로그 디렉터리]:directory:_directories' '--file[로그 파일]:path:_files' '--key[명령 키]:key' '--command[명령별 키 목록]:command:_command_names -e' '--limit[최근 실행 수]:count' '--failed[실패만 조회]' '*:command:_command_names -e'
            return
          fi
          if (( CURRENT == 2 )); then compadd history; fi
          _arguments -S '--stream[capture할 stream]:stream:(stdout stderr)' '--output[append log 경로]:path:_files' '--command-display[marker에 기록할 command 범위]:mode:(full name none)' '1:separator:(--)' '2:command:_command_names -e' '*::command argument'
          ;;
        report)
          _arguments '--json[JSON 출력 경로]:path:_files' '1:subcommand:(list show diff)' '*:report file:_files -g "*.json"'
          ;;
        remote)
          _arguments $common \
            '--inventory[inventory YAML 경로]:path:_files' \
            '--recipe[recipe YAML 경로]:path:_files' \
            '--group[실행할 inventory group]:group:_edc_remote_groups' \
            '--connect-timeout[SSH 연결 제한 시간]:duration' \
            '--output-limit[command별 출력 byte 상한]:bytes' \
            '--parallel[동시에 실행할 host 수]:count' \
            '(-f --force)'{-f,--force}'[계획 확인 프롬프트 생략]' \
            '(-n --dry-run)'{-n,--dry-run}'[실행 계획만 출력]' \
            '(-l --list)'{-l,--list}'[inventory group과 host 출력]' \
            '1:group:_edc_remote_groups'
          ;;
        completion)
          _arguments '1:shell:(zsh bash groups)'
          ;;
        setup)
          _arguments
          ;;
      esac
      ;;
  esac
}

# fpath에서 autoload되면 이 파일 전체가 함수 본문이므로 바로 실행한다.
if [[ $funcstack[1] == _edc ]]; then
  _edc "$@"
fi
`

const bashCompletion = `# edc completion bash 출력. source <(edc completion bash)로 읽는다.

# exits.yaml은 로컬 파일이므로 remote group의 edc 왕복 대신 grep으로 이름만 뽑는다.
# .edc/exits.yaml을 exits.yaml보다 먼저 본다(discoverRemoteFile과 같은 순서).
_edc_route_exit_names() {
  local file
  for file in .edc/exits.yaml exits.yaml; do
    if [[ -f $file ]]; then
      grep -E '^[[:space:]]*-?[[:space:]]*name:' "$file" 2>/dev/null | sed -E 's/.*name:[[:space:]]*//'
      return
    fi
  done
}

_edc() {
  local cur prev command index
  cur="${COMP_WORDS[COMP_CWORD]}"
  prev="${COMP_WORDS[COMP_CWORD-1]}"
  local commands="@@COMMANDS@@"
  local common="--timeout --json --verbose -v --redact"
  if [[ $COMP_CWORD -eq 1 ]]; then
    COMPREPLY=($(compgen -W "$commands" -- "$cur"))
    return
  fi
  command="${COMP_WORDS[1]}"
  case "$command" in
    top)
      case "$prev" in
        --json) COMPREPLY=($(compgen -f -- "$cur")); return ;;
        -w|--write) if [[ $cur != -* ]]; then COMPREPLY=($(compgen -f -- "$cur")); return; fi ;;
      esac
      COMPREPLY=($(compgen -W "--interval --count --no-header --process -d --detail --ebpf -w --write --json" -- "$cur")) ;;
    history)
      if [[ $COMP_CWORD -eq 2 ]]; then COMPREPLY=($(compgen -W "list top process" -- "$cur"))
      elif [[ $prev == --json ]]; then COMPREPLY=($(compgen -f -- "$cur"))
      elif [[ $prev == --run || $prev == --from || $prev == --to || $prev == --process || $prev == --metric || $prev == --min || $prev == --max || $prev == --limit ]]; then COMPREPLY=()
      elif [[ $cur == -* ]]; then COMPREPLY=($(compgen -W "--run --from --to --process --metric --min --max --limit --json" -- "$cur"))
      else COMPREPLY=($(compgen -f -- "$cur")); fi ;;
    watch)
      if [[ $COMP_CWORD -eq 2 ]]; then COMPREPLY=($(compgen -W "http fs" -- "$cur")); return; fi
      if [[ ${COMP_WORDS[2]} == fs ]]; then
        case "$prev" in
          --json|--rules) COMPREPLY=($(compgen -f -- "$cur")); return ;;
          --event) COMPREPLY=($(compgen -W "create modify remove rename" -- "$cur")); return ;;
          --exec|--match|--exclude|--debounce|--timeout|--duration) COMPREPLY=(); return ;;
        esac
        if [[ $cur == -* ]]; then COMPREPLY=($(compgen -W "--recursive --event --match --exec --rules --exclude --debounce --timeout --duration --json --dry-run" -- "$cur"))
        else COMPREPLY=($(compgen -d -- "$cur")); fi
      else COMPREPLY=($(compgen -W "$common -i --interval --duration --expect-status" -- "$cur")); fi ;;

    info) COMPREPLY=($(compgen -W "--public --timeout --verbose -v" -- "$cur")) ;;
    doctor) COMPREPLY=($(compgen -W "$common --profile --all-ips" -- "$cur")) ;;
    dns)
      if [[ $COMP_CWORD -eq 2 ]]; then COMPREPLY=($(compgen -W "lookup compare config" -- "$cur")); else COMPREPLY=($(compgen -W "$common --resolver" -- "$cur")); fi ;;
    tcp)
      if [[ $COMP_CWORD -eq 2 ]]; then COMPREPLY=($(compgen -W "check" -- "$cur")); else COMPREPLY=($(compgen -W "$common" -- "$cur")); fi ;;
    tls)
      if [[ $COMP_CWORD -eq 2 ]]; then COMPREPLY=($(compgen -W "check" -- "$cur")); else COMPREPLY=($(compgen -W "$common --min-days" -- "$cur")); fi ;;
    http)
      if [[ $COMP_CWORD -eq 2 ]]; then COMPREPLY=($(compgen -W "check" -- "$cur")); else COMPREPLY=($(compgen -W "$common --expect-status" -- "$cur")); fi ;;
    net)
      if [[ $COMP_CWORD -eq 2 ]]; then COMPREPLY=($(compgen -W "interfaces route ping trace" -- "$cur")); else COMPREPLY=($(compgen -W "$common" -- "$cur")); fi ;;
    route)
      if [[ $COMP_CWORD -eq 2 ]]; then
        COMPREPLY=($(compgen -W "check switch status rollback" -- "$cur"))
      else
        case "${COMP_WORDS[2]}" in
          switch)
            case "$prev" in
              --to) COMPREPLY=($(compgen -W "$(_edc_route_exit_names)" -- "$cur")); return ;;
              --exits) COMPREPLY=($(compgen -f -- "$cur")); return ;;
            esac
            if [[ $cur == -* ]]; then COMPREPLY=($(compgen -W "--to --seconds --exits --force --yes --dry-run" -- "$cur")); fi ;;
          rollback)
            case "$prev" in
              --state) COMPREPLY=($(compgen -f -- "$cur")); return ;;
            esac
            if [[ $cur == -* ]]; then COMPREPLY=($(compgen -W "--state" -- "$cur")); fi ;;
          check|status)
            COMPREPLY=($(compgen -W "$common" -- "$cur")) ;;
        esac
      fi ;;
    change)
      if [[ $COMP_CWORD -eq 2 ]]; then
        COMPREPLY=($(compgen -W "apply status confirm rollback" -- "$cur"))
      else
        case "\${COMP_WORDS[2]}" in
          apply) COMPREPLY=($(compgen -W "--kind --path --content-file --rules-file --seconds --yes $common" -- "$cur")) ;;
          confirm|rollback)
            if [[ $prev == --state ]]; then COMPREPLY=($(compgen -f -- "$cur")); else COMPREPLY=($(compgen -W "--state $common" -- "$cur")); fi ;;
          status) COMPREPLY=($(compgen -W "$common" -- "$cur")) ;;
        esac
      fi ;;
    listen) COMPREPLY=($(compgen -W "$common --tcp --udp --unix --all --watch -i --interval --duration" -- "$cur")) ;;
    quality) COMPREPLY=($(compgen -W "$common --server" -- "$cur")) ;;
    capture) COMPREPLY=($(compgen -W "--mode --interface --duration --count --filter --output --yes" -- "$cur")) ;;
    trace)
      if [[ $COMP_CWORD -eq 2 ]]; then COMPREPLY=($(compgen -W "tcp udp dns arp ndp http mysql" -- "$cur")); else COMPREPLY=($(compgen -W "--duration --json --raw --live --group-by --process --destination -d --detail --side --payload --payload=all --show-secrets --tls --port --yes" -- "$cur")); fi ;;
    log)
      if [[ $COMP_CWORD -eq 2 && $cur != -* ]]; then COMPREPLY=($(compgen -W "history" -- "$cur")); return; fi
      if [[ ${COMP_WORDS[2]} == history ]]; then
        for ((index=3; index<COMP_CWORD; index++)); do
          case "${COMP_WORDS[index]}" in
            --dir|--file|--key|--limit|--command|-dir|-file|-key|-limit|-command) ((index++)) ;;
            --) COMPREPLY=(); if [[ $index -eq $((COMP_CWORD-1)) ]]; then COMPREPLY=($(compgen -c -- "$cur")); fi; return ;;
            -|[!-]*) COMPREPLY=($(compgen -f -- "$cur")); return ;;
          esac
        done
        case "$prev" in
          --dir|--file) COMPREPLY=($(compgen -f -- "$cur")); return ;;
          --key|--limit) COMPREPLY=(); return ;;
          --command) COMPREPLY=($(compgen -c -- "$cur")); return ;;
        esac
        if [[ $cur == -* ]]; then
          COMPREPLY=($(compgen -W "--dir --file --key --command --limit --failed --" -- "$cur"))
        else
          COMPREPLY=($(compgen -c -- "$cur"))
        fi
        return
      fi
      for ((index=2; index<COMP_CWORD; index++)); do
        if [[ ${COMP_WORDS[index]} == -- ]]; then
          COMPREPLY=()
          if [[ $index -eq $((COMP_CWORD-1)) ]]; then COMPREPLY=($(compgen -c -- "$cur")); fi
          return
        fi
      done
      case "$prev" in
        --stream) COMPREPLY=($(compgen -W "stdout stderr" -- "$cur")); return ;;
        --command-display) COMPREPLY=($(compgen -W "full name none" -- "$cur")); return ;;
        --output) COMPREPLY=($(compgen -f -- "$cur")); return ;;
      esac
      if [[ $cur == -* ]]; then COMPREPLY=($(compgen -W "--stream --output --command-display --" -- "$cur")); fi ;;
    update) COMPREPLY=($(compgen -W "--check --yes --timeout" -- "$cur")) ;;
    report)
      if [[ $COMP_CWORD -eq 2 ]]; then COMPREPLY=($(compgen -W "list show diff" -- "$cur"))
      elif [[ $cur == -* ]]; then COMPREPLY=($(compgen -W "--json" -- "$cur"))
      else COMPREPLY=($(compgen -f -- "$cur")); fi ;;
    remote)
      case "$prev" in
        --inventory|--recipe|--json) COMPREPLY=($(compgen -f -- "$cur")); return ;;
        --group) COMPREPLY=($(compgen -W "$(edc completion groups 2>/dev/null)" -- "$cur")); return ;;
      esac
      if [[ $cur == -* ]]; then
        COMPREPLY=($(compgen -W "$common --inventory --recipe --group --connect-timeout --output-limit --parallel -f --force -n --dry-run -l --list" -- "$cur"))
      else
        COMPREPLY=($(compgen -W "$(edc completion groups 2>/dev/null)" -- "$cur"))
      fi ;;
    completion) COMPREPLY=($(compgen -W "zsh bash groups" -- "$cur")) ;;
    setup) COMPREPLY=() ;;
  esac
}
complete -F _edc edc
`
