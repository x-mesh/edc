package edc

import (
	"fmt"
	"os"
	"strings"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// viewerFixedLines는 제목과 도움말이 차지하는 줄 수다.
const viewerFixedLines = 2

// viewerEntry는 목록의 한 항목이다. line은 접었을 때, detail은 펼쳤을 때 보여 준다.
type viewerEntry struct {
	line   string
	detail []string
	keep   map[string]bool // 필터 이름별로 이 항목을 남길지
}

// viewerFilter는 목록을 거르는 기준이다.
// name은 entry.keep의 키라 언어와 상관없이 같고, label만 화면에 보인다.
type viewerFilter struct {
	name  string
	label string
}

type viewerModel struct {
	title    string
	entries  []viewerEntry
	filters  []viewerFilter
	filter   int
	expanded bool
	view     viewport.Model
	width    int
	height   int
	color    bool
}

func newViewerModel(title string, entries []viewerEntry, filters []viewerFilter) viewerModel {
	model := viewerModel{title: title, entries: entries, filters: filters, view: viewport.New()}
	model.view.SetContent(model.body())
	return model
}

func (model viewerModel) Init() tea.Cmd { return nil }

func (model viewerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch value := msg.(type) {
	case tea.WindowSizeMsg:
		model.width, model.height = value.Width, value.Height
		model.view.SetWidth(value.Width)
		model.view.SetHeight(max(1, value.Height-viewerFixedLines-strings.Count(model.title, "\n")))
		model.view.SetContent(model.body())
		return model, nil
	case tea.KeyPressMsg:
		switch value.String() {
		case "q", "esc", "ctrl+c":
			return model, tea.Quit
		case "f":
			model.filter = (model.filter + 1) % len(model.filters)
			model.view.SetContent(model.body())
			model.view.GotoTop()
			return model, nil
		case "e":
			model.expanded = !model.expanded
			model.view.SetContent(model.body())
			model.view.GotoTop()
			return model, nil
		}
	}
	var cmd tea.Cmd
	model.view, cmd = model.view.Update(msg)
	return model, cmd
}

// body는 현재 필터와 펼침 상태로 본문을 만든다.
func (model viewerModel) body() string {
	name := model.filters[model.filter].name
	var builder strings.Builder
	shown := 0
	for _, entry := range model.entries {
		if !entry.keep[name] {
			continue
		}
		shown++
		builder.WriteString(entry.line)
		if !strings.HasSuffix(entry.line, "\n") {
			builder.WriteString("\n")
		}
		if !model.expanded {
			continue
		}
		for _, line := range entry.detail {
			builder.WriteString(line + "\n")
		}
	}
	if shown == 0 {
		builder.WriteString(T("cli.report.filter_empty") + "\n")
	}
	return builder.String()
}

func (model viewerModel) View() tea.View {
	view := tea.NewView(model.fit(model.title) + "\n" + model.view.View() + "\n" + model.fit(model.help()))
	view.AltScreen = true
	return view
}

// fit은 제목과 키 안내의 줄마다 화면 폭에 맞춰 자른다. 넘치는 줄은 renderer가 다음 줄로 넘겨 본문을 밀어낸다.
func (model viewerModel) fit(text string) string {
	if model.width <= 0 {
		return text
	}
	lines := strings.Split(text, "\n")
	for index, line := range lines {
		lines[index] = ansi.Truncate(line, model.width, "…")
	}
	return strings.Join(lines, "\n")
}

func (model viewerModel) help() string {
	if model.color {
		state := richDim(T("cli.report.filter_label")) + richBold(model.filters[model.filter].label)
		if model.expanded {
			state += richDim(T("cli.report.detail_open"))
		}
		var on []string
		if model.expanded {
			on = append(on, "e")
		}
		return state + liveKeyHints(T("cli.report.viewer_help"), true, on...)
	}
	state := T("cli.report.filter_label") + model.filters[model.filter].label
	if model.expanded {
		state += T("cli.report.detail_open")
	}
	return state + T("cli.report.viewer_help")
}

// runReportViewer는 목록을 alt screen으로 보여 준다. 종료하면 화면이 원래대로 돌아온다.
func runReportViewer(title string, entries []viewerEntry, filters []viewerFilter) error {
	model := newViewerModel(title, entries, filters)
	model.color = true
	_, err := tea.NewProgram(model, tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout)).Run()
	return err
}

