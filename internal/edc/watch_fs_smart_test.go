package edc

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type fsSmartClock struct {
	smart *fsWatchSmart
	start time.Time
}

func newFSSmartClock() *fsSmartClock {
	return &fsSmartClock{smart: newFSWatchSmart(fsWatchSmartWindow), start: time.Date(2026, 10, 22, 17, 27, 19, 0, time.UTC)}
}

// add는 offsetMS가 지난 시각의 이벤트 하나를 넣는다.
func (clock *fsSmartClock) add(kind, path string, offsetMS int, dir ...bool) {
	at := clock.start.Add(time.Duration(offsetMS) * time.Millisecond)
	clock.smart.add(fsWatchEvent{Time: at, Event: kind, Path: path, IsDir: len(dir) > 0 && dir[0]}, at)
}

func (clock *fsSmartClock) flush() []fsWatchRecord { return clock.smart.flushAll() }

func smartLine(record fsWatchRecord) string {
	line := record.Event + " " + record.Path
	if record.Count > 0 {
		line += " x" + strconv.Itoa(record.Count)
	}
	if record.Origin != "" {
		line += " <- " + record.Origin
	}
	return line
}

func smartLines(records []fsWatchRecord) []string {
	lines := make([]string, len(records))
	for index, record := range records {
		lines[index] = smartLine(record)
	}
	return lines
}

