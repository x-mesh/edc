package edc

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestCompletionScriptsCoverCommandsAndRemoteFlags(t *testing.T) {
	for name, script := range map[string]string{"zsh": zshCompletion, "bash": bashCompletion} {
		for _, expected := range []string{"remote", "--dry-run", "--list", "--min-days", "--expect-status", "report", "diff", "edc completion groups"} {
			if !strings.Contains(script, expected) {
				t.Fatalf("%s completion does not mention %q", name, expected)
			}
		}
	}
	if !strings.HasPrefix(zshCompletion, "#compdef edc\n") {
		t.Fatalf("zsh script must start with #compdef: %q", zshCompletion[:20])
	}
	if !strings.HasSuffix(strings.TrimSpace(bashCompletion), "complete -F _edc edc") {
		t.Fatal("bash script must register the completion function")
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
