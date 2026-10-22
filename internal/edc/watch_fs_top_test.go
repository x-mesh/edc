package edc

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestFSWatchTopRanksDirectoriesAndSkipsAShortRun(t *testing.T) {
	top := newFSWatchTop()
	for index := range 6 {
		top.add(fmt.Sprintf("logs/f%d", index))
	}
	if rows := top.rows(); rows != nil {
		t.Fatalf("a run of %d events has a table: %+v", top.total, rows)
	}
	for range 6 {
		top.add("etc/app.conf")
	}
	for range 3 {
		top.add("top.txt")
	}
	rows := top.rows()
	want := []fsWatchTopRow{{"logs", 6, 40}, {"etc", 6, 40}, {".", 3, 20}}
	// 같은 횟수는 이름 순서이므로 etc가 logs보다 앞이다.
	want[0], want[1] = want[1], want[0]
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v", rows)
	}
	for index := range want {
		if rows[index] != want[index] {
			t.Errorf("row %d = %+v, want %+v", index, rows[index], want[index])
		}
	}
}

func TestFSWatchTopShowsAtMostFiveDirectories(t *testing.T) {
	top := newFSWatchTop()
	for index := range 8 {
		for range 3 + index {
			top.add(fmt.Sprintf("dir%d/file", index))
		}
	}
	rows := top.rows()
	if len(rows) != fsWatchTopShown || rows[0].Directory != "dir7" || rows[0].Events != 10 {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestFSWatchTopMemoryStaysBounded(t *testing.T) {
	top := newFSWatchTop()
	for index := range fsWatchTopMaxDirectories + 500 {
		top.add(fmt.Sprintf("d%d/f", index))
	}
	// 이미 센 디렉터리는 한도 뒤에도 계속 센다.
	top.add("d0/another")
	if len(top.counts) != fsWatchTopMaxDirectories || top.other != 500 || top.counts["d0"] != 2 || top.total != fsWatchTopMaxDirectories+501 {
		t.Errorf("directories %d, other %d, d0 %d, total %d", len(top.counts), top.other, top.counts["d0"], top.total)
	}
}

func TestFSWatchTopTextIsTranslated(t *testing.T) {
	previous := currentLanguage()
	defer setLanguage(previous)
	rows := []fsWatchTopRow{{"root/.aic/log-checkpoints", 150, 77}, {"etc", 12, 6}}
	for language, heading := range map[string]string{"en": "most active directories:", "ko": "이벤트가 많은 디렉터리:", "ja": "イベントが多いディレクトリ:"} {
		setLanguage(language)
		text := fsWatchTopText(rows)
		if !strings.HasPrefix(text, heading+"\n") || !strings.Contains(text, `     150   77%  "root/.aic/log-checkpoints"`) || !strings.Contains(text, `      12    6%  "etc"`) {
			t.Errorf("%s: text = %q", language, text)
		}
	}
	if fsWatchTopText(nil) != "" {
		t.Error("an empty table has text")
	}
}

// 요약 줄 아래에 표가 붙는 것은 묶어서 보일 때뿐이고, 이벤트가 적게 끝난 감시에는 붙지 않는다.
func TestFSWatchStreamPrintsTheTopDirectoriesAfterTheSummary(t *testing.T) {
	previous := currentLanguage()
	defer setLanguage(previous)
	setLanguage("en")
	run := func(smart bool, busy, quiet int) string {
		ctx, cancel := context.WithCancel(context.Background())
		events := make(chan fsWatchEvent, 64)
		output := &fsSmartSyncBuffer{}
		options := fsWatchOptions{root: ".", events: []string{"modify"}, match: "**", smart: smart}
		done := make(chan struct{})
		go func() {
			streamFSWatch(ctx, output, fsWatchSource{events: events, errors: make(chan error)}, options, nil)
			close(done)
		}()
		for index := range busy {
			events <- fsWatchEvent{Time: time.Now(), Event: "modify", Path: fmt.Sprintf("busy/f%d", index)}
		}
		for index := range quiet {
			events <- fsWatchEvent{Time: time.Now(), Event: "modify", Path: fmt.Sprintf("quiet/f%d", index)}
		}
		time.Sleep(60 * time.Millisecond)
		cancel()
		<-done
		return output.String()
	}
	text := run(true, 9, 3)
	summary := strings.Index(text, "12 events")
	table := strings.Index(text, "most active directories:")
	if summary < 0 || table < summary || !strings.Contains(text, `9   75%  "busy"`) || !strings.Contains(text, `3   25%  "quiet"`) {
		t.Errorf("smart output lacks the table after the summary:\n%s", text)
	}
	if text = run(false, 9, 3); strings.Contains(text, "most active directories") {
		t.Errorf("--no-smart printed the table:\n%s", text)
	}
	if text = run(true, 4, 3); strings.Contains(text, "most active directories") {
		t.Errorf("a short run printed the table:\n%s", text)
	}
}
