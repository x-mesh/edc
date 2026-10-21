package edc

import (
	"database/sql"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// history trend를 터미널에서 지표 없이 실행하면 이 화면이 열린다. 왼쪽에서 호스트, 기록된 프로세스, 스냅샷 결과 가운데
// 대상을 고르고, 오른쪽에서 고른 지표의 구간 표를 본다. 끝내면 마지막으로 본 표와 같은 결과를 내는 명령을 보여 준다.

const (
	historyTrendTargetWidth = 24
	historyTrendBarWidth    = 10
)

type historyTrendTargetKind int

const (
	historyTrendHost historyTrendTargetKind = iota
	historyTrendProcess
	historyTrendProbe
)

type historyTrendTarget struct {
	kind  historyTrendTargetKind
	value string
	// rss는 프로세스 대상의 최대 RSS다. 목록을 이 순서로 두고 이름 옆에 보인다.
	rss float64
}

type historyTrendLoadedMsg struct {
	seq     int
	buckets []historyTrendBucket
	summary historyTrendSummary
	growth  []historyGrowthProcess
	err     error
}

type historyTrendBrowser struct {
	db       *sql.DB
	location *time.Location
	options  historyTrendOptions
	targets  []historyTrendTarget
	// metrics는 대상 종류마다 마지막으로 고른 지표다. 대상을 옮겨도 같은 종류면 같은 지표를 계속 본다.
	metrics        map[string]string
	cursor         int
	width, height  int
	color, loading bool
	seq            int
	loaded         historyTrendLoadedMsg
	table          viewport.Model
	// now는 RSS 변화의 기본 범위(최근 7일)의 끝이다.
	now time.Time
	// probeKeys는 스냅샷 결과마다 기록된 지표다. 화면을 그릴 때마다 DB를 읽지 않게 처음 읽은 값을 둔다.
	probeKeys map[string][]string
}

// historyTrendTargets는 고를 수 있는 대상이다. top 기록이 있으면 호스트를 맨 앞에, 그다음 최대 메모리가 큰 프로세스, 그다음
// 스냅샷 결과를 이름순으로 둔다.
func historyTrendTargets(db *sql.DB) ([]historyTrendTarget, error) {
	var targets []historyTrendTarget
	var top int
	if err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM top_samples)").Scan(&top); err != nil {
		return nil, err
	}
	if top == 1 {
		targets = append(targets, historyTrendTarget{kind: historyTrendHost})
		names, err := historyProcessNames(db, historyProcessByRSS)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			targets = append(targets, historyTrendTarget{kind: historyTrendProcess, value: name.Name, rss: name.MaxMemory})
		}
	}
	has, err := historyHasSnapshots(db)
	if err != nil || !has {
		return targets, err
	}
	probes, err := historyDistinct(db, "SELECT DISTINCT probe FROM snapshot_metrics ORDER BY probe")
	for _, probe := range probes {
		targets = append(targets, historyTrendTarget{kind: historyTrendProbe, value: probe})
	}
	return targets, err
}

func newHistoryTrendBrowser(db *sql.DB, targets []historyTrendTarget, options historyTrendOptions, location *time.Location) historyTrendBrowser {
	model := historyTrendBrowser{db: db, location: location, options: options, targets: targets, width: 80, height: 24,
		color: os.Getenv("NO_COLOR") == "", table: viewport.New(), now: time.Now(), probeKeys: map[string][]string{}, metrics: map[string]string{"host": "cpu_pct", "process": "rss_bytes"}}
	// 명령에 준 --process나 --probe가 목록에 없으면 맨 앞에 넣어 그 대상에서 시작한다.
	if options.process.active() || options.probe != "" {
		want := historyTrendTarget{kind: historyTrendProcess, value: options.process.String()}
		if options.probe != "" {
			want = historyTrendTarget{kind: historyTrendProbe, value: options.probe}
		}
		model.cursor = -1
		for index, target := range targets {
			if target.kind == want.kind && strings.EqualFold(target.value, want.value) {
				model.cursor = index
			}
		}
		if model.cursor < 0 {
			model.targets = append([]historyTrendTarget{want}, targets...)
			model.cursor = 0
		}
	}
	if options.probe != "" && options.metric != "" {
		model.metrics["probe:"+options.probe] = options.metric
	}
	model.resize()
	return model
}

