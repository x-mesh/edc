package edc

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	tea "charm.land/bubbletea/v2"
)

func (model *topModel) attachRecorder(recorder *topRecorder) {
	model.recorder = recorder
	model.record = recorder.Record
	model.recordFailure = func() tea.Msg {
		select {
		case <-recorder.failed:
			return topRecordingErrorMsg{err: recorder.Err(), source: recorder}
		case <-recorder.done:
			return nil
		}
	}
}

func (model topModel) toggleRecording() (tea.Model, tea.Cmd) {
	if model.recorder != nil {
		if model.autoRecord != nil {
			// 사용자가 끈 기록을 다음 sample이 다시 열지 않게, 조건이 한 번 풀릴 때까지 자동 기록을 멈춘다.
			model.autoRecord.muted = true
		}
		return model.stopRecording()
	}
	path, err := model.historyPath()
	if err != nil {
		model.notice = T("history.error.write", err)
		return model, nil
	}
	recorder, err := newTopRecorder(path, model.version, model.details, model.interval, model.processFilter.String(), model.interfaces.String(), model.probeEnabled)
	if err != nil {
		model.notice = T("history.error.write", err)
		return model, nil
	}
	model.attachRecorder(recorder)
	model.recordCount, model.recordFrom = 0, time.Now()
	model.notice = T("history.recording.started", reportIdentityValue(path))
	model.recordNotice = true
	return model, model.recordFailure
}

func (model topModel) stopRecording() (tea.Model, tea.Cmd) {
	err := model.recorder.Close("finished")
	model.recorder, model.record, model.recordFailure = nil, nil, nil
	if model.autoRecord != nil {
		model.autoRecord.trigger, model.autoRecord.calmSince = "", time.Time{}
	}
	if err != nil {
		model.recordingErr = err
		return model, tea.Quit
	}
	model.notice = T("history.recording.stopped", model.recordCount)
	return model, nil
}

func (model topModel) historyPath() (string, error) {
	if model.recordPath != "" {
		return model.recordPath, nil
	}
	path, err := defaultHistoryPath()
	if err != nil {
		return "", err
	}
	return path, os.MkdirAll(filepath.Dir(path), 0o700)
}

const (
	// topAutoRecordWindow는 trigger 전에 메모리에 두는 구간이다. 부하가 오르기 시작한 때부터 run에 남긴다.
	topAutoRecordWindow = 5 * time.Minute
	// topAutoRecordTail은 조건이 모두 풀린 뒤에도 기록을 이어 가는 구간이다. 그 안에 다시 걸리면 같은 run을 늘린다.
	topAutoRecordTail = time.Minute
	// topAutoRecordLoadFactor는 load1을 코어 수의 몇 배부터 이상으로 볼지다.
	topAutoRecordLoadFactor = 2
)

// topAutoRecord는 --auto-record의 상태다. 모델은 값으로 복사되므로 포인터로 둔다. trigger가 비어 있지 않으면
// 지금 recorder는 자동으로 연 run이다.
type topAutoRecord struct {
	samples   []historyTopSample
	trigger   string
	calmSince time.Time
	muted     bool
}

// topAutoRecordTrigger는 이 구간이 이상인 이유다. 이상이 없으면 빈 문자열이다.
func topAutoRecordTrigger(rate resourceRate, cores int) string {
	if limit := float64(topAutoRecordLoadFactor * cores); cores > 0 && rate.Load1 > limit {
		return fmt.Sprintf("load %.0f > %d×%d", rate.Load1, topAutoRecordLoadFactor, cores)
	}
	if rate.SwapOut > 0 {
		return "swap out " + formatBytes(uint64(rate.SwapOut)) + "/s"
	}
	switch rate.MemoryPressure {
	case topMemoryPressureWarn:
		return "memory pressure warn"
	case topMemoryPressureCritical:
		return "memory pressure critical"
	}
	return ""
}

// observeAutoRecord는 sample을 받은 뒤 부른다. 기록 중이 아니면 버퍼에 담고, trigger가 걸리면 버퍼째 새 run을 연다.
// 자동으로 연 run은 조건이 topAutoRecordTail 동안 풀려 있으면 닫는다.
func (model topModel) observeAutoRecord(sample historyTopSample, rate resourceRate) (topModel, tea.Cmd) {
	auto := model.autoRecord
	if auto == nil {
		return model, nil
	}
	trigger := topAutoRecordTrigger(rate, model.details.Cores)
	if auto.trigger != "" {
		switch {
		case trigger != "":
			auto.calmSince = time.Time{}
		case auto.calmSince.IsZero():
			auto.calmSince = sample.Time
		case sample.Time.Sub(auto.calmSince) >= topAutoRecordTail:
			next, cmd := model.stopRecording()
			return next.(topModel), cmd
		}
		return model, nil
	}
	if model.recorder != nil {
		auto.samples = nil
		return model, nil
	}
	auto.samples = append(auto.samples, sample)
	cut := 0
	for cut < len(auto.samples) && sample.Time.Sub(auto.samples[cut].Time) > topAutoRecordWindow {
		cut++
	}
	auto.samples = auto.samples[cut:]
	if trigger == "" {
		auto.muted = false
		return model, nil
	}
	if auto.muted {
		return model, nil
	}
	path, err := model.historyPath()
	if err != nil {
		model.notice = T("history.error.write", err)
		return model, nil
	}
	recorder, err := newTriggeredTopRecorder(path, model.version, model.details, model.interval, model.processFilter.String(), model.interfaces.String(), model.probeEnabled, trigger, auto.samples)
	if err != nil {
		model.notice = T("history.error.write", err)
		return model, nil
	}
	model.attachRecorder(recorder)
	// 다음 sample의 시작점이 이 sample의 끝이므로 그 sample부터 기록한다.
	model.recordCount, model.recordFrom = len(auto.samples), sample.Time
	auto.samples, auto.trigger, auto.calmSince = nil, trigger, time.Time{}
	model.notice = T("history.recording.auto_started", trigger, reportIdentityValue(path))
	model.recordNotice = true
	return model, model.recordFailure
}
