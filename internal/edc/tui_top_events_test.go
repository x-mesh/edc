package edc

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func topEventItem(kind string, score float64) []topSignalItem {
	return []topSignalItem{{text: kind + " value", score: score, view: topViewDisk, kind: kind}}
}

func TestTopEventLogOpensAfterFiveSecondsAndMergesFlaps(t *testing.T) {
	log := &topEventLog{}
	culprits := 0
	culprit := func(topView) string { culprits++; return "gm (200 procs)" }
	at := func(second int) time.Time { return time.Unix(int64(second), 0) }
	// 3초 만에 꺼진 경고는 남지 않는다.
	for second := 0; second <= 2; second++ {
		log.observe(at(second), topEventItem("await", 0.5), culprit)
	}
	log.observe(at(3), nil, culprit)
	if len(log.events) != 0 || len(log.pending) != 0 {
		t.Fatalf("a short warning was kept: %+v %+v", log.events, log.pending)
	}
	for second := 10; second <= 14; second++ {
		log.observe(at(second), topEventItem("await", 0.5), culprit)
	}
	if len(log.events) != 0 {
		t.Fatalf("opened before five seconds: %+v", log.events)
	}
	log.observe(at(15), topEventItem("await", 0.5), culprit)
	if len(log.events) != 1 || !log.events[0].start.Equal(at(10)) || log.events[0].level != topLevelWarn || log.events[0].ended {
		t.Fatalf("events = %+v", log.events)
	}
	// 더 나빠지면 문구와 단계와 원인 후보를 그 순간 것으로 바꾼다.
	log.observe(at(16), []topSignalItem{{text: "await 3570ms", score: 1.5, view: topViewDisk, kind: "await"}}, func(topView) string { return "node" })
	if event := log.events[0]; event.text != "await 3570ms" || event.level != topLevelDanger || event.culprit != "node" {
		t.Fatalf("peak = %+v", event)
	}
	// 4초 꺼졌다 다시 켜지면 같은 이벤트다.
	for second := 17; second <= 20; second++ {
		log.observe(at(second), nil, culprit)
	}
	log.observe(at(21), topEventItem("await", 0.5), culprit)
	if len(log.events) != 1 || log.events[0].ended || !log.events[0].last.Equal(at(21)) {
		t.Fatalf("a flap split the event: %+v", log.events)
	}
	for second := 22; second <= 26; second++ {
		log.observe(at(second), nil, culprit)
	}
	if !log.events[0].ended || !log.events[0].last.Equal(at(21)) {
		t.Fatalf("the event must end at its last warning: %+v", log.events[0])
	}
}

func TestTopEventFillsAnEmptyCulpritLater(t *testing.T) {
	log := &topEventLog{}
	culprit := ""
	for second := 0; second <= 5; second++ {
		log.observe(time.Unix(int64(second), 0), topEventItem("load", 0.5), func(topView) string { return culprit })
	}
	if len(log.events) != 1 || log.events[0].culprit != "" {
		t.Fatalf("events = %+v", log.events)
	}
	// 값이 그대로여도 process 목록이 생기면 원인 후보를 채우고, 한 번 채운 뒤에는 더 나빠질 때만 바꾼다.
	culprit = "node 90%"
	log.observe(time.Unix(6, 0), topEventItem("load", 0.5), func(topView) string { return culprit })
	culprit = "sshd 1%"
	log.observe(time.Unix(7, 0), topEventItem("load", 0.5), func(topView) string { return culprit })
	if log.events[0].culprit != "node 90%" {
		t.Fatalf("culprit = %q", log.events[0].culprit)
	}
}

func TestTopEventMarksWarningsAlreadyOnAtStart(t *testing.T) {
	log := &topEventLog{}
	culprit := func(topView) string { return "node 90%" }
	for second := 100; second <= 105; second++ {
		log.observe(time.Unix(int64(second), 0), topEventItem("load", 0.5), culprit)
	}
	for second := 106; second <= 111; second++ {
		log.observe(time.Unix(int64(second), 0), append(topEventItem("load", 0.5), topEventItem("await", 0.5)...), culprit)
	}
	if len(log.events) != 2 || !log.events[0].sinceStart || log.events[1].sinceStart {
		t.Fatalf("events = %+v", log.events)
	}
	model := topEventModel(120)
	model.events = log
	if line := topEventLine(topEventCluster{log.events[0]}, time.Unix(111, 0), 56, false); !strings.HasPrefix(line, "●≤"+topClock(100)+" load value") {
		t.Fatalf("footer line = %q", line)
	}
	model.eventsOpen = true
	content := model.View().Content
	if !strings.Contains(content, "●≤"+topClock(100)+"–now") || !strings.Contains(content, "● "+topClock(106)+"–now") || !strings.Contains(content, "≤ was already on when edc started") {
		t.Fatalf("event list = %q", content)
	}
}

