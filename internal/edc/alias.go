package edc

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// aliasDoc은 edc alias가 내는 별칭 하나다. example은 줄 끝 주석으로 보이는 쓰임이다.
type aliasDoc struct {
	name, command, example string
}

type aliasGroup struct {
	title   string
	aliases []aliasDoc
}

// aliasGroups는 help의 묶음을 따른다. route, disk, change처럼 호스트를 바꾸는 명령은 짧은 이름이 실수를 부르므로 넣지 않는다.
// 별칭은 명령 앞에 옵션만 붙이므로 뒤의 인자가 그대로 이어지고, 같은 옵션을 다시 주면 나중 값이 이긴다.
var aliasGroups = []aliasGroup{
	{"logs", []aliasDoc{
		{"eg", "edc grep", "eg timeout /var/log/app/"},
		{"egs", "edc grep --ordered --since", "egs 10m --status 5xx access.log  (logs in time order only)"},
		{"ege", "edc grep --level error --last 20", "ege app.log, ege --last 50 app.log"},
		{"egr", "edc grep -E -i -C 2", "egr 'order=[0-9]+' app.log"},
		{"ev", "edc view", "ev /var/log/nginx/access.log"},
		{"evn", "edc view -n 50000", "evn app.log  (keep more lines on the screen and in the stats)"},
		{"evf", "edc view -f", "evf app.log | grep timeout  (in a pipe, follow like tail -F)"},
	}},
	{"diagnose", []aliasDoc{
		{"edoc", "edc doctor", "edoc example.com"},
		{"echk", "edc check", "echk"},
		{"edns", "edc dns", "edns lookup example.com"},
		{"etls", "edc tls check", "etls example.com:443"},
		{"ehttp", "edc http check", "ehttp https://example.com"},
		{"eevt", "edc events", "eevt --since 1h"},
	}},
	{"observe", []aliasDoc{
		{"etop", "edc top", "etop"},
		{"einfo", "edc info", "einfo"},
		{"esvc", "edc svc", "esvc nginx"},
		{"ectr", "edc ctr", "ectr"},
		{"edu", "edc du", "edu /var/log"},
		{"elsn", "edc listen", "elsn"},
		{"echg", "edc changes", "echg --since 24h"},
		{"ewf", "edc watch fs -r", "ewf /etc"},
		{"ewh", "edc watch http", "ewh https://example.com"},
		{"eth", "edc trace http", "eth"},
	}},
	{"record and remote", []aliasDoc{
		{"ehist", "edc history", "ehist"},
		{"esnap", "edc snapshot", "esnap"},
		{"erep", "edc report", "erep list"},
		{"erem", "edc remote", "erem web"},
		{"etun", "edc tunnel", "etun up db"},
	}},
}

// aliasShellWords는 alias로 가리면 안 되는 zsh와 bash의 내장 명령과 예약어다.
var aliasShellWords = map[string]bool{
	"alias": true, "autoload": true, "bg": true, "bind": true, "bindkey": true, "break": true, "builtin": true, "case": true, "cd": true,
	"command": true, "compdef": true, "complete": true, "continue": true, "declare": true, "dirs": true, "disown": true, "do": true, "done": true,
	"echo": true, "elif": true, "else": true, "emulate": true, "enable": true, "esac": true, "eval": true, "exec": true, "exit": true, "export": true,
	"fc": true, "fg": true, "fi": true, "for": true, "function": true, "getopts": true, "hash": true, "help": true, "history": true, "if": true,
	"in": true, "jobs": true, "kill": true, "let": true, "local": true, "logout": true, "popd": true, "print": true, "printf": true, "pushd": true,
	"pwd": true, "read": true, "readonly": true, "rehash": true, "return": true, "select": true, "set": true, "setopt": true, "shift": true,
	"shopt": true, "source": true, "suspend": true, "test": true, "then": true, "time": true, "times": true, "trap": true, "type": true,
	"typeset": true, "ulimit": true, "umask": true, "unalias": true, "unset": true, "unsetopt": true, "until": true, "wait": true, "whence": true,
	"where": true, "which": true, "while": true, "zle": true, "zmodload": true,
}

