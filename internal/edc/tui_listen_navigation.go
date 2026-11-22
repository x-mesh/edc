package edc

import (
	"errors"
	"os"
	"os/exec"
	"strconv"

	tea "charm.land/bubbletea/v2"
)

type listenBrowserTopDoneMsg struct{ err error }

func listenBrowserTopCommand(binary, pid string) (*exec.Cmd, error) {
	value, err := strconv.ParseUint(pid, 10, 31)
	if err != nil || value == 0 {
		return nil, errors.New(T("listenbrowse.no_owner"))
	}
	return exec.Command(binary, "top", "--interactive", "--count", "0", "--json=", "--process", strconv.FormatUint(value, 10)), nil
}

func listenBrowserOpenTop(pid string) tea.Cmd {
	binary, err := os.Executable()
	if err != nil {
		return func() tea.Msg { return listenBrowserTopDoneMsg{err: err} }
	}
	command, err := listenBrowserTopCommand(binary, pid)
	if err != nil {
		return func() tea.Msg { return listenBrowserTopDoneMsg{err: err} }
	}
	return tea.ExecProcess(command, func(err error) tea.Msg { return listenBrowserTopDoneMsg{err: err} })
}

func (model listenBrowser) navigationSocket() (listenSocket, bool) {
	if model.detail != nil {
		return model.detail.socket, true
	}
	return model.selected()
}

func (model listenBrowser) navigateProcess(key string) (listenBrowser, tea.Cmd) {
	socket, ok := model.navigationSocket()
	if !ok {
		return model, nil
	}
	if _, err := listenBrowserTopCommand("edc", socket.PID); err != nil {
		model.notice = T("listenbrowse.no_owner")
		return model, nil
	}
	if model.redact {
		model.notice = T("listenbrowse.navigation_redacted")
		return model, nil
	}
	switch key {
	case "t":
		if model.openTop != nil {
			return model, model.openTop(socket.PID)
		}
	case "x":
		model.detailSeq++
		model.detail = &listenBrowserDetail{socket: socket, seq: model.detailSeq, lines: []string{
			T("listenbrowse.trace_hint", socket.PID),
			"",
			"edc trace cpu --process " + socket.PID + " --duration 15s",
			"",
			T("listenbrowse.trace_requirements"),
		}}
	}
	return model, nil
}
