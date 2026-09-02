package edc

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// liveTerminal은 실시간 화면을 쓸 조건이다. PRODUCT.md에 따라 TTY가 아니거나 NO_COLOR면 motion을 쓰지 않는다.
func liveTerminal() bool {
	return isTerminal(os.Stdin) && isTerminal(os.Stdout) && os.Getenv("NO_COLOR") == ""
}

// liveReadyMsg는 event loop가 떴는지 확인하는 handshake다. filter가 삼키므로 model에는 닿지 않는다.
type liveReadyMsg struct{}

const (
	// liveModeQueries는 bubbletea가 시작할 때 보내는 terminal 질의 수다(동기 출력, unicode core).
	liveModeQueries = 2
	// liveSettleTimeout은 질의에 답하지 않는 terminal에서 기다리는 상한이다.
	liveSettleTimeout = 250 * time.Millisecond
)

// liveProgram은 goroutine에서 도는 bubbletea Program이다.
// Program이 사는 동안 stdout과 stderr에 직접 쓰면 화면이 깨지므로 finish 뒤에 출력한다.
type liveProgram struct {
	program *tea.Program
	done    chan struct{}
	settled chan struct{} // terminal 질의 응답을 다 읽었다
	model   tea.Model
	err     error
}

// startLiveProgram은 Program을 시작하고 event loop가 첫 메시지를 처리할 때까지 기다린다.
// TTY를 열지 못해 Run이 먼저 끝나면 error를 돌려주고, caller는 plain 출력으로 내려간다.
// onExit는 Run이 어떤 이유로든 끝날 때 호출된다. 취소가 필요한 caller는 여기서 context를 cancel한다.
func startLiveProgram(model tea.Model, onExit func(), options ...tea.ProgramOption) (*liveProgram, error) {
	ready := make(chan struct{})
	var readyOnce, settledOnce sync.Once
	live := &liveProgram{done: make(chan struct{}), settled: make(chan struct{})}
	reports := 0
	filter := tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg {
		switch msg.(type) {
		case liveReadyMsg:
			readyOnce.Do(func() { close(ready) })
			return nil
		case tea.ModeReportMsg:
			// 시작할 때 보낸 질의의 응답이다. 다 읽기 전에 종료하면 남은 byte가 shell로 새어 나온다.
			if reports++; reports >= liveModeQueries {
				settledOnce.Do(func() { close(live.settled) })
			}
		}
		return msg
	})
	live.program = tea.NewProgram(model, append([]tea.ProgramOption{filter}, options...)...)
	go func() {
		defer close(live.done)
		live.model, live.err = live.program.Run()
		if onExit != nil {
			onExit()
		}
	}()
	go live.program.Send(liveReadyMsg{})
	select {
	case <-ready:
		return live, nil
	case <-live.done:
		if live.err != nil {
			return nil, live.err
		}
		return nil, fmt.Errorf("%s", T("observe.live.exited_immediately"))
	}
}

func (live *liveProgram) send(msg tea.Msg) {
	if live == nil {
		return
	}
	live.program.Send(msg)
}

// finish는 마지막 메시지를 보내고 Run이 반환할 때까지 기다린다.
func (live *liveProgram) finish(msg tea.Msg) (tea.Model, error) {
	if live == nil {
		return nil, nil
	}
	// 질의 응답을 다 읽은 뒤에 끝내야 남은 byte가 shell로 새어 나오지 않는다.
	select {
	case <-live.settled:
	case <-live.done:
	case <-time.After(liveSettleTimeout):
	}
	live.program.Send(msg)
	<-live.done
	return live.model, live.err
}

// liveSpinner는 기존 remote 상태줄과 같은 frame을 쓴다.
func liveSpinner() spinner.Model {
	return spinner.New(spinner.WithSpinner(spinner.Spinner{Frames: liveSpinnerFrames, FPS: liveSpinnerFPS}))
}

var liveSpinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const (
	liveSpinnerFPS = 100 * time.Millisecond
	// liveElapsedPrecision은 경과 시간 표시가 매 프레임 흔들리지 않게 한다.
	liveElapsedPrecision = 100 * time.Millisecond
)

const (
	// liveSelectedBar는 고른 줄 왼쪽에 두는 막대다. 색을 못 써도 어느 줄인지 알 수 있다.
	liveSelectedBar = "▌"
	liveIdleBar     = " "
	// liveReverse는 고른 칸의 글자와 배경을 뒤집는다. 색 지각과 무관하게 대비가 크다.
	liveReverse = "\033[7m"
	liveDim     = "\033[2m"
	liveReset   = "\033[0m"
)

// liveSelected는 고른 항목을 반전으로 칠한다.
func liveSelected(text string, color bool) string {
	if !color {
		return text
	}
	return liveReverse + text + liveReset
}

// liveMuted는 고르지 않은 항목과 키 안내를 흐리게 만든다.
func liveMuted(text string, color bool) string {
	if !color {
		return text
	}
	return liveDim + text + liveReset
}

// liveCell은 ANSI escape를 포함한 문자열을 표시 폭 기준으로 채운다.
func liveCell(value string, width int) string {
	return lipgloss.NewStyle().Width(width).Render(value)
}

func liveWidth(value string) int {
	return lipgloss.Width(value)
}

// liveFrame은 화면 줄 수를 유지한 View를 만든다.
//
// bubbletea v2.0.9의 inline renderer는 두 가지 제약이 있다.
// View가 줄어들면 줄어든 만큼 이전 화면이 위쪽에 남고, content가 줄바꿈으로 끝나지 않으면 마지막 줄이 지워진다.
// 그래서 model은 가장 컸던 높이를 알려 주고, 여기서 빈 줄로 채우며 줄바꿈으로 끝맺는다.
func liveFrame(content string, height int) tea.View {
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	for liveLineCount(content) < height {
		content += "\n"
	}
	return tea.NewView(content)
}

func liveLineCount(content string) int {
	return strings.Count(content, "\n")
}
