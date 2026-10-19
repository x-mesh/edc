package edc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestTopPanelsParse(t *testing.T) {
	valid := []struct {
		value string
		want  []topPanel
	}{
		{"none", []topPanel{}},
		{"net", []topPanel{topPanelNet}},
		{"ai, net ,events", []topPanel{topPanelAI, topPanelNet, topPanelEvents}},
		{"disk,cpu,psi,mem", []topPanel{topPanelDisk, topPanelCPU, topPanelPSI, topPanelMemory}},
	}
	for _, test := range valid {
		got, err := parseTopPanels(test.value)
		if err != nil || !reflect.DeepEqual(got, test.want) {
			t.Errorf("parseTopPanels(%q) = %v, %v; want %v", test.value, got, err, test.want)
		}
	}
	for _, value := range []string{"", "net,", "network", "ai,ai", "none,ai", "memory"} {
		if got, err := parseTopPanels(value); err == nil {
			t.Errorf("parseTopPanels(%q) = %v, want error", value, got)
		}
	}
	for _, value := range []string{"all", "cpu", "memory", "disk", "network", "pressure", "split"} {
		if got, err := parseTopView(value); err != nil || string(got) != value {
			t.Errorf("parseTopView(%q) = %q, %v", value, got, err)
		}
	}
	for _, value := range []string{"", "mem", "net", "process", "Network"} {
		if _, err := parseTopView(value); err == nil {
			t.Errorf("parseTopView(%q) must fail", value)
		}
	}
	for value, want := range map[string]topView{"cpu": topViewCPU, "mem": topViewMemory, "io": topViewDisk} {
		if got, err := parseTopProcessSort(value); err != nil || got != want {
			t.Errorf("parseTopProcessSort(%q) = %q, %v; want %q", value, got, err, want)
		}
	}
	if _, err := parseTopProcessSort("disk"); err == nil {
		t.Error("parseTopProcessSort(disk) must fail")
	}
}

type fakeTopAISource struct {
	result aiPollResult
	usage  aiUsageSnapshot
	polls  int
	scans  int
	closed bool
}

func (source *fakeTopAISource) poll(context.Context, time.Duration) aiPollResult {
	source.polls++
	return source.result
}

func (source *fakeTopAISource) scan() aiUsageSnapshot {
	source.scans++
	return source.usage
}

func (source *fakeTopAISource) close() { source.closed = true }

func topAIFixtureResult() aiPollResult {
	reset := time.Unix(3600, 0)
	return aiPollResult{providers: []aiProvider{
		{Name: "claude", Windows: []aiWindow{{Name: "5h", Used: 78, ResetsAt: reset}, {Name: "7d", Used: 31, ResetsAt: reset}}},
		{Name: "codex", Err: "app server not found"},
	}}
}

// topPanelModelAt은 network 상세가 있는 행과 AI 조회 결과를 가진 모델이다. 이벤트는 없다.
func topPanelModelAt(width, height int, panels []topPanel) topModel {
	model := topSplitModelAt(width, height, 30, false, []topView{topViewCPU})
	model.view = topViewAll
	health := &networkHealthRate{networkHealth: networkHealth{Supported: true}}
	for index := range model.rows {
		model.rows[index].rate.NetworkHealth = health
	}
	model = model.withLayout(topLayout{panels: panels, panelsSet: true})
	model.aiProviders, model.aiPolled = topAIFixtureResult().providers, true
	return model
}

func topViewText(model topModel) string { return ansi.Strip(model.View().Content) }

func TestTopPanelsKeepTheirOrder(t *testing.T) {
	model := topPanelModelAt(topTableWidth, 60, []topPanel{topPanelAI, topPanelNet, topPanelEvents})
	text := topViewText(model)
	ai, net, events := strings.Index(text, "ai limits"), strings.Index(text, "network 09:00:29"), strings.Index(text, "events · e list")
	if ai < 0 || net < ai || events < net {
		t.Fatalf("panels out of order: ai %d, net %d, events %d\n%s", ai, net, events, text)
	}
	for _, expected := range []string{"claude 5h", "78%", "codex  ! app server not found"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("ai panel lacks %q:\n%s", expected, text)
		}
	}
	if strings.Contains(text, "hidden") {
		t.Fatalf("a 60-row terminal must show every panel:\n%s", text)
	}
}

