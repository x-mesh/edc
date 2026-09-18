package edc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeChangeRunner struct {
	outputs   map[string]string
	errors    map[string]error
	sequences map[string][]string
	inputs    [][]byte
}

func (runner *fakeChangeRunner) run(_ context.Context, name string, args ...string) (string, error) {
	key := strings.Join(append([]string{name}, args...), " ")
	if queue := runner.sequences[key]; len(queue) > 0 {
		runner.sequences[key] = queue[1:]
		return queue[0], nil
	}
	return runner.outputs[key], runner.errors[key]
}

func (runner *fakeChangeRunner) runInput(_ context.Context, input []byte, name string, args ...string) (string, error) {
	runner.inputs = append(runner.inputs, append([]byte(nil), input...))
	key := strings.Join(append([]string{name}, args...), " ")
	return runner.outputs[key], runner.errors[key]
}

func TestNormalizeChangeKindAllowsOnlyTypedAdapters(t *testing.T) {
	for _, value := range []string{"", "shell", "ssh", "iptables-save", "authorized_keys"} {
		if kind, ok := normalizeChangeKind(value); ok || kind != "" {
			t.Fatalf("normalizeChangeKind(%q) = %q, %v", value, kind, ok)
		}
	}
	for _, value := range []string{changeKindAuthorizedKeys, changeKindIPTables} {
		if kind, ok := normalizeChangeKind(value); !ok || kind != value {
			t.Fatalf("normalizeChangeKind(%q) = %q, %v", value, kind, ok)
		}
	}
}

func TestChangeSystemdRunArgs(t *testing.T) {
	want := []string{"--collect", "--on-active=120", "--timer-property=AccuracySec=1s", "--unit=edc-change-rollback-run1", "/usr/local/bin/edc", "change", "rollback", "--state", "/run/edc/change-run1.json"}
	got := changeSystemdRunArgs("edc-change-rollback-run1", 120, "/run/edc/change-run1.json", "/usr/local/bin/edc")
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestChangeStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "change.json")
	snapshot := changeSnapshot{SchemaVersion: changeStateSchemaVersion, RunID: "run1", CreatedAt: time.Unix(1, 0).UTC(), Kind: changeKindAuthorizedKeys, Path: "/tmp/authorized_keys", Existed: true, Mode: 0o600, Data: []byte("ssh-ed25519 AAAA\n"), AppliedData: []byte("ssh-ed25519 BBBB\n"), UnitName: changeUnitName("run1"), Seconds: 120}
	if err := writeChangeState(path, snapshot); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"schema_version", "run_id", "created_at", "unit_name"} {
		if !strings.Contains(string(data), "\""+field+"\"") {
			t.Fatalf("state JSON misses %q: %s", field, data)
		}
	}
	got, err := readChangeState(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Data) != string(snapshot.Data) || got.UnitName != snapshot.UnitName || got.Mode != snapshot.Mode {
		t.Fatalf("state = %#v, want %#v", got, snapshot)
	}
}

func TestAuthorizedKeysRollbackRestoresSnapshot(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "authorized_keys")
	before := []byte("ssh-ed25519 AAAA before\n")
	after := []byte("ssh-ed25519 AAAA after\n")
	if err := os.WriteFile(target, before, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot := changeSnapshot{SchemaVersion: changeStateSchemaVersion, RunID: "run1", CreatedAt: time.Now().UTC(), Kind: changeKindAuthorizedKeys, Path: target, Existed: true, Mode: 0o600, Data: before, AppliedData: after, UnitName: changeUnitName("run1"), Seconds: 120}
	if err := writeChangeFile(target, after, snapshot.Mode); err != nil {
		t.Fatal(err)
	}
	if err := rollbackChange(context.Background(), &fakeChangeRunner{}, snapshot); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(before) {
		t.Fatalf("target = %q, want %q", got, before)
	}
}

func TestAuthorizedKeysRollbackDoesNotOverwriteExternalChange(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "authorized_keys")
	before := []byte("ssh-ed25519 AAAA before\n")
	after := []byte("ssh-ed25519 AAAA after\n")
	external := []byte("ssh-ed25519 AAAA external\n")
	if err := os.WriteFile(target, external, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot := changeSnapshot{SchemaVersion: changeStateSchemaVersion, RunID: "run1", CreatedAt: time.Now().UTC(), Kind: changeKindAuthorizedKeys, Path: target, Existed: true, Mode: 0o600, Data: before, AppliedData: after, UnitName: changeUnitName("run1"), Seconds: 120}
	if err := rollbackChange(context.Background(), &fakeChangeRunner{}, snapshot); err == nil {
		t.Fatal("rollback must reject an external target change")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(external) {
		t.Fatalf("target = %q, want external content %q", got, external)
	}
}

func TestReadChangeStateRejectsMismatchedUnitAndMissingAppliedData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "change.json")
	snapshot := changeSnapshot{SchemaVersion: changeStateSchemaVersion, RunID: "run1", CreatedAt: time.Now().UTC(), Kind: changeKindAuthorizedKeys, Path: "/tmp/authorized_keys", Existed: true, Mode: 0o600, Data: []byte("old"), UnitName: "other", Seconds: 120}
	if err := writeChangeState(path, snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := readChangeState(path); err == nil {
		t.Fatal("invalid rollback state must be rejected")
	}
}

