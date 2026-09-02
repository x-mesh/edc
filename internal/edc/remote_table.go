package edc

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"
)

const (
	remoteColumnGap = 2
	// remoteWordWidth는 PASS, FAIL 같은 상태 단어의 폭이다.
	remoteWordWidth = 4
	// remoteSymbolWidth는 기호 하나의 폭이다.
	remoteSymbolWidth = 1
	// remoteMinStepWidth와 remoteMinHostWidth는 이름을 줄여도 남기는 최소 폭이다.
	remoteMinStepWidth = 3
	remoteMinHostWidth = 8
	remoteHostHeader   = "host"
	remoteDefaultWidth = 80
)

// remoteGlyph는 폭이 좁을 때 쓰는 기호다. 색을 못 써도 상태를 구분할 수 있어야 한다.
const (
	remoteGlyphPass    = "✓"
	remoteGlyphFail    = "✗"
	remoteGlyphWarn    = "!"
	remoteGlyphSkip    = "~"
	remoteGlyphAbsent  = "–"
	remoteGlyphPending = "·"
)

// remoteSymbolLegend는 기호 표기의 뜻을 한 줄로 알린다.
func remoteSymbolLegend() string { return T("remote.legend.symbols") }

// remoteTable은 host를 행, step을 열로 놓은 표의 배치다. 계획과 실행 결과가 같은 배치를 쓴다.
type remoteTable struct {
	hosts      []string
	steps      []string
	hostLabels []string // 폭에 맞춰 줄인 행 이름
	stepLabels []string // 폭에 맞춰 줄인 열 이름
	nameWidth  int
	widths     []int
	symbols    bool // 상태를 기호로 줄여 쓴다
}

func newRemoteTable(hosts []remoteHost, recipe remoteRecipe, width int) remoteTable {
	table := remoteTable{}
	for _, host := range hosts {
		table.hosts = append(table.hosts, host.Name)
	}
	for _, step := range recipe.Steps {
		table.steps = append(table.steps, step.Name)
	}
	return table.withWidth(width)
}

// withWidth는 주어진 폭에 맞는 배치를 고른다.
// 상태 단어가 가장 읽기 쉬우므로 먼저 시도하고, 넘치면 기호로, 그래도 넘치면 이름을 줄인다.
func (table remoteTable) withWidth(width int) remoteTable {
	if width <= 0 {
		width = remoteDefaultWidth
	}
	if fitted := table.apply(remoteWordWidth, false, 0, 0); fitted.total() <= width {
		return fitted
	}
	// 상태 단어는 열 이름을 줄여서라도 지킨다. 단어 폭보다 좁아지면 그때 기호로 바꾼다.
	longestStep := table.longestName(table.steps)
	for limit := longestStep - 1; limit >= remoteWordWidth; limit-- {
		if fitted := table.apply(remoteWordWidth, false, limit, 0); fitted.total() <= width {
			return fitted
		}
	}
	for limit := remoteWordWidth; limit >= remoteMinStepWidth; limit-- {
		if fitted := table.apply(remoteSymbolWidth, true, limit, 0); fitted.total() <= width {
			return fitted
		}
	}
	longestHost := table.longestName(table.hosts)
	for limit := longestHost - 1; limit >= remoteMinHostWidth; limit-- {
		if fitted := table.apply(remoteSymbolWidth, true, remoteMinStepWidth, limit); fitted.total() <= width {
			return fitted
		}
	}
	return table.apply(remoteSymbolWidth, true, remoteMinStepWidth, remoteMinHostWidth)
}

// apply는 상태 폭과 이름 제한으로 열을 배치한 사본을 만든다. 제한이 0이면 이름을 그대로 쓴다.
func (table remoteTable) apply(statusWidth int, symbols bool, stepLimit, hostLimit int) remoteTable {
	table.symbols = symbols
	table.hostLabels = shortenNames(table.hosts, hostLimit)
	table.stepLabels = shortenNames(table.steps, stepLimit)
	table.nameWidth = max(liveWidth(remoteHostHeader), table.longestName(table.hostLabels))
	table.widths = make([]int, len(table.stepLabels))
	for index, label := range table.stepLabels {
		table.widths[index] = max(liveWidth(label), statusWidth)
	}
	return table
}

func shortenNames(names []string, limit int) []string {
	shortened := make([]string, 0, len(names))
	for _, name := range names {
		if limit <= 0 {
			shortened = append(shortened, name)
			continue
		}
		shortened = append(shortened, truncateName(name, limit))
	}
	return shortened
}

func (table remoteTable) longestName(names []string) int {
	longest := 0
	for _, name := range names {
		longest = max(longest, liveWidth(name))
	}
	return longest
}

