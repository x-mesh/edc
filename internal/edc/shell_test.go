package edc

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestShellArgumentsAndGuidance(t *testing.T) {
	for _, tc := range []struct {
		args       []string
		env, shell string
		aliases    bool
		code       int
	}{
		{nil, "/bin/zsh", "zsh", true, 0},
		{nil, "/bin/bash", "bash", true, 0},
		{[]string{"zsh"}, "/bin/fish", "zsh", true, 0},
		{[]string{"bash", "--no-alias"}, "", "bash", false, 0},
		{[]string{"--no-alias", "zsh"}, "", "zsh", false, 0},
		{nil, "", "", false, 2},
		{[]string{"fish"}, "", "", false, 2},
		{[]string{"bash", "zsh"}, "", "", false, 2},
		{[]string{"--plain"}, "", "", false, 2},
	} {
		var out, err strings.Builder
		code := runShellWithIO(tc.args, tc.env, &out, &err)
		if code != tc.code {
			t.Fatalf("%v: code %d stderr %s", tc.args, code, &err)
		}
		if code != 0 {
			if out.Len() != 0 {
				t.Fatal("invalid args emitted script")
			}
			continue
		}
		if err.Len() != 0 {
			t.Fatal(err.String())
		}
		for _, want := range []string{"source <(edc shell " + tc.shell, "~/." + tc.shell + "rc", "_edc()"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("missing %q", want)
			}
		}
		if strings.Contains(out.String(), "_edc_alias()") != tc.aliases {
			t.Fatal("alias selection")
		}
		if strings.Contains(out.String(), "Install: echo") {
			t.Fatal("legacy install guide leaked")
		}
	}
}

func TestShellRejectsNonShellArgument(t *testing.T) {
	t.Setenv(shellPIDEnv, "")
	for _, tc := range []struct {
		args []string
		env  string
		key  string
		name string
	}{
		{[]string{"install"}, "/usr/bin/fish", "command.shell.not_shell", "install"},
		{[]string{"fish"}, "/bin/zsh", "command.shell.unsupported_shell", "fish"},
		{[]string{"ash"}, "/bin/zsh", "command.shell.unsupported_shell", "ash"},
		{nil, "/usr/bin/fish", "command.shell.unsupported_shell", "fish"},
	} {
		var out, err strings.Builder
		if code := runShellWithIO(tc.args, tc.env, &out, &err); code != 2 {
			t.Fatalf("%v: code %d", tc.args, code)
		}
		if want := T(tc.key, tc.name) + "\n"; err.String() != want {
			t.Fatalf("%v: stderr %q, want %q", tc.args, err.String(), want)
		}
	}
}

func TestShellNonShellArgumentReportsCurrentSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZDOTDIR", "")
	run := func(env string) string {
		var out, err strings.Builder
		runShellWithIO([]string{"install"}, env, &out, &err)
		return err.String()
	}
	guide := T("command.shell.current_session", "source <(edc shell)") + "\n"
	t.Setenv(shellPIDEnv, "")
	if got := run("/bin/zsh"); !strings.Contains(got, guide) || strings.Contains(got, T("command.shell.loaded")) {
		t.Fatalf("unset: %q", got)
	}
	t.Setenv(shellPIDEnv, strconv.Itoa(os.Getppid()+1))
	if got := run("/bin/zsh"); !strings.Contains(got, guide) {
		t.Fatalf("inherited from another shell: %q", got)
	}
	t.Setenv(shellPIDEnv, strconv.Itoa(os.Getppid()))
	if got := run("/bin/zsh"); !strings.Contains(got, T("command.shell.loaded")+"\n") || strings.Contains(got, guide) {
		t.Fatalf("loaded: %q", got)
	}
	if got := run("/usr/bin/fish"); !strings.Contains(got, T("command.shell.loaded")+"\n") {
		t.Fatalf("loaded under fish $SHELL: %q", got)
	}
}

func TestShellNonShellArgumentReportsStartupFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ZDOTDIR", "")
	t.Setenv(shellPIDEnv, "")
	run := func(env string) string {
		var out, err strings.Builder
		if code := runShellWithIO([]string{"install"}, env, &out, &err); code != 2 || out.Len() != 0 {
			t.Fatalf("code %d stdout %q", code, &out)
		}
		return err.String()
	}
	missing := T("command.shell.startup", "~/.zshrc") + "\n  source <(edc shell zsh)\n"
	if got := run("/bin/zsh"); !strings.HasSuffix(got, missing) {
		t.Fatalf("no rc file: %q", got)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(home, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(".zshrc", "# source <(edc shell zsh)\n")
	if got := run("/bin/zsh"); !strings.HasSuffix(got, missing) {
		t.Fatalf("commented line counted: %q", got)
	}
	// 주석과 작은따옴표 안의 글자는 셸이 실행하지 않는다.
	for _, body := range []string{": # source <(edc shell zsh)\n", "echo '# edc shell'\n", "x=1 #edc shell\n", "echo 'eval' 'edc shell'\n", "eval true; echo 'edc shell'\n", "echo eval 'source <(edc shell zsh)'\n", "{eval 'source <(edc shell zsh)'\n", "echo \"x; eval\" 'source <(edc shell zsh)'\n", "repeat 0 eval 'source <(edc shell zsh)'\n"} {
		write(".zshrc", body)
		if got := run("/bin/zsh"); !strings.HasSuffix(got, missing) {
			t.Fatalf("%q counted: %q", body, got)
		}
	}
	for _, body := range []string{"eval \"$(edc shell zsh)\"\n", "source <(edc shell zsh) # edc\n", "[ -n \"$x\" ]#x; source <(edc shell zsh)\n", "eval 'source <(edc shell zsh)'\n", "x=1; eval 'source <(edc shell zsh)'\n", "EDC_MODE=1 eval 'source <(edc shell zsh)'\n", "{ eval 'source <(edc shell zsh)'; }\n", "if true; then eval 'source <(edc shell zsh)'; fi\n", "repeat 1 eval 'source <(edc shell zsh)'\n", "repeat $n eval 'source <(edc shell zsh)'\n", "EDC_MODE=\"one two\" eval 'source <(edc shell zsh)'\n", "EDC_MODE=\"a;b\" eval 'source <(edc shell zsh)'\n"} {
		write(".zshrc", body)
		if got := run("/bin/zsh"); !strings.HasSuffix(got, T("command.shell.installed", "~/.zshrc")+"\n") {
			t.Fatalf("%q not counted: %q", body, got)
		}
	}
	write(".zshrc", "export X=1\n  source <(edc shell zsh)\n")
	if got := run("/bin/zsh"); !strings.HasSuffix(got, T("command.shell.installed", "~/.zshrc")+"\n") {
		t.Fatalf("installed zsh: %q", got)
	}
	write(".bashrc", "source <(edc shell bash --no-alias)\n")
	if got := run("/bin/bash"); !strings.HasSuffix(got, T("command.shell.installed", "~/.bashrc")+"\n") {
		t.Fatalf("installed bash: %q", got)
	}
	zdot := filepath.Join(home, "zdot")
	if err := os.Mkdir(zdot, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZDOTDIR", zdot)
	if got := run("/bin/zsh"); !strings.HasSuffix(got, T("command.shell.startup", "~/zdot/.zshrc")+"\n  source <(edc shell zsh)\n") {
		t.Fatalf("ZDOTDIR ignored: %q", got)
	}
	if got := run("/usr/bin/fish"); got != T("command.shell.not_shell", "install")+"\n" {
		t.Fatalf("fish: %q", got)
	}
}

func TestShellNonShellArgumentReportsUnreadableStartupFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ZDOTDIR", "")
	t.Setenv(shellPIDEnv, "")
	if err := os.Mkdir(filepath.Join(home, ".zshrc"), 0o700); err != nil {
		t.Fatal(err)
	}
	var out, err strings.Builder
	if code := runShellWithIO([]string{"install"}, "/bin/zsh", &out, &err); code != 2 || out.Len() != 0 {
		t.Fatalf("code %d stdout %q", code, &out)
	}
	got := err.String()
	if !strings.Contains(got, filepath.Join(home, ".zshrc")) || strings.Contains(got, T("command.shell.installed", "~/.zshrc")) || strings.Contains(got, "source <(edc shell zsh)") {
		t.Fatalf("unreadable rc file: %q", got)
	}
}

func TestShellScriptsRegisterInRealShells(t *testing.T) {
	for _, shell := range []string{"bash", "zsh"} {
		t.Run(shell, func(t *testing.T) {
			path, err := exec.LookPath(shell)
			if err != nil {
				t.Skip(err)
			}
			for _, aliases := range []bool{true, false} {
				verify := `[ "$EDC_SHELL_PID" = "$$" ] || exit 3; sh -c '[ "$EDC_SHELL_PID" = "$PPID" ]' || exit 4; complete -p edc; declare -F _edc`
				if shell == "zsh" {
					verify = `[ "$EDC_SHELL_PID" = "$$" ] || exit 3; sh -c '[ "$EDC_SHELL_PID" = "$PPID" ]' || exit 4; print -r -- ${_comps[edc]}; whence -w _edc`
				}
				if aliases {
					verify += "; alias ev"
				} else {
					verify += "; if alias ev >/dev/null 2>&1; then exit 1; fi"
				}
				cmd := exec.Command(path, "-f", "-c", "eval \"$(cat)\" || exit; "+verify)
				if shell == "bash" {
					cmd = exec.Command(path, "--noprofile", "--norc", "-c", "eval \"$(cat)\" || exit; "+verify)
				}
				cmd.Stdin = strings.NewReader(renderShell(shell, aliases))
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("aliases=%t: %v\n%s", aliases, err, out)
				}
				if !strings.Contains(string(out), "_edc") {
					t.Fatalf("completion missing: %s", out)
				}
			}
		})
	}
}

func TestShellPublicHelpAndLegacyHelp(t *testing.T) {
	var out strings.Builder
	printHelp(&out, "dev")
	if !strings.Contains(out.String(), "  shell ") || strings.Contains(out.String(), "  alias ") || strings.Contains(out.String(), "  completion ") {
		t.Fatal(out.String())
	}
	for _, name := range []string{"shell", "alias", "completion"} {
		out.Reset()
		if !printCommandHelp(&out, name) {
			t.Fatalf("missing help %s", name)
		}
	}
	for _, lang := range supportedLanguages {
		previous := currentLanguage()
		setLanguage(lang)
		out.Reset()
		printCommandHelp(&out, "shell")
		setLanguage(previous)
		for _, want := range []string{"~/.zshrc", "~/.bashrc", "source <(edc shell zsh)", "source <(edc shell bash)", "--no-alias"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("%s missing %q", lang, want)
			}
		}
	}
}
