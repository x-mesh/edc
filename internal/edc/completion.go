package edc

import (
	"fmt"
	"io"
	"os"
)

func runCompletion(args []string) int {
	const usage = "사용법: edc completion <zsh|bash|groups>"
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "zsh":
		fmt.Fprint(os.Stdout, zshCompletion)
	case "bash":
		fmt.Fprint(os.Stdout, bashCompletion)
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
		return writeCompletionGroups(os.Stdout, cwd, configDir)
	default:
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	return 0
}

// writeCompletionGroups는 shell completion이 읽도록 inventory group 이름을 한 줄에 하나씩 쓴다.
func writeCompletionGroups(writer io.Writer, cwd, configDir string) int {
	path, found := discoverRemoteInventory(cwd, configDir)
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

_edc() {
  local curcontext="$curcontext" state line
  typeset -A opt_args
  local -a common
  common=(
    '--timeout[실행 제한 시간]:duration'
    '--json[JSON 출력 경로, stdout은 -]:path:_files'
    '(-v --verbose)'{-v,--verbose}'[상세 evidence 출력]'
    '--redact[JSON 민감정보 redaction]:bool:(true false)'
  )
  _arguments -C '1:command:->command' '*::arg:->args' && return
  case $state in
    command)
      local -a commands
      commands=(
        'top:실시간 host resource 모니터'
        'info:system, network, disk 정보'
        'doctor:종합 network 진단'
        'dns:DNS lookup과 resolver 설정'
        'tcp:TCP 연결 확인'
        'tls:TLS handshake와 인증서 확인'
        'http:HTTP 응답 확인'
        'net:interfaces, route, ping, trace'
        'sockets:listening socket 목록'
        'quality:macOS networkQuality 측정'
        'capture:packet capture'
        'report:저장한 JSON report 보기와 비교'
        'remote:inventory group에 recipe 실행'
        'completion:shell completion 출력'
        'update:GitHub release에서 최신 버전 설치'
        'version:버전 출력'
        'help:도움말'
      )
      _describe -t commands 'edc command' commands
      ;;
    args)
      case $words[1] in
        top)
          _arguments '--interval[sampling interval]:duration' '--count[출력 row 수]:count' '--no-header[header 생략]' '--json[sample당 한 줄 JSON 출력 경로]:path:_files'
          ;;
        info)
          _arguments '--public[public IP, 지역, ASN 조회. --public=false로 끕니다]' '--timeout[public 조회 제한 시간]:duration' '(-v --verbose)'{-v,--verbose}'[조회 실패 원인 출력]'
          ;;
        doctor)
          _arguments $common '--profile[default 또는 full]:profile:(default full)' '1:host or URL:_hosts'
          ;;
        dns)
          _arguments $common '1:subcommand:(lookup config)' '2:host:_hosts'
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
        sockets|quality)
          _arguments $common
          ;;
        capture)
          _arguments '--interface[capture할 interface]:interface' '--duration[capture 시간]:duration' '--count[packet 수]:count' '--filter[BPF filter]:filter' '--output[pcap 저장 경로]:path:_files' '--yes[확인 생략]'
          ;;
        report)
          _arguments '--json[JSON 출력 경로]:path:_files' '1:subcommand:(show diff)' '*:report file:_files -g "*.json"'
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
_edc() {
  local cur prev command
  cur="${COMP_WORDS[COMP_CWORD]}"
  prev="${COMP_WORDS[COMP_CWORD-1]}"
  local commands="top info doctor dns tcp tls http net sockets quality capture report remote completion update version help"
  local common="--timeout --json --verbose -v --redact"
  if [[ $COMP_CWORD -eq 1 ]]; then
    COMPREPLY=($(compgen -W "$commands" -- "$cur"))
    return
  fi
  command="${COMP_WORDS[1]}"
  case "$command" in
    top) COMPREPLY=($(compgen -W "--interval --count --no-header --json" -- "$cur")) ;;
    info) COMPREPLY=($(compgen -W "--public --timeout --verbose -v" -- "$cur")) ;;
    doctor) COMPREPLY=($(compgen -W "$common --profile" -- "$cur")) ;;
    dns)
      if [[ $COMP_CWORD -eq 2 ]]; then COMPREPLY=($(compgen -W "lookup config" -- "$cur")); else COMPREPLY=($(compgen -W "$common" -- "$cur")); fi ;;
    tcp)
      if [[ $COMP_CWORD -eq 2 ]]; then COMPREPLY=($(compgen -W "check" -- "$cur")); else COMPREPLY=($(compgen -W "$common" -- "$cur")); fi ;;
    tls)
      if [[ $COMP_CWORD -eq 2 ]]; then COMPREPLY=($(compgen -W "check" -- "$cur")); else COMPREPLY=($(compgen -W "$common --min-days" -- "$cur")); fi ;;
    http)
      if [[ $COMP_CWORD -eq 2 ]]; then COMPREPLY=($(compgen -W "check" -- "$cur")); else COMPREPLY=($(compgen -W "$common --expect-status" -- "$cur")); fi ;;
    net)
      if [[ $COMP_CWORD -eq 2 ]]; then COMPREPLY=($(compgen -W "interfaces route ping trace" -- "$cur")); else COMPREPLY=($(compgen -W "$common" -- "$cur")); fi ;;
    sockets|quality) COMPREPLY=($(compgen -W "$common" -- "$cur")) ;;
    capture) COMPREPLY=($(compgen -W "--interface --duration --count --filter --output --yes" -- "$cur")) ;;
    update) COMPREPLY=($(compgen -W "--check --yes --timeout" -- "$cur")) ;;
    report)
      if [[ $COMP_CWORD -eq 2 ]]; then COMPREPLY=($(compgen -W "show diff" -- "$cur"))
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
  esac
}
complete -F _edc edc
`
