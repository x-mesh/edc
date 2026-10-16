package edc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strings"
	"time"
)

func buildReport(version string, started time.Time, target map[string]interface{}, results []Result, redact bool) Report {
	hostname, _ := os.Hostname()
	report := Report{
		SchemaVersion: "1.0", Tool: ToolInfo{Name: "edc", Version: version},
		Run:    RunInfo{ID: runID(started), StartedAt: started.UTC(), DurationMS: time.Since(started).Milliseconds()},
		Target: target, Host: map[string]interface{}{"hostname": hostname}, Results: results, Summary: summarize(results), Redaction: RedactionInfo{Enabled: redact},
	}
	if redact {
		redactReport(&report)
	}
	return report
}

func writeJSON(writer io.Writer, value interface{}) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

// writeJSONOutput은 "-"면 stdout에, 아니면 mode 0600으로 만든 파일에 JSON을 쓴다.
func writeJSONOutput(path string, value interface{}) error {
	if path == "-" {
		return writeJSON(os.Stdout, value)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	return writeJSON(file, value)
}

func printTerminal(writer io.Writer, results []Result, verbose bool) {
	printTerminalWithColor(writer, results, verbose, false)
}

func printTerminalWithColor(writer io.Writer, results []Result, verbose, color bool) {
	if len(results) > 0 && isRemoteResult(results[0]) {
		fmt.Fprintln(writer)
	}
	for _, result := range results {
		fmt.Fprint(writer, formatResultLine(result, color))
		printResultDetail(writer, result, verbose, color)
	}
	printResultSummary(writer, results, color)
}

// printResultTail은 결과 줄을 실행 중에 이미 출력한 실행의 마무리 출력이다.
func printResultTail(writer io.Writer, results []Result, verbose, color bool) {
	printResultDetails(writer, results, verbose, color)
	printResultSummary(writer, results, color)
}

// printResultDetails는 요약 없이 경고와 실패 상세만 출력한다. 요약을 직접 만드는 caller가 쓴다.
func printResultDetails(writer io.Writer, results []Result, verbose, color bool) {
	for _, result := range results {
		printResultDetail(writer, result, verbose, color)
	}
}

// resultLineFormat은 결과 줄과 실시간 화면의 대기 줄이 같은 열을 쓰게 한다.
const (
	resultLineFormat  = "%-4s  %-24s  %s\n"
	resultStatusWidth = 4
)

// resultLabelWidth는 resultLineFormat의 label 열 폭이다.
const resultLabelWidth = 24

func formatResultLine(result Result, color bool) string {
	// command 오류 summary는 여러 줄일 수 있다. 목록의 한 줄을 유지하려고 첫 줄만 쓴다. 전문은 실패 상자에 남는다.
	if color {
		// 배지와 굵은 글씨의 escape는 폭이 없으므로, 칸 맞춤은 색을 입히기 전의 label로 한다.
		label := fmt.Sprintf("%-*s", resultLabelWidth, resultLabel(result))
		return richStatusBadge(result.Status) + "  " + richBold(label) + "  " + firstLine(result.Summary) + "\n"
	}
	return fmt.Sprintf(resultLineFormat, terminalStatus(result.Status, color), resultLabel(result), firstLine(result.Summary))
}

// resultPendingStatus는 실시간 화면의 대기 줄 상태 칸이다. 끝난 줄의 상태 칸과 폭을 맞춘다.
func resultPendingStatus(spinner string, color bool) string {
	if color {
		return liveCell(spinner, richStatusBadgeWidth)
	}
	return liveCell(spinner, resultStatusWidth)
}

// evidenceShownProbes는 -v가 없어도 evidence를 보이는 probe다. 각 probe 파일이 init에서 자기 이름을 등록한다.
// 저장한 report를 report show로 다시 그릴 때도 같은 규칙을 써야 하므로, 표시를 JSON에 두지 않고 probe 이름으로 정한다.
var evidenceShownProbes = map[string]bool{}

func printResultDetail(writer io.Writer, result Result, verbose, color bool) {
	if result.Status == StatusFail {
		printFailureBox(writer, resultLabel(result), result, color)
	}
	if result.Next != "" {
		if color {
			fmt.Fprintf(writer, "      %s %s\n", richDim("next:"), richPaint(result.Next, richCyan, true))
		} else {
			fmt.Fprintf(writer, "      next: %s\n", result.Next)
		}
	}
	for _, warning := range result.Warnings {
		if color {
			fmt.Fprintf(writer, "      %s %s\n", richPaint("warning:", richYellow, true), warning)
		} else {
			fmt.Fprintf(writer, "      warning: %s\n", warning)
		}
	}
	if (verbose || evidenceShownProbes[result.Probe]) && result.Status != StatusFail && !isRemoteResult(result) {
		if lines := richEvidenceLinesWidth(color, result, writerWidth(writer)); lines != nil {
			for _, line := range lines {
				fmt.Fprintln(writer, line)
			}
			return
		}
		for _, evidence := range result.Evidence {
			if color {
				printEvidencePanel(writer, resultLabel(result), evidence)
				continue
			}
			fmt.Fprintf(writer, "      %s:\n%s\n", evidence.Label, indent(evidence.Value, "        "))
		}
	}
}

// printEvidencePanel은 evidence 하나를 회색 패널로 그린다. 실시간 화면 뒤에는 결과 줄과 떨어져 모이므로 제목 옆에 probe를 둔다.
func printEvidencePanel(writer io.Writer, probe string, evidence Evidence) {
	fmt.Fprintln(writer, richPanelTop(evidence.Label, probe, ""))
	for _, line := range evidenceValueLines(evidence.Value) {
		fmt.Fprintln(writer, richPanelLine(line, ""))
	}
	fmt.Fprintln(writer, richPanelBottom(""))
}

func evidenceValueLines(value string) []string {
	return strings.Split(strings.TrimRight(value, "\n"), "\n")
}

func printResultSummary(writer io.Writer, results []Result, color bool) {
	if color {
		fmt.Fprintf(writer, "\n%s\n", richResultCounts(results, writerWidth(writer)))
		return
	}
	fmt.Fprintf(writer, "\n%s\n", resultCounts(results))
}

// resultCountsBarWidth는 요약 줄 개수 막대의 폭이다. 좁은 terminal에서는 줄 폭 안으로 줄인다.
const resultCountsBarWidth = 36

// richResultCounts는 개수 막대와 상태별 개수다. 0인 개수는 회색, 나머지는 그 상태 색으로 굵게 쓴다.
func richResultCounts(results []Result, width int) string {
	s := summarize(results)
	counts := []struct {
		count  int
		status Status
	}{{s.Pass, StatusPass}, {s.Warn, StatusWarn}, {s.Fail, StatusFail}, {s.Skip, StatusSkip}}
	parts := make([]string, 0, len(counts))
	segments := make([]richSegment, 0, len(counts))
	for _, entry := range counts {
		text := fmt.Sprintf("%d %s", entry.count, entry.status)
		if entry.count == 0 {
			parts = append(parts, richDim(text))
		} else {
			parts = append(parts, richPaint(text, richStatusColor(entry.status), true))
		}
		segments = append(segments, richSegment{value: float64(entry.count), color: richStatusColor(entry.status)})
	}
	text := strings.Join(parts, "  ")
	// 결과가 하나면 막대가 한 색으로 꽉 차서 개수 글자 이상을 알려 주지 않는다.
	if len(results) < 2 {
		return text
	}
	bar := richSegments(segments, min(resultCountsBarWidth, width-liveWidth(text)-2))
	// 칸이 상태 수보다 적으면 richSegments가 상태마다 한 칸을 주어 줄이 폭을 넘는다.
	if bar == "" || liveWidth(bar)+2+liveWidth(text) > width {
		return text
	}
	return bar + "  " + text
}

func resultCounts(results []Result) string {
	s := summarize(results)
	return fmt.Sprintf("%d pass  ·  %d warn  ·  %d fail  ·  %d skip", s.Pass, s.Warn, s.Fail, s.Skip)
}

// compactResultCounts는 완료 줄 뒤에 붙는 짧은 개수 표기다. 구분점을 겹쳐 쓰지 않는다.
func compactResultCounts(results []Result) string {
	s := summarize(results)
	return fmt.Sprintf("%d pass  %d warn  %d fail  %d skip", s.Pass, s.Warn, s.Fail, s.Skip)
}

func resultLabel(result Result) string {
	return strings.TrimPrefix(result.Probe, "remote.")
}

func printFailureBox(writer io.Writer, probe string, result Result, color bool) {
	if color {
		printFailurePanel(writer, probe, result)
		return
	}
	fmt.Fprintf(writer, "┌─ ERROR  %s\n", probe)
	if result.Error != nil {
		fmt.Fprintf(writer, "│ phase   %s\n", result.Error.Kind)
		fmt.Fprintf(writer, "│ cause   %s\n", result.Error.Message)
	}
	for _, evidence := range result.Evidence {
		fmt.Fprintf(writer, "│\n│ %s\n", evidence.Label)
		for _, line := range strings.Split(strings.TrimRight(evidence.Value, "\n"), "\n") {
			fmt.Fprintf(writer, "│   %s\n", line)
		}
	}
	fmt.Fprintln(writer, "└─")
}

// printFailurePanel은 색을 쓸 때의 실패 상자다. 실행 중에 결과 줄을 먼저 그린 doctor는 상자를 모아서 출력하므로
// 제목에 probe 이름을 남긴다.
func printFailurePanel(writer io.Writer, probe string, result Result) {
	top := richSGR("╭─ ", richRed) + richPaint("ERROR", richRed, true) + "  " + richBold(probe)
	if result.Error != nil && result.Error.Kind != "" {
		top += "  " + richPaint(result.Error.Kind, richYellow, false)
	}
	fmt.Fprintln(writer, top+richSGR(" ───", richRed))
	if result.Error != nil {
		fmt.Fprintln(writer, richPanelLine(richDim("cause")+"  "+result.Error.Message, richRed))
	}
	// 줄 앞에 "│ "가 붙으므로 그림은 그만큼 좁게 그린다.
	if lines := richEvidenceLinesWidth(true, result, writerWidth(writer)-2); lines != nil {
		fmt.Fprintln(writer, richPanelLine("", richRed))
		for _, line := range lines {
			fmt.Fprintln(writer, richPanelLine(line, richRed))
		}
	} else {
		for _, evidence := range result.Evidence {
			fmt.Fprintln(writer, richPanelLine("", richRed))
			fmt.Fprintln(writer, richPanelLine(richBold(evidence.Label), richRed))
			for _, line := range evidenceValueLines(evidence.Value) {
				fmt.Fprintln(writer, richPanelLine("  "+line, richRed))
			}
		}
	}
	fmt.Fprintln(writer, richPanelBottom(richRed))
}

func terminalStatus(status Status, color bool) string {
	label := strings.ToUpper(string(status))
	if !color {
		return label
	}
	code := ""
	switch status {
	case StatusPass:
		code = "32"
	case StatusFail:
		code = "31"
	case StatusWarn:
		code = "33"
	case StatusSkip:
		code = "90"
	}
	if code == "" {
		return label
	}
	return richSGR(label, code)
}

func isRemoteResult(result Result) bool {
	return strings.HasPrefix(result.Probe, "remote.")
}

func redactReport(report *Report) {
	hostname, _ := report.Host["hostname"].(string)
	patterns := []struct{ value, label string }{{hostname, "host"}}
	// listen --unix가 홈 아래의 소켓 경로를 내보내면 그 경로에 계정 이름이 들어간다. 주소만 가리고
	// 경로를 그대로 두면 같은 정보가 다른 열로 새어 나간다. 이름만 지우면 root처럼 짧고 흔한 계정에서
	// 관계없는 낱말까지 바뀌므로 홈 경로 전체를 가린다.
	if home, err := os.UserHomeDir(); err == nil && home != "" && home != "/" {
		patterns = append(patterns, struct{ value, label string }{home, "home"})
	}
	data, _ := json.Marshal(report)
	text := string(data)
	for _, pattern := range patterns {
		if pattern.value != "" {
			text = strings.ReplaceAll(text, pattern.value, token(pattern.label, pattern.value))
		}
	}
	text = redactIPAddresses(text)
	_ = json.Unmarshal([]byte(text), report)
}

var (
	ipv4Pattern = regexp.MustCompile(`(?:[0-9]{1,3}\.){3}[0-9]{1,3}`)
	ipv6Pattern = regexp.MustCompile(`[0-9A-Fa-f]*:[0-9A-Fa-f:.%]+`)
)

func redactIPAddresses(value string) string {
	value = ipv4Pattern.ReplaceAllStringFunc(value, func(candidate string) string {
		if net.ParseIP(candidate) == nil {
			return candidate
		}
		return token("ip", candidate)
	})
	return ipv6Pattern.ReplaceAllStringFunc(value, func(candidate string) string {
		clean := strings.Trim(candidate, "[]")
		if zoneIndex := strings.LastIndex(clean, "%"); zoneIndex >= 0 {
			clean = clean[:zoneIndex]
		}
		if net.ParseIP(clean) == nil {
			return candidate
		}
		return token("ip", candidate)
	})
}

func token(kind, value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("<%s:%s>", kind, hex.EncodeToString(sum[:4]))
}

func runID(started time.Time) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d-%d", started.UnixNano(), os.Getpid())))
	return hex.EncodeToString(sum[:8])
}

func indent(value, prefix string) string {
	return prefix + strings.ReplaceAll(value, "\n", "\n"+prefix)
}

func exitCode(results []Result) int {
	for _, result := range results {
		if result.Status == StatusFail {
			return 1
		}
	}
	return 0
}

// richEvidenceLines는 색을 쓸 때 probe 전용 그림이 있으면 그 줄을 돌려준다. evidence가 없으면 그리지 않는다.
func richEvidenceLines(color bool, result Result) []string {
	return richEvidenceLinesWidth(color, result, terminalWidth())
}

func richEvidenceLinesWidth(color bool, result Result, width int) []string {
	if !color || len(result.Evidence) == 0 {
		return nil
	}
	return richEvidence(result, width)
}
