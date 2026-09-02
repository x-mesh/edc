package edc

import (
	"fmt"
	"os"
	"strings"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
)

// viewerFixedLines는 제목과 도움말이 차지하는 줄 수다.
const viewerFixedLines = 2

// viewerEntry는 목록의 한 항목이다. line은 접었을 때, detail은 펼쳤을 때 보여 준다.
type viewerEntry struct {
	line   string
	detail []string
	keep   map[string]bool // 필터 이름별로 이 항목을 남길지
}

type viewerFilter struct {
	name string
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
		model.view.SetHeight(max(1, value.Height-viewerFixedLines))
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
		builder.WriteString("이 필터에 해당하는 항목이 없습니다\n")
	}
	return builder.String()
}

func (model viewerModel) View() tea.View {
	view := tea.NewView(model.title + "\n" + model.view.View() + "\n" + model.help())
	view.AltScreen = true
	return view
}

func (model viewerModel) help() string {
	state := "필터: " + model.filters[model.filter].name
	if model.expanded {
		state += "  ·  상세 펼침"
	}
	return state + "  ·  f 필터, e 상세, ↑/↓ 이동, q 종료"
}

// runReportViewer는 목록을 alt screen으로 보여 준다. 종료하면 화면이 원래대로 돌아온다.
func runReportViewer(title string, entries []viewerEntry, filters []viewerFilter) error {
	_, err := tea.NewProgram(newViewerModel(title, entries, filters), tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout)).Run()
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
	viewerFilterAll      = "전체"
	viewerFilterFail     = "실패만"
	viewerFilterProblems = "실패와 경고"
	viewerFilterChanged  = "바뀐 것"
	viewerFilterWorse    = "악화된 것"
)

func resultFilters() []viewerFilter {
	return []viewerFilter{{name: viewerFilterAll}, {name: viewerFilterProblems}, {name: viewerFilterFail}}
}

func diffFilters() []viewerFilter {
	return []viewerFilter{{name: viewerFilterAll}, {name: viewerFilterChanged}, {name: viewerFilterWorse}}
}

func reportViewerTitle(label, path string, summary string) string {
	return fmt.Sprintf("%s  %s  ·  %s", label, path, summary)
}

func summaryLine(results []Result) string {
	s := summarize(results)
	return fmt.Sprintf("%d pass · %d warn · %d fail · %d skip", s.Pass, s.Warn, s.Fail, s.Skip)
}
