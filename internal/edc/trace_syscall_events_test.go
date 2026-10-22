package edc

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

// syscallEventClock는 1초 간격 snapshot을 만든다. 호출 수와 errno 수는 누적값이다.
type syscallEventClock struct {
	detector *syscallEventDetector
	start    time.Time
	tick     int
	calls    map[string]uint64
	errnos   map[string]uint64
	paths    map[string]uint64
}

func newSyscallEventClock() *syscallEventClock {
	return &syscallEventClock{detector: newSyscallEventDetector(), start: time.Date(2026, 10, 22, 13, 0, 0, 0, time.UTC), calls: map[string]uint64{}, errnos: map[string]uint64{}, paths: map[string]uint64{}}
}

// step은 1초가 지난 snapshot을 넣는다. calls는 "process syscall"별 증가분, errnos는 "process syscall errno"별 증가분,
// paths는 "path errno"별 증가분이다.
func (clock *syscallEventClock) step(calls, errnos, paths map[string]uint64) {
	clock.stepOver(1, calls, errnos, paths)
}

// stepOver는 gap초가 지난 snapshot을 넣는다. 증가분은 그 gap초 동안의 값이다.
func (clock *syscallEventClock) stepOver(gap int, calls, errnos, paths map[string]uint64) {
	for key, value := range calls {
		clock.calls[key] += value
	}
	for key, value := range errnos {
		clock.errnos[key] += value
	}
	for key, value := range paths {
		clock.paths[key] += value
	}
	samples := map[string]*syscallSample{}
	sample := func(process, syscall string) *syscallSample {
		key := process + " " + syscall
		if samples[key] == nil {
			samples[key] = &syscallSample{Process: process, Syscall: syscall, Calls: clock.calls[key], Errnos: map[string]uint64{}}
		}
		return samples[key]
	}
	for key := range clock.calls {
		process, syscall, _ := strings.Cut(key, " ")
		sample(process, syscall)
	}
	for key, value := range clock.errnos {
		fields := strings.Fields(key)
		sample(fields[0], fields[1]).Errnos[fields[2]] = value
	}
	var collection syscallCollection
	for _, value := range samples {
		collection.samples = append(collection.samples, *value)
	}
	rows := map[string]*syscallFileRow{}
	for key, value := range clock.paths {
		path, errno, _ := strings.Cut(key, " ")
		if rows[path] == nil {
			rows[path] = &syscallFileRow{Path: path, Errnos: map[string]uint64{}}
		}
		rows[path].Errnos[errno] = value
	}
	var files []syscallFileRow
	for _, row := range rows {
		files = append(files, *row)
	}
	clock.tick += gap
	collection.summary.DurationMS = int64(clock.tick) * 1000
	clock.detector.observe(clock.start.Add(time.Duration(clock.tick)*time.Second), collection, files)
}

func (clock *syscallEventClock) steps(count int, calls, errnos, paths map[string]uint64) {
	for range count {
		clock.step(calls, errnos, paths)
	}
}

func TestSyscallEventErrorLoopNeedsFiveSecondsAndMergesRestarts(t *testing.T) {
	clock := newSyscallEventClock()
	loop := map[string]uint64{"api connect ECONNREFUSED": 200}
	clock.step(nil, loop, nil)
	clock.steps(4, nil, loop, nil)
	if events := clock.detector.report(); len(events) != 0 {
		t.Fatalf("an event came before the condition held for 5 seconds: %+v", events)
	}
	clock.steps(3, nil, loop, nil)
	events := clock.detector.report()
	if len(events) != 1 || !events[0].Active || events[0].Kind != syscallKindErrorLoop || events[0].Process != "api" || events[0].Syscall != "connect" || events[0].Errno != "ECONNREFUSED" {
		t.Fatalf("events = %+v", events)
	}
	if events[0].PeakPerSec < 199 || events[0].PeakPerSec > 201 || !events[0].Start.Equal(clock.start.Add(2*time.Second)) {
		t.Errorf("event = %+v", events[0])
	}
	// 3초 쉬고 다시 켜지면 같은 event다.
	clock.steps(3, nil, nil, nil)
	if events := clock.detector.report(); len(events) != 1 || events[0].Active {
		t.Fatalf("during the pause: %+v", events)
	}
	clock.steps(2, nil, loop, nil)
	if events := clock.detector.report(); len(events) != 1 || !events[0].Active {
		t.Fatalf("after the restart: %+v", events)
	}
	// 5초 넘게 쉬면 닫히고, 그 뒤에 다시 켜지면 새 event다.
	clock.steps(7, nil, nil, nil)
	clock.steps(7, nil, loop, nil)
	if events := clock.detector.report(); len(events) != 2 {
		t.Fatalf("a restart after a long pause is not a new event: %+v", events)
	}
}

