package edc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
	uid, gid := testFileOwner(t, target)
	snapshot := changeSnapshot{SchemaVersion: changeStateSchemaVersion, RunID: "run1", CreatedAt: time.Now().UTC(), Kind: changeKindAuthorizedKeys, Path: target, Existed: true, Mode: 0o600, UID: uid, GID: gid, Data: before, AppliedData: after, UnitName: changeUnitName("run1"), Seconds: 120}
	if err := writeChangeFile(target, after, snapshot.Mode, uid, gid); err != nil {
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
	uid, gid := testFileOwner(t, target)
	snapshot := changeSnapshot{SchemaVersion: changeStateSchemaVersion, RunID: "run1", CreatedAt: time.Now().UTC(), Kind: changeKindAuthorizedKeys, Path: target, Existed: true, Mode: 0o600, UID: uid, GID: gid, Data: before, AppliedData: after, UnitName: changeUnitName("run1"), Seconds: 120}
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

// iptablesSaveAt은 실제 iptables-save의 모양을 따른다. 규칙이 같아도 실행할 때마다 시각 주석과
// 체인의 패킷·바이트 카운터가 바뀐다. 검증 VM에서 2초 간격으로 두 번 실행해 확인했다.
func iptablesSaveAt(stamp string, packets int, rules ...string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "# Generated by iptables-save v1.8.10 (nf_tables) on %s\n*filter\n", stamp)
	fmt.Fprintf(&builder, ":INPUT ACCEPT [%d:%d]\n:FORWARD DROP [0:0]\n:OUTPUT ACCEPT [%d:%d]\n", packets, packets*1500, packets, packets*900)
	for _, rule := range rules {
		builder.WriteString(rule + "\n")
	}
	fmt.Fprintf(&builder, "COMMIT\n# Completed on %s\n", stamp)
	return builder.String()
}

const iptablesExtraRule = "-A INPUT -s 192.0.2.1/32 -j DROP"

func TestIPTablesRollbackIgnoresTimestampsAndCounters(t *testing.T) {
	baseline := iptablesSaveAt("Fri Sep 18 12:10:00 2026", 100)
	applied := iptablesSaveAt("Fri Sep 18 12:10:01 2026", 110, iptablesExtraRule)
	runner := &fakeChangeRunner{sequences: map[string][]string{"iptables-save": {
		iptablesSaveAt("Fri Sep 18 12:10:16 2026", 250, iptablesExtraRule),
		iptablesSaveAt("Fri Sep 18 12:10:17 2026", 260),
	}}}
	snapshot := changeSnapshot{SchemaVersion: changeStateSchemaVersion, RunID: "run1", CreatedAt: time.Now().UTC(), Kind: changeKindIPTables, Data: []byte(baseline), AppliedData: []byte("input-rules"), AppliedState: []byte(applied), UnitName: changeUnitName("run1"), Seconds: 120}
	if err := rollbackChange(context.Background(), runner, snapshot); err != nil {
		t.Fatalf("규칙은 적용 상태 그대로인데 롤백이 거부됐다: %v", err)
	}
	if len(runner.inputs) != 1 || string(runner.inputs[0]) != baseline {
		t.Fatalf("rollback input = %#v, want baseline", runner.inputs)
	}
}

func TestIPTablesRollbackStillRejectsARuleChange(t *testing.T) {
	baseline := iptablesSaveAt("Fri Sep 18 12:10:00 2026", 100)
	applied := iptablesSaveAt("Fri Sep 18 12:10:01 2026", 110, iptablesExtraRule)
	runner := &fakeChangeRunner{outputs: map[string]string{
		"iptables-save": iptablesSaveAt("Fri Sep 18 12:10:16 2026", 250, iptablesExtraRule, "-A INPUT -s 198.51.100.7/32 -j DROP"),
	}}
	snapshot := changeSnapshot{SchemaVersion: changeStateSchemaVersion, RunID: "run1", CreatedAt: time.Now().UTC(), Kind: changeKindIPTables, Data: []byte(baseline), AppliedData: []byte("input-rules"), AppliedState: []byte(applied), UnitName: changeUnitName("run1"), Seconds: 120}
	if err := rollbackChange(context.Background(), runner, snapshot); err == nil {
		t.Fatal("규칙이 바뀐 외부 변경은 여전히 거부해야 한다")
	}
	if len(runner.inputs) != 0 {
		t.Fatalf("외부 변경을 덮어썼다: %#v", runner.inputs)
	}
}

