package edc

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// fsRepeatScreen은 출력 바이트를 터미널처럼 해석해 화면에 남는 줄을 돌려준다. \n은 줄 맨 앞으로 간다(ONLCR). 쓰는 것은 글자, \r,
// \n, ESC[nA(위로), ESC[J(커서부터 화면 끝까지 지움), ESC[2K(줄 지움)이고 SGR(색)은 무시한다.
func fsRepeatScreen(data string) []string {
	lines := [][]rune{{}}
	row, col := 0, 0
	put := func(r rune) {
		for len(lines[row]) <= col {
			lines[row] = append(lines[row], ' ')
		}
		lines[row][col] = r
		col++
	}
	runes := []rune(data)
	for index := 0; index < len(runes); index++ {
		switch r := runes[index]; {
		case r == '\r':
			col = 0
		case r == '\n':
			row, col = row+1, 0
			for len(lines) <= row {
				lines = append(lines, []rune{})
			}
		case r == 0x1b && index+1 < len(runes) && runes[index+1] == '[':
			end := index + 2
			for end < len(runes) && (runes[end] >= '0' && runes[end] <= '9' || runes[end] == ';' || runes[end] == '?') {
				end++
			}
			params, final := string(runes[index+2:end]), runes[end]
			index = end
			count := 1
			if params != "" {
				fmt.Sscanf(params, "%d", &count)
			}
			switch final {
			case 'A':
				row = max(0, row-count)
			case 'J':
				lines[row] = lines[row][:min(col, len(lines[row]))]
				lines = lines[:row+1]
			case 'K':
				lines[row] = lines[row][:0]
			}
		default:
			put(r)
		}
	}
	text := make([]string, 0, len(lines))
	for _, line := range lines {
		text = append(text, strings.TrimRight(string(line), " "))
	}
	return text
}

func TestFSRepeatScreenBehavesLikeATerminal(t *testing.T) {
	// 상태줄(status)을 지우고 한 줄 올라가 two를 지운 뒤 new를 쓰고 상태줄을 다시 그린다.
	got := fsRepeatScreen("one\ntwo\nstatus\r\x1b[2K\x1b[1A\r\x1b[Jnew\nstatus")
	if want := []string{"one", "new", "status"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("screen = %q, want %q", got, want)
	}
}

func repeatRecord(kind, path, origin string, at time.Time, count int) fsWatchRecord {
	return fsWatchRecord{Type: "event", Time: at, Event: kind, Path: path, Origin: origin, Count: count}
}

func TestFSWatchRepeaterFoldsOnlyAnIdenticalLineRightAfterIt(t *testing.T) {
	start := time.Date(2026, 10, 22, 17, 27, 0, 0, time.UTC)
	wide := func() int { return 200 }
	repeater := newFSWatchRepeater(wide, textStyle{})
	first, rows := repeater.next(repeatRecord("create", "d/final", "tmp", start, 0))
	if rows != 0 || first.Count != 0 {
		t.Fatalf("the first line folded: rows %d %+v", rows, first)
	}
	merged, rows := repeater.next(repeatRecord("create", "d/final", "tmp", start.Add(time.Second), 0))
	if rows != 1 || merged.Count != 2 || !merged.Time.Equal(start.Add(time.Second)) {
		t.Fatalf("second: rows %d %+v", rows, merged)
	}
	if merged, rows = repeater.next(repeatRecord("create", "d/final", "tmp", start.Add(2*time.Second), 0)); rows != 1 || merged.Count != 3 {
		t.Fatalf("third: rows %d %+v", rows, merged)
	}
	// 다른 경로, 다른 출처, 다른 종류는 새 줄이고, 그 뒤에는 앞의 줄과 이어지지 않는다.
	for name, other := range map[string]fsWatchRecord{
		"another path":   repeatRecord("create", "d/other", "tmp", start.Add(3*time.Second), 0),
		"another origin": repeatRecord("create", "d/final", "tmp2", start.Add(3*time.Second), 0),
		"another kind":   repeatRecord("modify", "d/final", "tmp", start.Add(3*time.Second), 0),
	} {
		repeater.reset()
		repeater.next(repeatRecord("create", "d/final", "tmp", start, 0))
		if _, rows := repeater.next(other); rows != 0 {
			t.Errorf("%s folded", name)
		}
	}
	// 직전 줄과 같아도 간격이 길면 새 줄이다.
	repeater.reset()
	repeater.next(repeatRecord("create", "d/final", "tmp", start, 0))
	if _, rows := repeater.next(repeatRecord("create", "d/final", "tmp", start.Add(fsWatchRepeatGap+time.Second), 0)); rows != 0 {
		t.Error("a repeat after a long gap folded")
	}
	// reset 뒤에는 같아도 이어지지 않는다.
	repeater.next(repeatRecord("create", "d/final", "tmp", start, 0))
	repeater.reset()
	if _, rows := repeater.next(repeatRecord("create", "d/final", "tmp", start.Add(time.Second), 0)); rows != 0 {
		t.Error("a line folded into one that was forgotten")
	}
}