func TestSyscallEventErrorLoopIgnoresNormalErrnos(t *testing.T) {
	clock := newSyscallEventClock()
	busy := map[string]uint64{"oz futex EAGAIN": 500000, "oz futex ETIMEDOUT": 5000, "oz read EAGAIN": 20000, "oz connect EINPROGRESS": 3000, "sh wait4 ECHILD": 400, "oz epoll_wait EINTR": 9000}
	clock.steps(12, nil, busy, nil)
	if events := clock.detector.report(); len(events) != 0 {
		t.Fatalf("normal errnos made events: %+v", events)
	}
	// 같은 futex라도 ETIMEDOUT이 아닌 오류는 센다.
	clock.steps(8, nil, map[string]uint64{"oz futex EINVAL": 300}, nil)
	if events := clock.detector.report(); len(events) != 1 || events[0].Errno != "EINVAL" {
		t.Fatalf("events = %+v", events)
	}
}

func TestSyscallEventMissingPathLoop(t *testing.T) {
	clock := newSyscallEventClock()
	clock.steps(9, nil, nil, map[string]uint64{"/etc/app.conf ENOENT": 80, "/dev/shm/x EAGAIN": 900, "/etc/rare ENOENT": 3, "/root/secret EACCES": 400, "/proc/1/mem EPERM": 400})
	events := clock.detector.report()
	if len(events) != 1 || events[0].Kind != syscallKindMissingPath || events[0].Path != "/etc/app.conf" || events[0].Errno != "ENOENT" {
		t.Fatalf("events = %+v", events)
	}
	if got := events[0].target(); got != "ENOENT /etc/app.conf" {
		t.Errorf("target = %q", got)
	}
}

func TestSyscallEventCallSpikeNeedsABaselineAndAJump(t *testing.T) {
	clock := newSyscallEventClock()
	// 처음부터 초당 4만 번 부르는 process는 평소 값이므로 event가 아니다.
	clock.steps(15, map[string]uint64{"oz futex": 40000}, nil, nil)
	if events := clock.detector.report(); len(events) != 0 {
		t.Fatalf("a steady heavy caller made events: %+v", events)
	}
	spike := newSyscallEventClock()
	spike.steps(10, map[string]uint64{"api openat": 100}, nil, nil)
	spike.steps(10, map[string]uint64{"api openat": 5000}, nil, nil)
	events := spike.detector.report()
	if len(events) != 1 || events[0].Kind != syscallKindCallSpike || events[0].BaselinePerSec == nil || *events[0].BaselinePerSec < 99 || *events[0].BaselinePerSec > 101 {
		t.Fatalf("events = %+v", events)
	}
	// 급증이 길어져도 평소 평균이 오르지 않아 조건이 꺼지지 않는다.
	spike.steps(20, map[string]uint64{"api openat": 5000}, nil, nil)
	events = spike.detector.report()
	if len(events) != 1 || !events[0].Active {
		t.Fatalf("a long spike ended early: %+v", events)
	}
	if got := events[0].target(); got != "api openat (usually 100/s)" {
		t.Errorf("target = %q", got)
	}
	// COUNT는 초당 횟수가 아니라 호출 수의 증가분을 더한 값이다. 5,000번씩 30초 동안이다.
	if events[0].Count != 5000*30 {
		t.Errorf("count = %d, want %d", events[0].Count, 5000*30)
	}
}

func TestSyscallEventsKeepOnlyTheNewest(t *testing.T) {
	detector := newSyscallEventDetector()
	for index := range syscallEventKeep + 10 {
		detector.events = append(detector.events, &syscallAnomaly{Kind: syscallKindErrorLoop, Process: "p", Syscall: "s", Errno: "E", Count: uint64(index)})
	}
	detector.trim()
	if len(detector.events) != syscallEventKeep || detector.events[0].Count != 10 {
		t.Errorf("events = %d, first count = %d", len(detector.events), detector.events[0].Count)
	}
}

func TestSyscallEventsPrintInTheLiveAndFinalTables(t *testing.T) {
	clock := newSyscallEventClock()
	clock.steps(9, nil, map[string]uint64{"api connect ECONNREFUSED": 1200}, nil)
	report := syscallReportFor(syscallTestCollection(), "")
	report.Events = clock.detector.report()
	var final, live bytes.Buffer
	report.render(&final, syscallFinalView(false))
	view := syscallLiveView(40, false, len(report.Events))
	report.render(&live, view)
	for name, text := range map[string]string{"final": final.String(), "live": live.String()} {
		for _, want := range []string{"EVENTS", "●", "error loop", "api connect ECONNREFUSED", "1,200/s"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s output lacks %q:\n%s", name, want, text)
			}
		}
	}
	if !strings.Contains(live.String(), "1 in total, the newest first") {
		t.Errorf("live heading:\n%s", live.String())
	}
	// 실시간 화면은 event가 없어도 패널을 그려서 감지가 켜져 있음을 알린다. 최종 출력은 그리지 않는다.
	var empty, emptyFinal bytes.Buffer
	none := syscallReportFor(syscallTestCollection(), "")
	none.render(&empty, syscallLiveView(40, false, 1))
	none.render(&emptyFinal, syscallFinalView(false))
	if !strings.Contains(empty.String(), "EVENTS: none so far") || strings.Contains(emptyFinal.String(), "EVENTS") {
		t.Errorf("empty panel: live=\n%s\nfinal=\n%s", empty.String(), emptyFinal.String())
	}
	var without bytes.Buffer
	syscallReportFor(syscallTestCollection(), "").render(&without, syscallFinalView(false))
	if strings.Contains(without.String(), "EVENTS") {
		t.Errorf("a trace without events prints the table:\n%s", without.String())
	}
}

