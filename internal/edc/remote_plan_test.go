package edc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRemoteDryRunAndListRejectConflictingFlags(t *testing.T) {
	if code := runRemoteRun("daily", []string{"--dry-run", "-f"}, "test"); code != 2 {
		t.Fatalf("--dry-run with -f exit code = %d", code)
	}
	if code := runRemoteRun("", []string{"--list", "-n"}, "test"); code != 2 {
		t.Fatalf("--list with --dry-run exit code = %d", code)
	}
	if code := runRemoteRun("", []string{"-l", "-f"}, "test"); code != 2 {
		t.Fatalf("--list with -f exit code = %d", code)
	}
}

func TestRemoteDryRunWritesPlanWithoutRunning(t *testing.T) {
	dir := t.TempDir()
	inventoryPath := filepath.Join(dir, "inventory.yaml")
	recipePath := filepath.Join(dir, "recipe.yaml")
	writeRemoteFixture(t, inventoryPath, "hosts: [{name: server, target: 192.0.2.10, tags: [linux]}, {name: laptop, tags: [mac]}]\ngroups: {daily: [server, laptop]}\nparallel: 2\n")
	writeRemoteFixture(t, recipePath, "name: daily\nsteps: [{name: gk, command: git-kit update, verify: git-kit --version}, {name: brew, command: brew upgrade, tags: [mac], timeout: 30s}]\n")
	output := filepath.Join(dir, "plan.json")
	if code := runRemoteRun("daily", []string{"--inventory", inventoryPath, "--recipe", recipePath, "--dry-run", "--json", output}, "test"); code != 0 {
		t.Fatalf("dry-run exit code = %d", code)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var plan remotePlan
	if err := json.Unmarshal(data, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Group != "daily" || plan.Parallel != 2 || plan.RecipeName != "daily" || len(plan.Hosts) != 2 || len(plan.Steps) != 2 {
		t.Fatalf("plan = %#v", plan)
	}
	if strings.Contains(string(data), "192.0.2.10") || !strings.HasPrefix(plan.Hosts[0].Target, "<ip:") {
		t.Fatalf("host address must be redacted: %s", data)
	}
	if strings.Join(plan.Steps[0].Hosts, ",") != "server,laptop" || strings.Join(plan.Steps[1].Hosts, ",") != "laptop" || plan.Steps[1].Timeout != "30s" {
		t.Fatalf("steps = %#v", plan.Steps)
	}
	if code := runRemoteRun("daily", []string{"--inventory", inventoryPath, "--recipe", recipePath, "--dry-run", "--redact=false", "--json", output}, "test"); code != 0 {
		t.Fatalf("unredacted dry-run exit code = %d", code)
	}
	if data, _ = os.ReadFile(output); !strings.Contains(string(data), "192.0.2.10") {
		t.Fatalf("--redact=false must keep the address: %s", data)
	}
}

func TestRemoteDryRunSkipsConfirmation(t *testing.T) {
	cwd := t.TempDir()
	writeRemoteFixture(t, filepath.Join(cwd, "inventory.yaml"), "hosts: [{name: one}]\ngroups: {daily: [one], weekly: [one]}\n")
	writeRemoteFixture(t, filepath.Join(cwd, "recipe.yaml"), "name: daily\nsteps: [{name: gk, command: git-kit update}]\n")
	var output strings.Builder
	options, err := promptRemoteOptions(strings.NewReader("2\n\n"), &output, cwd, t.TempDir(), 10*time.Minute, remoteRunOptions{}, remotePromptFlags{interactive: true, dryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if options.group != "weekly" || options.recipePath != filepath.Join(cwd, "recipe.yaml") {
		t.Fatalf("options = %#v", options)
	}
	text := output.String()
	if strings.Contains(text, "(y/N)") || strings.Contains(text, "edc remote  weekly") {
		t.Fatalf("dry-run must leave the plan and confirmation to the caller: %q", text)
	}
}

func TestRemoteListWritesInventory(t *testing.T) {
	dir := t.TempDir()
	inventoryPath := filepath.Join(dir, "inventory.yaml")
	writeRemoteFixture(t, inventoryPath, "hosts: [{name: server, target: build.internal, tags: [linux]}, {name: laptop}]\ngroups: {daily: [server, laptop], weekly: [laptop]}\ngroup_options: {weekly: {parallel: 3}}\n")
	inventory, err := loadRemoteInventory(inventoryPath)
	if err != nil {
		t.Fatal(err)
	}
	listing, err := buildRemoteListing(inventoryPath, inventory, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Groups) != 2 || listing.Groups[0].Name != "daily" || listing.Groups[1].Parallel != 3 {
		t.Fatalf("listing = %#v", listing)
	}
	var output strings.Builder
	printRemoteListing(&output, listing)
	for _, expected := range []string{"inventory  " + inventoryPath, "group  daily  (parallel 1, host 2)", "  server  → build.internal  [linux]", "  laptop\n", "group  weekly  (parallel 3, host 1)"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("listing %q does not contain %q", output.String(), expected)
		}
	}
	if _, err := buildRemoteListing(inventoryPath, inventory, "absent"); err == nil {
		t.Fatal("unknown group must fail")
	}
	jsonPath := filepath.Join(dir, "list.json")
	if code := runRemoteRun("weekly", []string{"--inventory", inventoryPath, "--list", "--json", jsonPath}, "test"); code != 0 {
		t.Fatalf("list exit code = %d", code)
	}
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	var decoded remoteListing
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Groups) != 1 || decoded.Groups[0].Name != "weekly" || len(decoded.Groups[0].Hosts) != 1 || decoded.Groups[0].Hosts[0].Tags == nil {
		t.Fatalf("decoded = %#v", decoded)
	}
	if code := runRemoteRun("", []string{"--inventory", filepath.Join(dir, "absent.yaml"), "--list"}, "test"); code != 2 {
		t.Fatalf("missing inventory exit code = %d", code)
	}
}