func (model historyTrendBrowser) target() historyTrendTarget { return model.targets[model.cursor] }

func (model historyTrendBrowser) metricKey() string {
	switch target := model.target(); target.kind {
	case historyTrendHost:
		return "host"
	case historyTrendProcess:
		return "process"
	default:
		return "probe:" + target.value
	}
}

// metricNames는 고른 대상의 지표다. 스냅샷 결과는 기록된 키만 고를 수 있다.
func (model historyTrendBrowser) metricNames() []string {
	switch target := model.target(); target.kind {
	case historyTrendHost:
		return historyMetricNames(historyHostMetrics)
	case historyTrendProcess:
		return historyProcessMetricNames()
	default:
		if keys, ok := model.probeKeys[target.value]; ok {
			return keys
		}
		keys, _ := historyTrendKeys(model.db, target.value)
		model.probeKeys[target.value] = keys
		return keys
	}
}

func (model historyTrendBrowser) metric() string {
	if metric, ok := model.metrics[model.metricKey()]; ok {
		return metric
	}
	if names := model.metricNames(); len(names) > 0 {
		return names[0]
	}
	return ""
}

// trendOptions는 지금 화면에 보이는 표의 옵션이다. 끝낼 때 보여 주는 명령도 이 값으로 만든다.
func (model historyTrendBrowser) trendOptions() historyTrendOptions {
	options := model.options
	options.process, options.probe, options.metric = topProcessFilter{}, "", model.metric()
	switch target := model.target(); target.kind {
	case historyTrendProcess:
		options.process, _ = parseTopProcessFilter(target.value)
	case historyTrendProbe:
		options.probe = target.value
	}
	if !options.process.active() {
		options.each = false
	}
	return options
}

func (model historyTrendBrowser) Init() tea.Cmd { return model.load() }

func (model historyTrendBrowser) load() tea.Cmd {
	db, options, location, now, seq := model.db, model.trendOptions(), model.location, model.now, model.seq
	return func() tea.Msg { return historyTrendLoad(db, options, location, now, seq) }
}

func historyTrendLoad(db *sql.DB, options historyTrendOptions, location *time.Location, now time.Time, seq int) historyTrendLoadedMsg {
	msg := historyTrendLoadedMsg{seq: seq}
	if options.metric == "" {
		return msg
	}
	msg.summary, msg.err = historyTrendBuckets(db, options, location, func(bucket historyTrendBucket) error {
		msg.buckets = append(msg.buckets, bucket)
		return nil
	})
	if msg.err == nil && options.process.active() {
		from, to := historyGrowthWindowOf(historyGrowthOptions{from: options.from, to: options.to}, now)
		msg.growth, msg.err = historyGrowthProcesses(db, from, to, options.process)
	}
	return msg
}

func (model *historyTrendBrowser) reload() tea.Cmd {
	model.seq++
	model.loading = true
	return model.load()
}

func (model *historyTrendBrowser) resize() {
	// 제목, 빈 줄, 표 머리, 빈 줄, 누수 줄, 안내 줄, 키 줄을 뺀 높이다.
	model.table.SetWidth(max(1, model.width-historyTrendTargetWidth-3))
	model.table.SetHeight(max(1, model.height-7))
	model.refreshTable()
}

func (model historyTrendBrowser) layout() string {
	if model.options.by == "hour" {
		return "2006-01-02 15:00"
	}
	return "2006-01-02"
}

