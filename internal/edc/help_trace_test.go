package edc

import (
	"io"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestTraceSubHelpCoversEverySubcommand(t *testing.T) {
	want := []string{"show"}
	for _, protocol := range traceProtocolRegistry {
		want = append(want, protocol.name)
	}
	slices.Sort(want)
	got := traceSubNames()
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("sub help %v, trace subcommands %v", got, want)
	}
	parent, _ := findCommandDoc("trace")
	for _, sub := range traceSubDocs {
		for _, option := range sub.options {
			if _, ok := traceOptionDoc(parent, option); !ok {
				t.Errorf("%s: --%s is not an option of edc help trace", sub.name, option)
			}
		}
		for _, group := range sub.noteGroups {
			if !slices.Contains(traceNoteGroups, group) {
				t.Errorf("%s: note group %s is not in traceNoteGroups", sub.name, group)
			}
		}
	}
}

func TestTraceNoteGroupsHoldEveryNoteInEveryLanguage(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	counts := map[string]int{}
	for _, language := range supportedLanguages {
		setLanguage(language)
		doc, _ := findCommandDoc("trace")
		for _, group := range traceNoteGroups {
			if len(TList("command.trace.note_group."+group)) == 0 {
				t.Errorf("%s: note group %s is empty", language, group)
			}
		}
		counts[language] = len(doc.notes())
	}
	// 묶음 이름을 traceNoteGroups에 빠뜨리면 그 notes는 어느 도움말에도 나오지 않는다. 세 언어의 notes 수가 같고, locale의
	// 묶음이 모두 traceNoteGroups에 있는지 본다.
	for _, language := range supportedLanguages {
		if counts[language] != counts["en"] {
			t.Errorf("%s has %d trace notes, en has %d", language, counts[language], counts["en"])
		}
		loadCatalogs()
		node, _ := catalogs[language].lookup("command.trace.note_group")
		groups, _ := node.(map[string]interface{})
		for name := range groups {
			if !slices.Contains(traceNoteGroups, name) {
				t.Errorf("%s: note group %s is missing from traceNoteGroups", language, name)
			}
		}
	}
}

func TestTraceSubHelpShowsOnlyItsOwnPart(t *testing.T) {
	var exec strings.Builder
	if !printTraceSubHelp(&exec, "exec") {
		t.Fatal("no help for trace exec")
	}
	text := exec.String()
	for _, want := range []string{"edc trace exec [options]", "--signal <names>", "edc trace exec --signal all --raw", TList("command.trace.note_group.exec")[0], "edc help trace"} {
		if !strings.Contains(text, want) {
			t.Errorf("trace exec help lacks %q:\n%s", want, text)
		}
	}
	for _, other := range []string{"--port <n>", "--frequency 99", TList("command.trace.note_group.mysql")[0], TList("command.trace.note_group.screen")[0]} {
		if strings.Contains(text, other) {
			t.Errorf("trace exec help has %q from another subcommand:\n%s", other, text)
		}
	}
	var full strings.Builder
	printCommandHelp(&full, "trace")
	if lines := strings.Count(full.String(), "\n"); lines <= 2*strings.Count(text, "\n") {
		t.Errorf("the full help (%d lines) is not much longer than one subcommand (%d lines)", lines, strings.Count(text, "\n"))
	}
	if !strings.Contains(full.String(), "edc help trace <subcommand>") {
		t.Error("the full help does not point to the subcommand help")
	}
}

func TestTraceSubHelpEntryPoints(t *testing.T) {
	run := func(args ...string) (int, string, string) {
		t.Helper()
		out, err := os.CreateTemp(t.TempDir(), "stdout")
		if err != nil {
			t.Fatal(err)
		}
		previous := os.Stdout
		os.Stdout = out
		var code int
		stderr := captureTraceStderr(t, func() { code = Run(args, "test") })
		os.Stdout = previous
		out.Seek(0, io.SeekStart)
		data, _ := io.ReadAll(out)
		return code, string(data), stderr
	}
	for _, args := range [][]string{{"help", "trace", "lock"}, {"trace", "lock", "--help"}, {"trace", "lock", "--process", "api", "-h"}} {
		if code, stdout, _ := run(args...); code != 0 || !strings.Contains(stdout, "edc trace lock") || !strings.Contains(stdout, "--kind kernel|futex") ||
			strings.Contains(stdout, "--signal <names>") {
			t.Errorf("%v: code %d:\n%s", args, code, stdout)
		}
	}
	if code, stdout, stderr := run("help", "trace", "nope"); code != 2 || stdout != "" || !strings.Contains(stderr, "trace nope") {
		t.Errorf("unknown subcommand: code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	// -- 뒤의 --help는 trace syscall이 실행할 명령의 인자다.
	if code, stdout, _ := run("trace", "syscall", "--edc-help-sentinel", "--", "ls", "--help"); code != 2 || strings.Contains(stdout, "edc trace syscall") {
		t.Errorf("--help after -- opened the help: code %d:\n%s", code, stdout)
	}
}