func TestTopPanelsHideFromTheBackWhenShort(t *testing.T) {
	for _, width := range []int{topTableWidth, topFooterDockWidth + 10} {
		model := topPanelModelAt(width, 24, []topPanel{topPanelAI, topPanelNet, topPanelEvents})
		lines := strings.Split(topViewText(model), "\n")
		text := strings.Join(lines, "\n")
		if len(lines) > 24 {
			t.Fatalf("width %d: %d lines for a 24-row terminal:\n%s", width, len(lines), text)
		}
		if model.bodyLines() < topPanelHistoryRows {
			t.Fatalf("width %d: table keeps %d rows, want at least %d", width, model.bodyLines(), topPanelHistoryRows)
		}
		// 맨 앞의 ai는 남고 뒤의 패널이 숨는다. 좁은 화면은 process 후보 자리를 남기므로 10줄인 net 상세가 들어가지 않고
		// 뒤의 작은 events가 남은 자리에 들어간다. 넓은 화면은 후보가 오른쪽에 있어 net까지 들어가고 events가 숨는다.
		want, hidden := "events · e list", "+net hidden"
		if width >= topFooterDockWidth {
			want, hidden = "network 09:00:29", "+events hidden"
		}
		if !strings.Contains(text, "ai limits") || !strings.Contains(text, want) || !strings.Contains(text, hidden) {
			t.Fatalf("width %d: want ai and %q shown, %q:\n%s", width, want, hidden, text)
		}
	}
}

func TestTopPanelsKeepEventsBesideOtherPanelsOnWideScreens(t *testing.T) {
	model := topPanelModelAt(topFooterDockWidth+10, 50, []topPanel{topPanelNet, topPanelEvents})
	text := topViewText(model)
	if !strings.Contains(text, "network 09:00:29") || !strings.Contains(text, "events · e list") {
		t.Fatalf("wide footer must show net and events together:\n%s", text)
	}
	if model.eventTicker() != "" {
		t.Fatal("events in the panel list must not also show the one-line ticker")
	}
}

func TestTopPanelsShowInBoxes(t *testing.T) {
	model := topPanelModelAt(topTableWidth, 40, []topPanel{topPanelNet})
	model = model.withSplit([]topView{topViewCPU})
	text := topViewText(model)
	if !strings.Contains(text, "network 09:00:29") || !strings.Contains(text, "╭─ cpu") {
		t.Fatalf("boxes must keep the net panel:\n%s", text)
	}
}

func TestTopDetailTakesThePanelPlace(t *testing.T) {
	model := topPanelModelAt(topTableWidth, 60, []topPanel{topPanelAI})
	model.detail = true
	text := topViewText(model)
	if strings.Contains(text, "ai limits") {
		t.Fatalf("Enter detail must take the place of the panels:\n%s", text)
	}
	model = topAfter(t, model, tea.KeyPressMsg{Code: tea.KeyEscape})
	if !strings.Contains(topViewText(model), "ai limits") {
		t.Fatal("closing the detail must bring the panels back")
	}
}