// tableRows는 구간 표의 머리와 줄이다. 칸은 history trend의 표와 같고, 끝에 평균을 기간 최대에 견준 막대를 붙인다.
func (model historyTrendBrowser) tableRows() (string, []string) {
	options := model.trendOptions()
	header := []string{"START", "COUNT", "MIN", "AVG", "P95", "MAX"}
	switch {
	case options.each:
		header = []string{"START", "PID", "STARTED", "COUNT", "SEEN", "MIN", "AVG", "P95", "MAX"}
	case options.process.active():
		header = []string{"START", "COUNT", "SEEN", "PROCS", "RESTARTS", "MIN", "AVG", "P95", "MAX"}
	}
	peak := 0.0
	for _, bucket := range model.loaded.buckets {
		peak = math.Max(peak, bucket.Max)
	}
	cells := [][]string{header}
	for _, bucket := range model.loaded.buckets {
		cells = append(cells, historyTrendRow(bucket, options, model.layout(), model.location))
	}
	widths := make([]int, len(header))
	for _, row := range cells {
		for index, cell := range row {
			widths[index] = max(widths[index], ansi.StringWidth(cell))
		}
	}
	format := func(row []string) string {
		parts := make([]string, len(row))
		for index, cell := range row {
			pad := strings.Repeat(" ", widths[index]-ansi.StringWidth(cell))
			if index == 0 {
				parts[index] = cell + pad
			} else {
				parts[index] = pad + cell
			}
		}
		return strings.Join(parts, "  ")
	}
	var rows []string
	for index, row := range cells[1:] {
		rows = append(rows, format(row)+"  "+historyTint(historyTrendBar(model.loaded.buckets[index].Avg, peak), richCyan, model.color))
	}
	return format(header), rows
}

// historyTrendBar는 value를 peak에 견준 막대다. 칸의 8분의 1까지 그린다.
func historyTrendBar(value, peak float64) string {
	if peak <= 0 || value <= 0 {
		return ""
	}
	eighths := int(math.Round(math.Min(value/peak, 1) * historyTrendBarWidth * 8))
	partial := []string{"", "▏", "▎", "▍", "▌", "▋", "▊", "▉"}
	return strings.Repeat("█", eighths/8) + partial[eighths%8]
}

func (model *historyTrendBrowser) refreshTable() {
	_, rows := model.tableRows()
	model.table.SetContent(strings.Join(rows, "\n"))
	model.table.GotoBottom()
}

func (model historyTrendBrowser) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch value := msg.(type) {
	case tea.WindowSizeMsg:
		model.width, model.height = value.Width, value.Height
		model.resize()
		return model, nil
	case historyTrendLoadedMsg:
		if value.seq != model.seq {
			return model, nil
		}
		model.loaded, model.loading = value, false
		model.refreshTable()
		return model, nil
	case tea.KeyPressMsg:
		switch value.String() {
		case "q", "esc", "ctrl+c":
			return model, tea.Quit
		case "up", "k":
			if model.cursor > 0 {
				model.cursor--
				return model, model.reload()
			}
		case "down", "j":
			if model.cursor < len(model.targets)-1 {
				model.cursor++
				return model, model.reload()
			}
		case "m", "M":
			names := model.metricNames()
			if len(names) < 2 {
				return model, nil
			}
			current := 0
			for index, name := range names {
				if name == model.metric() {
					current = index
				}
			}
			step := 1
			if value.String() == "M" {
				step = len(names) - 1
			}
			model.metrics[model.metricKey()] = names[(current+step)%len(names)]
			return model, model.reload()
		case "b":
			for index, period := range historyTrendPeriods {
				if period == model.options.by {
					model.options.by = historyTrendPeriods[(index+1)%len(historyTrendPeriods)]
					break
				}
			}
			return model, model.reload()
		case "e":
			if model.target().kind == historyTrendProcess {
				model.options.each = !model.options.each
				return model, model.reload()
			}
		case "pgup", "pgdown", "home", "end":
			var cmd tea.Cmd
			model.table, cmd = model.table.Update(msg)
			return model, cmd
		}
	}
	return model, nil
}

func (model historyTrendBrowser) targetLabel(index, width int) string {
	target := model.targets[index]
	switch target.kind {
	case historyTrendHost:
		return T("history.browser.host")
	case historyTrendProcess:
		name := topProcessName(target.value, width)
		var duplicates []string
		for _, candidate := range model.targets {
			if candidate.kind == historyTrendProcess && topProcessName(candidate.value, width) == name {
				duplicates = append(duplicates, candidate.value)
			}
		}
		if len(duplicates) > 1 {
			return ansi.Truncate(name+" · "+historyTrendProcessParent(target.value, duplicates), width, "…")
		}
		return name
	default:
		return "◆ " + historyTerminalText(target.value)
	}
}