func TestTopEventListKeepsTheSelectedEntryVisible(t *testing.T) {
	model := topEventModel(120)
	model.height = 8
	model.events.events = nil
	for index := 0; index < 10; index++ {
		model.events.events = append(model.events.events, topEvent{kind: "load", start: time.Unix(int64(index*10), 0), last: time.Unix(int64(index*10+3), 0), ended: true, level: topLevelWarn, text: fmt.Sprintf("load %d", index)})
	}
	model.eventsOpen, model.eventSelected = true, 7
	lines := model.eventListLines()
	if len(lines) > model.height-1 || !strings.HasPrefix(lines[len(lines)-2], ">") || !strings.Contains(lines[len(lines)-2], "load 2") {
		t.Fatalf("list = %q", lines)
	}
}

func TestTopEventLogKeepsTheLimit(t *testing.T) {
	log := &topEventLog{}
	culprit := func(topView) string { return "" }
	second := 0
	for index := 0; index < topEventLimit+5; index++ {
		for step := 0; step <= 5; step++ {
			log.observe(time.Unix(int64(second), 0), topEventItem("load", 0.5), culprit)
			second++
		}
		for step := 0; step <= 5; step++ {
			log.observe(time.Unix(int64(second), 0), nil, culprit)
			second++
		}
	}
	if len(log.events) != topEventLimit || log.events[0].start.Equal(time.Unix(0, 0)) {
		t.Fatalf("kept %d events, first at %v", len(log.events), log.events[0].start)
	}
}

func TestTopEventCulpritFollowsTheView(t *testing.T) {
	row := topDashboardRow{processesValid: true, processes: []topProcess{
		{PID: 1, CPU: 70, RSS: 500 << 20, Command: "node", DiskValid: true, DiskRead: 21 << 20},
		{PID: 2, CPU: 1, RSS: 2 << 20, Command: "gm", State: "D"},
	}, processTotal: topProcessTotal{Groups: []topProcessGroup{{Name: "gm", Count: 169, Blocked: 22, CPU: 20, RSS: 370 << 20, DiskValid: true, DiskWrite: 60 << 20}}}}
	for view, want := range map[topView]string{
		topViewDisk:     "gm (169 procs, 22 iowait) w 60.0M · node r 21.0M",
		topViewPressure: "gm (169 procs, 22 iowait) w 60.0M · node r 21.0M",
		topViewMemory:   "gm (169 procs, 22 iowait) ≤370M · node 500.0M",
		topViewCPU:      "gm (169 procs, 22 iowait) 20% · node 70%",
		topViewNetwork:  "",
	} {
		if got := topEventCulprit(row, view); got != want {
			t.Fatalf("view %v culprit = %q, want %q", view, got, want)
		}
	}
	// 묶음과 같은 이름의 process는 건너뛰고 다음 후보를 적는다.
	row.processes = append([]topProcess{{PID: 3, CPU: 1, Command: "gm", State: "D", DiskValid: true, DiskWrite: 90 << 20}}, row.processes...)
	if got := topEventCulprit(row, topViewDisk); got != "gm (169 procs, 22 iowait) w 60.0M · node r 21.0M" {
		t.Fatalf("a group member was repeated: %q", got)
	}
	if got := topEventCulprit(topDashboardRow{}, topViewCPU); got != "" {
		t.Fatalf("an invalid sample must not name a process: %q", got)
	}
}

func topClock(second int) string { return time.Unix(int64(second), 0).Format("15:04:05") }

func topEventModel(width int) topModel {
	model := topFixtureModel(nil)
	model.limits.color = false
	model.width, model.height = width, 30
	for second := 1; second <= 30; second++ {
		model.rows = append(model.rows, topDashboardRow{at: time.Unix(int64(second), 0), processesValid: true, processes: []topProcess{
			{PID: 1, CPU: 40, RSS: 500 << 20, Command: "node"}, {PID: 2, CPU: 3, RSS: 2 << 20, Command: "gm", State: "D"}, {PID: 3, CPU: 1, RSS: 1 << 20, Command: "sshd"},
		}})
	}
	model.selected = len(model.rows) - 1
	model.events = &topEventLog{events: []topEvent{
		{kind: "load", start: time.Unix(10, 0), last: time.Unix(14, 0), ended: true, level: topLevelWarn, text: "load 7.0", culprit: "node 90%"},
		{kind: "await", start: time.Unix(20, 0), last: time.Unix(30, 0), level: topLevelDanger, text: "await 3570ms", culprit: "gm (169 procs) w 60.2M"},
	}}
	return model
}