func TestTopAIKeyOpensTheCollectorOnce(t *testing.T) {
	source := &fakeTopAISource{result: topAIFixtureResult()}
	opened := 0
	model := topFixtureModel(nil)
	model.openAI = func() (topAISource, error) {
		opened++
		return source, nil
	}
	if model.customFooter() {
		t.Fatal("a model without panels or AI must keep the old footer")
	}
	updated, cmd := model.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	model = updated.(topModel)
	if !model.aiOn || cmd == nil || opened != 1 {
		t.Fatalf("a: on %v, cmd %v, opened %d", model.aiOn, cmd != nil, opened)
	}
	model = topAfter(t, model, cmd())
	if source.polls != 1 || !model.aiPolled || !strings.Contains(topViewText(model), "claude 5h") {
		t.Fatalf("polls %d, polled %v:\n%s", source.polls, model.aiPolled, topViewText(model))
	}
	// 꺼진 동안 도착한 결과는 다음 조회를 예약하지 않는다. 다시 켜도 수집기를 새로 열지 않는다.
	model = topAfter(t, model, tea.KeyPressMsg{Code: 'a', Text: "a"})
	updated, next := model.Update(topAIPollMsg(source.result))
	if model = updated.(topModel); next != nil || model.aiOn {
		t.Fatal("a poll result after a must not schedule another poll")
	}
	updated, cmd = model.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	if model = updated.(topModel); !model.aiOn || cmd == nil || opened != 1 {
		t.Fatalf("second a: on %v, cmd %v, opened %d", model.aiOn, cmd != nil, opened)
	}
	model.closeAI()
	if !source.closed {
		t.Fatal("quit must close the AI collector")
	}
}

func TestTopAIStaysClosedWithoutTheAIPanel(t *testing.T) {
	opened := 0
	model := topFixtureModel(nil)
	model.openAI = func() (topAISource, error) {
		opened++
		return &fakeTopAISource{}, nil
	}
	model = model.withLayout(topLayout{panels: []topPanel{topPanelNet, topPanelEvents}, panelsSet: true})
	model = topAfter(t, model, topSampleMsg{snapshot: topSampleAt(1)}, topSampleMsg{snapshot: topSampleAt(2)}, topAIStartMsg{})
	_ = model.View()
	if opened != 0 || model.aiOn {
		t.Fatalf("opened %d, ai %v without ai in the panel list", opened, model.aiOn)
	}
	if !topAfter(t, topFixtureModel(nil).withLayout(topLayout{panels: []topPanel{topPanelAI}, panelsSet: true})).aiOn {
		t.Fatal("ai in the panel list must start with the AI panel on")
	}
}

func TestTopAIOpenFailureLeavesTheDashboardRunning(t *testing.T) {
	model := topFixtureModel(nil)
	model.openAI = func() (topAISource, error) { return nil, errors.New("no home directory") }
	updated, cmd := model.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	model = updated.(topModel)
	if model.aiOn || cmd != nil || !strings.Contains(model.notice, "no home directory") {
		t.Fatalf("on %v, cmd %v, notice %q", model.aiOn, cmd != nil, model.notice)
	}
}

func TestTopLayoutStartView(t *testing.T) {
	model := topFixtureModel(nil).withSplit([]topView{topViewCPU, topViewNetwork}).withLayout(topLayout{view: topViewNetwork})
	if model.view != topViewNetwork || model.homeView() != topViewNetwork {
		t.Fatalf("view %q, home %q", model.view, model.homeView())
	}
	model = topAfter(t, model, tea.KeyPressMsg{Code: 'v', Text: "v"})
	if model.view != topViewSplit || !reflect.DeepEqual(model.splitViews(), []topView{topViewCPU, topViewNetwork}) {
		t.Fatalf("v: view %q, boxes %v", model.view, model.splitViews())
	}
	model = topAfter(t, model, tea.KeyPressMsg{Code: tea.KeyEscape})
	if model.view != topViewNetwork {
		t.Fatalf("Esc returns to %q, want the start view", model.view)
	}
	if split := topFixtureModel(nil).withLayout(topLayout{view: topViewSplit}); split.view != topViewSplit || split.homeView() != topViewSplit {
		t.Fatalf("view split: %q, home %q", split.view, split.homeView())
	}
	filter, err := parseTopProcessFilter("node")
	if err != nil {
		t.Fatal(err)
	}
	filtered := topFixtureModel(nil).withProcessFilter(filter).withLayout(topLayout{view: topViewDisk})
	if filtered.view != topViewProcess {
		t.Fatalf("a process filter must keep the process view, got %q", filtered.view)
	}
}