func historyTrendProcessParent(command string, commands []string) string {
	parts := strings.Split(strings.Trim(strings.TrimSpace(command), "/"), "/")
	for depth := 1; depth < len(parts); depth++ {
		suffix := strings.Join(parts[max(0, len(parts)-depth-1):len(parts)-1], "/")
		unique := true
		for _, other := range commands {
			if other == command {
				continue
			}
			otherParts := strings.Split(strings.Trim(strings.TrimSpace(other), "/"), "/")
			if depth >= len(otherParts) || strings.Join(otherParts[max(0, len(otherParts)-depth-1):len(otherParts)-1], "/") == suffix {
				unique = false
				break
			}
		}
		if unique {
			return historyTerminalText(suffix)
		}
	}
	return historyTerminalText(command)
}

func (model historyTrendBrowser) targetLines(height int) []string {
	start := max(0, min(model.cursor-height/2, len(model.targets)-height))
	var lines []string
	for index := start; index < min(len(model.targets), start+height); index++ {
		target := model.targets[index]
		count := ""
		if target.rss > 0 {
			count = formatSizeShort(uint64(target.rss))
		}
		space := historyTrendTargetWidth - 1 - ansi.StringWidth(count)
		label := ansi.Truncate(model.targetLabel(index, max(1, space-1)), max(1, space-1), "…")
		line := " " + label + strings.Repeat(" ", max(1, space-ansi.StringWidth(label))) + historyTint(count, richGray, model.color)
		if index == model.cursor {
			plain := " " + label + strings.Repeat(" ", max(1, space-ansi.StringWidth(label))) + count
			line = liveSelected(plain, model.color)
			if !model.color {
				line = "›" + plain[1:]
			}
		}
		lines = append(lines, line)
	}
	return lines
}

func (model historyTrendBrowser) targetHeader() string {
	value := T("history.browser.peak_memory")
	space := historyTrendTargetWidth - 1 - ansi.StringWidth(value)
	label := ansi.Truncate(T("history.browser.targets"), max(1, space-1), "…")
	return " " + label + strings.Repeat(" ", max(1, space-ansi.StringWidth(label))) + value
}

func (model historyTrendBrowser) title() string {
	options := model.trendOptions()
	_, offset := time.Now().In(model.location).Zone()
	zone := fmt.Sprintf("%+03d:%02d", offset/3600, abs(offset%3600)/60)
	switch {
	case options.process.active():
		return T("history.trend.process_title", options.metric, historyTerminalText(options.process.String()), options.by, zone)
	case options.probe != "":
		return T("history.trend.title", historyTerminalText(options.probe)+" "+historyTerminalText(options.metric), options.by, zone)
	default:
		return T("history.trend.title", options.metric, options.by, zone)
	}
}

// growthLine은 고른 프로세스의 RSS 변화 요약이다. 누수 의심이 있으면 그 수와 가장 빨리 느는 프로세스를 보인다.
func (model historyTrendBrowser) growthLine() string {
	if model.target().kind != historyTrendProcess || len(model.loaded.growth) == 0 {
		return ""
	}
	top := model.loaded.growth[0]
	if top.BytesPerDay == nil {
		return historyTint(T("history.browser.growth_none", historyGrowthProcessNote(top)), richGray, model.color)
	}
	suspects := 0
	for _, process := range model.loaded.growth {
		if process.LeakSuspect {
			suspects++
		}
	}
	text := T("history.browser.growth", historyGrowthRate(*top.BytesPerDay), top.PID)
	if suspects > 0 {
		return historyStrong(text+" · "+T("history.browser.leak", suspects, len(model.loaded.growth), *top.Fit), richYellow, model.color)
	}
	return historyTint(text, richGray, model.color)
}

