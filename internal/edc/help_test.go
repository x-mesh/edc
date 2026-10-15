package edc

import (
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// helpLineLimit은 첫 화면이 접히지 않게 지키는 폭이다. 좁은 terminal에서도 들여쓰기가 살아 있어야 한다.
const helpLineLimit = 80

func TestPrintHelpListsEveryCommandOnce(t *testing.T) {
	var output strings.Builder
	printHelp(&output, "0.18.0")
	text := output.String()

	if first, _, _ := strings.Cut(text, "\n"); !strings.HasPrefix(first, "edc 0.18.0 — ") {
		t.Errorf("the first line must show the version: %q", first)
	}

	for _, doc := range commandDocs {
		if strings.Count(text, "  "+doc.name+" ") != 1 {
			t.Errorf("command %q must appear once in the first screen", doc.name)
		}
		if !strings.Contains(text, doc.summary()) {
			t.Errorf("summary of %q is missing", doc.name)
		}
	}
	if !strings.Contains(text, "edc help <command>") {
		t.Error("the first screen must point at the detail command")
	}
}

// 번역이 길어지면 화면이 접히므로 세 언어를 모두 잰다.
func TestHelpScreensStayWithinTheLineLimit(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)

	for _, language := range supportedLanguages {
		setLanguage(language)
		var first strings.Builder
		// git describe가 만드는 개발 build의 긴 version으로 잰다.
		printHelp(&first, "0.18.0-6-gaca320a-dirty")
		assertWidth(t, language+" printHelp", first.String())

		for _, doc := range commandDocs {
			var detail strings.Builder
			if !printCommandHelp(&detail, doc.name) {
				t.Fatalf("printCommandHelp(%q) returned false", doc.name)
			}
			assertWidth(t, language+" help "+doc.name, detail.String())
		}
	}
}

// 색을 쓰는 화면도 패널 테두리와 배지가 평문보다 칸을 더 쓰지 않아야 한다.
func TestRichHelpScreensStayWithinTheLineLimit(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)

	for _, language := range supportedLanguages {
		setLanguage(language)
		var first strings.Builder
		printRichHelp(&first, "0.18.0-6-gaca320a-dirty")
		assertWidth(t, language+" printRichHelp", first.String())

		for _, doc := range commandDocs {
			var detail strings.Builder
			printRichCommandHelp(&detail, doc)
			assertWidth(t, language+" rich help "+doc.name, detail.String())
		}
	}
}

func TestRichHelpPaintsBadgePanelsAndNames(t *testing.T) {
	var output strings.Builder
	printRichHelp(&output, "0.18.0")
	text := output.String()

	for _, code := range []string{
		richExpect("\x1b[1;30;45m edc \x1b[0m"),
		richExpect("\x1b[90m0.18.0\x1b[0m"),
		richExpect("\x1b[90m╭─ \x1b[0m\x1b[1;35m") + T("help.group.diagnose"),
		richExpect("\x1b[90m│ \x1b[0m\x1b[1;36mdoctor\x1b[0m"),
		richExpect("\x1b[90m╰─\x1b[0m"),
		richExpect("\x1b[100;97m --redact \x1b[0m"),
		richExpect("\x1b[1;36medc help <command>\x1b[0m"),
	} {
		if !strings.Contains(text, code) {
			t.Errorf("the colored first screen misses %q", code)
		}
	}

	plain := ansi.Strip(text)
	if first, _, _ := strings.Cut(plain, "\n"); !strings.HasPrefix(first, " edc  0.18.0  ") {
		t.Errorf("the first line must show the badge and the version: %q", first)
	}
	for _, doc := range commandDocs {
		if strings.Count(plain, "│ "+doc.name+" ") != 1 {
			t.Errorf("command %q must appear once in the colored first screen", doc.name)
		}
		if !strings.Contains(plain, doc.summary()) {
			t.Errorf("summary of %q is missing", doc.name)
		}
	}
	diagnose, _ := helpGroupMembers("diagnose")
	if top := "╭─ " + T("help.group.diagnose") + "  " + strconv.Itoa(len(diagnose)) + " ───"; !strings.Contains(plain, top) {
		t.Errorf("the group panel must show the command count: %q", top)
	}
}

