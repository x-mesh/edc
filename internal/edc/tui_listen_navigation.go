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

// navigationRow는 t, x, p가 다룰 줄이다. 한 줄에 PID가 여럿이면 activePID가 그중 하나를 정한다.
func (model listenBrowser) navigationRow() (listenRow, bool) {
	if model.detail != nil {
		return model.detail.row, true
	}
	return model.selectedRow()
}

func (model listenBrowser) navigateProcess(key string) (listenBrowser, tea.Cmd) {
	row, ok := model.navigationRow()
	if !ok {
		return model, nil
	}
	pid := model.activePID(row)
	if _, err := listenBrowserTopCommand("edc", pid); err != nil {
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
			return model, model.openTop(pid)
		}
	case "x":
		model.detailSeq++
		model.detail = &listenBrowserDetail{row: row, seq: model.detailSeq, trace: true, lines: listenBrowserTraceHint(pid)}
	}
	return model, nil
}

// listenBrowserTraceHint는 x가 보이는 CPU trace 안내다.
func listenBrowserTraceHint(pid string) []string {
	return []string{
		T("listenbrowse.trace_hint", pid),
		"",
		"edc trace cpu --process " + pid + " --duration 15s",
		"",
		T("listenbrowse.trace_requirements"),
	}
}
