package edc

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"
)

// 감지 조건은 `edc top`의 event와 같은 규칙을 따른다. 조건이 syscallEventSustain 이상 이어지면 event가 되고, 꺼졌다가
// 그 시간 안에 다시 켜지면 같은 event다. 임계값은 처음 값이고 실제 process에서 돌려 보며 조정한다.
const (
	syscallEventSustain = 5 * time.Second
	// syscallEventKeep은 보관하는 event의 수다. 넘으면 끝난 것 중 오래된 것부터 버린다.
	syscallEventKeep = 100
	// 오류 반복과 없는 경로 반복은 초당 이 횟수 이상 실패할 때 켜진다.
	syscallErrorLoopRate   = 100.0
	syscallMissingPathRate = 50.0
	// 호출 급증은 초당 syscallSpikeRate 이상이면서 최근 syscallBaselineSamples초 평균의 syscallSpikeFactor배 이상일 때 켜진다.
	// 평균은 syscallBaselineMin초 이상 쌓인 뒤에 쓴다.
	syscallSpikeRate       = 1000.0
	syscallSpikeFactor     = 10.0
	syscallBaselineSamples = 30
	syscallBaselineMin     = 5
	// syscallEventRows는 최종 표에 보이는 event의 수이고, syscallLiveEventRows는 실시간 화면에 보이는 수다.
	syscallEventRows     = 20
	syscallLiveEventRows = 3
)

const (
	syscallKindErrorLoop   = "error_loop"
	syscallKindMissingPath = "missing_path"
	syscallKindCallSpike   = "call_spike"
)

// syscallBenignErrnos는 오류 반복으로 보지 않는 errno다. 비동기 I/O와 신호는 정상 동작에서도 초당 수만 번 실패한다.
var syscallBenignErrnos = map[string]bool{"EAGAIN": true, "EINTR": true, "EINPROGRESS": true, "ERESTARTSYS": true, "ERESTARTNOINTR": true, "ERESTARTNOHAND": true, "ERESTART_RESTARTBLOCK": true}

// syscallBenignPairs는 그 호출에서만 정상인 errno다. 시간 제한이 있는 futex 기다림은 ETIMEDOUT로 끝나고, 자식이 없을 때의 wait는 ECHILD다.
var syscallBenignPairs = map[string]bool{"futex ETIMEDOUT": true, "wait4 ECHILD": true, "waitid ECHILD": true}

// syscallAnomaly는 조건이 syscallEventSustain 이상 이어진 event 하나다. Active는 지금 조건이 켜져 있다는 뜻이다.
type syscallAnomaly struct {
	Kind    string    `json:"kind"`
	Process string    `json:"process,omitempty"`
	Syscall string    `json:"syscall,omitempty"`
	Errno   string    `json:"errno,omitempty"`
	Path    string    `json:"path,omitempty"`
	Start   time.Time `json:"start"`
	// End는 조건이 마지막으로 켜져 있던 시각이다.
	End    time.Time `json:"end"`
	Active bool      `json:"active"`
	// PeakPerSec는 가장 높았던 초당 횟수이고, BaselinePerSec는 호출 급증이 시작될 때의 평소 초당 횟수다.
	PeakPerSec     float64  `json:"peak_per_sec"`
	BaselinePerSec *float64 `json:"baseline_per_sec,omitempty"`
	Count          uint64   `json:"count"`
}

func (anomaly syscallAnomaly) kindName() string { return strings.ReplaceAll(anomaly.Kind, "_", " ") }

// target은 표의 마지막 칸이다.
func (anomaly syscallAnomaly) target() string {
	switch anomaly.Kind {
	case syscallKindErrorLoop:
		return anomaly.Process + " " + anomaly.Syscall + " " + anomaly.Errno
	case syscallKindMissingPath:
		return anomaly.Errno + " " + anomaly.Path
	}
	text := anomaly.Process + " " + anomaly.Syscall
	if anomaly.BaselinePerSec != nil {
		text += fmt.Sprintf(" (usually %s/s)", groupThousands(int64(*anomaly.BaselinePerSec)))
	}
	return text
}

// syscallHit은 이번 sample에서 켜진 조건 하나다.
type syscallHit struct {
	proto    syscallAnomaly
	rate     float64
	count    uint64
	baseline *float64
}

// syscallCondition은 조건 하나의 상태다. onSince는 켜진 채 이어진 시작이고, event가 있으면 이미 syscallEventSustain을 넘은 것이다.
type syscallCondition struct {
	onSince  time.Time
	offSince time.Time
	peak     float64
	count    uint64
	event    *syscallAnomaly
}

