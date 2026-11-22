package edc

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

const topRecordingSummaryNameWidth = 80

type topRecordingSummary struct {
	runID, path, trigger, status, failure string
	started, first, last                  time.Time
	samples                               int
	cpuPeak                               float64
	cpuKnown                              bool
	memoryFirst, memoryPeak, memoryLast   float64
	memoryFirstAt, memoryLastAt           time.Time
	memoryKnown                           bool
	processPID                            int
	processCommand                        string
	processRSS                            uint64
	processKnown                          bool
}

func (summary topRecordingSummary) observe(sample historyTopSample) topRecordingSummary {
	summary.samples++
	if !sample.Time.IsZero() {
		if summary.first.IsZero() || sample.Time.Before(summary.first) {
			summary.first = sample.Time
		}
		if summary.last.IsZero() || sample.Time.After(summary.last) {
			summary.last = sample.Time
		}
	}
	if historyFinite(sample.CPU) && sample.CPU >= 0 {
		if !summary.cpuKnown || sample.CPU > summary.cpuPeak {
			summary.cpuPeak = sample.CPU
		}
		summary.cpuKnown = true
	}
	if historyFinite(sample.MemoryPct) && sample.MemoryPct >= 0 {
		if !summary.memoryKnown || sample.Time.Before(summary.memoryFirstAt) {
			summary.memoryFirst, summary.memoryFirstAt = sample.MemoryPct, sample.Time
		}
		if !summary.memoryKnown || sample.Time.After(summary.memoryLastAt) {
			summary.memoryLast, summary.memoryLastAt = sample.MemoryPct, sample.Time
		}
		if !summary.memoryKnown || sample.MemoryPct > summary.memoryPeak {
			summary.memoryPeak = sample.MemoryPct
		}
		summary.memoryKnown = true
	}
	if sample.ProcessesValid && sample.Processes != nil {
		for _, process := range *sample.Processes {
			if process.RSSBytes == nil {
				continue
			}
			if !summary.processKnown || *process.RSSBytes > summary.processRSS {
				summary.processPID, summary.processRSS = process.PID, *process.RSSBytes
				summary.processCommand = ansi.Truncate(topPrintableText(process.Command), topRecordingSummaryNameWidth, "…")
				summary.processKnown = true
			}
		}
	}
	return summary
}

func (model *topModel) finishRecording(status string) error {
	recorder := model.recorder
	if recorder == nil {
		return nil
	}
	err := recorder.Close(status)
	if recorder.summary.trigger != "" {
		summary := recorder.summary
		summary.status = status
		if err != nil {
			summary.status = "error"
			summary.failure = infoSingleLine(err.Error())
		}
		if model.recordingSummary == nil || model.recordingSummary.runID != summary.runID {
			model.recordingsFinished++
		}
		model.recordingSummary = &summary
	}
	model.recorder, model.record, model.recordFailure = nil, nil, nil
	if model.autoRecord != nil {
		model.autoRecord.trigger, model.autoRecord.calmSince = "", time.Time{}
	}
	return err
}

func topRecordingReviewCommand(summary topRecordingSummary) string {
	quote := func(value string) string {
		if strings.ContainsFunc(value, unicode.IsControl) {
			quoted := strconv.Quote(value)
			return "$'" + strings.ReplaceAll(quoted[1:len(quoted)-1], "'", "\\'") + "'"
		}
		return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
	}
	return "edc history overview --run " + quote(summary.runID) + " " + quote(summary.path)
}

func printTopRecordingSummary(writer io.Writer, summary *topRecordingSummary, count, width int, color bool) {
	if summary == nil {
		return
	}
	style := infoStyle{textStyle{color: color, width: width}}
	fmt.Fprintln(writer)
	heading := T("history.recording.summary.saved", summary.runID)
	if summary.status != "finished" {
		heading = T("history.recording.summary.failed", summary.runID)
	}
	style.line(writer, style.heading(heading))
	if count > 1 {
		style.line(writer, T("history.recording.summary.latest", count))
	}
	if summary.samples > 0 && !summary.first.IsZero() && !summary.last.IsZero() {
		from := summary.started
		if from.IsZero() || from.After(summary.first) {
			from = summary.first
		}
		style.tree(writer, "├──", T("history.recording.summary.window", from.Local().Format(time.RFC3339), summary.last.Local().Format(time.RFC3339)))
	} else {
		style.tree(writer, "├──", T("history.recording.summary.no_samples"))
	}
	style.tree(writer, "├──", T("history.recording.summary.trigger", topPrintableText(summary.trigger)))
	style.tree(writer, "├──", T("history.recording.summary.samples", summary.samples))
	if summary.cpuKnown {
		style.tree(writer, "├──", T("history.recording.summary.cpu", summary.cpuPeak))
	}
	if summary.memoryKnown {
		style.tree(writer, "├──", T("history.recording.summary.memory", summary.memoryFirst, summary.memoryPeak, summary.memoryLast))
	}
	if summary.processKnown {
		style.tree(writer, "└──", T("history.recording.summary.process", summary.processCommand, summary.processPID, formatBytes(summary.processRSS)))
	} else {
		style.tree(writer, "└──", T("history.recording.summary.process_unavailable"))
	}
	if summary.failure != "" {
		style.line(writer, style.warn(T("history.recording.summary.error", topPrintableText(summary.failure))))
	}
	style.line(writer, T("history.recording.summary.review"))
	// 터미널의 시각적 줄바꿈은 허용하되, 인용된 경로 안에 개행을 넣으면 복사한 명령의 인자가 바뀐다.
	fmt.Fprintln(writer, topRecordingReviewCommand(*summary))
}
