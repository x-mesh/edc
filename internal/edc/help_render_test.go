package edc

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestAllCommandHelpPanelsFitNarrowTerminals(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	for _, language := range []string{"en", "ko", "ja"} {
		setLanguage(language)
		for _, doc := range commandDocs {
			for _, width := range []int{40, 80} {
				for _, color := range []bool{false, true} {
					var output strings.Builder
					renderCommandHelp(&output, doc, width, color)
					for _, line := range strings.Split(output.String(), "\n") {
						if liveWidth(line) > width {
							t.Fatalf("%s/%s/%d exceeds width: %q", language, doc.name, width, line)
						}
					}
					if color && (!strings.Contains(output.String(), "╭─") || !strings.Contains(output.String(), "╰─")) {
						t.Fatalf("%s lost the existing help panels", doc.name)
					}
					if !color && strings.Contains(output.String(), "\x1b[") {
						t.Fatal("plain help contains ANSI")
					}
				}
			}
		}
	}
}

func TestHistoryHelpGroupsNotesAndDistinguishesArguments(t *testing.T) {
	doc, _ := findCommandDoc("history")
	var output strings.Builder
	renderCommandHelp(&output, doc, 80, true)
	for _, group := range []string{"storage", "recording", "browsing", "analysis", "cleanup"} {
		if !strings.Contains(output.String(), richPanelTop(T("history.help_sections."+group+".title"), "", "")) {
			t.Fatalf("missing history section %s", group)
		}
	}
	for _, argument := range []string{"<id>", "<path|->", "DB"} {
		if !strings.Contains(output.String(), historyStrong(argument, richYellow, true)) {
			t.Fatalf("argument color missing for %s", argument)
		}
	}
	if strings.Contains(helpInline("stop with Ctrl-C", true), richCyan) {
		t.Fatal("a hyphen in prose was mistaken for an option")
	}
}

func TestHelpWrappedStylesDoNotLeakPastPanelLines(t *testing.T) {
	var output strings.Builder
	render := helpRenderer{output: &output, width: 40, color: true}
	render.panel("notes", func() {
		render.note("Read `edc history prune --older-than <age> [<DB>]` to choose a period, then inspect the selected records.")
	})
	for _, line := range strings.Split(output.String(), "\n") {
		if strings.HasPrefix(ansi.Strip(line), "│ ") && !strings.HasSuffix(line, richReset) {
			t.Fatalf("style left active across panel lines: %q", line)
		}
	}
}
