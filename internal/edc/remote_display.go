package edc

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
)

type remoteDisplayOptions struct {
	verbose bool
	live    bool // 실시간 매트릭스 화면을 쓴다
	confirm bool // 실행 전에 화면에서 확인을 받는다
	results bool
	redact  bool
	plan    remotePlanView
	cancel  context.CancelFunc
}

type remoteDisplay struct {
	mu       sync.Mutex
	output   io.Writer
	verbose  bool
	color    bool
	results  bool
	redact   bool
	live     *liveProgram
	answered chan bool // 화면이 확인 답을 보내는 통로
}

func newRemoteDisplay(output io.Writer, options remoteDisplayOptions) *remoteDisplay {
	display := &remoteDisplay{
		output: output, verbose: options.verbose,
		color: options.live, results: options.results, redact: options.redact,
	}
	if !options.live {
		return display
	}
	model := newRemoteModel(options.plan, options.confirm, true, options.verbose, options.cancel)
	live, err := startLiveProgram(model, options.cancel, tea.WithInput(os.Stdin), tea.WithOutput(output))
	if err != nil {
		// TTY를 열지 못하면 결과 줄만 흘려 보내는 기존 출력으로 내려간다.
		fmt.Fprintln(os.Stderr, T("remote.error.live_start_failed", err))
		return display
	}
	display.live = live
	if options.confirm {
		// 확인을 받을 때만 답을 기다린다. 채널을 항상 달아 두면 -f 실행이 답을 기다리다 멈춘다.
		display.answered = model.answered
	}
	return display
}

// awaitConfirm은 화면이 확인 답을 보낼 때까지 기다린다. 화면이 없거나 확인을 받지 않으면 바로 실행한다.
func (display *remoteDisplay) awaitConfirm() bool {
	if display == nil || display.live == nil || display.answered == nil {
		return true
	}
	select {
	case answer := <-display.answered:
		return answer
	case <-display.live.done:
		return false
	}
}

// Result는 step이 끝날 때마다 상태를 전달한다. 실시간 화면이 없으면 PASS/FAIL 줄을 바로 출력한다.
func (display *remoteDisplay) Result(result Result) {
	if display == nil || !display.results {
		return
	}
	if display.live != nil {
		host, _ := result.Metrics["host"].(string)
		step, _ := result.Metrics["step"].(string)
		display.live.send(remoteResultMsg{host: host, step: step, status: result.Status})
		return
	}
	line := formatResultLine(result, display.color)
	if display.redact {
		line = redactIPAddresses(line)
	}
	display.mu.Lock()
	defer display.mu.Unlock()
	fmt.Fprint(display.output, line)
}

func (display *remoteDisplay) Start(host, step, phase string) {
	if display == nil {
		return
	}
	if display.live != nil {
		display.live.send(remoteStartMsg{host: host, step: step, phase: phase})
		return
	}
	if !display.verbose {
		return
	}
	display.mu.Lock()
	defer display.mu.Unlock()
	fmt.Fprintf(display.output, "\n%s\n", display.phaseLabel(host, step, phase))
}

func (display *remoteDisplay) WriteLine(prefix, line string) {
	if display == nil {
		return
	}
	if display.live != nil {
		display.live.send(remoteLogMsg{prefix: prefix, line: line})
		return
	}
	display.mu.Lock()
	defer display.mu.Unlock()
	if display.color {
		fmt.Fprintf(display.output, "\033[90m  %-30s │\033[0m %s\n", prefix, line)
	} else {
		fmt.Fprintf(display.output, "  %-30s | %s\n", prefix, line)
	}
}

func (display *remoteDisplay) phaseLabel(host, step, phase string) string {
	label := fmt.Sprintf("› %s  %s  %s", host, step, phase)
	if display.color {
		return "\033[90m" + label + "\033[0m"
	}
	return label
}

func (display *remoteDisplay) Close() {
	if display == nil || display.live == nil {
		return
	}
	if _, err := display.live.finish(remoteDoneMsg{}); err != nil {
		fmt.Fprintln(os.Stderr, T("remote.error.live_end_failed", err))
	}
	display.live = nil
}

type remoteStreamWriter struct {
	mu      sync.Mutex
	display *remoteDisplay
	prefix  string
	pending string
}

func (writer *remoteStreamWriter) Write(data []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	writer.pending += string(data)
	for {
		index := strings.IndexByte(writer.pending, '\n')
		if index < 0 {
			break
		}
		writer.display.WriteLine(writer.prefix, writer.pending[:index])
		writer.pending = writer.pending[index+1:]
	}
	return len(data), nil
}

func (writer *remoteStreamWriter) Flush() {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.pending != "" {
		writer.display.WriteLine(writer.prefix, writer.pending)
		writer.pending = ""
	}
}
