package edc

import (
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
		err := model.recorder.Close("finished")
		model.recorder, model.record, model.recordFailure = nil, nil, nil
		if err != nil {
			model.recordingErr = err
			return model, tea.Quit
		}
		model.notice = T("history.recording.stopped", model.recordCount)
		return model, nil
	}
	path := model.recordPath
	if path == "" {
		var err error
		path, err = defaultHistoryPath()
		if err != nil {
			model.notice = T("history.error.write", err)
			return model, nil
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			model.notice = T("history.error.write", err)
			return model, nil
		}
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
