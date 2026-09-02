package edc

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
)

// doctorProbe는 실시간 화면이 결과를 기다리는 동안 대기 줄을 그릴 수 있게 probe에 이름을 붙인다.
type doctorProbe struct {
	name string
	run  func(context.Context) Result
}

func doctorProbeFuncs(probes []doctorProbe) []func(context.Context) Result {
	runs := make([]func(context.Context) Result, 0, len(probes))
	for _, probe := range probes {
		runs = append(runs, probe.run)
	}
	return runs
}

func doctorProbeNames(probes []doctorProbe) []string {
	names := make([]string, 0, len(probes))
	for _, probe := range probes {
		names = append(names, probe.name)
	}
	sort.Strings(names)
	return names
}

// runDoctorLive는 probe가 끝날 때마다 줄을 갱신하고, 끝나면 상세와 요약을 기존 형식으로 출력한다.
func runDoctorLive(ctx context.Context, cancel context.CancelFunc, probes []doctorProbe, options commonOptions, version string, started time.Time, target string, targetInfo map[string]interface{}) int {
	model := newDoctorModel(target, doctorProbeNames(probes), true, options.redact, cancel)
	live, err := startLiveProgram(model, cancel, tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout))
	if err != nil {
		fmt.Fprintf(os.Stderr, "실시간 화면을 표시하지 못했습니다: %v\n", err)
		results := runParallel(ctx, doctorProbeFuncs(probes))
		return emit(options, buildReport(version, started, targetInfo, results, options.redact))
	}
	results := runParallelWith(ctx, doctorProbeFuncs(probes), func(index int, result Result) {
		live.send(doctorResultMsg{name: probes[index].name, result: result})
	})
	// finish가 Run을 끝내면 onExit이 cancel을 부르므로, 사용자 취소 여부는 그 전에 읽는다.
	cancelled := ctx.Err() != nil
	if _, err := live.finish(doctorDoneMsg{}); err != nil {
		fmt.Fprintf(os.Stderr, "실시간 화면이 오류로 끝났습니다: %v\n", err)
	}
	report := buildReport(version, started, targetInfo, results, options.redact)
	printResultTail(os.Stdout, report.Results, options.verbose, true)
	if cancelled {
		fmt.Fprintln(os.Stderr, errDoctorCancelled)
		return 4
	}
	return exitCode(report.Results)
}

type doctorRow struct {
	name   string
	result Result
	done   bool
}

type doctorModel struct {
	target     string
	rows       []doctorRow
	index      map[string]int
	spinner    spinner.Model
	started    time.Time
	now        func() time.Time
	color      bool
	redact     bool
	finished   bool
	cancelling bool
	cancel     context.CancelFunc
}

type doctorResultMsg struct {
	name   string
	result Result
}

type doctorDoneMsg struct{}

func newDoctorModel(target string, names []string, color, redact bool, cancel context.CancelFunc) doctorModel {
	model := doctorModel{
		target: target, rows: make([]doctorRow, 0, len(names)), index: make(map[string]int, len(names)),
		spinner: liveSpinner(), started: time.Now(), now: time.Now, color: color, redact: redact, cancel: cancel,
	}
	for _, name := range names {
		model.index[name] = len(model.rows)
		model.rows = append(model.rows, doctorRow{name: name})
	}
	return model
}

func (model doctorModel) Init() tea.Cmd { return model.spinner.Tick }

func (model doctorModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch value := msg.(type) {
	case doctorResultMsg:
		index, known := model.index[value.name]
		if !known {
			index = len(model.rows)
			model.index = cloneIndex(model.index)
			model.index[value.name] = index
			model.rows = append(append([]doctorRow{}, model.rows...), doctorRow{name: value.name})
		} else {
			model.rows = append([]doctorRow{}, model.rows...)
		}
		model.rows[index].result = value.result
		model.rows[index].done = true
		return model, nil
	case doctorDoneMsg:
		model.finished = true
		return model, tea.Quit
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
		if model.finished {
			return model, nil
		}
		var cmd tea.Cmd
		model.spinner, cmd = model.spinner.Update(value)
		return model, cmd
	}
	return model, nil
}

func (model doctorModel) View() tea.View {
	var builder strings.Builder
	for _, row := range model.rows {
		if row.done {
			line := formatResultLine(row.result, model.color)
			if model.redact {
				line = redactIPAddresses(line)
			}
			builder.WriteString(line)
			continue
		}
		// spinner glyph는 여러 byte라 폭 기준으로 채워야 결과 줄과 열이 맞는다.
		builder.WriteString(fmt.Sprintf(resultLineFormat, liveCell(model.spinner.View(), resultStatusWidth), row.name, ""))
	}
	if !model.finished {
		builder.WriteString(model.statusLine())
	}
	return liveFrame(builder.String(), len(model.rows)+1)
}

func (model doctorModel) statusLine() string {
	if model.cancelling {
		return "취소 중, 실행 중인 probe를 종료합니다\n"
	}
	return fmt.Sprintf("%s  %s  %d/%d 완료  %s\n", model.spinner.View(), model.target, model.completed(), len(model.rows), model.elapsed())
}

func (model doctorModel) completed() int {
	count := 0
	for _, row := range model.rows {
		if row.done {
			count++
		}
	}
	return count
}

func (model doctorModel) elapsed() string {
	return model.now().Sub(model.started).Round(liveElapsedPrecision).String()
}

func cloneIndex(source map[string]int) map[string]int {
	clone := make(map[string]int, len(source)+1)
	for key, value := range source {
		clone[key] = value
	}
	return clone
}
