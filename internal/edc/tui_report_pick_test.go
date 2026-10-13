package edc

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

var reportPickNow = time.Date(2026, 10, 13, 8, 20, 0, 0, time.UTC)

// reportPickFixture는 discoverReports처럼 최근 것부터 놓인 세 report다.
func reportPickFixture() []savedReport {
	results := []Result{{Probe: "a", Status: StatusPass}, {Probe: "b", Status: StatusPass}, {Probe: "c", Status: StatusWarn}}
	var reports []savedReport
	for index, minute := range []int{14, 11, 9} {
		report := Report{Run: RunInfo{StartedAt: time.Date(2026, 10, 13, 8, minute, 0, 0, time.UTC)}, Results: results}
		reports = append(reports, savedReport{path: fmt.Sprintf("edc-snapshot-jw-server-20261013T08%02d00Z.json", minute), report: report})
		if index == 2 {
			reports[index].report.Results = append(results, Result{Probe: "d", Status: StatusFail})
		}
	}
	return reports
}

func reportPickKeys(t *testing.T, model reportPairModel, keys ...string) (reportPairModel, tea.Cmd) {
	t.Helper()
	var cmd tea.Cmd
	for _, key := range keys {
		var next tea.Model
		next, cmd = model.Update(topKey(key))
		model = next.(reportPairModel)
	}
	return model, cmd
}

// 가장 흔한 경우는 직전과 지금의 비교다. Enter 한 번이면 최근 두 개를 시간 순서로 비교한다.
func TestReportPairComparesTheTwoNewestByDefault(t *testing.T) {
	reports := reportPickFixture()
	model, cmd := reportPickKeys(t, newReportPairModel(reports, reportPickNow), "enter")
	if !model.done || model.enterPath || cmd == nil {
		t.Fatalf("enter = %+v", model)
	}
	picked := model.pickedReports()
	before, after := orderReportPair(picked[0], picked[1])
	if before.path != reports[1].path || after.path != reports[0].path {
		t.Fatalf("before %s, after %s", before.path, after.path)
	}
	// report가 하나뿐이면 그것만 미리 고른다.
	if single := newReportPairModel(reports[:1], reportPickNow); len(single.picked) != 1 {
		t.Fatalf("single = %v", single.picked)
	}
}

func TestReportPairKeepsTwoPicks(t *testing.T) {
	model, _ := reportPickKeys(t, newReportPairModel(reportPickFixture(), reportPickNow), "down", "down", " ")
	// 세 번째를 고르면 가장 먼저 고른 것(가장 최근 report)이 풀린다.
	if got := model.picked; len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("picked = %v", got)
	}
	model, _ = reportPickKeys(t, model, " ")
	if len(model.picked) != 1 || model.picked[0] != 1 {
		t.Fatalf("after clearing = %v", model.picked)
	}
	// 하나만 고른 채 Enter는 비교하지 않고 무엇이 모자란지 알린다.
	model, cmd := reportPickKeys(t, model, "enter")
	if model.done || cmd != nil || model.notice != "select two results with space (1 selected)" {
		t.Fatalf("enter with one = %+v", model)
	}
	// 경로 줄에서 Enter를 누르면 모자란 하나를 경로로 받는다.
	model, cmd = reportPickKeys(t, model, "down", "enter")
	if !model.done || !model.enterPath || cmd == nil || model.notice != "" {
		t.Fatalf("enter path = %+v", model)
	}
	// 이미 둘을 고른 채 경로 줄을 고르면 어느 쪽을 바꿀지 모르므로 하나를 풀라고 한다.
	full, _ := reportPickKeys(t, newReportPairModel(reportPickFixture(), reportPickNow), "up", "enter")
	if full.done || full.cursor != 3 || full.notice != "two results are selected: clear one with space to enter a path" {
		t.Fatalf("path with two = %+v", full)
	}
	if cancelled, cmd := reportPickKeys(t, newReportPairModel(reportPickFixture(), reportPickNow), "esc"); !cancelled.cancelled || cmd == nil {
		t.Fatalf("esc = %+v", cancelled)
	}
}

