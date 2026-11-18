package edc

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const shellPIDEnv = "EDC_SHELL_PID"

var otherShells = map[string]bool{
	"sh": true, "ash": true, "dash": true, "ksh": true, "mksh": true, "oksh": true, "yash": true, "csh": true, "tcsh": true,
	"fish": true, "nu": true, "pwsh": true, "elvish": true, "xonsh": true, "ion": true,
}

func runShell(args []string) int {
	return runShellWithIO(args, os.Getenv("SHELL"), os.Stdout, os.Stderr)
}

func runShellWithIO(args []string, shellEnv string, stdout, stderr io.Writer) int {
	shell := ""
	aliases := true
	for _, arg := range args {
		switch {
		case arg == "--no-alias":
			aliases = false
		case arg == "--help" || arg == "-h":
			printCommandHelp(stdout, "shell")
			return 0
		case strings.HasPrefix(arg, "-") || shell != "":
			fmt.Fprintln(stderr, T("cli.usage", "edc shell [zsh|bash] [--no-alias]"))
			return 2
		default:
			shell = arg
		}
	}
	explicit := shell != ""
	if !explicit {
		shell = filepath.Base(shellEnv)
	}
	if shell != "zsh" && shell != "bash" {
		key := "command.shell.unsupported_shell"
		if explicit && !otherShells[shell] {
			key = "command.shell.not_shell"
		}
		fmt.Fprintln(stderr, T(key, shell))
		if key == "command.shell.not_shell" {
			printShellSessionStatus(stderr, filepath.Base(shellEnv))
			printShellStartupStatus(stderr, filepath.Base(shellEnv))
		}
		return 2
	}
	_, err := io.WriteString(stdout, renderShell(shell, aliases))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	return 0
}

// 셸 함수는 하위 프로세스에 보이지 않으므로, 스크립트가 export한 셸 PID가 edc의 부모 PID와 같은지로 판단한다.
// 변수만 물려받은 중첩 셸은 PID가 달라 적용되지 않은 것으로 본다.
func shellLoadedInParent() bool {
	return os.Getenv(shellPIDEnv) == strconv.Itoa(os.Getppid())
}

func printShellSessionStatus(w io.Writer, shell string) {
	if shellLoadedInParent() {
		fmt.Fprintln(w, T("command.shell.loaded"))
		return
	}
	if shell == "zsh" || shell == "bash" {
		fmt.Fprintln(w, T("command.shell.current_session", "source <(edc shell)"))
	}
}

func printShellStartupStatus(w io.Writer, shell string) {
	if shell != "zsh" && shell != "bash" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(w, err)
		return
	}
	dir := home
	if zdot := os.Getenv("ZDOTDIR"); shell == "zsh" && zdot != "" {
		dir = zdot
	}
	path := filepath.Join(dir, "."+shell+"rc")
	shown := path
	if rel, err := filepath.Rel(home, path); err == nil && !strings.HasPrefix(rel, "..") {
		shown = "~/" + filepath.ToSlash(rel)
	}
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		fmt.Fprintln(w, err)
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(shellExecutedText(line), "edc shell") {
			fmt.Fprintln(w, T("command.shell.installed", shown))
			return
		}
	}
	fmt.Fprintln(w, T("command.shell.startup", shown))
	fmt.Fprintln(w, "  source <(edc shell "+shell+")")
}

// shellExecutedText는 시작 파일 한 줄에서 주석과 작은따옴표 안의 글자를 뺀 것이다. 둘 다 셸이 실행하지 않는다.
// 큰따옴표 안은 남긴다. eval "$(edc shell zsh)"처럼 명령 치환이 실행된다. 그 명령이 eval이면 작은따옴표 안도
// 남긴다. eval 'source <(edc shell zsh)'는 eval이 따옴표 안을 다시 읽어 실행한다.
func shellExecutedText(line string) string {
	var kept strings.Builder
	quote := byte(0)
	evaluated := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else if evaluated {
				kept.WriteByte(c)
			}
			continue
		case c == '\\' && i+1 < len(line):
			kept.WriteByte(c)
			i++
			c = line[i]
		case quote == '"':
			if c == '"' {
				quote = 0
			}
		case c == '\'':
			quote = c
			evaluated = shellCommandWord(kept.String()) == "eval"
			continue
		case c == '"':
			quote = c
		case c == '#' && (i == 0 || shellWordBreak(rune(line[i-1]))):
			return kept.String()
		}
		kept.WriteByte(c)
	}
	return kept.String()
}

