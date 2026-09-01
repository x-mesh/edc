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
    target: second-alias
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

func TestRemoteConfigRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name, content, want string
	}{
		{"unknown field", "hosts: []\ngroups: {}\nsecret: value\n", "field secret not found"},
		{"duplicate host", "hosts:\n- {name: one, target: one}\n- {name: one, target: two}\ngroups: {all: [one]}\n", "중복 host"},
		{"unknown member", "hosts: [{name: one, target: one}]\ngroups: {all: [two]}\n", "알 수 없는 host"},
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

func TestRemoteRecipeRequiresVerify(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recipe.yaml")
	writeRemoteFixture(t, path, "name: daily\nsteps: [{name: update, command: gk update}]\n")
	if _, err := loadRemoteRecipe(path, time.Minute); err == nil || !strings.Contains(err.Error(), "verify") {
		t.Fatalf("error = %v", err)
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
