package edc

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

const fsWatchStatusClear = "\r\x1b[2K"

func newFSWatchTestStatus(out *bytes.Buffer, width int, clock *time.Time, rules bool) *fsWatchStatus {
	return newFSWatchStatus(out, func() int { return width }, func() time.Time { return *clock }, []string{"create", "modify", "remove", "rename"}, rules)
}

func TestFSWatchStatusRedrawsAroundEachLine(t *testing.T) {
	previous := currentLanguage()
	defer setLanguage(previous)
	setLanguage("en")
	clock := time.Unix(1000, 0)
	var out, stderr bytes.Buffer
	status := newFSWatchTestStatus(&out, 200, &clock, true)
	status.Write([]byte("first\n"))
	if got := ansi.Strip(out.String()); got != "first\n─ 00:00 · create 0 modify 0 remove 0 rename 0 · 0.0/s · action 0 · failed 0" {
		t.Fatalf("first draw %q", got)
	}
	clock = clock.Add(2 * time.Second)
	status.event("create", clock)
	status.event("modify", clock)
	status.action(true)
	clock = clock.Add(3 * time.Second)
	out.Reset()
	status.through(&stderr).Write([]byte("notice\n"))
	if stderr.String() != "notice\n" || !strings.HasPrefix(out.String(), fsWatchStatusClear) {
		t.Fatalf("notice: stdout %q stderr %q", out.String(), stderr.String())
	}
	if got := ansi.Strip(strings.TrimPrefix(out.String(), fsWatchStatusClear)); got != "─ 00:05 · create 1 modify 1 remove 0 rename 0 · 0.4/s · action 1 · failed 1 · last 3s ago" {
		t.Fatalf("redraw %q", got)
	}
	out.Reset()
	status.close()
	status.Write([]byte("summary\n"))
	if out.String() != fsWatchStatusClear+"summary\n" {
		t.Fatalf("after close %q", out.String())
	}
}

func TestFSWatchStatusFitsTheTerminalWidth(t *testing.T) {
	previous := currentLanguage()
	defer setLanguage(previous)
	setLanguage("en")
	clock := time.Unix(1000, 0)
	for _, test := range []struct {
		width int
		want  string
	}{
		{120, "─ 00:05 · create 1 modify 0 remove 0 rename 0 · 0.2/s · action 4 · failed 1 · last 5s ago"},
		{80, "─ 00:05 · create 1 modify 0 remove 0 rename 0 · 0.2/s · action 4 · failed 1"},
		{70, "─ 00:05 · create 1 modify 0 remove 0 rename 0 · action 4 · failed 1"},
		{20, "─ 00:05 · create 1…"},
	} {
		now := clock
		var out bytes.Buffer
		status := newFSWatchTestStatus(&out, test.width, &now, true)
		status.event("create", now)
		for index := range 4 {
			status.action(index == 0)
		}
		now = now.Add(5 * time.Second)
		status.tick()
		if got := ansi.Strip(out.String()); got != test.want || ansi.StringWidth(got) >= test.width {
			t.Errorf("width %d: %q", test.width, got)
		}
	}
}

func TestFSWatchStatusRateUsesTheLastTenSeconds(t *testing.T) {
	clock := time.Unix(1000, 0)
	var out bytes.Buffer
	status := newFSWatchTestStatus(&out, 200, &clock, false)
	for range 30 {
		status.event("modify", clock)
	}
	clock = clock.Add(5 * time.Second)
	if rate := status.rate(clock); rate != 6 {
		t.Fatalf("rate after 5s = %v, want 6", rate)
	}
	clock = clock.Add(20 * time.Second)
	if rate := status.rate(clock); rate != 0 {
		t.Fatalf("rate after the window = %v, want 0", rate)
	}
}

func TestFSWatchStreamClearsTheStatusBeforeTheSummary(t *testing.T) {
	previous := currentLanguage()
	defer setLanguage(previous)
	setLanguage("en")
	clock := time.Now()
	var out bytes.Buffer
	options := fsWatchOptions{root: "/r", events: []string{"create"}, match: "**"}
	options.status = newFSWatchTestStatus(&out, 200, &clock, false)
	events := make(chan fsWatchEvent, 1)
	events <- fsWatchEvent{Time: clock, Event: "create", Path: "a.txt"}
	close(events)
	code, err := streamFSWatch(context.Background(), &out, fsWatchSource{events: events, errors: make(chan error)}, options, nil)
	if code != 1 || err == nil {
		t.Fatalf("code %d err %v", code, err)
	}
	text := out.String()
	if !strings.Contains(ansi.Strip(text), "create 1") {
		t.Fatalf("status did not count the event: %q", text)
	}
	// 터미널에서는 마지막 지우기가 상태줄을 없애므로 그 뒤에 요약 한 줄만 남아야 한다.
	after := text[strings.LastIndex(text, fsWatchStatusClear)+len(fsWatchStatusClear):]
	if strings.Contains(after, "─") || strings.Count(after, "\n") != 1 || !strings.HasSuffix(after, "\n") || !strings.Contains(after, "1 events") {
		t.Fatalf("after the last clear %q", after)
	}
}
