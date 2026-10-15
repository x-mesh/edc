package edc

import (
	"strings"
	"testing"
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
