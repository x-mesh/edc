package edc

import (
	"regexp"
	"slices"
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

func TestHelpWrapKeepsOptionNamesWhole(t *testing.T) {
	if got := helpWrap("use --watch now", 8); !slices.Equal(got, []string{"use", "--watch", "now"}) {
		t.Fatalf("helpWrap = %q", got)
	}
	// 줄 끝의 --나 낱말 안의 하이픈은 옵션 이름이나 낱말을 가른 자리다. 혼자 선 -(표준 출력, 빈 값)는 괜찮다.
	split := regexp.MustCompile(`(--|[A-Za-z0-9]-)$`)
	restore := currentLanguage()
	defer setLanguage(restore)
	for _, language := range supportedLanguages {
		setLanguage(language)
		for _, doc := range commandDocs {
			for _, color := range []bool{false, true} {
				var output strings.Builder
				renderCommandHelp(&output, doc, 80, color)
				for _, line := range strings.Split(ansi.Strip(output.String()), "\n") {
					// 색이 있으면 줄 끝에 테두리 │가 붙는다.
					text := strings.TrimRight(strings.TrimSuffix(strings.TrimRight(line, " "), "│"), " ")
					if split.MatchString(text) {
						t.Errorf("%s %s color=%v: a line ends inside a word: %q", language, doc.name, color, line)
					}
					// 하이픈 대신 쓰는 글자가 폭을 바꾸면 줄이 80칸을 넘는다.
					if width := liveWidth(line); width > 80 {
						t.Errorf("%s %s color=%v: %d columns: %q", language, doc.name, color, width, line)
					}
				}
			}
		}
	}
}
