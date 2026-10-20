package edc

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func historyProcessSamples(count int) []historyProcessSample {
	started := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	memory, read, write := uint64(8192), 0.0, 2048.0
	samples := make([]historyProcessSample, count)
	for i := range samples {
		samples[i] = historyProcessSample{
			topProcessSample: topProcessSample{PID: 100 + i%3, Started: &started, Command: "Chrome helper", CPU: 125, RSSBytes: 4096, MemoryBytes: &memory, Threads: 12, FDs: 42, DiskRead: &read, DiskWrite: &write},
			RunID:            "run-1", Time: started.Add(time.Hour - time.Duration(i/3)*time.Second), Hostname: "fixture", WindowS: 1,
		}
	}
	return samples
}

func TestHistoryProcessBrowserSelectionIdentityAndFollowing(t *testing.T) {
	model := newHistoryProcessBrowser(historyProcessSamples(30), "history.db")
	prepend := func() {
		newest := model.processes[0]
		newest.Time = newest.Time.Add(time.Second)
		model = historyTopAfter(t, model, historySamplesLoadedMsg{seq: model.seq, processes: append([]historyProcessSample{newest}, model.processes...)})
	}
	prepend()
	if model.selected != 0 || model.pinned {
		t.Fatal("untouched selection did not follow new observations")
	}
	model = historyTopAfter(t, model, tea.KeyPressMsg{Code: tea.KeyDown}, tea.KeyPressMsg{Code: tea.KeyDown})
	identity := model.identity(model.selected)
	prepend()
	if model.selected != 3 || model.identity(model.selected) != identity {
		t.Fatal("refresh confused processes observed at the same time")
	}
	model = historyTopAfter(t, model, tea.KeyPressMsg{Code: tea.KeyEnter})
	prepend()
	if !model.expanded || model.identity(model.selected) != identity {
		t.Fatal("expanded process details changed during refresh")
	}
	model = historyTopAfter(t, model, tea.KeyPressMsg{Code: tea.KeyEscape}, tea.KeyPressMsg{Code: tea.KeyHome})
	prepend()
	if model.selected != 0 || model.pinned || model.view.YOffset() != 0 {
		t.Fatal("Home did not resume following")
	}
	model = historyTopAfter(t, model, tea.KeyPressMsg{Code: tea.KeyDown}, tea.KeyPressMsg{Code: tea.KeyEnter})
	reused := model.processes[model.selected]
	restarted := reused.Started.Add(time.Minute)
	reused.Started = &restarted
	model = historyTopAfter(t, model, historySamplesLoadedMsg{seq: model.seq, processes: []historyProcessSample{reused}})
	if model.expanded || model.pinned || model.selected != 0 {
		t.Fatal("a reused PID was treated as the selected process")
	}
	model = historyTopAfter(t, model, historySamplesLoadedMsg{seq: model.seq})
	if model.count() != 0 || !strings.Contains(model.View().Content, "edc top") {
		t.Fatal("empty process history is unsafe or missing recording guidance")
	}
}

func TestHistoryProcessBrowserLayoutDetailsAndAvailability(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	for _, language := range []string{"en", "ko", "ja"} {
		setLanguage(language)
		for _, color := range []bool{false, true} {
			model := newHistoryProcessBrowser(historyProcessSamples(100), "history.db")
			model.processes[0].Command = "Chrome 秘密\n\x1b[2J" + strings.Repeat(" long-command", 20)
			model.color = color
			for _, size := range []tea.WindowSizeMsg{{Width: 40, Height: 10}, {Width: 60, Height: 16}, {Width: 80, Height: 24}, {Width: 160, Height: 40}, {Width: 240, Height: 60}} {
				model = historyTopAfter(t, model, size)
				view := model.View().Content
				lines := strings.Split(view, "\n")
				if len(lines) != size.Height || !strings.Contains(lines[len(lines)-1], "q") || strings.Contains(view, "\x1b[2J") {
					t.Fatalf("unsafe layout at %s/%+v: %q", language, size, view)
				}
				for _, line := range lines {
					if liveWidth(line) > size.Width || !color && strings.Contains(line, "\x1b[") {
						t.Fatalf("oversized/colored plain line: %q", line)
					}
				}
				if (!strings.Contains(view, "COMMAND") && !strings.Contains(view, "CMD")) || !strings.Contains(view, "PID") || !strings.Contains(view, "RSS") {
					t.Fatal("required process columns missing")
				}
				if size.Width >= 160 && (!strings.Contains(view, "READ/s") || !strings.Contains(view, "WRITE/s")) {
					t.Fatal("wide process layout did not expose I/O")
				}
			}
			model = historyTopAfter(t, model, tea.KeyPressMsg{Code: tea.KeyEnter})
			if !model.expanded || !strings.Contains(model.View().Content, "125.0%") || !strings.Contains(model.View().Content, "8.0 KB") {
				t.Fatal("process details lost CPU or physical memory")
			}
		}
	}
	samples := historyProcessSamples(1)
	for _, width := range []int{40, 48, 60, 71, 72, 80, 100, 160, 240} {
		columns := historyProcessColumns(width, samples)
		header := historyProcessTableLine(columns, samples, -1, false, false)
		row := historyProcessTableLine(columns, samples, 0, true, false)
		if liveWidth(header) != width || liveWidth(row) != width {
			t.Fatalf("columns do not fit width %d: %q / %q", width, header, row)
		}
	}
	zero := samples[0]
	if historyProcessCell(zero, "read") != "0 B/s" {
		t.Fatal("measured zero I/O was lost")
	}
	zero.DiskRead, zero.MemoryBytes = nil, nil
	if historyProcessCell(zero, "read") != "—" || historyProcessCell(zero, "memory") != "—" || historyProcessCell(zero, "rss") != "4.0 KB" {
		t.Fatal("unavailable memory/I/O confused with zero or RSS")
	}
}