func TestTopEventsSitInTheFooterOrFallBackToATicker(t *testing.T) {
	for _, width := range []int{200, 120} {
		model := topEventModel(width)
		content := model.View().Content
		if !strings.Contains(content, "events · e list") || !strings.Contains(content, "● "+topClock(20)+" await 3570ms") || strings.Contains(content, "e events") {
			t.Fatalf("width %d view = %q", width, content)
		}
		footer := strings.Join(model.footerLines(), "\n")
		if !strings.Contains(footer, "events · e list") || !strings.Contains(footer, "processes ·") {
			t.Fatalf("width %d: events must share the footer with processes: %q", width, footer)
		}
		// 표는 이벤트 칸에 폭을 내주지 않는다.
		if header := model.tableHeader()[0]; ansi.StringWidth(header) < width-2 {
			t.Fatalf("width %d: table header is %d columns", width, ansi.StringWidth(header))
		}
		for _, line := range strings.Split(content, "\n") {
			if lineWidth := ansi.StringWidth(line); lineWidth > width {
				t.Fatalf("width %d: line is %d columns: %q", width, lineWidth, line)
			}
		}
		if lines := len(strings.Split(content, "\n")); lines > model.height {
			t.Fatalf("width %d renders %d lines", width, lines)
		}
	}
	narrow := topEventModel(topTableWidth)
	content := narrow.View().Content
	if strings.Contains(content, "events · e list") || !strings.Contains(content, "● "+topClock(20)+" await 3570ms · gm (169 procs) w 60.2M · e events") {
		t.Fatalf("narrow view = %q", content)
	}
	if lines := len(strings.Split(content, "\n")); lines > narrow.height {
		t.Fatalf("the ticker pushed the view to %d lines", lines)
	}
	narrow.events.events[1].ended = true
	if strings.Contains(narrow.View().Content, "e events") {
		t.Fatal("an ended event must not stay in the ticker")
	}
	// 상세 패널이 왼쪽을 차지하면 넓은 화면도 한 줄 알림으로 돌아간다.
	wide := topEventModel(200)
	wide.detail = true
	if wide.eventsInFooter() || wide.eventTicker() == "" {
		t.Fatalf("detail panel: in footer %v, ticker %q", wide.eventsInFooter(), wide.eventTicker())
	}
}

func TestTopEventsThatStartTogetherShareALine(t *testing.T) {
	model := topEventModel(120)
	model.events.events = append(model.events.events,
		topEvent{kind: "io", start: time.Unix(22, 0), last: time.Unix(30, 0), level: topLevelDanger, score: 3, text: "io 97.5%", culprit: "gm (170 procs, 6 iowait)"},
		topEvent{kind: "load", start: time.Unix(24, 0), last: time.Unix(26, 0), ended: true, level: topLevelWarn, text: "load 27.2"},
	)
	clusters := topEventClusters(model.eventList())
	if len(clusters) != 2 || len(clusters[1]) != 3 || clusters[1].worst().kind != "io" {
		t.Fatalf("clusters = %+v", clusters)
	}
	lines := model.eventLines(56, 5)
	if len(lines) != 3 || !strings.Contains(lines[1], "● "+topClock(20)+" io 97.5% +2") || !strings.Contains(lines[1], "gm (170 procs, 6") || !strings.Contains(lines[1], "10s") {
		t.Fatalf("event lines = %q", lines)
	}
	if !strings.Contains(lines[2], topClock(10)+" load 7.0") || strings.Contains(lines[2], "●") {
		t.Fatalf("an ended event line = %q", lines[2])
	}
	// e 목록도 같은 기준으로 합치고, 함께 켜진 경고는 also 줄에 최고값으로 남긴다.
	model.eventsOpen = true
	content := model.View().Content
	if strings.Count(content, "–") != 2 || !strings.Contains(content, "io 97.5% +2") || !strings.Contains(content, "also: await 3570ms · load 27.2") || !strings.Contains(content, "gm (170 procs, 6 iowait)") {
		t.Fatalf("event list = %q", content)
	}
	// 아래로는 합친 항목 수까지만 가고, Enter는 그 항목의 첫 시작으로 옮긴다.
	model = topAfter(t, model, tea.KeyPressMsg{Code: tea.KeyDown}, tea.KeyPressMsg{Code: tea.KeyDown}, tea.KeyPressMsg{Code: tea.KeyDown})
	if model.eventSelected != 1 {
		t.Fatalf("selected entry %d of 2", model.eventSelected)
	}
	model = topAfter(t, model, tea.KeyPressMsg{Code: tea.KeyUp}, tea.KeyPressMsg{Code: tea.KeyEnter})
	if model.eventsOpen || !model.rows[model.selected].at.Equal(time.Unix(20, 0)) {
		t.Fatalf("Enter selected %v", model.rows[model.selected].at)
	}
	// [와 ]는 22초와 24초에 시작한 이벤트를 따로 밟지 않고 합친 항목의 첫 시작끼리 옮긴다.
	model = model.stepEvent(-1)
	if !model.rows[model.selected].at.Equal(time.Unix(10, 0)) {
		t.Fatalf("[ selected %v", model.rows[model.selected].at)
	}
	model = model.stepEvent(1)
	if !model.rows[model.selected].at.Equal(time.Unix(20, 0)) {
		t.Fatalf("] selected %v", model.rows[model.selected].at)
	}
	if model = model.stepEvent(1); model.notice != "no later event" {
		t.Fatalf("notice = %q", model.notice)
	}
}

