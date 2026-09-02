package edc

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
)

const (
	// probeLiveDelay는 이 시간 안에 끝나는 probe에는 실시간 화면을 띄우지 않는다.
	// 즉시 끝나는 probe에 TUI를 띄우면 terminal 질의 응답이 shell로 새어 나온다.
	probeLiveDelay = 300 * time.Millisecond
	// probeLineWidth는 terminal 폭을 모를 때 쓰는 기본 폭이다.
	probeLineWidth = 80
)

// probeModel은 항상 한 줄만 그린다. 줄 수가 변하지 않아 화면에 잔상이 남지 않는다.
type probeModel struct {
	name       string
	target     string
	latest     string
	result     Result
	done       bool
	spinner    spinner.Model
	started    time.Time
	now        func() time.Time
	color      bool
	redact     bool
	width      int
	cancelling bool
	cancel     context.CancelFunc
}

type probeResultMsg struct{ result Result }

type probeLogMsg struct{ line string }

func newProbeModel(name, target, latest string, color, redact bool, cancel context.CancelFunc) probeModel {
	return probeModel{
		name: name, target: target, latest: latest, spinner: liveSpinner(), started: time.Now(),
		now: time.Now, color: color, redact: redact, width: probeLineWidth, cancel: cancel,
	}
}

func (model probeModel) Init() tea.Cmd { return model.spinner.Tick }

func (model probeModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch value := msg.(type) {
	case probeResultMsg:
		model.result = value.result
		model.done = true
		return model, tea.Quit
	case probeLogMsg:
		model.latest = value.line
		return model, nil
	case tea.WindowSizeMsg:
		model.width = value.Width
		return model, nil
	case tea.KeyPressMsg:
		if value.String() == "ctrl+c" {
			if model.cancelling {
				return model, tea.Quit
			}
			model.cancelling = true
			if model.cancel != nil {
				model.cancel()
			}
		}
		return model, nil
	case spinner.TickMsg:
		if model.done {
			return model, nil
		}
		var cmd tea.Cmd
		model.spinner, cmd = model.spinner.Update(value)
		return model, cmd
	}
	return model, nil
}

func (model probeModel) View() tea.View {
	if model.done {
		line := formatResultLine(model.result, model.color)
		if model.redact {
			line = redactIPAddresses(line)
		}
		return liveFrame(line, 1)
	}
	line := fmt.Sprintf(resultLineFormat, liveCell(model.spinner.View(), resultStatusWidth), model.name, model.detail())
	return liveFrame(truncateLine(line, model.width), 1)
}

func (model probeModel) detail() string {
	if model.cancelling {
		return "취소 중, 실행 중인 command를 종료합니다"
	}
	detail := model.elapsed()
	if model.target != "" {
		detail = model.target + "  " + detail
	}
	if model.latest != "" {
		detail += "  ·  " + model.redactLine(model.latest)
	}
	return detail
}

func (model probeModel) redactLine(line string) string {
	if model.redact {
		return redactIPAddresses(line)
	}
	return line
}

func (model probeModel) elapsed() string {
	return model.now().Sub(model.started).Round(liveElapsedPrecision).String()
}

// truncateLine은 줄이 접혀 화면 높이가 늘어나지 않게 자른다.
func truncateLine(line string, width int) string {
	if width <= 0 {
		return line
	}
	trimmed := strings.TrimRight(line, "\n")
	if liveWidth(trimmed) <= width {
		return line
	}
	runes := []rune(trimmed)
	for len(runes) > 0 && liveWidth(string(runes))+1 > width {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + "…\n"
}

// probeProgress는 command 출력의 마지막 줄을 들고 있다가, 화면이 뜨면 이어서 전달한다.
type probeProgress struct {
	mu     sync.Mutex
	latest string
	live   *liveProgram
}

func (progress *probeProgress) observe(line string) {
	progress.mu.Lock()
	progress.latest = line
	live := progress.live
	progress.mu.Unlock()
	live.send(probeLogMsg{line: line})
}

// attach는 화면을 붙이고 그때까지 모인 마지막 줄을 돌려준다.
func (progress *probeProgress) attach(live *liveProgram) string {
	progress.mu.Lock()
	defer progress.mu.Unlock()
	progress.live = live
	return progress.latest
}

func (progress *probeProgress) snapshot() string {
	progress.mu.Lock()
	defer progress.mu.Unlock()
	return progress.latest
}

// runProbeLive는 probe 하나를 한 줄로 보여 준다. probeLiveDelay 안에 끝나면 화면 없이 결과만 출력한다.
func runProbeLive(ctx context.Context, cancel context.CancelFunc, probeID, target string, options commonOptions, version string, started time.Time, targetInfo map[string]interface{}, probe func(context.Context) Result) int {
	progress := &probeProgress{}
	finished := make(chan Result, 1)
	go func() { finished <- probe(withProbeObserver(ctx, progress.observe)) }()
	select {
	case result := <-finished:
		return emit(options, buildReport(version, started, targetInfo, []Result{result}, options.redact))
	case <-time.After(probeLiveDelay):
	}
	model := newProbeModel(probeID, target, progress.snapshot(), true, options.redact, cancel)
	live, err := startLiveProgram(model, cancel, tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout))
	if err != nil {
		fmt.Fprintf(os.Stderr, "실시간 화면을 표시하지 못했습니다: %v\n", err)
		return emit(options, buildReport(version, started, targetInfo, []Result{<-finished}, options.redact))
	}
	if latest := progress.attach(live); latest != "" {
		live.send(probeLogMsg{line: latest})
	}
	result := <-finished
	// finish가 Run을 끝내면 onExit이 cancel을 부르므로, 사용자 취소 여부는 그 전에 읽는다.
	cancelled := ctx.Err() != nil
	if _, err := live.finish(probeResultMsg{result: result}); err != nil {
		fmt.Fprintf(os.Stderr, "실시간 화면이 오류로 끝났습니다: %v\n", err)
	}
	report := buildReport(version, started, targetInfo, []Result{result}, options.redact)
	printResultTail(os.Stdout, report.Results, options.verbose, true)
	if cancelled {
		fmt.Fprintln(os.Stderr, errProbeCancelled)
		return 4
	}
	return exitCode(report.Results)
}
