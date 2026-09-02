package edc

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func doctorAfter(t *testing.T, model doctorModel, messages ...tea.Msg) doctorModel {
	t.Helper()
	var current tea.Model = model
	for _, msg := range messages {
		current, _ = current.Update(msg)
	}
	updated, ok := current.(doctorModel)
	if !ok {
		t.Fatalf("model = %#v", current)
	}
	return updated
}

func TestDoctorProbeNamesAreSorted(t *testing.T) {
	probes := []doctorProbe{{name: "tls.check"}, {name: "dns.lookup"}, {name: "http.check"}}
	names := doctorProbeNames(probes)
	if strings.Join(names, ",") != "dns.lookup,http.check,tls.check" {
		t.Fatalf("names = %#v", names)
	}
	if len(doctorProbeFuncs(probes)) != 3 {
		t.Fatalf("probe funcs = %d", len(doctorProbeFuncs(probes)))
	}
}

func TestDoctorModelUpdatesRows(t *testing.T) {
	model := newDoctorModel("example.com", []string{"dns.lookup", "tcp.check"}, false, false, nil)
	pending := model.View().Content
	if !strings.Contains(pending, "dns.lookup") || strings.Contains(pending, "PASS") {
		t.Fatalf("pending view = %q", pending)
	}
	updated := doctorAfter(t, model, doctorResultMsg{name: "tcp.check", result: Result{Probe: "tcp.check", Status: StatusPass, Summary: "연결 성공"}})
	view := updated.View().Content
	if !strings.Contains(view, "PASS  tcp.check") || !strings.Contains(view, "연결 성공") {
		t.Fatalf("view = %q", view)
	}
	if updated.completed() != 1 {
		t.Fatalf("completed = %d", updated.completed())
	}
	if !strings.Contains(view, T("observe.doctor.completed", 1, 2)) {
		t.Fatalf("status line missing progress: %q", view)
	}
}

func TestDoctorModelPendingRowMatchesResultWidth(t *testing.T) {
	model := newDoctorModel("example.com", []string{"dns.lookup"}, false, false, nil)
	pending := strings.Split(model.View().Content, "\n")[0]
	done := strings.TrimRight(formatResultLine(Result{Probe: "dns.lookup", Status: StatusPass}, false), "\n")
	// byte 위치가 아니라 표시 폭으로 비교한다. spinner glyph는 여러 byte다.
	column := func(line string) int { return liveWidth(line[:strings.Index(line, "dns.lookup")]) }
	if column(pending) != column(done) {
		t.Fatalf("pending %q and done %q do not share the name column", pending, done)
	}
}

func TestDoctorModelRedactsAddresses(t *testing.T) {
	model := newDoctorModel("example.com", []string{"dns.lookup"}, false, true, nil)
	updated := doctorAfter(t, model, doctorResultMsg{name: "dns.lookup", result: Result{Probe: "dns.lookup", Status: StatusPass, Summary: "example.com → 192.0.2.10"}})
	view := updated.View().Content
	if strings.Contains(view, "192.0.2.10") || !strings.Contains(view, "<ip:") {
		t.Fatalf("view leaked the address: %q", view)
	}
}

func TestDoctorModelAddsUnknownProbe(t *testing.T) {
	model := newDoctorModel("example.com", []string{"dns.lookup"}, false, false, nil)
	updated := doctorAfter(t, model, doctorResultMsg{name: "net.quality", result: Result{Probe: "net.quality", Status: StatusSkip, Summary: "macOS 전용"}})
	if len(updated.rows) != 2 || updated.rows[1].name != "net.quality" {
		t.Fatalf("rows = %#v", updated.rows)
	}
	if !strings.Contains(updated.View().Content, "SKIP  net.quality") {
		t.Fatalf("view = %q", updated.View().Content)
	}
}

func TestDoctorModelFinishDropsStatusLine(t *testing.T) {
	model := newDoctorModel("example.com", []string{"dns.lookup"}, false, false, nil)
	model.now = func() time.Time { return model.started.Add(1200 * time.Millisecond) }
	running := model.View().Content
	if !strings.Contains(running, "1.2s") {
		t.Fatalf("status line elapsed = %q", running)
	}
	updated, cmd := model.Update(doctorDoneMsg{})
	final, ok := updated.(doctorModel)
	if !ok || !final.finished {
		t.Fatalf("model = %#v", updated)
	}
	if cmd == nil {
		t.Fatal("done must quit the program")
	}
	if strings.Contains(final.View().Content, T("observe.doctor.completed", 0, 1)) {
		t.Fatalf("final view kept the status line: %q", final.View().Content)
	}
}

func TestDoctorModelCancelsOnCtrlC(t *testing.T) {
	cancelled := false
	model := newDoctorModel("example.com", []string{"dns.lookup"}, false, false, func() { cancelled = true })
	first := doctorAfter(t, model, tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if !cancelled || !first.cancelling {
		t.Fatalf("first ctrl+c did not cancel: %v %#v", cancelled, first.cancelling)
	}
	if !strings.Contains(first.View().Content, T("observe.doctor.cancelling")) {
		t.Fatalf("view = %q", first.View().Content)
	}
	if _, cmd := first.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}); cmd == nil {
		t.Fatal("second ctrl+c must quit the program")
	}
}

func TestRunParallelWithObservesEachProbe(t *testing.T) {
	probes := []func(context.Context) Result{
		func(context.Context) Result { return Result{Probe: "b", Status: StatusPass} },
		func(context.Context) Result { return Result{Probe: "a", Status: StatusFail} },
	}
	seen := make(chan int, len(probes))
	results := runParallelWith(context.Background(), probes, func(index int, _ Result) { seen <- index })
	close(seen)
	counts := map[int]int{}
	for index := range seen {
		counts[index]++
	}
	if counts[0] != 1 || counts[1] != 1 {
		t.Fatalf("observe counts = %#v", counts)
	}
	if len(results) != 2 || results[0].Probe != "a" {
		t.Fatalf("results must stay sorted: %#v", results)
	}
}