// syscallEventDetector는 snapshot을 받을 때마다 지난 snapshot과의 증가분으로 조건을 본다. snapshot을 부르는 goroutine
// 하나에서만 쓴다.
type syscallEventDetector struct {
	previousMS   int64
	havePrevious bool
	calls        map[string]uint64
	errnos       map[string]uint64
	pathFailures map[string]uint64
	history      map[string][]float64
	conditions   map[string]*syscallCondition
	events       []*syscallAnomaly
	// now는 snapshot을 읽은 시각이다. test가 바꾼다.
	now func() time.Time
}

func newSyscallEventDetector() *syscallEventDetector {
	return &syscallEventDetector{calls: map[string]uint64{}, errnos: map[string]uint64{}, pathFailures: map[string]uint64{}, history: map[string][]float64{}, conditions: map[string]*syscallCondition{}, now: time.Now}
}

func syscallBenignErrno(syscall, errno string) bool {
	return syscallBenignErrnos[errno] || syscallBenignPairs[syscall+" "+errno]
}

// observe는 snapshot 하나로 조건을 갱신한다. at은 snapshot을 읽은 시각이다. files는 파일 표의 지금까지의 행이고, 파일 표를
// 끈 trace에서는 nil이다.
func (detector *syscallEventDetector) observe(at time.Time, collection syscallCollection, files []syscallFileRow) {
	currentCalls := map[string]uint64{}
	currentErrnos := map[string]uint64{}
	owners := map[string][2]string{}
	for _, sample := range collection.samples {
		key := sample.Process + "\x00" + sample.Syscall
		currentCalls[key] += sample.Calls
		owners[key] = [2]string{sample.Process, sample.Syscall}
		for errno, count := range sample.Errnos {
			currentErrnos[key+"\x00"+errno] += count
		}
	}
	currentFailures := map[string]uint64{}
	for _, row := range files {
		for errno, count := range row.Errnos {
			if !syscallBenignErrnos[errno] {
				currentFailures[row.Path+"\x00"+errno] += count
			}
		}
	}
	elapsedMS := collection.summary.DurationMS
	seconds := float64(elapsedMS-detector.previousMS) / 1000
	hits := map[string]syscallHit{}
	if detector.havePrevious && seconds > 0 {
		for key, calls := range currentCalls {
			rate := float64(calls-min(calls, detector.calls[key])) / seconds
			owner := owners[key]
			spiking := detector.spike(key, rate, calls-min(calls, detector.calls[key]), hits, owner)
			if !spiking {
				history := append(detector.history[key], rate)
				detector.history[key] = history[max(0, len(history)-syscallBaselineSamples):]
			}
		}
		for key, count := range currentErrnos {
			delta := count - min(count, detector.errnos[key])
			rate := float64(delta) / seconds
			parts := strings.SplitN(key, "\x00", 3)
			if rate >= syscallErrorLoopRate && !syscallBenignErrno(parts[1], parts[2]) {
				hits["error\x00"+key] = syscallHit{proto: syscallAnomaly{Kind: syscallKindErrorLoop, Process: parts[0], Syscall: parts[1], Errno: parts[2]}, rate: rate, count: delta}
			}
		}
		for key, count := range currentFailures {
			delta := count - min(count, detector.pathFailures[key])
			rate := float64(delta) / seconds
			path, errno, _ := strings.Cut(key, "\x00")
			if rate >= syscallMissingPathRate {
				hits["path\x00"+key] = syscallHit{proto: syscallAnomaly{Kind: syscallKindMissingPath, Path: path, Errno: errno}, rate: rate, count: delta}
			}
		}
	}
	detector.calls, detector.errnos, detector.pathFailures = currentCalls, currentErrnos, currentFailures
	detector.previousMS, detector.havePrevious = elapsedMS, true
	detector.update(at, hits)
}

// spike는 호출 급증 조건을 본다. 급증이 이어지는 동안은 평소 평균에 넣지 않아야 평균이 오르며 조건이 꺼지지 않는다.
func (detector *syscallEventDetector) spike(key string, rate float64, delta uint64, hits map[string]syscallHit, owner [2]string) bool {
	history := detector.history[key]
	_, tracked := detector.conditions["spike\x00"+key]
	if len(history) < syscallBaselineMin {
		return tracked
	}
	var sum float64
	for _, value := range history {
		sum += value
	}
	baseline := sum / float64(len(history))
	if rate >= syscallSpikeRate && rate >= syscallSpikeFactor*math.Max(baseline, 1) {
		hits["spike\x00"+key] = syscallHit{proto: syscallAnomaly{Kind: syscallKindCallSpike, Process: owner[0], Syscall: owner[1]}, rate: rate, count: delta, baseline: &baseline}
		return true
	}
	return tracked
}

