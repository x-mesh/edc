package edc

import (
	"encoding/json"
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
	details := result
	details.Next = ""
	var buffer strings.Builder
	printResultDetail(&buffer, details, verbose, false)
	for _, line := range strings.Split(strings.TrimSuffix(buffer.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		observeOutputLine(writer, style, indent, observeOutputText(strings.TrimLeft(line, " ")))
	}
	if result.Next != "" {
		// 인용된 경로 안의 강제 개행은 복사한 명령의 인자를 바꾸므로 터미널의 시각적 줄바꿈에 맡긴다.
		fmt.Fprintln(writer, "      "+style.kv("next", style.command(observeOutputText(result.Next))))
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
	printResultSummary(&footer, results, false)
	footerStyle := style
	footerStyle.color = false
	for _, line := range strings.Split(strings.TrimSuffix(footer.String(), "\n"), "\n") {
		observeOutputLine(writer, footerStyle, 0, line)
	}
}

func observeOutputRedactedResults(results []Result) ([]Result, error) {
	copies := make([]Result, len(results))
	for i, result := range results {
		copies[i] = result
		copies[i].Metrics = nil
	}
	data, err := json.Marshal(Report{Results: copies})
	if err != nil {
		return nil, err
	}
	var report Report
	if err := json.Unmarshal(data, &report); err != nil {
		return nil, err
	}
	redactReport(&report)
	return report.Results, nil
}

func observeOutputField(value string, redact bool) string {
	if redact {
		value = redactIPAddresses(value)
	}
	return viewSanitize(value)
}

func observeOutputFits(text string, width int) bool {
	for _, line := range strings.Split(text, "\n") {
		if width > 0 && liveWidth(line) > width {
			return false
		}
	}
	return true
}

func formatObserveSvcTable(items []svcListItem, style infoStyle, redact bool) string {
	if len(items) == 0 {
		return ""
	}
	if style.width <= 0 {
		return formatSvcListTable(items, style.textStyle)
	}
	safe := append([]svcListItem(nil), items...)
	for i, item := range safe {
		safe[i].Unit = observeOutputField(item.Unit, redact)
		safe[i].Description = observeOutputField(item.Description, redact)
		safe[i].LoadState = observeOutputField(item.LoadState, redact)
		safe[i].ActiveState = observeOutputField(item.ActiveState, redact)
		safe[i].SubState = observeOutputField(item.SubState, redact)
		safe[i].Result = observeOutputField(item.Result, redact)
	}
	table := formatSvcListTable(safe, textStyle{color: style.color})
	if observeOutputFits(table, style.width) {
		return table
	}
	headers := svcListHeaders()
	var output strings.Builder
	for i, item := range safe {
		if i > 0 {
			fmt.Fprintln(&output)
		}
		observeOutputLine(&output, style, 0, item.Unit)
		cells := svcListCells(item)
		var fields []string
		for j := 1; j < len(cells); j++ {
			value := cells[j]
			if j == 1 {
				value = style.paint(value, item.stateColor())
			}
			fields = append(fields, style.kv(headers[j], value))
		}
		observeOutputLine(&output, style, 4, style.join(" · ", fields...))
	}
	return output.String()
}

func formatObserveChangesTable(events []changeEvent, all bool, style infoStyle, redact bool) string {
	if len(events) == 0 {
		return ""
	}
	if style.width <= 0 {
		return formatChangesTable(events, all, style.color)
	}
	safe := append([]changeEvent(nil), events...)
	for i, event := range safe {
		safe[i].Name = observeOutputField(event.Name, redact)
		safe[i].Detail = observeOutputField(event.Detail, redact)
		safe[i].Kind = observeOutputField(event.Kind, redact)
	}
	table := formatChangesTableWithStyle(safe, all, textStyle{color: style.color})
	if observeOutputFits(table, style.width) {
		return table
	}
	shown := safe
	if !all && len(shown) > changesTableLimit {
		shown = shown[:changesTableLimit]
	}
	var output strings.Builder
	for i, event := range shown {
		if i > 0 {
			fmt.Fprintln(&output)
		}
		observeOutputLine(&output, style, 0, style.dim(event.Time.Format("2006-01-02 15:04:05"))+style.dim(" · ")+event.Kind)
		observeOutputLine(&output, style, 4, style.kv(T("changes.column.name"), event.Name))
		observeOutputLine(&output, style, 4, style.kv(T("changes.column.detail"), listenCell(event.Detail)))
	}
	if hidden := len(events) - len(shown); hidden > 0 {
		observeOutputLine(&output, style, 0, style.dim(T("changes.more", hidden)))
	}
	return output.String()
}