func TestTopProcessSortSetsTheDefaultRank(t *testing.T) {
	model := topFixtureModel(nil).withLayout(topLayout{processSort: topViewDisk})
	if model.panelRank() != topViewDisk {
		t.Fatalf("rank %q, want I/O", model.panelRank())
	}
	model = topAfter(t, model, tea.KeyPressMsg{Code: 'o', Text: "o"})
	if model.panelRank() != topViewCPU {
		t.Fatalf("o must still choose the rank, got %q", model.panelRank())
	}
	model = topAfter(t, model, tea.KeyPressMsg{Code: tea.KeyEscape})
	if model.panelRank() != topViewDisk {
		t.Fatalf("Esc returns to %q, want the configured rank", model.panelRank())
	}
}

func TestTopHiddenNoticeNeverAddsAStatusLine(t *testing.T) {
	for _, width := range []int{40, 70, topTableWidth, 120} {
		model := topFixtureModel(nil)
		model.width = width
		count := len(model.statusLines())
		got := model.statusWithHidden([]topPanel{topPanelNet, topPanelAI, topPanelEvents}, count)
		if len(got) != count {
			t.Fatalf("width %d: %d status lines, want %d", width, len(got), count)
		}
	}
	model := topFixtureModel(nil)
	model.width = 120
	if got := strings.Join(model.statusWithHidden([]topPanel{topPanelAI}, len(model.statusLines())), "\n"); !strings.Contains(ansi.Strip(got), "+ai hidden") {
		t.Fatalf("status lacks the hidden notice: %q", got)
	}
	if !slices.Equal(topPanelNames, []topPanel{topPanelCPU, topPanelMemory, topPanelDisk, topPanelNet, topPanelPSI, topPanelAI, topPanelEvents}) {
		t.Fatal("panel names changed; update the config and help text")
	}
}

func TestTopLayoutRejectsNonDashboardUse(t *testing.T) {
	restore := activeConfig
	defer func() { activeConfig = restore }()
	activeConfig = edcConfig{}
	for _, args := range [][]string{
		{"--view", "network", "--count", "1"},
		{"--panels", "net,ai", "--count", "1"},
		{"--view", "mem"},
		{"--panels", "net,net"},
		{"--columns", "net+,cpu", "--count", "1"},
		{"--columns", "network"},
		{"--view", "disk", "--process", strconv.Itoa(os.Getpid())},
	} {
		if code := runTop(args, "test"); code != 2 {
			t.Errorf("runTop(%v) = %d, want 2", args, code)
		}
	}
	activeConfig.Defaults.Top.View, activeConfig.Defaults.Top.Panels = stringPointer("network"), stringPointer("ai")
	activeConfig.Defaults.Top.ProcessSort = stringPointer("io")
	if code := runTop([]string{"--interval", "200ms", "--count", "1", "--json", filepath.Join(t.TempDir(), "out.jsonl")}, "test"); code != 0 {
		t.Fatalf("a configured layout blocked a JSON run: %d", code)
	}
}

func TestTopSampleErrorDoesNotRepeatTheNetPanel(t *testing.T) {
	for _, panels := range [][]topPanel{{topPanelNet}, {topPanelEvents}} {
		model := topPanelModelAt(topTableWidth, 60, panels)
		model.view, model.lastErr = topViewNetwork, errors.New("read failed")
		model.previous.NetworkHealth = &networkHealth{Supported: true}
		text := topViewText(model)
		want := 0
		if panels[0] == topPanelNet {
			want = 1
		}
		if got := strings.Count(text, "network 09:00:29"); got != want || !strings.Contains(text, "sample failed") {
			t.Fatalf("panels %v: %d net panels, want %d:\n%s", panels, got, want, text)
		}
	}
}

func TestTopAIKeyHintShowsOnlyWhenOn(t *testing.T) {
	model := topFixtureModel(nil)
	model.width = 120
	if strings.Contains(ansi.Strip(strings.Join(model.statusLines(), "\n")), "a ai") {
		t.Fatal("the a hint must stay in help while the AI panel is off")
	}
	model.aiOn = true
	if !strings.Contains(ansi.Strip(strings.Join(model.statusLines(), "\n")), "a ai") {
		t.Fatal("the a hint must show while the AI panel is on")
	}
}