func TestAuthorizedKeysRejectsPrivateKeyAndSymlink(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "authorized_keys")
	if err := os.WriteFile(path, []byte("ssh-ed25519 AAAA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(directory, "private")
	if err := os.WriteFile(private, []byte("-----BEGIN OPENSSH PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	input := changeInput{kind: changeKindAuthorizedKeys, path: path, contentFile: private, execID: "run1", seconds: 120}
	if _, err := prepareChangeSnapshot(context.Background(), &fakeChangeRunner{}, input); err == nil {
		t.Fatal("expected private key rejection")
	}
	link := filepath.Join(directory, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readChangeFile(link); err == nil {
		t.Fatal("expected symlink rejection")
	}
}

func TestIPTablesRollbackUsesAppliedStateAndRestoresBaseline(t *testing.T) {
	runner := &fakeChangeRunner{
		sequences: map[string][]string{"iptables-save": {"applied-state", "baseline"}},
	}
	snapshot := changeSnapshot{SchemaVersion: changeStateSchemaVersion, RunID: "run1", CreatedAt: time.Now().UTC(), Kind: changeKindIPTables, Data: []byte("baseline"), AppliedData: []byte("input-rules"), AppliedState: []byte("applied-state"), UnitName: changeUnitName("run1"), Seconds: 120}
	if err := rollbackChange(context.Background(), runner, snapshot); err != nil {
		t.Fatal(err)
	}
	if len(runner.inputs) != 1 || string(runner.inputs[0]) != "baseline" {
		t.Fatalf("rollback input = %#v, want baseline", runner.inputs)
	}
}

func TestIPTablesRollbackRejectsExternalState(t *testing.T) {
	runner := &fakeChangeRunner{outputs: map[string]string{"iptables-save": "external-state"}}
	snapshot := changeSnapshot{SchemaVersion: changeStateSchemaVersion, RunID: "run1", CreatedAt: time.Now().UTC(), Kind: changeKindIPTables, Data: []byte("baseline"), AppliedData: []byte("input-rules"), AppliedState: []byte("applied-state"), UnitName: changeUnitName("run1"), Seconds: 120}
	if err := rollbackChange(context.Background(), runner, snapshot); err == nil {
		t.Fatal("rollback must reject an external firewall change")
	}
	if len(runner.inputs) != 0 {
		t.Fatalf("rollback must not overwrite external state: %#v", runner.inputs)
	}
}

func TestIPTablesRollbackRestoresWhenAppliedStateWasNotSaved(t *testing.T) {
	runner := &fakeChangeRunner{
		sequences: map[string][]string{"iptables-save": {"changed-state", "baseline"}},
	}
	snapshot := changeSnapshot{SchemaVersion: changeStateSchemaVersion, RunID: "run1", CreatedAt: time.Now().UTC(), Kind: changeKindIPTables, Data: []byte("baseline"), AppliedData: []byte("input-rules"), UnitName: changeUnitName("run1"), Seconds: 120}
	if err := rollbackChange(context.Background(), runner, snapshot); err != nil {
		t.Fatal(err)
	}
	if len(runner.inputs) != 1 || string(runner.inputs[0]) != "baseline" {
		t.Fatalf("rollback input = %#v, want baseline", runner.inputs)
	}
}

func TestValidateChangeStatePathRestrictsStateFiles(t *testing.T) {
	valid, err := validateChangeStatePath("/run/edc/change-run1.json")
	if err != nil || valid != "/run/edc/change-run1.json" {
		t.Fatalf("valid state path = %q, %v", valid, err)
	}
	for _, path := range []string{"/tmp/change-run1.json", "/run/edc/route-run1.json", "/run/edc/change-run1.txt"} {
		if _, err := validateChangeStatePath(path); err == nil {
			t.Fatalf("state path %q must be rejected", path)
		}
	}
}

func TestChangeLockRejectsConcurrentStateHandling(t *testing.T) {
	path := filepath.Join(t.TempDir(), "change.json")
	lock := path + ".lock"
	file, err := os.OpenFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	if err := withChangeLock(path, func() error { return nil }); err == nil || !errors.Is(err, os.ErrExist) {
		t.Fatalf("withChangeLock error = %v, want an existing lock error", err)
	}
}
