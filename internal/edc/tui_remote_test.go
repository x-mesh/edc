package edc

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func remoteFixtureModel(cancel func()) remoteModel {
	hosts := []remoteHost{{Name: "workstation", Tags: []string{"mac"}}, {Name: "build-server", Tags: []string{"linux"}}}
	recipe := remoteRecipe{Name: "daily", Steps: []remoteStep{
		{Name: "git-kit", Command: "git-kit update", Timeout: time.Minute},
		{Name: "brew", Command: "brew upgrade", Timeout: time.Minute, Tags: []string{"mac"}},
	}}
	return newRemoteModel(remotePlanView{group: "daily", hosts: hosts, recipe: recipe, width: 100}, false, false, false, cancel)
}

func remoteConfirmModel(cancel func()) remoteModel {
	hosts := []remoteHost{{Name: "workstation", Tags: []string{"mac"}}, {Name: "build-server", Tags: []string{"linux"}}}
	recipe := remoteRecipe{Name: "daily", Steps: []remoteStep{
		{Name: "git-kit", Command: "git-kit update", Timeout: time.Minute},
		{Name: "brew", Command: "brew upgrade", Timeout: time.Minute, Tags: []string{"mac"}},
	}}
	view := remotePlanView{group: "daily", inventoryPath: "/tmp/inventory.yaml", recipePath: "/tmp/recipe.yaml", hosts: hosts, recipe: recipe, width: 100}
	return newRemoteModel(view, true, false, false, cancel)
}

func remoteAfter(t *testing.T, model remoteModel, messages ...tea.Msg) remoteModel {
	t.Helper()
	var current tea.Model = model
	for _, msg := range messages {
		current, _ = current.Update(msg)
	}
	updated, ok := current.(remoteModel)
	if !ok {
		t.Fatalf("model = %#v", current)
	}
	return updated
}

func TestRemoteModelSkipsCellsForUnmatchedTags(t *testing.T) {
	model := remoteFixtureModel(nil)
	if model.total != 3 {
		t.Fatalf("total = %d, want 3", model.total)
	}
	if model.cells[1][1].state != remoteCellAbsent {
		t.Fatalf("linux host must have no brew cell: %#v", model.cells[1][1])
	}
	if model.cellText(model.cells[1][1]) != remoteGlyphAbsent {
		t.Fatalf("absent cell = %q", model.cellText(model.cells[1][1]))
	}
	if model.cellText(model.cells[0][1]) != remoteGlyphPending {
		t.Fatalf("pending cell = %q", model.cellText(model.cells[0][1]))
	}
}

func TestRemoteModelTracksStartAndResult(t *testing.T) {
	model := remoteAfter(t, remoteFixtureModel(nil), remoteStartMsg{host: "workstation", step: "git-kit", phase: remotePhaseVerify})
	cell := model.cells[0][0]
	if cell.state != remoteCellRunning || cell.phase != remotePhaseVerify {
		t.Fatalf("cell = %#v", cell)
	}
	// 실행 중인 칸은 spinner만 담고 phase는 상태줄이 알려 준다.
	if liveWidth(model.cellText(cell)) != 1 {
		t.Fatalf("running cell must stay narrow: %q", model.cellText(cell))
	}
	if !strings.Contains(model.View().Content, "workstation / git-kit / "+remotePhaseVerify) {
		t.Fatalf("status line must name the running phase: %q", model.View().Content)
	}
	model = remoteAfter(t, model,
		remoteResultMsg{host: "workstation", step: "git-kit", status: StatusPass},
		remoteResultMsg{host: "build-server", step: "git-kit", status: StatusFail},
	)
	if model.cells[0][0].status != StatusPass || model.cells[1][0].status != StatusFail {
		t.Fatalf("cells = %#v", model.cells)
	}
	if model.completed != 2 {
		t.Fatalf("completed = %d", model.completed)
	}
	view := model.View().Content
	for _, expected := range []string{"host", "git-kit", "workstation", "PASS", "FAIL", "2/3 완료"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("view %q does not contain %q", view, expected)
		}
	}
	if !strings.HasSuffix(view, "\n") {
		t.Fatalf("view must end with a newline: %q", view)
	}
}

func TestRemoteModelIgnoresUnknownCells(t *testing.T) {
	model := remoteAfter(t, remoteFixtureModel(nil), remoteResultMsg{host: "absent", step: "git-kit", status: StatusPass})
	if model.completed != 0 {
		t.Fatalf("unknown host must not change the matrix: %d", model.completed)
	}
}

func TestRemoteModelKeepsLogTail(t *testing.T) {
	model := remoteFixtureModel(nil)
	model.verbose = true
	for index := 0; index < remoteLogTailLines+5; index++ {
		model = remoteAfter(t, model, remoteLogMsg{prefix: "workstation/brew/command", line: "line"})
	}
	if len(model.logs) != remoteLogTailLines {
		t.Fatalf("logs = %d, want %d", len(model.logs), remoteLogTailLines)
	}
	if !strings.Contains(model.View().Content, "workstation/brew/command │ line") {
		t.Fatalf("verbose view = %q", model.View().Content)
	}
	quiet := remoteFixtureModel(nil)
	quiet = remoteAfter(t, quiet, remoteLogMsg{prefix: "workstation/brew/command", line: "line"})
	if strings.Contains(quiet.View().Content, "line") {
		t.Fatalf("log tail must stay hidden without verbose: %q", quiet.View().Content)
	}
}

