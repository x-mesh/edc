package edc

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestSuggestCommandsFindsTheNearCommands(t *testing.T) {
	for typed, want := range map[string][]string{
		"lisen":     {"listen"},
		"LISTNE":    {"listen"},
		"tpo":       {"top"},
		"tap":       {"tcp", "top"},
		"logs":      {"log"},
		"histroy":   {"history"},
		"hepl":      {"help"},
		"decmopres": {"decompress"},
		"--version": {"version"},
		// 다른 도구의 이름과 짧은 낱말은 오타로 읽지 않는다.
		"docker": nil,
		"status": nil,
		"ls":     nil,
		"zzzzzz": nil,
		"":       nil,
	} {
		if got := suggestCommands(typed); !reflect.DeepEqual(got, want) {
			t.Errorf("suggestCommands(%q) = %v, want %v", typed, got, want)
		}
	}
	for _, name := range commandNames() {
		for _, similar := range suggestCommands(name) {
			if similar == name {
				t.Errorf("%s suggests itself", name)
			}
		}
	}
}

func TestTypoDistanceCountsOneSwapAsOneTypo(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{{"top", "top", 0}, {"tpo", "top", 1}, {"lisen", "listen", 1}, {"listenn", "listen", 1}, {"docker", "doctor", 2}, {"", "du", 2}, {"du", "", 2}} {
		if got := typoDistance([]rune(tc.a), []rune(tc.b)); got != tc.want {
			t.Errorf("typoDistance(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestPrintUnknownCommandKeepsTheFirstLineForOlderHosts(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	// check --group과 find --group은 원격 edc의 stderr에서 "command: <name>"을 찾는다. 권하는 줄이 그 모양이면
	// 명령이 있는 host를 없는 host로 읽는다.
	remoteMarker := regexp.MustCompile(`command: \S`)
	for _, language := range supportedLanguages {
		setLanguage(language)
		var output strings.Builder
		printUnknownCommand(&output, "lisen", true)
		lines := strings.Split(strings.TrimRight(output.String(), "\n"), "\n")
		if len(lines) != 3 || lines[0] != T("cli.error.unknown_command", "lisen") || lines[2] != T("cli.error.help_hint") {
			t.Fatalf("%s: lines = %q", language, lines)
		}
		if !strings.Contains(lines[1], "listen") || remoteMarker.MatchString(lines[1]) {
			t.Errorf("%s: suggestion line = %q", language, lines[1])
		}
		output.Reset()
		printUnknownCommand(&output, "zzzzzz", false)
		if output.String() != T("cli.error.unknown_command", "zzzzzz")+"\n" {
			t.Errorf("%s: a name with no near command printed %q", language, output.String())
		}
	}
}
