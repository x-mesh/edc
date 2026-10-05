package edc

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompletionScriptsCoverCommandsAndRemoteFlags(t *testing.T) {
	for name, script := range map[string]string{"zsh": zshCompletion, "bash": bashCompletion} {
		for _, expected := range []string{"remote", "--dry-run", "--list", "--min-days", "--expect-status", "report", "diff", "edc completion groups", "log", "--stream", "--command-display", "stdout stderr", "full name none"} {
			if !strings.Contains(script, expected) {
				t.Fatalf("%s completion does not mention %q", name, expected)
			}
		}
	}
	if !strings.Contains(zshCompletion, "1:separator:(--)") {
		t.Fatal("zsh log completion must require -- before the child command")
	}
	if !strings.Contains(bashCompletion, "COMP_WORDS[index]} == --") {
		t.Fatal("bash completion must stop parsing edc options after --")
	}
	if !strings.HasPrefix(zshCompletion, "#compdef edc\n") {
		t.Fatalf("zsh script must start with #compdef: %q", zshCompletion[:20])
	}
	if !strings.HasSuffix(strings.TrimSpace(bashCompletion), "complete -F _edc edc") {
		t.Fatal("bash script must register the completion function")
	}
	if !strings.Contains(zshCompletion, "--group-by[그룹 기준]:group:(source target port process event)") {
		t.Fatal("zsh trace completion must offer source, target, port, process, and event group values")
	}
	if !strings.Contains(bashCompletion, "--group-by") {
		t.Fatal("bash trace completion must keep the group-by flag")
	}
}

func TestCompletionGroupsListInventoryGroups(t *testing.T) {
	cwd := t.TempDir()
	writeRemoteFixture(t, filepath.Join(cwd, "inventory.yaml"), "hosts: [{name: one}]\ngroups: {weekly: [one], daily: [one]}\n")
	var output strings.Builder
	if code := writeCompletionGroups(&output, cwd, t.TempDir()); code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if output.String() != "daily\nweekly\n" {
		t.Fatalf("groups = %q", output.String())
	}
	if code := writeCompletionGroups(&strings.Builder{}, t.TempDir(), t.TempDir()); code != 2 {
		t.Fatalf("missing inventory exit code = %d", code)
	}
}

func TestCompletionGroupsUsesConfiguredInventory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom.yaml")
	writeRemoteFixture(t, path, "hosts: [{name: one}]\ngroups: {configured: [one]}\n")
	var output strings.Builder
	if code := writeCompletionGroupsWithPath(&output, t.TempDir(), t.TempDir(), path); code != 0 || output.String() != "configured\n" {
		t.Fatalf("code=%d output=%q", code, output.String())
	}
}

func TestBashHistoryCompletionHonorsArgumentBoundary(t *testing.T) {
	for _, row := range []struct {
		words      string
		wantOption bool
	}{
		{`(edc log history --f)`, true},
		{`(edc log history --limit 5 --f)`, true},
		{`(edc log history ls --f)`, false},
		{`(edc log history ls -- --f)`, false},
		{`(edc log history -- ls --f)`, false},
	} {
		command := bashCompletion + "\nCOMP_WORDS=" + row.words + "\nCOMP_CWORD=$((${#COMP_WORDS[@]}-1))\n_edc\nprintf '%s\n' \"${COMPREPLY[@]}\"\n"
		output, err := exec.Command("bash", "-c", command).CombinedOutput()
		if err != nil {
			t.Fatalf("completion error: %v: %s", err, output)
		}
		if strings.Contains(string(output), "--failed") != row.wantOption {
			t.Fatalf("completion changed argv boundary for %s: %s", row.words, output)
		}
	}
}

func TestBashWatchCompletionSeparatesHTTPAndFS(t *testing.T) {
	for _, test := range []struct{ words, want, absent string }{
		{`(edc watch f)`, "fs", "--exec"},
		{`(edc watch http --d)`, "--duration", "--dry-run"},
		{`(edc watch fs --e)`, "--exec", "--expect-status"},
		{`(edc watch fs --event c)`, "create", "--exec"},
		{`(edc watch fs --exec --e)`, "", "--exec"},
	} {
		command := bashCompletion + "\nCOMP_WORDS=" + test.words + "\nCOMP_CWORD=$((${#COMP_WORDS[@]}-1))\n_edc\nprintf '%s\n' \"${COMPREPLY[@]}\"\n"
		output, err := exec.Command("bash", "-c", command).CombinedOutput()
		if err != nil {
			t.Fatalf("completion: %v %s", err, output)
		}
		if test.want != "" && !strings.Contains(string(output), test.want) {
			t.Fatalf("%s missing %s: %s", test.words, test.want, output)
		}
		if strings.Contains(string(output), test.absent) {
			t.Fatalf("%s unexpected %s: %s", test.words, test.absent, output)
		}
	}
}