func TestTopColumnsParse(t *testing.T) {
	valid := []struct {
		value string
		want  []topColumnGroup
	}{
		{"all", nil},
		{"net+, cpu ,mem", []topColumnGroup{{"network", true}, {"cpu", false}, {"mem", false}}},
		{"disk", []topColumnGroup{{"disk", false}}},
	}
	for _, test := range valid {
		got, err := parseTopColumns(test.value)
		if err != nil || !reflect.DeepEqual(got, test.want) {
			t.Errorf("parseTopColumns(%q) = %v, %v; want %v", test.value, got, err, test.want)
		}
	}
	for _, value := range []string{"", "net,", "network", "net,net+", "all,cpu", "cpu++", "+"} {
		if got, err := parseTopColumns(value); err == nil {
			t.Errorf("parseTopColumns(%q) = %v, want error", value, got)
		}
	}
}

func topLayoutTitles(columns []topAllColumn) (groups, titles []string) {
	for _, column := range columns {
		if len(groups) == 0 || groups[len(groups)-1] != column.group {
			groups = append(groups, column.group)
		}
		titles = append(titles, column.title)
	}
	return groups, titles
}

func TestTopColumnsKeepTheGroupOrderAndShowTheDetailGroupFirst(t *testing.T) {
	plain, _ := parseTopColumns("cpu,net,mem")
	groups, titles := topLayoutTitles(must(topAllLayoutOf(100, topAllGroupColumns(plain))))
	if !slices.Equal(groups, []string{"cpu", "network", "mem"}) || slices.Contains(titles, "read") {
		t.Fatalf("groups %v, titles %v", groups, titles)
	}
	// 같은 폭에서 +가 없으면 hot core가 먼저 들어가고, net+이면 network의 packet 열이 먼저 들어간다.
	detail, _ := parseTopColumns("cpu,net+,mem")
	_, detailTitles := topLayoutTitles(must(topAllLayoutOf(topTableWidth, topAllGroupColumns(detail))))
	_, plainTitles := topLayoutTitles(must(topAllLayoutOf(topTableWidth, topAllGroupColumns(plain))))
	if !slices.Contains(detailTitles, "pk_in") || slices.Contains(detailTitles, "hot core") || slices.Contains(plainTitles, "pk_in") || !slices.Contains(plainTitles, "hot core") {
		t.Fatalf("net+ %v, net %v", detailTitles, plainTitles)
	}
	// func 칸은 비교할 수 없으므로 제목과 폭으로 비교한다.
	shape := func(columns []topAllColumn) (shape []string) {
		for _, column := range columns {
			shape = append(shape, column.group+"/"+column.title+"/"+strconv.Itoa(column.width))
		}
		return shape
	}
	if got, want := shape(must(topAllLayoutOf(topTableWidth+40, topAllGroupColumns(nil)))), shape(must(topAllLayout(topTableWidth+40))); !slices.Equal(got, want) {
		t.Fatalf("no column groups must keep the old layout: %v, want %v", got, want)
	}
	model := topFixtureModel(nil).withLayout(topLayout{columns: detail})
	model.width = 120
	if header := ansi.Strip(model.tableHeader()[0]); strings.Index(header, "cpu") > strings.Index(header, "network") || strings.Contains(header, "disk") {
		t.Fatalf("header %q must follow cpu,net+,mem", header)
	}
}

func must(columns []topAllColumn, _ int) []topAllColumn { return columns }

