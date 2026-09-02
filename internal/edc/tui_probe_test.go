package edc

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func probeAfter(t *testing.T, model probeModel, messages ...tea.Msg) (probeModel, tea.Cmd) {
	t.Helper()
	var current tea.Model = model
	var cmd tea.Cmd
	for _, msg := range messages {
		current, cmd = current.Update(msg)
	}
	updated, ok := current.(probeModel)
	if !ok {
		t.Fatalf("model = %#v", current)
	}
	return updated, cmd
}

func TestProbeModelShowsPendingLineThenResult(t *testing.T) {
	model := newProbeModel("net.trace", "example.com", "", false, false, nil)
	model.now = func() time.Time { return model.started.Add(2500 * time.Millisecond) }
	pending := model.View().Content
	if !strings.Contains(pending, "net.trace") || !strings.Contains(pending, "example.com  2.5s") {
		t.Fatalf("pending view = %q", pending)
	}
	// 대기 줄과 결과 줄이 같은 열을 쓴다.
	done := formatResultLine(Result{Probe: "net.trace", Status: StatusPass}, false)
	column := func(line string) int { return liveWidth(line[:strings.Index(line, "net.trace")]) }
	if column(pending) != column(done) {
		t.Fatalf("pending %q and done %q do not share the name column", pending, done)
	}
	final, cmd := probeAfter(t, model, probeResultMsg{result: Result{Probe: "net.trace", Status: StatusPass, Summary: "20 hops"}})
	if !final.done || cmd == nil {
		t.Fatalf("model = %#v", final)
	}
	if final.View().Content != formatResultLine(final.result, false) {
		t.Fatalf("final view = %q", final.View().Content)
	}
	if liveLineCount(final.View().Content) != 1 {
		t.Fatalf("final view must stay on one line: %q", final.View().Content)
	}
}

// 화면 줄 수가 변하면 잔상이 남으므로 진행 표시는 항상 한 줄이어야 한다.
func TestProbeModelStaysOnOneLine(t *testing.T) {
	model := newProbeModel("net.trace", "example.com", "", false, false, nil)
	model, _ = probeAfter(t, model, tea.WindowSizeMsg{Width: 200, Height: 40})
	for _, line := range []string{" 1  hop one", " 2  hop two", " 3  hop three"} {
		model, _ = probeAfter(t, model, probeLogMsg{line: line})
	}
	view := model.View().Content
	if strings.Count(view, "\n") != 1 {
		t.Fatalf("view must stay on one line: %q", view)
	}
	if !strings.Contains(view, " 3  hop three") || strings.Contains(view, "hop one") {
		t.Fatalf("view must show the latest line only: %q", view)
	}
}

func TestProbeModelTruncatesToWidth(t *testing.T) {
	model := newProbeModel("net.trace", "example.com", "", false, false, nil)
	model, _ = probeAfter(t, model, tea.WindowSizeMsg{Width: 50, Height: 40}, probeLogMsg{line: strings.Repeat("x", 200)})
	view := strings.TrimRight(model.View().Content, "\n")
	if liveWidth(view) > 50 {
		t.Fatalf("view width = %d, want at most 50", liveWidth(view))
	}
	if !strings.HasSuffix(view, "…") {
		t.Fatalf("truncated view must end with an ellipsis: %q", view)
	}
}

func TestProbeProgressHandsOverLatestLine(t *testing.T) {
	progress := &probeProgress{}
	progress.observe("first")
	progress.observe("second")
	if got := progress.snapshot(); got != "second" {
		t.Fatalf("snapshot = %q", got)
	}
	if got := progress.attach(nil); got != "second" {
		t.Fatalf("attach = %q", got)
	}
	// 화면이 아직 없어도 observe는 실패하지 않아야 한다.
	progress.observe("third")
}

func TestProbeModelRedactsStreamedLines(t *testing.T) {
	model := newProbeModel("net.trace", "example.com", "", false, true, nil)
	model, _ = probeAfter(t, model, tea.WindowSizeMsg{Width: 200, Height: 40})
	model, _ = probeAfter(t, model, probeLogMsg{line: "1  192.0.2.1  1.2 ms"})
	view := model.View().Content
	if strings.Contains(view, "192.0.2.1") || !strings.Contains(view, "<ip:") {
		t.Fatalf("view leaked the address: %q", view)
	}
}

func TestProbeModelCancelsOnCtrlC(t *testing.T) {
	cancelled := false
	model, _ := probeAfter(t, newProbeModel("net.ping", "example.com", "", false, false, func() { cancelled = true }), tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
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

func TestProbeLineWriterSplitsLines(t *testing.T) {
	var lines []string
	writer := &probeLineWriter{observe: func(line string) { lines = append(lines, line) }}
	_, _ = writer.Write([]byte("first\r\nsec"))
	_, _ = writer.Write([]byte("ond\nthird"))
	writer.Flush()
	if strings.Join(lines, "|") != "first|second|third" {
		t.Fatalf("lines = %#v", lines)
	}
}

func TestCommandOutputStreamsWithObserver(t *testing.T) {
	if _, err := exec.LookPath("printf"); err != nil {
		t.Skip("printf is not available")
	}
	var lines []string
	ctx := withProbeObserver(context.Background(), func(line string) { lines = append(lines, line) })
	output, err := commandOutput(ctx, "printf", "one\ntwo\n")
	if err != nil {
		t.Fatal(err)
	}
	if output != "one\ntwo" {
		t.Fatalf("output = %q", output)
	}
	if strings.Join(lines, "|") != "one|two" {
		t.Fatalf("streamed lines = %#v", lines)
	}
	// observer가 없으면 예전처럼 모아서 돌려준다.
	plain, err := commandOutput(context.Background(), "printf", "one\ntwo\n")
	if err != nil || plain != "one\ntwo" {
		t.Fatalf("plain output = %q, %v", plain, err)
	}
}

func TestProbeObserverFromEmptyContext(t *testing.T) {
	if probeObserverFrom(context.Background()) != nil {
		t.Fatal("an empty context must carry no observer")
	}
}
