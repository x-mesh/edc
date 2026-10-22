package edc

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// feed는 한 디렉터리에 같은 시각으로 count건을 넣고 처음 나온 알림을 돌려준다.
func feedNoisy(noisy *fsWatchNoisy, directory string, count int, at time.Time) *fsWatchNoisyHint {
	var first *fsWatchNoisyHint
	for index := range count {
		if hint := noisy.add(fmt.Sprintf("%s/file%d", directory, index), at); hint != nil && first == nil {
			first = hint
		}
	}
	return first
}

func TestFSWatchNoisyHintsWhenOneDirectoryDominates(t *testing.T) {
	noisy := newFSWatchNoisy()
	start := time.Date(2026, 10, 22, 17, 27, 0, 0, time.UTC)
	if hint := feedNoisy(noisy, "root/.aic/log-checkpoints", fsWatchNoisyEvents-1, start); hint != nil {
		t.Fatalf("a hint came before the threshold: %+v", hint)
	}
	hint := noisy.add("root/.aic/log-checkpoints/last", start)
	if hint == nil || hint.Directory != "root/.aic/log-checkpoints" || hint.Events != fsWatchNoisyEvents || hint.Pattern != "root/.aic/log-checkpoints/**" {
		t.Fatalf("hint = %+v", hint)
	}
	// 같은 디렉터리는 한 번만 알린다.
	if again := feedNoisy(noisy, "root/.aic/log-checkpoints", 100, start.Add(time.Second)); again != nil {
		t.Fatalf("the same directory was hinted twice: %+v", again)
	}
	// 알린 패턴은 그 디렉터리의 이벤트를 실제로 뺀다.
	if !fsWatchExcluded([]string{hint.Pattern}, "root/.aic/log-checkpoints/file_syslog.checkpoint") {
		t.Errorf("the pattern %q does not exclude a file in the directory", hint.Pattern)
	}
	if fsWatchExcluded([]string{hint.Pattern}, "root/.aic/other/file") {
		t.Errorf("the pattern %q excludes a file in another directory", hint.Pattern)
	}
}

func TestFSWatchNoisyNeedsAShareOfTheEvents(t *testing.T) {
	noisy := newFSWatchNoisy()
	start := time.Date(2026, 10, 22, 17, 27, 0, 0, time.UTC)
	// 여러 디렉터리가 번갈아 고르게 시끄러우면 어느 하나를 빼라고 하지 않는다. 디렉터리마다 기준의 두 배를 넣는다.
	directories := []string{"a", "b", "c", "d"}
	for index := range fsWatchNoisyEvents * 2 * len(directories) {
		directory := directories[index%len(directories)]
		if hint := noisy.add(fmt.Sprintf("%s/file%d", directory, index), start); hint != nil {
			t.Fatalf("an even spread made a hint: %+v", hint)
		}
	}
	// 한 곳이 절반 이상을 차지하면 알린다.
	if hint := feedNoisy(noisy, "e", fsWatchNoisyEvents*8, start); hint == nil || hint.Directory != "e" {
		t.Fatalf("a dominant directory was not hinted: %+v", hint)
	}
}

func TestFSWatchNoisyForgetsEventsOutsideTheWindow(t *testing.T) {
	noisy := newFSWatchNoisy()
	start := time.Date(2026, 10, 22, 17, 27, 0, 0, time.UTC)
	for index := range fsWatchNoisyEvents * 3 {
		// 한 건씩 창보다 긴 간격으로 오면 쌓이지 않는다.
		if hint := noisy.add("slow/file", start.Add(time.Duration(index)*fsWatchNoisyWindow)); hint != nil {
			t.Fatalf("slow events made a hint at %d: %+v", index, hint)
		}
	}
	if len(noisy.queue) != 1 || noisy.counts["slow"] != 1 {
		t.Errorf("queue %d, counts %v", len(noisy.queue), noisy.counts)
	}
}

func TestFSWatchNoisyLeavesOutDirectoriesThatCannotBeExcluded(t *testing.T) {
	start := time.Date(2026, 10, 22, 17, 27, 0, 0, time.UTC)
	for _, test := range []struct{ name, path string }{
		{"the watch root", "file%d"},
		{"a glob character", "logs[1]/file%d"},
		{"a wildcard", "build*/file%d"},
		{"a backslash", `dir\x/file%d`},
	} {
		noisy := newFSWatchNoisy()
		var hint *fsWatchNoisyHint
		for index := range fsWatchNoisyEvents * 2 {
			if got := noisy.add(fmt.Sprintf(test.path, index), start); got != nil {
				hint = got
			}
		}
		if hint != nil {
			t.Errorf("%s: hint = %+v", test.name, hint)
		}
	}
}