func equalLines(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

func TestFSWatchSmartMergesAnAtomicSaveIntoTheFinalName(t *testing.T) {
	// 사용자가 붙여 넣은 실제 출력이다. 임시 파일에 쓰고 이름을 바꿔 교체한다.
	clock := newFSSmartClock()
	dir := "root/.aic/log-checkpoints/"
	clock.add("create", dir+"file_syslog.tmp", 683)
	clock.add("modify", dir+"file_syslog.tmp", 683)
	clock.add("rename", dir+"file_syslog.tmp", 688)
	clock.add("create", dir+"file_syslog.checkpoint", 688)
	records := clock.flush()
	if got := smartLines(records); !equalLines(got, "create "+dir+"file_syslog.checkpoint <- file_syslog.tmp") {
		t.Fatalf("lines = %v", got)
	}
	if !records[0].Time.Equal(clock.start.Add(688 * time.Millisecond)) {
		t.Errorf("time = %s, want the time of the last event", records[0].Time)
	}
}

func TestFSWatchSmartMergesOneFilesEvents(t *testing.T) {
	for _, test := range []struct {
		name string
		feed func(*fsSmartClock)
		want []string
	}{
		{"create then modify", func(c *fsSmartClock) { c.add("create", "a.txt", 0); c.add("modify", "a.txt", 1) }, []string{"create a.txt"}},
		{"modify five times", func(c *fsSmartClock) {
			for i := range 5 {
				c.add("modify", "a.log", i)
			}
		}, []string{"modify a.log x5"}},
		{"modify twelve times shows both digits", func(c *fsSmartClock) {
			for i := range 12 {
				c.add("modify", "a.log", i)
			}
		}, []string{"modify a.log x12"}},
		{"modify once has no count", func(c *fsSmartClock) { c.add("modify", "a.log", 0) }, []string{"modify a.log"}},
		{"created and removed", func(c *fsSmartClock) {
			c.add("create", "t.tmp", 0)
			c.add("modify", "t.tmp", 1)
			c.add("remove", "t.tmp", 2)
		}, []string{"temp t.tmp"}},
		{"modified then removed", func(c *fsSmartClock) { c.add("modify", "a.txt", 0); c.add("remove", "a.txt", 1) }, []string{"remove a.txt"}},
		{"removed then created again", func(c *fsSmartClock) { c.add("remove", "a.txt", 0); c.add("create", "a.txt", 1) }, []string{"modify a.txt"}},
		{"lone rename stays", func(c *fsSmartClock) { c.add("rename", "old.txt", 0) }, []string{"rename old.txt"}},
		{"different files stay apart, in order", func(c *fsSmartClock) {
			c.add("create", "b.txt", 0)
			c.add("modify", "a.txt", 1)
			c.add("modify", "b.txt", 2)
			c.add("remove", "c.txt", 3)
		}, []string{"create b.txt", "modify a.txt", "remove c.txt"}},
	} {
		clock := newFSSmartClock()
		test.feed(clock)
		if got := smartLines(clock.flush()); !equalLines(got, test.want...) {
			t.Errorf("%s: lines = %v, want %v", test.name, got, test.want)
		}
	}
}

func TestFSWatchSmartPairsARenameOnlyWithTheCreateRightAfterIt(t *testing.T) {
	for _, test := range []struct {
		name string
		feed func(*fsSmartClock)
		want []string
	}{
		{"same directory shows the name only", func(c *fsSmartClock) { c.add("rename", "docs/a.txt", 0); c.add("create", "docs/b.txt", 1) }, []string{"create docs/b.txt <- a.txt"}},
		{"another directory shows the path", func(c *fsSmartClock) { c.add("rename", "in/a.txt", 0); c.add("create", "out/a.txt", 1) }, []string{"create out/a.txt <- in/a.txt"}},
		{"an event between them breaks the pair", func(c *fsSmartClock) {
			c.add("rename", "a.txt", 0)
			c.add("modify", "x.txt", 1)
			c.add("create", "b.txt", 2)
		}, []string{"rename a.txt", "modify x.txt", "create b.txt"}},
		{"a file is not paired with a directory", func(c *fsSmartClock) { c.add("rename", "a.txt", 0); c.add("create", "dir", 1, true) }, []string{"rename a.txt", "create dir"}},
		{"a directory moves as a directory", func(c *fsSmartClock) { c.add("rename", "old", 0, true); c.add("create", "new", 1, true) }, []string{"create new <- old"}},
		{"only the first create is the destination", func(c *fsSmartClock) {
			c.add("rename", "a.txt", 0)
			c.add("create", "b.txt", 1)
			c.add("create", "c.txt", 2)
		}, []string{"create b.txt <- a.txt", "create c.txt"}},
	} {
		clock := newFSSmartClock()
		test.feed(clock)
		if got := smartLines(clock.flush()); !equalLines(got, test.want...) {
			t.Errorf("%s: lines = %v, want %v", test.name, got, test.want)
		}
	}
}

func TestFSWatchSmartFlushesAfterTheWindow(t *testing.T) {
	clock := newFSSmartClock()
	if _, ok := clock.smart.deadline(); ok {
		t.Fatal("an empty window has a deadline")
	}
	clock.add("create", "a.txt", 0)
	due, ok := clock.smart.deadline()
	if !ok || !due.Equal(clock.start.Add(fsWatchSmartWindow)) {
		t.Fatalf("deadline = %s %t", due, ok)
	}
	if records := clock.smart.flush(clock.start.Add(fsWatchSmartWindow - time.Millisecond)); records != nil {
		t.Fatalf("flushed before the window ended: %v", records)
	}
	if records := clock.smart.flush(clock.start.Add(fsWatchSmartWindow)); len(records) != 1 {
		t.Fatalf("flush at the window = %v", records)
	}
	// 내보낸 뒤에는 새 창이다. 앞 창의 항목이 남지 않는다.
	if clock.smart.pending() {
		t.Fatal("the window is still open after the flush")
	}
	clock.add("modify", "a.txt", 500)
	if got := smartLines(clock.flush()); !equalLines(got, "modify a.txt") {
		t.Fatalf("second window = %v", got)
	}
}

func TestFSWatchSmartEnabledFollowsTheOutputAndTheFlags(t *testing.T) {
	for _, test := range []struct {
		name     string
		on, off  bool
		jsonPath string
		terminal bool
		want     bool
	}{
		{"terminal text", false, false, "", true, true},
		{"pipe text keeps every line", false, false, "", false, false},
		{"--smart on a pipe", true, false, "", false, true},
		{"--no-smart on a terminal", false, true, "", true, false},
		{"json keeps every event", false, false, "-", true, false},
		{"json to a file", false, false, "out.jsonl", false, false},
	} {
		if got := fsWatchSmartEnabled(test.on, test.off, test.jsonPath, test.terminal); got != test.want {
			t.Errorf("%s: smart = %t, want %t", test.name, got, test.want)
		}
	}
}

func TestFSWatchSmartRecordsPrintTheOriginAndTheCount(t *testing.T) {
	var output bytes.Buffer
	stamp := time.Date(2026, 10, 22, 17, 27, 19, 0, time.UTC)
	records := []fsWatchRecord{
		{Type: "event", Time: stamp, Event: "create", Path: "dir/final", Origin: "tmp"},
		{Type: "event", Time: stamp, Event: "modify", Path: "app.log", Count: 5},
		{Type: "event", Time: stamp, Event: "temp", Path: "scratch"},
		{Type: "event", Time: stamp, Event: "modify", Path: "once"},
	}
	for _, record := range records {
		if err := writeFSWatchRecord(&output, record, false, textStyle{}); err != nil {
			t.Fatal(err)
		}
	}
	text := output.String()
	for _, want := range []string{`CREATE   "dir/final"  ← "tmp"`, `MODIFY   "app.log"  ×5`, `TEMP     "scratch"`, `MODIFY   "once"`} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, `"once"  ×`) {
		t.Errorf("a single event has a count:\n%s", text)
	}
}

