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
	commands, err := rollbackCommands(snapshot, mustParseRoutes(routeMetricTrapFixture))
	if err != nil {
		t.Fatalf("rollbackCommands error: %v", err)
	}
	if len(commands) != 2 {
		t.Fatalf("commands = %#v, want 2 (restore + delete residual)", commands)
	}
	restore := strings.Join(commands[0], " ")
	if restore != "route replace 8.8.8.8 via 10.20.1.1 dev enp1s0 metric 100" {
		t.Fatalf("restore command = %q", restore)
	}
	deleteResidual := strings.Join(commands[1], " ")
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
	commands, err := rollbackCommands(snapshot, mustParseRoutes(routeTableFixture))
	if err != nil {
		t.Fatalf("rollbackCommands error: %v", err)
	}
	if len(commands) != 1 {
		t.Fatalf("commands = %#v, want only the restore command", commands)
	}
}

// mustParseRoutes는 실측 텍스트 fixture를 백엔드가 돌려주는 형태로 바꾼다. 프로덕션은 netlink에서
// 읽지만 테스트 입력은 실제 출력이라야 한다.
func mustParseRoutes(text string) []routeEntry {
	entries, _ := parseRouteTable(text)
	return entries
}