func TestRemoteModelCancelsOnCtrlC(t *testing.T) {
	cancelled := false
	model := remoteAfter(t, remoteFixtureModel(func() { cancelled = true }), tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if !cancelled || !model.cancelling {
		t.Fatalf("first ctrl+c did not cancel: %v %#v", cancelled, model.cancelling)
	}
	if !strings.Contains(model.View().Content, "취소 중") {
		t.Fatalf("view = %q", model.View().Content)
	}
	if _, cmd := model.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}); cmd == nil {
		t.Fatal("second ctrl+c must quit the program")
	}
}

func TestRemoteModelFinishShowsSummaryLine(t *testing.T) {
	model := remoteFixtureModel(nil)
	model.now = func() time.Time { return model.started.Add(14 * time.Second) }
	updated, cmd := model.Update(remoteDoneMsg{})
	final, ok := updated.(remoteModel)
	if !ok || !final.finished || cmd == nil {
		t.Fatalf("model = %#v, cmd = %v", updated, cmd)
	}
	// 마지막 요약 줄은 표 아래에서 한 번만 나온다. model은 자리만 비운다.
	if strings.Contains(final.View().Content, "완료") {
		t.Fatalf("final view must not repeat the summary: %q", final.View().Content)
	}
}

func TestRemoteModelTrimsHostsToScreenHeight(t *testing.T) {
	model := remoteAfter(t, remoteFixtureModel(nil), tea.WindowSizeMsg{Width: 80, Height: 3})
	if len(model.visibleHosts()) != 1 {
		t.Fatalf("visible hosts = %#v", model.visibleHosts())
	}
	if !strings.Contains(model.View().Content, "…외 1 host") {
		t.Fatalf("view = %q", model.View().Content)
	}
}

func TestRemoteModelColumnsStayAligned(t *testing.T) {
	model := remoteAfter(t, remoteFixtureModel(nil),
		remoteStartMsg{host: "workstation", step: "git-kit", phase: remotePhaseCommand},
		remoteResultMsg{host: "build-server", step: "git-kit", status: StatusPass},
	)
	lines := strings.Split(strings.TrimRight(model.View().Content, "\n"), "\n")
	start := -1
	for index, line := range lines {
		if strings.HasPrefix(line, "host") {
			start = index
			break
		}
	}
	if start < 0 {
		t.Fatalf("table header is missing: %q", model.View().Content)
	}
	// header와 host 행은 같은 열 배치를 쓰고 terminal 폭을 넘지 않는다.
	for _, line := range lines[start : start+len(model.table.hosts)+1] {
		if liveWidth(line) > 100 {
			t.Fatalf("line %q is wider than the terminal", line)
		}
	}
	// 첫 step 열은 host 이름 열 바로 뒤에서 시작한다.
	first := model.table.nameWidth + remoteColumnGap
	for _, line := range lines[start : start+len(model.table.hosts)+1] {
		if got := liveWidth(line) - liveWidth(strings.TrimLeft(line[:len(line)], " ")); got != 0 {
			t.Fatalf("line %q must not start with padding", line)
		}
		if liveWidth(line) <= first {
			t.Fatalf("line %q is shorter than the name column", line)
		}
	}
	if column(lines[start], "git-kit") != first {
		t.Fatalf("header %q starts the first column at %d, want %d", lines[start], column(lines[start], "git-kit"), first)
	}
}

// 확인과 실행이 같은 표를 쓴다. 표가 두 번 그려지면 안 된다.
func TestRemoteModelConfirmsOnTheSameTable(t *testing.T) {
	model := remoteConfirmModel(nil)
	view := model.View().Content
	for _, expected := range []string{"edc remote  daily  ·  host 2  ·  step 2", "inventory  /tmp/inventory.yaml", "host", "workstation", "git-kit  git-kit update", "실행할까요?", confirmHelp} {
		if !strings.Contains(view, expected) {
			t.Fatalf("confirm view %q does not contain %q", view, expected)
		}
	}
	if strings.Count(view, "\nhost ") != 1 && strings.Count(view, "host  ") != 1 {
		t.Fatalf("table must appear once: %q", view)
	}
	answered, _ := model.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	running := answered.(remoteModel)
	if running.stage != remoteStageRunning || !<-running.answered {
		t.Fatalf("y must confirm: %#v", running.stage)
	}
	runningView := running.View().Content
	if strings.Contains(runningView, "실행할까요?") || !strings.Contains(runningView, "0/3 완료") {
		t.Fatalf("running view = %q", runningView)
	}
	// 확인 블록이 상태줄로 바뀌어도 화면 높이는 그대로여야 한다.
	if liveLineCount(runningView) != liveLineCount(view) {
		t.Fatalf("frame height changed: %d → %d", liveLineCount(view), liveLineCount(runningView))
	}
}

func TestRemoteModelConfirmRejects(t *testing.T) {
	for _, key := range []tea.KeyPressMsg{{Code: 'n', Text: "n"}, {Code: tea.KeyEscape}, {Code: 'c', Mod: tea.ModCtrl}} {
		model := remoteConfirmModel(nil)
		answered, cmd := model.Update(key)
		if cmd == nil {
			t.Fatalf("%s must quit the program", key.String())
		}
		if got := <-answered.(remoteModel).answered; got {
			t.Fatalf("%s must reject the run", key.String())
		}
	}
	// 방향키로 예를 고른 뒤 Enter를 누르면 실행한다.
	model := remoteConfirmModel(nil)
	moved, _ := model.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	confirmed, _ := moved.(remoteModel).Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := <-confirmed.(remoteModel).answered; !got {
		t.Fatal("enter on 예 must confirm the run")
	}
}
