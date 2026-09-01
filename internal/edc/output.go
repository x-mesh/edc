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

func writeJSON(writer io.Writer, report Report) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func printTerminal(writer io.Writer, results []Result, verbose bool) {
	printTerminalWithColor(writer, results, verbose, false)
}

func printTerminalWithColor(writer io.Writer, results []Result, verbose, color bool) {
	remote := len(results) > 0 && isRemoteResult(results[0])
	if remote {
		fmt.Fprintln(writer)
	}
	for _, result := range results {
		status := terminalStatus(result.Status, color)
		probe := result.Probe
		if remote {
			probe = strings.TrimPrefix(probe, "remote.")
		}
		fmt.Fprintf(writer, "%-4s  %-24s  %s\n", status, probe, result.Summary)
		if result.Status == StatusFail {
			printFailureBox(writer, probe, result, color)
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
	s := summarize(results)
	fmt.Fprintf(writer, "\n%d pass  ·  %d warn  ·  %d fail  ·  %d skip\n", s.Pass, s.Warn, s.Fail, s.Skip)
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
	username := os.Getenv("USER")
	patterns := []struct{ value, label string }{{hostname, "host"}, {username, "user"}}
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