func (table remoteTable) total() int {
	total := table.nameWidth + remoteColumnGap
	for _, width := range table.widths {
		total += width + remoteColumnGap
	}
	return total
}

// truncateName은 이름을 표시 폭 기준으로 줄이고 끝에 생략 기호를 붙인다.
func truncateName(name string, limit int) string {
	if liveWidth(name) <= limit {
		return name
	}
	runes := []rune(name)
	for len(runes) > 0 && liveWidth(string(runes))+1 > limit {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + "…"
}

func (table remoteTable) header() string {
	var builder strings.Builder
	builder.WriteString(liveCell(remoteHostHeader, table.nameWidth+remoteColumnGap))
	for index, label := range table.stepLabels {
		builder.WriteString(liveCell(label, table.widths[index]+remoteColumnGap))
	}
	return strings.TrimRight(builder.String(), " ")
}

// row는 host 한 줄을 그린다. cells는 step 순서와 같아야 한다.
func (table remoteTable) row(host string, cells []string) string {
	var builder strings.Builder
	builder.WriteString(liveCell(table.hostLabel(host), table.nameWidth+remoteColumnGap))
	for index, cell := range cells {
		builder.WriteString(liveCell(cell, table.widths[index]+remoteColumnGap))
	}
	return strings.TrimRight(builder.String(), " ")
}

// statusCell은 상태를 표의 폭에 맞는 표기로 바꾼다.
// hostLabel은 폭에 맞춰 줄인 host 이름이다.
func (table remoteTable) hostLabel(host string) string {
	for index, name := range table.hosts {
		if name == host {
			return table.hostLabels[index]
		}
	}
	return host
}

func (table remoteTable) statusCell(status Status, color bool) string {
	if !table.symbols {
		return terminalStatus(status, color)
	}
	glyph := remoteGlyphPending
	switch status {
	case StatusPass:
		glyph = remoteGlyphPass
	case StatusFail:
		glyph = remoteGlyphFail
	case StatusWarn:
		glyph = remoteGlyphWarn
	case StatusSkip:
		glyph = remoteGlyphSkip
	}
	return paintStatus(glyph, status, color)
}

// paintStatus는 terminalStatus와 같은 색 규칙을 기호에 적용한다.
func paintStatus(text string, status Status, color bool) string {
	if !color {
		return text
	}
	code := ""
	switch status {
	case StatusPass:
		code = "32"
	case StatusFail:
		code = "31"
	case StatusWarn:
		code = "33"
	case StatusSkip:
		code = "90"
	}
	if code == "" {
		return text
	}
	return "\033[" + code + "m" + text + "\033[0m"
}

// terminalWidth는 stdout의 폭이다. 알 수 없으면 기본값을 쓴다.
func terminalWidth() int {
	width, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || width <= 0 {
		return remoteDefaultWidth
	}
	return width
}

// shortPath는 현재 디렉터리 안의 파일을 상대 경로로 줄인다. 밖에 있으면 절대 경로를 그대로 쓴다.
func shortPath(cwd, path string) string {
	if cwd == "" || path == "" {
		return path
	}
	absolute := path
	if !filepath.IsAbs(absolute) {
		absolute = filepath.Join(cwd, absolute)
	}
	relative, err := filepath.Rel(cwd, absolute)
	if err != nil || strings.HasPrefix(relative, "..") {
		return absolute
	}
	return "./" + relative
}

// remoteStepLegend는 표 아래에 붙는 명령 목록이다.
func remoteStepLegend(recipe remoteRecipe, hosts []remoteHost) []string {
	width := 0
	for _, step := range recipe.Steps {
		width = max(width, liveWidth(step.Name))
	}
	lines := make([]string, 0, len(recipe.Steps))
	for _, step := range recipe.Steps {
		line := liveCell(step.Name, width+remoteColumnGap) + step.Command
		if step.Verify != "" {
			line += "  →  " + step.Verify
		}
		if len(step.Tags) > 0 {
			line += "   tags " + strings.Join(step.Tags, ", ")
			if len(stepHostNames(step, hosts)) == 0 {
				line += " " + T("remote.label.no_target")
			}
		}
		lines = append(lines, line)
	}
	return lines
}

// remoteRunHeader는 실행 대상과 파일을 두 줄로 요약한다.
func remoteRunHeader(group, inventoryPath, recipePath, cwd string, hosts []remoteHost, recipe remoteRecipe) string {
	planned := 0
	for _, host := range hosts {
		for _, step := range recipe.Steps {
			if stepRunsOnHost(step, host) {
				planned++
			}
		}
	}
	return fmt.Sprintf("%s\ninventory  %s      recipe  %s\n",
		T("remote.header.summary", group, len(hosts), len(recipe.Steps), planned),
		shortPath(cwd, inventoryPath), shortPath(cwd, recipePath))
}
