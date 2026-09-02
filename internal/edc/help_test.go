package edc

import (
	"strings"
	"testing"
)

// helpLineLimit은 첫 화면이 접히지 않게 지키는 폭이다. 좁은 terminal에서도 들여쓰기가 살아 있어야 한다.
const helpLineLimit = 80

func TestPrintHelpListsEveryCommandOnce(t *testing.T) {
	var output strings.Builder
	printHelp(&output)
	text := output.String()

	for _, doc := range commandDocs {
		if strings.Count(text, "  "+doc.name+" ") != 1 {
			t.Errorf("command %q must appear once in the first screen", doc.name)
		}
		if !strings.Contains(text, doc.summary) {
			t.Errorf("summary of %q is missing", doc.name)
		}
	}
	if !strings.Contains(text, "edc help <command>") {
		t.Error("the first screen must point at the detail command")
	}
}

func TestHelpScreensStayWithinTheLineLimit(t *testing.T) {
	var first strings.Builder
	printHelp(&first)
	assertWidth(t, "printHelp", first.String())

	for _, doc := range commandDocs {
		var detail strings.Builder
		if !printCommandHelp(&detail, doc.name) {
			t.Fatalf("printCommandHelp(%q) returned false", doc.name)
		}
		assertWidth(t, "help "+doc.name, detail.String())
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
		if doc.summary == "" || len(doc.usage) == 0 {
			t.Errorf("%s needs a summary and a usage line", doc.name)
		}
		if !groups[doc.group] {
			t.Errorf("%s has group %q, which printHelp never prints", doc.name, doc.group)
		}
		// zsh completion은 이름과 설명을 콜론으로 나누므로 설명에 콜론이 있으면 깨진다.
		if strings.ContainsAny(doc.summary, ":'") {
			t.Errorf("%s summary must hold no colon or quote: %q", doc.name, doc.summary)
		}
	}
}

// help와 completion이 한 표를 읽는지 확인한다. 둘이 갈라지면 설명이 서로 어긋난다.
func TestCompletionScriptsFollowTheCommandTable(t *testing.T) {
	zsh := renderCompletion(zshCompletion, zshCommandList())
	bash := renderCompletion(bashCompletion, bashCommandList())

	for _, doc := range commandDocs {
		if !strings.Contains(zsh, "'"+doc.name+":"+doc.summary+"'") {
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