func TestIPTablesRollbackSkipsRestoreWhenRulesAlreadyMatchBaseline(t *testing.T) {
	baseline := iptablesSaveAt("Fri Sep 18 12:10:00 2026", 100)
	runner := &fakeChangeRunner{outputs: map[string]string{"iptables-save": iptablesSaveAt("Fri Sep 18 12:10:30 2026", 400)}}
	snapshot := changeSnapshot{SchemaVersion: changeStateSchemaVersion, RunID: "run1", CreatedAt: time.Now().UTC(), Kind: changeKindIPTables, Data: []byte(baseline), AppliedData: []byte("input-rules"), AppliedState: []byte(iptablesSaveAt("Fri Sep 18 12:10:01 2026", 110, iptablesExtraRule)), UnitName: changeUnitName("run1"), Seconds: 120}
	if err := rollbackChange(context.Background(), runner, snapshot); err != nil {
		t.Fatal(err)
	}
	if len(runner.inputs) != 0 {
		t.Fatalf("이미 기준 상태인데 다시 복원했다: %#v", runner.inputs)
	}
}

func writePendingAuthorizedKeysState(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "change-run1.json")
	snapshot := changeSnapshot{SchemaVersion: changeStateSchemaVersion, RunID: "run1", CreatedAt: time.Now().UTC(), Kind: changeKindAuthorizedKeys, Path: "/tmp/authorized_keys", Existed: true, Mode: 0o600, Data: []byte("ssh-ed25519 AAAA before\n"), AppliedData: []byte("ssh-ed25519 AAAA after\n"), UnitName: changeUnitName("run1"), Seconds: 120}
	if err := writeChangeState(path, snapshot); err != nil {
		t.Fatal(err)
	}
	return path
}

// 타이머가 이미 발화했거나 거둬졌다면 멈출 것이 없다. systemctl stop은 그때 5를 돌려준다.
// 이것을 실패로 막으면 롤백도 확정도 못 하는 대기 상태가 남고 이후 apply가 모두 막힌다.
func TestConfirmChangeSucceedsWhenTimerAlreadyGone(t *testing.T) {
	path := writePendingAuthorizedKeysState(t)
	unit := changeUnitName("run1")
	runner := &fakeChangeRunner{
		outputs: map[string]string{"systemctl is-active " + unit + ".timer": "inactive"},
		errors:  map[string]error{"systemctl stop " + unit + ".timer": errors.New("exit status 5")},
	}
	if err := confirmChange(context.Background(), runner, path); err != nil {
		t.Fatalf("타이머가 이미 없는데 확정이 실패했다: %v", err)
	}
	if got, err := readChangeState(path); err != nil || got.CompletedAt == nil {
		t.Fatalf("확정이 기록되지 않았다: %#v, %v", got, err)
	}
}

// 멈추지 못한 타이머가 아직 살아 있으면 확정한 뒤에도 롤백이 발화한다. 그때는 실패가 맞다.
func TestConfirmChangeFailsWhileTimerStillActive(t *testing.T) {
	path := writePendingAuthorizedKeysState(t)
	unit := changeUnitName("run1")
	runner := &fakeChangeRunner{
		outputs: map[string]string{"systemctl is-active " + unit + ".timer": "active"},
		errors:  map[string]error{"systemctl stop " + unit + ".timer": errors.New("exit status 1")},
	}
	if err := confirmChange(context.Background(), runner, path); err == nil {
		t.Fatal("살아 있는 타이머를 멈추지 못했는데 확정했다")
	}
	if got, err := readChangeState(path); err != nil || got.CompletedAt != nil {
		t.Fatalf("확정되면 안 된다: %#v, %v", got, err)
	}
}

func testFileOwner(t *testing.T, path string) (int, int) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	return int(stat.Uid), int(stat.Gid)
}