func (model historyTrendBrowser) View() tea.View {
	var lines []string
	if model.width < 60 || model.height < 10 {
		lines = []string{T("cli.log_history.small"), T("cli.log_history.quit_help")}
	} else {
		state := ""
		if model.loading {
			state = " · " + T("history.browser.loading")
		}
		lines = append(lines, historyStrong(model.title(), richMagenta, model.color)+historyTint(state, richGray, model.color), "")
		header, rows := model.tableRows()
		right := []string{historyTint(header, richGray, model.color)}
		switch {
		case model.loaded.err != nil:
			right = append(right, historyTint(model.loaded.err.Error(), richRed, model.color))
		case len(rows) == 0 && model.target().kind == historyTrendProcess:
			right = append(right, T("history.trend.process_empty", historyTerminalText(model.target().value), quoteLocalShellWord(model.target().value)))
		case len(rows) == 0:
			right = append(right, T("history.trend.empty"))
		default:
			right = append(right, strings.Split(strings.TrimRight(model.table.View(), "\n"), "\n")...)
		}
		height := model.table.Height() + 1
		left := append([]string{historyTint(model.targetHeader(), richGray, model.color)}, model.targetLines(height-1)...)
		for index := 0; index < height; index++ {
			l, r := "", ""
			if index < len(left) {
				l = left[index]
			}
			if index < len(right) {
				r = right[index]
			}
			pad := strings.Repeat(" ", max(0, historyTrendTargetWidth-ansi.StringWidth(l)))
			lines = append(lines, l+pad+historyTint(" │ ", richGray, model.color)+r)
		}
		lines = append(lines, model.growthLine())
		notice := ""
		if model.loaded.summary.Unfiltered {
			value := model.target().value
			notice = historyTint(T("history.trend.unfiltered", historyTerminalText(value), quoteLocalShellWord(value)), richYellow, model.color)
		}
		lines = append(lines, notice)
		var on []string
		if model.trendOptions().each {
			on = append(on, "e")
		}
		lines = append(lines, liveKeyHints(T("history.browser.help"), model.color, on...))
	}
	for index, line := range lines {
		lines[index] = ansi.Truncate(line, max(1, model.width), "…")
	}
	if len(lines) > model.height {
		lines = lines[:max(1, model.height)]
	}
	view := tea.NewView(strings.Join(lines, "\n"))
	view.AltScreen = true
	return view
}

// historyTrendBrowserCommand는 마지막으로 본 표를 내는 명령이다. 화면에서 바꾼 option은 원래 인자에서 빼고 앞에 넣는다.
func historyTrendBrowserCommand(options historyTrendOptions, args []string) string {
	var chosen []string
	if options.process.active() {
		chosen = append(chosen, "--process", options.process.String())
	}
	if options.probe != "" {
		chosen = append(chosen, "--probe", options.probe)
	}
	chosen = append(chosen, "--metric", options.metric, "--by", options.by)
	if options.each {
		chosen = append(chosen, "--each")
	}
	return historyEchoCommand("trend", chosen, historyStripFlags(args, map[string]bool{"process": true, "probe": true, "metric": true, "by": true}, map[string]bool{"each": true}))
}

// historyStripFlags는 args에서 이름이 든 option을 값과 함께 뺀다. -- 뒤와 위치 인자는 그대로 둔다.
func historyStripFlags(args []string, valued, boolean map[string]bool) []string {
	var kept []string
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			return append(kept, args[index:]...)
		}
		name, _, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if !strings.HasPrefix(arg, "-") || (!valued[name] && !boolean[name]) {
			kept = append(kept, arg)
			continue
		}
		if valued[name] && !hasValue {
			index++
		}
	}
	return kept
}

func runHistoryTrendBrowser(input *os.File, db *sql.DB, options historyTrendOptions, args []string) (bool, error) {
	targets, err := historyTrendTargets(db)
	if err != nil || len(targets) == 0 && !options.process.active() && options.probe == "" {
		return false, err
	}
	final, err := tea.NewProgram(newHistoryTrendBrowser(db, targets, options, time.Local), tea.WithInput(input), tea.WithOutput(os.Stdout)).Run()
	if err != nil {
		return true, err
	}
	if model, ok := final.(historyTrendBrowser); ok && model.metric() != "" {
		promptEcho(os.Stdout, historyTrendBrowserCommand(model.trendOptions(), args))
	}
	return true, nil
}