func TestFSWatchSmartOptionsRejectContradictions(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][]string{{"--smart", "--no-smart", root}, {"--smart", "--json", "-", root}} {
		if _, err := parseFSWatchOptions(args, io.Discard); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
	for _, args := range [][]string{{"--smart", root}, {"--no-smart", root}, {"--no-smart", "--json", "-", root}} {
		if _, err := parseFSWatchOptions(args, io.Discard); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
}

type fsSmartSyncBuffer struct {
	mutex  sync.Mutex
	buffer bytes.Buffer
}

func (buffer *fsSmartSyncBuffer) Write(data []byte) (int, error) {
	buffer.mutex.Lock()
	defer buffer.mutex.Unlock()
	return buffer.buffer.Write(data)
}

func (buffer *fsSmartSyncBuffer) String() string {
	buffer.mutex.Lock()
	defer buffer.mutex.Unlock()
	return buffer.buffer.String()
}

// 화면에는 한 줄이 나가지만, 요약의 이벤트 수는 원본 4건이다. 멈출 때 창에 남은 것도 요약 앞에 나간다.
func TestFSWatchSmartStreamPrintsOneLineAndCountsEveryEvent(t *testing.T) {
	previous := currentLanguage()
	defer setLanguage(previous)
	setLanguage("en")
	for _, smart := range []bool{true, false} {
		ctx, cancel := context.WithCancel(context.Background())
		events := make(chan fsWatchEvent, 8)
		output := &fsSmartSyncBuffer{}
		options := fsWatchOptions{root: ".", events: []string{"create", "modify", "rename"}, match: "**", smart: smart}
		done := make(chan int, 1)
		go func() {
			code, _ := streamFSWatch(ctx, output, fsWatchSource{events: events, errors: make(chan error)}, options, nil)
			done <- code
		}()
		now := time.Now()
		for _, event := range []fsWatchEvent{
			{Time: now, Event: "create", Path: "d/f.tmp"}, {Time: now, Event: "modify", Path: "d/f.tmp"},
			{Time: now, Event: "rename", Path: "d/f.tmp"}, {Time: now, Event: "create", Path: "d/f"},
		} {
			events <- event
		}
		// 창(200ms)이 지나기 전에 멈춰도 모은 것이 나가야 한다.
		time.Sleep(50 * time.Millisecond)
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("stop timed out")
		}
		text := output.String()
		lines := 0
		for _, line := range strings.Split(text, "\n") {
			if strings.Contains(line, "CREATE") || strings.Contains(line, "MODIFY") || strings.Contains(line, "RENAME") {
				lines++
			}
		}
		wantLines := 4
		if smart {
			wantLines = 1
			if !strings.Contains(text, `CREATE   "d/f"  ← "f.tmp"`) {
				t.Errorf("smart output lacks the merged line:\n%s", text)
			}
		}
		if lines != wantLines || !strings.Contains(text, "4 events") {
			t.Errorf("smart=%t: %d event lines, want %d, and 4 events in the summary:\n%s", smart, lines, wantLines, text)
		}
	}
}

