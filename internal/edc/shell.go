package edc

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

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
	if shell == "" {
		shell = filepath.Base(shellEnv)
	}
	if shell != "zsh" && shell != "bash" {
		fmt.Fprintln(stderr, T("command.shell.unsupported_shell", shell))
		return 2
	}
	_, err := io.WriteString(stdout, renderShell(shell, aliases))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	return 0
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