// 교체는 임시 파일을 만들어 이름을 바꾼다. 임시 파일은 edc(root)의 것이므로 원래 소유자를 기록해 두지
// 않으면 교체와 롤백 모두 root 소유로 바꾼다. 검증 VM에서 ubuntu:ubuntu가 root:root가 된 채 롤백이
// 성공으로 보고됐다.
func TestAuthorizedKeysSnapshotRecordsOwner(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "authorized_keys")
	content := filepath.Join(directory, "new.pub")
	if err := os.WriteFile(target, []byte("ssh-ed25519 AAAA before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(content, []byte("ssh-ed25519 AAAA after\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := prepareChangeSnapshot(context.Background(), &fakeChangeRunner{}, changeInput{kind: changeKindAuthorizedKeys, path: target, contentFile: content, execID: "run1", seconds: 120})
	if err != nil {
		t.Fatal(err)
	}
	uid, gid := testFileOwner(t, target)
	if snapshot.UID != uid || snapshot.GID != gid {
		t.Fatalf("owner = %d:%d, want %d:%d", snapshot.UID, snapshot.GID, uid, gid)
	}
}

// 새로 만드는 파일은 디렉터리 주인의 것이 된다. 상태에 적힌 소유자가 그 값이어야 적용과 롤백이 맞는다.
func TestAuthorizedKeysSnapshotUsesDirectoryOwnerForNewFile(t *testing.T) {
	directory := t.TempDir()
	content := filepath.Join(directory, "new.pub")
	if err := os.WriteFile(content, []byte("ssh-ed25519 AAAA after\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := prepareChangeSnapshot(context.Background(), &fakeChangeRunner{}, changeInput{kind: changeKindAuthorizedKeys, path: filepath.Join(directory, "authorized_keys"), contentFile: content, execID: "run1", seconds: 120})
	if err != nil {
		t.Fatal(err)
	}
	uid, gid := testFileOwner(t, directory)
	if snapshot.Existed || snapshot.UID != uid || snapshot.GID != gid {
		t.Fatalf("snapshot = existed %v owner %d:%d, want new file owned by %d:%d", snapshot.Existed, snapshot.UID, snapshot.GID, uid, gid)
	}
}

// 소유자가 바뀐 것도 외부 변경이다. 내용과 권한이 같다고 덮어쓰면 다른 사람이 바꾼 소유자를 되돌린다.
func TestAuthorizedKeysRollbackRejectsOwnerChange(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "authorized_keys")
	after := []byte("ssh-ed25519 AAAA after\n")
	if err := os.WriteFile(target, after, 0o600); err != nil {
		t.Fatal(err)
	}
	uid, gid := testFileOwner(t, target)
	snapshot := changeSnapshot{SchemaVersion: changeStateSchemaVersion, RunID: "run1", CreatedAt: time.Now().UTC(), Kind: changeKindAuthorizedKeys, Path: target, Existed: true, Mode: 0o600, UID: uid + 1, GID: gid, Data: []byte("ssh-ed25519 AAAA before\n"), AppliedData: after, UnitName: changeUnitName("run1"), Seconds: 120}
	if err := rollbackChange(context.Background(), &fakeChangeRunner{}, snapshot); err == nil {
		t.Fatal("소유자가 바뀌었는데 덮어썼다")
	}
	if got, _ := os.ReadFile(target); string(got) != string(after) {
		t.Fatalf("target = %q, want untouched %q", got, after)
	}
}

// 2로 쓴 상태에는 소유자가 없다. 3으로 읽으면 uid 0(root)으로 채워지므로 거부해야 한다.
func TestReadChangeStateRejectsSchemaWithoutOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "change.json")
	snapshot := changeSnapshot{SchemaVersion: 2, RunID: "run1", CreatedAt: time.Now().UTC(), Kind: changeKindAuthorizedKeys, Path: "/tmp/authorized_keys", Existed: true, Mode: 0o600, Data: []byte("old"), AppliedData: []byte("new"), UnitName: changeUnitName("run1"), Seconds: 120}
	if err := writeChangeState(path, snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := readChangeState(path); err == nil {
		t.Fatal("소유자가 없는 옛 상태를 받아들였다")
	}
}
