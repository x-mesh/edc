package edc

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

type cleanProgressTick struct{ snapshot cleanProgressSnapshot }
type cleanProgressDone struct {
	snapshot cleanProgressSnapshot
	result   Result
}

type cleanProgressModel struct {
	snapshot                      cleanProgressSnapshot
	read                          func() cleanProgressSnapshot
	cancel                        context.CancelFunc
	workers, width, height        int
	color, help, cancelling, done bool
	// abandoned는 중지 후 끝나지 않는 확인을 기다리지 않고 나가기로 했다는 뜻이다. 커널 안에서 막힌 open은 취소로 풀리지 않는다.
	abandoned bool
	result    Result
}

func newCleanProgressModel(progress *cleanProgress, workers int, cancel context.CancelFunc) cleanProgressModel {
	return cleanProgressModel{snapshot: progress.snapshot(), read: progress.snapshot, cancel: cancel, workers: workers, width: 80, height: 24, color: true}
}

func (model cleanProgressModel) tick() tea.Cmd {
	return tea.Tick(200*time.Millisecond, func(time.Time) tea.Msg { return cleanProgressTick{model.read()} })
}

func (model cleanProgressModel) Init() tea.Cmd { return model.tick() }

func (model cleanProgressModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case cleanProgressTick:
		if !model.done {
			model.snapshot = msg.snapshot
			return model, model.tick()
		}
	case cleanProgressDone:
		model.done, model.snapshot, model.result = true, msg.snapshot, msg.result
		return model, tea.Quit
	case tea.WindowSizeMsg:
		model.width, model.height = max(1, msg.Width), max(1, msg.Height)
	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			switch {
			case model.done:
			case model.cancelling:
				model.abandoned = true
				return model, tea.Quit
			default:
				model.cancelling = true
				if model.cancel != nil {
					model.cancel()
				}
			}
		case "ctrl+z":
			return model, tea.Suspend
		case "?":
			model.help = !model.help
		}
	}
	return model, nil
}

func (model cleanProgressModel) paint(text, color string, bold bool) string {
	if !model.color {
		return text
	}
	return richPaint(text, color, bold)
}

func cleanProgressElapsed(start, end time.Time) string {
	elapsed := end.Sub(start)
	if elapsed < 0 {
		elapsed = 0
	}
	return fmt.Sprintf("%.1fs", elapsed.Seconds())
}

