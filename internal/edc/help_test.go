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

			var brief strings.Builder
			if printCommandBrief(&brief, doc.name) {
				assertWidth(t, language+" brief "+doc.name, brief.String())
			}

			names := make([]string, 0, len(doc.subcommands))
			for _, sub := range doc.subcommands {
				names = append(names, sub.name)
			}
			for _, item := range subcommandSelectItems(doc.name, names) {
				assertWidth(t, language+" select "+doc.name, liveSelectedBar+" "+item.label)
			}
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
			if len(doc.subcommands) > 0 {
				var brief strings.Builder
				printRichCommandBrief(&brief, doc)
				assertWidth(t, language+" rich brief "+doc.name, brief.String())
			}
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
		for _, sub := range doc.subcommands {
			for _, language := range supportedLanguages {
				if !hasMessage(language, doc.subcommandKey(sub.name)) {
					t.Errorf("%s has no %s description of %s %s", language, doc.subcommandKey(sub.name), doc.name, sub.name)
				}
			}
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

// 인자 없이 친 edc log가 "-- 뒤에 명령이 필요하다"는 오류만 내면 log가 무엇을 하는지 알 수 없다.
func TestBareLogPrintsTheBrief(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")

	var stdout, stderr strings.Builder
	code := runLogWithStreams(nil, logStreams{stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr})
	if code != 2 {
		t.Fatalf("bare edc log must return 2, got %d", code)
	}
	for _, want := range []string{"edc log -- <command> [args...]", "edc log history [<command>]", "edc log -- ls -l /tmp", "edc help log"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("the brief misses %q:\n%s", want, stderr.String())
		}
	}
	if strings.Contains(stderr.String(), T("cli.log.command_required")) {
		t.Errorf("bare edc log must not print the -- error:\n%s", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("the brief belongs on stderr, stdout got %q", stdout.String())
	}
}

func TestPrintCommandBriefNeedsSubcommands(t *testing.T) {
	var output strings.Builder
	if printCommandBrief(&output, "doctor") || printCommandBrief(&output, "nope") {
		t.Fatal("a command without subcommand docs has no brief")
	}
	if output.Len() != 0 {
		t.Fatalf("no brief must write nothing, got %q", output.String())
	}
}

func TestSubcommandSelectItemsDescribeEachChoice(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")

	items := subcommandSelectItems("dns", []string{"lookup", "compare", "config"})
	want := []selectItem{
		{label: "lookup   — " + T("command.dns.sub.lookup"), value: "lookup"},
		{label: "compare  — " + T("command.dns.sub.compare"), value: "compare"},
		{label: "config   — " + T("command.dns.sub.config"), value: "config"},
	}
	if len(items) != len(want) {
		t.Fatalf("got %d items, want %d", len(items), len(want))
	}
	for index := range want {
		if items[index] != want[index] {
			t.Errorf("item %d = %+v, want %+v", index, items[index], want[index])
		}
	}

	plain := subcommandSelectItems("tcp", []string{"check"})
	if len(plain) != 1 || plain[0] != (selectItem{label: "check", value: "check"}) {
		t.Errorf("a choice without a description keeps its name, got %+v", plain)
	}
}

// 색 안내 화면은 평문과 같은 형태와 예시를 담고, 그대로 칠 수 있는 명령을 굵은 청록으로 칠한다.
func TestRichCommandBriefKeepsTheFormsAndExamples(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	doc, _ := findCommandDoc("log")
	var colored, plain strings.Builder
	printRichCommandBrief(&colored, doc)
	printCommandBrief(&plain, "log")
	text := ansi.Strip(colored.String())
	for _, want := range []string{"edc log -- <command> [args...]", "edc log history [<command>]", "edc log -- ls -l /tmp", "edc help log", T("command.log.sub.run")} {
		if !strings.Contains(text, want) || !strings.Contains(plain.String(), want) {
			t.Errorf("brief misses %q:\n%s", want, text)
		}
	}
	if !strings.Contains(colored.String(), richPaint("edc log -- ls -l /tmp", richCyan, true)) {
		t.Errorf("examples should be bold cyan: %q", colored.String())
	}
}

// 평문 안내 화면도 색 화면처럼 형태 목록 위에 usage 머리줄을 둔다. history 예시는 history 명령만 담는다.
func TestPlainCommandBriefLabelsTheForms(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	var brief strings.Builder
	if !printCommandBrief(&brief, "history") {
		t.Fatal("history has a brief")
	}
	lines := strings.Split(brief.String(), "\n")
	if len(lines) < 3 || lines[2] != T("help.usage_label") || !strings.Contains(brief.String(), "edc history list [options] [DB]") {
		t.Errorf("brief = %q", brief.String())
	}
	if strings.Contains(brief.String(), "\n  edc top -w\n") {
		t.Errorf("history examples must run history: %q", brief.String())
	}
}

// 하위 명령 이름을 commandDocs에서 만들어도 평문 usage는 예전 글자 그대로다. script가 읽는 stderr가 바뀌지 않는다.
func TestSubcommandUsageKeepsTheOldText(t *testing.T) {
	for name, want := range map[string]string{
		"dns":        "edc dns <lookup|compare|config>",
		"net":        "edc net <interfaces|route|ping|trace>",
		"route":      "edc route <check|switch|status|rollback>",
		"disk":       "edc disk <check|grow>",
		"change":     "edc change <apply|status|confirm|rollback>",
		"completion": "edc completion <zsh|bash|groups>",
		"watch":      "edc watch <http|fs>",
		"history":    "edc history <list|top|process>",
	} {
		if got := subcommandUsage(name); got != want {
			t.Errorf("subcommandUsage(%q) = %q, want %q", name, got, want)
		}
	}
}

// 선택기가 고르게 하는 하위 명령은 모두 설명이 있어야 한다. 설명이 빠지면 그 항목만 이름으로 나와도 눈치채기 어렵다.
func TestPickerSubcommandsHaveDescriptions(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	for _, name := range []string{"dns", "net", "route", "disk", "change", "completion", "watch", "history"} {
		choices := subcommandChoices(name)
		if len(choices) < 2 {
			t.Errorf("%s needs its subcommands in commandDocs, got %q", name, choices)
			continue
		}
		for _, language := range supportedLanguages {
			setLanguage(language)
			for _, item := range subcommandSelectItems(name, choices) {
				if !strings.Contains(item.label, " — ") {
					t.Errorf("%s %s %s has no description: %q", language, name, item.value, item.label)
				}
			}
		}
	}
}