func TestHistoryProcessLoaderPreservesFiltersAndPayload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	recorder := topHistoryFixture(t, path)
	for i, name := range []string{"Chrome helper", "Safari"} {
		process := topProcess{PID: 10 + i, Command: name, CPU: 120, RSS: 4096, Memory: 8192, MemoryValid: true, DiskValid: true, DiskWrite: 1024}
		if err := recorder.Record(historyFixtureSample(i+1, 20, process)); err != nil {
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
	filter, _ := parseTopProcessFilter("Chrome")
	minimum := 1024.0
	samples, err := loadHistoryProcess(db, historyOptions{run: recorder.runID, limit: 1, process: filter, metric: "rss_bytes", min: &minimum})
	if err != nil || len(samples) != 1 || samples[0].Command != "Chrome helper" || samples[0].MemoryBytes == nil || *samples[0].MemoryBytes != 8192 || samples[0].DiskWrite == nil {
		t.Fatalf("process query lost filters or data: %+v err=%v", samples, err)
	}
	model := newHistoryProcessBrowser(samples, path)
	model.loadProcesses = func() ([]historyProcessSample, error) { return nil, errors.New("temporary error") }
	if model.Init() == nil {
		t.Fatal("process history does not refresh automatically")
	}
	next, cmd := model.Update(tea.KeyPressMsg{Code: 'r', Text: "r"})
	if cmd == nil {
		t.Fatal("manual refresh unavailable")
	}
	model = historyTopAfter(t, next.(historySampleBrowser), cmd())
	if model.count() != 1 || model.notice == "" {
		t.Fatal("refresh error lost process observations")
	}
}

func TestHistoryProcessTableShowsAppNameAndDetailsKeepFullCommand(t *testing.T) {
	samples := historyProcessSamples(1)
	samples[0].Command = "/Applications/Aside.app/Contents/MacOS/Aside Helper"
	model := newHistoryProcessBrowser(samples, "history.db")
	model.color = false
	columns := historyProcessColumns(80, samples)
	row := historyProcessTableLine(columns, samples, 0, true, false)
	if !strings.Contains(row, "Aside Helper") || strings.Contains(row, "/Applications") {
		t.Fatalf("table does not identify the app: %q", row)
	}
	if details := strings.Join(model.detailLines(200), "\n"); !strings.Contains(details, samples[0].Command) {
		t.Fatal("details lost the full command path")
	}
}

func TestHistoryDetailWrappingDoesNotColorFollowingRows(t *testing.T) {
	processes := historyProcessSamples(2)
	processes[0].Command = "/Applications/" + strings.Repeat("Long App Name/", 12) + "Helper"
	hosts := historyTopSamples(2)
	hosts[0].Hostname = strings.Repeat("long-host-name-", 12)
	for _, model := range []historySampleBrowser{newHistoryProcessBrowser(processes, "history.db"), newHistoryTopBrowser(hosts, "history.db")} {
		model.color = true
		lines := model.detailLines(42)
		colored := 0
		for _, line := range lines {
			if strings.Contains(line, "\x1b[") {
				colored++
				if !strings.HasSuffix(line, richReset) {
					t.Fatalf("wrapped detail line leaks its style: %q", line)
				}
			}
		}
		if colored < 2 {
			t.Fatal("fixture did not wrap the colored title")
		}
	}
}
