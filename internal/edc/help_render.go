package edc

import (
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

var helpArgument = regexp.MustCompile(`<[^>]+>|\b(?:DB|N|PID)\b|\b\d+(?:\.\d+)?(?:d|w|h|m|s)\b`)
var helpProseToken = regexp.MustCompile(`\B--?[a-zA-Z][a-zA-Z0-9-]*|<[^>]+>|\bDB\b|\b\d+(?:\.\d+)?(?:d|w|h|m|s)\b|\bedc [a-z][a-z0-9-]*(?: (?:list|show|diff|export|trend|growth|prune|top|process|history|setup))?`)
var helpStyleSequence = regexp.MustCompile("\x1b\\[[0-9;:]*m")

func helpSyntax(text string, color bool) string {
	if !color {
		return text
	}
	if strings.HasPrefix(text, "/") {
		return historyStrong(text, richGreen, true)
	}
	var output strings.Builder
	start := 0
	for _, span := range helpArgument.FindAllStringIndex(text, -1) {
		output.WriteString(historyStrong(text[start:span[0]], richCyan, true))
		value := text[span[0]:span[1]]
		code := richYellow
		if value != "DB" && value != "N" && value != "PID" && !strings.HasPrefix(value, "<") {
			code = richGreen
		}
		output.WriteString(historyStrong(value, code, true))
		start = span[1]
	}
	output.WriteString(historyStrong(text[start:], richCyan, true))
	return output.String()
}

func helpInline(text string, color bool) string {
	parts := strings.Split(text, "`")
	for i := range parts {
		if i%2 == 1 {
			parts[i] = helpSyntax(parts[i], color)
			continue
		}
		if color {
			parts[i] = helpProseToken.ReplaceAllStringFunc(parts[i], func(token string) string {
				if strings.HasPrefix(token, "edc ") {
					fields := strings.Fields(token)
					if _, ok := findCommandDoc(fields[1]); !ok {
						return token
					}
				}
				return helpSyntax(token, true)
			})
		}
	}
	return strings.Join(parts, "")
}

type helpRenderer struct {
	output io.Writer
	width  int
	color  bool
}

func (render helpRenderer) lines(text, first, rest string) {
	carry := ""
	for i, line := range strings.Split(ansi.Wrap(text, max(1, render.width-2-liveWidth(first)), ""), "\n") {
		prefix := rest
		if i == 0 {
			prefix = first
		}
		shown := ansi.Truncate(carry+line, max(1, render.width-2-liveWidth(prefix)), "")
		if render.color {
			for _, sequence := range helpStyleSequence.FindAllString(line, -1) {
				if sequence == "\x1b[m" || sequence == richReset {
					carry = ""
				} else {
					carry += sequence
				}
			}
			shown += richReset
			fmt.Fprintln(render.output, richPanelLine(prefix+shown, ""))
		} else {
			fmt.Fprintln(render.output, "  "+prefix+shown)
		}
	}
}

func (render helpRenderer) panel(title string, body func()) {
	if render.color {
		fmt.Fprintf(render.output, "\n%s\n", richPanelTop(title, "", ""))
	} else {
		fmt.Fprintf(render.output, "\n%s\n", title)
	}
	body()
	if render.color {
		fmt.Fprintln(render.output, richPanelBottom(""))
	}
}

func (render helpRenderer) syntax(text string) { render.lines(helpSyntax(text, render.color), "", "") }
func (render helpRenderer) note(text string) {
	render.lines(helpInline(text, render.color), "• ", "  ")
}

func (render helpRenderer) options(options []optionDoc) {
	width := optionFlagWidth(options)
	for _, option := range options {
		if render.width < 64 || width+18 > render.width {
			render.syntax(option.flag)
			if option.detail() != "" {
				render.lines(helpInline(option.detail(), render.color), "  ", "  ")
			}
			continue
		}
		prefix := helpSyntax(padRight(option.flag, width), render.color) + "  "
		render.lines(helpInline(option.detail(), render.color), prefix, strings.Repeat(" ", width+2))
	}
}

func commandHelpWidth() int { return max(20, min(80, terminalWidth())) }

func renderHelpHeader(output io.Writer, name, summary string, width int, color bool) {
	prefix := "edc " + name
	line := prefix + " — " + summary
	if color {
		prefix = richBadge(prefix, richMagenta)
		line = prefix + " " + richBold(summary)
	}
	if liveWidth(line) <= width {
		fmt.Fprintln(output, line)
		return
	}
	fmt.Fprintln(output, prefix)
	for _, line := range strings.Split(ansi.Wrap(summary, width, ""), "\n") {
		line = ansi.Truncate(line, width, "")
		if color {
			line = richBold(line)
		}
		fmt.Fprintln(output, line)
	}
}

func renderCommandHelp(output io.Writer, doc commandDoc, width int, color bool) {
	render := helpRenderer{output: output, width: width, color: color}
	renderHelpHeader(output, doc.name, doc.summary(), width, color)
	render.panel(T("help.usage_label"), func() {
		for _, line := range doc.usage {
			render.syntax(line)
		}
	})
	if len(doc.examples) > 0 {
		render.panel(T("help.examples_label"), func() {
			for _, line := range doc.examples {
				render.syntax(line)
			}
		})
	}
	if len(doc.options) > 0 {
		render.panel(T("help.options_label"), func() { render.options(doc.options) })
	}
	if doc.usesCommon {
		render.panel(T("help.common_options_label"), func() { render.options(uncoveredCommonOptions(doc.options)) })
	}
	if doc.name == "history" {
		for _, group := range []string{"storage", "recording", "browsing", "analysis", "cleanup"} {
			key := "history.help_sections." + group
			render.panel(T(key+".title"), func() {
				for _, note := range TList(key + ".notes") {
					render.note(note)
				}
				if group == "storage" {
					if path, err := defaultHistoryPath(); err == nil {
						render.note(T("history.database_path", "`"+reportIdentityValue(path)+"`"))
					}
				}
			})
		}
	} else if notes := doc.notes(); len(notes) > 0 {
		render.panel(T("help.notes_label"), func() {
			for _, note := range notes {
				render.note(note)
			}
		})
	}
}
