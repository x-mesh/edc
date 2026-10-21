package edc

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func historyTopSamples(count int) []historyTopSample {
	samples := make([]historyTopSample, count)
	for i := range samples {
		samples[i] = historyTopSample{
			RunID: "run-1", System: "darwin", CPU: 23.5, WindowS: 1,
			NetworkValid: true, DiskValid: true, SwapValid: true,
			topSample: topSample{Time: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC).Add(-time.Duration(i) * time.Second), Hostname: "fixture", Cores: 8,
				MemoryPct: 74, CPUUser: 20, CPUSystem: 3.5, DiskRead: 1024, DiskWrite: 2048, NetIn: 4096, NetOut: 8192},
		}
	}
	return samples
}

func historyTopAfter(t *testing.T, model historySampleBrowser, messages ...tea.Msg) historySampleBrowser {
	t.Helper()
	for _, message := range messages {
		next, _ := model.Update(message)
		model = next.(historySampleBrowser)
	}
	return model
}

func TestHistoryTopBrowserFollowsAndPreservesSelection(t *testing.T) {
	model := newHistoryTopBrowser(historyTopSamples(100), "history.db")
	prepend := func() {
		newest := model.samples[0]
		newest.Time = newest.Time.Add(time.Second)
		samples := append([]historyTopSample{newest}, model.samples...)
		model = historyTopAfter(t, model, historySamplesLoadedMsg{seq: model.seq, samples: samples})
	}
	prepend()
	if model.selected != 0 || model.pinned {
		t.Fatal("untouched selection did not follow the latest sample")
	}
	model = historyTopAfter(t, model, tea.KeyPressMsg{Code: tea.KeyDown})
	selected := model.samples[model.selected]
	prepend()
	if model.selected != 2 || !model.samples[model.selected].Time.Equal(selected.Time) {
		t.Fatal("refresh did not preserve the manually selected observation")
	}
	model = historyTopAfter(t, model, tea.KeyPressMsg{Code: tea.KeyEnter})
	prepend()
	if !model.expanded || !model.samples[model.selected].Time.Equal(selected.Time) {
		t.Fatal("expanded details changed observation during refresh")
	}
	model = historyTopAfter(t, model, tea.KeyPressMsg{Code: tea.KeyEscape}, tea.KeyPressMsg{Code: tea.KeyHome})
	prepend()
	if model.expanded || model.pinned || model.selected != 0 || model.view.YOffset() != 0 {
		t.Fatal("Home did not resume following")
	}
	model = historyTopAfter(t, model, tea.KeyPressMsg{Code: tea.KeyEnd})
	if model.selected != len(model.samples)-1 || model.view.YOffset() == 0 {
		t.Fatal("End did not scroll to the oldest loaded sample")
	}
	model = historyTopAfter(t, model, historySamplesLoadedMsg{seq: model.seq, samples: historyTopSamples(1)})
	if model.selected != 0 || model.expanded || model.pinned {
		t.Fatal("expired selection was not reset safely")
	}
	model = historyTopAfter(t, model, historySamplesLoadedMsg{seq: model.seq})
	if !strings.Contains(model.View().Content, "edc top") {
		t.Fatal("empty history does not explain how to record")
	}
}

func TestHistorySampleBrowserProcessFilterReloadsAndClears(t *testing.T) {
	model := newHistoryTopBrowser(historyTopSamples(1), "history.db")
	var got []historyOptions
	model.loadWith = func(options historyOptions) ([]historyTopSample, error) {
		got = append(got, options)
		return historyTopSamples(1), nil
	}
	model.options.process, _ = parseTopProcessFilter("old")
	model = historyTopAfter(t, model, tea.KeyPressMsg{Code: '/'}, tea.KeyPressMsg{Code: tea.KeyBackspace}, tea.KeyPressMsg{Code: tea.KeyBackspace}, tea.KeyPressMsg{Code: tea.KeyBackspace})
	next, command := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = next.(historySampleBrowser)
	if command != nil {
		model = historyTopAfter(t, model, command())
	}
	if model.filterEditing || model.options.process.active() || len(got) != 1 || got[0].process.active() {
		t.Fatalf("filter clear = editing:%t options:%q reloads:%+v", model.filterEditing, model.options.process, got)
	}
	model = historyTopAfter(t, model, tea.KeyPressMsg{Code: '/'}, tea.KeyPressMsg{Code: 'n', Text: "n"}, tea.KeyPressMsg{Code: 'g', Text: "g"}, tea.KeyPressMsg{Code: 'i', Text: "i"}, tea.KeyPressMsg{Code: 'n', Text: "n"}, tea.KeyPressMsg{Code: 'x', Text: "x"})
	next, command = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = next.(historySampleBrowser)
	if command != nil {
		model = historyTopAfter(t, model, command())
	}
	if model.options.process.String() != "nginx" || len(got) != 2 || got[1].process.String() != "nginx" {
		t.Fatalf("filter reload = options:%q reloads:%+v", model.options.process, got)
	}
}