func TestRichCommandHelpKeepsThePlainSections(t *testing.T) {
	doc, _ := findCommandDoc("http")
	var colored, plain strings.Builder
	printRichCommandHelp(&colored, doc)
	printCommandHelp(&plain, "http")
	text := colored.String()

	for _, code := range []string{
		richExpect("\x1b[1;30;45m edc http \x1b[0m \x1b[1m") + doc.summary() + richExpect("\x1b[0m"),
		richExpect("\x1b[1;35m") + T("help.usage_label") + richExpect("\x1b[0m"),
		richExpect("\x1b[1;35m") + T("help.options_label") + richExpect("\x1b[0m"),
		richExpect("\x1b[1;35m") + T("help.common_options_label") + richExpect("\x1b[0m"),
		richExpect("\x1b[1;36m--expect-status N         \x1b[0m  ") + T("command.http.option.expect_status"),
	} {
		if !strings.Contains(text, code) {
			t.Errorf("colored http help misses %q", code)
		}
	}
	// 패널 테두리 "│ "는 평문의 들여쓰기와 같은 두 칸이라, 색을 빼면 본문 줄이 평문과 같아야 한다.
	stripped := ansi.Strip(text)
	for _, line := range strings.Split(plain.String(), "\n") {
		if strings.HasPrefix(line, "  ") && !strings.Contains(stripped, "│ "+strings.TrimPrefix(line, "  ")+"\n") {
			t.Errorf("colored http help misses the plain line %q", line)
		}
	}
	for _, note := range doc.notes() {
		if !strings.Contains(text, "\n"+note+"\n") {
			t.Errorf("a note must stay in the default color: %q", note)
		}
	}
}

func TestHelpStaysPlainWhenTheWriterIsNotATerminal(t *testing.T) {
	var output strings.Builder
	printHelp(&output, "0.18.0")
	if !printCommandHelp(&output, "trace") {
		t.Fatal("trace help is missing")
	}
	if strings.Contains(output.String(), richExpect("\x1b[")) {
		t.Fatal("help for a non-terminal writer must hold no escape codes")
	}
}

func TestTraceHelpDescribesSourceTargetAndEventKeys(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	for _, language := range supportedLanguages {
		setLanguage(language)
		var output strings.Builder
		if !printCommandHelp(&output, "trace") {
			t.Fatalf("trace help is missing for %s", language)
		}
		text := output.String()
		for _, value := range []string{"io", "--device <major:minor>", "--group-by <view>", "--slow <duration>", "--http-port <n>", "source", "target", "port", "process", "device", "cgroup", "event", "g"} {
			if !strings.Contains(text, value) {
				t.Fatalf("%s trace help misses %q: %q", language, value, text)
			}
		}
		for _, value := range []string{"Linux 5.15", "trace http", "trace mysql", "trace clickhouse"} {
			if !strings.Contains(text, value) {
				t.Fatalf("%s trace help misses %q: %q", language, value, text)
			}
		}
	}
}

func assertWidth(t *testing.T, label, text string) {
	t.Helper()
	for index, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		// 한글은 두 칸을 쓰므로 rune 수가 아니라 표시 폭으로 잰다.
		if width := liveWidth(line); width > helpLineLimit {
			t.Errorf("%s line %d is %d columns: %q", label, index+1, width, line)
		}
	}
}

func TestPrintCommandHelpRejectsUnknownName(t *testing.T) {
	var output strings.Builder
	if printCommandHelp(&output, "nope") {
		t.Fatal("an unknown command must return false")
	}
	if output.Len() != 0 {
		t.Fatalf("an unknown command must write nothing, got %q", output.String())
	}
}

func TestEveryCommandDocIsComplete(t *testing.T) {
	groups := map[string]bool{}
	for _, group := range helpGroups {
		groups[group] = true
	}
	seen := map[string]bool{}
	for _, doc := range commandDocs {
		if seen[doc.name] {
			t.Errorf("duplicate command doc: %s", doc.name)
		}
		seen[doc.name] = true
		if doc.summary() == "" || len(doc.usage) == 0 {
			t.Errorf("%s needs a summary and a usage line", doc.name)
		}
		if !groups[doc.group] {
			t.Errorf("%s has group %q, which printHelp never prints", doc.name, doc.group)
		}
		// zsh completion은 이름과 설명을 콜론으로 나누므로 설명에 콜론이 있으면 깨진다.
		// summary가 언어마다 다르므로 세 언어를 모두 본다.
		restore := currentLanguage()
		for _, language := range supportedLanguages {
			setLanguage(language)
			if strings.ContainsAny(doc.summary(), ":'") {
				t.Errorf("%s summary of %s must hold no colon or quote: %q", language, doc.name, doc.summary())
			}
		}
		setLanguage(restore)
	}
}

// help와 completion이 한 표를 읽는지 확인한다. 둘이 갈라지면 설명이 서로 어긋난다.
func TestCompletionScriptsFollowTheCommandTable(t *testing.T) {
	zsh := renderCompletion(zshCompletion, zshCommandList())
	bash := renderCompletion(bashCompletion, bashCommandList())

	for _, doc := range commandDocs {
		if !strings.Contains(zsh, "'"+doc.name+":"+doc.summary()+"'") {
			t.Errorf("zsh completion misses %q with its summary", doc.name)
		}
		if !strings.Contains(bash, doc.name) {
			t.Errorf("bash completion misses %q", doc.name)
		}
	}
	for _, script := range []string{zsh, bash} {
		if strings.Contains(script, "@@COMMANDS@@") {
			t.Error("the placeholder must be replaced")
		}
	}
}
