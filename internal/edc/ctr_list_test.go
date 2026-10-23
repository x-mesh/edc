package edc

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// ctrListLine은 이름과 ID를 바꾼 docker inspect 한 줄이다. 한 번도 돈 적 없는 container는 FinishedAt이 0이다.
func ctrListLine(name, id, status string, restarts int, oom bool, exitCode int, health string, ran bool) string {
	fields := strings.Split(ctrInspectLine(status, restarts, oom, exitCode, health), "|")
	fields[1], fields[9] = "/"+name, id
	if !ran {
		fields[4] = "0001-01-01T00:00:00Z"
	}
	return strings.Join(fields, "|")
}

var ctrListLines = []string{
	ctrListLine("web", "aaaaaaaaaaaaaaaa", "running", 0, false, 0, "", true),
	ctrListLine("Batch", "bbbbbbbbbbbbbbbb", "exited", 0, false, 0, "", true),
	ctrListLine("worker", "cccccccccccccccc", "exited", 2, false, 137, "", true),
	ctrListLine("cache", "dddddddddddddddd", "running", 0, false, 0, "unhealthy", true),
	ctrListLine("api", "eeeeeeeeeeeeeeee", "running", 3, false, 0, "healthy", true),
	ctrListLine("new", "ffffffffffffffff", "created", 0, false, 0, "", false),
	ctrListLine("oom", "1111111111111111", "exited", 0, true, 0, "", true),
}

func ctrListFakeSources(asked *[]string) ctrSources {
	sources := ctrTestSources("", nil)
	sources.listIDs = func(context.Context) ([]string, error) {
		return []string{"aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", "cccccccccccccccc", "dddddddddddddddd", "eeeeeeeeeeeeeeee", "ffffffffffffffff", "1111111111111111"}, nil
	}
	sources.inspectMany = func(_ context.Context, ids []string) ([]ctrInspect, error) {
		*asked = append(*asked, ids...)
		items := make([]ctrInspect, 0, len(ctrListLines))
		for _, line := range ctrListLines {
			item, err := parseCtrInspect(line)
			if err != nil {
				return nil, err
			}
			items = append(items, item)
		}
		return items, nil
	}
	sources.stats = func(context.Context, string, ctrInspect, bool) ctrLimits {
		used := uint64(512 << 20)
		return ctrLimits{Memory: topCgroupMemory{topLimitStatus: topLimitStatus{Status: "available"}, Used: &used}}
	}
	sources.listStats = func(_ context.Context, inspects []ctrInspect) map[string]ctrListStats {
		result := make(map[string]ctrListStats, len(inspects))
		for _, inspect := range inspects {
			used := uint64(512 << 20)
			limit := uint64(1 << 30)
			pids := uint64(3)
			cpu := 0.0
			zero := uint64(0)
			entry := ctrListStats{Limits: ctrLimits{Memory: topCgroupMemory{topLimitStatus: topLimitStatus{Status: "available"}, Used: &used, Max: &limit}, Pids: ctrPidsLimits{topLimitStatus: topLimitStatus{Status: "available"}, Current: pids}}, CPUReason: "", NetworkRX: &zero, NetworkTX: &zero, BlockRead: &zero, BlockWrite: &zero}
			entry.Limits.CPUUsagePercent = &cpu
			result[inspect.ID] = entry
		}
		return result
	}
	return sources
}

func ctrListNames(items []ctrListItem) string {
	var names []string
	for _, item := range items {
		names = append(names, item.Name)
	}
	return strings.Join(names, ",")
}

func TestCollectCtrListPutsProblemsFirst(t *testing.T) {
	var asked []string
	items, err := collectCtrList(context.Background(), ctrListFakeSources(&asked), false)
	if err != nil {
		t.Fatal(err)
	}
	// 실패(0이 아닌 종료, OOM), 주의(unhealthy, created), 재시작 기록, 나머지 순이다. 코드 0으로 끝난 Batch는 빠진다.
	if got := ctrListNames(items); got != "oom,worker,cache,new,api,web" {
		t.Fatalf("order = %s", got)
	}
	if len(asked) != 7 {
		t.Fatalf("one inspect must ask for every container: %v", asked)
	}
	web := items[5]
	if web.ID != "aaaaaaaaaaaa" || web.UptimeS == nil || *web.UptimeS != 5400 || web.MemoryBytes == nil || *web.MemoryBytes != 512<<20 {
		t.Fatalf("web = %+v", web)
	}
	if worker := items[1]; worker.State != "exited, exit code 137" || worker.UptimeS != nil {
		t.Fatalf("worker = %+v", worker)
	}
	if created := items[3]; created.State != "created" {
		t.Fatalf("a container that never ran has no exit code: %+v", created)
	}
	items, _ = collectCtrList(context.Background(), ctrListFakeSources(&asked), true)
	if got := ctrListNames(items); got != "oom,worker,cache,new,api,web,Batch" {
		t.Fatalf("all = %s", got)
	}
	noDocker := ctrListFakeSources(&asked)
	noDocker.docker = func() bool { return false }
	if _, err := collectCtrList(context.Background(), noDocker, false); !errors.Is(err, errCtrNoDocker) {
		t.Fatalf("no docker = %v", err)
	}
}

func TestCollectCtrListUsesOneShotStats(t *testing.T) {
	var asked []string
	sources := ctrListFakeSources(&asked)
	calls := 0
	sources.listStats = func(_ context.Context, inspects []ctrInspect) map[string]ctrListStats {
		calls++
		if len(inspects) != 3 {
			t.Fatalf("running stats targets=%d", len(inspects))
		}
		return map[string]ctrListStats{}
	}
	if _, err := collectCtrList(context.Background(), sources, false); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("list stats batch calls=%d, want 1", calls)
	}
}

