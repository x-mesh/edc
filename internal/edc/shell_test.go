package edc

import (
	"os/exec"
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

func TestShellScriptsRegisterInRealShells(t *testing.T) {
	for _, shell := range []string{"bash", "zsh"} {
		t.Run(shell, func(t *testing.T) {
			path, err := exec.LookPath(shell)
			if err != nil {
				t.Skip(err)
			}
			for _, aliases := range []bool{true, false} {
				verify := "complete -p edc; declare -F _edc"
				if shell == "zsh" {
					verify = "print -r -- ${_comps[edc]}; whence -w _edc"
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
