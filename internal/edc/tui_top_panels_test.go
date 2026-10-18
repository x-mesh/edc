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
	}
	for _, test := range valid {
		got, err := parseTopPanels(test.value)
		if err != nil || !reflect.DeepEqual(got, test.want) {
			t.Errorf("parseTopPanels(%q) = %v, %v; want %v", test.value, got, err, test.want)
		}
	}
	for _, value := range []string{"", "net,", "network", "ai,ai", "none,ai", "cpu"} {
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
	polls  int
	closed bool
}

func (source *fakeTopAISource) poll(context.Context, time.Duration) aiPollResult {
	source.polls++
	return source.result
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
	if !slices.Equal(topPanelNames, []topPanel{topPanelNet, topPanelAI, topPanelEvents}) {
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