func TestHistoryTopBrowserLayoutAndAvailability(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	for _, language := range []string{"en", "ko", "ja"} {
		setLanguage(language)
		for _, color := range []bool{false, true} {
			model := newHistoryTopBrowser(historyTopSamples(100), "history.db")
			model.color = color
			for _, size := range []tea.WindowSizeMsg{{Width: 40, Height: 10}, {Width: 80, Height: 24}, {Width: 132, Height: 16}, {Width: 160, Height: 50}, {Width: 240, Height: 60}} {
				model = historyTopAfter(t, model, size)
				view := model.View().Content
				lines := strings.Split(view, "\n")
				if len(lines) != size.Height || !strings.Contains(lines[len(lines)-1], "q") {
					t.Fatalf("invalid height or missing quit hint at %s/%+v: %q", language, size, view)
				}
				for _, line := range lines {
					if liveWidth(line) > size.Width || !color && strings.Contains(line, "\x1b[") {
						t.Fatalf("oversized/colored plain line: %q", line)
					}
				}
				if strings.Contains(view, T("history.top_browser.details")) != model.split() {
					t.Fatal("detail panel breakpoint is wrong")
				}
				if size.Width >= 160 && (!strings.Contains(view, "NET IN/s") || !strings.Contains(view, "SWAP OUT/s")) {
					t.Fatal("wide layout did not expose network and swap")
				}
			}
		}
	}
	sample := historyTopSamples(1)[0]
	for _, width := range []int{40, 48, 80, 96, 120, 160, 240} {
		columns := historyTopColumns(width, []historyTopSample{sample})
		header := historyTopTableLine(columns, nil, false, false)
		row := historyTopTableLine(columns, &sample, true, false)
		if liveWidth(header) != liveWidth(row) || liveWidth(row) > width || strings.Contains(header, "IOWAIT") || strings.Contains(header, "PSI") {
			t.Fatalf("column alignment or unsupported metrics at %d: %q / %q", width, header, row)
		}
	}
	sample.NetworkValid = false
	if historyTopMetricText(sample, "net_in_bytes_per_s") != "—" || historyTopMetricText(sample, "swap_out_bytes_per_s") != "0 B/s" {
		t.Fatal("unavailable and measured zero are not distinguished")
	}
}

func TestHistoryTopBrowserRefreshPauseAndFailure(t *testing.T) {
	model := newHistoryTopBrowser(historyTopSamples(2), "history.db")
	model.load = func() ([]historyTopSample, error) { return nil, errors.New("temporary error") }
	if model.Init() == nil {
		t.Fatal("no automatic refresh")
	}
	model = historyTopAfter(t, model, tea.KeyPressMsg{Code: 'p', Text: "p"})
	if _, cmd := model.Update(historySamplesTickMsg{seq: model.seq}); cmd != nil || !model.paused {
		t.Fatal("paused viewer reloaded")
	}
	model = historyTopAfter(t, model, historySamplesLoadedMsg{seq: model.seq - 1})
	if len(model.samples) != 2 {
		t.Fatal("stale refresh replaced samples")
	}
	next, cmd := model.Update(tea.KeyPressMsg{Code: 'r', Text: "r"})
	if cmd == nil {
		t.Fatal("manual refresh unavailable while paused")
	}
	model = historyTopAfter(t, next.(historySampleBrowser), cmd())
	if len(model.samples) != 2 || model.notice == "" || !model.paused {
		t.Fatal("reload error lost observations or resumed refresh")
	}
}

func TestHistoryTopLoaderAndPrintedColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	recorder := topHistoryFixture(t, path)
	for i := 1; i <= 3; i++ {
		if err := recorder.Record(historyFixtureSample(i, float64(i*10), topProcess{})); err != nil {
			t.Fatal(err)
		}
	}
	if err := recorder.Close("finished"); err != nil {
		t.Fatal(err)
	}
	db, err := openHistoryDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	min := 20.0
	samples, err := loadHistoryTop(db, historyOptions{limit: 1, run: recorder.runID, metric: "cpu_pct", min: &min})
	if err != nil || len(samples) != 1 || samples[0].CPU != 30 {
		t.Fatalf("query options lost: samples=%+v err=%v", samples, err)
	}
	var output strings.Builder
	if err := printHistory(&output, db, "top", historyOptions{limit: 1}, false); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"DISK READ/s", "DISK WRITE/s", "NET IN/s", "NET OUT/s", "SWAP OUT/s"} {
		if !strings.Contains(output.String(), column) {
			t.Fatalf("missing printed column %s: %s", column, output.String())
		}
	}
}
