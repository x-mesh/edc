package edc

import (
	"fmt"
	"io"
	"os"
	"strings"
)

func runCompletion(args []string) int {
	usage := T("cli.usage", "edc completion <zsh|bash|groups>")
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
		return writeCompletionGroups(os.Stdout, cwd, configDir)
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
@@COMMANDS@@
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
  local commands="@@COMMANDS@@"
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