func (model cleanProgressModel) View() tea.View {
	width, height := min(100, max(1, model.width)), max(1, model.height)
	status := T("clean.ui.running")
	if model.cancelling {
		status = T("clean.ui.stopping")
	}
	if model.done {
		status = T("clean.ui.finished")
		if model.result.Status == StatusWarn {
			status = T("clean.ui.partial")
		}
		if model.result.Status == StatusFail {
			status = T("clean.ui.failed")
		}
		if model.cancelling {
			status = T("clean.ui.stopped")
		}
	}
	if model.done && model.result.Status == StatusWarn {
		status = model.paint(status, richYellow, true)
	}
	if model.done && model.result.Status == StatusFail {
		status = model.paint(status, richRed, true)
	}
	elapsed := cleanProgressElapsed(model.snapshot.started, model.snapshot.at)
	header := model.paint("edc clean", richMagenta, true) + "  " + model.paint(T("clean.ui.readonly"), richGray, false)
	header += "  ·  " + status + "  " + elapsed
	footer := T("clean.ui.keys")
	if model.help {
		footer = T("clean.ui.help")
	}
	if model.cancelling {
		footer = T("clean.ui.stopping_hint")
	}
	if model.done {
		footer = T("clean.ui.finished_hint")
	}
	advisory := T("clean.advisory")
	if ansi.StringWidth(advisory) > width {
		advisory = T("clean.ui.advisory_short")
	}
	if width < 40 || height < 5 {
		rows := []string{"edc clean  " + elapsed, advisory, T("clean.ui.small"), footer}
		rows = rows[:min(height, len(rows))]
		for i := range rows {
			rows[i] = ansi.Truncate(rows[i], width, "…")
		}
		return liveFrame(strings.Join(rows, "\n"), len(rows))
	}
	lines := []string{header, advisory, model.paint(T("clean.ui.workers", model.workers, min(4, model.workers)), richGray, false), ""}
	compact := width < 60 || height < 15
	stageRows := 0
	for _, stage := range cleanProgressStages {
		task, started := model.snapshot.stages[stage]
		if stage == "newest" && !started {
			continue
		}
		mark, state, duration := "·", T("clean.ui.waiting"), "—"
		if started {
			mark, state = "›", T("clean.ui.running")
			end := model.snapshot.at
			if !task.finished.IsZero() {
				mark, state, end = "─", T("clean.ui.finished"), task.finished
			}
			duration = cleanProgressElapsed(task.started, end)
			if task.skipped {
				mark, state, duration = "·", T("clean.ui.skipped"), "—"
			}
		}
		if compact && (!started || !task.finished.IsZero()) {
			continue
		}
		label := T("clean.progress.stage." + stage)
		left := mark + " " + label
		right := duration + "  " + state
		left = ansi.Truncate(left, max(1, width-ansi.StringWidth(right)-3), "…")
		gap := max(1, width-ansi.StringWidth(left)-ansi.StringWidth(right)-2)
		line := "  " + left + strings.Repeat(" ", gap) + right
		if started && task.finished.IsZero() {
			line = model.paint(line, richCyan, true)
		} else if !started {
			line = model.paint(line, richGray, false)
		}
		lines = append(lines, line)
		stageRows++
	}
	if compact && stageRows == 0 {
		lines = append(lines, "")
	}
	current := T("clean.ui.current")
	if model.done {
		current = ""
	}
	lines = append(lines, "", model.paint(current, richGray, true))
	slots := min(3, max(0, height-len(lines)-2))
	active := model.snapshot.active
	for index := 0; index < slots; index++ {
		line := ""
		if index < len(active) {
			task := active[len(active)-1-index]
			label := T("clean.progress.stage."+task.stage) + "  " + cleanProgressElapsed(task.started, model.snapshot.at)
			line = "  " + model.paint(label, richGray, false) + "  " + task.target
		}
		lines = append(lines, line)
	}
	if len(active) > slots && slots > 0 {
		lines[len(lines)-1] = model.paint(T("clean.ui.more", len(active)-slots+1), richGray, false)
	}
	lines = append(lines, "", model.paint(footer, richGray, false))
	if len(lines) > height {
		lines = append(lines[:max(0, height-1)], footer)
	}
	for i, line := range lines {
		lines[i] = ansi.Truncate(line, width, "…")
	}
	return liveFrame(strings.Join(lines, "\n"), min(height, len(lines)))
}

func cleanProgressTerminal(terminal bool, jsonPath string) bool {
	return terminal && jsonPath == "" && liveTerminal() && isTerminal(os.Stderr) && os.Getenv("TERM") != "dumb"
}

func runCleanProgressUI(scan func() Result, progress *cleanProgress, workers int, cancel context.CancelFunc) Result {
	finished := make(chan Result, 1)
	go func() { finished <- scan() }()
	select {
	case result := <-finished:
		return result
	case <-time.After(probeLiveDelay):
	}
	model := newCleanProgressModel(progress, workers, cancel)
	live, err := startLiveProgram(model, cancel, tea.WithInput(os.Stdin), tea.WithOutput(os.Stderr))
	if err != nil {
		progress.mutex.Lock()
		progress.writer = os.Stderr
		progress.mutex.Unlock()
		fmt.Fprintln(os.Stderr, T("observe.live.start_failed", err))
		return <-finished
	}
	var result Result
	select {
	case result = <-finished:
	case <-live.done:
		if model, _ := live.model.(cleanProgressModel); model.abandoned {
			return cleanAbandonedResult(progress.snapshot())
		}
		result = <-finished
	}
	if _, err := live.finish(cleanProgressDone{snapshot: progress.snapshot(), result: result}); err != nil {
		fmt.Fprintln(os.Stderr, T("observe.live.exit_error", err))
	}
	return result
}

// cleanAbandonedResult는 끝나지 않은 확인을 남긴다. 어느 경로에서 막혔는지 알아야 다음 실행에서 피할 수 있다.
func cleanAbandonedResult(snapshot cleanProgressSnapshot) Result {
	result := Result{Probe: cleanProbeID, Status: StatusWarn, StartedAt: snapshot.started.UTC(), Summary: T("clean.ui.abandoned")}
	for _, task := range snapshot.active {
		result.Warnings = append(result.Warnings, T("clean.ui.abandoned_task", T("clean.progress.stage."+task.stage), task.target))
	}
	return result
}