func TestTopResourcePanelsShowSummaryPeakAndTopProcesses(t *testing.T) {
	model := topPanelModelAt(160, 70, []topPanel{topPanelCPU, topPanelMemory, topPanelDisk, topPanelPSI})
	model.rows[len(model.rows)-1].processes[1].DiskValid, model.rows[len(model.rows)-1].processes[1].DiskRead = true, 5<<20
	text := topViewText(model)
	for _, want := range []string{
		"cpu 09:00:29 · load 1.5 · usr 20.0% sys 5.0%", "cores .#.. · peak 60s usr+sys 25.0% at 09:00:00", "top alpha 90% · beta 80% · gamma 70%",
		"mem 09:00:29 · 69.0% of 16.00 GB · swap out", "peak 60s 69.0% at 09:00:29", "top alpha 1.0M",
		"disk 09:00:29 · read 83.0M/s write 3.81M/s · iops 120 · await 174.0ms · busy 30%", "peak 60s await 174.0ms at 09:00:29", "top i/o beta 5.00M/s · alpha —",
		"psi 09:00:29 · some cpu 1.0 mem 29.0 io 3.0", "no process waits for I/O",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("resource panels lack %q:\n%s", want, text)
		}
	}
	noIO := model.rows[len(model.rows)-1]
	noIO.processes = []topProcess{{PID: 1, Command: "alpha", DiskStatus: "permission denied"}}
	if got := topPanelDiskProcesses(noIO); got != "— no process I/O measured · permission denied" {
		t.Fatalf("disk top line without I/O = %q", got)
	}
	// macOS는 PSI를 수집하지 않는다.
	model.details.System, model.rows[len(model.rows)-1].rate.PSIValid = "darwin", false
	if lines := model.resourcePanelLines(topPanelPSI); len(lines) != 1 || !strings.Contains(lines[0], "Linux only") {
		t.Fatalf("macOS psi panel = %v", lines)
	}
}

func TestTopDiskPanelReadsProcessIO(t *testing.T) {
	var scanIO []bool
	model := topFixtureModel(nil)
	model.setScanIO = func(on bool) { scanIO = append(scanIO, on) }
	model.syncScanIO()
	model.withLayout(topLayout{panels: []topPanel{topPanelDisk}, panelsSet: true}).syncScanIO()
	if !slices.Equal(scanIO, []bool{false, true}) {
		t.Fatalf("scan I/O = %v, want the disk panel to turn it on", scanIO)
	}
}

func TestTopAIColumnsShowRequestsPerRow(t *testing.T) {
	groups, err := parseTopColumns("cpu,ai")
	if err != nil {
		t.Fatal(err)
	}
	if all, _ := parseTopColumns("all"); all != nil || slices.ContainsFunc(topAllColumnGroups(), func(group topColumnGroup) bool { return group.name == topAIColumnGroup }) {
		t.Fatal("all must not read the AI logs")
	}
	model := topSplitModelAt(160, 40, 3, false, nil)
	model.view, model.split = topViewAll, nil
	previous, at := model.rows[1].at, model.rows[2].at
	source := &fakeTopAISource{usage: aiUsageSnapshot{Seconds: map[string]map[int64][]aiStampedUsage{
		"claude": {
			at.Unix():       {{At: at.UnixMilli(), Usage: aiUsage{Requests: 1, Input: 2300, Output: 410, Cache: 99_000}}},
			previous.Unix(): {{At: previous.UnixMilli(), Usage: aiUsage{Requests: 2, Input: 10, Output: 5}}},
		},
		"codex": {},
	}}}
	opened := 0
	model.openAI = func() (topAISource, error) {
		opened++
		return source, nil
	}
	model = model.withLayout(topLayout{columns: groups})
	if !model.aiTokens || model.aiOn {
		t.Fatalf("tokens %v, panel %v", model.aiTokens, model.aiOn)
	}
	// 그룹 머리줄이 도구 이름을, 열 이름이 req와 tok을 보인다.
	if header := ansi.Strip(strings.Join(model.tableHeader(), "\n")); !strings.Contains(header, " claude ") || !strings.Contains(header, " codex ") || !strings.Contains(header, "req") || !strings.Contains(header, "tok") {
		t.Fatalf("header %q lacks the ai columns", header)
	}
	if row := ansi.Strip(model.tableRow(model.rows[2])); !strings.Contains(row, "—") {
		t.Fatalf("tokens before the first scan must be —: %q", row)
	}
	updated, cmd := model.Update(topAIStartMsg{})
	model = updated.(topModel)
	if cmd == nil || opened != 1 || !model.aiScanning {
		t.Fatalf("start: cmd %v, opened %d, scanning %v", cmd != nil, opened, model.aiScanning)
	}
	updated, next := model.Update(cmd())
	model = updated.(topModel)
	if source.scans != 1 || source.polls != 0 || next == nil {
		t.Fatalf("scans %d, polls %d, next %v", source.scans, source.polls, next != nil)
	}
	// 행마다 직전 행 뒤부터 그 행까지의 초만 센다. cache는 열에 없다.
	tokens := model.aiTokensBetween(previous, at)
	if tokens.claude != (aiUsage{Requests: 1, Input: 2300, Output: 410, Cache: 99_000}) || tokens.codex != (aiUsage{}) || !tokens.claudeOK || !tokens.codexOK {
		t.Fatalf("tokens = %+v", tokens)
	}
	cells := func(row topDashboardRow) []string {
		line := ansi.Strip(model.tableRow(row))
		parts := strings.Split(line, "│")
		return append(strings.Fields(parts[len(parts)-3]), strings.Fields(parts[len(parts)-2])...)
	}
	// tok은 in + out이다. 2300 + 410 = 2710이다.
	if got := cells(model.rows[2]); !slices.Equal(got, []string{"1", "2.7K", "·", "·"}) {
		t.Fatalf("row 09:00:02 ai cells = %v", got)
	}
	if got := cells(model.rows[1]); !slices.Equal(got, []string{"2", "15", "·", "·"}) {
		t.Fatalf("row 09:00:01 ai cells = %v", got)
	}
}

