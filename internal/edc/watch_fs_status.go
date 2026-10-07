package edc

import (
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// fsWatchNotice는 감시 중에 생긴 안내를 받는다. 이벤트 출력과 섞이지 않게 stderr로 보낸다. 상태줄을 그리면
// 상태줄을 지운 뒤 안내를 쓰도록 바뀐다.
var fsWatchNotice io.Writer = os.Stderr

// fsWatchRateWindow는 초당 이벤트 수를 평균하는 구간이다.
const fsWatchRateWindow = 10

// fsWatchStatus는 터미널 맨 아래 한 줄에 감시 통계를 그린다. 이벤트 goroutine의 안내와 감시 루프의 출력이
// 함께 쓰므로, 다른 줄을 쓸 때마다 상태줄을 지우고 쓴 뒤 다시 그린다.
type fsWatchStatus struct {
	mutex   sync.Mutex
	out     io.Writer
	width   func() int
	now     func() time.Time
	events  []string
	rules   bool
	started time.Time
	last    time.Time
	counts  map[string]int
	buckets [fsWatchRateWindow]int
	seconds [fsWatchRateWindow]int64
	actions int
	failed  int
	shown   bool
	closed  bool
}

func newFSWatchStatus(out io.Writer, width func() int, now func() time.Time, events []string, rules bool) *fsWatchStatus {
	return &fsWatchStatus{out: out, width: width, now: now, events: events, rules: rules, started: now(), counts: map[string]int{}}
}

// Write는 out에 줄을 쓴다. 감시 출력이 이 writer를 거친다.
func (status *fsWatchStatus) Write(data []byte) (int, error) {
	return status.writeTo(status.out, data)
}

// through는 target에 쓰는 writer다. stderr 안내처럼 다른 stream도 같은 화면에 나오므로 상태줄을 먼저 지운다.
func (status *fsWatchStatus) through(target io.Writer) io.Writer {
	return fsWatchStatusWriter{status: status, target: target}
}

type fsWatchStatusWriter struct {
	status *fsWatchStatus
	target io.Writer
}

func (writer fsWatchStatusWriter) Write(data []byte) (int, error) {
	return writer.status.writeTo(writer.target, data)
}

func (status *fsWatchStatus) writeTo(target io.Writer, data []byte) (int, error) {
	status.mutex.Lock()
	defer status.mutex.Unlock()
	status.clear()
	written, err := target.Write(data)
	if err == nil {
		status.draw()
	}
	return written, err
}

func (status *fsWatchStatus) event(kind string, at time.Time) {
	status.mutex.Lock()
	defer status.mutex.Unlock()
	status.counts[kind]++
	status.last = at
	second := at.Unix()
	slot := second % fsWatchRateWindow
	if status.seconds[slot] != second {
		status.seconds[slot], status.buckets[slot] = second, 0
	}
	status.buckets[slot]++
}

func (status *fsWatchStatus) action(failed bool) {
	status.mutex.Lock()
	defer status.mutex.Unlock()
	status.actions++
	if failed {
		status.failed++
	}
}

// tick은 이벤트가 없어도 경과 시간과 초당 이벤트 수를 갱신한다.
func (status *fsWatchStatus) tick() {
	status.mutex.Lock()
	defer status.mutex.Unlock()
	status.clear()
	status.draw()
}

// close는 상태줄을 지운다. 이후에는 다시 그리지 않으므로 요약 줄과 오류가 상태줄 없이 남는다.
func (status *fsWatchStatus) close() {
	status.mutex.Lock()
	defer status.mutex.Unlock()
	status.clear()
	status.closed = true
}

func (status *fsWatchStatus) clear() {
	if status.shown {
		io.WriteString(status.out, "\r\x1b[2K")
		status.shown = false
	}
}

func (status *fsWatchStatus) draw() {
	if status.closed {
		return
	}
	// 마지막 열에 쓰면 일부 터미널이 줄을 넘기므로 한 칸을 비운다.
	line := ansi.Truncate(status.line(max(1, status.width()-1)), max(1, status.width()-1), "…")
	io.WriteString(status.out, "\x1b[2m"+line+"\x1b[0m")
	status.shown = true
}

// line은 width 안에 들어가게 상태줄을 만든다. 넘치면 마지막 이벤트 시각, 초당 이벤트 수 순서로 항목을 뺀다.
func (status *fsWatchStatus) line(width int) string {
	now := status.now()
	counts := make([]string, 0, len(status.events))
	for _, kind := range status.events {
		counts = append(counts, fmt.Sprintf("%s %d", kind, status.counts[kind]))
	}
	head := []string{"─ " + fsWatchElapsed(now.Sub(status.started)), strings.Join(counts, " ")}
	var actions []string
	if status.rules {
		actions = []string{T("watchfs.status.actions", status.actions), T("watchfs.status.failed", status.failed)}
	}
	rate := fmt.Sprintf("%.1f/s", status.rate(now))
	var last []string
	if !status.last.IsZero() {
		last = []string{T("watchfs.status.last", fsWatchAgo(now.Sub(status.last)))}
	}
	join := func(rate []string, last []string) string {
		parts := append(append(append(append([]string{}, head...), rate...), actions...), last...)
		return strings.Join(parts, " · ")
	}
	for _, text := range []string{join([]string{rate}, last), join([]string{rate}, nil), join(nil, nil)} {
		if ansi.StringWidth(text) <= width {
			return text
		}
	}
	return join(nil, nil)
}

// rate는 지난 fsWatchRateWindow초의 초당 이벤트 수다. 감시를 시작한 지 그보다 짧으면 지난 시간으로 나눈다.
func (status *fsWatchStatus) rate(now time.Time) float64 {
	total := 0
	for slot, second := range status.seconds {
		if age := now.Unix() - second; age >= 0 && age < fsWatchRateWindow {
			total += status.buckets[slot]
		}
	}
	window := math.Min(fsWatchRateWindow, math.Max(1, now.Sub(status.started).Seconds()))
	return float64(total) / window
}

func fsWatchElapsed(elapsed time.Duration) string {
	seconds := int(elapsed.Seconds())
	if seconds >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", seconds/3600, seconds/60%60, seconds%60)
	}
	return fmt.Sprintf("%02d:%02d", seconds/60, seconds%60)
}

func fsWatchAgo(elapsed time.Duration) string {
	switch {
	case elapsed < time.Minute:
		return fmt.Sprintf("%ds", int(elapsed.Seconds()))
	case elapsed < time.Hour:
		return fmt.Sprintf("%dm", int(elapsed.Minutes()))
	}
	return fmt.Sprintf("%dh", int(elapsed.Hours()))
}