// update는 켜진 조건과 꺼진 조건으로 event를 만들고 닫는다.
func (detector *syscallEventDetector) update(at time.Time, hits map[string]syscallHit) {
	for key, hit := range hits {
		state := detector.conditions[key]
		if state == nil {
			state = &syscallCondition{}
			detector.conditions[key] = state
		}
		state.offSince = time.Time{}
		if state.onSince.IsZero() {
			state.onSince = at
		}
		state.peak = math.Max(state.peak, hit.rate)
		state.count += hit.count
		if state.event == nil && at.Sub(state.onSince) >= syscallEventSustain {
			event := hit.proto
			event.Start, event.PeakPerSec, event.Count, event.BaselinePerSec = state.onSince, state.peak, state.count, hit.baseline
			state.event = &event
			detector.events = append(detector.events, state.event)
		}
		if event := state.event; event != nil {
			event.Active, event.End = true, at
			event.PeakPerSec = math.Max(event.PeakPerSec, hit.rate)
			if event.Count < state.count {
				event.Count = state.count
			}
		}
	}
	for key, state := range detector.conditions {
		if _, on := hits[key]; on {
			continue
		}
		if state.event == nil {
			delete(detector.conditions, key)
			continue
		}
		state.event.Active = false
		if state.offSince.IsZero() {
			state.offSince = at
		}
		if at.Sub(state.offSince) >= syscallEventSustain {
			delete(detector.conditions, key)
		}
	}
	detector.trim()
}

// trim은 보관 수를 넘으면 끝난 event 중 가장 오래된 것부터 버린다.
func (detector *syscallEventDetector) trim() {
	for len(detector.events) > syscallEventKeep {
		dropped := false
		for index, event := range detector.events {
			if !event.Active {
				detector.events = append(detector.events[:index], detector.events[index+1:]...)
				dropped = true
				break
			}
		}
		if !dropped {
			detector.events = detector.events[1:]
		}
	}
}

// report는 시작 시각 순서의 event 사본이다.
func (detector *syscallEventDetector) report() []syscallAnomaly {
	if detector == nil || len(detector.events) == 0 {
		return nil
	}
	events := make([]syscallAnomaly, len(detector.events))
	for index, event := range detector.events {
		events[index] = *event
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].Start.Before(events[j].Start) })
	return events
}

// printEvents는 event 표를 쓴다. 실시간 화면(live)은 지금 켜진 것을 앞에 두고 가장 최근 것부터 limit개를 보이고, 최종 표는
// 시작 순서로 보인다. ● 표시는 조건이 지금 켜져 있다는 뜻이다.
func (report syscallReport) printEvents(out io.Writer, style traceSummaryStyle, limit int, live bool) {
	events := append([]syscallAnomaly(nil), report.Events...)
	if live {
		sort.SliceStable(events, func(i, j int) bool {
			if events[i].Active != events[j].Active {
				return events[i].Active
			}
			return events[i].Start.After(events[j].Start)
		})
	}
	shown := events[:min(len(events), limit)]
	io.WriteString(out, "\n")
	if len(events) == 0 {
		style.line(out, style.subheading("EVENTS")+style.dim(": none so far. edc watches for error loops, missing paths, and call spikes that last 5 seconds"))
		return
	}
	heading := ": a condition that held for 5 seconds. ● means it is still on"
	if live {
		heading = fmt.Sprintf(": %d in total, the newest first. ● means it is still on", len(events))
	}
	style.line(out, style.subheading("EVENTS")+style.dim(heading))
	style.line(out, style.dim(fmt.Sprintf("  %-8s %7s  %-12s %9s %10s  %s", "START", "FOR", "KIND", "PEAK", "COUNT", "TARGET")))
	for _, event := range shown {
		mark := " "
		if event.Active {
			mark = "●"
		}
		style.printf(out, "%s %-8s %7s  %-12s %9s %10s  %s\n", mark, event.Start.Format("15:04:05"), syscallTime(float64(event.End.Sub(event.Start))), event.kindName(), groupThousands(int64(event.PeakPerSec))+"/s", groupThousands(int64(event.Count)), execPrintable([]byte(event.target())))
	}
	if hidden := len(events) - len(shown); hidden > 0 && !live {
		style.line(out, style.hint(syscallHiddenRows(hidden, false)))
	}
}