// 행 시각은 초 경계에 맞지 않는다. 00.800의 요청은 00.200 행이 아니라 01.200 행에, 01.500의 요청은 그다음 행에 붙어야 한다.
func TestTopAIColumnsUseTheExactRowInterval(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	stamp := func(offset time.Duration, requests int64) aiStampedUsage {
		return aiStampedUsage{At: base.Add(offset).UnixMilli(), Usage: aiUsage{Requests: requests, Input: 100 * requests}}
	}
	model := topFixtureModel(nil)
	model.rows = []topDashboardRow{{at: base.Add(200 * time.Millisecond)}, {at: base.Add(1200 * time.Millisecond)}, {at: base.Add(2200 * time.Millisecond)}}
	model.aiSeconds = map[string]map[int64][]aiStampedUsage{"claude": {
		base.Unix():     {stamp(100*time.Millisecond, 1), stamp(800*time.Millisecond, 2)},
		base.Unix() + 1: {stamp(1500*time.Millisecond, 4)},
	}}
	for index, want := range []int64{1, 2, 4} {
		row := model.rows[index]
		if got := model.aiTokensBetween(model.previousRowAt(row.at), row.at).claude.Requests; got != want {
			t.Errorf("row %s: %d requests, want %d", row.at.Format("05.000"), got, want)
		}
	}
}

// 행 시각은 ns이고 요청 시각은 ms다. 행과 같은 ms 안에서 더 늦은 요청도 두 끝을 같이 내린 앞 행에 한 번만 들어간다.
func TestTopAIColumnsCountARequestOnceAcrossSubMillisecondRows(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	model := topFixtureModel(nil)
	model.rows = []topDashboardRow{{at: base.Add(200500 * time.Microsecond)}, {at: base.Add(1200500 * time.Microsecond)}}
	model.aiSeconds = map[string]map[int64][]aiStampedUsage{"claude": {
		base.Unix(): {{At: base.Add(200900 * time.Microsecond).UnixMilli(), Usage: aiUsage{Requests: 1}}},
	}}
	var counts []int64
	for _, row := range model.rows {
		counts = append(counts, model.aiTokensBetween(model.previousRowAt(row.at), row.at).claude.Requests)
	}
	if !slices.Equal(counts, []int64{1, 0}) {
		t.Fatalf("requests per row = %v, want [1 0]", counts)
	}
}

