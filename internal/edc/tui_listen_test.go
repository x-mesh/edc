package edc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func listenBrowserFixture(t *testing.T) listenBrowser {
	t.Helper()
	model := newListenBrowser(context.Background(), time.Second, false, false)
	model.load = func(context.Context) Result {
		return Result{Status: StatusPass, Metrics: map[string]interface{}{"sockets": []listenSocket{
			{Proto: "tcp", Port: 8080, Address: "0.0.0.0", Process: "api-server", PID: "100"},
			{Proto: "tcp", Port: 5432, Address: "127.0.0.1", Process: "postgres", PID: "200"},
		}}}
	}
	model.inspect = func(_ context.Context, row listenRow) []string {
		return append([]string{"owner PID " + row.first().PID}, strings.Split(strings.Repeat("detail\n", 30), "\n")...)
	}
	next, cmd := model.Update(model.Init()())
	if cmd != nil {
		t.Fatal("manual browser scheduled an automatic update")
	}
	return next.(listenBrowser)
}

func listenBrowserKey(model listenBrowser, key string) (listenBrowser, tea.Cmd) {
	msg := topKey(key)
	if code, ok := map[string]rune{"pgup": tea.KeyPgUp, "pgdown": tea.KeyPgDown, "home": tea.KeyHome, "end": tea.KeyEnd}[key]; ok {
		msg = tea.KeyPressMsg{Code: code}
	}
	next, cmd := model.Update(msg)
	return next.(listenBrowser), cmd
}

func TestListenBrowserDetailsReturnToSameSelection(t *testing.T) {
	model := listenBrowserFixture(t)
	model, _ = listenBrowserKey(model, "down")
	model, cmd := listenBrowserKey(model, "enter")
	if model.detail == nil || model.detail.row.first().PID != "200" || cmd == nil {
		t.Fatal("Enter must inspect selected owner")
	}
	next, _ := model.Update(cmd())
	model = next.(listenBrowser)
	if !strings.Contains(model.View().Content, "owner PID 200") {
		t.Fatalf("detail: %s", model.View().Content)
	}
	model, _ = listenBrowserKey(model, "pgdown")
	if model.detail.offset == 0 {
		t.Fatal("details did not scroll")
	}
	model, reload := listenBrowserKey(model, "r")
	next, _ = model.Update(listenBrowserDetailMsg{seq: model.detail.seq - 1, lines: []string{"stale"}})
	model = next.(listenBrowser)
	if !model.detail.loading || reload == nil {
		t.Fatal("stale response replaced reloaded detail")
	}
	model, _ = listenBrowserKey(model, "esc")
	selected, _ := model.selected()
	if model.detail != nil || selected.PID != "200" {
		t.Fatal("Esc lost selection")
	}
	next, _ = model.Update(reload())
	if next.(listenBrowser).detail != nil {
		t.Fatal("late detail reopened the screen")
	}
	for _, key := range []string{"q", "ctrl+c"} {
		_, quit := listenBrowserKey(model, key)
		if quit == nil {
			t.Fatalf("%s did not quit", key)
		}
	}
}

func TestListenBrowserManualRefreshKeepsIdentityAndSnapshotOnError(t *testing.T) {
	model := listenBrowserFixture(t)
	model, _ = listenBrowserKey(model, "down")
	selected, _ := model.selected()
	model, cmd := listenBrowserKey(model, "r")
	if cmd == nil || !model.loading {
		t.Fatal("r must request a refresh")
	}
	if _, duplicate := listenBrowserKey(model, "r"); duplicate != nil {
		t.Fatal("duplicate refresh scheduled")
	}
	next, _ := model.Update(listenBrowserListMsg{seq: model.seq - 1, result: Result{Status: StatusPass}})
	model = next.(listenBrowser)
	if len(model.items) != 2 || !model.loading {
		t.Fatal("stale list replaced current snapshot")
	}
	next, automatic := model.Update(listenBrowserListMsg{seq: model.seq, result: Result{Status: StatusPass, Metrics: map[string]interface{}{"sockets": []listenSocket{selected, model.items[0].first()}}}, at: time.Now()})
	model = next.(listenBrowser)
	if model.cursor != 0 || automatic != nil {
		t.Fatal("refresh must retain socket identity without automatic reload")
	}
	model, cmd = listenBrowserKey(model, "r")
	next, _ = model.Update(listenBrowserListMsg{seq: model.seq, result: Result{Status: StatusFail, Summary: "permission denied"}})
	model = next.(listenBrowser)
	if len(model.items) != 2 || !strings.Contains(model.View().Content, "permission denied") || model.loading {
		t.Fatal("refresh failure must preserve previous snapshot and permit retry")
	}
	model, cmd = listenBrowserKey(model, "r")
	if cmd == nil {
		t.Fatal("retry unavailable")
	}
	next, _ = model.Update(listenBrowserListMsg{seq: model.seq, result: Result{Status: StatusPass, Metrics: map[string]interface{}{"sockets": []listenSocket{}}}})
	if model = next.(listenBrowser); len(model.items) != 0 || !strings.Contains(model.View().Content, T("listenbrowse.empty")) {
		t.Fatal("empty state missing")
	}
	if _, cmd = listenBrowserKey(model, "enter"); cmd != nil {
		t.Fatal("empty list opens detail")
	}
}