func TestFSWatchRepeaterAddsTheCountsOfMergedModifies(t *testing.T) {
	start := time.Date(2026, 10, 22, 17, 27, 0, 0, time.UTC)
	repeater := newFSWatchRepeater(func() int { return 200 }, textStyle{})
	repeater.next(repeatRecord("modify", "app.log", "", start, 5))
	merged, rows := repeater.next(repeatRecord("modify", "app.log", "", start.Add(time.Second), 3))
	if rows != 1 || merged.Count != 8 {
		t.Fatalf("rows %d, count %d, want 1 and 8", rows, merged.Count)
	}
}

func TestFSWatchRepeaterCountsTheRowsOfALongLine(t *testing.T) {
	start := time.Date(2026, 10, 22, 17, 27, 0, 0, time.UTC)
	long := "root/" + strings.Repeat("directory/", 12) + "file"
	repeater := newFSWatchRepeater(func() int { return 40 }, textStyle{})
	line := repeatRecord("create", long, "", start, 0)
	repeater.next(line)
	wantRows := repeater.rows
	if wantRows < 3 {
		t.Fatalf("a long line takes %d rows at width 40", wantRows)
	}
	// 고쳐 쓸 때는 화면에 있는 줄의 줄 수를 쓴다. ×N이 붙어 길어져도 이미 그려진 줄 수다.
	if _, rows := repeater.next(repeatRecord("create", long, "", start.Add(time.Second), 0)); rows != wantRows {
		t.Errorf("replace rows = %d, want %d", rows, wantRows)
	}
	// 폭을 모르면 고쳐 쓰지 않는다.
	unknown := newFSWatchRepeater(func() int { return 0 }, textStyle{})
	unknown.next(line)
	if _, rows := unknown.next(repeatRecord("create", long, "", start.Add(time.Second), 0)); rows != 0 {
		t.Error("folded without a terminal width")
	}
}

func TestFSWatchHintLineMarksTheNotice(t *testing.T) {
	previous := currentLanguage()
	defer setLanguage(previous)
	for language, prefix := range map[string]string{"en": "hint:", "ko": "힌트:", "ja": "ヒント:"} {
		setLanguage(language)
		if got := fsWatchHintLine(textStyle{}, "text"); got != prefix+" text" {
			t.Errorf("%s: line = %q", language, got)
		}
	}
	setLanguage("en")
	if colored := fsWatchHintLine(textStyle{color: true}, "text"); !strings.Contains(colored, "\x1b[") || ansi.Strip(colored) != "hint: text" {
		t.Errorf("colored line = %q", colored)
	}
}

