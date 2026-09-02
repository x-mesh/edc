package edc

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func viewerFixture(t *testing.T) viewerModel {
	t.Helper()
	results := []Result{
		{Probe: "dns.lookup", Status: StatusPass, Summary: "ok"},
		{Probe: "tls.check", Status: StatusWarn, Summary: "만료 임박", Warnings: []string{"인증서 만료까지 3일 남았습니다"}},
		{Probe: "http.check", Status: StatusFail, Summary: "HTTP 503", Error: &DiagnosticError{Kind: "status", Message: "HTTP 503"}},
	}
	model := newViewerModel("report show a.json", resultEntries(results, false), resultFilters())
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return updated.(viewerModel)
}

func viewerAfter(t *testing.T, model viewerModel, keys ...string) viewerModel {
	t.Helper()
	var current tea.Model = model
	for _, key := range keys {
		current, _ = current.Update(tea.KeyPressMsg{Code: rune(key[0]), Text: key})
	}
	updated, ok := current.(viewerModel)
	if !ok {
		t.Fatalf("model = %#v", current)
	}
	return updated
}

func TestViewerFilterCyclesThroughStatuses(t *testing.T) {
	model := viewerFixture(t)
	if !strings.Contains(model.body(), "dns.lookup") {
		t.Fatalf("default filter must show every probe: %q", model.body())
	}
	problems := viewerAfter(t, model, "f")
	if strings.Contains(problems.body(), "dns.lookup") || !strings.Contains(problems.body(), "tls.check") {
		t.Fatalf("second filter = %q", problems.body())
	}
	failures := viewerAfter(t, problems, "f")
	if strings.Contains(failures.body(), "tls.check") || !strings.Contains(failures.body(), "http.check") {
		t.Fatalf("third filter = %q", failures.body())
	}
	if back := viewerAfter(t, failures, "f"); !strings.Contains(back.body(), "dns.lookup") {
		t.Fatalf("filter must cycle back: %q", back.body())
	}
}

func TestViewerExpandShowsDetail(t *testing.T) {
	model := viewerFixture(t)
	if strings.Contains(model.body(), "인증서 만료까지") {
		t.Fatalf("collapsed body must hide the detail: %q", model.body())
	}
	expanded := viewerAfter(t, model, "e")
	for _, expected := range []string{"인증서 만료까지 3일 남았습니다", "┌─ ERROR  http.check", "│ cause   HTTP 503"} {
		if !strings.Contains(expanded.body(), expected) {
			t.Fatalf("expanded body %q does not contain %q", expanded.body(), expected)
		}
	}
	if !strings.Contains(expanded.help(), "상세 펼침") {
		t.Fatalf("help = %q", expanded.help())
	}
}

func TestViewerReportsEmptyFilter(t *testing.T) {
	results := []Result{{Probe: "dns.lookup", Status: StatusPass, Summary: "ok"}}
	model := newViewerModel("report show a.json", resultEntries(results, false), resultFilters())
	failures := viewerAfter(t, viewerAfter(t, model, "f"), "f")
	if !strings.Contains(failures.body(), "해당하는 항목이 없습니다") {
		t.Fatalf("body = %q", failures.body())
	}
}

func TestViewerUsesAltScreenAndQuits(t *testing.T) {
	model := viewerFixture(t)
	if !model.View().AltScreen {
		t.Fatal("viewer must use the alt screen")
	}
	if !strings.Contains(model.View().Content, "report show a.json") {
		t.Fatalf("view = %q", model.View().Content)
	}
	for _, key := range []tea.KeyPressMsg{{Code: 'q', Text: "q"}, {Code: tea.KeyEscape}, {Code: 'c', Mod: tea.ModCtrl}} {
		if _, cmd := model.Update(key); cmd == nil {
			t.Fatalf("%s must quit the viewer", key.String())
		}
	}
}

func TestDiffEntriesSplitLineAndMetrics(t *testing.T) {
	diff := reportDiff{
		Before: reportDiffSide{Path: "a.json", StartedAt: time.Now()},
		After:  reportDiffSide{Path: "b.json", StartedAt: time.Now()},
		Entries: []reportDiffEntry{
			{Probe: "tcp.check", Change: changeChanged, Regressed: true, BeforeStatus: StatusPass, AfterStatus: StatusFail, AfterSummary: "timeout", Metrics: []metricDelta{{Key: "connect_ms", Before: 10, After: 900, Delta: floatPointer(890)}}},
			{Probe: "dns.lookup", Change: changeSame, BeforeStatus: StatusPass, AfterStatus: StatusPass},
		},
	}
	entries := diffEntries(diff, false)
	if len(entries) != 2 {
		t.Fatalf("entries = %#v", entries)
	}
	if !strings.Contains(entries[0].line, "WORSE") || strings.Contains(entries[0].line, "connect_ms") {
		t.Fatalf("line = %q", entries[0].line)
	}
	if len(entries[0].detail) != 1 || !strings.Contains(entries[0].detail[0], "connect_ms") {
		t.Fatalf("detail = %#v", entries[0].detail)
	}
	if !entries[0].keep[viewerFilterWorse] || entries[1].keep[viewerFilterWorse] {
		t.Fatalf("worse filter = %#v", entries)
	}
	if !entries[0].keep[viewerFilterChanged] || entries[1].keep[viewerFilterChanged] {
		t.Fatalf("changed filter = %#v", entries)
	}
}