func TestListenBrowserResizeUnicodeAndRedaction(t *testing.T) {
	model := listenBrowserFixture(t)
	model.redact = true
	for i := 0; i < 60; i++ {
		model.sockets = append(model.sockets, listenSocket{Proto: "unix", Address: "/tmp/서비스.sock", Process: "프로세스🙂", PID: fmt.Sprint(4194245 + i)})
	}
	model.rebuild()
	model, _ = listenBrowserKey(model, "end")
	for _, color := range []bool{false, true} {
		t.Setenv("NO_COLOR", "")
		model.color = color
		for _, size := range [][2]int{{140, 24}, {80, 12}, {48, 8}, {20, 3}, {80, 12}} {
			next, _ := model.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			model = next.(listenBrowser)
			text := model.View().Content
			if size[0] >= 48 && !strings.Contains(text, "4194304") {
				t.Fatalf("selected PID was truncated: %s", text)
			}
			if strings.Contains(text, "\x1b") && !color {
				t.Fatal("NO_COLOR output contains escapes")
			}
			if len(strings.Split(text, "\n")) > size[1] {
				t.Fatalf("height exceeded: %s", text)
			}
			for _, line := range strings.Split(text, "\n") {
				if ansi.StringWidth(line) > size[0] {
					t.Fatalf("width exceeded: %q", line)
				}
			}
		}
	}
	model.cursor = 0
	text := model.View().Content
	if strings.Contains(text, "0.0.0.0") {
		t.Fatal("list leaked address")
	}
	model.detail = &listenBrowserDetail{row: model.items[0], lines: []string{"peer 192.0.2.1 " + strings.Repeat("한글", 1000)}}
	if text = model.View().Content; strings.Contains(text, "192.0.2.1") || strings.Contains(text, "0.0.0.0") {
		t.Fatal("detail leaked address")
	}
	model, _ = listenBrowserKey(model, "end")
	if model.detail.offset == 0 {
		t.Fatal("wrapped detail cannot scroll")
	}
}

func TestListenBrowseRejectsIncompatibleModesBeforeWritingJSON(t *testing.T) {
	for _, flag := range []string{"-b", "--browse"} {
		for _, extra := range [][]string{{"--watch"}, {"--json", filepath.Join(t.TempDir(), "report.json")}, {"--duration", "1s"}, {"-i", "1"}} {
			if code := runListen(append([]string{flag}, extra...), "test"); code != 2 {
				t.Fatalf("%s %v: exit %d", flag, extra, code)
			}
			if len(extra) > 1 && extra[0] == "--json" {
				if _, err := os.Stat(extra[1]); !os.IsNotExist(err) {
					t.Fatal("invalid mode wrote a JSON file")
				}
			}
		}
	}
	stdin, stdout := os.Stdin, os.Stdout
	file, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	os.Stdin, os.Stdout = file, file
	defer func() { os.Stdin, os.Stdout = stdin, stdout }()
	if code := runListen([]string{"-b"}, "test"); code != 2 {
		t.Fatalf("non-TTY browse exit %d", code)
	}
}

func TestCollectProcResultsHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := collectProcResults(ctx, "1", true); err != context.Canceled {
		t.Fatalf("error: %v", err)
	}
}

func TestListenSocketWithoutOwnerStillHasDetails(t *testing.T) {
	lines := inspectListenSocket(context.Background(), listenRow{Sockets: []listenSocket{{Proto: "tcp", Address: "127.0.0.1", Port: 80}}})
	text := strings.Join(lines, "\n")
	if !strings.Contains(text, "127.0.0.1") || !strings.Contains(text, T("listenbrowse.no_owner")) {
		t.Fatalf("details: %s", text)
	}
}