// 터미널처럼 상태줄이 맨 아래에 있을 때 화면에 남는 줄을 본다. 같은 저장이 이어지면 한 줄이 ×N으로 바뀌고, 안내가 끼면 새 줄이 된다.
func TestFSWatchStreamFoldsRepeatsOnAScreenAndKeepsTheHint(t *testing.T) {
	previous := currentLanguage()
	defer setLanguage(previous)
	setLanguage("en")
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan fsWatchEvent, 256)
	out := &fsSmartSyncBuffer{}
	status := newFSWatchStatus(out, func() int { return 120 }, time.Now, []string{"modify"}, false)
	options := fsWatchOptions{root: ".", events: []string{"modify"}, match: "**", smart: true, status: status}
	options.notice = func(text string) { fmt.Fprintln(status.through(out), fsWatchHintLine(textStyle{}, text)) }
	done := make(chan int, 1)
	go func() {
		code, _ := streamFSWatch(ctx, out, fsWatchSource{events: events, errors: make(chan error)}, options, nil)
		done <- code
	}()
	burst := func(path string, count int) {
		for range count {
			events <- fsWatchEvent{Time: time.Now(), Event: "modify", Path: path}
		}
		time.Sleep(fsWatchSmartWindow + 120*time.Millisecond)
	}
	// 한 파일이 네 번 이어서 바뀐다. 줄 하나가 ×N으로 바뀐다.
	for range 4 {
		burst("quiet/a.txt", 1)
	}
	screen := fsRepeatScreen(out.String())
	var eventLines []string
	for _, line := range screen {
		if strings.Contains(line, "MODIFY") {
			eventLines = append(eventLines, line)
		}
	}
	if len(eventLines) != 1 || !strings.HasSuffix(eventLines[0], `MODIFY   "quiet/a.txt"  ×4`) {
		t.Fatalf("4 identical saves left %d lines: %q", len(eventLines), screen)
	}
	// 다른 파일이 끼면 새 줄이다.
	burst("quiet/b.txt", 1)
	burst("quiet/a.txt", 1)
	// 한 디렉터리에 이벤트가 몰려 안내가 나오면, 안내 줄이 지워지지 않고 그 뒤의 줄은 새로 시작한다.
	burst("noisy/app.log", 10)
	burst("noisy/app.log", 25)
	burst("noisy/app.log", 5)
	screen = fsRepeatScreen(out.String())
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stop timed out")
	}
	var want []string
	for _, line := range screen {
		switch {
		case strings.Contains(line, "MODIFY"):
			want = append(want, line[strings.Index(line, "MODIFY"):])
		case strings.HasPrefix(line, "hint:"):
			want = append(want, "HINT")
		}
	}
	expected := []string{`MODIFY   "quiet/a.txt"  ×4`, `MODIFY   "quiet/b.txt"`, `MODIFY   "quiet/a.txt"`, `MODIFY   "noisy/app.log"  ×10`, "HINT", `MODIFY   "noisy/app.log"  ×30`}
	if strings.Join(want, "\n") != strings.Join(expected, "\n") {
		t.Fatalf("screen lines:\n%s\nwant:\n%s\nfull screen: %q", strings.Join(want, "\n"), strings.Join(expected, "\n"), screen)
	}
}

// 같은 줄이라도 일으킨 process가 다르면 이어서 합치지 않는다.
func TestFSWatchRepeaterKeepsDifferentProcessesApart(t *testing.T) {
	start := time.Date(2026, 10, 22, 17, 27, 0, 0, time.UTC)
	repeater := newFSWatchRepeater(func() int { return 200 }, textStyle{})
	first := repeatRecord("modify", "a.log", "", start, 0)
	first.Process = "writer-a"
	repeater.next(first)
	same := repeatRecord("modify", "a.log", "", start.Add(time.Second), 0)
	same.Process = "writer-a"
	if _, rows := repeater.next(same); rows != 1 {
		t.Fatal("the same process did not fold")
	}
	other := repeatRecord("modify", "a.log", "", start.Add(2*time.Second), 0)
	other.Process = "writer-b"
	if _, rows := repeater.next(other); rows != 0 {
		t.Error("another process folded into the line")
	}
}