// resultEntries는 report 결과를 뷰어 항목으로 바꾼다.
func resultEntries(results []Result, color bool) []viewerEntry {
	entries := make([]viewerEntry, 0, len(results))
	for _, result := range results {
		var detail strings.Builder
		printResultDetail(&detail, result, true, color)
		entries = append(entries, viewerEntry{
			line:   strings.TrimRight(formatResultLine(result, color), "\n"),
			detail: splitDetailLines(detail.String()),
			keep: map[string]bool{
				viewerFilterAll:      true,
				viewerFilterFail:     result.Status == StatusFail,
				viewerFilterProblems: result.Status == StatusFail || result.Status == StatusWarn,
			},
		})
	}
	return entries
}

// diffEntries는 report diff 결과를 뷰어 항목으로 바꾼다.
func diffEntries(diff reportDiff, color bool) []viewerEntry {
	entries := make([]viewerEntry, 0, len(diff.Entries))
	for _, entry := range diff.Entries {
		var line strings.Builder
		var detail strings.Builder
		printReportDiffEntry(&line, &detail, entry, color)
		entries = append(entries, viewerEntry{
			line:   strings.TrimRight(line.String(), "\n"),
			detail: splitDetailLines(detail.String()),
			keep: map[string]bool{
				viewerFilterAll:     true,
				viewerFilterChanged: entry.Change != changeSame,
				viewerFilterWorse:   entry.Regressed,
			},
		})
	}
	return entries
}

func splitDetailLines(value string) []string {
	value = strings.TrimRight(value, "\n")
	if value == "" {
		return nil
	}
	return strings.Split(value, "\n")
}

const (
	viewerFilterAll      = "all"
	viewerFilterFail     = "fail"
	viewerFilterProblems = "problems"
	viewerFilterChanged  = "changed"
	viewerFilterWorse    = "worse"
)

func resultFilters() []viewerFilter {
	return []viewerFilter{
		{name: viewerFilterAll, label: T("cli.report.filter.all")},
		{name: viewerFilterProblems, label: T("cli.report.filter.problems")},
		{name: viewerFilterFail, label: T("cli.report.filter.fail")},
	}
}

func diffFilters() []viewerFilter {
	return []viewerFilter{
		{name: viewerFilterAll, label: T("cli.report.filter.all")},
		{name: viewerFilterChanged, label: T("cli.report.filter.changed")},
		{name: viewerFilterWorse, label: T("cli.report.filter.worse")},
	}
}

func reportViewerTitle(label, path string, summary string) string {
	return fmt.Sprintf("%s  %s  ·  %s", label, path, summary)
}

// reportViewerHeading은 색을 쓰는 뷰어의 제목이다. 글자는 reportViewerTitle과 같다.
// summary의 개수 묶음은 sep으로 나뉘고, 0이 아닌 묶음은 colors에서 낱말에 맞는 색을 받는다. 0인 묶음은 회색이다.
func reportViewerHeading(label, path, summary, sep string, colors map[string]string) string {
	return richPaint(label, richMagenta, true) + "  " + richDim(path) + richDim("  ·  ") + reportPaintCounts(summary, sep, colors, true)
}

// reportPaintCounts는 "2 pass · 1 fail" 꼴의 개수 묶음을 칠한다. 구분자는 회색이다.
func reportPaintCounts(summary, sep string, colors map[string]string, bold bool) string {
	parts := strings.Split(summary, sep)
	for index, part := range parts {
		count, word, _ := strings.Cut(part, " ")
		switch {
		case count == "0":
			parts[index] = richDim(part)
		case colors[word] != "":
			parts[index] = richPaint(part, colors[word], bold)
		}
	}
	return strings.Join(parts, richDim(sep))
}

// reportSummaryColors와 reportDiffColors는 제목의 개수 묶음 색이다. 키는 숫자 뒤의 첫 낱말이다.
var (
	reportSummaryColors = map[string]string{"pass": richGreen, "warn": richYellow, "fail": richRed}
	reportDiffColors    = map[string]string{"worse": richRed}
)

// reportDiffIdentityHeading은 diff 제목 아래의 대상 비교 줄이다. 보조 정보라 회색으로, 대상이 다르다는 경고만 노랑으로 둔다.
func reportDiffIdentityHeading(identity string) string {
	lines := strings.Split(identity, "\n")
	for index, line := range lines {
		if line == T("cli.report.identity_differs") {
			lines[index] = richPaint(line, richYellow, false)
			continue
		}
		lines[index] = richDim(line)
	}
	return strings.Join(lines, "\n")
}

func summaryLine(results []Result) string {
	s := summarize(results)
	return fmt.Sprintf("%d pass · %d warn · %d fail · %d skip", s.Pass, s.Warn, s.Fail, s.Skip)
}
