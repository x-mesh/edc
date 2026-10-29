package edc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// macOS의 events는 커널 로그 대신 ReportCrash가 남긴 진단 보고서를 읽는다. unified log의 커널 memorystatus 줄은
// 보존되지 않아 jetsam이 있던 시각에도 비어 있고, runningboardd의 memorystatus 줄은 메모리 한도 변환 오류라 종료가 아니다.

const (
	eventsJetsam = "jetsam"
	eventsPanic  = "panic"
	// eventsReportLimit은 보고서 하나를 읽는 상한이다. JetsamEvent는 process 1000여 개를 담아 수백 KB다.
	eventsReportLimit = 16 << 20
	// ReportCrash 보고서 첫 줄의 bug_type이다. 298은 JetsamEvent, 210은 커널 패닉이다.
	eventsJetsamBugType = "298"
	eventsPanicBugType  = "210"
)

// eventsReportDirs는 시스템 진단 보고서 위치다. 오래된 보고서는 Retired로 옮겨진다.
var eventsReportDirs = []string{"/Library/Logs/DiagnosticReports", "/Library/Logs/DiagnosticReports/Retired"}

type macEventsFacts struct {
	Now, Start time.Time
	Since      time.Duration
	Signals    []eventsSignal
	// Err는 첫 보고서 위치를 읽지 못한 이유다. 비어 있지 않으면 판정하지 않는다.
	Err      error
	Warnings []string
}

func collectMacEventsFacts(dirs []string, now time.Time, since time.Duration) macEventsFacts {
	facts := macEventsFacts{Now: now, Start: now.Add(-since), Since: since}
	for index, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			// Retired는 보고서가 오래되기 전까지 없을 수 있다. 첫 위치를 못 읽으면 결과가 없는 것이 아니라 모르는 것이다.
			if index == 0 {
				facts.Err = err
				return facts
			}
			if !errors.Is(err, fs.ErrNotExist) {
				facts.Warnings = append(facts.Warnings, T("events.skip.unreadable", dir, infoSingleLine(err.Error())))
			}
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !(strings.HasSuffix(name, ".ips") || strings.HasSuffix(name, ".panic")) {
				continue
			}
			info, err := entry.Info()
			// 보고서는 사건 직후에 쓰이므로 창보다 먼저 바뀐 파일은 열지 않는다.
			if err != nil || info.ModTime().Before(facts.Start) {
				continue
			}
			path := filepath.Join(dir, name)
			data, err := readEventsReport(path)
			if errors.Is(err, errEventsOtherReport) {
				continue
			}
			if err != nil {
				facts.Warnings = append(facts.Warnings, T("events.skip.unreadable", path, infoSingleLine(err.Error())))
				continue
			}
			for _, signal := range parseMacEventsReport(data, now.Location()) {
				if !signal.Time.Before(facts.Start) {
					facts.Signals = append(facts.Signals, signal)
				}
			}
		}
	}
	sort.SliceStable(facts.Signals, func(i, j int) bool { return facts.Signals[i].Time.Before(facts.Signals[j].Time) })
	return facts
}

// errEventsOtherReport는 jetsam이나 패닉이 아닌 보고서다. 앱 crash와 .diag는 수 MB라 첫 줄만 보고 넘긴다.
var errEventsOtherReport = errors.New("not a jetsam or panic report")

// readEventsReport는 첫 줄의 bug_type이 jetsam이나 패닉일 때만 전체를 읽는다. 파일 이름 규칙은 macOS 판마다 달라 기대지 않는다.
func readEventsReport(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	reader := bufio.NewReader(io.LimitReader(file, eventsReportLimit+1))
	head, err := reader.ReadSlice('\n')
	if err != nil {
		return nil, errEventsOtherReport
	}
	var header struct {
		BugType string `json:"bug_type"`
	}
	if json.Unmarshal(head, &header) != nil || (header.BugType != eventsJetsamBugType && header.BugType != eventsPanicBugType) {
		return nil, errEventsOtherReport
	}
	// ReadSlice의 결과는 reader의 버퍼라 다음 읽기가 덮어쓴다. 나머지를 읽기 전에 복사한다.
	data := append([]byte(nil), head...)
	rest, err := io.ReadAll(reader)
	data = append(data, rest...)
	if err == nil && len(data) > eventsReportLimit {
		err = fmt.Errorf("larger than %d bytes", eventsReportLimit)
	}
	return data, err
}

