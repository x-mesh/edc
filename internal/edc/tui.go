package edc

import (
	"fmt"
	"os"
	"regexp"
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
	return spinner.New(spinner.WithSpinner(spinner.Spinner{Frames: liveSpinnerFrames, FPS: liveSpinnerFPS}),
		spinner.WithStyle(lipgloss.NewStyle().Foreground(lipgloss.Color(richHex(richMagenta)))))
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
	// liveReverse와 liveDim은 NO_COLOR에서 뜨는 선택기와 확인 화면의 흑백 표시다.
	liveReverse = "\033[7m"
	liveDim     = "\033[2m"
	liveReset   = "\033[0m"
)

// liveRich는 팔레트 색을 써도 되는지다. 선택기와 확인 화면은 NO_COLOR에서도 color를 켠 채로 뜨므로, 그때는 색 없이
// 반전과 흐림만 쓰던 예전 글자를 그대로 낸다.
func liveRich(color bool) bool {
	return color && os.Getenv("NO_COLOR") == ""
}

// liveSelected는 고른 항목을 chip 바탕에 굵은 글씨로 칠한다. 맨 앞의 막대는 자홍으로 칠한다.
// 칸마다 색을 입힌 줄은 칸 끝의 reset이 바탕까지 지우므로, reset 뒤마다 바탕을 다시 건다.
func liveSelected(text string, color bool) string {
	if !color {
		return text
	}
	if !liveRich(color) {
		return liveReverse + text + liveReset
	}
	base := "\033[1;" + richRGBCode(richChipBackground, true, richTruecolor()) + "m"
	bar := ""
	if rest, ok := strings.CutPrefix(text, liveSelectedBar); ok {
		// 39는 글자색만 기본으로 돌린다. reset을 쓰면 막대 뒤에서 바탕이 끊긴다.
		bar = "\033[" + richFixedColor(richMagenta, richTruecolor()) + "m" + liveSelectedBar + "\033[39m"
		text = rest
	}
	return base + bar + strings.ReplaceAll(text, richReset, richReset+base) + richReset
}

// liveMuted는 고르지 않은 항목과 키 안내를 회색으로 칠한다. 안쪽에 색을 입힌 조각이 있어도 그 뒤를 다시 회색으로 둔다.
func liveMuted(text string, color bool) string {
	if !color || text == "" {
		return text
	}
	if !liveRich(color) {
		return liveDim + text + liveReset
	}
	gray := strings.TrimSuffix(richDim("x"), "x"+richReset)
	return gray + strings.ReplaceAll(text, richReset, richReset+gray) + richReset
}

// liveHintSeparator는 키 안내의 묶음 사이 구분자다. 언어팩은 " · ", 공백 세 칸, ", " 중 하나로 묶음을 나눈다.
var liveHintSeparator = regexp.MustCompile(`\s*·\s*|\s{3,}|,\s+`)

// liveHintKeys는 키 안내에서 키로 읽는 낱말이다. 한 글자 키는 liveHintKey가 따로 받는다.
var liveHintKeys = map[string]bool{
	"↑/↓": true, "↑↓": true, "←/→": true, "Enter": true, "enter": true, "Esc": true, "esc": true, "Tab": true,
	"Esc/b": true, "End": true, "Home": true, "PgUp/PgDn": true, "space": true, "Space": true, "y/n": true, "Backspace": true, "Ctrl-C": true,
	"←": true, "→": true,
}

func liveHintKey(token string) bool {
	if liveHintKeys[token] {
		return true
	}
	if len(token) != 1 {
		return false
	}
	character := token[0]
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || strings.ContainsRune("/+-", rune(character))
}

// liveKeyHints는 "키 설명" 묶음의 키를 굵은 청록으로, 설명과 구분자를 회색으로 칠한다. 키로 시작하지 않는 묶음은 모두 회색이다.
// on에 든 묶음은 고른 항목과 같은 바탕으로 칠해 지금 켜진 보기나 상태를 보인다. on은 묶음 전체 글자나 그 키다.
// 번역된 안내도 키 글자는 같으므로 키로 고르면 언어와 상관없이 맞는다.
func liveKeyHints(text string, color bool, on ...string) string {
	if !liveRich(color) || text == "" {
		return liveMuted(text, color)
	}
	var builder strings.Builder
	last := 0
	for _, bounds := range append(liveHintSeparator.FindAllStringIndex(text, -1), []int{len(text), len(text)}) {
		builder.WriteString(liveHintGroup(text[last:bounds[0]], on))
		builder.WriteString(richDim(text[bounds[0]:bounds[1]]))
		last = bounds[1]
	}
	return builder.String()
}

func liveHintGroup(group string, on []string) string {
	trimmed := strings.TrimLeft(group, " ")
	lead := group[:len(group)-len(trimmed)]
	key, rest, _ := strings.Cut(trimmed, " ")
	if body := strings.TrimRight(trimmed, " "); liveHintOn(body, key, on) {
		return lead + liveSelected(body, true) + trimmed[len(body):]
	}
	if !liveHintKey(key) {
		return lead + richDim(trimmed)
	}
	if rest != "" {
		rest = " " + rest
	}
	return lead + richPaint(key, richCyan, true) + richDim(rest)
}

// liveHintOn은 묶음이 on에 들었는지다. 묶음 전체 글자나 그 키가 같으면 든 것이다.
func liveHintOn(group, key string, on []string) bool {
	for _, item := range on {
		if item != "" && (item == group || item == key) {
			return true
		}
	}
	return false
}

// liveTitleHints는 "질문   키 설명   키 설명" 꼴의 제목이다. 질문은 굵게, 뒤의 키 안내는 liveKeyHints로 칠한다.
func liveTitleHints(text string, color bool) string {
	if !liveRich(color) {
		return text
	}
	title, hints, found := strings.Cut(text, "   ")
	if !found {
		return richBold(text)
	}
	return richBold(title) + liveKeyHints("   "+hints, true)
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
