package edc

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPromptArgsEchoesTheCommandToRetype(t *testing.T) {
	var output strings.Builder
	values, ok := promptArgs(bufio.NewReader(strings.NewReader("example.com\n")), &output, "edc doctor", []string{"target"})
	if !ok || len(values) != 1 || values[0] != "example.com" {
		t.Fatalf("values = %#v, ok = %v", values, ok)
	}
	if got := output.String(); !strings.Contains(got, "target: ") || !strings.Contains(got, "→ edc doctor example.com") {
		t.Fatalf("output = %q", got)
	}
}

// 필수 위치 인자가 둘이면 둘 다 묻고, 되돌려 주는 명령에도 둘 다 들어가야 한다.
func TestPromptArgsAsksForEveryMissingValue(t *testing.T) {
	var output strings.Builder
	values, ok := promptArgs(bufio.NewReader(strings.NewReader("before.json\nafter.json\n")), &output, "edc report diff", []string{"before", "after"})
	if !ok || strings.Join(values, " ") != "before.json after.json" {
		t.Fatalf("values = %#v, ok = %v", values, ok)
	}
	if got := output.String(); !strings.Contains(got, "→ edc report diff before.json after.json") {
		t.Fatalf("output = %q", got)
	}
}

// 빈 값과 EOF는 취소다. caller가 usage를 내고 지금과 같은 exit code로 끝내야 한다.
func TestPromptArgsTreatsAnEmptyValueAsCancel(t *testing.T) {
	for name, input := range map[string]string{"empty line": "\n", "eof": ""} {
		var output strings.Builder
		if values, ok := promptArgs(bufio.NewReader(strings.NewReader(input)), &output, "edc doctor", []string{"target"}); ok || values != nil {
			t.Fatalf("%s: values = %#v, ok = %v", name, values, ok)
		}
		if strings.Contains(output.String(), "→") {
			t.Fatalf("%s: a cancel must echo no command: %q", name, output.String())
		}
	}
}

// go test의 stdin은 terminal이 아니다. 인자가 빠진 실행은 지금까지와 같이 usage 오류와
// exit code 2로 끝나야 script의 계약이 깨지지 않는다.
func TestTargetProbeKeepsTheUsageErrorWithoutATerminal(t *testing.T) {
	for _, args := range [][]string{
		{"dns", "lookup"},
		{"tcp", "check"},
		{"tls", "check"},
		{"http", "check"},
		{"net", "ping"},
		{"doctor"},
	} {
		if code := Run(args, "test"); code != 2 {
			t.Fatalf("%v: exit code = %d, want 2", args, code)
		}
	}
}

// report 목록은 schema를 확인해 고른다. 디렉터리에 있는 아무 JSON이나 올리면 고른 뒤에 실패한다.
func TestReportCandidatesKeepsOnlyReadableReports(t *testing.T) {
	t.Chdir(t.TempDir())
	data, err := json.Marshal(buildReport("test", time.Now(), nil, []Result{{Probe: "dns.lookup", Status: StatusPass}}, false))
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{
		"report.json": data,
		"notes.json":  []byte(`{"hello":"world"}`),
		"broken.json": []byte("not json"),
		"other.txt":   []byte("ignored"),
	} {
		if err := os.WriteFile(name, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	items := reportCandidates()
	if len(items) != 1 || items[0].value != "report.json" {
		t.Fatalf("candidates = %#v", items)
	}
}

// 하위 command가 빠진 실행도 terminal이 아니면 지금까지와 같은 usage 오류로 끝나야 한다.
func TestSubcommandGroupsKeepTheUsageErrorWithoutATerminal(t *testing.T) {
	for _, args := range [][]string{
		{"dns"}, {"net"}, {"report"}, {"route"}, {"change"}, {"completion"},
		{"report", "show"}, {"report", "diff"},
		{"tcp"}, {"tls"}, {"http"},
		{"change", "confirm"}, {"change", "rollback"}, {"route", "rollback"},
	} {
		if code := Run(args, "test"); code != 2 {
			t.Fatalf("%v: exit code = %d, want 2", args, code)
		}
	}
}

// 남아 있는 상태 파일이 없으면 고를 것이 없다. 묻지 않고 기존 오류로 끝나야 한다.
func TestPromptStatePathNeedsCandidates(t *testing.T) {
	if _, ok := promptStatePath("edc change confirm", nil); ok {
		t.Fatal("후보가 없으면 화면을 열지 않아야 한다")
	}
}