// event가 되기 전에 조건이 꺼지면 그 시간은 이어진 시간에 넣지 않는다. 3초 켜지고 1초 꺼진 뒤 다시 3초 켜져도 event가 아니다.
func TestSyscallEventConditionThatBreaksBeforeFiveSecondsIsNotAnEvent(t *testing.T) {
	clock := newSyscallEventClock()
	loop := map[string]uint64{"api connect ECONNREFUSED": 200}
	clock.step(nil, nil, nil)
	clock.steps(3, nil, loop, nil)
	clock.step(nil, nil, nil)
	clock.steps(3, nil, loop, nil)
	if events := clock.detector.report(); len(events) != 0 {
		t.Fatalf("a broken streak made an event: %+v", events)
	}
	clock.steps(3, nil, loop, nil)
	if events := clock.detector.report(); len(events) != 1 {
		t.Fatalf("a streak that reached 5 seconds is not an event: %+v", events)
	}
}

// detector가 nil이어도(--raw, --slow, --errors) report와 snapshot 처리가 멈추지 않아야 한다.
func TestSyscallEventDetectorMayBeNil(t *testing.T) {
	var detector *syscallEventDetector
	if events := detector.report(); events != nil {
		t.Fatalf("events = %+v", events)
	}
	file, err := os.CreateTemp(t.TempDir(), "live")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	newSyscallSnapshotHandler(file, "", nil, nil, true)(syscallTestCollection())
	newSyscallSnapshotHandler(file, "", nil, nil, false)(syscallTestCollection())
}

// 실시간 snapshot 처리가 event를 표에 넣는다. 처음에는 빈 패널이고, 조건이 5초 이어지면 event 행이 생긴다.
func TestSyscallSnapshotHandlerDrawsTheEventsPanel(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "live")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	clock := newSyscallEventClock()
	// 처리기는 snapshot마다 detector의 시계를 읽는다. 호출마다 1초씩 가는 시계를 넣는다.
	seconds := 0
	clock.detector.now = func() time.Time {
		seconds++
		return clock.start.Add(time.Duration(seconds) * time.Second)
	}
	handler := newSyscallSnapshotHandler(file, "", nil, clock.detector, true)
	collection := func(errors uint64, seconds int64) syscallCollection {
		return syscallCollection{
			samples: []syscallSample{{Process: "api", Syscall: "connect", Calls: errors, Errors: errors, Errnos: map[string]uint64{"ECONNREFUSED": errors}}},
			summary: syscallSummary{DurationMS: seconds * 1000},
		}
	}
	handler(collection(0, 0))
	first, _ := os.ReadFile(file.Name())
	if !strings.Contains(string(first), "EVENTS") || !strings.Contains(string(first), "none so far") {
		t.Fatalf("the first frame has no empty panel:\n%s", first)
	}
	for second := int64(1); second <= 8; second++ {
		handler(collection(uint64(second)*300, second))
	}
	all, _ := os.ReadFile(file.Name())
	last := string(all[strings.LastIndex(string(all), syscallLiveHome):])
	if !strings.Contains(last, "error loop") || !strings.Contains(last, "api connect ECONNREFUSED") || !strings.Contains(last, "●") || strings.Contains(last, "none so far") {
		t.Fatalf("the last frame lacks the event:\n%s", last)
	}
}

// snapshot 간격이 1초가 아니면 증가분과 초당 횟수가 다르다. COUNT는 증가분의 합이다.
func TestSyscallEventSpikeCountsTheIncreaseOfCallsOverLongerIntervals(t *testing.T) {
	clock := newSyscallEventClock()
	clock.steps(10, map[string]uint64{"api openat": 100}, nil, nil)
	// 2초마다 snapshot을 읽고, 매번 1만 번(초당 5,000번) 늘어난다. 4번째 snapshot에서 6초가 지나 event가 된다.
	for range 4 {
		clock.stepOver(2, map[string]uint64{"api openat": 10000}, nil, nil)
	}
	events := clock.detector.report()
	if len(events) != 1 || events[0].Kind != syscallKindCallSpike {
		t.Fatalf("events = %+v", events)
	}
	if events[0].Count != 40000 || events[0].PeakPerSec < 4999 || events[0].PeakPerSec > 5001 {
		t.Errorf("count = %d, peak = %.0f, want 40000 calls and 5000/s", events[0].Count, events[0].PeakPerSec)
	}
}