// 줄을 일으킨 process는 마지막으로 이름을 아는 이벤트의 것이다. 이름을 모르는 이벤트(inotify, 끝난 process)는 앞의 이름을 지우지 않는다.
func TestFSWatchSmartKeepsTheLastKnownProcess(t *testing.T) {
	clock := newFSSmartClock()
	at := func(ms int) time.Time { return clock.start.Add(time.Duration(ms) * time.Millisecond) }
	clock.smart.add(fsWatchEvent{Time: at(0), Event: "create", Path: "d/f.tmp", PID: 11, Process: "editor"}, at(0))
	clock.smart.add(fsWatchEvent{Time: at(1), Event: "modify", Path: "d/f.tmp", PID: 12, Process: "formatter"}, at(1))
	clock.smart.add(fsWatchEvent{Time: at(2), Event: "rename", Path: "d/f.tmp"}, at(2))
	clock.smart.add(fsWatchEvent{Time: at(3), Event: "create", Path: "d/f"}, at(3))
	records := clock.flush()
	if len(records) != 1 || records[0].Process != "formatter" || records[0].PID != 12 || records[0].Origin != "f.tmp" {
		// 도착점의 create에는 이름이 없어서, 같은 동작인 옛 이름 줄의 마지막 이름을 이어받는다.
		t.Fatalf("records = %+v", records)
	}
	other := newFSSmartClock()
	other.smart.add(fsWatchEvent{Time: at(0), Event: "create", Path: "a.txt", PID: 11, Process: "editor"}, at(0))
	other.smart.add(fsWatchEvent{Time: at(1), Event: "modify", Path: "a.txt"}, at(1))
	other.smart.add(fsWatchEvent{Time: at(2), Event: "modify", Path: "a.txt", PID: 12, Process: "formatter"}, at(2))
	other.smart.add(fsWatchEvent{Time: at(3), Event: "modify", Path: "a.txt"}, at(3))
	records = other.flush()
	if len(records) != 1 || records[0].Process != "formatter" || records[0].PID != 12 {
		t.Fatalf("records = %+v", records)
	}
}

func TestFSWatchRecordPrintsWhoChangedTheFile(t *testing.T) {
	previous := currentLanguage()
	defer setLanguage(previous)
	stamp := time.Date(2026, 10, 22, 17, 27, 19, 0, time.UTC)
	for language, want := range map[string]string{"en": `MODIFY   "app.log"  ×3  by syslog-ng`, "ko": `MODIFY   "app.log"  ×3  주체 syslog-ng`, "ja": `MODIFY   "app.log"  ×3  実行元 syslog-ng`} {
		setLanguage(language)
		var output bytes.Buffer
		record := fsWatchRecord{Type: "event", Time: stamp, Event: "modify", Path: "app.log", Count: 3, PID: 321, Process: "syslog-ng"}
		if err := writeFSWatchRecord(&output, record, false, textStyle{}); err != nil || !strings.Contains(output.String(), want) {
			t.Errorf("%s: %q, %v, want %q", language, output.String(), err, want)
		}
	}
	// 제어 문자가 든 이름은 터미널을 바꾸지 못한다.
	setLanguage("en")
	var output bytes.Buffer
	writeFSWatchRecord(&output, fsWatchRecord{Type: "event", Time: stamp, Event: "modify", Path: "x", Process: "bad\x1b[2Jname"}, false, textStyle{})
	if strings.Contains(output.String(), "\x1b") {
		t.Errorf("a control character reached the terminal: %q", output.String())
	}
}

// JSON에는 일으킨 process가 pid와 이름으로 들어간다. 모르면 필드가 없어 이전 출력과 같고, 묶지 않은 텍스트 출력에는 싣지 않는다.
func TestFSWatchEventRecordCarriesTheProcessOnlyInJSON(t *testing.T) {
	event := fsWatchEvent{Time: time.Now(), Event: "modify", Path: "a", PID: 7, Process: "writer"}
	withProcess := fsWatchEventRecord(event, true)
	if withProcess.PID != 7 || withProcess.Process != "writer" {
		t.Errorf("json record = %+v", withProcess)
	}
	if plain := fsWatchEventRecord(event, false); plain.PID != 0 || plain.Process != "" {
		t.Errorf("text record = %+v", plain)
	}
	line := fsWatchReadyLineForTest(t, fsWatchRecord{Type: "event", Time: time.Now(), Event: "modify", Path: "a"})
	if strings.Contains(line, "pid") || strings.Contains(line, "process") {
		t.Errorf("an unknown process appears in the JSON: %s", line)
	}
	if line := fsWatchReadyLineForTest(t, withProcess); !strings.Contains(line, `"pid":7`) || !strings.Contains(line, `"process":"writer"`) {
		t.Errorf("json = %s", line)
	}
}

func fsWatchReadyLineForTest(t *testing.T, record fsWatchRecord) string {
	t.Helper()
	var output bytes.Buffer
	if err := writeFSWatchRecord(&output, record, true, textStyle{}); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(output.String())
}
