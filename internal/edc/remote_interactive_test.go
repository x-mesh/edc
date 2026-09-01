package edc

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRemoteInteractiveOrderAndDiscovery(t *testing.T) {
	cwd := t.TempDir()
	inventoryPath := filepath.Join(cwd, "inventory.yaml")
	recipePath := filepath.Join(cwd, "recipe.yaml")
	writeRemoteFixture(t, inventoryPath, "hosts: [{name: one, target: one}]\ngroups: {daily: [one]}\n")
	writeRemoteFixture(t, recipePath, "name: daily\nsteps: [{name: gk, command: gk update, verify: gk --version}]\n")
	var output strings.Builder
	options, err := promptRemoteOptions(strings.NewReader("1\n\nn\ny\n"), &output, cwd, t.TempDir(), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if options.group != "daily" || options.inventoryPath != inventoryPath || options.recipePath != recipePath {
		t.Fatalf("options = %#v", options)
	}
	text := output.String()
	groupIndex := strings.Index(text, "group(대상)")
	inventoryIndex := strings.Index(text, "inventory 경로")
	recipeIndex := strings.Index(text, "recipe 경로")
	planIndex := strings.Index(text, "실행 계획")
	streamIndex := strings.Index(text, "상세 출력을 streaming으로 볼까요?")
	confirmIndex := strings.Index(text, "실행할까요?")
	if groupIndex < 0 || !(groupIndex < inventoryIndex && inventoryIndex < recipeIndex && recipeIndex < planIndex && planIndex < streamIndex && streamIndex < confirmIndex) {
		t.Fatalf("prompt order = %q", text)
	}
	for _, expected := range []string{"실행 계획  daily → one", "1. gk", "command  gk update", "verify   gk --version"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("output %q does not contain %q", text, expected)
		}
	}
	if strings.Contains(text, "실행 대상: group=") {
		t.Fatalf("output repeats file paths: %q", text)
	}
}

func TestRemoteInventoryDiscoveryPrecedence(t *testing.T) {
	cwd := t.TempDir()
	config := t.TempDir()
	writeRemoteFixture(t, filepath.Join(cwd, "inventory.yaml"), "hosts: [{name: cwd, target: cwd}]\ngroups: {all: [cwd]}\n")
	configPath := filepath.Join(config, "edc", "inventory.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	writeRemoteFixture(t, configPath, "hosts: [{name: config, target: config}]\ngroups: {all: [config]}\n")
	path, found := discoverRemoteInventory(cwd, config)
	if !found || path != filepath.Join(cwd, "inventory.yaml") {
		t.Fatalf("path = %q, found = %v", path, found)
	}
}

func TestRemoteInventoryDiscoveryFallback(t *testing.T) {
	cwd := t.TempDir()
	config := t.TempDir()
	configPath := filepath.Join(config, "edc", "inventory.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	writeRemoteFixture(t, configPath, "hosts: [{name: config, target: config}]\ngroups: {all: [config]}\n")
	path, found := discoverRemoteInventory(cwd, config)
	if !found || path != configPath {
		t.Fatalf("path = %q, found = %v", path, found)
	}
}

func TestRemoteInteractiveWithoutDiscoveredInventory(t *testing.T) {
	cwd := t.TempDir()
	inventoryPath := filepath.Join(cwd, "custom.yaml")
	recipePath := filepath.Join(cwd, "daily.yaml")
	writeRemoteFixture(t, inventoryPath, "hosts: [{name: one, target: one}]\ngroups: {daily: [one]}\n")
	writeRemoteFixture(t, recipePath, "name: daily\nsteps: [{name: gk, command: gk update, verify: gk --version}]\n")
	input := inventoryPath + "\n1\n" + recipePath + "\nn\ny\n"
	options, err := promptRemoteOptions(strings.NewReader(input), &strings.Builder{}, cwd, t.TempDir(), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if options.inventoryPath != inventoryPath || options.group != "daily" || options.recipePath != recipePath {
		t.Fatalf("options = %#v", options)
	}
}

func TestRemoteInteractiveRejectsSelectionAndCancellation(t *testing.T) {
	cwd := t.TempDir()
	writeRemoteFixture(t, filepath.Join(cwd, "inventory.yaml"), "hosts: [{name: one, target: one}]\ngroups: {daily: [one]}\n")
	if _, err := promptRemoteOptions(strings.NewReader("2\n"), &strings.Builder{}, cwd, t.TempDir(), 10*time.Minute); err == nil || !strings.Contains(err.Error(), "group 번호") {
		t.Fatalf("invalid selection error = %v", err)
	}

	recipePath := filepath.Join(cwd, "recipe.yaml")
	writeRemoteFixture(t, recipePath, "name: daily\nsteps: [{name: gk, command: gk update, verify: gk --version}]\n")
	if _, err := promptRemoteOptions(strings.NewReader("1\n\nn\nn\n"), &strings.Builder{}, cwd, t.TempDir(), 10*time.Minute); !errors.Is(err, errRemoteCancelled) {
		t.Fatalf("cancel error = %v", err)
	}
}

func TestRemoteInteractiveValidatesRecipeBeforeConfirmation(t *testing.T) {
	cwd := t.TempDir()
	writeRemoteFixture(t, filepath.Join(cwd, "inventory.yaml"), "hosts: [{name: one, target: one}]\ngroups: {daily: [one]}\n")
	invalidRecipe := filepath.Join(cwd, "invalid.yaml")
	writeRemoteFixture(t, invalidRecipe, "name: daily\nsteps: [{name: gk, command: gk update}]\n")
	var output strings.Builder
	_, err := promptRemoteOptions(strings.NewReader("1\n"+invalidRecipe+"\n"), &output, cwd, t.TempDir(), 10*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "verify") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(output.String(), "실행할까요?") {
		t.Fatalf("invalid recipe reached confirmation: %q", output.String())
	}
}