func TestFSWatchNoisyHintsAtMostThreeDirectories(t *testing.T) {
	noisy := newFSWatchNoisy()
	start := time.Date(2026, 10, 22, 17, 27, 0, 0, time.UTC)
	hints := 0
	for index := range 6 {
		// 디렉터리마다 차례로 시끄러워진다. 앞의 것은 창 밖으로 나가고 다음 것이 절반을 넘는다.
		at := start.Add(time.Duration(index) * (fsWatchNoisyWindow + time.Second))
		if feedNoisy(noisy, fmt.Sprintf("dir%d", index), fsWatchNoisyEvents, at) != nil {
			hints++
		}
	}
	if hints != fsWatchNoisyLimit {
		t.Errorf("hints = %d, want %d", hints, fsWatchNoisyLimit)
	}
}

func TestFSWatchNoisyMemoryStaysBounded(t *testing.T) {
	noisy := newFSWatchNoisy()
	start := time.Date(2026, 10, 22, 17, 27, 0, 0, time.UTC)
	for index := range fsWatchNoisyQueueMax + 5000 {
		noisy.add(fmt.Sprintf("d%d/f", index%7000), start)
	}
	total := 0
	for _, count := range noisy.counts {
		total += count
	}
	if len(noisy.queue) > fsWatchNoisyQueueMax || total != len(noisy.queue) {
		t.Errorf("queue %d (max %d), counted %d", len(noisy.queue), fsWatchNoisyQueueMax, total)
	}
}

func TestFSWatchShellQuoteSurvivesTheShell(t *testing.T) {
	for _, test := range []struct{ in, want string }{
		{"dir/**", `'dir/**'`},
		{"my dir/**", `'my dir/**'`},
		{"it's/**", `'it'\''s/**'`},
	} {
		if got := fsWatchShellQuote(test.in); got != test.want {
			t.Errorf("quote(%q) = %q, want %q", test.in, got, test.want)
		}
	}
}

type fsNoisyNotices struct {
	mutex sync.Mutex
	lines []string
}

func (notices *fsNoisyNotices) add(text string) {
	notices.mutex.Lock()
	defer notices.mutex.Unlock()
	notices.lines = append(notices.lines, text)
}

func (notices *fsNoisyNotices) get() []string {
	notices.mutex.Lock()
	defer notices.mutex.Unlock()
	return append([]string(nil), notices.lines...)
}

// 묶어서 보이는 출력에서만 알리고, 알림은 stdout 기록과 따로 나간다.
func TestFSWatchStreamNoticesANoisyDirectoryOnlyInSmartMode(t *testing.T) {
	previous := currentLanguage()
	defer setLanguage(previous)
	setLanguage("en")
	for _, test := range []struct {
		name   string
		smart  bool
		notice bool
		want   int
	}{
		{"smart with a notice", true, true, 1},
		{"no-smart keeps the old output", false, true, 0},
		{"smart without a place to tell", true, false, 0},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		events := make(chan fsWatchEvent, 256)
		output := &fsSmartSyncBuffer{}
		notices := &fsNoisyNotices{}
		options := fsWatchOptions{root: ".", events: []string{"modify"}, match: "**", smart: test.smart}
		if test.notice {
			options.notice = notices.add
		}
		done := make(chan int, 1)
		go func() {
			code, _ := streamFSWatch(ctx, output, fsWatchSource{events: events, errors: make(chan error)}, options, nil)
			done <- code
		}()
		for index := range fsWatchNoisyEvents + 10 {
			events <- fsWatchEvent{Time: time.Now(), Event: "modify", Path: fmt.Sprintf("my dir/log%d", index)}
		}
		time.Sleep(100 * time.Millisecond)
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("stop timed out")
		}
		lines := notices.get()
		if len(lines) != test.want {
			t.Errorf("%s: notices = %v, want %d", test.name, lines, test.want)
			continue
		}
		if test.want == 1 && (!strings.Contains(lines[0], `"my dir"`) || !strings.Contains(lines[0], `--exclude 'my dir/**'`) || !strings.Contains(lines[0], "10s")) {
			t.Errorf("%s: notice = %q", test.name, lines[0])
		}
		if strings.Contains(output.String(), "--exclude") {
			t.Errorf("%s: the hint leaked into the event output:\n%s", test.name, output.String())
		}
	}
}
