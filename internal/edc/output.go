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
	printResultSummary(writer, results)
}

// printResultTail은 결과 줄을 실행 중에 이미 출력한 실행의 마무리 출력이다.
func printResultTail(writer io.Writer, results []Result, verbose, color bool) {
	printResultDetails(writer, results, verbose, color)
	printResultSummary(writer, results)
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

func formatResultLine(result Result, color bool) string {
	// command 오류 summary는 여러 줄일 수 있다. 목록의 한 줄을 유지하려고 첫 줄만 쓴다. 전문은 실패 상자에 남는다.
	return fmt.Sprintf(resultLineFormat, terminalStatus(result.Status, color), resultLabel(result), firstLine(result.Summary))
}

func printResultDetail(writer io.Writer, result Result, verbose, color bool) {
	if result.Status == StatusFail {
		printFailureBox(writer, resultLabel(result), result, color)
	}
	for _, warning := range result.Warnings {
		fmt.Fprintf(writer, "      warning: %s\n", warning)
	}
	if verbose && result.Status != StatusFail && !isRemoteResult(result) {
		for _, evidence := range result.Evidence {
			fmt.Fprintf(writer, "      %s:\n%s\n", evidence.Label, indent(evidence.Value, "        "))
		}
	}
}

func printResultSummary(writer io.Writer, results []Result) {
	fmt.Fprintf(writer, "\n%s\n", resultCounts(results))
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
	title := "ERROR  " + probe
	if color {
		title = "\033[31m" + title + "\033[0m"
	}
	fmt.Fprintf(writer, "┌─ %s\n", title)
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
	return "\033[" + code + "m" + label + "\033[0m"
}

func isRemoteResult(result Result) bool {
	return strings.HasPrefix(result.Probe, "remote.")
}

func redactReport(report *Report) {
	hostname, _ := report.Host["hostname"].(string)
	patterns := []struct{ value, label string }{{hostname, "host"}}
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
