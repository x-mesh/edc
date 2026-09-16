package edc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// routeStateSchemaVersion이 바뀌면 readRouteState는 옛 스키마의 상태 파일을 거부한다.
//
// routeEntry의 키 필드가 늘어나면 반드시 올려야 한다. 옛 파일은 그 필드가 비어 있는 채로 읽히는데,
// 예를 들어 Tos가 0으로 채워지면 rollback이 tos로 구분되는 다른 경로를 복원한다. 거부하면 rollback이
// 실패를 보고하고 RemainingCommands에 손으로 실행할 ip 명령을 남기므로, 조용히 틀리는 것보다 낫다.
//
// 2: Scope와 Tos 추가(netlink 전환).
const routeStateSchemaVersion = 2

// routeSnapshot은 switch가 시작할 때 찍는 스냅샷이자, rollback이 원복 근거로 쓰는 상태 파일의 내용이다.
// 사람과 systemd 타이머가 같은 파일 형식을 읽고 쓴다.
type routeSnapshot struct {
	SchemaVersion  int        `json:"schema_version"`
	RunID          string     `json:"run_id"`
	CreatedAt      time.Time  `json:"created_at"`
	Dest           string     `json:"dest"`
	RouteTableText string     `json:"route_table_text"`
	IPRuleText     string     `json:"ip_rule_text"`
	TargetEntry    routeEntry `json:"target_entry"`
	NewVia         string     `json:"new_via"`
	CountBaseline  int        `json:"count_baseline"`
	UnitName       string     `json:"unit_name"`
	SSHConnection  string     `json:"ssh_connection"`
	ExitName       string     `json:"exit_name"`
	// Seconds는 무장한 타이머의 유예(초)다. status가 남은 시간을 계산하는 데 쓴다.
	Seconds     int        `json:"seconds"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

// writeRouteState는 0700 디렉터리 안에 0600 파일로 임시 파일을 쓰고 rename해 원자적으로 저장한다.
func writeRouteState(path string, snapshot routeSnapshot) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(directory, ".route-state-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath) // rename이 성공하면 이미 없으므로 실패한 경우에만 지운다.
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

// readRouteState는 상태 파일을 읽는다. 스키마 버전이 다르거나 rollback이 원복 근거로 쓸 필수 항목이
// 비어 있으면 오류를 돌려준다.
func readRouteState(path string) (routeSnapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return routeSnapshot{}, err
	}
	var snapshot routeSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return routeSnapshot{}, err
	}
	if snapshot.SchemaVersion != routeStateSchemaVersion {
		return routeSnapshot{}, fmt.Errorf("route state %s: unsupported schema version %d", path, snapshot.SchemaVersion)
	}
	if snapshot.RunID == "" || snapshot.Dest == "" || snapshot.TargetEntry.Dev == "" || snapshot.RouteTableText == "" {
		return routeSnapshot{}, fmt.Errorf("route state %s: missing required fields", path)
	}
	return snapshot, nil
}

// routeEntrySpecEqual은 두 entry가 같은 경로를 가리키는지 본다. via·dev·metric·table이 모두 같아야 한다.
func routeEntrySpecEqual(a, b routeEntry) bool {
	return a.Via == b.Via && a.Dev == b.Dev && a.HasMetric == b.HasMetric && a.Metric == b.Metric && a.Table == b.Table
}

// routeOp는 원복이 실행할 조작 하나다. 실행은 netlink으로 하고, 표시와 사람이 손으로 복구할 근거는
// 같은 값을 ip 명령 형태로 렌더링해 남긴다.
type routeOp struct {
	Kind   string // "replace" 또는 "delete"
	Entry  routeEntry
	NewVia string
}

// rollbackOps는 스냅샷의 원래 스펙을 되돌리는 replace 하나와, 현재 테이블에서 기준선을 넘는 잔존
// 경로를 지우는 delete들을 순서대로 돌려준다.
func rollbackOps(snapshot routeSnapshot, entries []routeEntry) ([]routeOp, error) {
	if snapshot.TargetEntry.Dest == "" {
		return nil, fmt.Errorf("snapshot carries no target route")
	}
	ops := []routeOp{{Kind: "replace", Entry: snapshot.TargetEntry, NewVia: snapshot.TargetEntry.Via}}
	matched := entriesForDest(entries, snapshot.Dest)
	if len(matched) <= snapshot.CountBaseline {
		return ops, nil
	}
	for _, entry := range matched {
		if routeEntrySpecEqual(entry, snapshot.TargetEntry) {
			continue
		}
		ops = append(ops, routeOp{Kind: "delete", Entry: entry})
	}
	return ops, nil
}

func applyRouteOp(ctx context.Context, backend routeBackend, op routeOp) error {
	if op.Kind == "delete" {
		return backend.DeleteRoute(ctx, op.Entry)
	}
	return backend.ReplaceRoute(ctx, op.Entry, op.NewVia)
}

// routeOpArgs는 조작 하나를 ip에 넘길 argv로 만든다. 실행은 netlink으로 하지만, 사람이 손으로
// 복구할 때 그대로 쓸 수 있는 형태를 남기는 것이 안전 도구로서 중요하다.
func routeOpArgs(op routeOp) []string {
	var args []string
	var err error
	if op.Kind == "delete" {
		args, err = routeDeleteArgs(op.Entry)
	} else {
		args, err = routeReplaceArgs(op.Entry, op.NewVia)
	}
	if err != nil {
		return []string{"route", op.Kind, op.Entry.Dest}
	}
	return args
}

// renderRouteOp은 오류 메시지에 쓸 한 줄이다.
func renderRouteOp(op routeOp) string {
	return "ip " + strings.Join(routeOpArgs(op), " ")
}

func renderRouteOps(ops []routeOp) [][]string {
	out := make([][]string, 0, len(ops))
	for _, op := range ops {
		out = append(out, routeOpArgs(op))
	}
	return out
}