func TestCollectCtrListKeepsTheLinesThatInspectReturned(t *testing.T) {
	var asked []string
	sources := ctrListFakeSources(&asked)
	// 목록을 읽은 뒤 사라진 container가 있으면 docker inspect는 실패하지만 나머지 줄은 낸다.
	sources.inspectMany = func(context.Context, []string) ([]ctrInspect, error) {
		item, _ := parseCtrInspect(ctrListLines[0])
		return []ctrInspect{item}, nil
	}
	if items, err := collectCtrList(context.Background(), sources, false); err != nil || ctrListNames(items) != "web" {
		t.Fatalf("items %v, err %v", ctrListNames(items), err)
	}
	sources.inspectMany = func(context.Context, []string) ([]ctrInspect, error) {
		return nil, errors.New("permission denied while trying to connect to the Docker daemon socket")
	}
	sources.listIDs = func(context.Context) ([]string, error) {
		return nil, errors.New("permission denied while trying to connect to the Docker daemon socket")
	}
	_, err := collectCtrList(context.Background(), sources, false)
	if result := evaluateCtrList(nil, err); result.Status != StatusSkip || result.Summary != T("ctr.skip.needs_docker_access") {
		t.Fatalf("docker access = %+v", result)
	}
}

func TestEvaluateCtrListKeepsPartialItemsOnPermissionError(t *testing.T) {
	var asked []string
	items, err := collectCtrList(context.Background(), ctrListFakeSources(&asked), false)
	if err != nil {
		t.Fatal(err)
	}
	partial := items[:1]
	result := evaluateCtrList(partial, errors.New("permission denied while reading Docker Engine socket"))
	containers, ok := result.Metrics["containers"].([]ctrListItem)
	if result.Status != StatusSkip || !ok || len(containers) != 1 || containers[0].Name != partial[0].Name {
		t.Fatalf("partial permission result=%+v", result)
	}
}

func TestEvaluateCtrListNamesTheFailedContainers(t *testing.T) {
	var asked []string
	items, _ := collectCtrList(context.Background(), ctrListFakeSources(&asked), false)
	result := evaluateCtrList(items, nil)
	if result.Probe != "ctr.list" || result.Status != StatusFail || result.Error == nil || result.Error.Message != "oom (exited, exit code 0, OOM killed), worker (exited, exit code 137)" {
		t.Fatalf("result = %+v", result)
	}
	if result.Metrics["failed"] != 2 || result.Metrics["attention"] != 2 {
		t.Fatalf("metrics = %+v", result.Metrics)
	}
	if status := evaluateCtrList(items[2:], nil).Status; status != StatusWarn {
		t.Fatalf("unhealthy and created = %s", status)
	}
	if status := evaluateCtrList(items[4:], nil).Status; status != StatusPass {
		t.Fatalf("restarted before = %s", status)
	}
	if result := evaluateCtrList(nil, nil); result.Status != StatusPass || len(result.Metrics["containers"].([]ctrListItem)) != 0 {
		t.Fatalf("no containers = %+v", result)
	}
	if result := evaluateCtrList(nil, errCtrNoDocker); result.Status != StatusSkip {
		t.Fatalf("no docker = %+v", result)
	}
}

func TestRunCtrWithoutAContainerListsThem(t *testing.T) {
	var asked []string
	output := filepath.Join(t.TempDir(), "ctr.json")
	if code := runCtrWith([]string{"--json", output}, "test", ctrListFakeSources(&asked), true); code != 1 {
		t.Fatalf("a failed container exits with 1, got %d", code)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Results []struct {
			Probe   string `json:"probe"`
			Metrics struct {
				Containers []ctrListItem `json:"containers"`
			} `json:"metrics"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &report); err != nil || len(report.Results) != 1 || report.Results[0].Probe != "ctr.list" || len(report.Results[0].Metrics.Containers) != 6 {
		t.Fatalf("report = %s", data)
	}
	if code := runCtrWith([]string{"--all", "web"}, "test", ctrListFakeSources(&asked), true); code != 2 {
		t.Fatalf("--all with a container exits with 2, got %d", code)
	}
	table := formatCtrListTable([]ctrListItem{{Name: "web", State: "running", Restarts: 1, Image: "nginx:1.27"}}, textStyle{})
	if lines := strings.Split(strings.TrimSpace(table), "\n"); len(lines) != 2 || !strings.HasPrefix(lines[0], "NAME") || !strings.HasSuffix(lines[1], "nginx:1.27") {
		t.Fatalf("table:\n%s", table)
	}
}

// 터미널에 쓰는 표는 머리글과 image가 회색이고 상태 칸은 대화형 화면과 같은 색이다.
func TestFormatCtrListTableColorsTheState(t *testing.T) {
	items := []ctrListItem{{Name: "web", State: "running", Status: "running", Image: "nginx:1.27"}, {Name: "job", State: "exited (1)", Status: "exited", ExitCode: 1, Image: "busybox"}}
	plain, colored := formatCtrListTable(items, textStyle{}), formatCtrListTable(items, textStyle{color: true})
	if ansi.Strip(colored) != plain {
		t.Fatalf("plain:\n%s\ncolored:\n%q", plain, colored)
	}
	for _, want := range []string{richExpect("\033[90mNAME\033[0m"), richExpect("\033[32mrunning\033[0m"), richExpect("\033[90mnginx:1.27\033[0m"), richPaint("exited (1)", items[1].stateColor(), false)} {
		if !strings.Contains(colored, want) {
			t.Fatalf("table lacks %q: %q", want, colored)
		}
	}
}
