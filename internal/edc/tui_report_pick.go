package edc

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// reportPairModel은 report diff에서 비교할 두 report를 한 화면에서 고른다. 목록은 최근 것부터이고, 가장 최근 두 개를
// 미리 골라 두어 Enter 한 번이면 직전과 지금을 비교한다. before와 after는 고른 순서가 아니라 시간 순서로 정한다.
// 두 번 따로 고르게 하면 같은 파일을 두 번 고르거나 앞뒤를 뒤집기 쉬웠다.
type reportPairModel struct {
	reports []savedReport
	now     time.Time
	// cursor가 len(reports)이면 경로를 직접 입력하는 줄이다.
	cursor int
	// picked는 고른 순서대로 쌓인 reports의 위치다. 많아야 두 개다.
	picked        []int
	width, height int
	color         bool
	notice        string
	done          bool
	cancelled     bool
	// enterPath는 경로 입력 줄에서 Enter를 눌렀다는 뜻이다. 모자란 report는 화면을 닫은 뒤 경로로 받는다.
	enterPath bool
}

// reportPairMinFile은 폭이 모자랄 때 파일 이름을 줄여도 남기는 칸 수다.
const reportPairMinFile = 16

func newReportPairModel(reports []savedReport, now time.Time) reportPairModel {
	model := reportPairModel{reports: reports, now: now, color: true}
	for index := range min(2, len(reports)) {
		model.picked = append(model.picked, index)
	}
	return model
}

func (model reportPairModel) Init() tea.Cmd { return nil }

func (model reportPairModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		model.width, model.height = msg.Width, msg.Height
		return model, nil
	case tea.KeyPressMsg:
		return model.key(msg.String())
	}
	return model, nil
}

func (model reportPairModel) key(key string) (tea.Model, tea.Cmd) {
	rows := len(model.reports) + 1
	model.notice = ""
	switch key {
	case "up", "k":
		model.cursor = (model.cursor - 1 + rows) % rows
	case "down", "j":
		model.cursor = (model.cursor + 1) % rows
	case "space", " ":
		if model.cursor < len(model.reports) {
			model.toggle(model.cursor)
		}
	case "enter":
		switch {
		case model.cursor == len(model.reports) && len(model.picked) == 2:
			model.notice = T("cli.report.pick_full")
		case model.cursor == len(model.reports):
			model.done, model.enterPath = true, true
			return model, tea.Quit
		case len(model.picked) < 2:
			model.notice = T("cli.report.pick_two", len(model.picked))
		default:
			model.done = true
			return model, tea.Quit
		}
	case "esc", "q", "ctrl+c":
		model.cancelled = true
		return model, tea.Quit
	}
	return model, nil
}

// toggle은 고른 report를 풀거나 고른다. 세 번째를 고르면 가장 먼저 고른 것을 푼다.
func (model *reportPairModel) toggle(index int) {
	var picked []int
	removed := false
	for _, value := range model.picked {
		if value == index {
			removed = true
			continue
		}
		picked = append(picked, value)
	}
	if !removed {
		if len(picked) == 2 {
			picked = picked[1:]
		}
		picked = append(picked, index)
	}
	model.picked = picked
}

func (model reportPairModel) isPicked(index int) bool {
	for _, picked := range model.picked {
		if picked == index {
			return true
		}
	}
	return false
}

func (model reportPairModel) pickedReports() []savedReport {
	reports := make([]savedReport, 0, len(model.picked))
	for _, index := range model.picked {
		reports = append(reports, model.reports[index])
	}
	return reports
}

func (model reportPairModel) View() tea.View {
	rows := model.rows()
	lines := []string{model.fit(T("cli.report.pick_title")), liveMuted(model.fit(T("cli.report.pick_help")), model.color)}
	start, end := model.window(len(rows))
	width := 0
	for _, row := range rows {
		width = max(width, liveWidth(row))
	}
	if model.width > 0 {
		width = min(width, model.width-2)
	}
	for index := start; index < end; index++ {
		row := rows[index]
		if model.width > 0 {
			row = ansi.Truncate(row, model.width-2, "…")
		}
		if index != model.cursor {
			lines = append(lines, liveIdleBar+" "+row)
			continue
		}
		lines = append(lines, liveSelectedBar+" "+liveSelected(liveCell(row, width), model.color))
	}
	lines = append(lines, model.notice)
	view := tea.NewView(strings.Join(lines, "\n"))
	view.AltScreen = true
	return view
}

// fit은 제목과 안내 줄을 화면 폭에 맞춰 자른다. 폭을 아직 모르면 그대로 둔다.
func (model reportPairModel) fit(text string) string {
	if model.width <= 0 {
		return text
	}
	return ansi.Truncate(text, model.width, "…")
}

