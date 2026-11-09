package edc

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"unicode/utf8"

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
	confirm  bool
	color    bool
	results  bool
	redact   bool
	live     *liveProgram
	answered chan bool // 화면이 확인 답을 보내는 통로
}

func newRemoteDisplay(output io.Writer, options remoteDisplayOptions) *remoteDisplay {
	return newRemoteDisplayWithStarter(output, options, startLiveProgram)
}

func newRemoteDisplayWithStarter(output io.Writer, options remoteDisplayOptions, start func(tea.Model, func(), ...tea.ProgramOption) (*liveProgram, error)) *remoteDisplay {
	display := &remoteDisplay{
		output: output, verbose: options.verbose, confirm: options.confirm,
		color: options.live, results: options.results, redact: options.redact,
	}
	if !options.live {
		return display
	}
	model := newRemoteModel(options.plan, options.confirm, true, options.verbose, options.cancel)
	live, err := start(model, options.cancel, tea.WithInput(os.Stdin), tea.WithOutput(output))
	if err != nil {
		// 확인 없이 실행하는 경우에는 TTY 시작 실패 후 기존 줄 출력을 사용한다.
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

func (display *remoteDisplay) awaitConfirm() bool {
	if display == nil || !display.confirm {
		return true
	}
	if display.live == nil || display.answered == nil {
		return false
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
	display.writeStreamLine("", "", prefix, line)
}

// writeStreamLine은 원격 출력 한 줄을 전달한다. 실시간 화면은 -v가 아니어도 칸마다 마지막 줄을 보여 줘야
// 멈춘 step이 어디서 걸렸는지 알 수 있으므로 host와 step을 함께 보낸다.
func (display *remoteDisplay) writeStreamLine(host, step, prefix, line string) {
	if display == nil {
		return
	}
	if display.live != nil {
		if display.redact {
			line = redactIPAddresses(line)
		}
		display.live.send(remoteLogMsg{host: host, step: step, prefix: prefix, line: line})
		return
	}
	display.mu.Lock()
	defer display.mu.Unlock()
	if display.color {
		fmt.Fprintf(display.output, "%s %s\n", richDim(fmt.Sprintf("  %-30s │", prefix)), line)
	} else {
		fmt.Fprintf(display.output, "  %-30s | %s\n", prefix, line)
	}
}

func (display *remoteDisplay) phaseLabel(host, step, phase string) string {
	label := fmt.Sprintf("› %s  %s  %s", host, step, phase)
	if display.color {
		return richDim(label)
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

// remoteStreamLineLimit는 줄바꿈을 기다리며 모아 두는 출력의 상한이다. 실시간 화면은 -v가 아니어도 출력을 읽으므로,
// 줄바꿈 없이 이어지는 출력을 끝까지 모으면 메모리가 출력 크기만큼 는다.
const remoteStreamLineLimit = 4096

type remoteStreamWriter struct {
	mu      sync.Mutex
	display *remoteDisplay
	host    string
	step    string
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
		writer.display.writeStreamLine(writer.host, writer.step, writer.prefix, writer.pending[:index])
		writer.pending = writer.pending[index+1:]
	}
	for len(writer.pending) > remoteStreamLineLimit {
		cut := remoteStreamLineLimit
		for cut > 0 && !utf8.RuneStart(writer.pending[cut]) {
			cut--
		}
		if cut == 0 {
			// 글자 경계가 없는 바이트열이면 상한에서 자른다. 0에서 멈추면 이 loop가 끝나지 않는다.
			cut = remoteStreamLineLimit
		}
		writer.display.writeStreamLine(writer.host, writer.step, writer.prefix, writer.pending[:cut])
		writer.pending = writer.pending[cut:]
	}
	return len(data), nil
}

func (writer *remoteStreamWriter) Flush() {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.pending != "" {
		writer.display.writeStreamLine(writer.host, writer.step, writer.prefix, writer.pending)
		writer.pending = ""
	}
}
