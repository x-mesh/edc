package edc

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// askStop은 S를 받아 고른 PID를 잡고 확인 상자를 띄운다. 막는 줄이면 이유만 알린다.
func (model listenBrowser) askStop() listenBrowser {
	row, ok := model.navigationRow()
	if !ok || model.tooSmall() {
		return model
	}
	if model.redact {
		model.notice = T("listenbrowse.stop_redacted")
		return model
	}
	// container가 publish한 포트를 연 process는 대개 docker-proxy다. 멈추면 container는 그대로 두고 포트 연결만 끊긴다.
	if container := row.first().Container; container != nil {
		model.notice = viewSanitize(T("listenbrowse.stop_container", container.Name))
		return model
	}
	pid, err := strconv.Atoi(model.activePID(row))
	if err != nil || pid <= 0 {
		model.notice = T("listenbrowse.no_owner")
		return model
	}
	if pid == 1 || pid == os.Getpid() {
		model.notice = T("listenbrowse.stop_refused", pid)
		return model
	}
	if model.openStop == nil {
		return model
	}
	name := model.activeSocket(row).Process
	process, err := model.openStop(pid, name, model.listed)
	if err != nil {
		model.notice = listenStopMessage(pid, err)
		return model
	}
	question := T("listenbrowse.stop_question", viewSanitize(listenCell(name)), pid, model.stopPorts(pid))
	// worker 하나만 멈추면 부모가 다시 띄울 수 있다. 줄에 PID가 여럿이면 그중 하나만 멈춘다는 것을 함께 보인다.
	note := ""
	if pids := row.pids(); len(pids) > 1 {
		note = T("listenbrowse.stop_one_of", len(pids))
	}
	model.stop = &listenStopConfirm{process: process, pid: pid, name: name, question: question, note: note}
	return model
}

// answerStop은 확인 상자가 떠 있을 때의 키를 받는다. 기본 선택은 아니오이고, y나 예에서 누른 Enter만 시그널을 보낸다.
// 이동 키를 누르다 확인 상자에 닿아도 시그널이 나가지 않게, 그 밖의 키는 모두 취소다. l은 loopback 키라서 선택을 바꾸지 않는다.
func (model listenBrowser) answerStop(key string) (tea.Model, tea.Cmd) {
	confirm := *model.stop
	switch key {
	case "left", "right", "tab", "shift+tab":
		confirm.yes = !confirm.yes
		model.stop = &confirm
		return model, nil
	case "y":
		confirm.yes = true
	case "enter":
	case "ctrl+c":
		confirm.process.close()
		model.stop = nil
		return model, tea.Quit
	default:
		confirm.yes = false
	}
	model.stop = nil
	if !confirm.yes {
		confirm.process.close()
		model.notice = T("listenbrowse.stop_cancelled")
		return model, nil
	}
	return model, func() tea.Msg {
		defer confirm.process.close()
		return listenStopDoneMsg{pid: confirm.pid, name: confirm.name, err: confirm.process.stop()}
	}
}

// listenStopPortsShown은 확인 상자에 적는 포트 수다. 나머지는 +N으로 줄여 질문이 상자에서 길어지지 않게 한다.
const listenStopPortsShown = 3

// stopPorts는 PID가 연 포트를 잇는다. 고른 줄 말고도 같은 process가 연 포트가 함께 닫힌다는 것을 확인 상자에서 보인다.
func (model listenBrowser) stopPorts(pid int) string {
	id := strconv.Itoa(pid)
	var ports []string
	for _, socket := range model.sockets {
		if socket.PID != id {
			continue
		}
		port := socket.Proto
		if socket.Port > 0 {
			port = fmt.Sprintf("%d/%s", socket.Port, socket.Proto)
		}
		if !slices.Contains(ports, port) {
			ports = append(ports, port)
		}
	}
	if len(ports) > listenStopPortsShown {
		return strings.Join(ports[:listenStopPortsShown], ", ") + fmt.Sprintf(" +%d", len(ports)-listenStopPortsShown)
	}
	return strings.Join(ports, ", ")
}

// listenStopBoxWidth는 확인 상자의 안쪽 폭이다. 좁은 화면에서는 화면 폭에 맞춰 줄인다.
const listenStopBoxWidth = 56

// stopBox는 화면 가운데에 겹칠 확인 상자다. 목록 위의 한 줄로는 눈에 잘 띄지 않아 상자로 띄운다. 질문은 상자 폭에서
// 줄을 바꾸고, 선택지는 잘리지 않게 따로 한 줄을 쓴다. 화면이 낮으면 빈 줄과 키 안내를 먼저 뺀다.
func (model listenBrowser) stopBox() []string {
	color := liveRich(model.color)
	inner := max(16, min(listenStopBoxWidth, model.width-4))
	// 배경은 회색으로 옅어지므로 테두리는 기본 글자색으로 두어 상자의 경계가 드러나게 한다.
	side := "│"
	pad := func(text string) string {
		text = ansi.Truncate(text, inner-2, "…")
		return side + " " + text + strings.Repeat(" ", max(0, inner-2-ansi.StringWidth(text))) + " " + side
	}
	title := " " + T("listenbrowse.stop_title") + " "
	rest := strings.Repeat("─", max(0, inner-ansi.StringWidth(title))) + "┐"
	top := "┌" + title + rest
	if color {
		top = "┌" + richPaint(title, richMagenta, true) + rest
	}
	bottom := "└" + strings.Repeat("─", inner) + "┘"
	var question []string
	for _, line := range strings.Split(ansi.Wrap(model.stop.question, inner-2, ""), "\n") {
		if color {
			line = richBold(line)
		}
		question = append(question, pad(line))
	}
	if model.stop.note != "" {
		question = append(question, pad(topDim(model.stop.note, color)))
	}
	choice := pad(confirmOption(confirmYesLabel(), model.stop.yes, model.color) + "  " + confirmOption(confirmNoLabel(), !model.stop.yes, model.color))
	help := pad(liveKeyHints(confirmHelp(), model.color))
	blank := pad("")
	lines := append(append([]string{top, blank}, question...), blank, choice, help, blank, bottom)
	if len(lines) > model.height {
		lines = append(append([]string{top}, question...), choice, help, bottom)
	}
	if len(lines) > model.height {
		lines = append(append([]string{top}, question...), choice, bottom)
	}
	return lines
}

// tooSmall이면 화면은 크기 안내만 보인다.
func (model listenBrowser) tooSmall() bool { return model.width < 48 || model.height < 8 }

// stopResult는 SIGTERM 뒤에 다시 읽은 목록에 그 PID의 소켓이 남았는지 알린다. 상세 화면의 줄이 사라졌으면 목록으로 돌아간다.
func (model *listenBrowser) stopResult(pid int) string {
	if model.detail != nil && !slices.ContainsFunc(model.items, func(row listenRow) bool { return row.key() == model.detail.row.key() }) {
		model.detail = nil
	}
	id := strconv.Itoa(pid)
	for _, socket := range model.sockets {
		if socket.PID == id {
			return T("listenbrowse.stop_still_open", pid)
		}
	}
	return T("listenbrowse.stop_closed", pid)
}
