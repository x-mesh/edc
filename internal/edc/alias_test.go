package edc

import (
	"bytes"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestAliasScriptGroupsAliasesAndGuards(t *testing.T) {
	script := renderAliases("zsh", false, nil)
	for _, want := range []string{"source <(edc alias zsh)' >> ~/.zshrc", "_edc_alias() {", "# logs", "_edc_alias eg 'edc grep'", "# diagnose", "# observe", "# record and remote", "unset -f _edc_alias", "# eg timeout /var/log/app/"} {
		if !strings.Contains(script, want) {
			t.Fatalf("script lacks %q:\n%s", want, script)
		}
	}
	// 호스트를 바꾸는 명령은 별칭이 없다.
	for _, name := range []string{"'edc route'", "'edc disk'", "'edc change'"} {
		if strings.Contains(script, name) {
			t.Fatalf("script aliases a host change: %s", name)
		}
	}
	// guard 모드는 겹치는 이름도 정의하되 주석으로 알리고, plain 모드는 그 줄을 막는다.
	guarded := renderAliases("zsh", false, map[string]string{"eg": "a shell builtin"})
	if !strings.Contains(guarded, "# eg is a shell builtin on this host. Use another name, for example: alias xeg='edc grep'") || !strings.Contains(guarded, "\n_edc_alias eg 'edc grep'") {
		t.Fatalf("guarded conflict:\n%s", guarded)
	}
	plain := renderAliases("bash", true, map[string]string{"eg": "/usr/bin/eg"})
	if strings.Contains(plain, "_edc_alias") || !strings.Contains(plain, "# alias eg='edc grep'") || !strings.Contains(plain, "\nalias ev='edc view'") || !strings.Contains(plain, "eg is /usr/bin/eg on this host") || !strings.Contains(plain, "~/.bashrc") {
		t.Fatalf("plain script:\n%s", plain)
	}
}

func TestAliasNamesAreUniqueAndSafe(t *testing.T) {
	seen := map[string]bool{}
	for _, group := range aliasGroups {
		for _, alias := range group.aliases {
			if seen[alias.name] || aliasShellWords[alias.name] || !strings.HasPrefix(alias.command, "edc ") || alias.example == "" || strings.ContainsAny(alias.command, "'\"") {
				t.Fatalf("alias %+v", alias)
			}
			seen[alias.name] = true
		}
	}
}

func TestAliasConflictsFindCommandsOnPath(t *testing.T) {
	conflicts := aliasConflicts(func(name string) (string, error) {
		if name == "edu" {
			return "/usr/local/bin/edu", nil
		}
		return "", errors.New("not found")
	})
	if len(conflicts) != 1 || conflicts["edu"] != "/usr/local/bin/edu" {
		t.Fatalf("conflicts = %v", conflicts)
	}
}

func TestParseAliasArgs(t *testing.T) {
	if shell, plain, help, ok := parseAliasArgs(nil, "/bin/zsh"); shell != "zsh" || plain || help || !ok {
		t.Fatalf("default shell: %q %t %t %t", shell, plain, help, ok)
	}
	if shell, plain, _, ok := parseAliasArgs([]string{"--plain", "bash"}, "/bin/zsh"); shell != "bash" || !plain || !ok {
		t.Fatalf("explicit shell: %q %t %t", shell, plain, ok)
	}
	if _, _, help, _ := parseAliasArgs([]string{"--help"}, ""); !help {
		t.Fatal("--help")
	}
	for _, args := range [][]string{{"zsh", "bash"}, {"--color"}} {
		if _, _, _, ok := parseAliasArgs(args, ""); ok {
			t.Fatalf("%v accepted", args)
		}
	}
}

func TestAliasScriptSkipsAnExistingAliasWhenSourced(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	// 이미 있는 alias는 건너뛰며 알리고, 비어 있는 이름은 정의한다.
	command := exec.Command(bash, "-c", `alias eg='echo mine'; eval "$(cat)"; alias eg; alias ev`)
	command.Stdin = strings.NewReader(renderAliases("bash", false, nil))
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("bash: %v\nstdout %s\nstderr %s", err, &stdout, &stderr)
	}
	if !strings.Contains(stderr.String(), "edc alias: eg is already an alias; skipped. Use another name: alias xeg='edc grep'  or remove yours: unalias eg") {
		t.Fatalf("stderr %q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "echo mine") || !strings.Contains(stdout.String(), "edc view") {
		t.Fatalf("stdout %q", stdout.String())
	}
}
