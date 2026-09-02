package edc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestRemoteYAMLFilesScansBothDirectories(t *testing.T) {
	cwd := t.TempDir()
	config := t.TempDir()
	writeRemoteFixture(t, filepath.Join(cwd, "b.yaml"), "name: b\nsteps: [{name: s, command: c}]\n")
	writeRemoteFixture(t, filepath.Join(cwd, "a.yml"), "name: a\nsteps: [{name: s, command: c}]\n")
	writeRemoteFixture(t, filepath.Join(cwd, "notes.txt"), "ignored\n")
	if err := os.MkdirAll(filepath.Join(config, "edc"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeRemoteFixture(t, filepath.Join(config, "edc", "c.yaml"), "name: c\nsteps: [{name: s, command: c}]\n")
	paths := remoteYAMLFiles(cwd, config)
	want := []string{filepath.Join(cwd, "a.yml"), filepath.Join(cwd, "b.yaml"), filepath.Join(config, "edc", "c.yaml")}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("paths = %#v, want %#v", paths, want)
	}
}

func TestRemoteCandidatesKeepReadableFilesOnly(t *testing.T) {
	cwd := t.TempDir()
	writeRemoteFixture(t, filepath.Join(cwd, "inventory.yaml"), "hosts: [{name: one}, {name: two}]\ngroups: {daily: [one], weekly: [two]}\n")
	writeRemoteFixture(t, filepath.Join(cwd, "recipe.yaml"), "name: daily-update\nsteps: [{name: gk, command: gk update}, {name: xm, command: xm update}]\n")
	writeRemoteFixture(t, filepath.Join(cwd, "broken.yaml"), "not: a recipe\n")
	inventories := remoteInventoryCandidates(cwd, "")
	if len(inventories) != 1 || inventories[0].value != filepath.Join(cwd, "inventory.yaml") {
		t.Fatalf("inventory candidates = %#v", inventories)
	}
	if !strings.Contains(inventories[0].label, T("remote.label.inventory_candidate", "inventory.yaml", 2, 2)) {
		t.Fatalf("inventory label = %q", inventories[0].label)
	}
	recipes := remoteRecipeCandidates(cwd, "", time.Minute)
	if len(recipes) != 1 || recipes[0].value != filepath.Join(cwd, "recipe.yaml") {
		t.Fatalf("recipe candidates = %#v", recipes)
	}
	if !strings.Contains(recipes[0].label, T("remote.label.recipe_candidate", "recipe.yaml", "daily-update", 2)) {
		t.Fatalf("recipe label = %q", recipes[0].label)
	}
}

func TestRemoteDisplayPathShortensLocalFiles(t *testing.T) {
	cwd := t.TempDir()
	if got := remoteDisplayPath(cwd, filepath.Join(cwd, "inventory.yaml")); got != "inventory.yaml" {
		t.Fatalf("local path = %q", got)
	}
	outside := filepath.Join(t.TempDir(), "inventory.yaml")
	if got := remoteDisplayPath(cwd, outside); got != outside {
		t.Fatalf("outside path = %q", got)
	}
}

func TestSelectModelStartsAtDefaultValue(t *testing.T) {
	items := []selectItem{{label: "a", value: "/a"}, {label: "b", value: "/b"}}
	model := newSelectModel(selectRecipeTitle, selectRecipeLabel, items).withCursorAt("/b")
	if model.cursor != 1 {
		t.Fatalf("cursor = %d", model.cursor)
	}
	if missing := newSelectModel(selectRecipeTitle, selectRecipeLabel, items).withCursorAt("/absent"); missing.cursor != 0 {
		t.Fatalf("unknown default must keep the first item: %d", missing.cursor)
	}
	chosen, _ := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	value, err := chosen.(selectModel).choice()
	if err != nil || value != "/b" {
		t.Fatalf("choice = %q, %v", value, err)
	}
	// 고른 값은 › 표시가 보여 주고 첫 줄은 이름표만 남는다.
	content := chosen.(selectModel).View().Content
	if !strings.HasPrefix(content, T(selectRecipeLabel)+"\n") || !strings.Contains(content, liveSelectedBar+" b") {
		t.Fatalf("final view = %q", content)
	}
}

func TestSelectModelShowsLabelsNotValues(t *testing.T) {
	items := []selectItem{{label: "recipe.yaml  ·  daily, step 2개", value: "/tmp/recipe.yaml"}}
	view := newSelectModel(selectRecipeTitle, selectRecipeLabel, items).View().Content
	if !strings.Contains(view, "recipe.yaml  ·  daily, step 2개") || strings.Contains(view, "/tmp/recipe.yaml") {
		t.Fatalf("view = %q", view)
	}
}