// 행 사이가 길어도 저장된 초만 읽는다. 초를 하나씩 세면 이 시험은 100년 치 약 31억 초를 조회한다.
func TestTopAIColumnsReadOnlyStoredSecondsForALongGap(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	model := topFixtureModel(nil)
	model.aiSeconds = map[string]map[int64][]aiStampedUsage{"claude": {
		base.Unix() - 10: {{At: base.Add(-10 * time.Second).UnixMilli(), Usage: aiUsage{Requests: 2}}},
		base.Unix() + 10: {{At: base.Add(10 * time.Second).UnixMilli(), Usage: aiUsage{Requests: 4}}},
	}}
	if got := model.aiTokensBetween(base.AddDate(-100, 0, 0), base).claude.Requests; got != 2 {
		t.Fatalf("requests over a long gap = %d, want 2", got)
	}
}

// 기록을 읽지 못한 도구는 0이 아니라 —로 보인다. 읽은 도구는 그대로 0을 ·로 보인다.
func TestTopAIColumnsMarkAFailedScan(t *testing.T) {
	model := topSplitModelAt(160, 40, 3, false, nil)
	model.view, model.split = topViewAll, nil
	model = model.withLayout(topLayout{columns: []topColumnGroup{{name: topAIColumnGroup}}})
	model, _ = model.updateAIScan(aiUsageSnapshot{Seconds: map[string]map[int64][]aiStampedUsage{"claude": {}, "codex": {}}, Failed: map[string]bool{"claude": true}})
	parts := strings.Split(ansi.Strip(model.tableRow(model.rows[2])), "│")
	claude, codex := strings.Fields(parts[len(parts)-3]), strings.Fields(parts[len(parts)-2])
	if !slices.Equal(claude, []string{"—", "—"}) || !slices.Equal(codex, []string{"·", "·"}) {
		t.Fatalf("claude %v, codex %v", claude, codex)
	}
}

// AI 패널은 한도 조회만, ai 열은 기록 scan만 부른다. 둘 다 켜면 둘 다 부른다.
func TestTopAIStartCallsOnlyWhatIsOn(t *testing.T) {
	for _, test := range []struct {
		name         string
		layout       topLayout
		polls, scans int
	}{
		{"panel", topLayout{panels: []topPanel{topPanelAI}, panelsSet: true}, 1, 0},
		{"columns", topLayout{columns: []topColumnGroup{{name: topAIColumnGroup}}}, 0, 1},
		{"both", topLayout{panels: []topPanel{topPanelAI}, panelsSet: true, columns: []topColumnGroup{{name: topAIColumnGroup}}}, 1, 1},
	} {
		source := &fakeTopAISource{}
		model := topFixtureModel(nil)
		model.openAI = func() (topAISource, error) { return source, nil }
		model = model.withLayout(test.layout)
		_, cmd := model.Update(topAIStartMsg{})
		if cmd == nil {
			t.Fatalf("%s: no command", test.name)
		}
		topRunBatch(cmd)
		if source.polls != test.polls || source.scans != test.scans {
			t.Errorf("%s: polls %d, scans %d; want %d, %d", test.name, source.polls, source.scans, test.polls, test.scans)
		}
	}
}

// topRunBatch는 명령을 실행한다. tea.Batch는 명령 목록을 담은 메시지를 돌려주므로 그 안의 명령도 실행한다.
func topRunBatch(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	if batch, ok := cmd().(tea.BatchMsg); ok {
		for _, inner := range batch {
			topRunBatch(inner)
		}
	}
}

// disk 패널로 시작하면 첫 tick에서 process I/O 수집을 켠다.
func TestTopDiskPanelTurnsOnProcessIOAtStart(t *testing.T) {
	var scanIO []bool
	model := topFixtureModel(nil)
	model.setScanIO = func(on bool) { scanIO = append(scanIO, on) }
	_ = model.withLayout(topLayout{panels: []topPanel{topPanelDisk}, panelsSet: true}).Init()
	if !slices.Equal(scanIO, []bool{true}) {
		t.Fatalf("scan I/O at start = %v", scanIO)
	}
}