// aliasGuard는 source할 때 이름이 비어 있는지 보고 alias를 건다. edc는 셸의 alias와 함수를 볼 수 없으므로 셸 안에서 확인한다.
const aliasGuard = `_edc_alias() {
	local kind='' hint=''
	if alias "$1" >/dev/null 2>&1; then kind='an alias' hint="unalias $1"
	elif typeset -f "$1" >/dev/null 2>&1; then kind='a function' hint="unset -f $1"
	elif command -v "$1" >/dev/null 2>&1; then kind='a command'
	fi
	if [ -z "$kind" ]; then
		alias "$1=$2"
		return 0
	fi
	printf 'edc alias: %s is already %s; skipped. Use another name: alias x%s=%s' "$1" "$kind" "$1" "'$2'" >&2
	if [ -n "$hint" ]; then printf '  or remove yours: %s' "$hint" >&2; fi
	printf '\n' >&2
}
`

func runAlias(args []string) int {
	shell, plain, help, ok := parseAliasArgs(args, os.Getenv("SHELL"))
	if help {
		printCommandHelp(os.Stdout, "alias")
		return 0
	}
	if !ok {
		fmt.Fprintln(os.Stderr, T("cli.usage", "edc alias [zsh|bash] [--plain]"))
		return 2
	}
	if shell != "zsh" && shell != "bash" {
		fmt.Fprintln(os.Stderr, T("command.alias.unsupported_shell", shell))
		return 2
	}
	fmt.Fprint(os.Stdout, renderAliases(shell, plain, aliasConflicts(exec.LookPath)))
	return 0
}

// parseAliasArgs는 셸 이름과 --plain을 읽는다. 셸 이름이 없으면 $SHELL의 이름이다.
func parseAliasArgs(args []string, shellEnv string) (shell string, plain, help, ok bool) {
	for _, arg := range args {
		switch {
		case arg == "--plain":
			plain = true
		case arg == "-h" || arg == "--help":
			help = true
		case strings.HasPrefix(arg, "-") || shell != "":
			return "", false, false, false
		default:
			shell = arg
		}
	}
	if shell == "" {
		shell = filepath.Base(shellEnv)
	}
	return shell, plain, help, true
}

// aliasConflicts는 이 호스트에서 이미 명령인 이름이다. 값은 그 명령의 경로이거나 내장 명령이라는 설명이다.
func aliasConflicts(lookPath func(string) (string, error)) map[string]string {
	conflicts := map[string]string{}
	for _, group := range aliasGroups {
		for _, alias := range group.aliases {
			if aliasShellWords[alias.name] {
				conflicts[alias.name] = "a shell builtin"
				continue
			}
			if path, err := lookPath(alias.name); err == nil {
				conflicts[alias.name] = path
			}
		}
	}
	return conflicts
}

// renderAliases는 source할 script다. plain이면 guard 없이 alias 줄만 내고, 이 호스트의 명령과 겹치는 이름은 주석으로 막는다.
func renderAliases(shell string, plain bool, conflicts map[string]string) string {
	var out strings.Builder
	rc := "~/." + shell + "rc"
	if plain {
		fmt.Fprintf(&out, "# edc aliases for %s. Paste into %s. A name that is already a command on this host is commented out.\n", shell, rc)
		out.WriteString("# To use another name, change the name before the '='.\n")
	} else {
		fmt.Fprintf(&out, "# edc aliases for %s. Install: echo 'source <(edc alias %s)' >> %s\n", shell, shell, rc)
		out.WriteString("# A name that is already an alias, a function, or a command in your shell is skipped with a notice when you source this.\n")
		out.WriteString("# To use another name, copy the line with the new name, for example: alias xeg='edc grep'\n")
		out.WriteString("# To use the edc one instead of yours: unalias NAME  or  unset -f NAME\n")
	}
	out.WriteString("# route, disk, and change have no alias: a short name invites a mistake on a host change.\n")
	if !plain {
		out.WriteString(aliasGuard)
	}
	for _, group := range aliasGroups {
		fmt.Fprintf(&out, "\n# %s\n", group.title)
		for _, alias := range group.aliases {
			definition := fmt.Sprintf("alias %s='%s'", alias.name, alias.command)
			if !plain {
				definition = fmt.Sprintf("_edc_alias %s '%s'", alias.name, alias.command)
			}
			if where, ok := conflicts[alias.name]; ok {
				fmt.Fprintf(&out, "# %s is %s on this host. Use another name, for example: alias x%s='%s'\n", alias.name, where, alias.name, alias.command)
				if plain {
					definition = "# " + definition
				}
			}
			fmt.Fprintf(&out, "%-52s # %s\n", definition, alias.example)
		}
	}
	if !plain {
		out.WriteString("\nunset -f _edc_alias\n")
	}
	return out.String()
}
