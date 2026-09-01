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
	options, err := promptRemoteOptions(strings.NewReader("1\n\nn\ny\n"), &output, cwd, t.TempDir(), 10*time.Minute, remoteRunOptions{}, remotePromptFlags{interactive: true})
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

func TestRemoteRecipeDiscoveryPrecedenceAndFallback(t *testing.T) {
	cwd := t.TempDir()
	config := t.TempDir()
	configPath := filepath.Join(config, "edc", "recipe.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	writeRemoteFixture(t, configPath, "name: config\nsteps: [{name: gk, command: gk update}]\n")
	path, found := discoverRemoteRecipe(cwd, config)
	if !found || path != configPath {
		t.Fatalf("fallback path = %q, found = %v", path, found)
	}
	cwdPath := filepath.Join(cwd, "recipe.yaml")
	writeRemoteFixture(t, cwdPath, "name: cwd\nsteps: [{name: gk, command: gk update}]\n")
	path, found = discoverRemoteRecipe(cwd, config)
	if !found || path != cwdPath {
		t.Fatalf("path = %q, found = %v", path, found)
	}
}

// group을 인자로 받으면 terminal이 아니어도 inventory와 recipe를 탐색해 실행할 수 있어야 한다.
func TestRemoteGroupArgumentDiscoversFilesWithoutTerminal(t *testing.T) {
	cwd := t.TempDir()
	writeRemoteFixture(t, filepath.Join(cwd, "inventory.yaml"), "hosts: [{name: one}]\ngroups: {daily: [one], weekly: [one]}\n")
	writeRemoteFixture(t, filepath.Join(cwd, "recipe.yaml"), "name: daily\nsteps: [{name: gk, command: git-kit update}]\n")
	var output strings.Builder
	seed := remoteRunOptions{group: "weekly"}
	options, err := promptRemoteOptions(strings.NewReader(""), &output, cwd, t.TempDir(), 10*time.Minute, seed, remotePromptFlags{})
	if err != nil {
		t.Fatal(err)
	}
	if options.group != "weekly" || options.inventoryPath != filepath.Join(cwd, "inventory.yaml") || options.recipePath != filepath.Join(cwd, "recipe.yaml") {
		t.Fatalf("options = %#v", options)
	}
	if output.String() != "" {
		t.Fatalf("non-terminal run wrote output: %q", output.String())
	}
}

// group을 인자로 주면 group이 여러 개여도 -f가 동작하고 streaming 질문을 하지 않는다.
func TestRemoteForceWithGroupArgumentSkipsSelection(t *testing.T) {
	cwd := t.TempDir()
	writeRemoteFixture(t, filepath.Join(cwd, "inventory.yaml"), "hosts: [{name: one}]\ngroups: {daily: [one], weekly: [one]}\n")
	writeRemoteFixture(t, filepath.Join(cwd, "recipe.yaml"), "name: daily\nsteps: [{name: gk, command: git-kit update}]\n")
	var output strings.Builder
	seed := remoteRunOptions{group: "weekly"}
	options, err := promptRemoteOptions(strings.NewReader(""), &output, cwd, t.TempDir(), 10*time.Minute, seed, remotePromptFlags{force: true, interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	if options.group != "weekly" {
		t.Fatalf("options = %#v", options)
	}
	if strings.Contains(output.String(), "(y/N)") {
		t.Fatalf("force prompted: %q", output.String())
	}
}

// group을 인자로 준 실행은 계획과 확인만 거치고 streaming 질문은 건너뛴다.
func TestRemoteGroupArgumentSkipsStreamingQuestion(t *testing.T) {
	cwd := t.TempDir()
	writeRemoteFixture(t, filepath.Join(cwd, "inventory.yaml"), "hosts: [{name: one}]\ngroups: {daily: [one]}\n")
	writeRemoteFixture(t, filepath.Join(cwd, "recipe.yaml"), "name: daily\nsteps: [{name: gk, command: git-kit update}]\n")
	var output strings.Builder
	seed := remoteRunOptions{group: "daily"}
	if _, err := promptRemoteOptions(strings.NewReader("y\n"), &output, cwd, t.TempDir(), 10*time.Minute, seed, remotePromptFlags{interactive: true}); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if strings.Contains(text, "streaming") {
		t.Fatalf("group argument still asks the streaming question: %q", text)
	}
	if !strings.Contains(text, "실행 계획") || !strings.Contains(text, "실행할까요?") {
		t.Fatalf("output = %q", text)
	}
}

func TestRemoteInteractiveWithoutDiscoveredInventory(t *testing.T) {
	cwd := t.TempDir()
	inventoryPath := filepath.Join(cwd, "custom.yaml")
	recipePath := filepath.Join(cwd, "daily.yaml")
	writeRemoteFixture(t, inventoryPath, "hosts: [{name: one, target: one}]\ngroups: {daily: [one]}\n")
	writeRemoteFixture(t, recipePath, "name: daily\nsteps: [{name: gk, command: gk update, verify: gk --version}]\n")
	input := inventoryPath + "\n1\n" + recipePath + "\nn\ny\n"
	options, err := promptRemoteOptions(strings.NewReader(input), &strings.Builder{}, cwd, t.TempDir(), 10*time.Minute, remoteRunOptions{}, remotePromptFlags{interactive: true})
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
	if _, err := promptRemoteOptions(strings.NewReader("2\n"), &strings.Builder{}, cwd, t.TempDir(), 10*time.Minute, remoteRunOptions{}, remotePromptFlags{interactive: true}); err == nil || !strings.Contains(err.Error(), "group 번호") {
		t.Fatalf("invalid selection error = %v", err)
	}

	recipePath := filepath.Join(cwd, "recipe.yaml")
	writeRemoteFixture(t, recipePath, "name: daily\nsteps: [{name: gk, command: gk update, verify: gk --version}]\n")
	if _, err := promptRemoteOptions(strings.NewReader("1\n\nn\nn\n"), &strings.Builder{}, cwd, t.TempDir(), 10*time.Minute, remoteRunOptions{}, remotePromptFlags{interactive: true}); !errors.Is(err, errRemoteCancelled) {
		t.Fatalf("cancel error = %v", err)
	}
}

func TestRemoteInteractiveValidatesRecipeBeforeConfirmation(t *testing.T) {
	cwd := t.TempDir()
	writeRemoteFixture(t, filepath.Join(cwd, "inventory.yaml"), "hosts: [{name: one, target: one}]\ngroups: {daily: [one]}\n")
	invalidRecipe := filepath.Join(cwd, "invalid.yaml")
	writeRemoteFixture(t, invalidRecipe, "name: daily\nsteps: [{name: gk, verify: gk --version}]\n")
	var output strings.Builder
	_, err := promptRemoteOptions(strings.NewReader("1\n"+invalidRecipe+"\n"), &output, cwd, t.TempDir(), 10*time.Minute, remoteRunOptions{}, remotePromptFlags{interactive: true})
	if err == nil || !strings.Contains(err.Error(), "command") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(output.String(), "실행할까요?") {
		t.Fatalf("invalid recipe reached confirmation: %q", output.String())
	}
}

func TestRemotePlanShowsTaggedHosts(t *testing.T) {
	hosts := []remoteHost{{Name: "server", Tags: []string{"linux"}}, {Name: "laptop", Tags: []string{"mac"}}}
	recipe := remoteRecipe{Name: "daily", Steps: []remoteStep{
		{Name: "git-kit", Command: "git-kit update", Verify: "git-kit --version"},
		{Name: "brew", Command: "brew upgrade", Verify: "brew --version", Tags: []string{"mac"}},
		{Name: "apt", Command: "apt-get update", Verify: "apt-get --version", Tags: []string{"bsd"}},
	}}
	var output strings.Builder
	printRemotePlan(&output, "daily", hosts, recipe)
	text := output.String()
	if strings.Contains(text, "1. git-kit\n     hosts") {
		t.Fatalf("untagged step must not list hosts: %q", text)
	}
	for _, expected := range []string{"hosts    laptop", "대상 없음  tag bsd"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("plan %q does not contain %q", text, expected)
		}
	}
}

func TestRemoteForceSkipsQuestions(t *testing.T) {
	cwd := t.TempDir()
	writeRemoteFixture(t, filepath.Join(cwd, "inventory.yaml"), "hosts: [{name: one}]\ngroups: {daily: [one]}\n")
	writeRemoteFixture(t, filepath.Join(cwd, "recipe.yaml"), "name: daily\nsteps: [{name: gk, command: git-kit update, verify: git-kit --version}]\n")
	var output strings.Builder
	options, err := promptRemoteOptions(strings.NewReader(""), &output, cwd, t.TempDir(), 10*time.Minute, remoteRunOptions{}, remotePromptFlags{force: true, interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	if options.group != "daily" {
		t.Fatalf("options = %#v", options)
	}
	if options.recipePath != filepath.Join(cwd, "recipe.yaml") {
		t.Fatalf("options = %#v", options)
	}
	if strings.Contains(output.String(), "(y/N)") || strings.Contains(output.String(), "group 번호") || strings.Contains(output.String(), "recipe 경로 [") {
		t.Fatalf("force prompted: %q", output.String())
	}
}

func TestRemoteForceRequiresDiscoveredFiles(t *testing.T) {
	cwd := t.TempDir()
	if _, err := promptRemoteOptions(strings.NewReader(""), &strings.Builder{}, cwd, t.TempDir(), 10*time.Minute, remoteRunOptions{}, remotePromptFlags{force: true, interactive: true}); err == nil || !strings.Contains(err.Error(), "inventory.yaml") {
		t.Fatalf("missing inventory error = %v", err)
	}
	writeRemoteFixture(t, filepath.Join(cwd, "inventory.yaml"), "hosts: [{name: one}]\ngroups: {daily: [one]}\n")
	_, err := promptRemoteOptions(strings.NewReader(""), &strings.Builder{}, cwd, t.TempDir(), 10*time.Minute, remoteRunOptions{}, remotePromptFlags{force: true, interactive: true})
	if err == nil || !strings.Contains(err.Error(), filepath.Join(cwd, "recipe.yaml")) {
		t.Fatalf("missing recipe error = %v", err)
	}
}

func TestRemoteForceRejectsAmbiguousGroups(t *testing.T) {
	cwd := t.TempDir()
	writeRemoteFixture(t, filepath.Join(cwd, "inventory.yaml"), "hosts: [{name: one}]\ngroups: {daily: [one], weekly: [one]}\n")
	_, err := promptRemoteOptions(strings.NewReader(""), &strings.Builder{}, cwd, t.TempDir(), 10*time.Minute, remoteRunOptions{}, remotePromptFlags{force: true, interactive: true})
	if err == nil || !strings.Contains(err.Error(), "group이 하나") {
		t.Fatalf("error = %v", err)
	}
}