// 폭이 모자라면 경과, 파일 이름 뒤쪽, 결과 수 순서로 줄인다. 시각은 어느 폭에서도 남는다.
func TestReportPairRowsFitTheWidth(t *testing.T) {
	model := newReportPairModel(reportPickFixture(), reportPickNow)
	started := time.Date(2026, 10, 13, 8, 14, 0, 0, time.UTC).Local().Format(time.DateTime)
	cases := []struct {
		width                 int
		age, counts, ellipsis bool
	}{
		{0, true, true, false},
		{120, true, true, false},
		{80, false, true, true},
		{50, false, false, true},
	}
	for _, test := range cases {
		model.width = test.width
		rows := model.rows()
		first := rows[0]
		if !strings.HasPrefix(first, "[x] "+started+"  ") || strings.Contains(first, "6m ago") != test.age || strings.Contains(first, "2 pass · 1 warn") != test.counts || strings.Contains(first, "…") != test.ellipsis {
			t.Errorf("width %d: %q", test.width, first)
		}
		if test.width > 0 {
			for _, row := range rows[:3] {
				if width := ansi.StringWidth(row); width > test.width-2 {
					t.Errorf("width %d: row is %d wide: %q", test.width, width, row)
				}
			}
		}
		if !strings.HasSuffix(rows[3], "Enter another edc diagnostic result file path…") || !strings.HasPrefix(rows[2], "[ ] ") {
			t.Errorf("width %d: rows = %q", test.width, rows)
		}
	}
	// 0인 결과는 빼고 센다.
	if got := reportPairCounts(reportPickFixture()[2].report.Results); got != "2 pass · 1 warn · 1 fail" {
		t.Fatalf("counts = %q", got)
	}
	if got := reportPairCounts(nil); got != "-" {
		t.Fatalf("empty counts = %q", got)
	}
}

// 목록이 화면보다 길면 커서가 보이는 범위만 그리고, 제목과 안내 줄은 폭에 맞춰 자른다.
func TestReportPairViewScrollsAndFits(t *testing.T) {
	model := newReportPairModel(reportPickFixture(), reportPickNow)
	model.color = false
	next, _ := model.Update(tea.WindowSizeMsg{Width: 30, Height: 5})
	model = next.(reportPairModel)
	model, _ = reportPickKeys(t, model, "down", "down", "down")
	lines := strings.Split(model.View().Content, "\n")
	if len(lines) != 5 || !strings.HasPrefix(lines[0], "compare two edc") || !strings.HasSuffix(lines[0], "…") || !strings.HasPrefix(lines[3], liveSelectedBar+" ") || !strings.Contains(lines[3], "Enter another") {
		t.Fatalf("view = %q", lines)
	}
	for _, line := range lines {
		if width := ansi.StringWidth(line); width > 30 {
			t.Errorf("line is %d wide: %q", width, line)
		}
	}
	if start, end := model.window(4); start != 2 || end != 4 {
		t.Fatalf("window = %d-%d", start, end)
	}
	if !model.View().AltScreen {
		t.Fatal("the picker must not leave its rows in the scrollback")
	}
}

func TestOrderReportPairPutsTheEarlierFirst(t *testing.T) {
	reports := reportPickFixture()
	if before, after := orderReportPair(reports[0], reports[1]); before.path != reports[1].path || after.path != reports[0].path {
		t.Fatalf("newer first: %s, %s", before.path, after.path)
	}
	// 시각이 같거나 입력한 파일을 읽지 못해 시각을 모르면 받은 순서를 지킨다.
	same := reports[1]
	same.path = "same.json"
	if before, _ := orderReportPair(reports[1], same); before.path != reports[1].path {
		t.Fatalf("same time: %s", before.path)
	}
	if before, after := orderReportPair(reports[0], savedReport{path: "typed.json"}); before.path != reports[0].path || after.path != "typed.json" {
		t.Fatalf("unknown time: %s, %s", before.path, after.path)
	}
	var output strings.Builder
	printReportPair(&output, reports[1], savedReport{path: "typed.json"})
	want := "before  " + reports[1].report.Run.StartedAt.Local().Format(time.DateTime) + "  " + reports[1].path + "\nafter   -  typed.json\n"
	if output.String() != want {
		t.Fatalf("pair = %q, want %q", output.String(), want)
	}
}