// parseMacEventsReport는 보고서 하나에서 신호를 꺼낸다. 첫 줄은 bug_type과 시각을 담은 JSON이고, 나머지가 본문 JSON이다.
func parseMacEventsReport(data []byte, location *time.Location) []eventsSignal {
	head, body, _ := bytes.Cut(data, []byte("\n"))
	var header struct {
		BugType   string `json:"bug_type"`
		Timestamp string `json:"timestamp"`
	}
	if json.Unmarshal(head, &header) != nil {
		return nil
	}
	at, err := time.Parse("2006-01-02 15:04:05.00 -0700", header.Timestamp)
	if err != nil {
		return nil
	}
	at = at.In(location)
	switch header.BugType {
	case eventsJetsamBugType:
		return parseJetsamReport(body, at)
	case eventsPanicBugType:
		var report struct {
			PanicString string `json:"panicString"`
		}
		_ = json.Unmarshal(body, &report)
		message := strings.TrimSpace(report.PanicString)
		if message == "" {
			message = "kernel panic"
		}
		return []eventsSignal{{Kind: eventsPanic, Time: at, Brief: firstLine(message), Message: message}}
	}
	return nil
}

// parseJetsamReport는 jetsam이 종료한 process마다 신호를 만든다. 종료된 process만 reason을 가진다.
func parseJetsamReport(body []byte, at time.Time) []eventsSignal {
	var report struct {
		Largest string `json:"largestProcess"`
		Memory  struct {
			PageSize uint64 `json:"pageSize"`
		} `json:"memoryStatus"`
		Processes []struct {
			Name   string `json:"name"`
			PID    int    `json:"pid"`
			Reason string `json:"reason"`
			Pages  uint64 `json:"rpages"`
		} `json:"processes"`
	}
	if json.Unmarshal(body, &report) != nil {
		return nil
	}
	var signals []eventsSignal
	for _, process := range report.Processes {
		if process.Reason == "" {
			continue
		}
		brief := fmt.Sprintf("%s (%s)", process.Name, process.Reason)
		message := fmt.Sprintf("%s pid %d killed by jetsam: %s, resident %s", process.Name, process.PID, process.Reason, formatBytes(process.Pages*report.Memory.PageSize))
		if report.Largest != "" {
			message += ", largest process " + report.Largest
		}
		signals = append(signals, eventsSignal{Kind: eventsJetsam, Time: at, Subject: process.Name, Brief: brief, Message: message})
	}
	return signals
}

func evaluateMacEvents(facts macEventsFacts) []Result {
	if facts.Err != nil {
		result := eventsResult("window", StatusSkip, T("events.skip.reports", eventsReportDirs[0], infoSingleLine(facts.Err.Error())))
		if errors.Is(facts.Err, fs.ErrPermission) {
			result = eventsResult("window", StatusSkip, T("events.skip.reports_admin", eventsReportDirs[0]))
		}
		return []Result{result}
	}
	byKind := map[string][]eventsSignal{}
	for _, signal := range facts.Signals {
		byKind[signal.Kind] = append(byKind[signal.Kind], signal)
	}
	since := eventsDurationText(facts.Since)
	window := eventsResult("window", StatusPass, T("events.window.summary", len(facts.Signals), since, facts.Start.Format(eventsTimeLayout)))
	if len(facts.Signals) > 0 {
		lines := make([]string, 0, len(facts.Signals))
		for _, signal := range facts.Signals {
			lines = append(lines, fmt.Sprintf("%s  %-11s  %s", signal.Time.Format(eventsTimeLayout), signal.Kind, signal.Brief))
		}
		window.Evidence = append(window.Evidence, Evidence{Label: T("events.evidence.timeline"), Value: eventsRecent(lines, eventsTimelineLines)})
	}
	window.Evidence = append(window.Evidence, Evidence{Label: T("events.evidence.source"), Value: fmt.Sprintf("reports: %s\nwindow: %s .. %s", strings.Join(eventsReportDirs, ", "), facts.Start.Format(eventsTimeLayout), facts.Now.Format(eventsTimeLayout))})
	window.Warnings = facts.Warnings
	return []Result{
		window,
		macEventsCategory(facts, eventsPanic, byKind[eventsPanic], StatusFail),
		// jetsam은 per-process-limit처럼 한 process가 자기 한도를 넘은 경우가 흔해 시스템 장애로 판정하지 않는다.
		macEventsCategory(facts, eventsJetsam, byKind[eventsJetsam], StatusWarn),
	}
}

func macEventsCategory(facts macEventsFacts, id string, signals []eventsSignal, status Status) Result {
	start := facts.Start.Format(eventsTimeLayout)
	if len(signals) == 0 {
		return eventsResult(id, StatusPass, T("events."+id+".none", start))
	}
	result := eventsResult(id, status, T("events."+id+".found", len(signals), eventsLatest(signals[len(signals)-1])))
	if status == StatusFail {
		result.Error = &DiagnosticError{Kind: "events", Message: result.Summary}
	}
	result.Evidence = eventsEvidence(signals)
	result.Next = "ls -lt " + eventsReportDirs[0]
	return result
}
