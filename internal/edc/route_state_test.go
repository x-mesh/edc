package edc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sampleRouteSnapshot() routeSnapshot {
	entries, _ := parseRouteTable(routeTableFixture)
	return routeSnapshot{
		SchemaVersion:  routeStateSchemaVersion,
		RunID:          "abc123",
		CreatedAt:      time.Now().UTC().Truncate(time.Second),
		Dest:           "default",
		RouteTableText: routeTableFixture,
		IPRuleText:     ipRuleFixture,
		TargetEntry:    entries[0],
		NewVia:         "192.0.2.254",
		CountBaseline:  1,
		UnitName:       "edc-route-rollback-abc123",
		SSHConnection:  "100.64.0.2 50015 100.64.0.1 22",
		ExitName:       "lab-nat-02",
	}
}

func TestWriteReadRouteStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run", "route-abc123.json")
	snapshot := sampleRouteSnapshot()
	if err := writeRouteState(path, snapshot); err != nil {
		t.Fatalf("writeRouteState error: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat error: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode = %v, want 0600", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir error: %v", err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("state dir mode = %v, want 0700", dirInfo.Mode().Perm())
	}
	loaded, err := readRouteState(path)
	if err != nil {
		t.Fatalf("readRouteState error: %v", err)
	}
	if loaded.RunID != snapshot.RunID || loaded.Dest != snapshot.Dest || loaded.UnitName != snapshot.UnitName ||
		loaded.ExitName != snapshot.ExitName || loaded.NewVia != snapshot.NewVia || loaded.CountBaseline != snapshot.CountBaseline ||
		!loaded.CreatedAt.Equal(snapshot.CreatedAt) || loaded.TargetEntry != snapshot.TargetEntry {
		t.Fatalf("round trip mismatch: got %#v, want %#v", loaded, snapshot)
	}
}

func TestReadRouteStateRejectsWrongSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "route.json")
	snapshot := sampleRouteSnapshot()
	snapshot.SchemaVersion = routeStateSchemaVersion + 1
	if err := writeRouteState(path, snapshot); err != nil {
		t.Fatalf("writeRouteState error: %v", err)
	}
	if _, err := readRouteState(path); err == nil {
		t.Fatal("expected an error for a mismatched schema version")
	}
}

func TestReadRouteStateRejectsMissingFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "route.json")
	snapshot := sampleRouteSnapshot()
	snapshot.RunID = ""
	if err := writeRouteState(path, snapshot); err != nil {
		t.Fatalf("writeRouteState error: %v", err)
	}
	if _, err := readRouteState(path); err == nil {
		t.Fatal("expected an error for a missing run id")
	}
}

func TestRollbackCommandsRestoresAndDeletesResidual(t *testing.T) {
	entries, _ := parseRouteTable(routeMetricTrapFixture)
	snapshot := routeSnapshot{
		Dest:          "8.8.8.8",
		TargetEntry:   entries[0], // metric 100 via 10.20.1.1
		CountBaseline: 1,
	}
	ops, err := rollbackOps(snapshot, mustParseRoutes(routeMetricTrapFixture))
	if err != nil {
		t.Fatalf("rollbackOps error: %v", err)
	}
	if len(renderRouteOps(ops)) != 2 {
		t.Fatalf("renderRouteOps(ops) = %#v, want 2 (restore + delete residual)", renderRouteOps(ops))
	}
	restore := strings.Join(renderRouteOps(ops)[0], " ")
	if restore != "route replace 8.8.8.8 via 10.20.1.1 dev enp1s0 metric 100" {
		t.Fatalf("restore command = %q", restore)
	}
	deleteResidual := strings.Join(renderRouteOps(ops)[1], " ")
	if deleteResidual != "route del 8.8.8.8 via 192.0.2.254 dev enp1s0" {
		t.Fatalf("delete command = %q", deleteResidual)
	}
}

func TestRollbackCommandsSkipsDeleteWhenCountMatchesBaseline(t *testing.T) {
	entries, _ := parseRouteTable(routeTableFixture)
	snapshot := routeSnapshot{
		Dest:          "default",
		TargetEntry:   entries[0],
		CountBaseline: 1,
	}
	ops, err := rollbackOps(snapshot, mustParseRoutes(routeTableFixture))
	if err != nil {
		t.Fatalf("rollbackOps error: %v", err)
	}
	if len(renderRouteOps(ops)) != 1 {
		t.Fatalf("renderRouteOps(ops) = %#v, want only the restore command", renderRouteOps(ops))
	}
}

// mustParseRoutes는 실측 텍스트 fixture를 백엔드가 돌려주는 형태로 바꾼다. 프로덕션은 netlink에서
// 읽지만 테스트 입력은 실제 출력이라야 한다.
func mustParseRoutes(text string) []routeEntry {
	entries, _ := parseRouteTable(text)
	return entries
}

// rollback은 별도 프로세스다. systemd 타이머가 edc route rollback --state를 부르므로 그 프로세스는
// JSON 스냅샷만 보고 경로를 재구성한다. 커널이 경로를 찾는 키(dest, table, metric, tos)가 왕복에서
// 하나라도 사라지면 replace가 엉뚱한 경로를 건드리거나 새 경로를 만든다.
func TestRouteEntryKeyFieldsSurviveTheStateFile(t *testing.T) {
	original := routeEntry{
		Dest: "default", Type: "unicast", Via: "192.0.2.1", Dev: "enp1s0",
		Proto: "dhcp", Src: "192.0.2.13", Scope: "link",
		Metric: "100", HasMetric: true, Tos: 4, Table: "52",
	}
	snapshot := routeSnapshot{
		SchemaVersion: routeStateSchemaVersion, RunID: "t1", CreatedAt: time.Now().UTC(),
		Dest: "default", RouteTableText: "default via 192.0.2.1 dev enp1s0\n",
		TargetEntry: original, NewVia: "192.0.2.254", CountBaseline: 1,
		UnitName: "edc-route-rollback-t1",
	}
	path := filepath.Join(t.TempDir(), "route.json")
	if err := writeRouteState(path, snapshot); err != nil {
		t.Fatal(err)
	}
	loaded, err := readRouteState(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.TargetEntry != original {
		t.Fatalf("target entry changed across the state file:\n got %#v\nwant %#v", loaded.TargetEntry, original)
	}
}

// 옛 스키마 파일은 거부해야 한다. 그대로 받아들이면 새로 생긴 키 필드가 0으로 채워져 rollback이
// 다른 경로를 복원한다. 거부는 시끄럽게 실패하고 손으로 복구할 명령을 남기므로 조용히 틀리는 것보다 낫다.
func TestReadRouteStateRejectsOlderSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "route.json")
	old := `{
		"schema_version": 1,
		"run_id": "old1",
		"dest": "default",
		"route_table_text": "default via 192.0.2.1 dev enp1s0\n",
		"target_entry": {"Dest": "default", "Via": "192.0.2.1", "Dev": "enp1s0"},
		"count_baseline": 1
	}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readRouteState(path); err == nil {
		t.Fatal("a state file from an older schema must be rejected, not silently filled in")
	}
}
