package edc

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
)

func TestRemoteStreamLayout(t *testing.T) {
	var output strings.Builder
	display := newRemoteDisplay(&output, remoteDisplayOptions{verbose: true})
	writer := &remoteStreamWriter{display: display, prefix: "build-server/git-kit/command"}
	_, _ = writer.Write([]byte("first\nsecond"))
	writer.Flush()
	text := output.String()
	if !strings.Contains(text, "build-server/git-kit/command") || !strings.Contains(text, "| first") || !strings.Contains(text, "| second") {
		t.Fatalf("layout = %q", text)
	}
}

func TestRemotePlainResultsStreamWithoutLiveScreen(t *testing.T) {
	var output strings.Builder
	display := newRemoteDisplay(&output, remoteDisplayOptions{results: true})
	if display.live != nil {
		t.Fatal("live screen must stay off when the option is not set")
	}
	display.Result(Result{Probe: "remote.one.gk", Status: StatusPass, Summary: "ok", Metrics: map[string]interface{}{"host": "one", "step": "gk"}})
	display.Close()
	if !strings.Contains(output.String(), "PASS  one.gk") {
		t.Fatalf("output = %q", output.String())
	}
}

// live 화면이 켜지면 결과 줄 대신 매트릭스 model이 갱신되어야 한다.
func TestRemoteDisplaySendsToLiveModel(t *testing.T) {
	hosts := []remoteHost{{Name: "one", Target: "one"}}
	recipe := remoteRecipe{Name: "daily", Steps: []remoteStep{{Name: "gk", Command: "gk update", Timeout: time.Minute}}}
	model := newRemoteModel(remotePlanView{group: "daily", hosts: hosts, recipe: recipe, width: 100}, false, false, true, nil)
	live, err := startLiveProgram(model, nil, headlessOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	display := &remoteDisplay{output: io.Discard, results: true, verbose: true, live: live}
	display.Start("one", "gk", remotePhaseCommand)
	display.WriteLine("one/gk/command", "updating")
	display.Result(Result{Probe: "remote.one.gk", Status: StatusPass, Summary: "ok", Metrics: map[string]interface{}{"host": "one", "step": "gk"}})
	display.Close()
	final, ok := live.model.(remoteModel)
	if !ok {
		t.Fatalf("model = %#v", live.model)
	}
	if final.cells[0][0].state != remoteCellDone || final.cells[0][0].status != StatusPass {
		t.Fatalf("cell = %#v", final.cells[0][0])
	}
	if len(final.logs) != 1 || !strings.Contains(final.logs[0], "updating") {
		t.Fatalf("logs = %#v", final.logs)
	}
	if !final.finished || final.completed != 1 {
		t.Fatalf("model did not finish: %#v", final)
	}
}

// -f 실행은 확인을 받지 않으므로 기다리지 않고 바로 시작해야 한다.
func TestRemoteDisplayWithoutConfirmDoesNotWait(t *testing.T) {
	hosts := []remoteHost{{Name: "one", Target: "one"}}
	recipe := remoteRecipe{Name: "daily", Steps: []remoteStep{{Name: "gk", Command: "gk update", Timeout: time.Minute}}}
	view := remotePlanView{group: "daily", hosts: hosts, recipe: recipe, width: 100}
	model := newRemoteModel(view, false, false, false, nil)
	live, err := startLiveProgram(model, nil, headlessOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	display := &remoteDisplay{output: io.Discard, results: true, live: live}
	done := make(chan bool, 1)
	go func() { done <- display.awaitConfirm() }()
	select {
	case answer := <-done:
		if !answer {
			t.Fatal("확인이 없으면 바로 실행해야 한다")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("awaitConfirm이 답을 기다리며 멈췄다")
	}
	display.Close()
}

// 확인을 받는 실행은 화면이 답을 보낼 때까지 기다린다.
func TestRemoteDisplayWithConfirmWaitsForAnswer(t *testing.T) {
	hosts := []remoteHost{{Name: "one", Target: "one"}}
	recipe := remoteRecipe{Name: "daily", Steps: []remoteStep{{Name: "gk", Command: "gk update", Timeout: time.Minute}}}
	view := remotePlanView{group: "daily", hosts: hosts, recipe: recipe, width: 100}
	model := newRemoteModel(view, true, false, false, nil)
	live, err := startLiveProgram(model, nil, headlessOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	display := &remoteDisplay{output: io.Discard, results: true, live: live, confirm: true, answered: model.answered}
	waiting := make(chan bool, 1)
	go func() { waiting <- display.awaitConfirm() }()
	select {
	case <-waiting:
		t.Fatal("답을 보내기 전에는 기다려야 한다")
	case <-time.After(200 * time.Millisecond):
	}
	live.send(tea.KeyPressMsg{Code: 'y', Text: "y"})
	select {
	case answer := <-waiting:
		if !answer {
			t.Fatal("예를 고르면 실행해야 한다")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("확인 답이 전달되지 않았다")
	}
	display.Close()
}

func TestRemoteDisplayStartupFailure(t *testing.T) {
	for _, confirm := range []bool{true, false} {
		t.Run(fmt.Sprintf("confirm=%t", confirm), func(t *testing.T) {
			var output strings.Builder
			calls := 0
			display := newRemoteDisplayWithStarter(&output, remoteDisplayOptions{live: true, confirm: confirm, results: true}, func(tea.Model, func(), ...tea.ProgramOption) (*liveProgram, error) {
				calls++
				return nil, errors.New("mock startup failure")
			})
			if calls != 1 || display.live != nil {
				t.Fatalf("calls = %d, live = %v", calls, display.live)
			}
			if allowed := display.awaitConfirm(); allowed != !confirm {
				t.Fatalf("awaitConfirm = %t, want %t", allowed, !confirm)
			}
			if !confirm {
				display.Result(Result{Probe: "remote.one.gk", Status: StatusPass, Summary: "ok", Metrics: map[string]interface{}{"host": "one", "step": "gk"}})
				if !strings.Contains(output.String(), "PASS") || !strings.Contains(output.String(), "one.gk") {
					t.Fatalf("output = %q", output.String())
				}
			}
		})
	}
}

func TestRemoteDisplayConfirmWithoutScreenOrChannel(t *testing.T) {
	for name, display := range map[string]*remoteDisplay{
		"no screen":  {confirm: true, answered: make(chan bool)},
		"no channel": {confirm: true, live: &liveProgram{done: make(chan struct{})}},
		"neither":    {confirm: true},
	} {
		t.Run(name, func(t *testing.T) {
			if display.awaitConfirm() {
				t.Fatal("missing confirmation screen or channel must deny execution")
			}
		})
	}
}

func TestRemoteDisplayConfirmDeniesNoAndClose(t *testing.T) {
	for _, key := range []rune{'n', 'q'} {
		t.Run(string(key), func(t *testing.T) {
			model := newRemoteModel(remotePlanView{}, true, false, false, nil)
			live, err := startLiveProgram(model, nil, headlessOptions()...)
			if err != nil {
				t.Fatal(err)
			}
			display := &remoteDisplay{output: io.Discard, confirm: true, live: live, answered: model.answered}
			defer display.Close()
			waiting := make(chan bool, 1)
			go func() { waiting <- display.awaitConfirm() }()
			live.send(tea.KeyPressMsg{Code: key, Text: string(key)})
			select {
			case answer := <-waiting:
				if answer {
					t.Fatal("refusal or screen close must deny execution")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("confirmation did not stop after refusal or close")
			}
		})
	}
}

// 줄바꿈 없이 이어지는 출력도 상한을 넘으면 한 줄로 내보내, 실시간 화면이 출력 전체를 메모리에 쌓지 않게 한다.
func TestRemoteStreamWriterBoundsALineWithoutNewline(t *testing.T) {
	var output strings.Builder
	display := newRemoteDisplay(&output, remoteDisplayOptions{verbose: true})
	writer := &remoteStreamWriter{display: display, prefix: "p"}
	chunk := strings.Repeat("가", 1000)
	for index := 0; index < 20; index++ {
		_, _ = writer.Write([]byte(chunk))
		if len(writer.pending) > remoteStreamLineLimit {
			t.Fatalf("pending grew to %d bytes", len(writer.pending))
		}
	}
	writer.Flush()
	written := 0
	for _, line := range strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n") {
		_, text, _ := strings.Cut(line, "| ")
		if !utf8.ValidString(text) || len(text) > remoteStreamLineLimit {
			t.Fatalf("line of %d bytes, valid=%v", len(text), utf8.ValidString(text))
		}
		written += len(text)
	}
	if written != 20*len(chunk) {
		t.Fatalf("wrote %d bytes, want %d", written, 20*len(chunk))
	}
}

// 글자 경계가 없는 바이트열도 상한에서 잘라야 loop가 끝난다.
func TestRemoteStreamWriterCutsBytesWithoutARuneStart(t *testing.T) {
	display := newRemoteDisplay(io.Discard, remoteDisplayOptions{verbose: true})
	writer := &remoteStreamWriter{display: display, prefix: "p"}
	_, _ = writer.Write([]byte(strings.Repeat("\x80", 3*remoteStreamLineLimit)))
	if len(writer.pending) > remoteStreamLineLimit {
		t.Fatalf("pending grew to %d bytes", len(writer.pending))
	}
}