// shellCommandWord는 text의 마지막 명령, 곧 큰따옴표 밖의 마지막 ;, &, |, ( 뒤의 첫 단어다.
// 앞에 붙은 {, then 같은 예약어, zsh의 repeat 횟수, NAME=value 대입은 건너뛴다.
func shellCommandWord(text string) string {
	words := shellLastCommandWords(text)
	for len(words) > 0 {
		if slices.Contains(shellCommandPrefixes, words[0]) {
			words = words[1:]
		} else if words[0] == "repeat" && len(words) > 1 && !shellNeverRepeats(words[1]) {
			words = words[2:]
		} else {
			break
		}
	}
	for _, word := range words {
		if name, _, ok := strings.Cut(word, "="); !ok || !shellIsName(name) {
			return word
		}
	}
	return ""
}

// shellNeverRepeats는 zsh repeat의 횟수가 0 이하의 숫자라서 명령을 한 번도 실행하지 않는지 본다.
// "0"처럼 따옴표나 \가 붙은 숫자도 읽는다. $n이나 1-1처럼 값을 모르는 횟수는 조건과 같이 평가하지 않는다.
func shellNeverRepeats(count string) bool {
	n, err := strconv.Atoi(strings.NewReplacer(`"`, "", `\`, "").Replace(count))
	return err == nil && n <= 0
}

// shellLastCommandWords는 text를 공백으로 나눈 단어 중 큰따옴표 밖의 마지막 ;, &, |, ( 뒤의 단어다.
// 큰따옴표 안의 공백과 구분자, \ 뒤의 글자는 단어를 나누지 않는다. 따옴표는 단어에 남는다.
func shellLastCommandWords(text string) []string {
	var words []string
	var word strings.Builder
	inWord, quoted := false, false
	end := func() {
		if inWord {
			words = append(words, word.String())
			word.Reset()
			inWord = false
		}
	}
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case c == '\\' && i+1 < len(text):
			word.WriteByte(c)
			i++
			c = text[i]
		case c == '"':
			quoted = !quoted
		case quoted:
		case c == ' ' || c == '\t':
			end()
			continue
		case strings.IndexByte(";&|(", c) >= 0:
			end()
			words = words[:0]
			continue
		}
		word.WriteByte(c)
		inWord = true
	}
	end()
	return words
}

// shellCommandPrefixes는 명령 앞에 와도 그 명령을 실행하는 예약어다. { eval '...'; }와 then eval '...'의 명령은 eval이다.
var shellCommandPrefixes = []string{"{", "!", "if", "then", "else", "elif", "while", "until", "do", "time"}

func shellIsName(text string) bool {
	for i, r := range text {
		if r != '_' && !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return text != ""
}

func shellWordBreak(r rune) bool {
	return strings.ContainsRune(" \t;&|()", r)
}

func renderShell(shell string, aliases bool) string {
	var out strings.Builder
	invocation := "source <(edc shell " + shell
	if !aliases {
		invocation += " --no-alias"
	}
	invocation += ")"
	fmt.Fprintln(&out, "# "+T("command.shell.current_session", invocation))
	fmt.Fprintln(&out, "# "+T("command.shell.startup", "~/."+shell+"rc"))
	fmt.Fprintln(&out, "#   "+invocation)
	fmt.Fprintln(&out, "# "+T("command.shell.read_only"))
	fmt.Fprintln(&out, "export "+shellPIDEnv+"=$$")
	if shell == "zsh" {
		out.WriteString("if (( ! $+functions[compdef] )); then\n  autoload -Uz compinit\n  compinit -D || return\nfi\n")
		out.WriteString(renderCompletion(zshCompletion, zshCommandList()))
	} else {
		out.WriteString(renderCompletion(bashCompletion, bashCommandList()))
	}
	if aliases {
		script := renderAliases(shell, false, aliasConflicts(exec.LookPath))
		_, body, _ := strings.Cut(script, "\n")
		out.WriteString("\n" + body)
	}
	return out.String()
}
