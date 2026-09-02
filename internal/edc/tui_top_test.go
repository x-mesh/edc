package edc

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func topFixtureModel(sample func() (resourceSnapshot, error)) topModel {
	details := hostDetails{Hostname: "host", Model: "model", Cores: 8, MemoryTotal: 16 * 1024 * 1024 * 1024}
	first := resourceSnapshot{TakenAt: time.Unix(0, 0), CPUTotal: 100}
	model := newTopModel(details, first, time.Second, sample)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: topTableWidth, Height: 20})
	return updated.(topModel)
}

func topSampleAt(seconds int) resourceSnapshot {
	return resourceSnapshot{TakenAt: time.Unix(int64(seconds), 0), CPUTotal: 200, MemoryUsed: 25, MemoryTotal: 100}
}

func topAfter(t *testing.T, model topModel, messages ...tea.Msg) topModel {
	t.Helper()
	var current tea.Model = model
	for _, msg := range messages {
		current, _ = current.Update(msg)
	}
	updated, ok := current.(topModel)
	if !ok {
		t.Fatalf("model = %#v", current)
	}
	return updated
}

func TestTopModelAppendsRowsFromSamples(t *testing.T) {
	model := topAfter(t, topFixtureModel(nil), topSampleMsg{snapshot: topSampleAt(1)}, topSampleMsg{snapshot: topSampleAt(2)})
	if len(model.rows) != 2 {
		t.Fatalf("rows = %#v", model.rows)
	}
	want := formatTopRow(topSampleAt(2).TakenAt, calculateRate(topSampleAt(1), topSampleAt(2)), model.limits)
	if model.rows[1] != want {
		t.Fatalf("row = %q, want %q", model.rows[1], want)
	}
	view := model.View()
	if !view.AltScreen {
		t.Fatal("dashboard must use the alt screen")
	}
	for _, expected := range []string{topColumnHeader, T("observe.top.help_line"), "interval 1s"} {
		if !strings.Contains(view.Content, expected) {
			t.Fatalf("view %q does not contain %q", view.Content, expected)
		}
	}
}

func TestTopModelIgnoresStaleSamples(t *testing.T) {
	model := topAfter(t, topFixtureModel(nil), topSampleMsg{seq: 3, snapshot: topSampleAt(1)})
	if len(model.rows) != 0 {
		t.Fatalf("stale sample was applied: %#v", model.rows)
	}
}

func TestTopModelQuitsOnSampleError(t *testing.T) {
	model, cmd := topFixtureModel(nil).Update(topSampleMsg{err: errors.New("read failed")})
	final, ok := model.(topModel)
	if !ok || final.err == nil {
		t.Fatalf("model = %#v", model)
	}
	if cmd == nil {
		t.Fatal("a sample error must quit the dashboard")
	}
}

func TestTopModelPauseStopsSampling(t *testing.T) {
	sample := func() (resourceSnapshot, error) { return topSampleAt(1), nil }
	paused, cmd := topFixtureModel(sample).Update(tea.KeyPressMsg{Code: 'p', Text: "p"})
	model, ok := paused.(topModel)
	if !ok || !model.paused {
		t.Fatalf("p did not pause: %#v", paused)
	}
	if cmd != nil {
		t.Fatal("a paused dashboard must not schedule the next sample")
	}
	resumed, cmd := model.Update(tea.KeyPressMsg{Code: 'p', Text: "p"})
	if resumed.(topModel).paused {
		t.Fatal("p must resume the dashboard")
	}
	if cmd == nil {
		t.Fatal("resuming must schedule the next sample")
	}
	if !strings.Contains(model.View().Content, T("observe.top.paused")) {
		t.Fatalf("view = %q", model.View().Content)
	}
}

func TestTopModelChangesInterval(t *testing.T) {
	model := topFixtureModel(func() (resourceSnapshot, error) { return topSampleAt(1), nil })
	slower := topAfter(t, model, tea.KeyPressMsg{Code: '+', Text: "+"})
	if slower.interval != 2*time.Second || slower.seq == model.seq {
		t.Fatalf("interval = %s, seq = %d", slower.interval, slower.seq)
	}
	faster := topAfter(t, model, tea.KeyPressMsg{Code: '-', Text: "-"})
	if faster.interval != 500*time.Millisecond {
		t.Fatalf("interval = %s", faster.interval)
	}
}

func TestNextTopIntervalClampsAndInsertsCLIValue(t *testing.T) {
	if got := nextTopInterval(topMinInterval, -1); got != topMinInterval {
		t.Fatalf("lower bound = %s", got)
	}
	if got := nextTopInterval(topMaxInterval, 1); got != topMaxInterval {
		t.Fatalf("upper bound = %s", got)
	}
	// ladder에 없는 CLI 값은 자기 자리를 만들고 그 옆으로 움직인다.
	if got := nextTopInterval(3*time.Second, 1); got != 5*time.Second {
		t.Fatalf("next from 3s = %s", got)
	}
	if got := nextTopInterval(3*time.Second, -1); got != 2*time.Second {
		t.Fatalf("previous from 3s = %s", got)
	}
}

func TestTopModelTrimsHistory(t *testing.T) {
	rows := make([]string, topDashboardHistory)
	model := topFixtureModel(nil)
	model.rows = rows
	model = topAfter(t, model, topSampleMsg{snapshot: topSampleAt(1)})
	if len(model.rows) != topDashboardHistory {
		t.Fatalf("rows = %d, want %d", len(model.rows), topDashboardHistory)
	}
}

func TestTopModelQuitKeys(t *testing.T) {
	for _, key := range []tea.KeyPressMsg{{Code: 'q', Text: "q"}, {Code: 'c', Mod: tea.ModCtrl}} {
		if _, cmd := topFixtureModel(nil).Update(key); cmd == nil {
			t.Fatalf("%s must quit the dashboard", key.String())
		}
	}
}

func TestFormatTopRowMatchesPrintedRow(t *testing.T) {
	var output strings.Builder
	rate := resourceRate{NetIn: 0.04 * 1024 * 1024, CPUUser: 1, MemoryPercent: 11.8}
	at := time.Date(2026, 1, 1, 11, 36, 44, 0, time.UTC)
	printTopRow(&output, at, rate, newTopLimits(8, false))
	if strings.TrimRight(output.String(), "\n") != formatTopRow(at, rate, newTopLimits(8, false)) {
		t.Fatalf("printed row and formatted row differ: %q", output.String())
	}
}
