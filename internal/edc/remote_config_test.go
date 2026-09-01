package edc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRemoteConfig(t *testing.T) {
	directory := t.TempDir()
	inventoryPath := filepath.Join(directory, "inventory.yaml")
	recipePath := filepath.Join(directory, "recipe.yaml")
	writeRemoteFixture(t, inventoryPath, `
hosts:
  - name: first
    target: first-alias
  - name: second
groups:
  daily: [second, first]
parallel: 2
group_options:
  daily:
    parallel: 3
`)
	writeRemoteFixture(t, recipePath, `
name: daily
steps:
  - name: update
    command: gk update
    verify: gk --version
    timeout: 30s
`)
	inventory, err := loadRemoteInventory(inventoryPath)
	if err != nil {
		t.Fatal(err)
	}
	hosts, err := hostsForRemoteGroup(inventory, "daily")
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 2 || hosts[0].Name != "first" || hosts[1].Name != "second" {
		t.Fatalf("host order = %#v", hosts)
	}
	if hosts[1].Target != "second" {
		t.Fatalf("default target = %q", hosts[1].Target)
	}
	if got := remoteParallelForGroup(inventory, "daily", 0); got != 3 {
		t.Fatalf("parallel = %d", got)
	}
	if got := remoteParallelForGroup(inventory, "daily", 4); got != 4 {
		t.Fatalf("override parallel = %d", got)
	}
	recipe, err := loadRemoteRecipe(recipePath, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if recipe.Steps[0].Timeout != 30*time.Second {
		t.Fatalf("timeout = %s", recipe.Steps[0].Timeout)
	}
}

func TestRemoteTagFilter(t *testing.T) {
	directory := t.TempDir()
	inventoryPath := filepath.Join(directory, "inventory.yaml")
	recipePath := filepath.Join(directory, "recipe.yaml")
	writeRemoteFixture(t, inventoryPath, `
hosts:
  - name: server
    tags: [linux]
  - name: laptop
    tags: [mac, personal]
groups:
  daily: [server, laptop]
`)
	writeRemoteFixture(t, recipePath, `
name: daily
steps:
  - name: git-kit
    command: git-kit update
    verify: git-kit --version
  - name: brew
    tags: [mac]
    command: brew upgrade
    verify: brew --version
  - name: apt
    tags: [linux, bsd]
    command: apt-get update
    verify: apt-get --version
`)
	inventory, err := loadRemoteInventory(inventoryPath)
	if err != nil {
		t.Fatal(err)
	}
	recipe, err := loadRemoteRecipe(recipePath, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	hosts, err := hostsForRemoteGroup(inventory, "daily")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"git-kit": {"server", "laptop"}, "brew": {"laptop"}, "apt": {"server"}}
	for _, step := range recipe.Steps {
		got := stepHostNames(step, hosts)
		if strings.Join(got, ",") != strings.Join(want[step.Name], ",") {
			t.Fatalf("step %q hosts = %v, want %v", step.Name, got, want[step.Name])
		}
	}
}

func TestRemoteConfigRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name, content, want string
	}{
		{"unknown field", "hosts: []\ngroups: {}\nsecret: value\n", "field secret not found"},
		{"duplicate host", "hosts:\n- {name: one, target: one}\n- {name: one, target: two}\ngroups: {all: [one]}\n", "중복 host"},
		{"unknown member", "hosts: [{name: one, target: one}]\ngroups: {all: [two]}\n", "알 수 없는 host"},
		{"empty tag", "hosts: [{name: one, tags: [mac, \"  \"]}]\ngroups: {all: [one]}\n", "tag는 비어 있을 수 없습니다"},
		{"duplicate tag", "hosts: [{name: one, tags: [mac, mac]}]\ngroups: {all: [one]}\n", "중복 tag"},
		{"reserved group", "hosts: [{name: one}]\ngroups: {run: [one]}\n", "예약되어 있습니다"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "inventory.yaml")
			writeRemoteFixture(t, path, test.content)
			_, err := loadRemoteInventory(path)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRemoteRecipeRejectsDuplicateTag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recipe.yaml")
	writeRemoteFixture(t, path, "name: daily\nsteps: [{name: brew, command: brew upgrade, verify: brew --version, tags: [mac, mac]}]\n")
	if _, err := loadRemoteRecipe(path, time.Minute); err == nil || !strings.Contains(err.Error(), "중복 tag") {
		t.Fatalf("error = %v", err)
	}
}

func TestRemoteRecipeRequiresCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recipe.yaml")
	writeRemoteFixture(t, path, "name: daily\nsteps: [{name: update, verify: gk --version}]\n")
	if _, err := loadRemoteRecipe(path, time.Minute); err == nil || !strings.Contains(err.Error(), "command") {
		t.Fatalf("error = %v", err)
	}
}

func TestRemoteRecipeAllowsMissingVerify(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recipe.yaml")
	writeRemoteFixture(t, path, "name: daily\nsteps: [{name: brew, command: brew update}, {name: empty, command: brew upgrade, verify: \"\"}]\n")
	recipe, err := loadRemoteRecipe(path, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if recipe.Steps[0].Verify != "" || recipe.Steps[1].Verify != "" {
		t.Fatalf("steps = %#v", recipe.Steps)
	}
}

func TestRemoteConfigRejectsLimits(t *testing.T) {
	t.Run("oversized inventory", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "inventory.yaml")
		content := strings.Repeat("#", remoteConfigLimit+1)
		writeRemoteFixture(t, path, content)
		if _, err := loadRemoteInventory(path); err == nil || !strings.Contains(err.Error(), "파일 크기") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("unbounded timeout", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "recipe.yaml")
		writeRemoteFixture(t, path, "name: daily\nsteps: [{name: update, command: gk update, verify: gk --version, timeout: 25h}]\n")
		if _, err := loadRemoteRecipe(path, time.Minute); err == nil || !strings.Contains(err.Error(), "timeout") {
			t.Fatalf("error = %v", err)
		}
	})
}

func writeRemoteFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.TrimSpace(content)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}
