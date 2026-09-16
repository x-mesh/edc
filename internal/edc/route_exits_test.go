package edc

import (
	"os"
	"path/filepath"
	"testing"
)

const routeExitsFixture = `dest: default
exits:
  - name: lab-nat-01
    via: 10.20.1.1
    dev: enp1s0
    expect_public_ip: 203.0.113.10
  - name: lab-nat-02
    via: 192.0.2.254
    dev: enp1s0
    expect_public_ip: 203.0.113.20
`

func writeRouteExitsFixture(t *testing.T, dir string) string {
	t.Helper()
	edcDir := filepath.Join(dir, ".edc")
	if err := os.MkdirAll(edcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(edcDir, "exits.yaml")
	if err := os.WriteFile(path, []byte(routeExitsFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadRouteExitsDiscoversDotEdc(t *testing.T) {
	cwd := t.TempDir()
	writeRouteExitsFixture(t, cwd)
	file, path, err := loadRouteExits(cwd, "", "")
	if err != nil {
		t.Fatalf("loadRouteExits error: %v", err)
	}
	if path != filepath.Join(cwd, ".edc", "exits.yaml") {
		t.Fatalf("path = %q", path)
	}
	if file.Dest != "default" || len(file.Exits) != 2 {
		t.Fatalf("file = %#v", file)
	}
	exit, ok := findRouteExit(file, "lab-nat-02")
	if !ok || exit.Via != "192.0.2.254" || exit.Dev != "enp1s0" || exit.ExpectPublicIP != "203.0.113.20" {
		t.Fatalf("exit = %#v, ok=%v", exit, ok)
	}
}

func TestLoadRouteExitsMissingFile(t *testing.T) {
	cwd := t.TempDir()
	if _, _, err := loadRouteExits(cwd, "", ""); err == nil {
		t.Fatal("expected an error when exits.yaml cannot be found")
	}
}

func TestLoadRouteExitsOverridePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom-exits.yaml")
	if err := os.WriteFile(path, []byte(routeExitsFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	file, resolved, err := loadRouteExits(t.TempDir(), "", path)
	if err != nil {
		t.Fatalf("loadRouteExits error: %v", err)
	}
	if resolved != path || len(file.Exits) != 2 {
		t.Fatalf("file=%#v resolved=%q", file, resolved)
	}
}

func TestLoadRouteExitsRejectsIncompleteEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exits.yaml")
	if err := os.WriteFile(path, []byte("exits:\n  - name: broken\n    via: 10.20.1.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadRouteExits(dir, "", path); err == nil {
		t.Fatal("expected an error when an exit entry carries no dev")
	}
}

func TestRouteExitNames(t *testing.T) {
	file := routeExitsFile{Exits: []routeExit{{Name: "lab-nat-01"}, {Name: "lab-nat-02"}}}
	names := routeExitNames(file)
	if len(names) != 2 || names[0] != "lab-nat-01" || names[1] != "lab-nat-02" {
		t.Fatalf("names = %#v", names)
	}
}
