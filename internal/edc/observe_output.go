package edc

import (
	"fmt"
	"io"
	"os"
	"strings"
)

func observeOutputFamily(probe string) string {
	family, _, _ := strings.Cut(probe, ".")
	switch family {
	case "proc", "svc", "jobs", "changes":
		return family
	}
	return ""
}

func observeOutputResults(results []Result) bool {
	if len(results) == 0 {
		return false
	}
	family := observeOutputFamily(results[0].Probe)
	if family == "" {
		return false
	}
	for _, result := range results {
		if observeOutputFamily(result.Probe) != family {
			return false
		}
	}
	return true
}

func observeTerminalStyle(output *os.File) infoStyle {
	return infoStyle{textStyle{width: writerWidth(output), color: richEnabled(output)}}
}

func observeOutputText(value string) string {
	lines := strings.Split(value, "\n")
	for i, line := range lines {
		lines[i] = viewSanitize(line)
	}
	return strings.Join(lines, "\n")
}

func observeOutputLine(writer io.Writer, style infoStyle, indent int, text string) {
	if style.width > 0 {
		indent = min(indent, max(0, style.width-1))
	}
	prefix := strings.Repeat(" ", indent)
	style.write(writer, prefix, prefix, text)
}

func printObserveResult(writer io.Writer, result Result, verbose bool, style infoStyle) {
	if style.width <= 0 {
		fmt.Fprint(writer, formatResultLine(result, style.color))
		printResultDetail(writer, result, verbose, style.color)
		return
	}
	label := observeOutputText(resultLabel(result))
	if style.color {
		label = richBold(label)
	}
	header := terminalStatus(result.Status, style.color) + "  " + label
	summary := observeOutputText(result.Summary)
	line := header + "  " + summary
	if !strings.Contains(line, "\n") && liveWidth(line) <= style.width {
		fmt.Fprintln(writer, line)
	} else {
		observeOutputLine(writer, style, 0, header)
		observeOutputLine(writer, style, 4, summary)
	}
	if result.Status == StatusFail {
		observeOutputLine(writer, style, 2, style.bad("ERROR"))
		if result.Error != nil {
			observeOutputLine(writer, style, 4, style.kv("phase", observeOutputText(result.Error.Kind)))
			observeOutputLine(writer, style, 4, style.kv("cause", observeOutputText(result.Error.Message)))
		}
	}
	if result.Next != "" {
		// 인용된 경로 안의 강제 개행은 복사한 명령의 인자를 바꾸므로 터미널의 시각적 줄바꿈에 맡긴다.
		fmt.Fprintln(writer, "      "+style.kv("next", style.command(observeOutputText(result.Next))))
	}
	for _, warning := range result.Warnings {
		observeOutputLine(writer, style, 4, style.warn("warning: ")+observeOutputText(warning))
	}
	shown := verbose || evidenceShownProbes[result.Probe] || (result.Status == StatusWarn && warnEvidenceProbes[result.Probe]) || result.Status == StatusFail
	if shown {
		for _, evidence := range result.Evidence {
			observeOutputLine(writer, style, 4, style.dim(observeOutputText(evidence.Label)+":"))
			observeOutputLine(writer, style, 6, observeOutputText(strings.TrimRight(evidence.Value, "\n")))
		}
	}
}

func printObserveTerminal(writer io.Writer, results []Result, verbose bool, style infoStyle) {
	if style.width <= 0 {
		printTerminalWithColor(writer, results, verbose, style.color)
		return
	}
	for _, result := range results {
		printObserveResult(writer, result, verbose, style)
	}
	var footer strings.Builder
	printResultSummary(&footer, results, style.color)
	for _, line := range strings.Split(strings.TrimSuffix(footer.String(), "\n"), "\n") {
		observeOutputLine(writer, style, 0, line)
	}
}