func TestTopEventListJumpsToTheStartRow(t *testing.T) {
	model := topEventModel(100)
	model = topAfter(t, model, tea.KeyPressMsg{Code: 'e', Text: "e"})
	content := model.View().Content
	if !model.eventsOpen || !strings.Contains(content, "Events ·") || !strings.Contains(content, "> ● "+topClock(20)+"–now") || !strings.Contains(content, "load 7.0") {
		t.Fatalf("event list = %q", content)
	}
	model = topAfter(t, model, tea.KeyPressMsg{Code: tea.KeyDown}, tea.KeyPressMsg{Code: tea.KeyEnter})
	if model.eventsOpen || model.follow || !model.rows[model.selected].at.Equal(time.Unix(10, 0)) {
		t.Fatalf("jump selected %d (follow %v, open %v)", model.selected, model.follow, model.eventsOpen)
	}
	model = topAfter(t, model, tea.KeyPressMsg{Code: ']', Text: "]"})
	if !model.rows[model.selected].at.Equal(time.Unix(20, 0)) {
		t.Fatalf("] selected %v", model.rows[model.selected].at)
	}
	model = topAfter(t, model, tea.KeyPressMsg{Code: ']', Text: "]"})
	if model.notice != "no later event" {
		t.Fatalf("notice = %q", model.notice)
	}
	model = topAfter(t, model, tea.KeyPressMsg{Code: '[', Text: "["})
	if !model.rows[model.selected].at.Equal(time.Unix(10, 0)) {
		t.Fatalf("[ selected %v", model.rows[model.selected].at)
	}
	// 히스토리에서 밀려난 이벤트는 요약만 남는다.
	model.rows = model.rows[15:]
	model.selected = len(model.rows) - 1
	if jumped := model.jumpToEvent(model.events.events[0], model.events.events[0].start); !strings.Contains(jumped.notice, "older than the history") {
		t.Fatalf("notice = %q", jumped.notice)
	}
}

func TestTopEventStartRowIsMarked(t *testing.T) {
	model := topEventModel(topTableWidth)
	var marked string
	for _, line := range strings.Split(model.View().Content, "\n") {
		if strings.HasPrefix(line, topClock(20)) {
			marked = line
		}
	}
	if len(marked) <= topSelectionColumn || marked[topSelectionColumn] != '!' {
		t.Fatalf("start row = %q", marked)
	}
}

func TestTopModelRecordsEventsFromSamples(t *testing.T) {
	model := topFixtureModel(nil)
	for second := 1; second <= 6; second++ {
		// 매초 CPU 시간의 절반이 iowait다.
		snapshot := resourceSnapshot{TakenAt: time.Unix(int64(second), 0), CPUTotal: 100 + 100*uint64(second), CPUIOWait: 50 * uint64(second)}
		model = topAfter(t, model, topSampleMsg{snapshot: snapshot})
	}
	events := model.eventList()
	if len(events) != 1 || events[0].kind != "io" || !events[0].start.Equal(time.Unix(1, 0)) || events[0].level != topLevelDanger {
		t.Fatalf("events = %+v", events)
	}
}

// memory 압박 경고는 memory 보기의 원인 후보(RSS 순)를 남긴다.
func TestTopEventLogKeepsMemoryPressureWithMemoryCandidates(t *testing.T) {
	log := &topEventLog{}
	var views []topView
	culprit := func(view topView) string { views = append(views, view); return "Google Chr (35 procs)" }
	limits := newTopLimits(4, false)
	rate := resourceRate{MemoryPressure: topMemoryPressureWarn, MemoryPressureValid: true}
	log.observe(time.Unix(0, 0), nil, culprit)
	for second := 1; second <= 7; second++ {
		log.observe(time.Unix(int64(second), 0), topSignals(rate, limits), culprit)
	}
	if len(log.events) != 1 || log.events[0].kind != "mem pressure" || log.events[0].text != "mem pressure warn" || log.events[0].level != topLevelWarn {
		t.Fatalf("events = %+v", log.events)
	}
	if log.events[0].culprit != "Google Chr (35 procs)" || len(views) == 0 || views[0] != topViewMemory {
		t.Fatalf("culprit %q from views %v", log.events[0].culprit, views)
	}
}