// window는 화면에 들어가는 줄 범위다. 제목, 키 안내, 알림 줄을 빼고 남는 높이에서 커서가 보이게 한다.
func (model reportPairModel) window(total int) (int, int) {
	visible := model.height - 3
	if model.height == 0 || total <= visible {
		return 0, total
	}
	visible = max(1, visible)
	start := min(max(0, model.cursor-visible/2), total-visible)
	return start, start + visible
}

// rows는 report마다 고름 표시, 시작 시각, 경과, 결과 수, 파일 이름을 한 줄로 만든다. 폭이 모자라면 경과를 빼고,
// 그다음 파일 이름의 뒤를 줄인다. 시각을 남겨야 어느 것이 앞인지 읽히고, snapshot 파일 이름 뒤쪽의 시각은 시각 열과 같다.
// 결과 수는 파일 이름을 reportPairMinFile까지 줄여도 모자랄 때만 뺀다.
func (model reportPairModel) rows() []string {
	type columns struct{ box, started, age, counts, file string }
	cells := make([]columns, 0, len(model.reports))
	ageWidth, countsWidth, fileWidth := 0, 0, 0
	for index, report := range model.reports {
		box := "[ ]"
		if model.isPicked(index) {
			box = "[x]"
		}
		started := report.report.Run.StartedAt
		age := "-"
		if elapsed := model.now.Sub(started); !started.IsZero() && elapsed >= 0 {
			age = T("cli.report.pick_age", svcFormatUptime(elapsed))
		}
		cell := columns{box: box, started: started.Local().Format(time.DateTime), age: age, counts: reportPairCounts(report.report.Results), file: reportIdentityValue(report.path)}
		ageWidth, countsWidth, fileWidth = max(ageWidth, liveWidth(cell.age)), max(countsWidth, liveWidth(cell.counts)), max(fileWidth, liveWidth(cell.file))
		cells = append(cells, cell)
	}
	// 줄 맨 앞의 막대와 공백 한 칸을 뺀 폭이다.
	available := model.width - 2
	base := liveWidth("[ ] ") + liveWidth(time.DateTime) + 2
	showAge, showCounts := true, true
	if model.width > 0 {
		if base+ageWidth+2+countsWidth+2+fileWidth > available {
			showAge = false
		}
		if base+countsWidth+2+reportPairMinFile > available {
			showCounts = false
		}
	}
	rows := make([]string, 0, len(cells)+1)
	for _, cell := range cells {
		line := cell.box + " " + cell.started + "  "
		if showAge {
			line += liveCell(cell.age, ageWidth) + "  "
		}
		if showCounts {
			line += liveCell(cell.counts, countsWidth) + "  "
		}
		file := cell.file
		if model.width > 0 {
			file = ansi.Truncate(file, max(reportPairMinFile/2, available-liveWidth(line)), "…")
		}
		rows = append(rows, line+file)
	}
	return append(rows, strings.Repeat(" ", liveWidth("[ ] "))+T("cli.report.enter_path"))
}

// reportPairCounts는 0인 항목을 뺀 결과 수다. 좁은 화면에서도 결과 수가 한 줄에 남게 한다.
func reportPairCounts(results []Result) string {
	summary := summarize(results)
	var parts []string
	for _, part := range []struct {
		count int
		name  string
	}{{summary.Pass, "pass"}, {summary.Warn, "warn"}, {summary.Fail, "fail"}, {summary.Skip, "skip"}} {
		if part.count > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", part.count, part.name))
		}
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, " · ")
}

// runReportPair는 선택 화면을 띄운다. 취소는 errRemoteCancelled다.
func runReportPair(input *os.File, output io.Writer, model reportPairModel) (reportPairModel, error) {
	final, err := tea.NewProgram(model, tea.WithInput(input), tea.WithOutput(output)).Run()
	if err != nil {
		if errors.Is(err, tea.ErrInterrupted) || errors.Is(err, tea.ErrProgramKilled) {
			return reportPairModel{}, errRemoteCancelled
		}
		return reportPairModel{}, err
	}
	chosen, ok := final.(reportPairModel)
	if !ok || chosen.cancelled || !chosen.done {
		return reportPairModel{}, errRemoteCancelled
	}
	return chosen, nil
}

// orderReportPair는 시작 시각이 이른 쪽을 before로 둔다. 시각이 같거나 한쪽 시각을 모르면 받은 순서를 지킨다.
func orderReportPair(first, second savedReport) (savedReport, savedReport) {
	firstStarted, secondStarted := first.report.Run.StartedAt, second.report.Run.StartedAt
	if !firstStarted.IsZero() && !secondStarted.IsZero() && secondStarted.Before(firstStarted) {
		return second, first
	}
	return first, second
}
